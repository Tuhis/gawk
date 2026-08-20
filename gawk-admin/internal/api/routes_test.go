package api_test

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-admin/internal/api"
	"github.com/Tuhis/gawk/gawk-admin/internal/config"
	"github.com/Tuhis/gawk/gawk-admin/internal/relayscan"
	"github.com/Tuhis/gawk/gawk-admin/internal/store"
	"github.com/Tuhis/gawk/gawk-server/moderation"
)

// --- GET /broadcasts --------------------------------------------------

func TestListBroadcastsJoinsFleetBanStateAndLinks(t *testing.T) {
	h := newHarness(t)
	h.fleet.set(liveSnapshot("ABC234", "3f9a1c2b4d5e", "203.0.113.7"))

	var body struct {
		Broadcasts []wireBroadcast `json:"broadcasts"`
	}
	h.decode(http.MethodGet, "/api/v1/broadcasts", nil, http.StatusOK, &body)
	if len(body.Broadcasts) != 1 {
		t.Fatalf("broadcasts = %+v", body.Broadcasts)
	}
	b := body.Broadcasts[0]
	if b.ID != "ABC234" || b.Key != "3f9a1c2b4d5e" || !b.PublisherActive || b.ViewersGlobal != 340 {
		t.Fatalf("broadcast = %+v", b)
	}
	if b.PublisherRemoteIP != "203.0.113.7" {
		t.Fatalf("publisherRemoteIp = %q", b.PublisherRemoteIP)
	}
	if len(b.Pods) != 2 || b.Pods[0].Role != "origin" || b.Pods[1].ViewersLocal != 328 {
		t.Fatalf("pods = %+v", b.Pods)
	}
	if b.Links == nil || b.Links.Watch != "https://gawk.example/#/view/ABC234" {
		t.Fatalf("watch link = %+v", b.Links)
	}
	if b.Links.Telemetry != "https://telemetry.example/#/broadcast/3f9a1c2b4d5e" {
		t.Fatalf("telemetry link = %q", b.Links.Telemetry)
	}
	if b.BanState == nil || b.BanState.Banned {
		t.Fatalf("banState on an unbanned broadcast = %+v", b.BanState)
	}

	// An active ID ban surfaces on the row.
	created, err := h.store.CreateBan(t.Context(), store.Ban{
		Target: moderation.Target{Type: moderation.TargetBroadcastID, Value: "ABC234"}, CreatedBy: "op"})
	if err != nil {
		t.Fatalf("CreateBan: %v", err)
	}
	h.decode(http.MethodGet, "/api/v1/broadcasts", nil, http.StatusOK, &body)
	got := body.Broadcasts[0].BanState
	if got == nil || !got.Banned || got.Ban == nil || got.Ban.ID != created.ID.String() {
		t.Fatalf("banState after an ID ban = %+v", got)
	}
}

// An IP ban covers a broadcast by PREFIX, not by string equality: a /24 ban
// covers a publisher the operator never typed, and the portal must say so.
func TestListBroadcastsMatchesIPBansByPrefix(t *testing.T) {
	h := newHarness(t)
	h.fleet.set(liveSnapshot("ABC234", "key", "203.0.113.7"))

	if _, err := h.store.CreateBan(t.Context(), store.Ban{
		Target: moderation.Target{Type: moderation.TargetIP, Value: "203.0.113.0/24"}, CreatedBy: "op"}); err != nil {
		t.Fatalf("CreateBan: %v", err)
	}
	var body struct {
		Broadcasts []wireBroadcast `json:"broadcasts"`
	}
	h.decode(http.MethodGet, "/api/v1/broadcasts", nil, http.StatusOK, &body)
	state := body.Broadcasts[0].BanState
	if state == nil || !state.Banned || state.Ban == nil || state.Ban.Target.Value != "203.0.113.0/24" {
		t.Fatalf("banState = %+v, want the covering /24", state)
	}
}

// Deep links are OMITTED when their base URL is unconfigured — a dead link is
// worse than no link (§4.7).
func TestListBroadcastsOmitsLinksWithoutBaseURLs(t *testing.T) {
	h := newHarness(t, withConfig(func(c *config.Config) {
		c.AppBaseURL = ""
		c.TelemetryBaseURL = ""
	}))
	h.fleet.set(liveSnapshot("ABC234", "key", "203.0.113.7"))

	var body struct {
		Broadcasts []wireBroadcast `json:"broadcasts"`
	}
	h.decode(http.MethodGet, "/api/v1/broadcasts", nil, http.StatusOK, &body)
	if body.Broadcasts[0].Links != nil {
		t.Fatalf("links rendered with no base URLs: %+v", body.Broadcasts[0].Links)
	}
}

// --- POST /broadcasts/{id}/kill ---------------------------------------

func TestKillCreatesCooldownBanEventAndCR(t *testing.T) {
	h := newHarness(t)
	h.fleet.set(liveSnapshot("ABC234", "3f9a1c2b4d5e", "203.0.113.7"))

	var out struct {
		Ban wireBan `json:"ban"`
	}
	h.decode(http.MethodPost, "/api/v1/broadcasts/abc234/kill",
		map[string]any{"reason": "terms violation"}, http.StatusCreated, &out)

	if out.Ban.Target.Type != string(moderation.TargetBroadcastID) || out.Ban.Target.Value != "ABC234" {
		t.Fatalf("ban target = %+v (the ID must be normalized)", out.Ban.Target)
	}
	if out.Ban.State != string(store.BanActive) || out.Ban.CreatedBy != "op@example.com" {
		t.Fatalf("ban = %+v", out.Ban)
	}
	if out.Ban.SourceBroadcastID != "ABC234" || out.Ban.CRName != "ban-id-abc234" {
		t.Fatalf("ban = %+v", out.Ban)
	}
	if out.Ban.ExpiresAt == nil {
		t.Fatalf("a plain kill must carry the cooldown expiry (D5)")
	}
	expiry, err := time.Parse(time.RFC3339, *out.Ban.ExpiresAt)
	if err != nil {
		t.Fatalf("expiresAt: %v", err)
	}
	if d := time.Until(expiry); d < 9*time.Minute || d > 11*time.Minute {
		t.Fatalf("cooldown = %v, want the configured 10m default", d)
	}

	// The CR was projected inline, from this replica, in the same request.
	projected, ok := h.proj.last()
	if !ok || projected.CRName != "ban-id-abc234" {
		t.Fatalf("projected = %+v ok=%v", projected, ok)
	}
	if h.kicks.count() == 0 {
		t.Fatalf("the reconciler was not kicked after a mutation")
	}
	if h.fleet.invalidated == 0 {
		t.Fatalf("the fleet cache was not invalidated after a mutation")
	}

	// One broadcast.killed event, offered to the dispatcher, carrying the
	// HMAC'd key — never the raw ID — in its webhook-safe fields.
	events := h.enq.all()
	if len(events) != 1 || events[0].Type != store.EventBroadcastKilled {
		t.Fatalf("enqueued = %+v", events)
	}
	ev := events[0]
	if ev.BroadcastKey != "3f9a1c2b4d5e" || ev.BroadcastID != "ABC234" || ev.Actor != "op@example.com" {
		t.Fatalf("event = %+v", ev)
	}
	summary := ev.PayloadString(store.PayloadSummary)
	if summary == "" || strings.Contains(summary, "ABC234") {
		t.Fatalf("summary %q must not carry the raw broadcast ID (D8)", summary)
	}
	if !strings.Contains(summary, "3f9a1c2b4d5e") {
		t.Fatalf("summary %q should name the HMAC'd key", summary)
	}
	if ev.PayloadString(store.PayloadReason) != "terms violation" {
		t.Fatalf("reason payload = %q", ev.PayloadString(store.PayloadReason))
	}

	// Reasons are operator-private: Info must not carry one (docs/42 §5).
	for _, line := range strings.Split(h.logText(), "\n") {
		if strings.Contains(line, "level=INFO") && strings.Contains(line, "terms violation") {
			t.Fatalf("a ban reason leaked into an Info log line: %s", line)
		}
	}
}

func TestKillHonoursCooldownOverrideAndRejectsBadInput(t *testing.T) {
	h := newHarness(t)
	h.fleet.set(liveSnapshot("ABC234", "key", "203.0.113.7"))

	var out struct {
		Ban wireBan `json:"ban"`
	}
	h.decode(http.MethodPost, "/api/v1/broadcasts/ABC234/kill",
		map[string]any{"reason": "spam", "cooldownSeconds": 60}, http.StatusCreated, &out)
	expiry, _ := time.Parse(time.RFC3339, *out.Ban.ExpiresAt)
	if d := time.Until(expiry); d > 2*time.Minute {
		t.Fatalf("cooldown override ignored: %v", d)
	}

	// A missing reason is a 400: an audit trail with no reason is worthless.
	if code := h.errorCode(http.MethodPost, "/api/v1/broadcasts/BBB234/kill",
		map[string]any{"reason": "   "}, http.StatusBadRequest); code != api.CodeBadRequest {
		t.Fatalf("blank reason error code = %q", code)
	}
	if code := h.errorCode(http.MethodPost, "/api/v1/broadcasts/BBB234/kill",
		map[string]any{"reason": "x", "cooldownSeconds": 0}, http.StatusBadRequest); code != api.CodeBadRequest {
		t.Fatalf("zero cooldown error code = %q", code)
	}
	// An ID outside the broadcast alphabet never reaches the store.
	if code := h.errorCode(http.MethodPost, "/api/v1/broadcasts/notanid/kill",
		map[string]any{"reason": "x"}, http.StatusBadRequest); code != api.CodeInvalidTarget {
		t.Fatalf("invalid ID error code = %q", code)
	}
}

// A second kill of an already-banned broadcast answers 409 WITH the ban that
// is already in force (§4.7) — the operator sees the state, not a bare
// conflict.
func TestKillConflictsWithAnExistingActiveBan(t *testing.T) {
	h := newHarness(t)
	h.fleet.set(liveSnapshot("ABC234", "key", "203.0.113.7"))

	var first struct {
		Ban wireBan `json:"ban"`
	}
	h.decode(http.MethodPost, "/api/v1/broadcasts/ABC234/kill",
		map[string]any{"reason": "first"}, http.StatusCreated, &first)

	var conflict struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
		Ban wireBan `json:"ban"`
	}
	h.decode(http.MethodPost, "/api/v1/broadcasts/abc234/kill",
		map[string]any{"reason": "second"}, http.StatusConflict, &conflict)
	if conflict.Error.Code != api.CodeDuplicateActive {
		t.Fatalf("conflict code = %q", conflict.Error.Code)
	}
	if conflict.Ban.ID != first.Ban.ID {
		t.Fatalf("409 returned ban %q, want the existing %q", conflict.Ban.ID, first.Ban.ID)
	}
	// Exactly one ban, exactly one event: the conflict wrote nothing.
	bans, err := h.store.ListBans(t.Context(), store.FilterAll)
	if err != nil || len(bans) != 1 {
		t.Fatalf("bans = %d (err=%v)", len(bans), err)
	}
	if got := len(h.enq.all()); got != 1 {
		t.Fatalf("events = %d, want 1", got)
	}
}

// A CR write that fails must not be reported as a clean success: the row is
// committed and the reconciler will heal it, and the operator is told so.
func TestKillReportsAProjectionFailureHonestly(t *testing.T) {
	h := newHarness(t, withProjectorError(errors.New("kubernetes API is unreachable")))
	h.fleet.set(liveSnapshot("ABC234", "key", "203.0.113.7"))

	var out struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
		Ban wireBan `json:"ban"`
	}
	h.decode(http.MethodPost, "/api/v1/broadcasts/ABC234/kill",
		map[string]any{"reason": "spam"}, http.StatusBadGateway, &out)
	if out.Error.Code != api.CodeProjectionFail {
		t.Fatalf("code = %q, want %q", out.Error.Code, api.CodeProjectionFail)
	}
	if out.Ban.ID == "" {
		t.Fatalf("the response must still carry the recorded ban")
	}
	bans, err := h.store.ListBans(t.Context(), store.FilterActive)
	if err != nil || len(bans) != 1 {
		t.Fatalf("the ban row must be committed regardless: %d rows (err=%v)", len(bans), err)
	}
}

// --- POST /bans -------------------------------------------------------

// The §4.9 contract: the literal "publisher" resolves through relayscan and
// the operator-confirmed prefix is applied.
func TestCreateBanResolvesPublisherIP(t *testing.T) {
	h := newHarness(t)
	h.fleet.set(liveSnapshot("ABC234", "3f9a1c2b4d5e", "203.0.113.7"))

	var ban wireBan
	h.decode(http.MethodPost, "/api/v1/bans", map[string]any{
		"target":            map[string]any{"type": "ip", "value": "publisher", "prefixLength": 32},
		"expiresAt":         nil,
		"reason":            "repeat offender",
		"sourceBroadcastId": "ABC234",
	}, http.StatusCreated, &ban)

	if ban.Target.Type != "ip" || ban.Target.Value != "203.0.113.7/32" {
		t.Fatalf("resolved target = %+v", ban.Target)
	}
	if ban.ExpiresAt != nil {
		t.Fatalf("expiresAt null must mean permanent, got %v", *ban.ExpiresAt)
	}
	if ban.SourceBroadcastID != "ABC234" {
		t.Fatalf("sourceBroadcastId = %q", ban.SourceBroadcastID)
	}
	// The event carries the HMAC'd key and never the resolved IP in its
	// webhook-safe summary (D8).
	events := h.enq.all()
	if len(events) != 1 || events[0].BroadcastKey != "3f9a1c2b4d5e" {
		t.Fatalf("events = %+v", events)
	}
	if s := events[0].PayloadString(store.PayloadSummary); strings.Contains(s, "203.0.113.7") {
		t.Fatalf("summary %q leaks the publisher IP (D8)", s)
	}
}

// A wider prefix is honoured, and a /24 ban on a v4 publisher is the operator's
// call — but a prefix length that does not belong to the family is refused
// rather than coerced.
func TestCreateBanPrefixLengthRules(t *testing.T) {
	h := newHarness(t)
	h.fleet.set(liveSnapshot("ABC234", "key", "203.0.113.7"))

	var ban wireBan
	h.decode(http.MethodPost, "/api/v1/bans", map[string]any{
		"target":            map[string]any{"type": "ip", "value": "publisher", "prefixLength": 24},
		"expiresAt":         nil,
		"reason":            "shared host",
		"sourceBroadcastId": "ABC234",
	}, http.StatusCreated, &ban)
	if ban.Target.Value != "203.0.113.0/24" {
		t.Fatalf("target = %q, want the masked /24", ban.Target.Value)
	}

	// /64 on a v4 address is a family mismatch, not a coercion.
	code := h.errorCode(http.MethodPost, "/api/v1/bans", map[string]any{
		"target":            map[string]any{"type": "ip", "value": "publisher", "prefixLength": 64},
		"expiresAt":         nil,
		"reason":            "x",
		"sourceBroadcastId": "ABC234",
	}, http.StatusBadRequest)
	if code != api.CodeInvalidTarget {
		t.Fatalf("family mismatch code = %q", code)
	}

	// A literal address takes prefixLength too, not only "publisher".
	h.decode(http.MethodPost, "/api/v1/bans", map[string]any{
		"target":    map[string]any{"type": "ip", "value": "198.51.100.9", "prefixLength": 32},
		"expiresAt": nil,
		"reason":    "literal",
	}, http.StatusCreated, &ban)
	if ban.Target.Value != "198.51.100.9/32" {
		t.Fatalf("literal target = %q", ban.Target.Value)
	}

	// v6 defaults to /64 when no prefix is confirmed (§4.9).
	h.decode(http.MethodPost, "/api/v1/bans", map[string]any{
		"target":    map[string]any{"type": "ip", "value": "2001:db8::1"},
		"expiresAt": nil,
		"reason":    "v6 default",
	}, http.StatusCreated, &ban)
	if ban.Target.Value != "2001:db8::/64" {
		t.Fatalf("v6 default target = %q, want 2001:db8::/64", ban.Target.Value)
	}

	// A prefixLength that disagrees with an explicit CIDR is an error: one of
	// the two is not what the operator meant.
	code = h.errorCode(http.MethodPost, "/api/v1/bans", map[string]any{
		"target":    map[string]any{"type": "ip", "value": "192.0.2.0/24", "prefixLength": 32},
		"expiresAt": nil,
		"reason":    "x",
	}, http.StatusBadRequest)
	if code != api.CodeInvalidTarget {
		t.Fatalf("CIDR/prefixLength disagreement code = %q", code)
	}
}

func TestCreateBanPublisherResolutionFailures(t *testing.T) {
	h := newHarness(t)
	h.fleet.set(relayscan.Snapshot{PodsResolved: 1, PodsAnswered: 1})

	// "publisher" with no sourceBroadcastId cannot be resolved.
	if code := h.errorCode(http.MethodPost, "/api/v1/bans", map[string]any{
		"target": map[string]any{"type": "ip", "value": "publisher"}, "expiresAt": nil, "reason": "x",
	}, http.StatusBadRequest); code != api.CodeInvalidTarget {
		t.Fatalf("code = %q", code)
	}
	// A broadcast that is not live has no publisher to resolve.
	if code := h.errorCode(http.MethodPost, "/api/v1/bans", map[string]any{
		"target": map[string]any{"type": "ip", "value": "publisher"}, "expiresAt": nil,
		"reason": "x", "sourceBroadcastId": "ABC234",
	}, http.StatusBadRequest); code != api.CodeInvalidTarget {
		t.Fatalf("code = %q", code)
	}
}

func TestCreateBanConflictsOnDuplicateActiveTarget(t *testing.T) {
	h := newHarness(t)

	body := map[string]any{
		"target": map[string]any{"type": "broadcastId", "value": "ABC234"}, "expiresAt": nil, "reason": "first",
	}
	var first wireBan
	h.decode(http.MethodPost, "/api/v1/bans", body, http.StatusCreated, &first)

	var conflict struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
		Ban wireBan `json:"ban"`
	}
	// Differently cased, same target after normalization.
	body["target"] = map[string]any{"type": "broadcastId", "value": "abc234"}
	h.decode(http.MethodPost, "/api/v1/bans", body, http.StatusConflict, &conflict)
	if conflict.Error.Code != api.CodeDuplicateActive || conflict.Ban.ID != first.ID {
		t.Fatalf("conflict = %+v", conflict)
	}
}

func TestCreateBanValidatesExpiryAndTargetType(t *testing.T) {
	h := newHarness(t)

	if code := h.errorCode(http.MethodPost, "/api/v1/bans", map[string]any{
		"target":    map[string]any{"type": "broadcastId", "value": "ABC234"},
		"expiresAt": "not-a-time", "reason": "x",
	}, http.StatusBadRequest); code != api.CodeBadRequest {
		t.Fatalf("bad expiry code = %q", code)
	}
	if code := h.errorCode(http.MethodPost, "/api/v1/bans", map[string]any{
		"target": map[string]any{"type": "email", "value": "a@b"}, "expiresAt": nil, "reason": "x",
	}, http.StatusBadRequest); code != api.CodeInvalidTarget {
		t.Fatalf("unknown target type code = %q", code)
	}
	// prefixLength is meaningless on a broadcast-ID target.
	if code := h.errorCode(http.MethodPost, "/api/v1/bans", map[string]any{
		"target":    map[string]any{"type": "broadcastId", "value": "ABC234", "prefixLength": 32},
		"expiresAt": nil, "reason": "x",
	}, http.StatusBadRequest); code != api.CodeInvalidTarget {
		t.Fatalf("prefixLength on an ID target code = %q", code)
	}
}

func TestCreateBanAcceptsAnExplicitExpiry(t *testing.T) {
	h := newHarness(t)
	want := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)

	var ban wireBan
	h.decode(http.MethodPost, "/api/v1/bans", map[string]any{
		"target":    map[string]any{"type": "broadcastId", "value": "ABC234"},
		"expiresAt": want.Format(time.RFC3339),
		"reason":    "24h",
	}, http.StatusCreated, &ban)
	if ban.ExpiresAt == nil {
		t.Fatalf("expiresAt missing")
	}
	got, err := time.Parse(time.RFC3339, *ban.ExpiresAt)
	if err != nil || !got.Equal(want) {
		t.Fatalf("expiresAt = %v want %v (err=%v)", got, want, err)
	}
}

// --- GET /bans, DELETE /bans/{id} -------------------------------------

func TestListBansFiltersAndUnbanRoundTrip(t *testing.T) {
	h := newHarness(t)

	var keep, drop wireBan
	h.decode(http.MethodPost, "/api/v1/bans", map[string]any{
		"target": map[string]any{"type": "broadcastId", "value": "ABC234"}, "expiresAt": nil, "reason": "keep",
	}, http.StatusCreated, &keep)
	h.decode(http.MethodPost, "/api/v1/bans", map[string]any{
		"target": map[string]any{"type": "broadcastId", "value": "BBB234"}, "expiresAt": nil, "reason": "drop",
	}, http.StatusCreated, &drop)

	h.decode(http.MethodDelete, "/api/v1/bans/"+drop.ID, nil, http.StatusNoContent, nil)

	// The CR was deleted inline (Project on a non-active ban).
	projected, ok := h.proj.last()
	if !ok || projected.State != store.BanRemoved {
		t.Fatalf("unban did not project a removal: %+v ok=%v", projected, ok)
	}

	var active struct {
		Bans []wireBan `json:"bans"`
	}
	h.decode(http.MethodGet, "/api/v1/bans?state=active", nil, http.StatusOK, &active)
	if len(active.Bans) != 1 || active.Bans[0].ID != keep.ID {
		t.Fatalf("active bans = %+v", active.Bans)
	}
	var all struct {
		Bans []wireBan `json:"bans"`
	}
	h.decode(http.MethodGet, "/api/v1/bans?state=all", nil, http.StatusOK, &all)
	if len(all.Bans) != 2 {
		t.Fatalf("all bans = %d", len(all.Bans))
	}
	for _, b := range all.Bans {
		if b.ID != drop.ID {
			continue
		}
		if b.State != string(store.BanRemoved) || b.RemovedBy != "op@example.com" || b.RemovedAt == nil {
			t.Fatalf("removed ban = %+v", b)
		}
	}
	// A ban.removed event was recorded and offered to the dispatcher.
	events := h.enq.all()
	if len(events) != 3 || events[2].Type != store.EventBanRemoved {
		t.Fatalf("events = %+v", events)
	}

	if code := h.errorCode(http.MethodGet, "/api/v1/bans?state=activee", nil, http.StatusBadRequest); code != api.CodeBadRequest {
		t.Fatalf("unknown state filter code = %q", code)
	}
	if code := h.errorCode(http.MethodDelete, "/api/v1/bans/not-a-uuid", nil, http.StatusNotFound); code != api.CodeNotFound {
		t.Fatalf("malformed ban id code = %q", code)
	}
}

// --- GET /events ------------------------------------------------------

func TestEventsCursorPaginationAndDeliveryVisibility(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()

	var ids []int64
	for i := range 5 {
		ev, err := h.store.AppendEvent(ctx, store.Event{
			Type: store.EventBanCreated, Actor: "op@example.com",
			BroadcastKey: "3f9a1c2b4d5e", BroadcastID: "ABC234",
			OccurredAt: time.Now().Add(time.Duration(i) * time.Second),
			Payload:    []byte(`{"reason":"spam","summary":"a broadcast ban was created by op@example.com"}`),
		})
		if err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
		ids = append(ids, ev.ID)
	}
	// A failed delivery on the newest event: it MUST be visible (§4.10).
	if err := h.store.EnqueueDeliveries(ctx, ids[4], []string{"ntfy"}); err != nil {
		t.Fatalf("EnqueueDeliveries: %v", err)
	}
	claimed, err := h.store.ClaimDueDeliveries(ctx, time.Now(), 10)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("ClaimDueDeliveries: %d (err=%v)", len(claimed), err)
	}
	if err := h.store.MarkDeliveryFailed(ctx, claimed[0].ID, "connection refused"); err != nil {
		t.Fatalf("MarkDeliveryFailed: %v", err)
	}

	type page struct {
		Events      []wireEvent `json:"events"`
		NextAfterID *int64      `json:"nextAfterId"`
	}
	var p1 page
	h.decode(http.MethodGet, "/api/v1/events?limit=2", nil, http.StatusOK, &p1)
	if len(p1.Events) != 2 || p1.Events[0].ID != ids[4] || p1.Events[1].ID != ids[3] {
		t.Fatalf("page 1 = %+v", p1.Events)
	}
	if p1.NextAfterID == nil || *p1.NextAfterID != ids[3] {
		t.Fatalf("nextAfterId = %v, want %d", p1.NextAfterID, ids[3])
	}
	if p1.Events[0].Reason != "spam" || p1.Events[0].Summary == "" {
		t.Fatalf("event fields = %+v", p1.Events[0])
	}
	if p1.Events[0].BroadcastKey != "3f9a1c2b4d5e" || p1.Events[0].BroadcastID != "ABC234" {
		t.Fatalf("event = %+v", p1.Events[0])
	}
	if len(p1.Events[0].Deliveries) != 1 || p1.Events[0].Deliveries[0].State != string(store.DeliveryFailed) {
		t.Fatalf("a failed delivery is not visible in the feed: %+v", p1.Events[0].Deliveries)
	}
	if p1.Events[0].Deliveries[0].LastError != "connection refused" {
		t.Fatalf("delivery error = %q", p1.Events[0].Deliveries[0].LastError)
	}

	var p2 page
	h.decode(http.MethodGet, "/api/v1/events?limit=2&afterId="+strconv.FormatInt(*p1.NextAfterID, 10), nil, http.StatusOK, &p2)
	if len(p2.Events) != 2 || p2.Events[0].ID != ids[2] {
		t.Fatalf("page 2 = %+v", p2.Events)
	}

	var p3 page
	h.decode(http.MethodGet, "/api/v1/events?limit=2&afterId="+strconv.FormatInt(*p2.NextAfterID, 10), nil, http.StatusOK, &p3)
	if len(p3.Events) != 1 || p3.Events[0].ID != ids[0] {
		t.Fatalf("page 3 = %+v", p3.Events)
	}
	// A short page is the end of the feed: no cursor.
	if p3.NextAfterID != nil {
		t.Fatalf("nextAfterId on the final page = %v, want null", *p3.NextAfterID)
	}

	if code := h.errorCode(http.MethodGet, "/api/v1/events?limit=0", nil, http.StatusBadRequest); code != api.CodeBadRequest {
		t.Fatalf("limit=0 code = %q", code)
	}
	if code := h.errorCode(http.MethodGet, "/api/v1/events?afterId=nope", nil, http.StatusBadRequest); code != api.CodeBadRequest {
		t.Fatalf("bad afterId code = %q", code)
	}
}

// --- GET /relays ------------------------------------------------------

func TestListRelaysIsReadOnlyPerPodView(t *testing.T) {
	h := newHarness(t)
	h.fleet.set(liveSnapshot("ABC234", "key", "203.0.113.7"))

	var body struct {
		Relays []struct {
			Pod       string         `json:"pod"`
			Reachable bool           `json:"reachable"`
			Version   string         `json:"version"`
			Config    map[string]any `json:"config"`
			Error     string         `json:"error"`
		} `json:"relays"`
	}
	h.decode(http.MethodGet, "/api/v1/relays", nil, http.StatusOK, &body)
	if len(body.Relays) != 2 {
		t.Fatalf("relays = %+v", body.Relays)
	}
	if !body.Relays[0].Reachable || body.Relays[0].Version != "1.42.0" || body.Relays[0].Config["addr"] != ":4433" {
		t.Fatalf("reachable pod = %+v", body.Relays[0])
	}
	if body.Relays[1].Reachable || body.Relays[1].Error == "" {
		t.Fatalf("unreachable pod = %+v", body.Relays[1])
	}

	// D10: there is no write path of any kind on this resource.
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		if status, _ := h.raw(method, "/api/v1/relays", map[string]any{"addr": ":9999"}); status != http.StatusNotFound {
			t.Fatalf("%s /api/v1/relays = %d, want 404: settings are read-only (D10)", method, status)
		}
	}
}
