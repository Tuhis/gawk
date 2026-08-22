package api_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Tuhis/gawk/gawk-admin/internal/api"
	"github.com/Tuhis/gawk/gawk-admin/internal/store"
)

// POST /api/v1/content-flags is R40's (docs/42 §4.11, D11). In R39 it must
// 404: the path is frozen in the design so R40 integrates against a known
// contract, and this test is what keeps anything else from squatting it in the
// meantime.
func TestContentFlagsRouteIsReservedAnd404s(t *testing.T) {
	h := newHarnessWithoutPostgres(t)

	for _, method := range []string{http.MethodPost, http.MethodGet, http.MethodPut} {
		code := h.errorCode(method, "/api/v1/content-flags", map[string]any{
			"schema": "gawk.content-flag.v1", "broadcastId": "ABC234",
		}, http.StatusNotFound)
		if code != api.CodeNotFound {
			t.Fatalf("%s /api/v1/content-flags error code = %q, want %q", method, code, api.CodeNotFound)
		}
	}
}

// An unknown /api/v1 path answers the documented error envelope, not
// net/http's plain text — the SPA's client parses {"error":{"code","message"}}
// on every failure.
func TestUnknownRouteUsesTheErrorEnvelope(t *testing.T) {
	h := newHarnessWithoutPostgres(t)
	status, body := h.raw(http.MethodGet, "/api/v1/nonesuch", nil)
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", status)
	}
	if !strings.Contains(body, `"error"`) || !strings.Contains(body, `"code"`) {
		t.Fatalf("body = %s, want the error envelope", body)
	}
}

// /readyz is false when Postgres cannot be reached, and says so once.
func TestReadyzIsFalseWithoutPostgres(t *testing.T) {
	h := newHarnessWithoutPostgres(t)

	var body struct {
		Ready  bool `json:"ready"`
		Checks []struct {
			Name  string `json:"name"`
			Error string `json:"error"`
		} `json:"checks"`
	}
	h.decode(http.MethodGet, "/readyz", nil, http.StatusServiceUnavailable, &body)
	if body.Ready {
		t.Fatalf("readyz reported ready with no database")
	}
	if len(body.Checks) == 0 || body.Checks[0].Name != "postgres" || body.Checks[0].Error == "" {
		t.Fatalf("checks = %+v", body.Checks)
	}
	if !strings.Contains(h.logText(), "refusing to serve") {
		t.Fatalf("no refusing-to-serve log line; got:\n%s", h.logText())
	}

	// Liveness is deliberately independent: a database outage must not get
	// every replica restarted.
	if status, _ := h.raw(http.MethodGet, "/healthz", nil); status != http.StatusOK {
		t.Fatalf("healthz = %d with the database down, want 200", status)
	}
}

// The serving process refuses to serve on a schema older than its minimum, and
// says exactly why — it never applies DDL to fix it (docs/42 §4.15/D18).
func TestReadyzRefusesTooOldSchemaAndNeverMigrates(t *testing.T) {
	h := newHarness(t)

	h.decode(http.MethodGet, "/readyz", nil, http.StatusOK, nil)

	before, _, err := h.store.SchemaVersion(t.Context())
	if err != nil {
		t.Fatalf("schema version: %v", err)
	}
	// Stand in for a future build whose minimum has moved past this database.
	h.store.MinVersion = before + 5

	var body struct {
		Ready  bool `json:"ready"`
		Checks []struct {
			Name  string `json:"name"`
			Error string `json:"error"`
		} `json:"checks"`
	}
	h.decode(http.MethodGet, "/readyz", nil, http.StatusServiceUnavailable, &body)
	if body.Ready {
		t.Fatalf("readyz reported ready on a too-old schema")
	}
	if !strings.Contains(body.Checks[0].Error, "migrate") {
		t.Fatalf("the readyz body does not name the fix: %q", body.Checks[0].Error)
	}
	logs := h.logText()
	if !strings.Contains(logs, "refusing to serve") || !strings.Contains(logs, "schema") {
		t.Fatalf("no clear schema refusal line; got:\n%s", logs)
	}

	after, _, err := h.store.SchemaVersion(t.Context())
	if err != nil {
		t.Fatalf("schema version after refusal: %v", err)
	}
	if after != before {
		t.Fatalf("the serving path moved the schema version %d -> %d; it must never run DDL", before, after)
	}
}

// GET /me is the SPA's authorization probe and the only authenticated place
// the kill dialog's default can come from (/auth/config is pinned to
// {issuer, clientId, audience}).
func TestMeCarriesIdentityAndKillCooldownDefault(t *testing.T) {
	h := newHarnessWithoutPostgres(t)

	var me struct {
		Email    string   `json:"email"`
		Subject  string   `json:"subject"`
		Roles    []string `json:"roles"`
		Defaults struct {
			KillCooldownSeconds int `json:"killCooldownSeconds"`
		} `json:"defaults"`
	}
	h.decode(http.MethodGet, "/api/v1/me", nil, http.StatusOK, &me)
	if me.Email != "op@example.com" || me.Subject != "sub-1" {
		t.Fatalf("me = %+v", me)
	}
	if len(me.Roles) != 1 || me.Roles[0] != "operator" {
		t.Fatalf("roles = %v", me.Roles)
	}
	if me.Defaults.KillCooldownSeconds != 600 {
		t.Fatalf("defaults.killCooldownSeconds = %d, want 600 (the configured 10m)", me.Defaults.KillCooldownSeconds)
	}
}

// The injected role check gates every route. api never decides authorization
// itself — but it must actually apply what auth hands it.
func TestRoutesAreBehindTheInjectedRoleCheck(t *testing.T) {
	h := newHarnessWithoutPostgres(t)
	h.identity.Roles = []string{"someone-else"}

	for _, path := range []string{"/api/v1/me", "/api/v1/bans", "/api/v1/broadcasts", "/api/v1/webhooks", "/api/v1/events", "/api/v1/relays"} {
		if status, _ := h.raw(http.MethodGet, path, nil); status != http.StatusForbidden {
			t.Fatalf("GET %s without the operator role = %d, want 403", path, status)
		}
	}
}

// Postgres down: mutations answer 503 (§6), not 500 — the operator should
// retry, and enforcement of existing bans is unaffected either way.
//
// This is the bottom row of the status matrix, and 202 exists precisely so it
// stays distinguishable from it: NOTHING was recorded here, so nothing may be
// projected, nothing may be emitted, and no ban may come back. A 202 says "the
// record is durable"; a client that got one of these instead would be told the
// opposite of the truth.
func TestMutationsReport503WhenPostgresIsDown(t *testing.T) {
	h := newHarnessWithoutPostgres(t)

	cases := []struct {
		name, method, path string
		body               any
	}{
		{"kill", http.MethodPost, "/api/v1/broadcasts/ABC234/kill", map[string]any{"reason": "terms violation"}},
		{"create ban", http.MethodPost, "/api/v1/bans", map[string]any{
			"target": map[string]any{"type": "broadcastId", "value": "ABC234"}, "expiresAt": nil, "reason": "x"}},
		{"unban", http.MethodDelete, "/api/v1/bans/f2a6b2f4-0d5f-4a2f-9a1a-4a3b7c8d9e01", nil},
	}
	for _, c := range cases {
		status, body := h.raw(c.method, c.path, c.body)
		if status != http.StatusServiceUnavailable {
			t.Fatalf("%s = %d, want 503; body: %s", c.name, status, body)
		}
		if !strings.Contains(body, api.CodeUnavailable) {
			t.Fatalf("%s body = %s, want code %q", c.name, body, api.CodeUnavailable)
		}
		if strings.Contains(body, `"ban"`) || strings.Contains(body, `"id"`) {
			t.Fatalf("%s returned a ban on a write that never happened: %s", c.name, body)
		}
	}
	assertNothingHappened(t, h)
}

// The other 5xx: Postgres is reachable, the row write itself fails. 500, and
// the same all-or-nothing guarantee — a CHECK the INSERT cannot satisfy leaves
// reads working, so the handler gets past its duplicate lookup and fails
// exactly where a real constraint violation or a full disk would.
func TestARowWriteFailureIs500AndRecordsNothing(t *testing.T) {
	h := newHarness(t)
	h.fleet.set(liveSnapshot("ABC234", "key", "203.0.113.7"))

	if _, err := h.store.Pool().Exec(t.Context(),
		`ALTER TABLE bans ADD CONSTRAINT refuse_every_insert CHECK (false) NOT VALID`); err != nil {
		t.Fatalf("block inserts: %v", err)
	}

	for _, c := range []struct {
		name, path string
		body       any
	}{
		{"kill", "/api/v1/broadcasts/ABC234/kill", map[string]any{"reason": "terms violation"}},
		{"create ban", "/api/v1/bans", map[string]any{
			"target": map[string]any{"type": "broadcastId", "value": "BBB234"}, "expiresAt": nil, "reason": "x"}},
	} {
		status, body := h.raw(http.MethodPost, c.path, c.body)
		if status != http.StatusInternalServerError {
			t.Fatalf("%s = %d, want 500; body: %s", c.name, status, body)
		}
		if !strings.Contains(body, api.CodeInternal) {
			t.Fatalf("%s body = %s, want code %q", c.name, body, api.CodeInternal)
		}
		if strings.Contains(body, `"ban"`) || strings.Contains(body, `"id"`) {
			t.Fatalf("%s returned a ban on a write that never happened: %s", c.name, body)
		}
	}
	assertNothingHappened(t, h)
}

// assertNothingHappened is the guarantee both 5xx tests exist for: a failed
// row write projects no CR and emits no event. It is structural — every
// handler returns from a.fail before it reaches either — and this is what
// keeps it structural.
func assertNothingHappened(t *testing.T, h *harness) {
	t.Helper()
	if n := h.proj.count(); n != 0 {
		t.Fatalf("%d CR(s) projected for a mutation whose row was never written", n)
	}
	if evs := h.enq.all(); len(evs) != 0 {
		t.Fatalf("events emitted for a mutation whose row was never written: %+v", evs)
	}
}

// Sanity: the store's event-type vocabulary is what the API writes, so the
// portal feed and R40 read the same names.
func TestEventTypeVocabulary(t *testing.T) {
	for _, want := range []string{"broadcast.killed", "ban.created", "ban.expired", "ban.removed", "content_flag.raised"} {
		found := false
		for _, got := range []string{store.EventBroadcastKilled, store.EventBanCreated, store.EventBanExpired, store.EventBanRemoved, store.EventContentFlag} {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("event type %q is not in the store's vocabulary", want)
		}
	}
}
