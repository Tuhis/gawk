package ops

// R39 AP3 (docs/42 §4.5, §9): the credential-gated relay admin API.

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/coreos/go-oidc/v3/oidc/oidctest"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/internal/metrics"
)

const (
	adminToken = "s3cr3t-machine-token"
	testKeyID  = "test-key"
	testAud    = "gawk-admin"
)

// adminHandler builds the ops mux with the admin routes wired to a fresh
// registry, plus whatever auth the caller configured.
func adminHandler(t *testing.T, cfg config.Config, auth *AdminAuth,
	remote func(string) (netip.Addr, bool)) (http.Handler, *hub.Registry) {
	t.Helper()
	r := hub.NewRegistry(discardLog, hub.Options{})
	promReg := metrics.NewBaseRegistry("test-version")
	promReg.MustRegister(metrics.NewRegistryCollector(r))
	h := Handler(r, promReg, discardLog, nil, &AdminOptions{
		Registry:        r,
		Config:          cfg,
		Pod:             "gawk-server-abc123",
		Version:         "test-version",
		PublisherRemote: remote,
		Auth:            auth,
		Log:             discardLog,
	})
	return h, r
}

func get(h http.Handler, path, bearer string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	h.ServeHTTP(w, req)
	return w
}

func staticAuth(t *testing.T, token string) *AdminAuth {
	t.Helper()
	return NewAdminAuth(t.Context(), AdminAuthOptions{Token: token, Log: discardLog})
}

// THE SURFACE STAYS DARK. With no token and no issuer the routes are never
// registered, so a probe cannot even tell R39 shipped — 404, not 401
// (docs/42 §4.3).
func TestAdminRoutesAreDarkWithoutACredential(t *testing.T) {
	for _, tc := range []struct {
		name  string
		admin *AdminOptions
		auth  *AdminAuth
	}{
		{name: "nil AdminOptions"},
		{name: "no credential configured", auth: NewAdminAuth(t.Context(), AdminAuthOptions{Log: discardLog})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var h http.Handler
			if tc.auth == nil && tc.admin == nil {
				r := hub.NewRegistry(discardLog, hub.Options{})
				h = Handler(r, metrics.NewBaseRegistry("v"), discardLog, nil, nil)
			} else {
				h, _ = adminHandler(t, config.Config{}, tc.auth, nil)
			}
			for _, path := range []string{"/internal/admin/broadcasts", "/internal/admin/config"} {
				// Unauthenticated AND with a plausible credential: both 404.
				for _, bearer := range []string{"", adminToken} {
					if w := get(h, path, bearer); w.Code != http.StatusNotFound {
						t.Errorf("GET %s (bearer %q) = %d, want 404", path, bearer, w.Code)
					}
				}
			}
			// The rest of the ops surface is unaffected.
			if w := get(h, "/statusz", ""); w.Code != http.StatusOK {
				t.Errorf("/statusz = %d, want 200", w.Code)
			}
		})
	}
}

// The static token is the machine path gawk-admin uses. Every wrong shape is
// 401, and the compare is constant-time (subtle.ConstantTimeCompare) so the
// endpoint is not a byte-at-a-time oracle for the token.
func TestAdminStaticTokenAuth(t *testing.T) {
	h, r := adminHandler(t, config.Config{AdminAPIToken: adminToken}, staticAuth(t, adminToken), nil)
	if _, _, err := r.StartPublish(""); err != nil {
		t.Fatalf("StartPublish: %v", err)
	}

	if w := get(h, "/internal/admin/broadcasts", adminToken); w.Code != http.StatusOK {
		t.Fatalf("GET with the right token = %d, want 200", w.Code)
	}
	for _, tc := range []struct{ name, bearer string }{
		{"no header", ""},
		{"wrong token", "not-the-token"},
		{"right token with a trailing byte", adminToken + "x"},
		{"a prefix of the token", adminToken[:len(adminToken)-1]},
		{"empty credential", " "},
	} {
		w := get(h, "/internal/admin/broadcasts", tc.bearer)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", tc.name, w.Code)
		}
		if body := w.Body.String(); strings.Contains(body, adminToken) {
			t.Errorf("%s: the response echoed the configured token: %q", tc.name, body)
		}
	}

	// A malformed scheme is 401, not a panic or a 500.
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/internal/admin/broadcasts", nil)
	req.Header.Set("Authorization", "Basic "+adminToken)
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("Basic scheme = %d, want 401", w.Code)
	}
}

// The broadcasts response carries the RAW id (D8) AND the HMAC'd key, and the
// key must be exactly what ObfuscateID produces — that identity is what makes
// the portal's telemetry deep link land on the right broadcast without the
// portal ever holding -stats-key.
func TestAdminBroadcastsCarriesRawAndObfuscatedIDs(t *testing.T) {
	h, r := adminHandler(t, config.Config{AdminAPIToken: adminToken}, staticAuth(t, adminToken),
		func(string) (netip.Addr, bool) { return netip.MustParseAddr("203.0.113.7"), true })
	id, pub, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	defer pub.Close()

	w := get(h, "/internal/admin/broadcasts", adminToken)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var got struct {
		Schema     string               `json:"schema"`
		Pod        string               `json:"pod"`
		Broadcasts []hub.AdminBroadcast `json:"broadcasts"`
	}
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Schema != SchemaAdminBroadcasts {
		t.Errorf("schema = %q, want %q", got.Schema, SchemaAdminBroadcasts)
	}
	if got.Pod != "gawk-server-abc123" {
		t.Errorf("pod = %q, want the injected pod name", got.Pod)
	}
	if len(got.Broadcasts) != 1 {
		t.Fatalf("broadcasts = %d, want 1", len(got.Broadcasts))
	}
	b := got.Broadcasts[0]
	if b.ID != id {
		t.Errorf("id = %q, want the raw %q", b.ID, id)
	}
	if b.Key != r.ObfuscateID(id) {
		t.Errorf("key = %q, want ObfuscateID(%q) = %q", b.Key, id, r.ObfuscateID(id))
	}
	if b.Key == b.ID {
		t.Error("key and id are identical: the obfuscation is not applied")
	}
	if !b.PublisherActive || b.Role != "origin" {
		t.Errorf("publisherActive/role = %v/%q, want true/origin", b.PublisherActive, b.Role)
	}
	if b.PublisherRemoteIP != "203.0.113.7" {
		t.Errorf("publisherRemoteIp = %q, want 203.0.113.7", b.PublisherRemoteIP)
	}
	if b.StartedAt.IsZero() {
		t.Error("startedAt is zero")
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store — the body carries joinable IDs",
			w.Header().Get("Cache-Control"))
	}
}

// THE INVARIANT THAT MUST NOT MOVE (docs/42 §5, D8): /statusz stays HMAC-only
// and byte-identical, on BOTH listeners — the TCP ops mux and the H3 route
// share one handler, and this asserts that the R39 additions changed neither.
func TestStatuszStaysByteIdenticalOnBothListeners(t *testing.T) {
	r := hub.NewRegistry(discardLog, hub.Options{})
	id, pub, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	defer pub.Close()

	// The TCP ops listener, WITH the admin API enabled...
	promReg := metrics.NewBaseRegistry("test-version")
	promReg.MustRegister(metrics.NewRegistryCollector(r))
	withAdmin := Handler(r, promReg, discardLog, nil, &AdminOptions{
		Registry: r, Config: config.Config{AdminAPIToken: adminToken},
		Auth: staticAuth(t, adminToken), Log: discardLog,
	})
	// ...the same listener with it disabled...
	promReg2 := metrics.NewBaseRegistry("test-version")
	promReg2.MustRegister(metrics.NewRegistryCollector(r))
	withoutAdmin := Handler(r, promReg2, discardLog, nil, nil)
	// ...and the H3 route, which is the bare shared handler.
	h3 := StatuszHandler(r, discardLog)

	bodies := map[string]string{}
	for name, h := range map[string]http.Handler{
		"ops listener with admin enabled":  withAdmin,
		"ops listener with admin disabled": withoutAdmin,
		"h3 route":                         http.HandlerFunc(h3),
	} {
		w := get(h, "/statusz", "")
		if w.Code != http.StatusOK {
			t.Fatalf("%s: /statusz = %d, want 200", name, w.Code)
		}
		bodies[name] = w.Body.String()
	}
	var first string
	for name, body := range bodies {
		if first == "" {
			first = body
			continue
		}
		if body != first {
			t.Errorf("%s: /statusz body differs from the others:\n%s\nvs\n%s", name, body, first)
		}
	}
	// And the raw ID is still nowhere in it.
	if strings.Contains(first, id) {
		t.Errorf("/statusz leaked the raw broadcast ID %q:\n%s", id, first)
	}
	if !strings.Contains(first, r.ObfuscateID(id)) {
		t.Errorf("/statusz is not keyed by ObfuscateID:\n%s", first)
	}
}

// The config route serves the redacted view and nothing else. The
// leak-proofing itself is config's own acceptance gate
// (config.TestSanitizedLeaksNoSecretValue); this checks the wiring.
func TestAdminConfigRoute(t *testing.T) {
	cfg := config.Config{
		Addr: ":4433", ClusterMode: true, MaxBroadcasts: 200,
		AdminAPIToken: adminToken, PublishSecret: "SENTINEL-publish-xyzzy",
		InternalPSK: "SENTINEL-psk-xyzzy",
	}
	h, _ := adminHandler(t, cfg, staticAuth(t, adminToken), nil)

	w := get(h, "/internal/admin/config", adminToken)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "xyzzy") {
		t.Fatalf("the config route leaked a secret:\n%s", body)
	}
	var got struct {
		Schema  string         `json:"schema"`
		Pod     string         `json:"pod"`
		Version string         `json:"version"`
		Config  map[string]any `json:"config"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Schema != SchemaAdminConfig {
		t.Errorf("schema = %q, want %q", got.Schema, SchemaAdminConfig)
	}
	if got.Version != "test-version" || got.Pod != "gawk-server-abc123" {
		t.Errorf("pod/version = %q/%q, want the injected values", got.Pod, got.Version)
	}
	if got.Config["publishSecret"] != "<set>" || got.Config["adminApiToken"] != "<set>" {
		t.Errorf("secrets not redacted: %v", got.Config)
	}
	if got.Config["addr"] != ":4433" || got.Config["clusterMode"] != true {
		t.Errorf("non-secret config missing: %v", got.Config)
	}
}

// KILL/BAN VERBS ARE NOT EXPOSED HERE (docs/42 D2). Anything but GET on the
// admin paths must not be routed — the Ban CR is the single write path.
func TestAdminAPIExposesNoWriteVerbs(t *testing.T) {
	h, _ := adminHandler(t, config.Config{AdminAPIToken: adminToken}, staticAuth(t, adminToken), nil)
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		for _, path := range []string{
			"/internal/admin/broadcasts", "/internal/admin/config",
			"/internal/admin/broadcasts/ABC23Z/kill", "/internal/admin/bans",
		} {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(method, path, nil)
			req.Header.Set("Authorization", "Bearer "+adminToken)
			h.ServeHTTP(w, req)
			if w.Code != http.StatusNotFound && w.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s %s = %d, want 404/405 — no write path may exist here", method, path, w.Code)
			}
		}
	}
}

// --- the JWT path, against a fake issuer -------------------------------

// rotationKey is the key every rotation test rotates TO. Shared because RSA
// keygen is the slowest thing in this file; each fakeIDP still starts with a
// key of its own, so "signed by another issuer's key" stays a real forgery.
var rotationKey = sync.OnceValue(newRSAKey)

func newRSAKey() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic("generating test key: " + err.Error())
	}
	return k
}

type fakeIDP struct {
	srv *httptest.Server
	// priv is the key the issuer STARTS with. Never reassigned, so the tests
	// that never rotate can read it directly.
	priv *rsa.PrivateKey
	url  string

	// keyFetches counts JWKS requests that reached the wire. A test that
	// stops the server and still verifies is proving this did not move.
	keyFetches atomic.Int64

	mu      sync.Mutex
	handler *oidctest.Server // replaced wholesale on rotation, never mutated
	signer  *rsa.PrivateKey
	kid     string
	delay   time.Duration
}

func newFakeIDP(t *testing.T) *fakeIDP {
	t.Helper()
	priv := newRSAKey()
	f := &fakeIDP{priv: priv}
	f.srv = httptest.NewServer(f)
	f.url = f.srv.URL
	f.useKey(testKeyID, priv)
	t.Cleanup(f.srv.Close) // idempotent; tests that stop the issuer early call it too
	return f
}

func (f *fakeIDP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	h, delay := f.handler, f.delay
	f.mu.Unlock()
	if r.URL.Path == "/keys" {
		f.keyFetches.Add(1)
		if delay > 0 {
			// A JWKS that takes a human-visible moment, so a herd of
			// concurrent verifications demonstrably overlaps inside one fetch
			// rather than racing through it one at a time.
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}
	}
	h.ServeHTTP(w, r)
}

// useKey makes kid/key the only key the issuer advertises and signs with — a
// hard rotation, which is the case a cached verifier is most likely to get
// wrong.
func (f *fakeIDP) useKey(kid string, key *rsa.PrivateKey) {
	h := &oidctest.Server{PublicKeys: []oidctest.PublicKey{
		{PublicKey: key.Public(), KeyID: kid, Algorithm: oidc.RS256},
	}}
	h.SetIssuer(f.srv.URL)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handler = h
	f.signer = key
	f.kid = kid
}

// delayKeys makes every subsequent JWKS request take d.
func (f *fakeIDP) delayKeys(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delay = d
}

// token mints a JWT with the issuer's CURRENT key. iss/aud/exp default to the
// good values; roles is the Keycloak client-roles path this relay defaults to.
func (f *fakeIDP) token(iss, aud string, exp time.Time, rolesJSON string) string {
	f.mu.Lock()
	key, kid := f.signer, f.kid
	f.mu.Unlock()
	return f.tokenWith(key, kid, iss, aud, exp, rolesJSON)
}

// tokenWith signs with an arbitrary key — a rotated one, or an attacker's.
func (f *fakeIDP) tokenWith(key *rsa.PrivateKey, kid, iss, aud string, exp time.Time, rolesJSON string) string {
	claims := fmt.Sprintf(`{
		"iss": %q, "aud": %q, "sub": "operator@example.com",
		"exp": %s, "iat": %s,
		"resource_access": {%q: {"roles": %s}}
	}`, iss, aud,
		strconv.FormatInt(exp.Unix(), 10),
		strconv.FormatInt(time.Now().Add(-time.Minute).Unix(), 10),
		aud, rolesJSON)
	return oidctest.SignIDToken(key, kid, oidc.RS256, claims)
}

// goodToken is the happy path: right issuer, right audience, unexpired, with
// the operator role in the default claim path.
func (f *fakeIDP) goodToken() string {
	return f.token(f.url, testAud, time.Now().Add(time.Hour), `["operator"]`)
}

// unverifiable mints a well-formed token this relay can never accept: a key
// the issuer has never advertised, under a `kid` nothing has cached. Every one
// of them is a key-cache miss, which is what makes go-oidc reach for the
// network.
func (f *fakeIDP) unverifiable(kid string) string {
	return f.tokenWith(rotationKey(), kid, f.url, testAud, time.Now().Add(time.Hour), `["operator"]`)
}

// countingTransport counts every HTTP attempt, reached or refused. The
// issuer's own counter cannot see a request to a stopped server, so a "verify
// offline" test built on it alone would pass against an implementation that
// tries the IdP every time and shrugs off the error — which is exactly the
// behaviour that guarantee forbids (a connect timeout on every request while
// the IdP is away). This sees the attempt.
type countingTransport struct {
	base     http.RoundTripper
	attempts atomic.Int64
}

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.attempts.Add(1)
	return c.base.RoundTrip(r)
}

func countingClient() (*http.Client, *countingTransport) {
	tr := &countingTransport{base: http.DefaultTransport}
	return &http.Client{Transport: tr, Timeout: 10 * time.Second}, tr
}

// testClock is a hand-wound clock: the fetch bucket's refill is a property of
// the bucket, not of the machine running the test, so nothing here sleeps.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

// Anchored at real "now" so tokens minted with real timestamps still validate.
func newTestClock() *testClock { return &testClock{t: time.Now()} }

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// oidcAuth builds an authenticator against the fake IdP and waits for OIDC
// discovery, so the JWT path is live by the time any assertion runs. The JWKS
// itself is fetched lazily, on the first token that needs it.
func oidcAuth(t *testing.T, f *fakeIDP, extra func(*AdminAuthOptions)) *AdminAuth {
	t.Helper()
	opts := AdminAuthOptions{
		Issuer:     f.url,
		Audience:   testAud,
		RolesClaim: config.DefaultAdminOIDCRolesClaim,
		Role:       config.DefaultAdminOIDCRole,
		Log:        discardLog,
		// The JWKS fetch bucket has its own tests below. Everywhere else it
		// must not be load-bearing: how many of a test's cases happen to miss
		// the key cache — a forged signature does, a wrong `aud` does not —
		// would otherwise silently decide whether that test passes.
		JWKSFetchBurst: 1_000,
	}
	if extra != nil {
		extra(&opts)
	}
	a := NewAdminAuth(t.Context(), opts)
	select {
	case <-a.resolved:
	case <-time.After(10 * time.Second):
		t.Fatal("OIDC discovery never resolved against the fake issuer")
	}
	return a
}

func TestAdminJWTAuthAcceptsAnOperatorToken(t *testing.T) {
	idp := newFakeIDP(t)
	h, r := adminHandler(t, config.Config{AdminOIDCIssuer: idp.url, AdminOIDCAudience: testAud},
		oidcAuth(t, idp, nil), nil)
	if _, _, err := r.StartPublish(""); err != nil {
		t.Fatalf("StartPublish: %v", err)
	}

	if w := get(h, "/internal/admin/broadcasts", idp.goodToken()); w.Code != http.StatusOK {
		t.Fatalf("a valid operator JWT = %d, want 200 (body %q)", w.Code, w.Body.String())
	}
	if w := get(h, "/internal/admin/config", idp.goodToken()); w.Code != http.StatusOK {
		t.Errorf("/internal/admin/config with a valid operator JWT = %d, want 200", w.Code)
	}
}

// Every way a token can be wrong, and the status each must produce. 401 for
// "this is not a token I accept"; 403 — and only 403 — for "a token I accept,
// from someone who is not an operator".
func TestAdminJWTAuthRejections(t *testing.T) {
	idp := newFakeIDP(t)
	other := newFakeIDP(t)
	h, _ := adminHandler(t, config.Config{AdminOIDCIssuer: idp.url, AdminOIDCAudience: testAud},
		oidcAuth(t, idp, nil), nil)

	// A token from a DIFFERENT issuer, signed by that issuer's key.
	foreign := other.token(other.url, testAud, time.Now().Add(time.Hour), `["operator"]`)
	// A token claiming our issuer but signed by the wrong key.
	forged := other.token(idp.url, testAud, time.Now().Add(time.Hour), `["operator"]`)

	for _, tc := range []struct {
		name  string
		token string
		want  int
	}{
		{"wrong audience", idp.token(idp.url, "some-other-client", time.Now().Add(time.Hour), `["operator"]`), http.StatusUnauthorized},
		{"wrong issuer", foreign, http.StatusUnauthorized},
		{"forged signature", forged, http.StatusUnauthorized},
		{"expired", idp.token(idp.url, testAud, time.Now().Add(-time.Hour), `["operator"]`), http.StatusUnauthorized},
		{"not a jwt at all", "not-a-jwt", http.StatusUnauthorized},
		{"valid token without the role", idp.token(idp.url, testAud, time.Now().Add(time.Hour), `["viewer"]`), http.StatusForbidden},
		{"valid token with an empty roles array", idp.token(idp.url, testAud, time.Now().Add(time.Hour), `[]`), http.StatusForbidden},
		{"valid token with no roles claim at all", noRolesToken(idp), http.StatusForbidden},
		{"valid token whose roles claim is not an array", idp.token(idp.url, testAud, time.Now().Add(time.Hour), `{"nested":"object"}`), http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := get(h, "/internal/admin/broadcasts", tc.token)
			if w.Code != tc.want {
				t.Errorf("status = %d, want %d (body %q)", w.Code, tc.want, w.Body.String())
			}
		})
	}
}

// noRolesToken mints a structurally valid token with no resource_access claim
// — the "missing/malformed claim → 403, never 500" case.
func noRolesToken(f *fakeIDP) string {
	claims := fmt.Sprintf(`{"iss": %q, "aud": %q, "sub": "nobody", "exp": %s}`,
		f.url, testAud, strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
	return oidctest.SignIDToken(f.priv, testKeyID, oidc.RS256, claims)
}

// --- the JWKS cache and its rate floor (auth.go) ------------------------

// The two numbers the doc comment promises an operator: three fetches back to
// back from cold, then one per twenty seconds.
func TestJWKSThrottleDefaultsAreThreeFetchesPerMinute(t *testing.T) {
	if defaultJWKSFetchBurst != 3 {
		t.Errorf("defaultJWKSFetchBurst = %d, want 3", defaultJWKSFetchBurst)
	}
	if defaultJWKSFetchInterval != 20*time.Second {
		t.Errorf("defaultJWKSFetchInterval = %v, want 20s (three per minute)", defaultJWKSFetchInterval)
	}
}

// The bucket starts FULL, so a key rotation on an otherwise idle relay gets
// its fetch immediately and costs zero 401s.
func TestJWKSThrottleStartsFullAndRefillsOnePerInterval(t *testing.T) {
	clk := newTestClock()
	th := newJWKSThrottle(defaultJWKSFetchInterval, defaultJWKSFetchBurst, clk.now)

	for i := range defaultJWKSFetchBurst {
		if !th.allow() {
			t.Fatalf("fetch %d refused from a full bucket", i+1)
		}
	}
	if th.allow() {
		t.Fatal("the bucket handed out more than its burst without time passing")
	}

	// One tick short of the interval is still refused; the interval exactly is
	// allowed. This is the worst-case rotation delay the comment claims.
	clk.advance(defaultJWKSFetchInterval - time.Nanosecond)
	if th.allow() {
		t.Fatal("a token accrued before the refill interval elapsed")
	}
	clk.advance(time.Nanosecond)
	if !th.allow() {
		t.Fatalf("no token after a full %v refill interval", defaultJWKSFetchInterval)
	}

	// And it never accrues past the burst, however long it idles.
	clk.advance(24 * time.Hour)
	for i := range defaultJWKSFetchBurst {
		if !th.allow() {
			t.Fatalf("fetch %d refused after a long idle", i+1)
		}
	}
	if th.allow() {
		t.Fatal("the bucket accumulated past its burst while idle")
	}
}

// Zero and negative values are the caller asking for the default, never for an
// unthrottled or a permanently locked bucket.
func TestJWKSThrottleZeroOptionsMeanTheDefaults(t *testing.T) {
	for _, th := range []*jwksThrottle{
		newJWKSThrottle(0, 0, nil),
		newJWKSThrottle(-time.Second, -4, nil),
	} {
		if th.burst != float64(defaultJWKSFetchBurst) || th.interval != defaultJWKSFetchInterval {
			t.Errorf("burst %v interval %v, want the defaults", th.burst, th.interval)
		}
	}
}

func TestJWKSThrottleIsSafeUnderConcurrentUse(t *testing.T) {
	th := newJWKSThrottle(time.Hour, 5, time.Now)
	var wg sync.WaitGroup
	granted := make([]bool, 64)
	for i := range granted {
		wg.Add(1)
		go func() {
			defer wg.Done()
			granted[i] = th.allow()
		}()
	}
	wg.Wait()
	n := 0
	for _, ok := range granted {
		if ok {
			n++
		}
	}
	if n != 5 {
		t.Errorf("granted %d tokens concurrently, want exactly the burst (5)", n)
	}
}

// THE THROTTLE BITES. A caller feeding tokens signed by keys the issuer never
// advertised misses the key cache every single time, which is what makes
// go-oidc reach for the network. The bucket caps that at its burst; the rest
// are refused inside this process. Every one of them is a 401 — never a 5xx,
// and never an accepted token.
func TestAdminJWTUnverifiableTokensCannotFetchMoreThanTheBucket(t *testing.T) {
	idp := newFakeIDP(t)
	clk := newTestClock()
	auth := oidcAuth(t, idp, func(o *AdminAuthOptions) {
		o.Now = clk.now
		// Production defaults, pinned so the test is about them.
		o.JWKSFetchInterval = defaultJWKSFetchInterval
		o.JWKSFetchBurst = defaultJWKSFetchBurst
	})
	h, _ := adminHandler(t, config.Config{AdminOIDCIssuer: idp.url, AdminOIDCAudience: testAud}, auth, nil)

	const attempts = 25
	for i := range attempts {
		w := get(h, "/internal/admin/broadcasts", idp.unverifiable("forged-kid-"+strconv.Itoa(i)))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("request %d: status = %d, want 401 (body %q)", i, w.Code, w.Body.String())
		}
	}
	if got := idp.keyFetches.Load(); got != int64(defaultJWKSFetchBurst) {
		t.Errorf("JWKS fetches = %d for %d unverifiable tokens, want exactly the burst (%d)",
			got, attempts, defaultJWKSFetchBurst)
	}
}

// The other half of the same story: a REAL rotation lands, the bucket has a
// token, and nobody is refused. One fetch serves the whole shift, and the
// retired key stops working — the cache was replaced, not accumulated.
func TestAdminJWTRotationIsPickedUpOnOneFetch(t *testing.T) {
	idp := newFakeIDP(t)
	clk := newTestClock()
	auth := oidcAuth(t, idp, func(o *AdminAuthOptions) {
		o.Now = clk.now
		o.JWKSFetchInterval = defaultJWKSFetchInterval
		o.JWKSFetchBurst = defaultJWKSFetchBurst
	})
	h, _ := adminHandler(t, config.Config{AdminOIDCIssuer: idp.url, AdminOIDCAudience: testAud}, auth, nil)

	// Warm-up: the lazy first fetch, and so far the only one.
	if w := get(h, "/internal/admin/broadcasts", idp.goodToken()); w.Code != http.StatusOK {
		t.Fatalf("warm-up = %d, want 200 (body %q)", w.Code, w.Body.String())
	}
	if got := idp.keyFetches.Load(); got != 1 {
		t.Fatalf("JWKS fetches after warm-up = %d, want 1", got)
	}

	idp.useKey("rotated-key", rotationKey())
	for i := range 8 {
		if w := get(h, "/internal/admin/broadcasts", idp.goodToken()); w.Code != http.StatusOK {
			t.Fatalf("post-rotation request %d = %d, want 200 (body %q)", i, w.Code, w.Body.String())
		}
	}
	if got := idp.keyFetches.Load(); got != 2 {
		t.Errorf("JWKS fetches = %d, want 2 (the warm-up and one for the rotation)", got)
	}

	stale := idp.tokenWith(idp.priv, testKeyID, idp.url, testAud, time.Now().Add(time.Hour), `["operator"]`)
	if w := get(h, "/internal/admin/broadcasts", stale); w.Code != http.StatusUnauthorized {
		t.Errorf("retired key = %d, want 401", w.Code)
	}
}

// THE WORST CASE, pinned: a rotation that lands while an attacker has already
// emptied the bucket waits one refill interval — 20 seconds at the defaults —
// and then goes through. Not longer, and not a permanent lockout.
func TestAdminJWTRotationDuringAnAttackWaitsOneRefillInterval(t *testing.T) {
	idp := newFakeIDP(t)
	clk := newTestClock()
	auth := oidcAuth(t, idp, func(o *AdminAuthOptions) {
		o.Now = clk.now
		o.JWKSFetchInterval = defaultJWKSFetchInterval
		o.JWKSFetchBurst = defaultJWKSFetchBurst
	})
	h, _ := adminHandler(t, config.Config{AdminOIDCIssuer: idp.url, AdminOIDCAudience: testAud}, auth, nil)

	for i := range defaultJWKSFetchBurst + 5 {
		if w := get(h, "/internal/admin/broadcasts", idp.unverifiable("forged-kid-"+strconv.Itoa(i))); w.Code != http.StatusUnauthorized {
			t.Fatalf("drain %d: status = %d, want 401", i, w.Code)
		}
	}

	// Now the IdP rotates for real. The operator's next token is signed by a
	// key nothing here has, and there is no budget to go and get it.
	idp.useKey("rotated-key", rotationKey())
	rotated := idp.goodToken()
	fetches := idp.keyFetches.Load()
	if w := get(h, "/internal/admin/broadcasts", rotated); w.Code != http.StatusUnauthorized {
		t.Fatalf("status with an empty bucket = %d, want 401", w.Code)
	}
	if got := idp.keyFetches.Load(); got != fetches {
		t.Errorf("a throttled verification still reached the IdP (%d fetches, was %d)", got, fetches)
	}

	// Still refused a hair short of the interval...
	clk.advance(defaultJWKSFetchInterval - time.Second)
	if w := get(h, "/internal/admin/broadcasts", rotated); w.Code != http.StatusUnauthorized {
		t.Fatalf("status before the refill = %d, want 401", w.Code)
	}
	// ...and in, on one fetch, once it has elapsed.
	clk.advance(time.Second)
	if w := get(h, "/internal/admin/broadcasts", rotated); w.Code != http.StatusOK {
		t.Fatalf("status after %v = %d, want 200 (body %q)", defaultJWKSFetchInterval, w.Code, w.Body.String())
	}
	if got := idp.keyFetches.Load(); got != fetches+1 {
		t.Errorf("JWKS fetches = %d, want %d: the refill buys exactly one", got, fetches+1)
	}
}

// A rotation is a thundering herd: every operator's next token is signed by a
// key this pod has not seen, and they arrive at once. All of them must be
// accepted off the single fetch the first one triggers — go-oidc's own
// singleflight, asserted here so a future upstream change that loses it is
// caught in this repository rather than at the IdP.
func TestAdminJWTConcurrentUnknownKeyRequestsShareOneFetch(t *testing.T) {
	idp := newFakeIDP(t)
	clk := newTestClock()
	auth := oidcAuth(t, idp, func(o *AdminAuthOptions) {
		o.Now = clk.now
		// Production-shaped: the bucket holds three, and the herd must cost
		// one. If the coalescing were lost, sixteen requests would want
		// sixteen fetches and thirteen of them would be refused.
		o.JWKSFetchInterval = defaultJWKSFetchInterval
		o.JWKSFetchBurst = defaultJWKSFetchBurst
	})
	h, _ := adminHandler(t, config.Config{AdminOIDCIssuer: idp.url, AdminOIDCAudience: testAud}, auth, nil)

	if w := get(h, "/internal/admin/broadcasts", idp.goodToken()); w.Code != http.StatusOK {
		t.Fatalf("warm-up = %d, want 200", w.Code)
	}

	idp.useKey("rotated-key", rotationKey())
	tokens := make([]string, 16)
	for i := range tokens {
		tokens[i] = idp.goodToken()
	}
	// A JWKS that takes a visible moment, so "they arrive at once" is a fact
	// about the test rather than a hope about the scheduler.
	idp.delayKeys(150 * time.Millisecond)
	before := idp.keyFetches.Load()

	var wg sync.WaitGroup
	codes := make([]int, len(tokens))
	start := make(chan struct{})
	for i, token := range tokens {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			codes[i] = get(h, "/internal/admin/broadcasts", token).Code
		}()
	}
	close(start)
	wg.Wait()

	for i, code := range codes {
		if code != http.StatusOK {
			t.Errorf("request %d = %d, want 200: the rotated key was fetched for the whole herd", i, code)
		}
	}
	if got := idp.keyFetches.Load() - before; got != 1 {
		t.Errorf("JWKS fetches during the herd = %d, want 1 (go-oidc coalesces concurrent misses)", got)
	}
}

// THE OFFLINE GUARANTEE (docs/42 §4.5, §6): once a key is cached, verifying a
// token it signed never touches the IdP. Proven by stopping the issuer's
// server outright and verifying again — and by watching the HTTP transport,
// which sees an attempt even when there is nothing at the other end.
func TestAdminJWTVerifiesWithTheIssuerStopped(t *testing.T) {
	idp := newFakeIDP(t)
	client, transport := countingClient()
	clk := newTestClock()
	auth := oidcAuth(t, idp, func(o *AdminAuthOptions) {
		o.HTTPClient = client
		o.Now = clk.now
	})
	h, _ := adminHandler(t, config.Config{AdminOIDCIssuer: idp.url, AdminOIDCAudience: testAud}, auth, nil)

	// Mint the token BEFORE the IdP dies — an operator's access token
	// outlives a provider outage by its own lifetime, which is the whole
	// reason validation must be offline.
	token := idp.goodToken()
	if w := get(h, "/internal/admin/broadcasts", token); w.Code != http.StatusOK {
		t.Fatalf("control: status = %d, want 200 (body %q)", w.Code, w.Body.String())
	}
	if idp.keyFetches.Load() == 0 {
		t.Fatal("the JWKS was never fetched: the test is not proving anything about a cache")
	}

	idp.srv.Close()
	attempts := transport.attempts.Load()
	tokensLeft := auth.throttleTokensLeft()

	if w := get(h, "/internal/admin/broadcasts", token); w.Code != http.StatusOK {
		t.Fatalf("with the issuer down: status = %d, want 200 (verification must be offline)", w.Code)
	}
	// Not "the fetch failed harmlessly" — no attempt at all. An attempt would
	// mean a connect timeout on every admin request while the IdP is away.
	if got := transport.attempts.Load(); got != attempts {
		t.Errorf("HTTP attempts = %d, want %d: a cached verification must not touch the IdP", got, attempts)
	}
	// The fetch throttle sits in FRONT of that transport, so an unchanged,
	// non-empty bucket proves the fetch path was not even consulted.
	if got := auth.throttleTokensLeft(); got != tokensLeft || got == 0 {
		t.Errorf("fetch tokens = %v, want %v (and non-zero)", got, tokensLeft)
	}
	// A token that was never good is still rejected — the cache is not a
	// bypass, and a dead IdP does not turn a 401 into a 500.
	if w := get(h, "/internal/admin/broadcasts", "garbage"); w.Code != http.StatusUnauthorized {
		t.Errorf("garbage credential with the issuer down = %d, want 401", w.Code)
	}
}

// THE RELAY STARTS WITHOUT THE IdP (docs/42 §6). Discovery is resolved in the
// background, so an unreachable provider costs 401s, never a startup failure
// and never a 500.
func TestAdminAuthSurvivesAnUnreachableIssuer(t *testing.T) {
	// A server that exists only long enough to hand out a URL nothing
	// answers on: discovery can never succeed against it.
	dead := httptest.NewServer(http.NewServeMux())
	deadURL := dead.URL
	dead.Close()

	auth := NewAdminAuth(t.Context(), AdminAuthOptions{
		Issuer: deadURL, Audience: testAud,
		RolesClaim: config.DefaultAdminOIDCRolesClaim,
		Role:       config.DefaultAdminOIDCRole,
		Log:        discardLog,
	})
	if !auth.Configured() {
		t.Fatal("an OIDC-configured auth reports no credential")
	}
	h, _ := adminHandler(t, config.Config{AdminOIDCIssuer: deadURL, AdminOIDCAudience: testAud}, auth, nil)

	// The route EXISTS (a credential is configured) and answers 401 — not
	// 404, not 500, not a hang.
	w := get(h, "/internal/admin/broadcasts", "any.jwt.here")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status with an unreachable IdP = %d, want 401", w.Code)
	}
}

// Both credentials at once: the machine token and an operator JWT open the
// same door, which is what lets gawk-admin scrape while a human hits the
// endpoint directly with the portal's identity.
func TestAdminBothCredentialsWorkTogether(t *testing.T) {
	idp := newFakeIDP(t)
	auth := oidcAuth(t, idp, func(o *AdminAuthOptions) { o.Token = adminToken })
	h, _ := adminHandler(t, config.Config{
		AdminAPIToken: adminToken, AdminOIDCIssuer: idp.url, AdminOIDCAudience: testAud,
	}, auth, nil)

	if w := get(h, "/internal/admin/broadcasts", adminToken); w.Code != http.StatusOK {
		t.Errorf("static token = %d, want 200", w.Code)
	}
	if w := get(h, "/internal/admin/broadcasts", idp.goodToken()); w.Code != http.StatusOK {
		t.Errorf("operator JWT = %d, want 200", w.Code)
	}
	if w := get(h, "/internal/admin/broadcasts", "neither"); w.Code != http.StatusUnauthorized {
		t.Errorf("neither credential = %d, want 401", w.Code)
	}
}

// A custom roles-claim dot-path is resolved, including a top-level claim —
// not every IdP is Keycloak.
func TestAdminJWTCustomRolesClaimPath(t *testing.T) {
	idp := newFakeIDP(t)
	auth := oidcAuth(t, idp, func(o *AdminAuthOptions) {
		o.RolesClaim = "groups"
		o.Role = "gawk-operators"
	})
	h, _ := adminHandler(t, config.Config{AdminOIDCIssuer: idp.url, AdminOIDCAudience: testAud}, auth, nil)

	mint := func(groups string) string {
		claims := fmt.Sprintf(`{"iss": %q, "aud": %q, "sub": "x", "exp": %s, "groups": %s}`,
			idp.url, testAud, strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10), groups)
		return oidctest.SignIDToken(idp.priv, testKeyID, oidc.RS256, claims)
	}
	if w := get(h, "/internal/admin/broadcasts", mint(`["gawk-operators"]`)); w.Code != http.StatusOK {
		t.Errorf("top-level roles claim = %d, want 200", w.Code)
	}
	// A single role rendered as a bare string is accepted too.
	if w := get(h, "/internal/admin/broadcasts", mint(`"gawk-operators"`)); w.Code != http.StatusOK {
		t.Errorf("bare-string role = %d, want 200", w.Code)
	}
	if w := get(h, "/internal/admin/broadcasts", mint(`["someone-else"]`)); w.Code != http.StatusForbidden {
		t.Errorf("wrong group = %d, want 403", w.Code)
	}
}

// The default claim path substitutes {audience}, so one flag covers every
// Keycloak client without the operator writing the path out.
func TestAdminJWTDefaultClaimPathSubstitutesAudience(t *testing.T) {
	idp := newFakeIDP(t)
	auth := oidcAuth(t, idp, nil)
	// The token's roles live under resource_access.<audience>.roles, which is
	// only reachable if "{audience}" was replaced.
	if got, _ := auth.authorize(bearerReq(idp.goodToken())); got != http.StatusOK {
		t.Fatalf("authorize = %d, want 200 — {audience} was not substituted", got)
	}
	// A token whose roles sit under a DIFFERENT client is not ours.
	claims := fmt.Sprintf(`{"iss": %q, "aud": %q, "sub": "x", "exp": %s,
		"resource_access": {"some-other-client": {"roles": ["operator"]}}}`,
		idp.url, testAud, strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
	tok := oidctest.SignIDToken(idp.priv, testKeyID, oidc.RS256, claims)
	if got, _ := auth.authorize(bearerReq(tok)); got != http.StatusForbidden {
		t.Errorf("another client's roles = %d, want 403", got)
	}
}

func bearerReq(token string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/internal/admin/broadcasts", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	return req.WithContext(context.Background())
}
