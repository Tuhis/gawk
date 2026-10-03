package oidcauth_test

// The package's exported surface, end to end against oidcauthtest. The deep
// behaviour — the fetch floor under attack, rotation herds, offline
// verification, priming, the failure budget, the header sweep — is asserted
// by the two suites that predate this package and moved onto it unchanged
// (R53, docs/55 G5): gawk-admin's internal/auth (R39 AP5) and the relay's
// internal/ops admin-API suite. What is here is what a new consumer relies on
// first, plus the contract points neither suite reaches.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/oidcauth"
	"github.com/Tuhis/gawk/gawk-server/oidcauth/oidcauthtest"
	"github.com/Tuhis/gawk/gawk-server/oidcroles"
)

const (
	testAudience = "gawk-telemetry"
	testClientID = "gawk-telemetry-spa"
	testRole     = "telemetry-reader"
)

var testKey = sync.OnceValue(oidcauthtest.NewRSAKey)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func testConfig(issuer string) oidcauth.Config {
	return oidcauth.Config{
		Issuer:     issuer,
		Audience:   testAudience,
		RolesClaim: oidcroles.DefaultClaim,
		Role:       testRole,
		ClientID:   testClientID,
	}
}

func newVerifier(t *testing.T, cfg oidcauth.Config) *oidcauth.Verifier {
	t.Helper()
	v, err := oidcauth.New(context.Background(), cfg, oidcauth.Options{
		Logger:               quiet,
		ResolveRetryInterval: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("oidcauth.New: %v", err)
	}
	t.Cleanup(func() { _ = v.Close() })
	return v
}

func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s never happened", what)
	}
}

func token(iss *oidcauthtest.Issuer, roles string) string {
	return iss.Mint(fmt.Sprintf(`{"iss": %q, "aud": %q, "sub": "sub-1", "email": "op@example.test",
		"exp": %s, "resource_access": {%q: {"roles": %s}}}`,
		iss.URL(), testAudience, strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10), testAudience, roles))
}

func TestVerifyProjectsAGoodTokenOntoAnIdentity(t *testing.T) {
	iss := oidcauthtest.NewIssuer(t, "k1", testKey())
	v := newVerifier(t, testConfig(iss.URL()))
	waitFor(t, v.Resolved(), "discovery")
	waitFor(t, v.Primed(), "priming")
	if !v.Ready() || v.ResolveError() != nil {
		t.Fatalf("Ready = %v, ResolveError = %v after resolution", v.Ready(), v.ResolveError())
	}
	if got := iss.KeyFetches(); got != 1 {
		t.Errorf("JWKS fetches after priming = %d, want 1", got)
	}

	id, err := v.Verify(context.Background(), token(iss, `["`+testRole+`"]`))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if id.Subject != "sub-1" || id.Email != "op@example.test" || !id.HasRole(testRole) || id.Actor() != "op@example.test" {
		t.Errorf("identity = %+v", id)
	}
	// A malformed roles claim verifies — with no roles. Authorizing is the
	// caller's job, and a token that cannot prove a role does not have it.
	id, err = v.Verify(context.Background(), token(iss, `{"not":"an array"}`))
	if err != nil || len(id.Roles) != 0 {
		t.Errorf("malformed roles claim: identity %+v, err %v; want a verified identity with no roles", id, err)
	}
	if _, err := v.Verify(context.Background(), oidcauthtest.Tamper(t, token(iss, `[]`))); err == nil {
		t.Error("a tampered signature verified")
	}
	if v.JWKSFetchTokensLeft() != float64(oidcauth.DefaultJWKSFetchBurst)-1 {
		// The tampered token missed the cache and spent one fetch; the good
		// ones did not.
		t.Errorf("fetch tokens left = %v, want the burst less one", v.JWKSFetchTokensLeft())
	}
}

func TestVerifyIsErrNotReadyUntilDiscoveryResolves(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	url := dead.URL
	dead.Close()
	v := newVerifier(t, testConfig(url))
	if _, err := v.Verify(context.Background(), "a.b.c"); !errors.Is(err, oidcauth.ErrNotReady) {
		t.Fatalf("Verify before discovery = %v, want ErrNotReady", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for v.ResolveError() == nil {
		if time.Now().After(deadline) {
			t.Fatal("no resolution failure was recorded")
		}
		time.Sleep(time.Millisecond)
	}
	if v.Ready() {
		t.Error("Ready with an unreachable issuer")
	}
}

func TestTheHTTPSurfaceComposes(t *testing.T) {
	iss := oidcauthtest.NewIssuer(t, "k1", testKey())
	v := newVerifier(t, testConfig(iss.URL()))
	waitFor(t, v.Primed(), "priming")

	mux := http.NewServeMux()
	mux.Handle("GET /auth/config", v.ConfigHandler())
	mux.Handle("/v1/", v.Middleware(v.RequireRole(v.Role())(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, _ := oidcauth.FromContext(r.Context())
		_, _ = w.Write([]byte(id.Subject))
	}))))
	h := oidcauth.SecurityHeaders(iss.URL())(mux)

	call := func(path, bearer string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	if rec := call("/v1/sessions", token(iss, `["`+testRole+`"]`)); rec.Code != http.StatusOK || rec.Body.String() != "sub-1" {
		t.Errorf("good token = %d %q, want 200 sub-1", rec.Code, rec.Body.String())
	}
	if rec := call("/v1/sessions", token(iss, `["someone-else"]`)); rec.Code != http.StatusForbidden {
		t.Errorf("token without the role = %d, want 403", rec.Code)
	}
	rec := call("/v1/sessions", "")
	if rec.Code != http.StatusUnauthorized || rec.Header().Get("Content-Security-Policy") == "" {
		t.Errorf("no token = %d (CSP %q), want 401 with the security headers", rec.Code, rec.Header().Get("Content-Security-Policy"))
	}
	var env struct {
		Error struct{ Code string } `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil || env.Error.Code != oidcauth.CodeUnauthorized {
		t.Errorf("401 envelope = %q (%v), want code %q", rec.Body.String(), err, oidcauth.CodeUnauthorized)
	}

	rec = call("/auth/config", "")
	var boot map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &boot); err != nil {
		t.Fatalf("/auth/config body %q: %v", rec.Body.String(), err)
	}
	want := map[string]string{"issuer": iss.URL(), "clientId": testClientID, "audience": testAudience}
	if len(boot) != len(want) || boot["issuer"] != want["issuer"] || boot["clientId"] != want["clientId"] || boot["audience"] != want["audience"] {
		t.Errorf("/auth/config = %v, want exactly %v", boot, want)
	}
}

// A pure resource server (the relay) has no browser client, so New accepts a
// blank ClientID — but publishing a bootstrap without one is a wiring error.
func TestConfigHandlerRefusesABlankClientID(t *testing.T) {
	cfg := testConfig("https://id.example.test/realms/gawk")
	cfg.ClientID = ""
	v := newVerifier(t, cfg)
	defer func() {
		if recover() == nil {
			t.Error("ConfigHandler with no ClientID did not panic")
		}
	}()
	v.ConfigHandler()
}

func TestNewRefusesAConfigurationThatWouldAdmitEveryone(t *testing.T) {
	for name, mutate := range map[string]func(*oidcauth.Config){
		"blank issuer":               func(c *oidcauth.Config) { c.Issuer = " " },
		"blank audience":             func(c *oidcauth.Config) { c.Audience = "" },
		"blank role":                 func(c *oidcauth.Config) { c.Role = "  " },
		"blank roles claim":          func(c *oidcauth.Config) { c.RolesClaim = "" },
		"roles claim, empty segment": func(c *oidcauth.Config) { c.RolesClaim = "resource_access..roles" },
	} {
		cfg := testConfig("https://id.example.test/realms/gawk")
		mutate(&cfg)
		if v, err := oidcauth.New(context.Background(), cfg, oidcauth.Options{Logger: quiet}); err == nil {
			_ = v.Close()
			t.Errorf("%s: New succeeded", name)
		}
	}
}

// Neither the provider's discovery document nor Options.SigningAlgorithms
// can widen the allowlist to "none" or the HMAC family.
func TestTheAlgorithmAllowlistIsAsymmetricOnly(t *testing.T) {
	algs := oidcauth.AsymmetricSigningAlgs()
	for _, bad := range []string{"none", "HS256", "HS384", "HS512"} {
		if slices.Contains(algs, bad) {
			t.Errorf("allowlist carries %s", bad)
		}
	}
	algs[0] = "HS256" // a copy: mutating it must not widen the next caller's
	if slices.Contains(oidcauth.AsymmetricSigningAlgs(), "HS256") {
		t.Error("AsymmetricSigningAlgs returned the package's own slice")
	}
}

// A consumer that holds a response open past the request — telemetry's SSE
// feed (docs/55 D4) — must end it at the token's `exp`, so Verify carries the
// expiry the signature check validated.
func TestVerifyCarriesTheTokenExpiry(t *testing.T) {
	iss := oidcauthtest.NewIssuer(t, "k1", testKey())
	v := newVerifier(t, testConfig(iss.URL()))
	waitFor(t, v.Resolved(), "discovery")

	exp := time.Now().Add(42 * time.Minute).Truncate(time.Second)
	raw := iss.Mint(fmt.Sprintf(`{"iss": %q, "aud": %q, "sub": "sub-1", "exp": %d}`,
		iss.URL(), testAudience, exp.Unix()))
	id, err := v.Verify(context.Background(), raw)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !id.Expiry.Equal(exp) {
		t.Errorf("Expiry = %v, want the token's exp %v", id.Expiry, exp)
	}
}
