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

type fakeIDP struct {
	srv  *httptest.Server
	priv *rsa.PrivateKey
	url  string
}

func newFakeIDP(t *testing.T) *fakeIDP {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	s := &oidctest.Server{PublicKeys: []oidctest.PublicKey{
		{PublicKey: priv.Public(), KeyID: testKeyID, Algorithm: oidc.RS256},
	}}
	srv := httptest.NewServer(s)
	s.SetIssuer(srv.URL)
	t.Cleanup(srv.Close)
	return &fakeIDP{srv: srv, priv: priv, url: srv.URL}
}

// token mints a JWT. iss/aud/exp default to the good values; roles is the
// Keycloak client-roles path this relay defaults to.
func (f *fakeIDP) token(iss, aud string, exp time.Time, rolesJSON string) string {
	claims := fmt.Sprintf(`{
		"iss": %q, "aud": %q, "sub": "operator@example.com",
		"exp": %s, "iat": %s,
		"resource_access": {%q: {"roles": %s}}
	}`, iss, aud,
		strconv.FormatInt(exp.Unix(), 10),
		strconv.FormatInt(time.Now().Add(-time.Minute).Unix(), 10),
		aud, rolesJSON)
	return oidctest.SignIDToken(f.priv, testKeyID, oidc.RS256, claims)
}

// goodToken is the happy path: right issuer, right audience, unexpired, with
// the operator role in the default claim path.
func (f *fakeIDP) goodToken() string {
	return f.token(f.url, testAud, time.Now().Add(time.Hour), `["operator"]`)
}

// oidcAuth builds an authenticator against the fake IdP and waits for the
// background refresher to cache its keys — so the request path is genuinely
// offline by the time any assertion runs.
func oidcAuth(t *testing.T, f *fakeIDP, extra func(*AdminAuthOptions)) *AdminAuth {
	t.Helper()
	opts := AdminAuthOptions{
		Issuer:          f.url,
		Audience:        testAud,
		RolesClaim:      config.DefaultAdminOIDCRolesClaim,
		Role:            config.DefaultAdminOIDCRole,
		Log:             discardLog,
		RefreshInterval: 20 * time.Millisecond,
	}
	if extra != nil {
		extra(&opts)
	}
	a := NewAdminAuth(t.Context(), opts)
	select {
	case <-a.jwksReady:
	case <-time.After(10 * time.Second):
		t.Fatal("the JWKS was never fetched from the fake issuer")
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

// THE OFFLINE GUARANTEE (docs/42 §4.5, §6): once the JWKS is cached,
// verification never touches the IdP. Proven by stopping the issuer's server
// outright and verifying again.
func TestAdminJWTVerifiesWithTheIssuerStopped(t *testing.T) {
	idp := newFakeIDP(t)
	auth := oidcAuth(t, idp, nil)
	h, _ := adminHandler(t, config.Config{AdminOIDCIssuer: idp.url, AdminOIDCAudience: testAud}, auth, nil)

	// Mint the token BEFORE the IdP dies — an operator's access token
	// outlives a provider outage by its own lifetime, which is the whole
	// reason validation must be offline.
	token := idp.goodToken()
	if w := get(h, "/internal/admin/broadcasts", token); w.Code != http.StatusOK {
		t.Fatalf("control: status = %d, want 200", w.Code)
	}

	idp.srv.Close()
	// Give the background refresher a few failed attempts, so the test also
	// proves a failing refresh does not throw the cached keys away.
	time.Sleep(100 * time.Millisecond)

	if w := get(h, "/internal/admin/broadcasts", token); w.Code != http.StatusOK {
		t.Fatalf("with the issuer down: status = %d, want 200 (verification must be offline)", w.Code)
	}
	// A token that was never good is still rejected — the cache is not a
	// bypass.
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
