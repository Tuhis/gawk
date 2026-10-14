package store_test

import (
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-admin/internal/store"
	"github.com/Tuhis/gawk/gawk-admin/internal/store/storetest"
	"github.com/Tuhis/gawk/gawk-server/events"
	"github.com/Tuhis/gawk/gawk-server/screening"
)

var t0 = time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

// batch builds a one-session sample batch: taken samples, of which the given
// positions are sent with the given score on the first default label.
func batch(epoch string, taken int, at time.Time, sent map[int]float64) screening.SampleBatch {
	labels := screening.DefaultLabels()
	s := screening.SessionSamples{
		BroadcastID: "ABC234", BroadcastKey: "0123456789ab", PublisherEpoch: epoch,
		Taken: taken, FirstAt: at, LastAt: at.Add(time.Duration(taken-1) * 5 * time.Second),
	}
	positions := make([]int, 0, len(sent))
	for p := range sent {
		positions = append(positions, p)
	}
	slices.Sort(positions)
	for _, p := range positions {
		s.Samples = append(s.Samples, screening.Sample{
			Position: p, At: at.Add(time.Duration(p-1) * 5 * time.Second),
			Scores: map[string]float64{labels[0]: sent[p], labels[1]: 0, labels[2]: 0},
		})
	}
	return screening.SampleBatch{Schema: screening.SchemaSamples, SamplerID: "gawk-sampler-0", Mode: screening.ModeObserve,
		Floor: 0.3, Labels: labels, Sessions: []screening.SessionSamples{s}}
}

func sessionSeqs(t *testing.T, s *store.Store, epoch string) (taken int64, seqs []int64) {
	t.Helper()
	if err := s.Pool().QueryRow(t.Context(), `SELECT samples_taken FROM screening_sessions WHERE publisher_epoch = $1`, epoch).Scan(&taken); err != nil {
		t.Fatalf("read session: %v", err)
	}
	rows, err := s.Pool().Query(t.Context(), `SELECT seq FROM screening_samples WHERE publisher_epoch = $1 ORDER BY seq`, epoch)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var n int64
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		seqs = append(seqs, n)
	}
	return taken, seqs
}

// D20: gawk-admin numbers the samples — the session's count before the batch
// plus each sent sample's position — and a restarted sampler, whose own
// positions start again at 1, continues the same numbering with no collision.
func TestIngestSamplesNumbersBySessionCount(t *testing.T) {
	s := storetest.New(t)
	ctx := t.Context()
	if n, err := s.IngestSamples(ctx, batch("e1", 5, t0, map[int]float64{2: 0.4, 5: 0.9})); err != nil || n != 2 {
		t.Fatalf("first batch: %d, %v", n, err)
	}
	// A restarted sampler: positions restart at 1.
	if n, err := s.IngestSamples(ctx, batch("e1", 3, t0.Add(time.Minute), map[int]float64{1: 0.5})); err != nil || n != 1 {
		t.Fatalf("second batch: %d, %v", n, err)
	}
	taken, seqs := sessionSeqs(t, s, "e1")
	if taken != 8 || !slices.Equal(seqs, []int64{2, 5, 6}) {
		t.Fatalf("taken %d, seqs %v; want 8 and [2 5 6]", taken, seqs)
	}
}

// Two concurrent batches for one session get disjoint numbers: the session
// row's lock serializes them (D20).
func TestConcurrentSampleBatchesGetDisjointNumbers(t *testing.T) {
	s := storetest.New(t)
	ctx := t.Context()
	if _, err := s.IngestSamples(ctx, batch("e2", 1, t0, nil)); err != nil {
		t.Fatal(err)
	}
	const writers = 8
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.IngestSamples(ctx, batch("e2", 4, t0, map[int]float64{1: 0.5, 2: 0.6, 3: 0.7, 4: 0.8}))
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("a concurrent batch failed: %v", err)
		}
	}
	taken, seqs := sessionSeqs(t, s, "e2")
	if taken != 1+writers*4 || len(seqs) != writers*4 {
		t.Fatalf("taken %d, %d samples; want %d and %d", taken, len(seqs), 1+writers*4, writers*4)
	}
	for i, n := range seqs {
		if n != int64(i+2) {
			t.Fatalf("seqs %v are not the disjoint run 2..%d", seqs, 1+writers*4)
		}
	}
}

// D21: a fixture session with known gaps gives the hand-computed would-trips
// for three settings, and a threshold under the floor is refused.
func TestReplayOverStoredSamples(t *testing.T) {
	s := storetest.New(t)
	ctx := t.Context()
	// 20 samples taken; stored (≥ the 0.3 floor): 3:0.85 4:0.9 5:0.82 7:0.95
	// 15:0.5 16:0.6 — the screening package's ReplaySession fixture, through
	// the database.
	if _, err := s.IngestSamples(ctx, batch("e3", 20, t0, map[int]float64{3: 0.85, 4: 0.9, 5: 0.82, 7: 0.95, 15: 0.5, 16: 0.6})); err != nil {
		t.Fatal(err)
	}
	at := func(seq int) time.Time { return t0.Add(time.Duration(seq-1) * 5 * time.Second) }
	for _, tc := range []struct {
		name      string
		threshold float64
		window    int
		hits      int
		want      []time.Time
	}{
		{"0.8/6/4", 0.8, 6, 4, []time.Time{at(7)}},
		{"0.8/6/3", 0.8, 6, 3, []time.Time{at(5)}},
		{"0.45/3/2", 0.45, 3, 2, []time.Time{at(16), at(4)}}, // newest first
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := s.Replay(ctx, screening.ReplayRequest{Threshold: tc.threshold, Window: tc.window, WindowHits: tc.hits,
				From: t0.Add(-time.Hour), To: t0.Add(time.Hour)})
			if err != nil {
				t.Fatal(err)
			}
			var got []time.Time
			for _, tr := range out.Trips {
				got = append(got, tr.At)
				if tr.BroadcastID != "ABC234" || tr.PublisherEpoch != "e3" {
					t.Errorf("trip identity = %+v", tr)
				}
			}
			if !slices.EqualFunc(got, tc.want, time.Time.Equal) || out.WouldTrips != len(tc.want) {
				t.Fatalf("trips at %v (%d), want %v", got, out.WouldTrips, tc.want)
			}
			if out.Sessions != 1 || out.SamplesTaken != 20 || out.SamplesStored != 6 || out.Floor != 0.3 {
				t.Errorf("sizes = %+v", out)
			}
			// 20 samples 5 s apart span 95 s.
			if want := (95 * time.Second).Hours(); out.BroadcastHours < want*0.999 || out.BroadcastHours > want*1.001 {
				t.Errorf("broadcastHours = %v, want %v", out.BroadcastHours, want)
			}
		})
	}
	_, err := s.Replay(ctx, screening.ReplayRequest{Threshold: 0.2, Window: 6, WindowHits: 4, From: t0.Add(-time.Hour), To: t0.Add(time.Hour)})
	if !errors.Is(err, store.ErrReplayBelowFloor) {
		t.Fatalf("threshold under the floor: %v, want ErrReplayBelowFloor", err)
	}
	_, err = s.Replay(ctx, screening.ReplayRequest{Threshold: 0.8, Window: 6, WindowHits: 4, Labels: []string{"FACE_FEMALE"},
		From: t0.Add(-time.Hour), To: t0.Add(time.Hour)})
	if !errors.Is(err, store.ErrReplayUnknownLabel) {
		t.Fatalf("a label no session has: %v, want ErrReplayUnknownLabel", err)
	}
	// A range that misses the session replays nothing.
	out, err := s.Replay(ctx, screening.ReplayRequest{Threshold: 0.8, Window: 6, WindowHits: 4, From: t0.Add(time.Hour), To: t0.Add(2 * time.Hour)})
	if err != nil || out.Sessions != 0 || out.WouldTrips != 0 || out.Trips == nil {
		t.Fatalf("empty range = %+v, %v", out, err)
	}
}

// D22: the newest poll's `of` is the expected count; a shard that has not
// polled within the window is missing, clears when it returns, and a poll at
// a new count re-bases the set.
func TestShardStatus(t *testing.T) {
	s := storetest.New(t)
	ctx := t.Context()
	window := 30 * time.Second
	st, err := s.ShardStatus(ctx, t0, window)
	if err != nil || st.Expected != 0 || len(st.Missing) != 0 || st.LastPoll != nil {
		t.Fatalf("before any poll: %+v, %v", st, err)
	}
	for _, shard := range []int{0, 1} {
		if err := s.RecordShardPoll(ctx, shard, 2, "gawk-sampler", t0); err != nil {
			t.Fatal(err)
		}
	}
	if st, _ = s.ShardStatus(ctx, t0.Add(10*time.Second), window); st.Expected != 2 || len(st.Missing) != 0 {
		t.Fatalf("both polled: %+v", st)
	}
	// Shard 1 stops polling.
	if err := s.RecordShardPoll(ctx, 0, 2, "gawk-sampler-0", t0.Add(25*time.Second)); err != nil {
		t.Fatal(err)
	}
	if st, _ = s.ShardStatus(ctx, t0.Add(31*time.Second), window); !slices.Equal(st.Missing, []int{1}) {
		t.Fatalf("shard 1 silent for 31 s: missing %v, want [1]", st.Missing)
	}
	if err := s.RecordShardPoll(ctx, 1, 2, "gawk-sampler-1", t0.Add(32*time.Second)); err != nil {
		t.Fatal(err)
	}
	if st, _ = s.ShardStatus(ctx, t0.Add(33*time.Second), window); len(st.Missing) != 0 {
		t.Fatalf("shard 1 back: missing %v", st.Missing)
	}
	// A resize to three: the newest poll re-bases the expected set.
	if err := s.RecordShardPoll(ctx, 2, 3, "gawk-sampler-2", t0.Add(34*time.Second)); err != nil {
		t.Fatal(err)
	}
	st, _ = s.ShardStatus(ctx, t0.Add(35*time.Second), window)
	if st.Expected != 3 || !slices.Equal(st.Missing, []int{0, 1}) {
		t.Fatalf("after a resize: %+v, want expected 3 with 0 and 1 missing until they poll at 3", st)
	}
}

// D17: pruning removes observed flags, their events and the samples older
// than the retention — and never a flag-mode row, however old.
func TestPruneScreeningNeverTouchesFlagModeRows(t *testing.T) {
	s := storetest.New(t)
	ctx := t.Context()
	old := t0.Add(-40 * 24 * time.Hour)
	for _, mode := range []string{screening.ModeFlag, screening.ModeObserve} {
		body := flagBody("ABC234", "e-old", mode, screening.StateTripped)
		res, err := s.PostContentFlag(ctx, store.FlagPost{Body: body, Actor: "sampler", Now: old}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.ResolveContentFlag(ctx, res.Flag, events.ContentFlagResolutionEnded, "", "system", old, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.IngestSamples(ctx, batch("e-old", 2, old, map[int]float64{1: 0.9})); err != nil {
		t.Fatal(err)
	}
	if _, err := s.IngestSamples(ctx, batch("e-new", 2, t0, map[int]float64{1: 0.9})); err != nil {
		t.Fatal(err)
	}
	n, err := s.PruneScreening(ctx, t0.Add(-30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n.Flags != 1 || n.Events != 1 || n.Samples != 1 || n.Sessions != 1 {
		t.Fatalf("pruned %+v, want one observed flag, its observed event, one sample and one session", n)
	}
	flags, err := s.ListContentFlags(ctx, store.FlagQuery{State: store.FlagFilterAll, Mode: store.FlagFilterAll})
	if err != nil {
		t.Fatal(err)
	}
	if len(flags) != 1 || flags[0].Mode != screening.ModeFlag {
		t.Fatalf("after pruning: %+v, want only the flag-mode row", flags)
	}
	// The flag-mode flag's own events are moderation rows: untouched.
	evs, err := s.ListEvents(ctx, store.EventQuery{Category: store.CategoryModeration})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 {
		t.Fatalf("moderation rows after pruning = %d, want the raise and the resolution", len(evs))
	}
	if _, seqs := sessionSeqs(t, s, "e-new"); len(seqs) != 1 {
		t.Fatalf("the recent session lost its samples: %v", seqs)
	}
}

func flagBody(id, epoch, mode, state string) screening.ContentFlag {
	return screening.ContentFlag{
		Schema: screening.SchemaContentFlag, BroadcastID: id, PublisherEpoch: epoch, SamplerID: "gawk-sampler-0",
		Mode: mode, State: state,
		Window: screening.WindowSummary{Start: t0.Add(-25 * time.Second), End: t0, Samples: 6, FlaggedSamples: 4},
		Scores: screening.Scores{Max: 0.95, Mean: 0.8}, Labels: []string{"FEMALE_GENITALIA_EXPOSED"},
	}
}

// docs/71 D16 through the real fan-out: a webhook with no filter receives
// raised, reminded and resolved but never observed; one whose filter names
// content_flag.observed receives it. The queue is written in the flag's own
// transaction.
func TestContentFlagEventsFanOutByFilter(t *testing.T) {
	s := storetest.New(t)
	ctx := t.Context()
	if _, err := s.CreateWebhook(ctx, store.Webhook{Name: "pager", URL: "https://pager.example", Secret: "aw==", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateWebhook(ctx, store.Webhook{Name: "calibration", URL: "https://cal.example", Secret: "aw==", Enabled: true,
		Events: []string{store.EventContentFlagObserved}}); err != nil {
		t.Fatal(err)
	}
	route, err := s.FlagRoute(ctx, nil)
	if err != nil || !route {
		t.Fatalf("FlagRoute with an unfiltered webhook = %v, %v", route, err)
	}

	raised, err := s.PostContentFlag(ctx, store.FlagPost{Body: flagBody("ABC234", "e1", screening.ModeFlag, screening.StateTripped), Actor: "sampler", Now: t0}, nil)
	if err != nil || raised.Status != screening.FlagStatusRaised {
		t.Fatalf("raise: %+v, %v", raised, err)
	}
	if _, ok, err := s.RemindContentFlag(ctx, raised.Flag.ID, events.ContentFlagSignalLive, 10*time.Minute, t0.Add(11*time.Minute), nil); err != nil || !ok {
		t.Fatalf("remind: %v, %v", ok, err)
	}
	cleared := flagBody("ABC234", "e1", screening.ModeFlag, screening.StateCleared)
	if res, err := s.PostContentFlag(ctx, store.FlagPost{Body: cleared, Actor: "sampler", Now: t0.Add(12 * time.Minute)}, nil); err != nil || res.Status != screening.FlagStatusResolved {
		t.Fatalf("clear: %+v, %v", res, err)
	}
	if res, err := s.PostContentFlag(ctx, store.FlagPost{Body: flagBody("DEF567", "e2", screening.ModeObserve, screening.StateTripped), Actor: "sampler", Now: t0}, nil); err != nil || res.Status != screening.FlagStatusRaised {
		t.Fatalf("observe: %+v, %v", res, err)
	}

	evs, err := s.ListEvents(ctx, store.EventQuery{})
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]int64, 0, len(evs))
	for _, e := range evs {
		ids = append(ids, e.ID)
	}
	deliveries, err := s.ListDeliveriesForEvents(ctx, ids)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{}
	for _, e := range evs {
		for _, d := range deliveries[e.ID] {
			got[e.Type] = append(got[e.Type], d.WebhookName)
		}
	}
	want := map[string][]string{
		store.EventContentFlag:         {"pager"},
		store.EventContentFlagReminded: {"pager"},
		store.EventContentFlagResolved: {"pager"},
		store.EventContentFlagObserved: {"calibration"},
	}
	for typ, names := range want {
		if !slices.Equal(got[typ], names) {
			t.Errorf("%s delivered to %v, want %v", typ, got[typ], names)
		}
	}
	for _, e := range evs {
		if e.Type == store.EventContentFlagObserved && e.Category != store.CategoryScreening {
			t.Errorf("observed row category = %q, want %q", e.Category, store.CategoryScreening)
		}
	}
}

// The ceiling: every R40 moderation row is delivered with no filter, the
// observed row only on request — and room.screening_changed like the room
// events it sits beside.
func TestWebhookWantsR40Types(t *testing.T) {
	for _, typ := range []string{store.EventContentFlag, store.EventContentFlagReminded, store.EventContentFlagResolved, store.EventRoomScreeningChanged} {
		if !store.WebhookWants(nil, store.Event{Type: typ}) {
			t.Errorf("an unfiltered webhook does not receive %s", typ)
		}
	}
	if store.WebhookWants(nil, store.Event{Type: store.EventContentFlagObserved}) {
		t.Error("an unfiltered webhook receives content_flag.observed: a fresh observe-mode install would page its pager")
	}
	if !store.WebhookWants([]string{store.EventContentFlagObserved}, store.Event{Type: store.EventContentFlagObserved}) {
		t.Error("a webhook that names content_flag.observed does not receive it")
	}
	if err := store.ValidateWebhookEvents([]string{store.EventContentFlagObserved, store.EventRoomScreeningChanged}); err != nil {
		t.Errorf("the R40 types are not valid filter entries: %v", err)
	}
}

// D12: no route when the only webhook is disabled, or filters raised out —
// and a chart-defined webhook counts as well as a UI-created one.
func TestFlagRoute(t *testing.T) {
	s := storetest.New(t)
	ctx := t.Context()
	if route, err := s.FlagRoute(ctx, nil); err != nil || route {
		t.Fatalf("no webhooks: %v, %v", route, err)
	}
	if _, err := s.CreateWebhook(ctx, store.Webhook{Name: "rooms-bot", URL: "https://bot.example", Secret: "aw==", Enabled: true,
		Events: []string{store.EventRoomAttached, store.EventContentFlagObserved}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateWebhook(ctx, store.Webhook{Name: "parked", URL: "https://parked.example", Secret: "aw==", Enabled: false}); err != nil {
		t.Fatal(err)
	}
	if route, err := s.FlagRoute(ctx, nil); err != nil || route {
		t.Fatalf("only a filter that excludes raised and a disabled webhook: %v, %v", route, err)
	}
	if route, _ := s.FlagRoute(ctx, []store.ConfigWebhook{{Name: "ops-pager"}}); !route {
		t.Fatal("a chart-defined webhook with no filter is not a route")
	}
}
