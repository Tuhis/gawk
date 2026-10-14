package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-admin/internal/store"
	"github.com/Tuhis/gawk/gawk-admin/internal/store/storetest"
	"github.com/Tuhis/gawk/gawk-server/events"
	"github.com/Tuhis/gawk/gawk-server/screening"
)

// R70's incident lifecycle (docs/72 D3–D5) on top of R40's flags.

const rejoin = 10 * time.Minute

// postRejoin is post with the rejoin window on.
func postRejoin(t *testing.T, s *store.Store, body screening.ContentFlag, at time.Time) store.FlagPostResult {
	t.Helper()
	res, err := s.PostContentFlag(t.Context(), store.FlagPost{Body: body, Actor: "gawk-sampler", Now: at, Rejoin: rejoin}, nil)
	if err != nil {
		t.Fatalf("post %s/%s %s: %v", body.BroadcastID, body.PublisherEpoch, body.State, err)
	}
	return res
}

func tripped(id, epoch string) screening.ContentFlag {
	return flagBody(id, epoch, screening.ModeFlag, screening.StateTripped)
}

func cleared(id, epoch string) screening.ContentFlag {
	return flagBody(id, epoch, screening.ModeFlag, screening.StateCleared)
}

// D3: with the rejoin window on, a clear of an open flag is held — the flag
// stays unresolved, its signal reads cleared, nothing is written to the feed —
// and a trip inside the window rejoins the same flag without a second page.
func TestHeldClearRejoinsWithinTheWindow(t *testing.T) {
	s := storetest.New(t)
	f := postRejoin(t, s, tripped("ABC234", "e1"), t0).Flag
	if f.Trips != 1 {
		t.Fatalf("a raised flag has %d trips, want 1", f.Trips)
	}

	held := postRejoin(t, s, cleared("ABC234", "e1"), t0.Add(time.Minute))
	if held.Status != screening.FlagStatusUpdated || held.Flag.ID != f.ID || !held.Flag.Unresolved() ||
		held.Flag.ClearedAt == nil || !held.Flag.ClearedAt.Equal(t0.Add(time.Minute)) {
		t.Fatalf("a clear of an open flag: %+v, want it held unresolved", held)
	}
	if sig := held.Flag.Signal(t0.Add(time.Minute), 2*time.Minute); sig != "cleared" {
		t.Fatalf("a held clear's signal = %q, want cleared (no reminders)", sig)
	}
	if n := len(eventsOfType(t, s, store.EventContentFlagResolved)); n != 0 {
		t.Fatalf("resolved events after a held clear = %d, want 0", n)
	}

	back := postRejoin(t, s, tripped("ABC234", "e1"), t0.Add(4*time.Minute))
	if back.Status != screening.FlagStatusUpdated || back.Flag.ID != f.ID || back.Flag.Trips != 2 || back.Flag.ClearedAt != nil {
		t.Fatalf("a re-trip inside the window: %+v, want the same flag with 2 trips", back)
	}
	if n := len(eventsOfType(t, s, store.EventContentFlag)); n != 1 {
		t.Fatalf("raised events = %d, want the first page only", n)
	}
}

// D3: the sweep resolves a held clear once the window has passed, with R40's
// one resolved event, and not before.
func TestSweepResolvesHeldClearsAfterTheWindow(t *testing.T) {
	s := storetest.New(t)
	ctx := t.Context()
	f := postRejoin(t, s, tripped("ABC234", "e1"), t0).Flag
	postRejoin(t, s, cleared("ABC234", "e1"), t0.Add(time.Minute))

	early, err := s.ResolveHeldClears(ctx, rejoin, t0.Add(5*time.Minute), nil)
	if err != nil || len(early) != 0 {
		t.Fatalf("sweep inside the window resolved %+v, %v; want nothing", early, err)
	}
	done, err := s.ResolveHeldClears(ctx, rejoin, t0.Add(12*time.Minute), nil)
	if err != nil || len(done) != 1 || done[0].ID != f.ID || done[0].Resolution != events.ContentFlagResolutionCleared {
		t.Fatalf("sweep after the window = %+v, %v; want flag %d cleared", done, err, f.ID)
	}
	res := eventsOfType(t, s, store.EventContentFlagResolved)
	if len(res) != 1 || res[0].ContentFlag().Resolution != events.ContentFlagResolutionCleared {
		t.Fatalf("resolved events = %+v, want one cleared", res)
	}
	// A trip after the resolution is a new incident and pages.
	if r := postRejoin(t, s, tripped("ABC234", "e1"), t0.Add(13*time.Minute)); r.Status != screening.FlagStatusRaised || r.Flag.ID == f.ID {
		t.Fatalf("a trip after the resolved clear: %+v, want a new raise", r)
	}
}

// D3: a trip that arrives after the window but before the sweep ran must not
// rejoin: the old incident resolves cleared and a new one is raised.
func TestATripAfterTheWindowDoesNotRejoin(t *testing.T) {
	s := storetest.New(t)
	f := postRejoin(t, s, tripped("ABC234", "e1"), t0).Flag
	postRejoin(t, s, cleared("ABC234", "e1"), t0.Add(time.Minute))
	r := postRejoin(t, s, tripped("ABC234", "e1"), t0.Add(20*time.Minute))
	if r.Status != screening.FlagStatusRaised || r.Flag.ID == f.ID {
		t.Fatalf("a trip past the window: %+v, want a new raise", r)
	}
	old, err := s.GetContentFlag(t.Context(), f.ID)
	if err != nil || old.Resolution != events.ContentFlagResolutionCleared {
		t.Fatalf("the old flag = %+v, %v; want resolved cleared", old, err)
	}
}

// D3: an acknowledged flag clears at once, as in R40, and a held clear that is
// acknowledged afterwards is resolved by the next sweep, window or not.
func TestAcknowledgedFlagsDoNotWaitForTheWindow(t *testing.T) {
	s := storetest.New(t)
	ctx := t.Context()
	f := postRejoin(t, s, tripped("ABC234", "e1"), t0).Flag
	if _, err := s.AcknowledgeContentFlag(ctx, f.ID, "op@example.com", t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if r := postRejoin(t, s, cleared("ABC234", "e1"), t0.Add(2*time.Minute)); r.Status != screening.FlagStatusResolved {
		t.Fatalf("a clear of an acknowledged flag: %q, want resolved at once", r.Status)
	}

	g := postRejoin(t, s, tripped("DEF567", "e2"), t0).Flag
	postRejoin(t, s, cleared("DEF567", "e2"), t0.Add(time.Minute))
	if _, err := s.AcknowledgeContentFlag(ctx, g.ID, "op@example.com", t0.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	done, err := s.ResolveHeldClears(ctx, rejoin, t0.Add(3*time.Minute), nil)
	if err != nil || len(done) != 1 || done[0].ID != g.ID {
		t.Fatalf("sweep of an acknowledged held clear = %+v, %v; want it resolved", done, err)
	}
}

// D3: with the window off (zero) a clear resolves at once: R40's behaviour.
func TestRejoinOffResolvesAtOnce(t *testing.T) {
	s := storetest.New(t)
	post(t, s, tripped("ABC234", "e1"), "", t0)
	if r := post(t, s, cleared("ABC234", "e1"), "", t0.Add(time.Minute)); r.Status != screening.FlagStatusResolved {
		t.Fatalf("a clear with rejoin off: %q, want resolved", r.Status)
	}
}

// D3: observed flags hold their clears too, and never write an event.
func TestObservedFlagsHoldClearsSilently(t *testing.T) {
	s := storetest.New(t)
	ctx := t.Context()
	f := postRejoin(t, s, flagBody("ABC234", "e1", screening.ModeObserve, screening.StateTripped), t0).Flag
	postRejoin(t, s, flagBody("ABC234", "e1", screening.ModeObserve, screening.StateCleared), t0.Add(time.Minute))
	back := postRejoin(t, s, flagBody("ABC234", "e1", screening.ModeObserve, screening.StateTripped), t0.Add(2*time.Minute))
	if back.Flag.ID != f.ID || back.Flag.Trips != 2 {
		t.Fatalf("an observed re-trip: %+v, want the same flag with 2 trips", back)
	}
	postRejoin(t, s, flagBody("ABC234", "e1", screening.ModeObserve, screening.StateCleared), t0.Add(3*time.Minute))
	if done, err := s.ResolveHeldClears(ctx, rejoin, t0.Add(20*time.Minute), nil); err != nil || len(done) != 1 {
		t.Fatalf("sweep = %+v, %v", done, err)
	}
	if n := len(eventsOfType(t, s, store.EventContentFlagResolved)); n != 0 {
		t.Fatalf("resolved events for an observed flag = %d, want 0", n)
	}
	if n := len(eventsOfType(t, s, store.EventContentFlagObserved)); n != 1 {
		t.Fatalf("observed events = %d, want one", n)
	}
}

// D4: a raise after an acknowledged or dismissed incident on the same
// broadcast is returned-after-review, in the row and in the page; a raise
// after an incident that only cleared is not, nor is an observed one.
func TestReturnedAfterReview(t *testing.T) {
	s := storetest.New(t)
	ctx := t.Context()

	// Only cleared, never reviewed.
	post(t, s, tripped("ABC234", "e1"), "", t0)
	post(t, s, cleared("ABC234", "e1"), "", t0.Add(time.Minute))
	plain := post(t, s, tripped("ABC234", "e1"), "", t0.Add(2*time.Minute)).Flag
	if plain.ReturnedAfterReview {
		t.Fatal("a raise after an unreviewed clear is marked returned after review")
	}

	// Acknowledged, then cleared: the next raise is a return.
	if _, err := s.AcknowledgeContentFlag(ctx, plain.ID, "op@example.com", t0.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	post(t, s, cleared("ABC234", "e1"), "", t0.Add(4*time.Minute))
	back := post(t, s, tripped("ABC234", "e1"), "", t0.Add(5*time.Minute)).Flag
	if !back.ReturnedAfterReview {
		t.Fatal("a raise after an acknowledged incident is not marked returned after review")
	}
	raised := eventsOfType(t, s, store.EventContentFlag)
	if len(raised) != 3 || !raised[0].ContentFlag().ReturnedAfterReview || raised[1].ContentFlag().ReturnedAfterReview {
		t.Fatalf("raised events = %+v; want only the newest one returned after review", raised)
	}

	// Dismissed, mute over: the next raise is a return.
	if _, err := s.DismissContentFlag(ctx, back.ID, "op@example.com", t0.Add(6*time.Minute),
		store.Dismissal{Scope: store.MuteScopeDuration, MutedUntil: t0.Add(7 * time.Minute)}, nil); err != nil {
		t.Fatal(err)
	}
	if again := post(t, s, tripped("ABC234", "e1"), "", t0.Add(8*time.Minute)).Flag; !again.ReturnedAfterReview {
		t.Fatal("a raise after a dismissed incident is not marked returned after review")
	}

	// A review a day ago does not count.
	if old := post(t, s, tripped("XYZ789", "e9"), "", t0).Flag; old.ReturnedAfterReview {
		t.Fatal("a first raise is marked returned after review")
	} else {
		if _, err := s.AcknowledgeContentFlag(ctx, old.ID, "op@example.com", t0.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		post(t, s, cleared("XYZ789", "e9"), "", t0.Add(2*time.Minute))
		if late := post(t, s, tripped("XYZ789", "e9"), "", t0.Add(25*time.Hour)).Flag; late.ReturnedAfterReview {
			t.Fatal("a review 25 hours earlier still marks a raise as returned")
		}
	}

	// Observe mode is never a return.
	if obs := post(t, s, flagBody("ABC234", "e1", screening.ModeObserve, screening.StateTripped), "", t0.Add(9*time.Minute)).Flag; obs.ReturnedAfterReview {
		t.Fatal("an observed flag is marked returned after review")
	}
}

// D5: a dismissal records its reason and scope on the flag and in the event,
// and the three scopes end the mute where they say.
func TestDismissReasonAndScope(t *testing.T) {
	s := storetest.New(t)
	ctx := t.Context()

	// episode: muted until the sampler reports the window cleared for the
	// session, then a trip pages again.
	f := post(t, s, tripped("ABC234", "e1"), "", t0).Flag
	out, err := s.DismissContentFlag(ctx, f.ID, "op@example.com", t0.Add(time.Minute),
		store.Dismissal{Reason: store.DismissReasonGameScene, Scope: store.MuteScopeEpisode, MutedUntil: t0.Add(24 * time.Hour)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.ResolutionReason != store.DismissReasonGameScene || out.MuteScope != store.MuteScopeEpisode || out.MutedUntil == nil {
		t.Fatalf("dismissed flag = %+v", out)
	}
	res := eventsOfType(t, s, store.EventContentFlagResolved)
	if len(res) != 1 || res[0].ContentFlag().Reason != store.DismissReasonGameScene {
		t.Fatalf("resolved event reason = %+v, want game_scene", res)
	}
	if r := post(t, s, tripped("ABC234", "e1"), "", t0.Add(2*time.Minute)); r.Status != screening.FlagStatusMuted {
		t.Fatalf("a trip under an episode mute: %q, want muted", r.Status)
	}
	if r := post(t, s, cleared("ABC234", "e1"), "", t0.Add(3*time.Minute)); r.Status != screening.FlagStatusMuted {
		t.Fatalf("the clear that ends an episode mute: %q, want muted", r.Status)
	}
	if mutes, _ := s.ActiveMutes(ctx, t0.Add(3*time.Minute)); len(mutes) != 0 {
		t.Fatalf("mutes after the episode cleared = %v, want none", mutes)
	}
	ended, _ := s.GetContentFlag(ctx, f.ID)
	if ended.MutedUntil == nil || !ended.MutedUntil.Equal(t0.Add(3*time.Minute)) {
		t.Fatalf("the flag's muted_until after the episode = %v, want the clear's time", ended.MutedUntil)
	}
	if r := post(t, s, tripped("ABC234", "e1"), "", t0.Add(4*time.Minute)); r.Status != screening.FlagStatusRaised {
		t.Fatalf("a trip after the episode: %q, want raised", r.Status)
	}

	// session: muted for the rest of the session and not for the next one.
	g := post(t, s, tripped("DEF567", "e2"), "", t0).Flag
	if _, err := s.DismissContentFlag(ctx, g.ID, "op@example.com", t0.Add(time.Minute),
		store.Dismissal{Reason: store.DismissReasonNotExplicit, Scope: store.MuteScopeSession, MutedUntil: t0.Add(24 * time.Hour)}, nil); err != nil {
		t.Fatal(err)
	}
	mutes, err := s.ActiveMutes(ctx, t0.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	m, ok := mutes["DEF567"]
	if !ok || !m.Covers("e2") || m.Covers("e3") || m.Scope != store.MuteScopeSession {
		t.Fatalf("session mute = %+v, want it to cover e2 only", m)
	}
	if r := post(t, s, cleared("DEF567", "e2"), "", t0.Add(3*time.Minute)); r.Status != screening.FlagStatusMuted {
		t.Fatalf("a clear under a session mute: %q, want muted (the mute stays)", r.Status)
	}
	if r := post(t, s, tripped("DEF567", "e2"), "", t0.Add(4*time.Minute)); r.Status != screening.FlagStatusMuted {
		t.Fatalf("a trip under a session mute: %q, want muted", r.Status)
	}
	if r := post(t, s, tripped("DEF567", "e3"), "", t0.Add(5*time.Minute)); r.Status != screening.FlagStatusRaised {
		t.Fatalf("a trip in a new publisher session: %q, want raised", r.Status)
	}

	// duration: R40's mute, covering every session until it ends.
	h := post(t, s, tripped("GHJ234", "e4"), "", t0).Flag
	if _, err := s.DismissContentFlag(ctx, h.ID, "op@example.com", t0.Add(time.Minute),
		store.Dismissal{Scope: store.MuteScopeDuration, MutedUntil: t0.Add(30 * time.Minute)}, nil); err != nil {
		t.Fatal(err)
	}
	if r := post(t, s, tripped("GHJ234", "e5"), "", t0.Add(2*time.Minute)); r.Status != screening.FlagStatusMuted {
		t.Fatalf("a new session under a duration mute: %q, want muted", r.Status)
	}

	// The vocabulary is closed.
	k := post(t, s, tripped("KLM234", "e6"), "", t0).Flag
	if _, err := s.DismissContentFlag(ctx, k.ID, "op@example.com", t0, store.Dismissal{Reason: "boring", Scope: store.MuteScopeDuration, MutedUntil: t0.Add(time.Hour)}, nil); !errors.Is(err, store.ErrInvalidDismissal) {
		t.Fatalf("an unknown reason = %v, want ErrInvalidDismissal", err)
	}
	if _, err := s.DismissContentFlag(ctx, k.ID, "op@example.com", t0, store.Dismissal{Scope: "forever", MutedUntil: t0.Add(time.Hour)}, nil); !errors.Is(err, store.ErrInvalidDismissal) {
		t.Fatalf("an unknown scope = %v, want ErrInvalidDismissal", err)
	}
}

// D1: each batch's rule lands on its session; a batch without one leaves the
// stored rule alone, and a later batch replaces it.
func TestIngestRecordsTheRule(t *testing.T) {
	s := storetest.New(t)
	ctx := t.Context()
	batch := func(rule *screening.BatchRule, at time.Time) screening.SampleBatch {
		return screening.SampleBatch{
			Schema: screening.SchemaSamples, SamplerID: "gawk-sampler-0", Mode: screening.ModeObserve,
			Floor: 0.3, Labels: screening.DefaultLabels(), Rule: rule,
			Sessions: []screening.SessionSamples{{
				BroadcastID: "ABC234", BroadcastKey: "0123456789ab", PublisherEpoch: "e1",
				Taken: 1, FirstAt: at, LastAt: at,
			}},
		}
	}
	if _, err := s.IngestSamples(ctx, batch(nil, t0)); err != nil {
		t.Fatal(err)
	}
	sess, err := s.ScreeningSessionsForKey(ctx, "0123456789ab", t0.Add(-time.Hour), t0.Add(time.Hour))
	if err != nil || len(sess) != 1 || sess[0].Rule != nil {
		t.Fatalf("a session without a rule = %+v, %v; want Rule nil", sess, err)
	}
	if _, err := s.IngestSamples(ctx, batch(&screening.BatchRule{Threshold: 0.5, Window: 6, WindowHits: 3}, t0.Add(5*time.Second))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.IngestSamples(ctx, batch(nil, t0.Add(10*time.Second))); err != nil {
		t.Fatal(err)
	}
	sess, _ = s.ScreeningSessionsForKey(ctx, "0123456789ab", t0.Add(-time.Hour), t0.Add(time.Hour))
	if len(sess) != 1 || sess[0].Rule == nil || *sess[0].Rule != (screening.BatchRule{Threshold: 0.5, Window: 6, WindowHits: 3}) ||
		sess[0].SamplesTaken != 3 || sess[0].Floor != 0.3 || sess[0].BroadcastID != "ABC234" {
		t.Fatalf("session = %+v", sess)
	}
	if _, err := s.IngestSamples(ctx, batch(&screening.BatchRule{Threshold: 0.6, Window: 8, WindowHits: 4}, t0.Add(15*time.Second))); err != nil {
		t.Fatal(err)
	}
	sess, _ = s.ScreeningSessionsForKey(ctx, "0123456789ab", t0.Add(-time.Hour), t0.Add(time.Hour))
	if *sess[0].Rule != (screening.BatchRule{Threshold: 0.6, Window: 8, WindowHits: 4}) {
		t.Fatalf("rule after a newer batch = %+v, want the newest", sess[0].Rule)
	}
	if other, _ := s.ScreeningSessionsForKey(ctx, "ffffffffffff", t0.Add(-time.Hour), t0.Add(time.Hour)); len(other) != 0 {
		t.Fatalf("another key's sessions = %+v, want none", other)
	}
}
