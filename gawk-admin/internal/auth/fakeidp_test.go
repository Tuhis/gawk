package auth

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/coreos/go-oidc/v3/oidc/oidctest"
)

// The fake OIDC issuer AP5's acceptance criteria call for: a real HTTP server
// serving /.well-known/openid-configuration and a JWKS, with locally generated
// keys we mint tokens against. oidctest supplies the discovery/JWKS rendering
// and the signer; the wrapper adds the two things the criteria need and
// oidctest cannot do — swapping the advertised key set at runtime (rotation)
// without racing a concurrent request, and counting JWKS fetches so a test can
// prove a verification did NOT touch the network.

const (
	testClientID = "gawk-admin"
	testAudience = "gawk-admin-api"
	testOperator = "operator"
	testSubject  = "5f2b1a3c-user"
	testEmail    = "operator@example.test"
)

// RSA keygen is the slowest thing in this file, so the suite shares two keys:
// one the issuer starts with and one it rotates to.
var (
	keyA = sync.OnceValue(newRSAKey)
	keyB = sync.OnceValue(newRSAKey)
)

func newRSAKey() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic("generating test key: " + err.Error())
	}
	return k
}

type fakeIDP struct {
	srv *httptest.Server

	// keyFetches counts JWKS requests. A test that stops the server and still
	// verifies is proving this counter did not move.
	keyFetches atomic.Int64

	mu       sync.Mutex
	handler  *oidctest.Server // replaced wholesale on rotation; never mutated in place
	signer   *rsa.PrivateKey
	kid      string
	hangCh   chan struct{} // when non-nil, /keys blocks on it
	down     bool          // when true, every endpoint 503s
	keyDelay time.Duration // when non-zero, /keys sleeps this long
}

func newFakeIDP(t *testing.T) *fakeIDP {
	t.Helper()
	f := &fakeIDP{}
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close) // idempotent; tests that stop the issuer early may call it too
	f.useKey("key-a", keyA())
	return f
}

func (f *fakeIDP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	h, hang, down, delay := f.handler, f.hangCh, f.down, f.keyDelay
	f.mu.Unlock()
	if down {
		// An IdP that is up enough to answer but not to serve — the shape a
		// restarting Keycloak has, and the one that must not crash this pod.
		http.Error(w, "identity provider unavailable", http.StatusServiceUnavailable)
		return
	}
	if r.URL.Path == "/keys" {
		f.keyFetches.Add(1)
		if delay > 0 {
			// A JWKS that takes a human-visible moment to answer, so a herd of
			// concurrent verifications demonstrably overlaps inside one fetch
			// instead of racing through it one at a time.
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}
		if hang != nil {
			// An IdP that accepts the connection and then says nothing — the
			// shape that turns "blocks on the IdP" into a stalled request or a
			// stalled shutdown, rather than a clean error.
			select {
			case <-hang:
			case <-r.Context().Done():
				return
			}
		}
	}
	h.ServeHTTP(w, r)
}

// setDown toggles whether the issuer answers at all.
func (f *fakeIDP) setDown(down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.down = down
}

// delayKeys makes every subsequent JWKS request take d.
func (f *fakeIDP) delayKeys(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keyDelay = d
}

// hangKeys makes every subsequent JWKS request block. The returned func
// releases them; call it before the test ends, or httptest's own shutdown will
// wait on the stuck handler.
func (f *fakeIDP) hangKeys() (release func()) {
	ch := make(chan struct{})
	f.mu.Lock()
	f.hangCh = ch
	f.mu.Unlock()
	return sync.OnceFunc(func() {
		f.mu.Lock()
		f.hangCh = nil
		f.mu.Unlock()
		close(ch)
	})
}

// useKey makes kid/key the only key the issuer advertises and signs with —
// a hard rotation, which is the case a cached verifier is most likely to get
// wrong.
func (f *fakeIDP) useKey(kid string, key *rsa.PrivateKey) {
	h := &oidctest.Server{
		PublicKeys: []oidctest.PublicKey{{PublicKey: key.Public(), KeyID: kid, Algorithm: oidc.RS256}},
	}
	h.SetIssuer(f.srv.URL)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handler = h
	f.signer = key
	f.kid = kid
}

func (f *fakeIDP) url() string { return f.srv.URL }

// stop takes the issuer off the network. Everything cached must keep working.
func (f *fakeIDP) stop() { f.srv.Close() }

// claims is the shape a Keycloak access token has for this deployment: the
// operator role at the default client-roles path.
func (f *fakeIDP) claims(mutators ...func(map[string]any)) map[string]any {
	now := time.Now()
	c := map[string]any{
		"iss":   f.url(),
		"aud":   testAudience,
		"sub":   testSubject,
		"email": testEmail,
		"iat":   now.Add(-time.Minute).Unix(),
		"exp":   now.Add(time.Hour).Unix(),
		"resource_access": map[string]any{
			testClientID: map[string]any{"roles": []any{"offline_access", testOperator}},
		},
	}
	for _, m := range mutators {
		m(c)
	}
	return c
}

// mint signs claims with the issuer's current key.
func (f *fakeIDP) mint(t *testing.T, claims map[string]any) string {
	t.Helper()
	f.mu.Lock()
	key, kid := f.signer, f.kid
	f.mu.Unlock()
	return f.mintWith(t, key, kid, claims)
}

// mintWith signs with an arbitrary key — a rotated one, or an attacker's.
func (f *fakeIDP) mintWith(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshalling claims: %v", err)
	}
	return oidctest.SignIDToken(key, kid, oidc.RS256, string(raw))
}

// tamper flips a bit inside the signature, leaving a well-formed JWT whose
// signature no longer verifies.
//
// It decodes and re-encodes rather than editing a base64 character in place:
// an RSA signature is 256 bytes, which base64url renders in 342 characters
// whose last one carries four bits nothing decodes. Editing that character
// produces a token that still verifies — this helper did exactly that at
// first, and the test passed anyway on whichever mint happened to land a
// meaningful bit there.
func tamper(t *testing.T, token string) string {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("not a JWS compact serialization: %q", token)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(sig) == 0 {
		t.Fatalf("decoding signature: %v", err)
	}
	sig[0] ^= 0x80
	parts[2] = base64.RawURLEncoding.EncodeToString(sig)
	return strings.Join(parts, ".")
}

// rewriteClaims re-encodes the payload segment, leaving the original
// signature in place: the forgery an attacker actually attempts when they hold
// a valid token for an account that lacks the role they want.
func rewriteClaims(t *testing.T, token string, mutate func(map[string]any)) string {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("not a JWS compact serialization: %q", token)
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decoding payload: %v", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatalf("decoding claims: %v", err)
	}
	mutate(claims)
	forged, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("encoding claims: %v", err)
	}
	parts[1] = base64.RawURLEncoding.EncodeToString(forged)
	return strings.Join(parts, ".")
}

// nextClientIP hands every request a distinct source address so that one
// test's rejections cannot spend another's failure budget. The rate-limit
// test pins its own addresses instead.
var ipCounter atomic.Int64

func nextClientIP() string {
	n := ipCounter.Add(1)
	return fmt.Sprintf("203.0.113.%d:%d", n%250+1, 1024+n%60000)
}

// countingTransport counts every HTTP attempt, reached or refused.
//
// The JWKS-fetch counter on the fake issuer cannot see a request to a stopped
// server, so a "verify offline" test built on it alone passes even against an
// implementation that tries the IdP on every request and shrugs off the error
// — which is precisely the behaviour that guarantee exists to forbid (it would
// put a connect timeout on every request while the IdP is down). This sees the
// attempt.
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

// syncBuffer collects log output written from the background worker.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
