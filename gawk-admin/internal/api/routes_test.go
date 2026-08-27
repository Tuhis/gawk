package api_test

import (
	"bytes"
	"context"
	"encoding/json"
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
		Broadcasts   []wireBroadcast `json:"broadcasts"`
		PodsResolved int             `json:"podsResolved"`
		PodsAnswered int             `json:"podsAnswered"`
	}
	h.decode(http.MethodGet, "/api/v1/broadcasts", nil, http.StatusOK, &body)
	if len(body.Broadcasts) != 1 {
		t.Fatalf("broadcasts = %+v", body.Broadcasts)
	}
	// Coverage rides the envelope so a partial scan is visible to the UI: an
	// empty list from an unreachable fleet must not read as a quiet fleet.
	// The fixture is asymmetric (see liveSnapshot) so swapped keys fail here.
	if body.PodsResolved != 3 || body.PodsAnswered != 2 {
		t.Fatalf("coverage = %d/%d, want 2/3", body.PodsAnswered, body.PodsResolved)
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

// A CR write that fails is neither a clean success nor an error: the row is
// committed, the reconciler will heal it, and 202 Accepted is what says so.
//
// It is NOT a 5xx (nothing failed that the operator can retry — a retry would
// 409 against the row that now exists) and it is NOT a 502 (nothing acted as a
// gateway). The `enforcement` object is what carries the difference from a
// 201, and the body keeps kill's `{ban}` envelope either way.
func TestKillProjectionFailureIsAccepted(t *testing.T) {
	h := newHarness(t, withProjectorError(errors.New("kubernetes API is unreachable")))
	h.fleet.set(liveSnapshot("ABC234", "key", "203.0.113.7"))

	var out struct {
		Ban wireBan `json:"ban"`
	}
	h.decode(http.MethodPost, "/api/v1/broadcasts/ABC234/kill",
		map[string]any{"reason": "spam"}, http.StatusAccepted, &out)
	if out.Ban.ID == "" {
		t.Fatalf("the 202 must still carry the recorded ban — a client needs its ID")
	}
	if out.Ban.Enforcement == nil || out.Ban.Enforcement.InSync {
		t.Fatalf("enforcement = %+v, want inSync:false", out.Ban.Enforcement)
	}
	if out.Ban.Enforcement.Detail != api.DetailBanPending {
		t.Fatalf("detail = %q", out.Ban.Enforcement.Detail)
	}
	bans, err := h.store.ListBans(t.Context(), store.FilterActive)
	if err != nil || len(bans) != 1 {
		t.Fatalf("the ban row must be committed regardless: %d rows (err=%v)", len(bans), err)
	}
	// The event is recorded too: the kill happened, whatever the CR did.
	if got := len(h.enq.all()); got != 1 {
		t.Fatalf("events = %d, want 1", got)
	}
}

// A 201 carries NO enforcement object. Absence is the signal that record and
// enforcement agree, so a 201 that grew one would make every client that reads
// `enforcement.inSync` have to distinguish two shapes of success.
func TestASuccessfulMutationCarriesNoEnforcement(t *testing.T) {
	h := newHarness(t)
	h.fleet.set(liveSnapshot("ABC234", "key", "203.0.113.7"))

	var killed struct {
		Ban wireBan `json:"ban"`
	}
	h.decode(http.MethodPost, "/api/v1/broadcasts/ABC234/kill",
		map[string]any{"reason": "spam"}, http.StatusCreated, &killed)
	if killed.Ban.Enforcement != nil {
		t.Fatalf("kill 201 carried enforcement: %+v", killed.Ban.Enforcement)
	}

	var created wireBan
	h.decode(http.MethodPost, "/api/v1/bans", map[string]any{
		"target": map[string]any{"type": "broadcastId", "value": "BBB234"}, "expiresAt": nil, "reason": "x",
	}, http.StatusCreated, &created)
	if created.Enforcement != nil {
		t.Fatalf("create 201 carried enforcement: %+v", created.Enforcement)
	}

	// And a clean unban is still an empty 204.
	status, body := h.raw(http.MethodDelete, "/api/v1/bans/"+created.ID, nil)
	if status != http.StatusNoContent || strings.TrimSpace(body) != "" {
		t.Fatalf("clean unban = %d %q, want 204 with no body", status, body)
	}
}

// The read surface must stay byte-identical: `enforcement` reports on ONE
// in-flight mutation, and a stored row has none. A list that grew the key
// would be claiming something about every row it returns.
func TestListAndReadRoutesNeverCarryEnforcement(t *testing.T) {
	h := newHarness(t, withProjectorError(errors.New("kubernetes API is unreachable")))
	h.fleet.set(liveSnapshot("ABC234", "key", "203.0.113.7"))

	// A ban created through the failing projector: its row is committed and
	// its CR is not, which is the state most likely to leak into a read.
	h.decode(http.MethodPost, "/api/v1/broadcasts/ABC234/kill",
		map[string]any{"reason": "spam"}, http.StatusAccepted, nil)

	for _, path := range []string{"/api/v1/bans?state=active", "/api/v1/bans?state=all", "/api/v1/broadcasts"} {
		_, body := h.raw(http.MethodGet, path, nil)
		if strings.Contains(body, "enforcement") {
			t.Fatalf("GET %s leaked an enforcement key: %s", path, body)
		}
	}
	// Nor does the 409, whose ban is a read of an existing row.
	_, conflict := h.raw(http.MethodPost, "/api/v1/broadcasts/ABC234/kill", map[string]any{"reason": "again"})
	if strings.Contains(conflict, "enforcement") {
		t.Fatalf("the 409 body leaked an enforcement key: %s", conflict)
	}
}

// The 202's verdict must reach the EVENT, not just the HTTP response.
//
// The webhook is derived from the event, and it is what the operator reads on
// a phone — away from the portal that showed them the 202. An event announcing
// a kill the relays were never told about is not a statement of something that
// happened, so the grade rides in the payload (machine-readable) and in the
// summary (the sentence a dumb ntfy bridge renders).
func TestAPendingMutationRecordsThePendingStateOnItsEvent(t *testing.T) {
	h := newHarness(t, withProjectorError(errors.New("kubernetes API is unreachable")))
	h.fleet.set(liveSnapshot("ABC234", "3f9a1c2b4d5e", "203.0.113.7"))

	h.decode(http.MethodPost, "/api/v1/broadcasts/ABC234/kill",
		map[string]any{"reason": "spam"}, http.StatusAccepted, nil)
	h.decode(http.MethodPost, "/api/v1/bans", map[string]any{
		"target": map[string]any{"type": "ip", "value": "203.0.113.7", "prefixLength": 32},
		"reason": "repeat offender", "expiresAt": nil,
	}, http.StatusAccepted, nil)

	events := h.enq.all()
	if len(events) != 2 {
		t.Fatalf("events = %+v, want the kill and the ban", events)
	}
	for _, tc := range []struct {
		what       string
		ev         store.Event
		wantType   string
		mustNotSay string
	}{
		{what: "kill", ev: events[0], wantType: store.EventBroadcastKilled, mustNotSay: "was terminated"},
		{what: "ban", ev: events[1], wantType: store.EventBanCreated, mustNotSay: "was created"},
	} {
		if tc.ev.Type != tc.wantType {
			t.Fatalf("%s event type = %q, want %q", tc.what, tc.ev.Type, tc.wantType)
		}
		if got := tc.ev.EnforcementState(); got != store.EnforcementPending {
			t.Errorf("%s event enforcement = %q, want %q; the webhook would announce an enforcement that has not started",
				tc.what, got, store.EnforcementPending)
		}
		summary := tc.ev.PayloadString(store.PayloadSummary)
		if !strings.Contains(summary, "NOT enforced yet") {
			t.Errorf("%s summary %q must say the action is not enforced yet", tc.what, summary)
		}
		if strings.Contains(summary, tc.mustNotSay) {
			t.Errorf("%s summary %q claims %q, which has not happened", tc.what, summary, tc.mustNotSay)
		}
		if strings.Contains(summary, "ABC234") || strings.Contains(summary, "203.0.113.7") {
			t.Errorf("%s summary %q names a raw ID or an address (D8)", tc.what, summary)
		}
	}
}

// The unban half, and the direction an operator is most likely to misread: the
// row says removed while the CR — the only thing that enforces — is still
// there. The event must not read as a completed lifting.
func TestAPendingUnbanRecordsThatTheTargetIsStillBanned(t *testing.T) {
	h := newHarness(t)

	var ban wireBan
	h.decode(http.MethodPost, "/api/v1/bans", map[string]any{
		"target": map[string]any{"type": "broadcastId", "value": "ABC234"}, "expiresAt": nil, "reason": "keep",
	}, http.StatusCreated, &ban)
	h.proj.breakFrom(errors.New("kubernetes API is unreachable"))
	h.decode(http.MethodDelete, "/api/v1/bans/"+ban.ID, nil, http.StatusAccepted, nil)

	events := h.enq.all()
	if len(events) != 2 || events[1].Type != store.EventBanRemoved {
		t.Fatalf("events = %+v", events)
	}
	// The clean create that preceded it is untouched: only the mutation whose
	// CR write failed carries a grade.
	if got := events[0].EnforcementState(); got != store.EnforcementInSync {
		t.Errorf("the clean create was graded %q", got)
	}
	removed := events[1]
	if got := removed.EnforcementState(); got != store.EnforcementPending {
		t.Errorf("the unban event enforcement = %q, want %q", got, store.EnforcementPending)
	}
	summary := removed.PayloadString(store.PayloadSummary)
	if !strings.Contains(summary, "STILL banned") {
		t.Errorf("the unban summary %q must say the target is still banned — %q reads as backwards", summary, "not enforced yet")
	}
	if strings.Contains(summary, "was lifted by") {
		t.Errorf("the unban summary %q reads as a completed lifting", summary)
	}
}

// The other direction: a mutation whose CR landed records nothing extra. The
// event's payload — and so the webhook body built from it — stays exactly what
// it has always been, which is what makes the new key additive.
func TestAnInSyncMutationRecordsNoEnforcementState(t *testing.T) {
	h := newHarness(t)
	h.fleet.set(liveSnapshot("ABC234", "3f9a1c2b4d5e", "203.0.113.7"))

	h.decode(http.MethodPost, "/api/v1/broadcasts/ABC234/kill",
		map[string]any{"reason": "spam"}, http.StatusCreated, nil)
	var ban wireBan
	h.decode(http.MethodPost, "/api/v1/bans", map[string]any{
		"target": map[string]any{"type": "broadcastId", "value": "BBB234"}, "expiresAt": nil, "reason": "x",
	}, http.StatusCreated, &ban)
	h.decode(http.MethodDelete, "/api/v1/bans/"+ban.ID, nil, http.StatusNoContent, nil)

	events := h.enq.all()
	if len(events) != 3 {
		t.Fatalf("events = %+v, want kill, create, remove", events)
	}
	wantSays := []string{"was terminated", "was created", "was lifted by"}
	for i, ev := range events {
		if got := ev.EnforcementState(); got != store.EnforcementInSync {
			t.Errorf("event %d (%s) was graded %q despite a CR that landed", i, ev.Type, got)
		}
		// Not merely "reads as in sync": the key is ABSENT, because absence is
		// the signal a receiver has always parsed.
		if strings.Contains(string(ev.Payload), store.PayloadEnforcement) {
			t.Errorf("event %d (%s) grew an enforcement key: %s", i, ev.Type, ev.Payload)
		}
		if summary := ev.PayloadString(store.PayloadSummary); !strings.Contains(summary, wantSays[i]) {
			t.Errorf("event %d (%s) summary = %q, want the plain wording %q", i, ev.Type, summary, wantSays[i])
		}
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

// The create path's 202: bare ban (not the kill's envelope), carrying the
// enforcement object that says the record is ahead of the CR.
func TestCreateBanProjectionFailureIsAccepted(t *testing.T) {
	h := newHarness(t, withProjectorError(errors.New("kubernetes API is unreachable")))

	var ban wireBan
	h.decode(http.MethodPost, "/api/v1/bans", map[string]any{
		"target":    map[string]any{"type": "ip", "value": "203.0.113.7", "prefixLength": 32},
		"expiresAt": nil, "reason": "repeat offender",
	}, http.StatusAccepted, &ban)

	if ban.ID == "" || ban.Target.Value != "203.0.113.7/32" {
		t.Fatalf("the 202 must carry the recorded ban: %+v", ban)
	}
	if ban.Enforcement == nil || ban.Enforcement.InSync || ban.Enforcement.Detail != api.DetailBanPending {
		t.Fatalf("enforcement = %+v", ban.Enforcement)
	}
	// The detail is the operator's instruction, and "do not re-submit" is the
	// load-bearing half: a retry now 409s against the row that exists.
	if !strings.Contains(ban.Enforcement.Detail, "NOT enforced yet") ||
		!strings.Contains(ban.Enforcement.Detail, "do not re-submit") {
		t.Fatalf("the create detail must say what happened and what not to do: %q", ban.Enforcement.Detail)
	}
	bans, err := h.store.ListBans(t.Context(), store.FilterActive)
	if err != nil || len(bans) != 1 {
		t.Fatalf("the ban row must be committed regardless: %d rows (err=%v)", len(bans), err)
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

// The unban half of 202, and the direction an operator is most likely to
// misread: the row says `removed` while the CR — the only thing that actually
// enforces — is still there, so the target is STILL banned.
//
// The clean unban answers 204 with no body; this one answers 202 WITH the
// removed ban, because there is something to say and something to say it about.
func TestUnbanProjectionFailureIsAccepted(t *testing.T) {
	h := newHarness(t)

	// Created cleanly: the CR exists, so only its DELETE is lost.
	var ban wireBan
	h.decode(http.MethodPost, "/api/v1/bans", map[string]any{
		"target": map[string]any{"type": "broadcastId", "value": "ABC234"}, "expiresAt": nil, "reason": "keep",
	}, http.StatusCreated, &ban)
	if h.proj.count() != 1 {
		t.Fatalf("setup: the create must have projected a CR (%d)", h.proj.count())
	}
	h.proj.breakFrom(errors.New("kubernetes API is unreachable"))

	var out wireBan
	h.decode(http.MethodDelete, "/api/v1/bans/"+ban.ID, nil, http.StatusAccepted, &out)
	if out.ID != ban.ID || out.State != string(store.BanRemoved) {
		t.Fatalf("the 202 must carry the removed ban: %+v", out)
	}
	if out.Enforcement == nil || out.Enforcement.InSync || out.Enforcement.Detail != api.DetailUnbanPending {
		t.Fatalf("enforcement = %+v", out.Enforcement)
	}
	// The copy has to say the target is still banned. "Unbanned, but not
	// enforced" would read as "not banned yet", which is backwards.
	if !strings.Contains(out.Enforcement.Detail, "STILL banned") {
		t.Fatalf("the unban detail must say the target is still banned: %q", out.Enforcement.Detail)
	}

	// The row moved regardless, and the removal event was recorded.
	all, err := h.store.ListBans(t.Context(), store.FilterAll)
	if err != nil || len(all) != 1 || all[0].State != store.BanRemoved {
		t.Fatalf("bans = %+v (err=%v)", all, err)
	}
	events := h.enq.all()
	if len(events) != 2 || events[1].Type != store.EventBanRemoved {
		t.Fatalf("events = %+v", events)
	}
}

// --- GET /events ------------------------------------------------------

func TestEventsCursorPaginationAndDeliveryVisibility(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()

	var ids []int64
	for i := range 5 {
		seed := store.Event{
			Type: store.EventBanCreated, Actor: "op@example.com",
			BroadcastKey: "3f9a1c2b4d5e", BroadcastID: "ABC234",
			OccurredAt: time.Now().Add(time.Duration(i) * time.Second),
			Payload:    []byte(`{"reason":"spam","summary":"a broadcast ban was created by op@example.com"}`),
		}
		// The newest event gets a delivery — enqueued the only way delivery
		// rows exist, in the event's own transaction — because a failed
		// delivery MUST be visible (§4.10).
		var (
			ev  store.Event
			err error
		)
		if i == 4 {
			ev, err = h.store.AppendEventAndEnqueue(ctx, seed, []string{"ntfy"})
		} else {
			ev, err = h.store.AppendEvent(ctx, seed)
		}
		if err != nil {
			t.Fatalf("append event %d: %v", i, err)
		}
		ids = append(ids, ev.ID)
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

// A kill whose cooldown has lapsed must be re-killable in the time of one API
// call, with NO janitor sweep in between.
//
// Relays evaluate expiresAt against their own clocks (§4.2), so the moment the
// cooldown passes the broadcaster reclaims and the broadcast is live and
// unenforced. Answering 409 duplicate_active there tells the operator "already
// banned" about a broadcast nothing is banning — for up to a minute in the
// happy case, and for as long as the abuse lasts whenever no replica currently
// holds the leader Lease.
func TestReKillAfterTheCooldownLapsesIsNotADuplicate(t *testing.T) {
	clock := &testClock{t: time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)}
	h := newHarness(t, withClock(clock))
	h.fleet.set(liveSnapshot("ABC234", "3f9a1c2b4d5e", "203.0.113.7"))

	kill := func(reason string, wantStatus int) wireBan {
		t.Helper()
		var out struct {
			Ban wireBan `json:"ban"`
		}
		h.decode(http.MethodPost, "/api/v1/broadcasts/ABC234/kill",
			map[string]any{"reason": reason, "cooldownSeconds": 600}, wantStatus, &out)
		return out.Ban
	}

	first := kill("terms violation", http.StatusCreated)

	// While the cooldown is live, a second kill still conflicts — the point is
	// the expiry, not the removal of the guard.
	clock.advance(5 * time.Minute)
	if code := h.errorCode(http.MethodPost, "/api/v1/broadcasts/ABC234/kill",
		map[string]any{"reason": "again"}, http.StatusConflict); code != api.CodeDuplicateActive {
		t.Fatalf("a kill inside the cooldown = %q, want %q", code, api.CodeDuplicateActive)
	}

	// Past the cooldown, with no sweep having run.
	clock.advance(6 * time.Minute)
	second := kill("still at it", http.StatusCreated)
	if second.ID == first.ID {
		t.Fatalf("the re-kill returned the lapsed ban %s instead of a new one", first.ID)
	}
	if second.State != string(store.BanActive) {
		t.Fatalf("re-kill ban = %+v", second)
	}

	// The lapsed row was expired inline rather than left squatting the partial
	// unique index, and the audit trail says it lapsed.
	var bans struct {
		Bans []wireBan `json:"bans"`
	}
	h.decode(http.MethodGet, "/api/v1/bans?state=all", nil, http.StatusOK, &bans)
	for _, b := range bans.Bans {
		if b.ID == first.ID && b.State != string(store.BanExpired) {
			t.Fatalf("the lapsed ban is still %q", b.State)
		}
	}
	var expired int
	for _, ev := range h.enq.all() {
		if ev.Type == store.EventBanExpired {
			expired++
		}
	}
	if expired != 1 {
		t.Fatalf("ban.expired emitted %d times for one lapsed cooldown; events: %+v", expired, h.enq.all())
	}
}

// Post-commit bookkeeping must survive the client hanging up.
//
// The row is the commitment point: once it is written the enforcement has
// happened. If the operator's browser aborts in the window after that, running
// the CR projection and the event write on the REQUEST context loses both —
// and losing the event loses every webhook page with it, permanently, because
// deliveries are only ever enqueued from the event row. There is no retry: the
// "a page must reach a human" pipe would drop the action silently.
func TestAClientAbortAfterTheRowCommitsStillProjectsRecordsAndPages(t *testing.T) {
	h := newHarness(t)
	h.fleet.set(liveSnapshot("ABC234", "3f9a1c2b4d5e", "203.0.113.7"))

	// Suspend the handler at the fleet lookup — the last thing a kill does on
	// the request context, and after CreateBan has committed — and hold it
	// until the abort has actually been observed server-side.
	reached := make(chan struct{})
	h.fleet.setHook(func(ctx context.Context) {
		close(reached)
		<-ctx.Done()
	})

	ctx, cancel := context.WithCancel(context.Background())
	body, err := json.Marshal(map[string]any{"reason": "terms violation"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		h.srv.URL+"/api/v1/broadcasts/ABC234/kill", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	done := make(chan struct{})
	go func() {
		defer close(done)
		if resp, err := h.srv.Client().Do(req); err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-reached
	cancel()
	<-done

	// The handler runs on past the abort. Poll, because it is now racing this
	// goroutine rather than answering it.
	waitForEvent(t, h, store.EventBroadcastKilled)

	for _, ctxErr := range h.proj.contexts() {
		if ctxErr != nil {
			t.Fatalf("the Ban CR was projected on an already-cancelled context (%v): "+
				"an aborted request leaves the row committed and nothing enforcing it", ctxErr)
		}
	}
	if n := h.proj.count(); n != 1 {
		t.Fatalf("projections = %d, want 1", n)
	}

	// The webhook fan-out is the half with no retry path at all.
	evs := h.enq.all()
	if len(evs) != 1 || evs[0].Type != store.EventBroadcastKilled {
		t.Fatalf("enqueued = %+v, want one broadcast.killed", evs)
	}
	if evs[0].ID == 0 {
		t.Fatalf("the dispatcher was offered an unsaved event: %+v", evs[0])
	}
}

// waitForEvent polls the audit trail for an event of the given type. It exists
// for the tests whose handler outlives the request that started it.
func waitForEvent(t *testing.T, h *harness, typ string) store.Event {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		events, err := h.store.ListEvents(context.Background(), 0, 50)
		if err == nil {
			for _, e := range events {
				if e.Type == typ {
					return e
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no %s event was ever written: the enforcement happened and nothing recorded or paged it", typ)
	return store.Event{}
}

// The "is there another page?" test must compare against the limit the STORE
// applied, not the one the caller asked for.
//
// ListEvents clamps to store.MaxEventLimit, so `?limit=1000` over a longer
// feed comes back with 500 rows and `len(events) == limit` is 500 == 1000:
// false, no cursor, and the operator exporting an audit trail concludes it
// ended at 500 events. The remainder is unreachable with that request shape —
// silently, which on an audit surface is the worst way to lose data.
func TestEventsFeedPagesPastTheStoreClamp(t *testing.T) {
	h := newHarness(t)

	const extra = 5
	if _, err := h.store.Pool().Exec(t.Context(),
		`INSERT INTO moderation_events (type, occurred_at, actor, payload)
		 SELECT 'ban.created', now(), 'op@example.com', '{}'::jsonb FROM generate_series(1, $1)`,
		store.MaxEventLimit+extra); err != nil {
		t.Fatalf("seed the feed: %v", err)
	}

	var page struct {
		Events      []wireEvent `json:"events"`
		NextAfterID *int64      `json:"nextAfterId"`
	}
	h.decode(http.MethodGet, "/api/v1/events?limit=1000", nil, http.StatusOK, &page)

	if len(page.Events) != store.MaxEventLimit {
		t.Fatalf("page carried %d events, want the store's clamp of %d", len(page.Events), store.MaxEventLimit)
	}
	if page.NextAfterID == nil {
		t.Fatalf("a page truncated to %d of %d events reported no more pages: the rest of the "+
			"audit trail is unreachable with that request shape", len(page.Events), store.MaxEventLimit+extra)
	}
	if *page.NextAfterID != page.Events[len(page.Events)-1].ID {
		t.Fatalf("nextAfterId = %d, want the last event on the page (%d)",
			*page.NextAfterID, page.Events[len(page.Events)-1].ID)
	}

	var rest struct {
		Events      []wireEvent `json:"events"`
		NextAfterID *int64      `json:"nextAfterId"`
	}
	h.decode(http.MethodGet, "/api/v1/events?limit=1000&afterId="+strconv.FormatInt(*page.NextAfterID, 10),
		nil, http.StatusOK, &rest)
	if len(rest.Events) != extra {
		t.Fatalf("the remainder page carried %d events, want %d", len(rest.Events), extra)
	}
	if rest.NextAfterID != nil {
		t.Fatalf("a short page must end the feed, got cursor %d", *rest.NextAfterID)
	}
}

// A double-clicked (or replayed) unban must page every webhook receiver once.
//
// RemoveBan is deliberately idempotent — the already-removed branch returns
// the row unchanged — but recording unconditionally on top of that writes a
// SECOND ban.removed row and sends a second signed delivery to every enabled
// webhook, each with its own delivery ID because the event ID differs. Receiver
// -side dedup on X-Gawk-Delivery cannot catch that, so the on-call phone buzzes
// twice and the audit trail shows one ban lifted twice, possibly by two actors.
func TestARepeatedUnbanRecordsAndPagesOnlyOnce(t *testing.T) {
	h := newHarness(t)

	var ban wireBan
	h.decode(http.MethodPost, "/api/v1/bans", map[string]any{
		"target": map[string]any{"type": "broadcastId", "value": "ABC234"}, "expiresAt": nil, "reason": "spam",
	}, http.StatusCreated, &ban)

	h.decode(http.MethodDelete, "/api/v1/bans/"+ban.ID, nil, http.StatusNoContent, nil)
	// The second call is still a success — DELETE is idempotent — but it must
	// not restate anything.
	h.decode(http.MethodDelete, "/api/v1/bans/"+ban.ID, nil, http.StatusNoContent, nil)

	var removals int
	for _, ev := range h.enq.all() {
		if ev.Type == store.EventBanRemoved {
			removals++
		}
	}
	if removals != 1 {
		t.Fatalf("a repeated unban paged every webhook receiver %d times", removals)
	}

	events, err := h.store.ListEvents(t.Context(), 0, 50)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	removals = 0
	for _, ev := range events {
		if ev.Type == store.EventBanRemoved {
			removals++
		}
	}
	if removals != 1 {
		t.Fatalf("the audit trail records the same ban lifted %d times", removals)
	}

	// A different actor replaying it must not be able to rewrite attribution
	// either: the row still names whoever actually lifted it.
	var all struct {
		Bans []wireBan `json:"bans"`
	}
	h.decode(http.MethodGet, "/api/v1/bans?state=all", nil, http.StatusOK, &all)
	for _, b := range all.Bans {
		if b.ID == ban.ID && b.RemovedBy != "op@example.com" {
			t.Fatalf("removedBy = %q after a replayed unban", b.RemovedBy)
		}
	}
}
