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

// D7: the timeline route over a seeded store — sessions with their rule,
// buckets, incidents, events, levels and the computed pattern — operator
// only, and an unknown key is an empty timeline, not an error.
func TestScreeningTimelineRoute(t *testing.T) {
	h, clock := screeningHarness(t, withConfig(func(c *config.Config) { c.AppBaseURL = "https://gawk.example" }))
	ctx := t.Context()
	rule := &screening.BatchRule{Threshold: 0.5, Window: 6, WindowHits: 3}
	var samples []screening.Sample
	for i := 1; i <= 40; i++ { // 200 s at 0.8: one sustained run at critical
		samples = append(samples, screening.Sample{Position: i, At: flagT0.Add(time.Duration(i-1) * 5 * time.Second),
			Scores: map[string]float64{"FEMALE_GENITALIA_EXPOSED": 0.8, "MALE_GENITALIA_EXPOSED": 0, "ANUS_EXPOSED": 0.1}})
	}
	if _, err := h.store.IngestSamples(ctx, screening.SampleBatch{
		Schema: screening.SchemaSamples, SamplerID: "gawk-sampler-0", Mode: screening.ModeObserve, Floor: 0.3,
		Labels: screening.DefaultLabels(), Rule: rule,
		Sessions: []screening.SessionSamples{{BroadcastID: flagID, BroadcastKey: flagKey, PublisherEpoch: flagEpoch,
			Taken: 60, FirstAt: flagT0, LastAt: flagT0.Add(295 * time.Second), Samples: samples}},
	}); err != nil {
		t.Fatal(err)
	}
	raised := h.postFlag(report(flagID, flagEpoch, screening.ModeFlag, screening.StateTripped), http.StatusCreated)
	clock.advance(10 * time.Minute)

	h.identity.Roles = []string{"flagger"}
	h.decode(http.MethodGet, "/api/v1/screening/timeline?key="+flagKey, nil, http.StatusForbidden, nil)
	h.asOperator()
	h.decode(http.MethodGet, "/api/v1/screening/timeline?key=NOT-HEX", nil, http.StatusBadRequest, nil)
	h.decode(http.MethodGet, "/api/v1/screening/timeline?key="+flagKey+"&from=2026-10-13T13:00:00Z&to=2026-10-13T12:00:00Z", nil, http.StatusBadRequest, nil)

	var tl struct {
		BroadcastID   string `json:"broadcastId"`
		Live          bool   `json:"live"`
		From          string `json:"from"`
		BucketSeconds int    `json:"bucketSeconds"`
		Levels        struct {
			Floor     float64  `json:"floor"`
			Threshold *float64 `json:"threshold"`
			Critical  float64  `json:"critical"`
		} `json:"levels"`
		Sessions []struct {
			PublisherEpoch string   `json:"publisherEpoch"`
			Threshold      *float64 `json:"threshold"`
		} `json:"sessions"`
		Buckets []struct {
			Max   float64 `json:"max"`
			Above int     `json:"above"`
		} `json:"buckets"`
		Incidents []struct {
			ID int64 `json:"id"`
		} `json:"incidents"`
		Events []struct {
			Type string `json:"type"`
		} `json:"events"`
		Summary struct {
			Pattern string `json:"pattern"`
		} `json:"summary"`
		Suggestion struct {
			Verdict    string `json:"verdict"`
			BanSeconds int    `json:"banSeconds"`
		} `json:"suggestion"`
		Links struct {
			Watch string `json:"watch"`
		} `json:"links"`
	}
	h.decode(http.MethodGet, "/api/v1/screening/timeline?key="+flagKey, nil, http.StatusOK, &tl)
	if tl.BroadcastID != flagID || !tl.Live || tl.Links.Watch != "https://gawk.example/#/view/"+flagID {
		t.Fatalf("identity = %+v", tl)
	}
	if tl.Levels.Floor != 0.3 || tl.Levels.Threshold == nil || *tl.Levels.Threshold != 0.5 || tl.Levels.Critical != 0.7 {
		t.Fatalf("levels = %+v", tl.Levels)
	}
	if len(tl.Sessions) != 1 || tl.Sessions[0].Threshold == nil || *tl.Sessions[0].Threshold != 0.5 {
		t.Fatalf("sessions = %+v", tl.Sessions)
	}
	// The default range starts at the first sample (10 minutes minimum), so
	// buckets are 5 s and the 40 stored samples fill 40 of them.
	if tl.BucketSeconds != 5 || len(tl.Buckets) != 40 || tl.Buckets[0].Max != 0.8 || tl.Buckets[0].Above != 1 {
		t.Fatalf("bucket %ds, %d buckets, first %+v", tl.BucketSeconds, len(tl.Buckets), tl.Buckets)
	}
	if len(tl.Incidents) != 1 || tl.Incidents[0].ID != raised.FlagID {
		t.Fatalf("incidents = %+v", tl.Incidents)
	}
	if len(tl.Events) != 1 || tl.Events[0].Type != store.EventContentFlag {
		t.Fatalf("events = %+v", tl.Events)
	}
	if tl.Summary.Pattern != "sustained" || tl.Suggestion.Verdict != "violation" || tl.Suggestion.BanSeconds != 86_400 {
		t.Fatalf("summary %+v, suggestion %+v", tl.Summary, tl.Suggestion)
	}

	var empty struct {
		Sessions []any `json:"sessions"`
		Buckets  []any `json:"buckets"`
		Summary  struct {
			Pattern string `json:"pattern"`
		} `json:"summary"`
	}
	h.decode(http.MethodGet, "/api/v1/screening/timeline?key=ffffffffffff", nil, http.StatusOK, &empty)
	if empty.Sessions == nil || len(empty.Sessions) != 0 || len(empty.Buckets) != 0 || empty.Summary.Pattern != "none" {
		t.Fatalf("unknown key = %+v", empty)
	}
}
