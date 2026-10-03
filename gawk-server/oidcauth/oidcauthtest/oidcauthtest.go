// Package oidcauthtest is the fake OIDC issuer every oidcauth consumer tests
// against (R53, docs/55 D2): a real HTTP server serving
// /.well-known/openid-configuration and a JWKS, with locally generated keys
// that tokens are minted against.
//
// go-oidc's oidctest supplies the discovery/JWKS rendering and the signer;
// Issuer adds what the R39 suites needed and oidctest cannot do — swapping
// the advertised key set at runtime (rotation) without racing a concurrent
// request, counting JWKS fetches so a test can prove a verification did NOT
// touch the network, and the failure shapes a real IdP has (down, key set
// down, slow, hanging). The relay's and the portal's harnesses restated all
// of it before this package existed; a new consumer writes no third copy.
//
// It is test support: nothing outside _test.go files may import it.
package oidcauthtest

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
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

// KeysPath is the JWKS path oidctest serves, and the one Issuer counts.
const KeysPath = "/keys"

// NewRSAKey generates a 2048-bit RSA key, panicking on failure. Key generation
// is the slowest thing in any suite that uses this package, so share keys
// (sync.OnceValue) where the test does not need them distinct.
func NewRSAKey() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic("generating test key: " + err.Error())
	}
	return k
}

// Issuer is a fake OIDC provider on a local httptest server. Its zero value is
// not usable; construct it with NewIssuer.
type Issuer struct {
	srv *httptest.Server

	// keyFetches counts JWKS requests that reached the server. A test that
	// stops the server and still verifies is proving this did not move.
	keyFetches atomic.Int64

	mu       sync.Mutex
	handler  *oidctest.Server // replaced wholesale on rotation; never mutated in place
	signer   *rsa.PrivateKey
	kid      string
	hangCh   chan struct{} // when non-nil, the JWKS blocks on it
	down     bool          // when true, every endpoint 503s
	keysDown bool          // when true, only the JWKS 503s — discovery still answers
	keyDelay time.Duration // when non-zero, the JWKS sleeps this long
}

// NewIssuer starts an issuer advertising (and signing with) key under kid. The
// server is closed by t.Cleanup; Close may be called earlier and is idempotent.
func NewIssuer(t testing.TB, kid string, key *rsa.PrivateKey) *Issuer {
	t.Helper()
	i := &Issuer{}
	i.srv = httptest.NewServer(i)
	t.Cleanup(i.srv.Close)
	i.UseKey(kid, key)
	return i
}

// ServeHTTP serves discovery and the JWKS, honouring the failure toggles.
func (i *Issuer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	i.mu.Lock()
	h, hang, down, delay, keysDown := i.handler, i.hangCh, i.down, i.keyDelay, i.keysDown
	i.mu.Unlock()
	if down {
		// An IdP that is up enough to answer but not to serve — the shape a
		// restarting Keycloak has, and the one that must not crash a pod.
		http.Error(w, "identity provider unavailable", http.StatusServiceUnavailable)
		return
	}
	if r.URL.Path == KeysPath {
		i.keyFetches.Add(1)
		if keysDown {
			// Discovery answers, the key set does not — the shape a
			// half-restarted IdP has, and the split that separates "resolved"
			// from "primed".
			http.Error(w, "key set unavailable", http.StatusServiceUnavailable)
			return
		}
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

// URL is the issuer URL (also the `iss` a good token carries).
func (i *Issuer) URL() string { return i.srv.URL }

// Close takes the issuer off the network. Everything a verifier cached must
// keep working. Idempotent.
func (i *Issuer) Close() { i.srv.Close() }

// KeyFetches is the number of JWKS requests that reached the server.
func (i *Issuer) KeyFetches() int64 { return i.keyFetches.Load() }

// UseKey makes kid/key the only key the issuer advertises and signs with — a
// hard rotation, which is the case a cached verifier is most likely to get
// wrong. The handler is swapped whole under the lock, so a request in flight
// is served entirely by the old key set or entirely by the new one.
func (i *Issuer) UseKey(kid string, key *rsa.PrivateKey) {
	h := &oidctest.Server{
		PublicKeys: []oidctest.PublicKey{{PublicKey: key.Public(), KeyID: kid, Algorithm: oidc.RS256}},
	}
	h.SetIssuer(i.srv.URL)
	i.mu.Lock()
	defer i.mu.Unlock()
	i.handler = h
	i.signer = key
	i.kid = kid
}

// SigningKey returns the key the issuer currently signs with, and its kid.
func (i *Issuer) SigningKey() (string, *rsa.PrivateKey) {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.kid, i.signer
}

// SetDown toggles whether the issuer answers at all (every endpoint 503s).
func (i *Issuer) SetDown(down bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.down = down
}

// SetKeysDown toggles whether the JWKS 503s while discovery keeps answering.
func (i *Issuer) SetKeysDown(down bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.keysDown = down
}

// DelayKeys makes every subsequent JWKS request take d.
func (i *Issuer) DelayKeys(d time.Duration) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.keyDelay = d
}

// HangKeys makes every subsequent JWKS request block. The returned func
// releases them; call it before the test ends, or httptest's own shutdown
// will wait on the stuck handler.
func (i *Issuer) HangKeys() (release func()) {
	ch := make(chan struct{})
	i.mu.Lock()
	i.hangCh = ch
	i.mu.Unlock()
	return sync.OnceFunc(func() {
		i.mu.Lock()
		i.hangCh = nil
		i.mu.Unlock()
		close(ch)
	})
}

// Mint signs claimsJSON (a JSON object) with the issuer's current key.
func (i *Issuer) Mint(claimsJSON string) string {
	kid, key := i.SigningKey()
	return Sign(key, kid, claimsJSON)
}

// Sign signs claimsJSON with an arbitrary key under kid, RS256 — a rotated
// key, or an attacker's.
func Sign(key *rsa.PrivateKey, kid, claimsJSON string) string {
	return oidctest.SignIDToken(key, kid, oidc.RS256, claimsJSON)
}

// SignClaims marshals claims and signs them like Sign.
func SignClaims(t testing.TB, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshalling claims: %v", err)
	}
	return Sign(key, kid, string(raw))
}

// Tamper flips a bit inside the signature, leaving a well-formed JWT whose
// signature no longer verifies.
//
// It decodes and re-encodes rather than editing a base64 character in place:
// an RSA signature is 256 bytes, which base64url renders in 342 characters
// whose last one carries four bits nothing decodes. Editing that character
// produces a token that still verifies.
func Tamper(t testing.TB, token string) string {
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

// RewriteClaims re-encodes the payload segment, leaving the original
// signature in place: the forgery an attacker actually attempts when they hold
// a valid token for an account that lacks the role they want.
func RewriteClaims(t testing.TB, token string, mutate func(map[string]any)) string {
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

// CountingTransport counts every HTTP attempt, reached or refused.
//
// An issuer's own fetch counter cannot see a request to a stopped server, so
// a "verify offline" test built on it alone passes even against an
// implementation that tries the IdP on every request and shrugs off the error
// — which is precisely the behaviour that guarantee exists to forbid (it would
// put a connect timeout on every request while the IdP is down). This sees
// the attempt.
type CountingTransport struct {
	base     http.RoundTripper
	attempts atomic.Int64
}

// RoundTrip counts the attempt and forwards it.
func (c *CountingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.attempts.Add(1)
	return c.base.RoundTrip(r)
}

// Attempts is the number of requests made through the transport.
func (c *CountingTransport) Attempts() int64 { return c.attempts.Load() }

// CountingClient returns a bounded-timeout client over a CountingTransport.
func CountingClient() (*http.Client, *CountingTransport) {
	tr := &CountingTransport{base: http.DefaultTransport}
	return &http.Client{Transport: tr, Timeout: 10 * time.Second}, tr
}
