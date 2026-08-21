package auth

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-admin/internal/config"
	"github.com/Tuhis/gawk/gawk-admin/internal/identity"
)

// testConfig goes through config.ParseFlags rather than building a struct
// literal, so the DEFAULT roles-claim path (`resource_access.{clientId}.roles`)
// is the one under test — including the {clientId} substitution, which only
// happens inside config.RolesClaimPath (docs/42 §4.8).
func testConfig(t *testing.T, issuer string) config.Config {
	t.Helper()
	cfg, err := config.ParseFlags([]string{
		"-external-url", "https://portal.example.test",
		"-oidc-issuer", issuer,
		"-oidc-client-id", testClientID,
		"-oidc-audience", testAudience,
		"-pg-dsn", "postgres://gawk@127.0.0.1/gawkadmin",
		"-relay-scan-target", "gawk-server-metrics",
		"-relay-admin-token", "relay-token",
		"-namespace", "production",
	}, func(string) string { return "" })
	if err != nil {
		t.Fatalf("parsing test config: %v", err)
	}
	return cfg
}

// newUnresolvedAuth constructs the authenticator without waiting for the
// provider: New no longer touches the network, so readiness is asynchronous.
func newUnresolvedAuth(t *testing.T, cfg config.Config, opts Options) *Auth {
	t.Helper()
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	if opts.ResolveRetryInterval == 0 {
		opts.ResolveRetryInterval = 5 * time.Millisecond
	}
	if opts.JWKSFetchBurst == 0 {
		// The JWKS fetch bucket has its own tests (keyset_test.go). Everywhere
		// else it must not be load-bearing: how many of a test's cases happen
		// to miss the key cache — a tampered signature does, a wrong `aud`
		// does not — would otherwise silently decide whether that test passes.
		opts.JWKSFetchBurst = 1_000
	}
	a, err := New(context.Background(), cfg, opts)
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

// newTestAuth is newUnresolvedAuth plus the wait, so every test that is not
// about the resolution lifecycle reads as though startup were synchronous.
func newTestAuth(t *testing.T, cfg config.Config, opts Options) *Auth {
	t.Helper()
	a := newUnresolvedAuth(t, cfg, opts)
	waitReady(t, a)
	return a
}

// waitResolveError blocks until at least one resolution attempt has failed and
// been recorded. Tests that want the unresolved state must wait for it to be
// OBSERVED: New's first attempt is in flight the moment New returns, so a test
// that flips the issuer back up without this barrier can race that attempt and
// silently exercise the happy path instead.
func waitResolveError(t *testing.T, a *Auth) error {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if err := a.ResolveError(); err != nil {
			return err
		}
		if time.Now().After(deadline) {
			t.Fatal("no resolution failure was recorded")
		}
		time.Sleep(time.Millisecond)
	}
}

func waitReady(t *testing.T, a *Auth) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !a.Ready() {
		if time.Now().After(deadline) {
			t.Fatalf("auth never became ready: %v", a.ResolveError())
		}
		time.Sleep(time.Millisecond)
	}
}

type echoedIdentity struct {
	Subject string   `json:"subject"`
	Email   string   `json:"email"`
	Roles   []string `json:"roles"`
}

// identityEcho renders what Middleware put on the context — the exact seam
// internal/api reads for GET /api/v1/me (§4.7).
func identityEcho() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := identity.FromContext(r.Context())
		if !ok {
			http.Error(w, "no identity on context", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(echoedIdentity{Subject: id.Subject, Email: id.Email, Roles: id.Roles})
	})
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
}

// testStack is the wiring cmd/gawk-admin will use, plus one deliberately
// mis-wired route (/misconfigured: RequireRole without Middleware) so the
// header and cookie sweeps cover the 500 path too.
func testStack(a *Auth, issuer string) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/auth/config", a.ConfigHandler())
	mux.Handle("/api/v1/me", a.Middleware(a.RequireRole(testOperator)(identityEcho())))
	mux.Handle("/api/v1/bans", a.Middleware(a.RequireRole(testOperator)(okHandler())))
	mux.Handle("/healthz", okHandler())
	mux.Handle("/misconfigured", a.RequireRole(testOperator)(okHandler()))
	return SecurityHeaders(issuer)(mux)
}

func do(t *testing.T, h http.Handler, method, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	return doFrom(t, h, method, path, token, nextClientIP())
}

func doFrom(t *testing.T, h http.Handler, method, path, token, remoteAddr string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.RemoteAddr = remoteAddr
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeError(t *testing.T, rec *httptest.ResponseRecorder) apiErrorBody {
	t.Helper()
	var env apiError
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("error body is not the §4.7 envelope: %v (%q)", err, rec.Body.String())
	}
	if env.Error.Code == "" || env.Error.Message == "" {
		t.Fatalf("error envelope missing code or message: %q", rec.Body.String())
	}
	return env.Error
}

// --- validation -------------------------------------------------------------

func TestValidTokenIsAcceptedAndCarriesIdentity(t *testing.T) {
	idp := newFakeIDP(t)
	a := newTestAuth(t, testConfig(t, idp.url()), Options{})
	h := testStack(a, idp.url())

	rec := do(t, h, http.MethodGet, "/api/v1/me", idp.mint(t, idp.claims()))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var got echoedIdentity
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding identity: %v", err)
	}
	if got.Subject != testSubject || got.Email != testEmail {
		t.Errorf("identity = %+v, want subject %q email %q", got, testSubject, testEmail)
	}
	if len(got.Roles) != 2 || got.Roles[1] != testOperator {
		t.Errorf("roles = %v, want the token's roles array verbatim", got.Roles)
	}
}

// Every way a credential can be invalid answers 401 — never 500, never a hint
// about which check failed (docs/42 §4.8).
func TestInvalidCredentialsAreRejectedWith401(t *testing.T) {
	idp := newFakeIDP(t)
	a := newTestAuth(t, testConfig(t, idp.url()), Options{})
	h := testStack(a, idp.url())
	now := time.Now()

	cases := []struct {
		name  string
		token func() string
	}{
		{"no token", func() string { return "" }},
		{"not a jwt", func() string { return "not-a-token" }},
		{"tampered signature", func() string { return tamper(t, idp.mint(t, idp.claims())) }},
		{"wrong issuer", func() string {
			return idp.mint(t, idp.claims(func(c map[string]any) { c["iss"] = "https://evil.example.test" }))
		}},
		{"wrong audience", func() string {
			return idp.mint(t, idp.claims(func(c map[string]any) { c["aud"] = "some-other-api" }))
		}},
		{"expired", func() string {
			return idp.mint(t, idp.claims(func(c map[string]any) { c["exp"] = now.Add(-time.Minute).Unix() }))
		}},
		{"no expiry at all", func() string {
			return idp.mint(t, idp.claims(func(c map[string]any) { delete(c, "exp") }))
		}},
		{"nbf in the future", func() string {
			// Comfortably past go-oidc's 5-minute clock-skew leeway.
			return idp.mint(t, idp.claims(func(c map[string]any) { c["nbf"] = now.Add(time.Hour).Unix() }))
		}},
		{"signed by a key the issuer never advertised", func() string {
			return idp.mintWith(t, keyB(), "attacker-key", idp.claims())
		}},
		{"claims rewritten under a real signature", func() string {
			// The forgery that matters here: a caller who legitimately holds a
			// role-less token edits the payload to grant themselves `operator`.
			// It must fail authentication (401), never reach authorization.
			roleless := idp.mint(t, idp.claims(func(c map[string]any) {
				c["resource_access"] = map[string]any{testClientID: map[string]any{"roles": []any{"viewer"}}}
			}))
			return rewriteClaims(t, roleless, func(c map[string]any) {
				c["resource_access"] = map[string]any{testClientID: map[string]any{"roles": []any{testOperator}}}
			})
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, h, http.MethodGet, "/api/v1/me", tc.token())
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 (body %q)", rec.Code, rec.Body.String())
			}
			if body := decodeError(t, rec); body.Code != "unauthorized" {
				t.Errorf("error code = %q, want %q", body.Code, "unauthorized")
			}
		})
	}
}

func TestNonBearerAuthorizationSchemeIsRejected(t *testing.T) {
	idp := newFakeIDP(t)
	a := newTestAuth(t, testConfig(t, idp.url()), Options{})
	h := testStack(a, idp.url())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	req.RemoteAddr = nextClientIP()
	req.Header.Set("Authorization", "Basic "+idp.mint(t, idp.claims()))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestBearerSchemeIsCaseInsensitive(t *testing.T) {
	idp := newFakeIDP(t)
	a := newTestAuth(t, testConfig(t, idp.url()), Options{})
	h := testStack(a, idp.url())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	req.RemoteAddr = nextClientIP()
	req.Header.Set("Authorization", "bearer "+idp.mint(t, idp.claims()))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: RFC 6750 auth schemes are case-insensitive", rec.Code)
	}
}

// The availability guarantee behind D7: once the JWKS is cached, verification
// is pure CPU. Prove it by shutting the issuer down and verifying a token the
// process has never seen.
func TestVerificationSucceedsFromCacheWithTheIssuerDown(t *testing.T) {
	idp := newFakeIDP(t)
	client, transport := countingClient()
	a := newTestAuth(t, testConfig(t, idp.url()), Options{HTTPClient: client})
	h := testStack(a, idp.url())

	if rec := do(t, h, http.MethodGet, "/api/v1/me", idp.mint(t, idp.claims())); rec.Code != http.StatusOK {
		t.Fatalf("warm-up status = %d, want 200", rec.Code)
	}
	if idp.keyFetches.Load() == 0 {
		t.Fatal("expected the JWKS to have been fetched at least once during startup")
	}

	fresh := idp.mint(t, idp.claims(func(c map[string]any) { c["sub"] = "another-operator" }))
	idp.stop()
	attempts := transport.attempts.Load()
	tokens := a.throttle.tokensLeft()

	rec := do(t, h, http.MethodGet, "/api/v1/me", fresh)
	if rec.Code != http.StatusOK {
		t.Fatalf("status with the issuer down = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	// Not "the fetch failed harmlessly" — no attempt at all. An attempt would
	// mean a connect timeout on the request path whenever the IdP is away.
	if got := transport.attempts.Load(); got != attempts {
		t.Errorf("HTTP attempts = %d, want %d: verification from cache must not touch the IdP", got, attempts)
	}
	// Belt and braces: the fetch throttle sits in FRONT of that transport, so
	// an unchanged bucket proves the fetch path was not even consulted.
	if got := a.throttle.tokensLeft(); got != tokens {
		t.Errorf("fetch tokens = %v, want %v: a cached verification must not reach for the network", got, tokens)
	}
}

// Rotation, on-demand fetch counts and the throttle live in keyset_test.go:
// they are properties of the key set, and asserting them needs the fake
// issuer's fetch counter rather than a status code.

// --- authorization ----------------------------------------------------------

func TestValidTokenWithoutTheOperatorRoleIsForbiddenEverywhere(t *testing.T) {
	idp := newFakeIDP(t)
	a := newTestAuth(t, testConfig(t, idp.url()), Options{})
	h := testStack(a, idp.url())

	token := idp.mint(t, idp.claims(func(c map[string]any) {
		c["resource_access"] = map[string]any{testClientID: map[string]any{"roles": []any{"viewer"}}}
	}))
	for _, path := range []string{"/api/v1/me", "/api/v1/bans"} {
		rec := do(t, h, http.MethodGet, path, token)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s status = %d, want 403 (body %q)", path, rec.Code, rec.Body.String())
		}
		if body := decodeError(t, rec); body.Code != "forbidden" {
			t.Errorf("%s error code = %q, want %q", path, body.Code, "forbidden")
		}
	}
}

// A claim shape we do not recognise is the IdP's configuration talking, not a
// bug in this process: it must deny, not crash (AP5).
func TestUnusableRolesClaimIsForbiddenNeverInternalError(t *testing.T) {
	idp := newFakeIDP(t)
	a := newTestAuth(t, testConfig(t, idp.url()), Options{})
	h := testStack(a, idp.url())

	cases := []struct {
		name    string
		mutator func(map[string]any)
	}{
		{"claim absent entirely", func(c map[string]any) { delete(c, "resource_access") }},
		{"client not in resource_access", func(c map[string]any) {
			c["resource_access"] = map[string]any{"some-other-client": map[string]any{"roles": []any{testOperator}}}
		}},
		{"roles key missing", func(c map[string]any) {
			c["resource_access"] = map[string]any{testClientID: map[string]any{}}
		}},
		{"roles is a string", func(c map[string]any) {
			c["resource_access"] = map[string]any{testClientID: map[string]any{"roles": testOperator}}
		}},
		{"roles is an object", func(c map[string]any) {
			c["resource_access"] = map[string]any{testClientID: map[string]any{"roles": map[string]any{"0": testOperator}}}
		}},
		{"roles holds a non-string", func(c map[string]any) {
			c["resource_access"] = map[string]any{testClientID: map[string]any{"roles": []any{testOperator, 7}}}
		}},
		{"intermediate segment is not an object", func(c map[string]any) { c["resource_access"] = "nope" }},
		{"roles array is empty", func(c map[string]any) {
			c["resource_access"] = map[string]any{testClientID: map[string]any{"roles": []any{}}}
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, h, http.MethodGet, "/api/v1/me", idp.mint(t, idp.claims(tc.mutator)))
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 (body %q)", rec.Code, rec.Body.String())
			}
		})
	}
}

// The default path only works if {clientId} is substituted — proven by moving
// the roles under a different client and watching authorization fail.
func TestDefaultClaimPathSubstitutesTheClientID(t *testing.T) {
	cfg := testConfig(t, "https://issuer.example.test")
	if want := "resource_access." + testClientID + ".roles"; cfg.RolesClaimPath() != want {
		t.Fatalf("RolesClaimPath() = %q, want %q", cfg.RolesClaimPath(), want)
	}

	idp := newFakeIDP(t)
	a := newTestAuth(t, testConfig(t, idp.url()), Options{})
	h := testStack(a, idp.url())

	underOtherClient := idp.mint(t, idp.claims(func(c map[string]any) {
		c["resource_access"] = map[string]any{"another-client": map[string]any{"roles": []any{testOperator}}}
	}))
	if rec := do(t, h, http.MethodGet, "/api/v1/me", underOtherClient); rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: roles under another client must not authorize", rec.Code)
	}
}

func TestOverriddenRolesClaimPath(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		mutator func(map[string]any)
	}{
		{
			// The non-Keycloak shape §4.8 names: a plain top-level claim.
			name:    "top-level claim",
			path:    "groups",
			mutator: func(c map[string]any) { c["groups"] = []any{"someone-else", testOperator} },
		},
		{
			name: "nested claim",
			path: "authz.gawk.roles",
			mutator: func(c map[string]any) {
				c["authz"] = map[string]any{"gawk": map[string]any{"roles": []any{testOperator}}}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			idp := newFakeIDP(t)
			cfg := testConfig(t, idp.url())
			cfg.OIDCRolesClaim = tc.path
			a := newTestAuth(t, cfg, Options{})
			h := testStack(a, idp.url())

			// The overridden path authorizes...
			if rec := do(t, h, http.MethodGet, "/api/v1/me", idp.mint(t, idp.claims(tc.mutator))); rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
			}
			// ...and the default Keycloak claim, still present in the token, no
			// longer does. Only the configured path is read.
			if rec := do(t, h, http.MethodGet, "/api/v1/me", idp.mint(t, idp.claims())); rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403: only the configured path may authorize", rec.Code)
			}
		})
	}
}

// docs/42 §5, spelled out as a test: the access-token lifetime IS the
// revocation horizon. Removing the role at the IdP takes effect at the next
// refresh — not before, and not later.
func TestRefreshHorizonIsTheRevocationHorizon(t *testing.T) {
	idp := newFakeIDP(t)
	a := newTestAuth(t, testConfig(t, idp.url()), Options{})
	h := testStack(a, idp.url())

	// An operator holding a live access token.
	beforeRevocation := idp.mint(t, idp.claims())
	if rec := do(t, h, http.MethodGet, "/api/v1/me", beforeRevocation); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	// The role is removed at the IdP. The next token the SPA's refresh obtains
	// no longer carries it.
	afterRevocation := idp.mint(t, idp.claims(func(c map[string]any) {
		c["resource_access"] = map[string]any{testClientID: map[string]any{"roles": []any{}}}
	}))
	if rec := do(t, h, http.MethodGet, "/api/v1/me", afterRevocation); rec.Code != http.StatusForbidden {
		t.Fatalf("status for the post-revocation token = %d, want 403", rec.Code)
	}

	// And the honest other half: the already-issued token keeps working until
	// it expires. That is the horizon §4.8 asks operators to keep short
	// (5–15 minute access tokens), not a bug to fix here.
	if rec := do(t, h, http.MethodGet, "/api/v1/me", beforeRevocation); rec.Code != http.StatusOK {
		t.Fatalf("status for the pre-revocation token = %d, want 200: a JWT cannot be revoked before it expires", rec.Code)
	}
}

func TestRequireRoleWithoutMiddlewareIsInternalError(t *testing.T) {
	idp := newFakeIDP(t)
	a := newTestAuth(t, testConfig(t, idp.url()), Options{})
	h := testStack(a, idp.url())

	rec := do(t, h, http.MethodGet, "/misconfigured", idp.mint(t, idp.claims()))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: RequireRole without Middleware is a wiring bug, not an anonymous caller", rec.Code)
	}
}

// --- refusing to start ------------------------------------------------------

func TestNewRefusesAConfigurationThatWouldAuthorizeEveryone(t *testing.T) {
	idp := newFakeIDP(t)
	base := testConfig(t, idp.url())

	cases := []struct {
		name   string
		mutate func(*config.Config)
	}{
		{"blank roles claim path", func(c *config.Config) { c.OIDCRolesClaim = "" }},
		{"whitespace roles claim path", func(c *config.Config) { c.OIDCRolesClaim = "   " }},
		{"roles claim path with an empty segment", func(c *config.Config) { c.OIDCRolesClaim = "resource_access..roles" }},
		{"blank operator role", func(c *config.Config) { c.OperatorRole = "" }},
		{"blank audience", func(c *config.Config) { c.OIDCAudience = "" }},
		{"blank issuer", func(c *config.Config) { c.OIDCIssuer = "" }},
		{"blank client id", func(c *config.Config) { c.OIDCClientID = "" }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			tc.mutate(&cfg)
			a, err := New(context.Background(), cfg, Options{})
			if err == nil {
				_ = a.Close()
				t.Fatal("New succeeded; want a refusal — this configuration would authorize every valid token")
			}
		})
	}

	// config.ParseFlags refuses the same two knobs, so neither entry point can
	// bring the portal up with authorization effectively off (docs/42 §4.12).
	for _, flag := range [][]string{{"-oidc-roles-claim", ""}, {"-operator-role", ""}} {
		args := append([]string{
			"-external-url", "https://portal.example.test",
			"-oidc-issuer", "https://issuer.example.test",
			"-oidc-client-id", testClientID,
			"-oidc-audience", testAudience,
			"-pg-dsn", "postgres://gawk@127.0.0.1/gawkadmin",
			"-relay-scan-target", "gawk-server-metrics",
			"-relay-admin-token", "relay-token",
			"-namespace", "production",
		}, flag[0]+"="+flag[1])
		if _, err := config.ParseFlags(args, func(string) string { return "" }); err == nil {
			t.Errorf("config.ParseFlags accepted %s=%q", flag[0], flag[1])
		}
	}
}

func TestRequireRoleWithAnEmptyRolePanics(t *testing.T) {
	idp := newFakeIDP(t)
	a := newTestAuth(t, testConfig(t, idp.url()), Options{})

	defer func() {
		if recover() == nil {
			t.Fatal("RequireRole(\"\") returned a middleware; want a panic at wiring time")
		}
	}()
	a.RequireRole("")
}

// docs/42 §6 "OIDC provider down": no new logins, enforcement untouched — the
// portal degrades. It does not die, and it must not take both replicas into
// CrashLoopBackOff over an IdP restart (D16).
func TestNewSucceedsWhileTheIssuerIsUnreachable(t *testing.T) {
	idp := newFakeIDP(t)
	cfg := testConfig(t, idp.url())
	idp.stop()

	a := newUnresolvedAuth(t, cfg, Options{})
	// ResolveError is what /readyz reports; without it "not ready" is mute.
	waitResolveError(t, a)
	if a.Ready() {
		t.Fatal("Ready() is true with the issuer down")
	}

	h := testStack(a, idp.url())

	// Authenticated routes refuse, with 401 — never 500, and never a hint that
	// this process is confused rather than the IdP being away.
	for _, path := range []string{"/api/v1/me", "/api/v1/bans"} {
		rec := do(t, h, http.MethodGet, path, "any-token-at-all")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s status = %d, want 401 (body %q)", path, rec.Code, rec.Body.String())
		}
		if body := decodeError(t, rec); body.Code != "idp_unavailable" {
			t.Errorf("%s error code = %q, want %q", path, body.Code, "idp_unavailable")
		}
	}

	// The SPA bootstrap still works: it is what lets the browser be bounced to
	// the IdP, which is where this failure belongs.
	if rec := do(t, h, http.MethodGet, "/auth/config", ""); rec.Code != http.StatusOK {
		t.Errorf("/auth/config status = %d, want 200 while the issuer is down", rec.Code)
	}
	// And the unauthenticated routes the kubelet uses are untouched.
	if rec := do(t, h, http.MethodGet, "/healthz", ""); rec.Code != http.StatusOK {
		t.Errorf("/healthz status = %d, want 200", rec.Code)
	}
}

// An operator retrying during an IdP outage must not be rate-limited: the
// failure is ours, not their credential's.
func TestUnresolvedRefusalsDoNotSpendTheFailureBudget(t *testing.T) {
	idp := newFakeIDP(t)
	cfg := testConfig(t, idp.url())
	idp.stop()

	a := newUnresolvedAuth(t, cfg, Options{FailureBurst: 2, FailureRate: 0.001})
	waitResolveError(t, a)
	h := testStack(a, idp.url())

	const operator = "198.51.100.77:5555"
	for i := range 10 {
		rec := doFrom(t, h, http.MethodGet, "/api/v1/me", "token", operator)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status = %d, want 401 — an outage of ours must not become a 429", i+1, rec.Code)
		}
	}
}

// The recovery path, end to end: unready, then the IdP comes up mid-flight,
// Ready flips, and a token minted by that issuer verifies.
func TestBecomesReadyWhenTheIssuerComesUp(t *testing.T) {
	idp := newFakeIDP(t)
	idp.setDown(true)

	logs := &syncBuffer{}
	a := newUnresolvedAuth(t, testConfig(t, idp.url()), Options{
		Logger: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo})),
	})
	h := testStack(a, idp.url())

	waitResolveError(t, a)
	if a.Ready() {
		t.Fatal("Ready() is true before the issuer answered")
	}
	rec := do(t, h, http.MethodGet, "/api/v1/me", "unverifiable")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status while unresolved = %d, want 401", rec.Code)
	}
	if body := decodeError(t, rec); body.Code != "idp_unavailable" {
		t.Fatalf("error code = %q, want %q — the test must be exercising the unresolved path", body.Code, "idp_unavailable")
	}

	idp.setDown(false)
	waitReady(t, a)

	if a.ResolveError() != nil {
		t.Errorf("ResolveError() = %v, want nil once resolved", a.ResolveError())
	}
	if rec := do(t, h, http.MethodGet, "/api/v1/me", idp.mint(t, idp.claims())); rec.Code != http.StatusOK {
		t.Fatalf("status after the issuer came up = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}

	// Both state changes are legible to an operator reading the log at its
	// default level, and the first one says the portal is up but unusable.
	out := logs.String()
	for _, want := range []string{
		"level=WARN",
		"oidc issuer unresolved",
		"the portal is serving but no request can authenticate",
		"oidc issuer resolved: authentication is available again",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log does not mention %q; got:\n%s", want, out)
		}
	}
}

// --- /auth/config -----------------------------------------------------------

func TestAuthConfigServesExactlyTheBootstrapTriple(t *testing.T) {
	idp := newFakeIDP(t)
	a := newTestAuth(t, testConfig(t, idp.url()), Options{})
	h := testStack(a, idp.url())

	// Unauthenticated: this is what the SPA reads before it has a token.
	rec := do(t, h, http.MethodGet, "/auth/config", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}

	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decoding body: %v", err)
	}
	want := map[string]string{
		"issuer":   idp.url(),
		"clientId": testClientID,
		"audience": testAudience,
	}
	if len(raw) != len(want) {
		t.Fatalf("keys = %v, want exactly %v: this endpoint is world-readable", keysOf(raw), keysOf(toAny(want)))
	}
	for k, v := range want {
		if got, ok := raw[k].(string); !ok || got != v {
			t.Errorf("%s = %v, want %q", k, raw[k], v)
		}
	}
}

func TestAuthConfigRejectsNonGET(t *testing.T) {
	idp := newFakeIDP(t)
	a := newTestAuth(t, testConfig(t, idp.url()), Options{})
	h := testStack(a, idp.url())

	rec := do(t, h, http.MethodPost, "/auth/config", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func toAny(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// --- rate limiting ----------------------------------------------------------

// §4.8: invalid-credential responses are rate-limited per IP. The properties
// that matter are that the budget is per source AND that a valid token is
// never collateral damage — behind an Ingress, every operator can share one
// observed address (auth.go, denyCredential).
func TestInvalidCredentialsAreRateLimitedPerIP(t *testing.T) {
	idp := newFakeIDP(t)
	a := newTestAuth(t, testConfig(t, idp.url()), Options{
		FailureBurst: 2,
		// Slow enough that refill cannot rescue the attacker mid-test.
		FailureRate: 0.001,
	})
	h := testStack(a, idp.url())

	const attacker = "198.51.100.9:5555"
	const bystander = "198.51.100.10:5555"

	for i := range 2 {
		if rec := doFrom(t, h, http.MethodGet, "/api/v1/me", "forged-"+string(rune('a'+i)), attacker); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status = %d, want 401", i+1, rec.Code)
		}
	}
	rec := doFrom(t, h, http.MethodGet, "/api/v1/me", "forged-again", attacker)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status after the burst = %d, want 429 (body %q)", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("429 carries no Retry-After")
	}
	if body := decodeError(t, rec); body.Code != "rate_limited" {
		t.Errorf("error code = %q, want %q", body.Code, "rate_limited")
	}

	// Another source still gets the ordinary answer: the budget is per IP.
	if rec := doFrom(t, h, http.MethodGet, "/api/v1/me", "forged", bystander); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bystander status = %d, want 401", rec.Code)
	}

	// And the exhausted IP can still authenticate: only failures spend tokens,
	// so a fuzzing loop cannot lock out an operator sharing its egress.
	if rec := doFrom(t, h, http.MethodGet, "/api/v1/me", idp.mint(t, idp.claims()), attacker); rec.Code != http.StatusOK {
		t.Fatalf("valid token from the rate-limited IP = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
}

// --- headers and cookies ----------------------------------------------------

// responseSet exercises every status this package can produce, so the header
// and cookie assertions below are genuinely "on every response".
func responseSet(t *testing.T, idp *fakeIDP, h http.Handler) map[string]*httptest.ResponseRecorder {
	t.Helper()
	roleless := idp.mint(t, idp.claims(func(c map[string]any) {
		c["resource_access"] = map[string]any{testClientID: map[string]any{"roles": []any{"viewer"}}}
	}))
	const hot = "198.51.100.200:5555"
	out := map[string]*httptest.ResponseRecorder{
		"200 authenticated":   do(t, h, http.MethodGet, "/api/v1/me", idp.mint(t, idp.claims())),
		"200 auth config":     do(t, h, http.MethodGet, "/auth/config", ""),
		"200 unguarded route": do(t, h, http.MethodGet, "/healthz", ""),
		"401 no credential":   do(t, h, http.MethodGet, "/api/v1/me", ""),
		"403 missing role":    do(t, h, http.MethodGet, "/api/v1/me", roleless),
		"405 wrong method":    do(t, h, http.MethodPost, "/auth/config", ""),
		"500 mis-wired route": do(t, h, http.MethodGet, "/misconfigured", idp.mint(t, idp.claims())),
	}
	// Drive one IP into the limiter for the 429.
	for range 32 {
		out["429 rate limited"] = doFrom(t, h, http.MethodGet, "/api/v1/me", "forged", hot)
	}
	if out["429 rate limited"].Code != http.StatusTooManyRequests {
		t.Fatalf("could not produce a 429 (got %d)", out["429 rate limited"].Code)
	}
	return out
}

// D17: no cookies, no sessions, no CSRF machinery. A Set-Cookie anywhere is a
// test failure.
func TestNoResponseEverSetsACookie(t *testing.T) {
	idp := newFakeIDP(t)
	a := newTestAuth(t, testConfig(t, idp.url()), Options{})
	h := testStack(a, idp.url())

	for name, rec := range responseSet(t, idp, h) {
		if values := rec.Result().Header.Values("Set-Cookie"); len(values) > 0 {
			t.Errorf("%s set a cookie: %v", name, values)
		}
	}
}

func TestSecurityHeadersOnEveryResponse(t *testing.T) {
	idp := newFakeIDP(t)
	a := newTestAuth(t, testConfig(t, idp.url()), Options{})
	h := testStack(a, idp.url())

	for name, rec := range responseSet(t, idp, h) {
		csp := rec.Header().Get("Content-Security-Policy")
		if csp == "" {
			t.Errorf("%s: no Content-Security-Policy", name)
			continue
		}
		if !strings.Contains(csp, "default-src 'self'") {
			t.Errorf("%s: CSP %q lacks default-src 'self'", name, csp)
		}
		// The IdP is the single sanctioned external origin: the SPA runs the
		// code+PKCE flow against it directly (§4.8).
		if !strings.Contains(csp, "connect-src 'self' "+idp.url()) {
			t.Errorf("%s: CSP %q lacks the issuer origin in connect-src", name, csp)
		}
		if !strings.Contains(csp, "frame-ancestors 'none'") {
			t.Errorf("%s: CSP %q lacks frame-ancestors 'none'", name, csp)
		}
		if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s: X-Content-Type-Options = %q, want nosniff", name, got)
		}
		if got := rec.Header().Get("Referrer-Policy"); got != "no-referrer" {
			t.Errorf("%s: Referrer-Policy = %q, want no-referrer", name, got)
		}
	}
}

// Middleware and ConfigHandler stamp the headers themselves, so an /api/v1
// response is never bare even if the mux-level wrap is forgotten.
func TestAuthOwnedRoutesCarryHeadersWithoutTheMuxWrap(t *testing.T) {
	idp := newFakeIDP(t)
	a := newTestAuth(t, testConfig(t, idp.url()), Options{})

	mux := http.NewServeMux()
	mux.Handle("/auth/config", a.ConfigHandler())
	mux.Handle("/api/v1/me", a.Middleware(a.RequireRole(testOperator)(identityEcho())))

	for _, path := range []string{"/auth/config", "/api/v1/me"} {
		rec := do(t, mux, http.MethodGet, path, idp.mint(t, idp.claims()))
		if rec.Header().Get("Content-Security-Policy") == "" {
			t.Errorf("%s: no CSP without the mux-level SecurityHeaders wrap", path)
		}
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	idp := newFakeIDP(t)
	a := newTestAuth(t, testConfig(t, idp.url()), Options{})
	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// A key rotation lands while requests are in flight: the cache is swapped
// under concurrent readers, and the on-demand refresh path is reachable from
// many goroutines at once. Both are exactly where a cached verifier races.
func TestConcurrentVerificationAcrossARotation(t *testing.T) {
	idp := newFakeIDP(t)
	a := newTestAuth(t, testConfig(t, idp.url()), Options{
		// Six hard rotations in a row is not a rate the fetch floor is meant
		// to survive — this test is about the race, so the bucket is opened up
		// and the throttle gets its own tests (keyset_test.go).
		JWKSFetchInterval: time.Microsecond,
		JWKSFetchBurst:    1_000,
	})
	h := testStack(a, idp.url())

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				// Mint inside the loop so the token always carries the
				// issuer's current key, rotation or not.
				rec := do(t, h, http.MethodGet, "/api/v1/me", idp.mint(t, idp.claims()))
				if rec.Code != http.StatusOK && rec.Code != http.StatusUnauthorized {
					t.Errorf("unexpected status under concurrent rotation: %d (%q)", rec.Code, rec.Body.String())
					return
				}
			}
		}()
	}
	for i := range 6 {
		if i%2 == 0 {
			idp.useKey("key-b", keyB())
		} else {
			idp.useKey("key-a", keyA())
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(stop)
	wg.Wait()

	// After the churn settles, the current key must still authenticate.
	if rec := do(t, h, http.MethodGet, "/api/v1/me", idp.mint(t, idp.claims())); rec.Code != http.StatusOK {
		t.Fatalf("status after the rotation churn = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
}

// Shutdown must not wait on the IdP either. The only JWKS traffic left is a
// request-path fetch, and go-oidc runs it detached from cancellation
// (keyset.go) — so Close must not be waiting on one.
func TestCloseReturnsPromptlyWhileTheIssuerHangs(t *testing.T) {
	idp := newFakeIDP(t)
	a := newTestAuth(t, testConfig(t, idp.url()), Options{})
	h := testStack(a, idp.url())

	release := idp.hangKeys()
	defer release()

	// One request stuck on the IdP: its token is signed by a key nothing has
	// cached, so verification reaches for the JWKS and stays there.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		rec := do(t, h, http.MethodGet, "/api/v1/me", idp.mintWith(t, keyB(), "never-advertised", idp.claims()))
		// Whether the fetch is released or times out, the answer is a refusal
		// — never a 5xx out of a stalled dependency.
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("stalled-fetch request = %d, want 401 (body %q)", rec.Code, rec.Body.String())
		}
	}()

	deadline := time.Now().Add(5 * time.Second)
	for idp.keyFetches.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no JWKS fetch arrived to hang")
		}
		time.Sleep(time.Millisecond)
	}

	closed := make(chan struct{})
	go func() {
		_ = a.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Close blocked on a hanging IdP; shutdown must not wait out the fetch timeout")
	}

	release()
	wg.Wait()
}

// A rotation is a thundering herd: every operator's next token is signed by a
// key this process has not seen, and they arrive at once. All of them must be
// accepted off the single fetch the first one triggers — go-oidc's own
// singleflight, asserted here so a future upstream change that loses it is
// caught in this repository rather than at the IdP.
func TestConcurrentUnknownKeyRequestsAllSucceedOnOneFetch(t *testing.T) {
	idp := newFakeIDP(t)
	client, transport := countingClient()
	// A stopped clock, so the bucket cannot refill under the test's own
	// wall-clock duration and "the herd cost one token" stays an exact claim.
	clk := newTestClock()
	a := newTestAuth(t, testConfig(t, idp.url()), Options{
		HTTPClient: client,
		Now:        clk.now,
		// Production-shaped: the bucket holds three, and the herd must cost
		// one. If the coalescing were lost, sixteen requests would want
		// sixteen fetches and thirteen of them would be refused.
		JWKSFetchInterval: defaultJWKSFetchInterval,
		JWKSFetchBurst:    defaultJWKSFetchBurst,
	})
	h := testStack(a, idp.url())

	if rec := do(t, h, http.MethodGet, "/api/v1/me", idp.mint(t, idp.claims())); rec.Code != http.StatusOK {
		t.Fatalf("warm-up status = %d, want 200", rec.Code)
	}

	idp.useKey("key-b", keyB())
	tokens := make([]string, 16)
	for i := range tokens {
		tokens[i] = idp.mint(t, idp.claims())
	}
	// A JWKS that takes a visible moment, so "they arrive at once" is a fact
	// about the test rather than a hope about the scheduler.
	idp.delayKeys(150 * time.Millisecond)
	attemptsBefore := transport.attempts.Load()

	var wg sync.WaitGroup
	codes := make([]int, len(tokens))
	start := make(chan struct{})
	for i, token := range tokens {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			codes[i] = do(t, h, http.MethodGet, "/api/v1/me", token).Code
		}()
	}
	close(start)
	wg.Wait()

	for i, code := range codes {
		if code != http.StatusOK {
			t.Errorf("request %d = %d, want 200: the rotated key was fetched for the whole herd", i, code)
		}
	}
	if got := transport.attempts.Load() - attemptsBefore; got != 1 {
		t.Errorf("JWKS fetches during the herd = %d, want 1 (go-oidc coalesces concurrent misses)", got)
	}
	if got := a.throttle.tokensLeft(); got != float64(defaultJWKSFetchBurst)-2 {
		t.Errorf("tokens left = %v, want %v: the herd must cost exactly one",
			got, float64(defaultJWKSFetchBurst)-2)
	}
}
