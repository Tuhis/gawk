package main

// R53 TO2/TO4 (docs/55 §8): the read listener's gate, driven through the
// production wiring (newReadHandler) against oidcauthtest's fake issuer.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/oidcauth"
	"github.com/Tuhis/gawk/gawk-server/oidcauth/oidcauthtest"
	"github.com/Tuhis/gawk/gawk-server/oidcroles"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/dashboard"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/ingest"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/live"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/mcp"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/readapi"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/store"
)

const (
	tmAudience = "gawk-telemetry"
	tmClientID = "gawk-telemetry"
	tmSession  = "3c3c3c3c3c3c3c3c3c3c3c3c"
)

var (
	tmKey = sync.OnceValue(oidcauthtest.NewRSAKey)
	quiet = slog.New(slog.DiscardHandler)
)

// readFixture is the read listener's content: a store with one finalized
// session, every clock pinned so two handlers over it answer byte-for-byte.
type readFixture struct {
	routes readRoutes
}

func newReadFixture(t *testing.T) readFixture {
	t.Helper()
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	st, err := store.New(store.Options{Root: t.TempDir(), Now: clock})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	projection := live.New(clock)
	api, err := readapi.New(readapi.Options{Store: st, Live: projection, Now: clock, DashboardBase: "http://dash"})
	if err != nil {
		t.Fatal(err)
	}
	w := newWriter(st, quiet, config{sessionIdle: time.Minute, scrapeInterval: 5 * time.Second}, api, projection)
	samples := make([]ingest.Sample, 0, 10)
	for i := range 10 {
		samples = append(samples, ingest.Sample{TMs: float64(i) * 2000, Stats: map[string]any{
			"receivedFps": 60.0, "decoderFps": 60.0, "timeSinceLastFrameMs": 16.0,
		}})
	}
	if err := w.Accept(ingest.Accepted{
		SessionID: tmSession, BroadcastKey: "1a2b3c4d5e6f", Role: "viewer", Final: true,
		App:         ingest.AppInfo{Version: "0.33.2", Surface: "viewer", Browser: "Chrome 152", OS: "Windows"},
		StartedAtMs: now.UnixMilli(), ReceivedAt: now, Samples: samples,
	}); err != nil {
		t.Fatal(err)
	}
	dash, err := dashboard.Handler()
	if err != nil {
		t.Fatal(err)
	}
	m, err := mcp.New(mcp.Options{API: api, Now: clock})
	if err != nil {
		t.Fatal(err)
	}
	return readFixture{routes: readRoutes{dashboard: dash, api: api.Handler(), mcp: m}}
}

// preR53ReadHandler is the read listener exactly as run() built it before
// R53 — the golden G8 compares against.
func preR53ReadHandler(cfg config, routes readRoutes) http.Handler {
	readMux := http.NewServeMux()
	readMux.Handle("/", routes.dashboard)
	readMux.Handle("/v1/", routes.api)
	readMux.Handle("GET /live", routes.api)
	readMux.Handle("GET /live/", routes.api)
	readMux.Handle("/mcp", routes.mcp)
	var h http.Handler = readMux
	if cfg.basicAuthUser != "" {
		h = basicAuth(readMux, cfg.basicAuthUser, cfg.basicAuthPass)
	}
	return h
}

func do(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// securityHeaderNames are what D5 adds in every mode — the one intended
// difference from the pre-R53 responses besides /auth/config and /readyz.
var securityHeaderNames = []string{"Content-Security-Policy", "X-Content-Type-Options", "Referrer-Policy"}

func assertNoCookie(t *testing.T, what string, h http.Header) {
	t.Helper()
	if v := h.Values("Set-Cookie"); len(v) > 0 {
		t.Errorf("%s: Set-Cookie %q — no response of the read listener may set a cookie (docs/55 §7)", what, v)
	}
}

// G8 / G6: with no OIDC knob set, none and basic mode answer every request
// exactly as before — status, body and headers — except that the security
// headers are stamped (D5), /auth/config answers 404 and /readyz 200.
func TestReadListenerWithoutOIDCIsUnchanged(t *testing.T) {
	f := newReadFixture(t)
	initialize := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`
	diagnose := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"diagnose","arguments":{"sessionId":"` + tmSession + `"}}}`
	type req struct {
		method, path, body string
	}
	requests := []req{
		{"GET", "/", ""},
		{"GET", "/index.html", ""},
		{"GET", "/some/deep/link", ""},
		{"GET", "/v1/fleet", ""},
		{"GET", "/v1/sessions", ""},
		{"GET", "/v1/sessions/" + tmSession + "/diagnose", ""},
		{"GET", "/v1/me", ""},
		{"GET", "/v1/nope", ""},
		{"GET", "/live", ""},
		{"POST", "/live", ""},
		{"GET", "/livex", ""},
		{"GET", "/mcp", ""},
		{"POST", "/mcp", initialize},
		{"POST", "/mcp", diagnose},
		{"GET", "/.well-known/oauth-protected-resource/mcp", ""},
		{"GET", "/.well-known/oauth-protected-resource", ""},
		{"GET", "/idp/x", ""},
		{"POST", "/readyz", ""},
	}
	for _, mode := range []struct {
		name string
		cfg  config
		auth func(*http.Request)
	}{
		{"none", config{}, func(*http.Request) {}},
		{"basic, no credentials", config{basicAuthUser: "ops", basicAuthPass: "pw"}, func(*http.Request) {}},
		{"basic, wrong credentials", config{basicAuthUser: "ops", basicAuthPass: "pw"}, func(r *http.Request) { r.SetBasicAuth("ops", "nope") }},
		{"basic, credentials", config{basicAuthUser: "ops", basicAuthPass: "pw"}, func(r *http.Request) { r.SetBasicAuth("ops", "pw") }},
	} {
		t.Run(mode.name, func(t *testing.T) {
			before := preR53ReadHandler(mode.cfg, f.routes)
			after := newReadHandler(mode.cfg, f.routes, nil)
			for _, rq := range requests {
				mk := func() *http.Request {
					r := httptest.NewRequest(rq.method, rq.path, strings.NewReader(rq.body))
					mode.auth(r)
					return r
				}
				want, got := do(before, mk()), do(after, mk())
				what := rq.method + " " + rq.path
				if got.Code != want.Code {
					t.Errorf("%s: status %d, was %d", what, got.Code, want.Code)
				}
				if !bytes.Equal(got.Body.Bytes(), want.Body.Bytes()) {
					t.Errorf("%s: body changed\nwas: %.200s\nnow: %.200s", what, want.Body, got.Body)
				}
				gh, wh := got.Header().Clone(), want.Header().Clone()
				for _, k := range securityHeaderNames {
					if gh.Get(k) == "" {
						t.Errorf("%s: no %s; the security headers wrap the read mux in every mode (D5)", what, k)
					}
					// http.Error already set nosniff on some answers; the
					// comparison is over everything else.
					gh.Del(k)
					wh.Del(k)
				}
				if fmt.Sprint(gh) != fmt.Sprint(wh) {
					t.Errorf("%s: headers changed\nwas: %v\nnow: %v", what, wh, gh)
				}
				assertNoCookie(t, what, got.Header())
			}

			// The two answers R53 adds — outside the basic gate, so the
			// SPA's bootstrap and an operator's curl get them unauthenticated.
			for _, r := range []*http.Request{
				httptest.NewRequest("GET", "/auth/config", nil),
				httptest.NewRequest("POST", "/auth/config", nil),
			} {
				if rec := do(after, r); rec.Code != http.StatusNotFound {
					t.Errorf("%s /auth/config = %d, want 404 (the SPA reads it as not-OIDC)", r.Method, rec.Code)
				}
			}
			rec := do(after, httptest.NewRequest("GET", "/readyz", nil))
			if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"idp":"none"}` {
				t.Errorf("/readyz = %d %s, want 200 {\"idp\":\"none\"}", rec.Code, rec.Body)
			}
		})
	}
}

// oidcHarness is the OIDC mode against a live fake issuer.
type oidcHarness struct {
	iss     *oidcauthtest.Issuer
	v       *oidcauth.Verifier
	handler http.Handler
	cfg     config
}

func oidcConfig(issuer string) config {
	return config{
		oidcIssuer: issuer, oidcClientID: tmClientID, oidcAudience: tmAudience,
		oidcRolesClaim: oidcroles.DefaultClaim, oidcRole: DefaultOIDCRole,
	}
}

func newVerifierFor(t *testing.T, cfg config) *oidcauth.Verifier {
	t.Helper()
	return newVerifierLogging(t, cfg, quiet)
}

func newVerifierLogging(t *testing.T, cfg config, log *slog.Logger) *oidcauth.Verifier {
	t.Helper()
	v, err := oidcauth.New(context.Background(), oidcauth.Config{
		Issuer: cfg.oidcIssuer, Audience: cfg.oidcAudience, RolesClaim: cfg.oidcRolesClaim,
		Role: cfg.oidcRole, ClientID: cfg.oidcClientID,
	}, oidcauth.Options{
		Logger: log, ResolveRetryInterval: 5 * time.Millisecond,
		// The suite sends many deliberately bad tokens from one address;
		// the failure budget is oidcauth's to test, not this suite's.
		FailureBurst: 10_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = v.Close() })
	return v
}

func newOIDCHarness(t *testing.T, f readFixture) *oidcHarness {
	t.Helper()
	iss := oidcauthtest.NewIssuer(t, "k1", tmKey())
	cfg := oidcConfig(iss.URL())
	v := newVerifierFor(t, cfg)
	select {
	case <-v.Primed():
	case <-time.After(10 * time.Second):
		t.Fatal("verifier never primed")
	}
	return &oidcHarness{iss: iss, v: v, cfg: cfg, handler: newReadHandler(cfg, f.routes, v)}
}

// mint signs a token for this deployment; mutate edits the claims first.
func (h *oidcHarness) mint(t *testing.T, ttl time.Duration, roles []string, mutate func(map[string]any)) string {
	t.Helper()
	claims := map[string]any{
		"iss": h.iss.URL(), "aud": tmAudience, "sub": "op-1", "email": "op@example.test",
		"exp":             time.Now().Add(ttl).Unix(),
		"resource_access": map[string]any{tmAudience: map[string]any{"roles": roles}},
	}
	if mutate != nil {
		mutate(claims)
	}
	kid, key := h.iss.SigningKey()
	return oidcauthtest.SignClaims(t, key, kid, claims)
}

func (h *oidcHarness) good(t *testing.T) string {
	return h.mint(t, time.Hour, []string{DefaultOIDCRole}, nil)
}

func bearer(r *http.Request, tok string) *http.Request {
	if tok != "" {
		r.Header.Set("Authorization", "Bearer "+tok)
	}
	return r
}

func mcpInit() io.Reader {
	return strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct{ Code string } `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return body.Error.Code
}

// G1: every data route needs a valid token carrying the role; the shell and
// its bootstrap stay open.
func TestOIDCModeGatesEveryDataRoute(t *testing.T) {
	f := newReadFixture(t)
	h := newOIDCHarness(t, f)

	gated := []func() *http.Request{
		func() *http.Request { return httptest.NewRequest("GET", "/live", nil) },
		func() *http.Request { return httptest.NewRequest("GET", "/v1/fleet", nil) },
		func() *http.Request { return httptest.NewRequest("GET", "/v1/sessions/"+tmSession+"/diagnose", nil) },
		func() *http.Request { return httptest.NewRequest("POST", "/v1/resolve", strings.NewReader(`{}`)) },
		func() *http.Request { return httptest.NewRequest("POST", "/mcp", mcpInit()) },
	}
	good := h.good(t)
	noRole := h.mint(t, time.Hour, []string{"operator"}, nil)
	bad := map[string]string{
		"missing":  "",
		"garbage":  "not.a.jwt",
		"tampered": oidcauthtest.Tamper(t, good),
		"forged": oidcauthtest.RewriteClaims(t, noRole, func(c map[string]any) {
			c["resource_access"] = map[string]any{tmAudience: map[string]any{"roles": []string{DefaultOIDCRole}}}
		}),
		"expired":   h.mint(t, -time.Minute, []string{DefaultOIDCRole}, nil),
		"wrong iss": h.mint(t, time.Hour, []string{DefaultOIDCRole}, func(c map[string]any) { c["iss"] = "https://elsewhere.example" }),
		"wrong aud": h.mint(t, time.Hour, []string{DefaultOIDCRole}, func(c map[string]any) { c["aud"] = "gawk-admin" }),
		"wrong key": oidcauthtest.SignClaims(t, oidcauthtest.NewRSAKey(), "k1", map[string]any{
			"iss": h.iss.URL(), "aud": tmAudience, "sub": "op-1", "exp": time.Now().Add(time.Hour).Unix(),
		}),
	}
	for _, mk := range gated {
		probe := mk()
		what := probe.Method + " " + probe.URL.Path

		rec := do(h.handler, bearer(mk(), good))
		// /v1/resolve answers 501 here (no stats key): reaching the handler
		// is the point, not what it says.
		if want := map[bool]int{true: http.StatusNotImplemented, false: http.StatusOK}[probe.URL.Path == "/v1/resolve"]; rec.Code != want {
			t.Errorf("%s with a good token = %d %s, want %d", what, rec.Code, rec.Body, want)
		}
		assertNoCookie(t, what, rec.Header())
		if rec.Header().Get("WWW-Authenticate") != "" {
			t.Errorf("%s: a 200 carries a challenge", what)
		}

		for name, tok := range bad {
			rec := do(h.handler, bearer(mk(), tok))
			if rec.Code != http.StatusUnauthorized || errorCode(t, rec) != oidcauth.CodeUnauthorized {
				t.Errorf("%s with a %s token = %d %s, want 401 unauthorized", what, name, rec.Code, rec.Body)
			}
			if !strings.HasPrefix(rec.Header().Get("WWW-Authenticate"), "Bearer") {
				t.Errorf("%s with a %s token: WWW-Authenticate = %q, want a Bearer challenge", what, name, rec.Header().Get("WWW-Authenticate"))
			}
			assertNoCookie(t, what, rec.Header())
		}

		rec = do(h.handler, bearer(mk(), noRole))
		if rec.Code != http.StatusForbidden || errorCode(t, rec) != oidcauth.CodeForbidden {
			t.Errorf("%s with a valid token lacking the role = %d %s, want 403 forbidden", what, rec.Code, rec.Body)
		}
	}

	// Open in the OIDC mode: the shell, its assets, the bootstrap, readiness.
	for _, path := range []string{"/", "/index.html", "/auth/config", "/readyz"} {
		rec := do(h.handler, httptest.NewRequest("GET", path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s unauthenticated = %d, want 200 (open in the OIDC mode)", path, rec.Code)
		}
		for _, k := range securityHeaderNames {
			if rec.Header().Get(k) == "" {
				t.Errorf("GET %s: no %s", path, k)
			}
		}
		assertNoCookie(t, path, rec.Header())
	}
	csp := do(h.handler, httptest.NewRequest("GET", "/", nil)).Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "connect-src 'self' "+h.iss.URL()) {
		t.Errorf("CSP = %q, want connect-src carrying the issuer origin", csp)
	}
}

func TestOIDCModeBootstrapAndIdentity(t *testing.T) {
	f := newReadFixture(t)
	h := newOIDCHarness(t, f)

	rec := do(h.handler, httptest.NewRequest("GET", "/auth/config", nil))
	var cfgBody map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &cfgBody); err != nil {
		t.Fatal(err)
	}
	if want := (map[string]string{"issuer": h.iss.URL(), "clientId": tmClientID, "audience": tmAudience}); fmt.Sprint(cfgBody) != fmt.Sprint(want) {
		t.Errorf("/auth/config = %v, want %v", cfgBody, want)
	}

	// /v1/me needs a valid token but not the role: it is how the SPA tells
	// "signed in without the role" from "not signed in".
	for _, tc := range []struct {
		name  string
		roles []string
		want  string
	}{
		{"with the role", []string{DefaultOIDCRole}, `{"subject":"op-1","email":"op@example.test","roles":["telemetry-reader"]}`},
		{"without the role", nil, `{"subject":"op-1","email":"op@example.test","roles":[]}`},
	} {
		rec := do(h.handler, bearer(httptest.NewRequest("GET", "/v1/me", nil), h.mint(t, time.Hour, tc.roles, nil)))
		if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != tc.want {
			t.Errorf("/v1/me %s = %d %s, want 200 %s", tc.name, rec.Code, rec.Body, tc.want)
		}
	}
	rec = do(h.handler, httptest.NewRequest("GET", "/v1/me", nil))
	if rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") != "Bearer" {
		t.Errorf("/v1/me without a token = %d (WWW-Authenticate %q), want 401 Bearer", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}
}

// An IdP outage after startup: per-request verification is offline, from the
// cached key set (docs/42 §4.5).
func TestOIDCModeVerifiesFromCacheWithTheIssuerDown(t *testing.T) {
	f := newReadFixture(t)
	h := newOIDCHarness(t, f)
	tok := h.good(t)
	h.iss.Close()
	if rec := do(h.handler, bearer(httptest.NewRequest("GET", "/live", nil), tok)); rec.Code != http.StatusOK {
		t.Errorf("GET /live with the issuer down = %d %s, want 200 from the cached keys", rec.Code, rec.Body)
	}
}

// D8: /readyz reports discovery; gated routes answer 401 idp_unavailable
// until it resolves, and the shell stays reachable.
func TestOIDCModeBeforeDiscovery(t *testing.T) {
	f := newReadFixture(t)
	iss := oidcauthtest.NewIssuer(t, "k1", tmKey())
	iss.SetDown(true)
	cfg := oidcConfig(iss.URL())
	v := newVerifierFor(t, cfg)
	handler := newReadHandler(cfg, f.routes, v)

	rec := do(handler, httptest.NewRequest("GET", "/readyz", nil))
	var body readyzBody
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != http.StatusServiceUnavailable || body.IDP != "unresolved" || body.Error == "" {
		t.Errorf("/readyz before discovery = %d %s, want 503 idp unresolved with an error", rec.Code, rec.Body)
	}
	rec = do(handler, httptest.NewRequest("GET", "/live", nil))
	if rec.Code != http.StatusUnauthorized || errorCode(t, rec) != oidcauth.CodeIDPUnavailable {
		t.Errorf("/live before discovery = %d %s, want 401 idp_unavailable", rec.Code, rec.Body)
	}
	if rec := do(handler, httptest.NewRequest("GET", "/", nil)); rec.Code != http.StatusOK {
		t.Errorf("the shell before discovery = %d, want 200", rec.Code)
	}
	if rec := do(handler, httptest.NewRequest("GET", "/auth/config", nil)); rec.Code != http.StatusOK {
		t.Errorf("/auth/config before discovery = %d, want 200 (it is how the SPA reaches the IdP)", rec.Code)
	}

	iss.SetDown(false)
	select {
	case <-v.Resolved():
	case <-time.After(10 * time.Second):
		t.Fatal("discovery never resolved")
	}
	rec = do(handler, httptest.NewRequest("GET", "/readyz", nil))
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"idp":"resolved"}` {
		t.Errorf("/readyz after discovery = %d %s, want 200 resolved", rec.Code, rec.Body)
	}
}

// D4 / G4: a stream opened with a short-lived token ends at its `exp` with a
// final `event: expired`. Real listener, because a stream needs a Flusher.
func TestOIDCModeLiveStreamEndsAtTokenExpiry(t *testing.T) {
	f := newReadFixture(t)
	h := newOIDCHarness(t, f)
	srv := httptest.NewServer(h.handler)
	defer srv.Close()

	// `exp` has whole-second resolution: this token lapses within 1–2 s.
	tok := h.mint(t, 2*time.Second, []string{DefaultOIDCRole}, func(c map[string]any) {
		c["exp"] = time.Now().Add(time.Second).Unix() + 1
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/live/stream", nil)
	start := time.Now()
	resp, err := http.DefaultClient.Do(bearer(req, tok))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q", ct)
	}
	var events []string
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		if ev, ok := strings.CutPrefix(sc.Text(), "event: "); ok {
			events = append(events, ev)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("the stream did not end by itself: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("stream ended after %v; the token lapsed within 2 s", elapsed)
	}
	if len(events) < 2 || events[0] != "snapshot" || events[len(events)-1] != "expired" {
		t.Errorf("events = %v, want a snapshot first and expired last", events)
	}
}

// TO4 (D6): the MCP endpoint answers the spec's challenge and serves its
// RFC 9728 metadata at the path-inserted location, and nowhere else.
func TestOIDCModeMCPAuthorizationDiscovery(t *testing.T) {
	f := newReadFixture(t)
	h := newOIDCHarness(t, f)

	for _, tc := range []struct {
		name, host, proto, want string
	}{
		{"ingress with TLS terminated upstream", "gawk-telemetry.kube.example", "https", "https://gawk-telemetry.kube.example/mcp"},
		{"same host without the header", "gawk-telemetry.kube.example", "", "http://gawk-telemetry.kube.example/mcp"},
		{"port-forward", "localhost:8081", "", "http://localhost:8081/mcp"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mk := func(method, path string, body io.Reader) *http.Request {
				r := httptest.NewRequest(method, path, body)
				r.Host = tc.host
				if tc.proto != "" {
					r.Header.Set("X-Forwarded-Proto", tc.proto)
				}
				return r
			}
			rec := do(h.handler, mk("POST", "/mcp", mcpInit()))
			metaURL := strings.Replace(tc.want, "/mcp", "/.well-known/oauth-protected-resource/mcp", 1)
			if want := `Bearer resource_metadata="` + metaURL + `"`; rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") != want {
				t.Errorf("unauthenticated /mcp = %d, WWW-Authenticate %q; want 401 %q", rec.Code, rec.Header().Get("WWW-Authenticate"), want)
			}

			rec = do(h.handler, mk("GET", "/.well-known/oauth-protected-resource/mcp", nil))
			var doc struct {
				Resource               string   `json:"resource"`
				AuthorizationServers   []string `json:"authorization_servers"`
				BearerMethodsSupported []string `json:"bearer_methods_supported"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil || rec.Code != http.StatusOK {
				t.Fatalf("metadata = %d %s (%v)", rec.Code, rec.Body, err)
			}
			if doc.Resource != tc.want {
				t.Errorf("resource = %q, want %q", doc.Resource, tc.want)
			}
			if !slices.Equal(doc.AuthorizationServers, []string{h.iss.URL()}) {
				t.Errorf("authorization_servers = %v, want exactly the issuer", doc.AuthorizationServers)
			}
			if !slices.Equal(doc.BearerMethodsSupported, []string{"header"}) {
				t.Errorf("bearer_methods_supported = %v", doc.BearerMethodsSupported)
			}
		})
	}

	// No copy at the root: under RFC 9728 §3.1 it would describe the whole
	// origin — and the SPA fallback must not answer for it either.
	for _, path := range []string{"/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/", "/.well-known/openid-configuration"} {
		if rec := do(h.handler, httptest.NewRequest("GET", path, nil)); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, rec.Code)
		}
	}

	// Origin: any request carrying it is refused, a valid token or not.
	r := bearer(httptest.NewRequest("POST", "/mcp", mcpInit()), h.good(t))
	r.Header.Set("Origin", "https://gawk-telemetry.kube.example")
	if rec := do(h.handler, r); rec.Code != http.StatusForbidden {
		t.Errorf("/mcp with an Origin header = %d, want 403", rec.Code)
	}

	// A valid token initializes.
	rec := do(h.handler, bearer(httptest.NewRequest("POST", "/mcp", mcpInit()), h.good(t)))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"protocolVersion"`) {
		t.Errorf("/mcp initialize with a good token = %d %s", rec.Code, rec.Body)
	}
}

// One implementation, two façades — through the gate too: with a bearer
// supplied, the MCP result and the HTTP response are identical.
func TestOIDCModeMCPAndHTTPReturnTheSameData(t *testing.T) {
	f := newReadFixture(t)
	h := newOIDCHarness(t, f)
	tok := h.good(t)

	rec := do(h.handler, bearer(httptest.NewRequest("POST", "/mcp", strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"diagnose","arguments":{"sessionId":"`+tmSession+`"}}}`)), tok))
	var rpc struct {
		Result struct {
			StructuredContent json.RawMessage `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rpc); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("mcp = %d %s (%v)", rec.Code, rec.Body, err)
	}
	var viaMCP, viaHTTP any
	_ = json.Unmarshal(rpc.Result.StructuredContent, &viaMCP)

	rec = do(h.handler, bearer(httptest.NewRequest("GET", "/v1/sessions/"+tmSession+"/diagnose", nil), tok))
	if rec.Code != http.StatusOK {
		t.Fatalf("http = %d %s", rec.Code, rec.Body)
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &viaHTTP)
	a, _ := json.Marshal(viaMCP)
	b, _ := json.Marshal(viaHTTP)
	if viaMCP == nil || string(a) != string(b) {
		t.Errorf("MCP and HTTP disagree through the gate:\n mcp: %s\nhttp: %s", a, b)
	}
}

// D1/D9: mode exclusivity and the OIDC triple.
func TestReadAuthFlags(t *testing.T) {
	base := []string{"-telemetry-key", key64}
	triple := []string{"-oidc-issuer", "https://id.example/realms/gawk", "-oidc-client-id", "gawk-telemetry", "-oidc-audience", "gawk-telemetry"}

	c, err := parseFlags(base, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if c.readAuthMode() != readAuthNone || c.oidcRole != "telemetry-reader" || c.oidcRolesClaim != oidcroles.DefaultClaim {
		t.Errorf("defaults: mode %s role %q claim %q", c.readAuthMode(), c.oidcRole, c.oidcRolesClaim)
	}

	c, err = parseFlags(append(append([]string{}, base...), triple...), noEnv)
	if err != nil || c.readAuthMode() != readAuthOIDC {
		t.Fatalf("the triple: mode %s, err %v", c.readAuthMode(), err)
	}

	// Both modes: refuse, naming both.
	for _, basic := range [][]string{{"-read-user", "ops"}, {"-read-password", "pw"}} {
		args := append(append(append([]string{}, base...), triple[:2]...), basic...)
		_, err := parseFlags(args, noEnv)
		if err == nil || !strings.Contains(err.Error(), "-read-user") || !strings.Contains(err.Error(), "-oidc-issuer") {
			t.Errorf("%v: err = %v, want a refusal naming both modes", args, err)
		}
	}

	// Partial triples refuse, naming what is missing.
	for i := 0; i < 3; i++ {
		args := append([]string{}, base...)
		for j := 0; j < 3; j++ {
			if j != i {
				args = append(args, triple[2*j], triple[2*j+1])
			}
		}
		_, err := parseFlags(args, noEnv)
		if err == nil || !strings.Contains(err.Error(), triple[2*i]) {
			t.Errorf("missing %s: err = %v", triple[2*i], err)
		}
	}

	// A blank roles claim or role would admit every valid token.
	for _, extra := range [][]string{{"-oidc-role", " "}, {"-oidc-roles-claim", ""}, {"-oidc-roles-claim", "a..b"}} {
		args := append(append(append([]string{}, base...), triple...), extra...)
		if _, err := parseFlags(args, noEnv); err == nil {
			t.Errorf("%v accepted", extra)
		}
	}

	// The dev proxy is meaningless without the OIDC mode.
	if _, err := parseFlags(append(append([]string{}, base...), "-dev-oidc-proxy", "http://fakeidp:8080"), noEnv); err == nil {
		t.Error("-dev-oidc-proxy accepted without the OIDC mode")
	}

	// Every knob has its env.
	c, err = parseFlags(nil, envMap(map[string]string{
		"GAWK_TELEMETRY_KEY":              key64,
		"GAWK_TELEMETRY_OIDC_ISSUER":      "https://id.example/realms/gawk",
		"GAWK_TELEMETRY_OIDC_CLIENT_ID":   "spa",
		"GAWK_TELEMETRY_OIDC_AUDIENCE":    "aud",
		"GAWK_TELEMETRY_OIDC_ROLES_CLAIM": "realm_access.roles",
		"GAWK_TELEMETRY_OIDC_ROLE":        "reader",
		"GAWK_TELEMETRY_DEV_OIDC_PROXY":   "http://fakeidp:8080",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.oidcIssuer != "https://id.example/realms/gawk" || c.oidcClientID != "spa" || c.oidcAudience != "aud" ||
		c.oidcRolesClaim != "realm_access.roles" || c.oidcRole != "reader" || c.devOIDCProxy != "http://fakeidp:8080" {
		t.Errorf("env not honoured: %+v", c)
	}
}

// docs/42 §5, restated in docs/55 §2: a client IP appears in the logs at
// Debug only. Every refusal shape, logged at Info.
func TestOIDCModeLogsNoClientIPAboveDebug(t *testing.T) {
	f := newReadFixture(t)
	iss := oidcauthtest.NewIssuer(t, "k1", tmKey())
	cfg := oidcConfig(iss.URL())
	var buf bytes.Buffer
	var mu sync.Mutex
	log := slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return buf.Write(p)
	}), &slog.HandlerOptions{Level: slog.LevelInfo}))
	v := newVerifierLogging(t, cfg, log)
	select {
	case <-v.Primed():
	case <-time.After(10 * time.Second):
		t.Fatal("verifier never primed")
	}
	h := &oidcHarness{iss: iss, v: v, cfg: cfg, handler: newReadHandler(cfg, f.routes, v)}
	const ip = "203.0.113.77"
	for _, tok := range []string{"", "garbage", h.mint(t, time.Hour, nil, nil), h.mint(t, -time.Hour, []string{DefaultOIDCRole}, nil)} {
		for _, path := range []string{"/live", "/v1/me"} {
			r := bearer(httptest.NewRequest("GET", path, nil), tok)
			r.RemoteAddr = ip + ":4321"
			do(h.handler, r)
		}
		r := bearer(httptest.NewRequest("POST", "/mcp", mcpInit()), tok)
		r.RemoteAddr = ip + ":4321"
		do(h.handler, r)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Contains(buf.String(), ip) {
		t.Errorf("a client IP was logged above Debug:\n%s", buf.String())
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
