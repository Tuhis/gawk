package api_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-admin/internal/config"
	"github.com/Tuhis/gawk/gawk-admin/internal/store"
	"github.com/Tuhis/gawk/gawk-server/events"
	"github.com/Tuhis/gawk/gawk-server/screening"
)

// R70 (docs/72): the incident lifecycle through the API.

func withRejoin(d time.Duration) harnessOption {
	return withConfig(func(c *config.Config) { c.ContentFlagRejoin = d })
}

// D6: a ban on the broadcast's ID — the Violation step's kill-and-ban —
// resolves its open flag `killed` by the operator, as R39's kill does.
func TestABanOnTheBroadcastResolvesItsFlag(t *testing.T) {
	h, _ := screeningHarness(t)
	raised := h.postFlag(report(flagID, flagEpoch, screening.ModeFlag, screening.StateTripped), http.StatusCreated)
	h.asOperator()
	h.decode(http.MethodPost, "/api/v1/bans", map[string]any{
		"target": map[string]any{"type": "broadcastId", "value": flagID},
		"reason": "explicit content", "sourceBroadcastId": flagID,
	}, http.StatusCreated, nil)
	f := h.flag(raised.FlagID)
	if f.Resolution != events.ContentFlagResolutionKilled || f.ResolvedBy != "op@example.com" {
		t.Fatalf("after the ban: %+v", f)
	}
	if res := h.flagEvents(store.EventContentFlagResolved); len(res) != 1 || res[0].ContentFlag().Resolution != events.ContentFlagResolutionKilled {
		t.Fatalf("resolved = %+v", res)
	}
}

// D5: the dismiss body carries a reason and a scope; an unknown one is 400
// and changes nothing.
func TestDismissTakesAReasonAndAScope(t *testing.T) {
	h, clock := screeningHarness(t)
	raised := h.postFlag(report(flagID, flagEpoch, screening.ModeFlag, screening.StateTripped), http.StatusCreated)
	h.asOperator()
	path := "/api/v1/content-flags/" + itoa(raised.FlagID) + "/dismiss"
	h.decode(http.MethodPost, path, map[string]any{"reason": "boring"}, http.StatusBadRequest, nil)
	h.decode(http.MethodPost, path, map[string]any{"scope": "forever"}, http.StatusBadRequest, nil)
	h.decode(http.MethodPost, path, map[string]any{"reason": "game_scene", "extra": 1}, http.StatusBadRequest, nil)
	if f := h.flag(raised.FlagID); !f.Unresolved() {
		t.Fatalf("a refused dismissal changed the flag: %+v", f)
	}

	var out struct {
		Resolution       string `json:"resolution"`
		ResolutionReason string `json:"resolutionReason"`
		MuteScope        string `json:"muteScope"`
		MutedUntil       string `json:"mutedUntil"`
	}
	h.decode(http.MethodPost, path, map[string]any{"reason": "game_scene", "scope": "session"}, http.StatusOK, &out)
	if out.Resolution != events.ContentFlagResolutionDismissed || out.ResolutionReason != "game_scene" || out.MuteScope != "session" ||
		out.MutedUntil != flagT0.Add(24*time.Hour).Format(time.RFC3339) {
		t.Fatalf("dismissed = %+v", out)
	}
	if res := h.flagEvents(store.EventContentFlagResolved); len(res) != 1 || res[0].ContentFlag().Reason != "game_scene" {
		t.Fatalf("resolved = %+v", res)
	}
	// The session is muted; a new publisher session of the same broadcast is
	// not, and the target list says which.
	clock.advance(time.Minute)
	if res := h.postFlag(report(flagID, flagEpoch, screening.ModeFlag, screening.StateTripped), http.StatusAccepted); res.Status != screening.FlagStatusMuted {
		t.Fatalf("same session = %+v, want muted", res)
	}
	var targets screening.TargetsResponse
	h.decode(http.MethodGet, "/api/v1/screening/targets?shard=0&of=1", nil, http.StatusOK, &targets)
	if len(targets.Targets) != 1 || !targets.Targets[0].Muted {
		t.Fatalf("targets = %+v, want the session muted", targets.Targets)
	}
	h.fleet.set(fleetOf(originBroadcast(flagID, flagKey, "e-next")))
	h.decode(http.MethodGet, "/api/v1/screening/targets?shard=0&of=1", nil, http.StatusOK, &targets)
	if len(targets.Targets) != 1 || targets.Targets[0].Muted {
		t.Fatalf("targets after a new session = %+v, want unmuted", targets.Targets)
	}
	if res := h.postFlag(report(flagID, "e-next", screening.ModeFlag, screening.StateTripped), http.StatusCreated); res.Status != screening.FlagStatusRaised {
		t.Fatalf("new session = %+v, want raised", res)
	}
}

// D3 through the API and the sweep: a held clear answers 200, a re-trip
// inside the window is the same flag with no second page, a held clear is
// never re-paged, and the sweep resolves it once the window passes.
func TestRejoinWindowThroughTheSweep(t *testing.T) {
	h, clock := screeningHarness(t, withRejoin(10*time.Minute))
	raised := h.postFlag(report(flagID, flagEpoch, screening.ModeFlag, screening.StateTripped), http.StatusCreated)
	clock.advance(time.Minute)
	if res := h.postFlag(report(flagID, flagEpoch, screening.ModeFlag, screening.StateCleared), http.StatusOK); res.Status != screening.FlagStatusUpdated || res.FlagID != raised.FlagID {
		t.Fatalf("held clear = %+v", res)
	}
	clock.advance(time.Minute)
	if res := h.postFlag(report(flagID, flagEpoch, screening.ModeFlag, screening.StateTripped), http.StatusOK); res.Status != screening.FlagStatusUpdated || res.FlagID != raised.FlagID {
		t.Fatalf("re-trip = %+v, want the same flag updated", res)
	}
	if f := h.flag(raised.FlagID); f.Trips != 2 {
		t.Fatalf("trips = %d, want 2", f.Trips)
	}
	clock.advance(time.Minute)
	h.postFlag(report(flagID, flagEpoch, screening.ModeFlag, screening.StateCleared), http.StatusOK)

	// Past the repage interval but inside the window: a held clear is not
	// re-paged.
	clock.advance(9 * time.Minute)
	h.api.ScreeningTick(t.Context())
	if n := len(h.flagEvents(store.EventContentFlagReminded)); n != 0 {
		t.Fatalf("a held clear was re-paged %d times", n)
	}
	if f := h.flag(raised.FlagID); !f.Unresolved() {
		t.Fatalf("resolved inside the window: %+v", f)
	}
	clock.advance(2 * time.Minute)
	h.api.ScreeningTick(t.Context())
	if f := h.flag(raised.FlagID); f.Resolution != events.ContentFlagResolutionCleared {
		t.Fatalf("after the window: %+v, want cleared", f)
	}
	if n := len(h.flagEvents(store.EventContentFlag)); n != 1 {
		t.Fatalf("raises = %d, want one page for the whole incident", n)
	}
}
