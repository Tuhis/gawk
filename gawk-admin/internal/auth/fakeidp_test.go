package auth

import (
	"bytes"
	"crypto/rsa"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/oidcauth"
	"github.com/Tuhis/gawk/gawk-server/oidcauth/oidcauthtest"
)

// The fake OIDC issuer AP5's acceptance criteria call for. Since R53 (docs/55
// D2) it is gawk-server's oidcauthtest.Issuer, shared with the relay's suite
// and telemetry's; what remains here is the vocabulary this suite was written
// in — a key shared across issuers, map-shaped claims, unexported helper
// names — so the tests themselves read exactly as they did.

const (
	testClientID = "gawk-admin"
	testAudience = "gawk-admin-api"
	testOperator = "operator"
	testSubject  = "5f2b1a3c-user"
	testEmail    = "operator@example.test"
)

// The names this suite asserts against, which moved into oidcauth with the
// code they describe.
const (
	defaultJWKSFetchInterval = oidcauth.DefaultJWKSFetchInterval
	defaultJWKSFetchBurst    = oidcauth.DefaultJWKSFetchBurst

	CodeUnauthorized   = oidcauth.CodeUnauthorized
	CodeIDPUnavailable = oidcauth.CodeIDPUnavailable
	CodeForbidden      = oidcauth.CodeForbidden
	CodeRateLimited    = oidcauth.CodeRateLimited
	CodeInternal       = oidcauth.CodeInternal
)

// apiError is the docs/42 §4.7 envelope the verifier answers with, decoded.
type apiError struct {
	Error apiErrorBody `json:"error"`
}

type apiErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// RSA keygen is the slowest thing in this file, so the suite shares two keys:
// one the issuer starts with and one it rotates to.
var (
	keyA = sync.OnceValue(oidcauthtest.NewRSAKey)
	keyB = sync.OnceValue(oidcauthtest.NewRSAKey)
)

// counter reads a count the way an atomic.Int64 field would.
type counter func() int64

func (c counter) Load() int64 { return c() }

type fakeIDP struct {
	*oidcauthtest.Issuer
	// keyFetches counts JWKS requests. A test that stops the server and still
	// verifies is proving this counter did not move.
	keyFetches counter
}

func newFakeIDP(t *testing.T) *fakeIDP {
	t.Helper()
	iss := oidcauthtest.NewIssuer(t, "key-a", keyA())
	return &fakeIDP{Issuer: iss, keyFetches: iss.KeyFetches}
}

func (f *fakeIDP) setDown(down bool)                      { f.SetDown(down) }
func (f *fakeIDP) downKeys(down bool)                     { f.SetKeysDown(down) }
func (f *fakeIDP) delayKeys(d time.Duration)              { f.DelayKeys(d) }
func (f *fakeIDP) hangKeys() (release func())             { return f.HangKeys() }
func (f *fakeIDP) useKey(kid string, key *rsa.PrivateKey) { f.UseKey(kid, key) }
func (f *fakeIDP) url() string                            { return f.URL() }
func (f *fakeIDP) stop()                                  { f.Close() }

// claims is the shape a Keycloak access token has for this deployment: the
// operator role at the default client-roles path.
//
// The roles sit under testAudience, not testClientID. Those are deliberately
// different strings here, and the default path addresses the AUDIENCE — the
// resource server the token was minted for, which is the client whose
// `resource_access` entry carries this API's roles. The SPA's public client ID
// is a different thing that holds none.
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
			testAudience: map[string]any{"roles": []any{"offline_access", testOperator}},
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
	kid, key := f.SigningKey()
	return f.mintWith(t, key, kid, claims)
}

// mintWith signs with an arbitrary key — a rotated one, or an attacker's.
func (f *fakeIDP) mintWith(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	return oidcauthtest.SignClaims(t, key, kid, claims)
}

// tamper flips a bit inside the signature (oidcauthtest.Tamper).
func tamper(t *testing.T, token string) string {
	t.Helper()
	return oidcauthtest.Tamper(t, token)
}

// rewriteClaims re-encodes the payload, keeping the original signature
// (oidcauthtest.RewriteClaims).
func rewriteClaims(t *testing.T, token string, mutate func(map[string]any)) string {
	t.Helper()
	return oidcauthtest.RewriteClaims(t, token, mutate)
}

// nextClientIP hands every request a distinct source address so that one
// test's rejections cannot spend another's failure budget. The rate-limit
// test pins its own addresses instead.
var ipCounter atomic.Int64

func nextClientIP() string {
	n := ipCounter.Add(1)
	return fmt.Sprintf("203.0.113.%d:%d", n%250+1, 1024+n%60000)
}

// countingTransport exposes oidcauthtest.CountingTransport's attempt count
// under the name this suite reads it by.
type countingTransport struct {
	attempts counter
}

func countingClient() (*http.Client, *countingTransport) {
	client, tr := oidcauthtest.CountingClient()
	return client, &countingTransport{attempts: tr.Attempts}
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
