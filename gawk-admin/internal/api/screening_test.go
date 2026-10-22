package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-admin/internal/api"
	"github.com/Tuhis/gawk/gawk-admin/internal/config"
	"github.com/Tuhis/gawk/gawk-admin/internal/kube"
	"github.com/Tuhis/gawk/gawk-admin/internal/relayscan"
	"github.com/Tuhis/gawk/gawk-admin/internal/store"
	"github.com/Tuhis/gawk/gawk-server/events"
	"github.com/Tuhis/gawk/gawk-server/rooms"
	"github.com/Tuhis/gawk/gawk-server/screening"
)

// R40 (docs/71): the content-flag lifecycle through the API — CF3's routes and
// CF4's leader sweep, driven by a fake clock.

const (
	flagID    = "ABC234"
	flagKey   = "0123456789ab"
	flagEpoch = "5f2c9a0b11d4e6f7"
)

var flagT0 = time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

// withPager configures one chart-defined webhook with no filter: the route a
// flag-mode flag needs (D12).
func withPager() harnessOption {
	return withConfig(func(c *config.Config) {
		c.StaticWebhooks = []config.StaticWebhook{{Name: "ops-pager", URL: "https://pager.example", SecretEnv: "PAGER", Secret: "aw=="}}
	})
}

func originBroadcast(id, key, epoch string) relayscan.Aggregate {
	return relayscan.Aggregate{
		ID: id, Key: key, PublisherActive: true, PublisherEpoch: epoch,
		OriginPod: "gawk-server-0", OriginAddr: "10.42.0.7:2112",
		Pods: []relayscan.Placement{{Pod: "gawk-server-0", Role: "origin", Addr: "10.42.0.7:2112"}},
	}
}

func fleetOf(bs ...relayscan.Aggregate) relayscan.Snapshot {
	return relayscan.Snapshot{At: flagT0, PodsResolved: 1, PodsAnswered: 1, Broadcasts: bs}
}

func report(id, epoch, mode, state string, recent ...float64) screening.ContentFlag {
	return screening.ContentFlag{
		Schema: screening.SchemaContentFlag, BroadcastID: id, PublisherEpoch: epoch, SamplerID: "gawk-sampler-0",
		Mode: mode, State: state,
		Window: screening.WindowSummary{Start: flagT0.Add(-25 * time.Second), End: flagT0, Samples: 6, FlaggedSamples: 4},
		Scores: screening.Scores{Max: 0.95, Mean: 0.82}, Labels: []string{"FEMALE_GENITALIA_EXPOSED"},
		LabelCounts: map[string]int{"FEMALE_GENITALIA_EXPOSED": 4}, Recent: recent,
	}
}

// screeningHarness is a Postgres harness with screening on, a pager, a fake
// clock and one live broadcast.
func screeningHarness(t *testing.T, opts ...harnessOption) (*harness, *testClock) {
	t.Helper()
	clock := &testClock{t: flagT0}
	h := newHarness(t, append([]harnessOption{withScreening(), withPager(), withClock(clock)}, opts...)...)
	h.fleet.set(fleetOf(originBroadcast(flagID, flagKey, flagEpoch)))
	return h, clock
}

func (h *harness) postFlag(body any, wantStatus int) screening.FlagResponse {
	h.t.Helper()
	h.identity.Roles = []string{"flagger"}
	var out screening.FlagResponse
	h.decode(http.MethodPost, "/api/v1/content-flags", body, wantStatus, &out)
	return out
}

func (h *harness) asOperator() { h.identity.Roles = []string{"operator"} }

func (h *harness) flagEvents(types ...string) []store.Event {
	h.t.Helper()
	evs, err := h.store.ListEvents(h.t.Context(), store.EventQuery{Types: types, Limit: store.MaxEventLimit})
	if err != nil {
		h.t.Fatal(err)
	}
	return evs
}

func (h *harness) allFlags() []store.ContentFlag {
	h.t.Helper()
	flags, err := h.store.ListContentFlags(h.t.Context(), store.FlagQuery{State: store.FlagFilterAll, Mode: store.FlagFilterAll})
	if err != nil {
		h.t.Fatal(err)
	}
	return flags
}

func (h *harness) flag(id int64) store.ContentFlag {
	h.t.Helper()
	f, err := h.store.GetContentFlag(h.t.Context(), id)
	if err != nil {
		h.t.Fatal(err)
	}
	return f
}

// CF3: the first tripped POST raises (201, one row, one content_flag.raised),
// a repeat updates (200, no new event), a cleared POST resolves, and the next
// tripped POST raises a NEW flag that pages again.
func TestContentFlagRaiseUpdateClearRaise(t *testing.T) {
	h, clock := screeningHarness(t)
	first := h.postFlag(report(flagID, flagEpoch, screening.ModeFlag, screening.StateTripped), http.StatusCreated)
	if first.Status != screening.FlagStatusRaised || first.FlagID == 0 {
		t.Fatalf("first POST = %+v", first)
	}
	if n := len(h.allFlags()); n != 1 {
		t.Fatalf("%d flags after the raise", n)
	}
	raised := h.flagEvents(store.EventContentFlag)
	if len(raised) != 1 || raised[0].BroadcastID != flagID || raised[0].BroadcastKey != flagKey {
		t.Fatalf("raised events = %+v", raised)
	}
	if f := raised[0].ContentFlag(); f.FlagID != first.FlagID || f.Window == nil || f.Window.FlaggedSamples != 4 || f.SamplerID != "gawk-sampler-0" {
		t.Fatalf("raised payload = %+v", f)
	}

	clock.advance(5 * time.Second)
	again := h.postFlag(report(flagID, flagEpoch, screening.ModeFlag, screening.StateTripped), http.StatusOK)
	if again.Status != screening.FlagStatusUpdated || again.FlagID != first.FlagID {
		t.Fatalf("repeat POST = %+v", again)
	}
	if n := len(h.flagEvents(store.EventContentFlag)); n != 1 {
		t.Fatalf("a repeat POST recorded another raise (%d)", n)
	}
	if got := h.flag(first.FlagID).LastPostAt; !got.Equal(flagT0.Add(5 * time.Second)) {
		t.Fatalf("lastPostAt = %v, not moved by the update", got)
	}

	clock.advance(5 * time.Second)
	cleared := h.postFlag(report(flagID, flagEpoch, screening.ModeFlag, screening.StateCleared), http.StatusOK)
	if cleared.Status != screening.FlagStatusResolved {
		t.Fatalf("cleared POST = %+v", cleared)
	}
	if f := h.flag(first.FlagID); f.State != store.FlagStateResolved || f.Resolution != events.ContentFlagResolutionCleared {
		t.Fatalf("after clear: %+v", f)
	}
	if res := h.flagEvents(store.EventContentFlagResolved); len(res) != 1 || res[0].ContentFlag().Resolution != events.ContentFlagResolutionCleared {
		t.Fatalf("resolved events = %+v", res)
	}
	// A second clear finds nothing to resolve.
	if again := h.postFlag(report(flagID, flagEpoch, screening.ModeFlag, screening.StateCleared), http.StatusAccepted); again.Status != screening.FlagStatusIgnored {
		t.Fatalf("second clear = %+v", again)
	}

	clock.advance(time.Minute)
	second := h.postFlag(report(flagID, flagEpoch, screening.ModeFlag, screening.StateTripped), http.StatusCreated)
	if second.FlagID == first.FlagID {
		t.Fatal("a second episode updated the resolved flag instead of raising a new one")
	}
	if n := len(h.flagEvents(store.EventContentFlag)); n != 2 {
		t.Fatalf("a second episode did not page again (%d raises)", n)
	}
}

// D19: mode is required — absent is 400, it decides between paging and
// silence — and the rest of the body is validated too.
func TestContentFlagBodyValidation(t *testing.T) {
	h, _ := screeningHarness(t)
	noMode := report(flagID, flagEpoch, "", screening.StateTripped)
	h.identity.Roles = []string{"flagger"}
	if code := h.errorCode(http.MethodPost, "/api/v1/content-flags", noMode, http.StatusBadRequest); code != api.CodeBadRequest {
		t.Fatalf("no mode: %s", code)
	}
	tooMany := report(flagID, flagEpoch, screening.ModeFlag, screening.StateTripped, make([]float64, screening.MaxRecent+1)...)
	h.errorCode(http.MethodPost, "/api/v1/content-flags", tooMany, http.StatusBadRequest)
	unknown := map[string]any{"schema": screening.SchemaContentFlag, "broadcastId": flagID, "mode": "flag", "frame": "AAAA"}
	h.errorCode(http.MethodPost, "/api/v1/content-flags", unknown, http.StatusBadRequest)
	if n := len(h.allFlags()); n != 0 {
		t.Fatalf("a refused body left %d flags", n)
	}
}

// D9: a tripped POST 5 s after the operator acknowledged updates the
// acknowledged flag — no duplicate, no new page — and a clear still resolves
// it, so a later episode pages again.
func TestTrippedAfterAcknowledgeUpdatesTheAcknowledgedFlag(t *testing.T) {
	h, clock := screeningHarness(t)
	raised := h.postFlag(report(flagID, flagEpoch, screening.ModeFlag, screening.StateTripped), http.StatusCreated)
	h.asOperator()
	var acked struct {
		State          string `json:"state"`
		AcknowledgedBy string `json:"acknowledgedBy"`
		Links          struct {
			Watch string `json:"watch"`
		} `json:"links"`
	}
	h.decode(http.MethodPost, "/api/v1/content-flags/"+itoa(raised.FlagID)+"/acknowledge", map[string]any{}, http.StatusOK, &acked)
	if acked.State != store.FlagStateAcknowledged || acked.AcknowledgedBy != "op@example.com" || acked.Links.Watch != "https://gawk.example/#/view/ABC234" {
		t.Fatalf("acknowledged = %+v", acked)
	}
	clock.advance(5 * time.Second)
	if res := h.postFlag(report(flagID, flagEpoch, screening.ModeFlag, screening.StateTripped), http.StatusOK); res.FlagID != raised.FlagID || res.Status != screening.FlagStatusUpdated {
		t.Fatalf("tripped after acknowledge = %+v", res)
	}
	if n := len(h.allFlags()); n != 1 {
		t.Fatalf("%d flags, want the one acknowledged flag", n)
	}
	if f := h.flag(raised.FlagID); f.State != store.FlagStateAcknowledged {
		t.Fatalf("the update reopened the flag: %s", f.State)
	}
	h.postFlag(report(flagID, flagEpoch, screening.ModeFlag, screening.StateCleared), http.StatusOK)
	if f := h.flag(raised.FlagID); f.Resolution != events.ContentFlagResolutionCleared {
		t.Fatalf("an acknowledged flag did not resolve on clear: %+v", f)
	}
}

// D10, OD11: dismiss resolves and mutes; POSTs during the mute answer 202
// muted and change nothing; after it expires a persisting signal raises a new
// flag.
func TestDismissMutesReflagging(t *testing.T) {
	h, clock := screeningHarness(t)
	raised := h.postFlag(report(flagID, flagEpoch, screening.ModeFlag, screening.StateTripped), http.StatusCreated)
	h.asOperator()
	var dismissed struct {
		State      string `json:"state"`
		Resolution string `json:"resolution"`
		MutedUntil string `json:"mutedUntil"`
	}
	h.decode(http.MethodPost, "/api/v1/content-flags/"+itoa(raised.FlagID)+"/dismiss", nil, http.StatusOK, &dismissed)
	if dismissed.Resolution != events.ContentFlagResolutionDismissed || dismissed.MutedUntil != flagT0.Add(30*time.Minute).Format(time.RFC3339) {
		t.Fatalf("dismissed = %+v", dismissed)
	}
	clock.advance(time.Minute)
	if res := h.postFlag(report(flagID, flagEpoch, screening.ModeFlag, screening.StateTripped), http.StatusAccepted); res.Status != screening.FlagStatusMuted {
		t.Fatalf("POST during the mute = %+v", res)
	}
	if n := len(h.flagEvents(store.EventContentFlag)); n != 1 {
		t.Fatalf("a muted POST paged (%d raises)", n)
	}
	// The target list says so too.
	var targets screening.TargetsResponse
	h.decode(http.MethodGet, "/api/v1/screening/targets?shard=0&of=1", nil, http.StatusOK, &targets)
	if len(targets.Targets) != 1 || !targets.Targets[0].Muted {
		t.Fatalf("targets during the mute = %+v", targets.Targets)
	}
	clock.advance(30 * time.Minute)
	if res := h.postFlag(report(flagID, flagEpoch, screening.ModeFlag, screening.StateTripped), http.StatusCreated); res.FlagID == raised.FlagID {
		t.Fatalf("after the mute = %+v, want a new flag", res)
	}
}

// D12: with no enabled webhook that would deliver content_flag.raised — none
// at all, or only filters that exclude it — a flag-mode POST is 503
// no_flag_route and the target list says flagRoute: false; observe mode is
// unaffected.
func TestNoFlagRouteRefusesFlagMode(t *testing.T) {
	clock := &testClock{t: flagT0}
	h := newHarness(t, withScreening(), withClock(clock), withConfig(func(c *config.Config) {
		c.StaticWebhooks = []config.StaticWebhook{{Name: "rooms-bot", URL: "https://bot.example", SecretEnv: "B", Secret: "aw==",
			Events: []string{store.EventRoomAttached}}}
	}))
	h.fleet.set(fleetOf(originBroadcast(flagID, flagKey, flagEpoch)))
	h.identity.Roles = []string{"flagger"}
	if code := h.errorCode(http.MethodPost, "/api/v1/content-flags", report(flagID, flagEpoch, screening.ModeFlag, screening.StateTripped),
		http.StatusServiceUnavailable); code != api.CodeNoFlagRoute {
		t.Fatalf("flag mode with no route: %s", code)
	}
	var targets screening.TargetsResponse
	h.decode(http.MethodGet, "/api/v1/screening/targets?shard=0&of=1", nil, http.StatusOK, &targets)
	if targets.FlagRoute {
		t.Fatal("targets say flagRoute: true with no route")
	}
	if res := h.postFlag(report(flagID, flagEpoch, screening.ModeObserve, screening.StateTripped), http.StatusCreated); res.Status != screening.FlagStatusRaised {
		t.Fatalf("observe mode with no route = %+v", res)
	}
	h.asOperator()
	var page struct {
		Screening struct {
			FlagRoute bool `json:"flagRoute"`
		} `json:"screening"`
	}
	h.decode(http.MethodGet, "/api/v1/content-flags", nil, http.StatusOK, &page)
	if page.Screening.FlagRoute {
		t.Fatal("the portal's banner would not show")
	}
}

// D19, OD14: an observed flag emits exactly one content_flag.observed and no
// raise, is never pinned or reminded, ignores mutes, cannot be acknowledged,
// and resolves cleared on the cleared POST or as stale when the sampler goes
// quiet.
func TestObservedFlags(t *testing.T) {
	h, clock := screeningHarness(t)
	obs := h.postFlag(report(flagID, flagEpoch, screening.ModeObserve, screening.StateTripped), http.StatusCreated)
	h.postFlag(report(flagID, flagEpoch, screening.ModeObserve, screening.StateTripped), http.StatusOK)
	if n := len(h.flagEvents(store.EventContentFlagObserved)); n != 1 {
		t.Fatalf("%d observed events, want exactly one", n)
	}
	if n := len(h.flagEvents(store.EventContentFlag)); n != 0 {
		t.Fatalf("observe mode raised %d pages", n)
	}
	h.asOperator()
	var bpage struct {
		Broadcasts []struct {
			ContentFlag *struct{ ID int64 } `json:"contentFlag"`
		} `json:"broadcasts"`
	}
	h.decode(http.MethodGet, "/api/v1/broadcasts", nil, http.StatusOK, &bpage)
	if len(bpage.Broadcasts) != 1 || bpage.Broadcasts[0].ContentFlag != nil {
		t.Fatalf("an observed flag was pinned: %+v", bpage.Broadcasts)
	}
	var page struct {
		ContentFlags []struct {
			ID    int64  `json:"id"`
			Mode  string `json:"mode"`
			Links struct {
				Watch string `json:"watch"`
			} `json:"links"`
		} `json:"contentFlags"`
	}
	h.decode(http.MethodGet, "/api/v1/content-flags", nil, http.StatusOK, &page)
	if len(page.ContentFlags) != 0 {
		t.Fatalf("the default (flag-mode) list shows observed flags: %+v", page.ContentFlags)
	}
	h.decode(http.MethodGet, "/api/v1/content-flags?mode=observe", nil, http.StatusOK, &page)
	if len(page.ContentFlags) != 1 || page.ContentFlags[0].Links.Watch == "" {
		t.Fatalf("observed list = %+v, want the flag with its watch link", page.ContentFlags)
	}
	if code := h.errorCode(http.MethodPost, "/api/v1/content-flags/"+itoa(obs.FlagID)+"/acknowledge", nil, http.StatusConflict); code != api.CodeContentFlagState {
		t.Fatalf("acknowledge an observed flag: %s", code)
	}
	if code := h.errorCode(http.MethodPost, "/api/v1/content-flags/"+itoa(obs.FlagID)+"/dismiss", nil, http.StatusConflict); code != api.CodeContentFlagState {
		t.Fatalf("dismiss an observed flag: %s", code)
	}
	// Never reminded, however long it stays tripped.
	for range 3 {
		clock.advance(11 * time.Minute)
		h.postFlag(report(flagID, flagEpoch, screening.ModeObserve, screening.StateTripped), http.StatusOK)
		h.api.ScreeningTick(t.Context())
	}
	if n := len(h.flagEvents(store.EventContentFlagReminded)); n != 0 {
		t.Fatalf("an observed flag was reminded %d times", n)
	}
	h.postFlag(report(flagID, flagEpoch, screening.ModeObserve, screening.StateCleared), http.StatusOK)
	if f := h.flag(obs.FlagID); f.Resolution != events.ContentFlagResolutionCleared || f.ResolutionReason != "" {
		t.Fatalf("after clear: %+v", f)
	}
	if n := len(h.flagEvents(store.EventContentFlagResolved)); n != 0 {
		t.Fatalf("an observed flag's resolution paged (%d)", n)
	}

	// Stale: a new observed flag whose sampler goes quiet.
	stale := h.postFlag(report(flagID, flagEpoch, screening.ModeObserve, screening.StateTripped), http.StatusCreated)
	clock.advance(2*time.Minute + time.Second)
	h.api.ScreeningTick(t.Context())
	if f := h.flag(stale.FlagID); f.Resolution != events.ContentFlagResolutionCleared || f.ResolutionReason != store.FlagReasonStale {
		t.Fatalf("a quiet observed flag = %+v, want cleared/stale", f)
	}

	// Observe ignores a mute: dismiss a flag-mode flag, then observe again.
	fl := h.postFlag(report(flagID, flagEpoch, screening.ModeFlag, screening.StateTripped), http.StatusCreated)
	h.asOperator()
	h.decode(http.MethodPost, "/api/v1/content-flags/"+itoa(fl.FlagID)+"/dismiss", nil, http.StatusOK, nil)
	if res := h.postFlag(report(flagID, flagEpoch, screening.ModeObserve, screening.StateTripped), http.StatusCreated); res.Status != screening.FlagStatusRaised {
		t.Fatalf("observe during a mute = %+v", res)
	}
}

// docs/42 D17 as R40 uses it: a flagger token reaches exactly the three
// flagger routes and is 403 everywhere else — the R49 loop, so a route added
// later is covered the day it lands.
func TestFlaggerReachesExactlyTheFlaggerRoutes(t *testing.T) {
	h := newHarnessWithoutPostgres(t, withRoomsEnabled(), withScreening(), withPopsEnabled())
	h.identity.Roles = []string{"flagger"}
	allowed := map[string]bool{
		"POST /api/v1/content-flags":     true,
		"GET /api/v1/screening/targets":  true,
		"POST /api/v1/screening/samples": true,
	}
	for _, r := range api.RouteTable() {
		if len(r.Roles) == 0 {
			continue
		}
		key := r.Method + " " + r.Pattern
		status, body := h.raw(r.Method, concretePath(r.Pattern), nil)
		if allowed[key] {
			// No body or no shard is a 400 from the handler — past the role.
			if status == http.StatusForbidden || status >= 500 {
				t.Errorf("%s as flagger = %d, want it admitted; body: %s", key, status, body)
			}
			continue
		}
		if status != http.StatusForbidden {
			t.Errorf("%s as flagger = %d, want 403; body: %s", key, status, body)
		}
	}
	// And the operator does not hold the flagger routes by implication.
	h.identity.Roles = []string{"operator"}
	for key := range allowed {
		method, path, _ := strings.Cut(key, " ")
		if status, _ := h.raw(method, concretePath(path), nil); status != http.StatusForbidden {
			t.Errorf("%s as operator = %d, want 403", key, status)
		}
	}
}

// D2, D11, D22: origin rows with an active publisher only; exempt broadcasts
// listed and marked; muted marked; the shards of a split are disjoint and
// cover the list; a bad pair is 400.
func TestScreeningTargets(t *testing.T) {
	secret := &rooms.SecretKeyRef{Name: "room-crew", Key: kube.RoomSecretKey}
	exempt := kube.RoomObject{Name: "crew"}
	exempt.Room.Name = "crew"
	exempt.Room.Spec = rooms.RoomSpec{Kind: rooms.KindStatic, AttachSecretRef: secret, Screening: rooms.ScreeningExempt}
	exempt.Room.Status.Attachments = []rooms.Attachment{{BroadcastID: "EXM234"}}
	// A dynamic room marked exempt by hand is screened anyway (fails closed).
	sneaky := dynamicRoomObject("r7k3mx", "9c1d2e3f4a5b", "gawk-server-0", 0)
	sneaky.Room.Spec.Screening = rooms.ScreeningExempt
	sneaky.Room.Status.Attachments = []rooms.Attachment{{BroadcastID: "SNK234"}}
	h, _ := screeningHarness(t, withRooms(newFakeRooms(exempt, sneaky), &memoryRecorder{}))

	edge := originBroadcast("EDG234", "aaaaaaaaaaaa", "e-edge")
	edge.OriginAddr, edge.OriginPod = "", ""
	away := originBroadcast("AWY234", "bbbbbbbbbbbb", "e-away")
	away.PublisherActive = false
	old := originBroadcast("OLD234", "cccccccccccc", "")
	broadcasts := []relayscan.Aggregate{
		originBroadcast(flagID, flagKey, flagEpoch),
		originBroadcast("EXM234", "dddddddddddd", "e-exempt"),
		originBroadcast("SNK234", "eeeeeeeeeeee", "e-sneaky"),
		edge, away, old,
	}
	for i := 0; i < 20; i++ {
		id := "B" + string("23456789ABCDEFGHJKMN"[i]) + "2345"
		broadcasts = append(broadcasts, originBroadcast(id, strings.Repeat(string("0123456789abcdef"[i%16]), 12), "e"))
	}
	h.fleet.set(fleetOf(broadcasts...))

	h.identity.Roles = []string{"flagger"}
	var all screening.TargetsResponse
	h.decode(http.MethodGet, "/api/v1/screening/targets?shard=0&of=1", nil, http.StatusOK, &all)
	if all.Schema != screening.SchemaTargets || !all.FlagRoute {
		t.Fatalf("envelope = %+v", all)
	}
	got := map[string]screening.Target{}
	for _, tg := range all.Targets {
		got[tg.BroadcastID] = tg
	}
	for _, skipped := range []string{"EDG234", "AWY234", "OLD234"} {
		if _, ok := got[skipped]; ok {
			t.Errorf("%s is listed: only origins with an active publisher and an epoch are screened", skipped)
		}
	}
	if len(got) != 23 {
		t.Fatalf("%d targets, want 23", len(got))
	}
	if tg := got[flagID]; tg.Relay != "http://10.42.0.7:2112" || tg.PublisherEpoch != flagEpoch || tg.BroadcastKey != flagKey || tg.Exempt || tg.Muted {
		t.Errorf("target = %+v", tg)
	}
	if !got["EXM234"].Exempt {
		t.Error("a broadcast in an exempt room is not marked exempt")
	}
	if got["SNK234"].Exempt {
		t.Error("a dynamic room's exemption was honoured")
	}

	// of=2: disjoint, and together the full list.
	seen := map[string]int{}
	for shard := 0; shard < 2; shard++ {
		var part screening.TargetsResponse
		h.decode(http.MethodGet, "/api/v1/screening/targets?shard="+itoa(int64(shard))+"&of=2", nil, http.StatusOK, &part)
		if part.Shard != shard || part.Of != 2 {
			t.Fatalf("shard echo = %d/%d", part.Shard, part.Of)
		}
		for _, tg := range part.Targets {
			seen[tg.BroadcastID]++
			if screening.Shard(tg.BroadcastKey, 2) != shard {
				t.Errorf("%s listed on shard %d", tg.BroadcastID, shard)
			}
		}
	}
	if len(seen) != len(got) {
		t.Fatalf("the two shards cover %d broadcasts, want %d", len(seen), len(got))
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("%s is on %d shards", id, n)
		}
	}
	for _, q := range []string{"", "?shard=0", "?of=2", "?shard=2&of=2", "?shard=-1&of=2", "?shard=0&of=0", "?shard=x&of=2"} {
		h.errorCode(http.MethodGet, "/api/v1/screening/targets"+q, nil, http.StatusBadRequest)
	}
}

// D22: a shard that stops polling is reported missing — on the portal's flag
// list, which the banner reads — and clears when it returns.
func TestMissingShardBanner(t *testing.T) {
	h, clock := screeningHarness(t)
	poll := func(shard int) {
		h.identity.Roles = []string{"flagger"}
		h.decode(http.MethodGet, "/api/v1/screening/targets?shard="+itoa(int64(shard))+"&of=2", nil, http.StatusOK, nil)
	}
	status := func() (int, []int) {
		h.asOperator()
		var page struct {
			Screening struct {
				ExpectedShards int   `json:"expectedShards"`
				MissingShards  []int `json:"missingShards"`
			} `json:"screening"`
		}
		h.decode(http.MethodGet, "/api/v1/content-flags", nil, http.StatusOK, &page)
		return page.Screening.ExpectedShards, page.Screening.MissingShards
	}
	poll(0)
	poll(1)
	if n, missing := status(); n != 2 || len(missing) != 0 {
		t.Fatalf("both polling: %d, %v", n, missing)
	}
	clock.advance(31 * time.Second)
	poll(0)
	if _, missing := status(); !slices.Equal(missing, []int{1}) {
		t.Fatalf("shard 1 silent 31 s: missing %v", missing)
	}
	if n, err := h.api.ShardsMissing(t.Context()); err != nil || n != 1 {
		t.Fatalf("the gauge's source = %d, %v", n, err)
	}
	poll(1)
	if _, missing := status(); len(missing) != 0 {
		t.Fatalf("shard 1 back: missing %v", missing)
	}
}

// D20, D21 through the routes: a batch is stored, replay answers over it, a
// threshold under the floor is 400, and replay is the operator's — the
// flagger is refused.
func TestSamplesAndReplayRoutes(t *testing.T) {
	h, _ := screeningHarness(t)
	labels := screening.DefaultLabels()
	sc := func(v float64) map[string]float64 {
		return map[string]float64{labels[0]: v, labels[1]: 0, labels[2]: 0}
	}
	batch := screening.SampleBatch{Schema: screening.SchemaSamples, SamplerID: "gawk-sampler-0", Mode: screening.ModeObserve,
		Floor: 0.3, Labels: labels, Sessions: []screening.SessionSamples{{
			BroadcastID: strings.ToLower(flagID), BroadcastKey: flagKey, PublisherEpoch: flagEpoch, Taken: 6,
			FirstAt: flagT0, LastAt: flagT0.Add(25 * time.Second),
			Samples: []screening.Sample{
				{Position: 1, At: flagT0, Scores: sc(0.9)}, {Position: 2, At: flagT0.Add(5 * time.Second), Scores: sc(0.9)},
				{Position: 3, At: flagT0.Add(10 * time.Second), Scores: sc(0.9)}, {Position: 4, At: flagT0.Add(15 * time.Second), Scores: sc(0.9)},
			},
		}}}
	h.identity.Roles = []string{"flagger"}
	var stored screening.SamplesResponse
	h.decode(http.MethodPost, "/api/v1/screening/samples", batch, http.StatusOK, &stored)
	if stored.Stored != 4 {
		t.Fatalf("stored %d", stored.Stored)
	}
	bad := batch
	bad.Sessions = []screening.SessionSamples{batch.Sessions[0]}
	bad.Sessions[0].Samples = []screening.Sample{{Position: 1, At: flagT0, Scores: sc(0.1)}}
	h.errorCode(http.MethodPost, "/api/v1/screening/samples", bad, http.StatusBadRequest)

	req := screening.ReplayRequest{Threshold: 0.8, Window: 6, WindowHits: 4, From: flagT0.Add(-time.Hour), To: flagT0.Add(time.Hour)}
	if status, _ := h.raw(http.MethodPost, "/api/v1/screening/replay", req); status != http.StatusForbidden {
		t.Fatalf("replay as flagger = %d, want 403", status)
	}
	h.asOperator()
	var out screening.ReplayResponse
	h.decode(http.MethodPost, "/api/v1/screening/replay", req, http.StatusOK, &out)
	if out.WouldTrips != 1 || len(out.Trips) != 1 || out.Trips[0].BroadcastID != flagID || !out.Trips[0].At.Equal(flagT0.Add(15*time.Second)) {
		t.Fatalf("replay = %+v", out)
	}
	req.Threshold = 0.2
	h.errorCode(http.MethodPost, "/api/v1/screening/replay", req, http.StatusBadRequest)
	req.Threshold, req.WindowHits = 0.8, 7
	h.errorCode(http.MethodPost, "/api/v1/screening/replay", req, http.StatusBadRequest)
}

// D11: only a static room with an attach secret can be exempt; the toggle
// writes exactly one room.screening_changed, and the room view reports it.
func TestRoomScreeningPatch(t *testing.T) {
	gated := kube.RoomObject{Name: "crew", Managed: true}
	gated.Room.Name = "crew"
	gated.Room.Spec = rooms.RoomSpec{Kind: rooms.KindStatic, AttachSecretRef: &rooms.SecretKeyRef{Name: "room-crew", Key: kube.RoomSecretKey}}
	gated.Room.Status.Key = "9c1d2e3f4a5b"
	open := kube.RoomObject{Name: "lobby", Managed: true}
	open.Room.Name = "lobby"
	open.Room.Spec = rooms.RoomSpec{Kind: rooms.KindStatic}
	dyn := dynamicRoomObject("r7k3mx", "1c1d2e3f4a5b", "gawk-server-0", 0)
	rec := &memoryRecorder{}
	h := newHarnessWithoutPostgres(t, withRooms(newFakeRooms(gated, open, dyn), rec), withScreening())

	var room wireRoom
	var screeningField struct {
		Screening string `json:"screening"`
	}
	h.decode(http.MethodPatch, "/api/v1/rooms/Crew", map[string]any{"screening": "exempt"}, http.StatusOK, &room)
	_, raw := h.raw(http.MethodGet, "/api/v1/rooms/crew", nil)
	_ = json.Unmarshal([]byte(raw), &screeningField)
	if room.Name != "crew" || screeningField.Screening != "exempt" {
		t.Fatalf("after PATCH: room %+v, screening %q", room, screeningField.Screening)
	}
	// The same value again changes nothing and records nothing.
	h.decode(http.MethodPatch, "/api/v1/rooms/crew", map[string]any{"screening": "exempt"}, http.StatusOK, nil)
	evs := rec.all()
	if len(evs) != 1 || evs[0].Type != store.EventRoomScreeningChanged || evs[0].RoomScreening() != events.RoomScreeningExempt ||
		evs[0].PayloadString(store.PayloadRoom) != "crew" || evs[0].RoomKey() != "9c1d2e3f4a5b" {
		t.Fatalf("events = %+v", evs)
	}
	h.decode(http.MethodPatch, "/api/v1/rooms/crew", map[string]any{"screening": ""}, http.StatusOK, nil)
	if evs = rec.all(); len(evs) != 2 || evs[1].RoomScreening() != events.RoomScreeningScreened {
		t.Fatalf("un-exempting did not record screened: %+v", evs)
	}

	for _, name := range []string{"lobby", "r7k3mx"} {
		if code := h.errorCode(http.MethodPatch, "/api/v1/rooms/"+name, map[string]any{"screening": "exempt"}, http.StatusBadRequest); code != api.CodeScreeningNotAllowed {
			t.Errorf("exempting %s: %s", name, code)
		}
	}
	h.errorCode(http.MethodPatch, "/api/v1/rooms/crew", map[string]any{}, http.StatusBadRequest)
	h.errorCode(http.MethodPatch, "/api/v1/rooms/crew", map[string]any{"screening": "off"}, http.StatusBadRequest)
	h.errorCode(http.MethodPatch, "/api/v1/rooms/nosuchroom", map[string]any{"screening": ""}, http.StatusNotFound)
	if len(rec.all()) != 2 {
		t.Fatal("a refused PATCH recorded an event")
	}

	// Screening on, rooms off: there is no room to patch.
	noRooms := newHarnessWithoutPostgres(t, withScreening())
	noRooms.errorCode(http.MethodPatch, "/api/v1/rooms/crew", map[string]any{"screening": "exempt"}, http.StatusNotFound)
}

// D9, D2: the publisher session ending resolves the flag `ended` — on a
// complete scan only — and a tripped POST for a broadcast whose publisher is
// no longer active is 202 ended and raises nothing.
func TestSessionEndResolvesEnded(t *testing.T) {
	h, clock := screeningHarness(t)
	raised := h.postFlag(report(flagID, flagEpoch, screening.ModeFlag, screening.StateTripped), http.StatusCreated)

	// A pod that did not answer: unknown, not ended.
	partial := fleetOf()
	partial.PodsResolved = 2
	h.fleet.set(partial)
	h.api.ScreeningTick(t.Context())
	if f := h.flag(raised.FlagID); !f.Unresolved() {
		t.Fatalf("a partial scan ended the flag: %+v", f)
	}

	away := originBroadcast(flagID, flagKey, flagEpoch)
	away.PublisherActive = false
	h.fleet.set(fleetOf(away))
	clock.advance(time.Second)
	h.api.ScreeningTick(t.Context())
	f := h.flag(raised.FlagID)
	if f.Resolution != events.ContentFlagResolutionEnded || f.ResolvedBy != "system" {
		t.Fatalf("after the session ended: %+v", f)
	}
	if res := h.flagEvents(store.EventContentFlagResolved); len(res) != 1 || res[0].ContentFlag().Resolution != events.ContentFlagResolutionEnded {
		t.Fatalf("resolved events = %+v", res)
	}
	before := len(h.flagEvents())
	if res := h.postFlag(report(flagID, flagEpoch, screening.ModeFlag, screening.StateTripped), http.StatusAccepted); res.Status != screening.FlagStatusEnded {
		t.Fatalf("a tripped POST after the end = %+v", res)
	}
	if n := len(h.allFlags()); n != 1 {
		t.Fatalf("%d flags, want no new one", n)
	}
	if after := len(h.flagEvents()); after != before {
		t.Fatalf("an ended POST recorded %d events", after-before)
	}

	// A new publisher session under an unresolved flag ends the old one at
	// the POST that names the new session.
	h.fleet.set(fleetOf(originBroadcast(flagID, flagKey, "e-new")))
	first := h.postFlag(report(flagID, "e-new", screening.ModeFlag, screening.StateTripped), http.StatusCreated)
	h.fleet.set(fleetOf(originBroadcast(flagID, flagKey, "e-newer")))
	second := h.postFlag(report(flagID, "e-newer", screening.ModeFlag, screening.StateTripped), http.StatusCreated)
	if f := h.flag(first.FlagID); f.Resolution != events.ContentFlagResolutionEnded || second.FlagID == first.FlagID {
		t.Fatalf("the old session's flag = %+v", f)
	}
}

// D2's backstop fails open (PR #500 review): a tripped POST is answered
// `ended` only on a complete scan that either lacks the broadcast or has its
// origin's row. An edge's row carries the edge's own publisher epoch, which
// never matches the flag's, so a hot broadcast whose origin missed the scan
// would otherwise come back `ended`, unflagged and unpaged.
func TestATrippedPostIsEndedOnlyByTheOrigin(t *testing.T) {
	edgeOnly := relayscan.Aggregate{
		ID: flagID, Key: flagKey, PublisherActive: true, PublisherEpoch: "edge-own-epoch",
		Pods: []relayscan.Placement{{Pod: "gawk-server-1", Role: "edge", Addr: "10.42.0.8:2112"}},
	}
	staleOrigin := originBroadcast(flagID, flagKey, "an-older-epoch")
	cases := []struct {
		name  string
		snap  relayscan.Snapshot
		ended bool
	}{
		{"the origin missed the scan; an edge's row stands in", relayscan.Snapshot{PodsResolved: 2, PodsAnswered: 1, Broadcasts: []relayscan.Aggregate{edgeOnly}}, false},
		{"a complete scan with only an edge's row", relayscan.Snapshot{PodsResolved: 2, PodsAnswered: 2, Broadcasts: []relayscan.Aggregate{edgeOnly}}, false},
		{"no pod resolved at all", relayscan.Snapshot{}, false},
		{"a partial scan whose origin row is another session", relayscan.Snapshot{PodsResolved: 2, PodsAnswered: 1, Broadcasts: []relayscan.Aggregate{staleOrigin}}, false},
		{"a complete scan whose origin row is another session", relayscan.Snapshot{PodsResolved: 2, PodsAnswered: 2, Broadcasts: []relayscan.Aggregate{staleOrigin}}, true},
		{"a complete scan without the broadcast", relayscan.Snapshot{PodsResolved: 2, PodsAnswered: 2}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := screeningHarness(t)
			h.fleet.set(tc.snap)
			body := report(flagID, flagEpoch, screening.ModeFlag, screening.StateTripped)
			if tc.ended {
				if res := h.postFlag(body, http.StatusAccepted); res.Status != screening.FlagStatusEnded {
					t.Fatalf("POST = %+v, want ended", res)
				}
				return
			}
			if res := h.postFlag(body, http.StatusCreated); res.Status != screening.FlagStatusRaised {
				t.Fatalf("POST = %+v, want raised", res)
			}
		})
	}
}

// D9: an open flag re-pages every repage interval while its signal is live
// AND while it is unknown — a silent sampler never silences a page — and stops
// once acknowledged. A sampler posting every 15 s (D6 stretched 3×) keeps
// the signal live.
func TestRemindersWhileLiveAndUnknown(t *testing.T) {
	h, clock := screeningHarness(t)
	raised := h.postFlag(report(flagID, flagEpoch, screening.ModeFlag, screening.StateTripped), http.StatusCreated)
	for clock.Now().Before(flagT0.Add(10*time.Minute + 30*time.Second)) {
		clock.advance(15 * time.Second)
		h.postFlag(report(flagID, flagEpoch, screening.ModeFlag, screening.StateTripped), http.StatusOK)
		h.api.ScreeningTick(t.Context())
	}
	reminded := h.flagEvents(store.EventContentFlagReminded)
	if len(reminded) != 1 || reminded[0].ContentFlag().Signal != events.ContentFlagSignalLive {
		t.Fatalf("after 10 m of 15 s posts: %+v, want one live reminder", reminded)
	}
	// The sampler goes quiet: the signal turns unknown and the page repeats.
	clock.advance(10 * time.Minute)
	h.api.ScreeningTick(t.Context())
	reminded = h.flagEvents(store.EventContentFlagReminded)
	if len(reminded) != 2 || reminded[0].ContentFlag().Signal != events.ContentFlagSignalUnknown || reminded[0].ContentFlag().Reminder != 2 {
		t.Fatalf("a quiet sampler: %+v, want a second reminder with an unknown signal", reminded)
	}
	// A tick before the interval is up sends nothing more.
	clock.advance(time.Minute)
	h.api.ScreeningTick(t.Context())
	if n := len(h.flagEvents(store.EventContentFlagReminded)); n != 2 {
		t.Fatalf("%d reminders after an early tick", n)
	}
	h.asOperator()
	h.decode(http.MethodPost, "/api/v1/content-flags/"+itoa(raised.FlagID)+"/acknowledge", nil, http.StatusOK, nil)
	clock.advance(30 * time.Minute)
	h.api.ScreeningTick(t.Context())
	if n := len(h.flagEvents(store.EventContentFlagReminded)); n != 2 {
		t.Fatalf("an acknowledged flag kept re-paging (%d)", n)
	}
	if f := h.flag(raised.FlagID); f.Reminders != 2 || !f.Unresolved() {
		t.Fatalf("flag = %+v", f)
	}
}

// D15, OD2: the deadline kill is off by default; at 30m it kills an open flag
// whose signal is live, and never an acknowledged, muted or unknown one.
func TestDeadlineKill(t *testing.T) {
	cases := []struct {
		name     string
		deadline time.Duration
		setup    func(h *harness, flagID int64)
		live     bool
		wantKill bool
	}{
		{"off by default", 0, nil, true, false},
		{"on, live and open", 30 * time.Minute, nil, true, true},
		{"on, acknowledged", 30 * time.Minute, func(h *harness, id int64) {
			h.asOperator()
			h.decode(http.MethodPost, "/api/v1/content-flags/"+itoa(id)+"/acknowledge", nil, http.StatusOK, nil)
		}, true, false},
		{"on, muted", 30 * time.Minute, func(h *harness, _ int64) {
			if _, err := h.store.Pool().Exec(h.t.Context(), `INSERT INTO screening_mutes (broadcast_id, muted_until, muted_by) VALUES ($1,$2,'op')`,
				flagID, flagT0.Add(2*time.Hour)); err != nil {
				h.t.Fatal(err)
			}
		}, true, false},
		{"on, signal unknown", 30 * time.Minute, nil, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, clock := screeningHarness(t, withConfig(func(c *config.Config) { c.ContentFlagDeadline = tc.deadline }))
			raised := h.postFlag(report(flagID, flagEpoch, screening.ModeFlag, screening.StateTripped), http.StatusCreated)
			if tc.live {
				clock.advance(30*time.Minute - 30*time.Second)
				h.postFlag(report(flagID, flagEpoch, screening.ModeFlag, screening.StateTripped), http.StatusOK)
				clock.advance(31 * time.Second)
			} else {
				clock.advance(31 * time.Minute)
			}
			// The state under test is set up just before the sweep, so the
			// signal is still live when it runs.
			if tc.setup != nil {
				tc.setup(h, raised.FlagID)
			}
			h.api.ScreeningTick(t.Context())
			bans, err := h.store.ListBans(t.Context(), store.FilterActive)
			if err != nil {
				t.Fatal(err)
			}
			f := h.flag(raised.FlagID)
			if !tc.wantKill {
				if len(bans) != 0 || f.Resolution == events.ContentFlagResolutionAutoKilled {
					t.Fatalf("killed: bans %+v, flag %+v", bans, f)
				}
				return
			}
			if len(bans) != 1 || bans[0].CreatedBy != store.ActorContentFlag || bans[0].Target.Value != flagID {
				t.Fatalf("bans = %+v, want one by %s", bans, store.ActorContentFlag)
			}
			if f.Resolution != events.ContentFlagResolutionAutoKilled || f.ResolutionReason != store.FlagReasonDeadline {
				t.Fatalf("flag = %+v", f)
			}
			if killed := h.enq.all(); len(killed) == 0 || killed[len(killed)-1].Type != store.EventBroadcastKilled || killed[len(killed)-1].Actor != store.ActorContentFlag {
				t.Fatalf("no broadcast.killed by %s: %+v", store.ActorContentFlag, killed)
			}
			if res := h.flagEvents(store.EventContentFlagResolved); len(res) != 1 || res[0].ContentFlag().Reason != store.FlagReasonDeadline {
				t.Fatalf("resolved events = %+v", res)
			}
		})
	}
}

// D14, OD1: off never kills; a 10/12 policy kills — once — with actor
// system:content-flag when `recent` holds at least 10 of 12 at ≥ 0.9, and
// not with fewer than 12 entries (a new session, a restarted sampler) or 9
// hits.
func TestAutoKillPolicy(t *testing.T) {
	hi, lo := 0.95, 0.4
	series := func(hits, total int) []float64 {
		out := make([]float64, total)
		for i := range out {
			out[i] = lo
			if i >= total-hits {
				out[i] = hi
			}
		}
		return out
	}
	policy, err := config.ParseAutoKillPolicy("score>=0.9,hits>=10/12")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		policy *config.AutoKillPolicy
		recent []float64
		kill   bool
	}{
		{"off", nil, series(12, 12), false},
		{"10 of 12", policy, series(10, 12), true},
		{"11 entries", policy, series(11, 11), false},
		{"9 of 12", policy, series(9, 12), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, clock := screeningHarness(t, withConfig(func(c *config.Config) { c.ContentFlagAutoKill = tc.policy }))
			res := h.postFlag(report(flagID, flagEpoch, screening.ModeFlag, screening.StateTripped, tc.recent...), http.StatusCreated)
			bans, _ := h.store.ListBans(t.Context(), store.FilterActive)
			f := h.flag(res.FlagID)
			if !tc.kill {
				if len(bans) != 0 || !f.Unresolved() {
					t.Fatalf("killed: %+v / %+v", bans, f)
				}
				return
			}
			if len(bans) != 1 || bans[0].CreatedBy != store.ActorContentFlag {
				t.Fatalf("bans = %+v", bans)
			}
			if f.Resolution != events.ContentFlagResolutionAutoKilled || f.ResolutionReason != store.FlagReasonPolicy {
				t.Fatalf("flag = %+v", f)
			}
			// The page still went out, and a second matching POST (the relay
			// not yet having dropped the session) does not ban twice.
			if n := len(h.flagEvents(store.EventContentFlag)); n != 1 {
				t.Fatalf("%d raises", n)
			}
			clock.advance(5 * time.Second)
			h.postFlag(report(flagID, flagEpoch, screening.ModeFlag, screening.StateTripped, tc.recent...), http.StatusCreated)
			if bans, _ = h.store.ListBans(t.Context(), store.FilterAll); len(bans) != 1 {
				t.Fatalf("the auto-kill banned %d times", len(bans))
			}
		})
	}
}

// D9 and D10 against a stale read (PR #500 review). The sweep decides on the
// flags it read at the start of a tick, and D14's kill on the POST's own
// flag, while an operator can act at any moment. Acknowledging stops the
// deadline (D9) and dismissing mutes the broadcast (D10): whichever commits
// first wins, and a broadcast is never banned while its flag says it was
// acknowledged or dismissed. The operator acts inside a fleet scan: the
// first is the sweep's (after it read the flags) or the POST's session check,
// the second runs inside the kill path.
func TestAnAutomaticKillAndTheOperatorNeverBothWin(t *testing.T) {
	acknowledge := func(h *harness, id int64, now time.Time) error {
		_, err := h.store.AcknowledgeContentFlag(context.Background(), id, "op@example.com", now)
		return err
	}
	dismiss := func(h *harness, id int64, now time.Time) error {
		_, err := h.store.DismissContentFlag(context.Background(), id, "op@example.com", now, store.Dismissal{Scope: store.MuteScopeDuration, MutedUntil: now.Add(30 * time.Minute)}, nil)
		return err
	}
	policy, err := config.ParseAutoKillPolicy("score>=0.9,hits>=10/12")
	if err != nil {
		t.Fatal(err)
	}
	hot := make([]float64, 12)
	for i := range hot {
		hot[i] = 0.95
	}
	cases := []struct {
		name     string
		policy   bool // D14 on a tripped POST; otherwise D15's deadline in the sweep
		scan     int  // the fleet scan the operator acts in
		act      func(*harness, int64, time.Time) error
		wantKill bool
		want     string // the flag's state or resolution afterwards
	}{
		{"deadline: acknowledged after the sweep read the flags", false, 1, acknowledge, false, store.FlagStateAcknowledged},
		{"deadline: dismissed after the sweep read the flags", false, 1, dismiss, false, events.ContentFlagResolutionDismissed},
		{"deadline: dismissed while the kill runs", false, 2, dismiss, true, events.ContentFlagResolutionAutoKilled},
		{"policy: dismissed while the kill runs", true, 2, dismiss, true, events.ContentFlagResolutionAutoKilled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, clock := screeningHarness(t, withConfig(func(c *config.Config) {
				c.ContentFlagDeadline = 30 * time.Minute
				if tc.policy {
					c.ContentFlagAutoKill = policy
				}
			}))
			var flagged int64
			if !tc.policy {
				flagged = h.postFlag(report(flagID, flagEpoch, screening.ModeFlag, screening.StateTripped), http.StatusCreated).FlagID
				clock.advance(30*time.Minute - 30*time.Second)
				h.postFlag(report(flagID, flagEpoch, screening.ModeFlag, screening.StateTripped), http.StatusOK)
				clock.advance(31 * time.Second)
			}
			var (
				scans  int
				actErr error
			)
			h.fleet.setHook(func(context.Context) {
				scans++
				if scans != tc.scan {
					return
				}
				id := flagged
				if id == 0 { // the POST's own flag, raised a moment ago
					flags := h.allFlags()
					if len(flags) == 0 {
						return // the session check: no flag yet, so act at the kill
					}
					id = flags[0].ID
				}
				actErr = tc.act(h, id, clock.Now())
			})
			if tc.policy {
				// The POST's first scan is its session check, before the flag
				// exists; act in the kill path's scan instead.
				res := h.postFlag(report(flagID, flagEpoch, screening.ModeFlag, screening.StateTripped, hot...), http.StatusCreated)
				flagged = res.FlagID
			} else {
				h.api.ScreeningTick(t.Context())
			}
			h.fleet.setHook(nil)

			bans, err := h.store.ListBans(t.Context(), store.FilterActive)
			if err != nil {
				t.Fatal(err)
			}
			f := h.flag(flagged)
			got := f.State
			if f.State == store.FlagStateResolved {
				got = f.Resolution
			}
			if got != tc.want || (len(bans) > 0) != tc.wantKill {
				t.Fatalf("flag %s with %d bans (operator: %v), want %s and kill=%v", got, len(bans), actErr, tc.want, tc.wantKill)
			}
		})
	}
}

// D9: an operator's kill through R39 resolves the open flag `killed`, and the
// broadcast view pins the flag until then.
func TestKillResolvesTheFlagAndTheViewPinsIt(t *testing.T) {
	h, _ := screeningHarness(t)
	raised := h.postFlag(report(flagID, flagEpoch, screening.ModeFlag, screening.StateTripped), http.StatusCreated)
	h.asOperator()
	var page struct {
		Broadcasts []struct {
			ID          string `json:"id"`
			ContentFlag *struct {
				ID     int64  `json:"id"`
				State  string `json:"state"`
				Signal string `json:"signal"`
			} `json:"contentFlag"`
		} `json:"broadcasts"`
	}
	h.decode(http.MethodGet, "/api/v1/broadcasts", nil, http.StatusOK, &page)
	if len(page.Broadcasts) != 1 || page.Broadcasts[0].ContentFlag == nil || page.Broadcasts[0].ContentFlag.ID != raised.FlagID ||
		page.Broadcasts[0].ContentFlag.Signal != events.ContentFlagSignalLive {
		t.Fatalf("broadcasts = %+v", page.Broadcasts)
	}
	h.decode(http.MethodPost, "/api/v1/broadcasts/"+flagID+"/kill", map[string]any{"reason": "explicit content"}, http.StatusCreated, nil)
	f := h.flag(raised.FlagID)
	if f.Resolution != events.ContentFlagResolutionKilled || f.ResolvedBy != "op@example.com" {
		t.Fatalf("after the kill: %+v", f)
	}
	if res := h.flagEvents(store.EventContentFlagResolved); len(res) != 1 || res[0].ContentFlag().Resolution != events.ContentFlagResolutionKilled {
		t.Fatalf("resolved = %+v", res)
	}
	page.Broadcasts = nil
	h.decode(http.MethodGet, "/api/v1/broadcasts", nil, http.StatusOK, &page)
	if page.Broadcasts[0].ContentFlag != nil {
		t.Fatal("a resolved flag is still pinned")
	}
}

// The operator's actions refuse the wrong state with 409 content_flag_state,
// and an unknown flag is a 404.
func TestFlagActionStates(t *testing.T) {
	h, _ := screeningHarness(t)
	raised := h.postFlag(report(flagID, flagEpoch, screening.ModeFlag, screening.StateTripped), http.StatusCreated)
	h.asOperator()
	path := "/api/v1/content-flags/" + itoa(raised.FlagID)
	h.decode(http.MethodPost, path+"/acknowledge", nil, http.StatusOK, nil)
	if code := h.errorCode(http.MethodPost, path+"/acknowledge", nil, http.StatusConflict); code != api.CodeContentFlagState {
		t.Fatalf("acknowledge twice: %s", code)
	}
	h.decode(http.MethodPost, path+"/dismiss", nil, http.StatusOK, nil)
	if code := h.errorCode(http.MethodPost, path+"/dismiss", nil, http.StatusConflict); code != api.CodeContentFlagState {
		t.Fatalf("dismiss twice: %s", code)
	}
	h.errorCode(http.MethodPost, "/api/v1/content-flags/999999/acknowledge", nil, http.StatusNotFound)
	h.errorCode(http.MethodPost, "/api/v1/content-flags/x/dismiss", nil, http.StatusNotFound)
	h.errorCode(http.MethodPost, path+"/acknowledge", map[string]any{"note": "x"}, http.StatusBadRequest)
	h.errorCode(http.MethodGet, "/api/v1/content-flags?state=open", nil, http.StatusBadRequest)
	h.errorCode(http.MethodGet, "/api/v1/content-flags?mode=page", nil, http.StatusBadRequest)
}

// /me tells the SPA whether screening is served.
func TestMeReportsScreening(t *testing.T) {
	for _, on := range []bool{false, true} {
		opts := []harnessOption{}
		if on {
			opts = append(opts, withScreening())
		}
		h := newHarnessWithoutPostgres(t, opts...)
		var me struct {
			Features struct {
				Screening bool `json:"screening"`
			} `json:"features"`
		}
		h.decode(http.MethodGet, "/api/v1/me", nil, http.StatusOK, &me)
		if me.Features.Screening != on {
			t.Fatalf("screening %v: features.screening = %v", on, me.Features.Screening)
		}
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
