package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-admin/internal/store"
	"github.com/Tuhis/gawk/gawk-server/events"
	"github.com/Tuhis/gawk/gawk-server/screening"
)

// R40's leader loop (docs/71 D9, D17): RunScreening prunes once at start,
// then sweeps, and returns when leadership is lost.

// oldObservedFlag stores a resolved observe-mode flag raised at `at`, the
// calibration row D17 ages out.
func oldObservedFlag(t *testing.T, h *harness, id string, at time.Time) store.ContentFlag {
	t.Helper()
	ctx := t.Context()
	res, err := h.store.PostContentFlag(ctx, store.FlagPost{
		Body: report(id, "e-"+id, screening.ModeObserve, screening.StateTripped), Actor: "gawk-sampler", Now: at,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := h.store.ResolveContentFlag(ctx, res.Flag, events.ContentFlagResolutionEnded, "", "system", at, nil)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The loop's first pass does both jobs without waiting a tick: calibration
// data past the retention is pruned and a due reminder goes out. Cancelling
// the context — leadership lost — returns it.
func TestRunScreeningPrunesSweepsAndStopsWithLeadership(t *testing.T) {
	h, clock := screeningHarness(t)
	stale := oldObservedFlag(t, h, "OLD234", flagT0.Add(-40*24*time.Hour))
	raised := h.postFlag(report(flagID, flagEpoch, screening.ModeFlag, screening.StateTripped), http.StatusCreated)
	clock.advance(11 * time.Minute) // past the 10 m repage

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.api.RunScreening(ctx)
	}()
	waitFor(t, "the first sweep's reminder", func() bool {
		return len(h.flagEvents(store.EventContentFlagReminded)) == 1
	})
	waitFor(t, "the start-up prune", func() bool {
		_, err := h.store.GetContentFlag(t.Context(), stale.ID)
		return err != nil
	})
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunScreening did not return after its context was cancelled")
	}

	if f := h.flag(raised.FlagID); f.Reminders != 1 || !f.Unresolved() {
		t.Fatalf("the live flag after one sweep = %+v, want one reminder and still unresolved", f)
	}
	logs := h.logText()
	for _, want := range []string{"content-flag sweep started", "pruned screening data", "content flag re-paged"} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs lack %q:\n%s", want, logs)
		}
	}
}

// Screening off: the leader has nothing to run and returns at once.
func TestRunScreeningIsANoOpWithScreeningOff(t *testing.T) {
	h := newHarness(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.api.RunScreening(t.Context())
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunScreening kept running with screening off")
	}
	if strings.Contains(h.logText(), "content-flag sweep started") {
		t.Fatal("a sweep started with screening off")
	}
}

// D17: the prune cuts at -screening-sample-retention before now — an observed
// flag just past it goes, one just inside it stays — and never touches a
// flag-mode flag, however old.
func TestPruneScreeningHonoursTheRetention(t *testing.T) {
	h, _ := screeningHarness(t)
	retention := 30 * 24 * time.Hour
	gone := oldObservedFlag(t, h, "GONE23", flagT0.Add(-retention-time.Hour))
	kept := oldObservedFlag(t, h, "KEPT23", flagT0.Add(-retention+time.Hour))
	audit, err := h.store.PostContentFlag(t.Context(), store.FlagPost{
		Body: report("AUDIT2", "e-audit", screening.ModeFlag, screening.StateTripped), Actor: "gawk-sampler", Now: flagT0.Add(-90 * 24 * time.Hour),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.store.ResolveContentFlag(t.Context(), audit.Flag, events.ContentFlagResolutionDismissed, "", "op@example.com", flagT0.Add(-90*24*time.Hour), nil); err != nil {
		t.Fatal(err)
	}

	h.api.PruneScreening(t.Context())

	if _, err := h.store.GetContentFlag(t.Context(), gone.ID); err == nil {
		t.Error("an observed flag past the retention survived the prune")
	}
	if _, err := h.store.GetContentFlag(t.Context(), kept.ID); err != nil {
		t.Errorf("an observed flag inside the retention was pruned: %v", err)
	}
	if f, err := h.store.GetContentFlag(t.Context(), audit.Flag.ID); err != nil || f.Mode != screening.ModeFlag {
		t.Errorf("the 90-day-old flag-mode flag = %+v, %v; the audit trail is never pruned", f, err)
	}
	if !strings.Contains(h.logText(), "pruned screening data") {
		t.Errorf("the prune was not logged:\n%s", h.logText())
	}
	// A second pass has nothing left to do and says nothing.
	before := strings.Count(h.logText(), "pruned screening data")
	h.api.PruneScreening(t.Context())
	if after := strings.Count(h.logText(), "pruned screening data"); after != before {
		t.Error("an empty prune was logged as if it removed something")
	}
}

// With Postgres gone, the leader's two jobs log and carry on: neither a
// failed prune nor a failed sweep panics or stops the loop's caller.
func TestScreeningJobsSurviveAnUnreachableStore(t *testing.T) {
	h := newHarnessWithoutPostgres(t, withScreening(), withPager())
	h.api.PruneScreening(t.Context())
	h.api.ScreeningTick(t.Context())
	logs := h.logText()
	for _, want := range []string{"screening prune failed", "content-flag sweep: listing flags failed"} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs lack %q:\n%s", want, logs)
		}
	}
}
