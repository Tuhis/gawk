package sampler

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Tuhis/gawk/gawk-sampler/internal/admin"
	"github.com/Tuhis/gawk/gawk-sampler/internal/relay"
	"github.com/Tuhis/gawk/gawk-server/screening"
)

type harness struct {
	t    *testing.T
	e    *Engine
	adm  *fakeAdmin
	rel  *fakeRelay
	reg  *prometheus.Registry
	logs *syncBuffer
	ctx  context.Context
	opts Options
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func target(id, epoch string) screening.Target {
	return screening.Target{BroadcastID: id, BroadcastKey: "0123456789ab", PublisherEpoch: epoch, Relay: "http://relay.invalid:2112"}
}

func newHarness(t *testing.T, mode string, tweak func(*Options)) *harness {
	t.Helper()
	h := &harness{t: t, adm: &fakeAdmin{flagRoute: true}, rel: newFakeRelay(), reg: prometheus.NewRegistry(), logs: &syncBuffer{}, ctx: context.Background()}
	opts := Options{
		Mode:           mode,
		SamplerID:      "gawk-sampler-1",
		ShardIndex:     1,
		ShardCount:     2,
		Labels:         screening.DefaultLabels(),
		Rule:           screening.WindowRule{Threshold: 0.8, Size: 6, Hits: 4},
		ReportSamples:  4,
		Floor:          0.3,
		Interval:       50 * time.Millisecond,
		Workers:        2,
		TargetsRefresh: time.Hour,
		BatchInterval:  time.Hour,
		Admin:          h.adm,
		Keyframes:      h.rel,
		Decoder:        fakeDecoder{},
		Scorer:         fakeScorer{},
		Metrics:        NewMetrics(h.reg),
		Log:            slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	if tweak != nil {
		tweak(&opts)
	}
	e, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	h.e, h.opts = e, opts
	return h
}

func (h *harness) setTargets(ts ...screening.Target) {
	h.adm.mu.Lock()
	h.adm.targets = ts
	h.adm.mu.Unlock()
	h.e.refresh(h.ctx)
}

// sampleOnce runs one sample of id synchronously, as the dispatcher would.
func (h *harness) sampleOnce(id string) {
	h.e.mu.Lock()
	b := h.e.bcs[id]
	if b == nil {
		h.e.mu.Unlock()
		h.t.Fatalf("%s is not a target", id)
	}
	b.busy = true
	h.e.mu.Unlock()
	h.e.sample(h.ctx, b)
}

func states(fs []screening.ContentFlag) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.State
	}
	return out
}

// CF5: trip, hold, clear — every sample while tripped POSTs `tripped` with
// `recent` holding the last -report-samples scores, and the clear POSTs
// `cleared` exactly once.
func TestHysteresisPosts(t *testing.T) {
	h := newHarness(t, screening.ModeFlag, nil)
	h.setTargets(target("ABC234", "e1"))
	h.rel.script("ABC234", "e1", scores(0.9, 0.1, 0.9, 0.9, 0.9, 0.1, 0.1, 0.1, 0.1, 0.1, 0.1, 0.1)...)

	for i := 0; i < 4; i++ {
		h.sampleOnce("ABC234")
	}
	if fs := h.adm.takeFlags(); len(fs) != 0 {
		t.Fatalf("POSTed before the window tripped: %v", states(fs))
	}
	h.sampleOnce("ABC234") // 4 hits of 5: trips
	fs := h.adm.takeFlags()
	if len(fs) != 1 || fs[0].State != screening.StateTripped {
		t.Fatalf("trip: %v", states(fs))
	}
	f := fs[0]
	if f.Mode != screening.ModeFlag || f.PublisherEpoch != "e1" || f.SamplerID != "gawk-sampler-1" ||
		f.Window.Samples != 5 || f.Window.FlaggedSamples != 4 || f.Scores.Max != 0.9 ||
		!reflect.DeepEqual(f.Recent, []float64{0.1, 0.9, 0.9, 0.9}) ||
		!reflect.DeepEqual(f.Labels, []string{label}) || f.LabelCounts[label] != 4 {
		t.Fatalf("tripped flag %+v", f)
	}

	for i := 0; i < 5; i++ {
		h.sampleOnce("ABC234") // holds while a hit remains in the last six
	}
	fs = h.adm.takeFlags()
	if len(fs) != 5 {
		t.Fatalf("hold: %v", states(fs))
	}
	for _, f := range fs {
		if f.State != screening.StateTripped || len(f.Recent) != 4 {
			t.Fatalf("hold flag %+v", f)
		}
	}
	h.sampleOnce("ABC234") // the sixth low sample: clears
	fs = h.adm.takeFlags()
	if len(fs) != 1 || fs[0].State != screening.StateCleared || len(fs[0].Recent) != 0 || len(fs[0].Labels) != 0 {
		t.Fatalf("clear: %+v", fs)
	}
	h.sampleOnce("ABC234")
	if fs := h.adm.takeFlags(); len(fs) != 0 {
		t.Fatalf("POSTed after the clear: %v", states(fs))
	}
}

// CF5: a new publisher session resets the window; recent starts empty, so it
// holds fewer than -report-samples after the reset; a late sample of the old
// session is discarded rather than allowed to bring it back.
func TestNewSessionResetsTheWindow(t *testing.T) {
	h := newHarness(t, screening.ModeFlag, func(o *Options) { o.ReportSamples = 6 })
	h.setTargets(target("ABC234", "e1"))
	h.rel.script("ABC234", "e1", scores(0.9, 0.9, 0.9, 0.9, 0.9)...)
	for i := 0; i < 5; i++ {
		h.sampleOnce("ABC234")
	}
	if fs := h.adm.takeFlags(); len(fs) != 2 || fs[1].PublisherEpoch != "e1" || len(fs[1].Recent) != 5 {
		t.Fatalf("first session: %+v", fs)
	}

	h.rel.mu.Lock()
	h.rel.scripts["ABC234"] = []step{{score: 0.9, epoch: "e2"}, {score: 0.9}, {score: 0.9}, {score: 0.9}}
	h.rel.mu.Unlock()
	for i := 0; i < 3; i++ {
		h.sampleOnce("ABC234")
	}
	if fs := h.adm.takeFlags(); len(fs) != 0 {
		t.Fatalf("a new session's first samples POSTed: %+v", fs)
	}
	h.sampleOnce("ABC234")
	fs := h.adm.takeFlags()
	if len(fs) != 1 || fs[0].PublisherEpoch != "e2" || len(fs[0].Recent) != 4 || fs[0].Window.Samples != 4 {
		t.Fatalf("second session trip: %+v", fs)
	}

	// A stale listing that still names e1 does not reset e2 back.
	h.setTargets(target("ABC234", "e1"))
	h.e.mu.Lock()
	epoch, n := h.e.bcs["ABC234"].epoch, h.e.bcs["ABC234"].window.Len()
	h.e.mu.Unlock()
	if epoch != "e2" || n != 4 {
		t.Fatalf("stale listing reset the session: epoch %s, %d samples", epoch, n)
	}
	// Neither does a late frame of e1.
	h.rel.mu.Lock()
	h.rel.scripts["ABC234"] = []step{{score: 0.1, epoch: "e1"}}
	h.rel.mu.Unlock()
	h.sampleOnce("ABC234")
	h.e.mu.Lock()
	epoch, n = h.e.bcs["ABC234"].epoch, h.e.bcs["ABC234"].window.Len()
	h.e.mu.Unlock()
	if epoch != "e2" || n != 4 {
		t.Fatalf("a retired session's frame counted: epoch %s, %d samples", epoch, n)
	}
}

// CF5: a tripped broadcast that drops out of the target list gets no
// further POST — not even from a sample in flight — and if it returns, its
// window and recent start empty.
func TestLeavingTheListEndsPosting(t *testing.T) {
	h := newHarness(t, screening.ModeFlag, func(o *Options) { o.ReportSamples = 6 })
	h.setTargets(target("ABC234", "e1"))
	h.rel.script("ABC234", "e1", scores(0.9, 0.9, 0.9, 0.9, 0.9, 0.9, 0.9, 0.9, 0.9, 0.9)...)
	for i := 0; i < 5; i++ {
		h.sampleOnce("ABC234")
	}
	h.adm.takeFlags()

	h.e.mu.Lock()
	inFlight := h.e.bcs["ABC234"]
	inFlight.busy = true
	h.e.mu.Unlock()
	h.setTargets() // the publisher went away: the broadcast leaves the list
	h.e.sample(h.ctx, inFlight)
	if fs := h.adm.takeFlags(); len(fs) != 0 {
		t.Fatalf("a broadcast off the list POSTed: %+v", fs)
	}

	h.setTargets(target("ABC234", "e1"))
	for i := 0; i < 3; i++ {
		h.sampleOnce("ABC234")
	}
	if fs := h.adm.takeFlags(); len(fs) != 0 {
		t.Fatalf("the returning broadcast kept its old window: %+v", fs)
	}
	h.sampleOnce("ABC234")
	if fs := h.adm.takeFlags(); len(fs) != 1 || len(fs[0].Recent) != 4 {
		t.Fatalf("the returning broadcast kept its old recent: %+v", fs)
	}
}

// CF5/D20: below-floor samples count but are not sent; a 404, a decode
// failure neither count nor enter the window; a 304 counts with the previous
// score; each batch reports Taken and every sent sample's position.
func TestSampleAccounting(t *testing.T) {
	h := newHarness(t, screening.ModeObserve, nil)
	h.setTargets(target("ABC234", "e1"))
	h.rel.script("ABC234", "e1",
		step{score: 0.1},                 // taken 1, below the floor
		step{score: 0.5},                 // taken 2, sent
		step{same: true},                 // 304: taken 3, repeats 0.5, sent
		step{err: relay.ErrNotFound},     // no sample
		step{bad: true},                  // decode failure: no sample
		step{err: errors.New("timeout")}, // network: no sample
		step{score: 0.2},                 // taken 4
		step{score: 0.9},                 // taken 5, sent
	)
	for i := 0; i < 8; i++ {
		h.sampleOnce("ABC234")
	}
	h.e.mu.Lock()
	n := h.e.bcs["ABC234"].window.Len()
	h.e.mu.Unlock()
	if n != 5 {
		t.Fatalf("window holds %d samples, want 5", n)
	}
	h.e.flush(h.ctx)
	if len(h.adm.batches) != 1 {
		t.Fatalf("%d batches", len(h.adm.batches))
	}
	b := h.adm.batches[0]
	if b.Mode != screening.ModeObserve || b.Floor != 0.3 || b.SamplerID != "gawk-sampler-1" || len(b.Sessions) != 1 {
		t.Fatalf("batch %+v", b)
	}
	s := b.Sessions[0]
	var pos []int
	var vals []float64
	for _, sm := range s.Samples {
		pos = append(pos, sm.Position)
		vals = append(vals, sm.Scores[label])
	}
	if s.Taken != 5 || s.PublisherEpoch != "e1" || !reflect.DeepEqual(pos, []int{2, 3, 5}) || !reflect.DeepEqual(vals, []float64{0.5, 0.5, 0.9}) {
		t.Fatalf("session taken %d, positions %v, scores %v", s.Taken, pos, vals)
	}
	for _, sm := range s.Samples {
		if len(sm.Scores) != len(screening.DefaultLabels()) {
			t.Fatalf("a sample scores %d labels, want every flagged label", len(sm.Scores))
		}
	}
	if got := testutil.ToFloat64(h.opts.Metrics.failures.WithLabelValues("not_found")); got != 1 {
		t.Fatalf("not_found failures %v", got)
	}
	if got := testutil.ToFloat64(h.opts.Metrics.samples.WithLabelValues("unchanged")); got != 1 {
		t.Fatalf("unchanged samples %v", got)
	}
	// A flush with nothing new sends nothing.
	h.e.flush(h.ctx)
	if len(h.adm.batches) != 1 {
		t.Fatal("an empty batch was sent")
	}
}

// A batch gawk-admin refused goes back in front of what was taken since, so
// the next batch carries one consistent count and positions.
func TestFailedBatchMergesIntoTheNext(t *testing.T) {
	h := newHarness(t, screening.ModeObserve, nil)
	h.setTargets(target("ABC234", "e1"))
	h.rel.script("ABC234", "e1", scores(0.5, 0.1, 0.6, 0.7)...)
	h.sampleOnce("ABC234")
	h.sampleOnce("ABC234")
	h.adm.batchErr = errors.New("503")
	h.e.flush(h.ctx)
	h.adm.batchErr = nil
	h.sampleOnce("ABC234")
	h.sampleOnce("ABC234")
	h.e.flush(h.ctx)
	if len(h.adm.batches) != 1 {
		t.Fatalf("%d batches", len(h.adm.batches))
	}
	s := h.adm.batches[0].Sessions[0]
	var pos []int
	for _, sm := range s.Samples {
		pos = append(pos, sm.Position)
	}
	if s.Taken != 4 || !reflect.DeepEqual(pos, []int{1, 3, 4}) {
		t.Fatalf("merged batch: taken %d, positions %v", s.Taken, pos)
	}
}

// CF5: in observe mode a tripped window POSTs with mode observe, and the
// sampler does not pause without a flag route (D19 is exempt from D12).
func TestObserveMode(t *testing.T) {
	h := newHarness(t, screening.ModeObserve, nil)
	h.adm.flagRoute = false
	h.setTargets(target("ABC234", "e1"))
	if h.e.paused {
		t.Fatal("observe mode paused without a flag route")
	}
	h.rel.script("ABC234", "e1", scores(0.9, 0.9, 0.9, 0.9)...)
	for i := 0; i < 4; i++ {
		h.sampleOnce("ABC234")
	}
	fs := h.adm.takeFlags()
	if len(fs) != 1 || fs[0].Mode != screening.ModeObserve {
		t.Fatalf("observe flags %+v", fs)
	}
}

// D12: a 503 no_flag_route on a POST pauses a flag-mode sampler at once and
// drops its windows.
func TestNoFlagRoutePausesAtOnce(t *testing.T) {
	h := newHarness(t, screening.ModeFlag, nil)
	h.setTargets(target("ABC234", "e1"))
	h.rel.script("ABC234", "e1", scores(0.9, 0.9, 0.9, 0.9)...)
	h.adm.flagErr = admin.ErrNoFlagRoute
	for i := 0; i < 4; i++ {
		h.sampleOnce("ABC234")
	}
	h.e.mu.Lock()
	paused, n := h.e.paused, h.e.bcs["ABC234"].window.Len()
	h.e.mu.Unlock()
	if !paused || n != 0 {
		t.Fatalf("paused %v, window %d", paused, n)
	}
	if got := testutil.ToFloat64(h.opts.Metrics.paused.WithLabelValues("no_flag_route")); got != 1 {
		t.Fatalf("paused gauge %v", got)
	}
}

// A cleared POST that did not land is retried on the next sample.
func TestClearedPostIsRetried(t *testing.T) {
	h := newHarness(t, screening.ModeFlag, func(o *Options) { o.Rule = screening.WindowRule{Threshold: 0.8, Size: 2, Hits: 1} })
	h.setTargets(target("ABC234", "e1"))
	h.rel.script("ABC234", "e1", scores(0.9, 0.1, 0.1, 0.1, 0.1)...)
	h.sampleOnce("ABC234")
	h.sampleOnce("ABC234")
	h.adm.flagErr = errors.New("gawk-admin unreachable")
	h.sampleOnce("ABC234") // clears; the POST fails
	h.adm.flagErr = nil
	h.sampleOnce("ABC234") // retried
	h.sampleOnce("ABC234") // landed: nothing more
	if got := states(h.adm.takeFlags()); !reflect.DeepEqual(got, []string{"tripped", "tripped", "cleared"}) {
		t.Fatalf("posts %v", got)
	}
}

// run starts the engine and returns a stop function.
func (h *harness) run() func() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.e.Run(ctx); close(done) }()
	return func() { cancel(); <-done }
}

func (h *harness) fetches(id string) int {
	h.rel.mu.Lock()
	defer h.rel.mu.Unlock()
	return h.rel.fetches[id]
}

// CF5/D12: in flag mode with flagRoute false nothing is sampled — and once
// the route exists, sampling starts.
func TestFlagModeWithoutRouteSamplesNothing(t *testing.T) {
	h := newHarness(t, screening.ModeFlag, func(o *Options) {
		o.TargetsRefresh = 20 * time.Millisecond
		o.PausedLogEvery = time.Hour
	})
	h.adm.flagRoute = false
	h.adm.targets = []screening.Target{target("ABC234", "e1")}
	h.rel.script("ABC234", "e1", scores(0.1)...)
	stop := h.run()
	time.Sleep(300 * time.Millisecond)
	if n := h.fetches("ABC234"); n != 0 {
		stop()
		t.Fatalf("%d fetches while paused", n)
	}
	if !strings.Contains(h.logs.String(), "sampling nothing") {
		stop()
		t.Fatalf("the pause was not logged:\n%s", h.logs.String())
	}
	h.adm.mu.Lock()
	h.adm.flagRoute = true
	h.adm.mu.Unlock()
	time.Sleep(300 * time.Millisecond)
	stop()
	if n := h.fetches("ABC234"); n == 0 {
		t.Fatal("no fetch after the route came back")
	}
	// D22: every poll names this pod's share.
	for _, p := range h.adm.polls {
		if p != [2]int{1, 2} {
			t.Fatalf("poll %v, want shard 1 of 2", p)
		}
	}
}

// D11: exempt broadcasts are listed and never sampled; muted ones are
// sampled.
func TestExemptNeverSampledMutedIs(t *testing.T) {
	h := newHarness(t, screening.ModeFlag, nil)
	ex := target("EXEMP2", "e1")
	ex.Exempt = true
	mu := target("MUTED2", "e2")
	mu.Muted = true
	h.adm.targets = []screening.Target{ex, mu}
	h.rel.script("EXEMP2", "e1", scores(0.9)...)
	h.rel.script("MUTED2", "e2", scores(0.1)...)
	stop := h.run()
	time.Sleep(300 * time.Millisecond)
	stop()
	if n := h.fetches("EXEMP2"); n != 0 {
		t.Fatalf("an exempt broadcast was fetched %d times", n)
	}
	if n := h.fetches("MUTED2"); n == 0 {
		t.Fatal("a muted broadcast was not sampled")
	}
	if got := testutil.ToFloat64(h.opts.Metrics.targets.WithLabelValues("exempt")); got != 1 {
		t.Fatalf("exempt gauge %v", got)
	}
}

// CF5: a broadcast whose frames never decode fails only its own samples; the
// others keep being sampled and counted.
func TestBadFramesFailOnlyTheirBroadcast(t *testing.T) {
	h := newHarness(t, screening.ModeObserve, func(o *Options) { o.BatchInterval = 50 * time.Millisecond })
	h.adm.targets = []screening.Target{target("BADBAD", "e1"), target("GDDD22", "e2"), target("GDDD33", "e3")}
	h.rel.script("BADBAD", "e1", step{bad: true})
	h.rel.script("GDDD22", "e2", scores(0.5)...)
	h.rel.script("GDDD33", "e3", scores(0.5)...)
	stop := h.run()
	time.Sleep(400 * time.Millisecond)
	stop()
	taken := map[string]int{}
	for _, b := range h.adm.batches {
		for _, s := range b.Sessions {
			taken[s.BroadcastID] += s.Taken
		}
	}
	if taken["BADBAD"] != 0 || taken["GDDD22"] < 2 || taken["GDDD33"] < 2 {
		t.Fatalf("samples taken %v", taken)
	}
	if h.fetches("BADBAD") < 2 {
		t.Fatalf("the bad broadcast left the rotation after %d fetches", h.fetches("BADBAD"))
	}
}

// CF5/D6: at 3× overload every broadcast's interval stretches by the same
// factor and none is left unsampled.
func TestOverloadStretchesEvenly(t *testing.T) {
	ids := []string{"AAAAA2", "BBBBB2", "CCCCC2", "DDDDD2", "EEEEE2", "FFFFF2"}
	run := func(workers int) map[string]time.Duration {
		h := newHarness(t, screening.ModeObserve, func(o *Options) {
			o.Interval = 300 * time.Millisecond
			o.Workers = workers
		})
		h.rel.delay = 150 * time.Millisecond
		for _, id := range ids {
			h.adm.targets = append(h.adm.targets, target(id, "e-"+id))
			h.rel.script(id, "e-"+id, scores(0.1)...)
		}
		stop := h.run()
		time.Sleep(3 * time.Second)
		got := h.e.EffectiveIntervals()
		eff := testutil.ToFloat64(h.opts.Metrics.effective)
		stop()
		for _, id := range ids {
			if h.fetches(id) < 2 {
				t.Fatalf("workers=%d: %s sampled %d times", workers, id, h.fetches(id))
			}
		}
		longest := time.Duration(0)
		for _, d := range got {
			longest = max(longest, d)
		}
		if diff := eff - longest.Seconds(); diff > 1e-9 || diff < -1e-9 {
			t.Fatalf("gauge %v, longest interval %v", eff, longest)
		}
		return got
	}
	// One worker, 150 ms a sample, six broadcasts every 300 ms: 3× the work
	// one worker can do, so each interval stretches to ~900 ms.
	for id, d := range run(1) {
		if d < 750*time.Millisecond || d > 1150*time.Millisecond {
			t.Errorf("overloaded: %s interval %s, want ~900ms", id, d)
		}
	}
	// Enough workers: the target interval holds.
	for id, d := range run(6) {
		if d < 250*time.Millisecond || d > 450*time.Millisecond {
			t.Errorf("unloaded: %s interval %s, want ~300ms", id, d)
		}
	}
}

// D8: no frame byte reaches a log line or a metric, whatever fails.
func TestNoFrameBytesInLogsOrMetrics(t *testing.T) {
	h := newHarness(t, screening.ModeFlag, func(o *Options) { o.BatchInterval = 50 * time.Millisecond })
	h.adm.targets = []screening.Target{target("BADBAD", "e1"), target("GDDD22", "e2")}
	h.rel.script("BADBAD", "e1", step{bad: true})
	h.rel.script("GDDD22", "e2", scores(0.9, 0.9, 0.9, 0.9, 0.9, 0.1)...)
	stop := h.run()
	time.Sleep(500 * time.Millisecond)
	stop()
	var metrics bytes.Buffer
	mfs, err := h.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		metrics.WriteString(mf.String())
	}
	marker := []byte(frameMarker)
	for name, text := range map[string]string{"logs": h.logs.String(), "metrics": metrics.String()} {
		for _, form := range []string{frameMarker, hex.EncodeToString(marker), base64.StdEncoding.EncodeToString(marker), base64.StdEncoding.EncodeToString(marker[:12])} {
			if strings.Contains(text, form) {
				t.Fatalf("frame bytes in the %s:\n%s", name, text)
			}
		}
	}
	if !strings.Contains(h.logs.String(), "keyframe decode failed") {
		t.Fatal("the failure path was not exercised")
	}
}
