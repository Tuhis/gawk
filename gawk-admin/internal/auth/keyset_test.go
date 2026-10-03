package auth

// The JWKS rate floor, against real requests once oidc.RemoteKeySet is behind
// it. The floor itself lives in gawk-server/oidcauth (keyset.go) since R53
// (docs/55 D2), and the bucket's own arithmetic is tested there.

import (
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"
)

// testClock is a hand-wound clock. Every duration this file asserts on is a
// property of the bucket, not of the machine running the test, so nothing here
// sleeps.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func newTestClock() *testClock {
	// Anchored at real "now" so tokens minted with real timestamps still
	// validate against oidc.Config{Now: clk.now}.
	return &testClock{t: time.Now()}
}

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

// --- the floor, against real requests ---------------------------------------

// THE THROTTLE BITES. A caller feeding tokens signed by keys the issuer never
// advertised misses the cache every single time, which is what makes go-oidc
// reach for the network. The bucket caps that at its burst; the rest are
// refused inside this process. Every one of them is a 401 — never a 5xx, and
// never an accepted token.
func TestUnverifiableTokensCannotFetchMoreThanTheBucketAllows(t *testing.T) {
	idp := newFakeIDP(t)
	clk := newTestClock()
	a := newTestAuth(t, testConfig(t, idp.url()), Options{
		Now: clk.now,
		// Production defaults, pinned so the test is about them.
		JWKSFetchInterval: defaultJWKSFetchInterval,
		JWKSFetchBurst:    defaultJWKSFetchBurst,
	})
	h := testStack(a, idp.url())

	const attempts = 25
	for i := range attempts {
		// A distinct, never-advertised `kid` per request: the shape of a
		// caller probing for a key the process will accept.
		token := idp.mintWith(t, keyB(), "forged-kid-"+strconv.Itoa(i), idp.claims())
		rec := do(t, h, http.MethodGet, "/api/v1/me", token)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("request %d: status = %d, want 401 (body %q)", i, rec.Code, rec.Body.String())
		}
	}

	// The burst, plus the one throttle-exempt fetch startup priming made. That
	// exemption is a single request per pod, not a hole an attacker can widen:
	// twenty-five unverifiable tokens still buy exactly three fetches.
	if got := idp.keyFetches.Load(); got != int64(defaultJWKSFetchBurst)+1 {
		t.Errorf("JWKS fetches = %d for %d unverifiable tokens, want the burst (%d) plus the priming fetch",
			got, attempts, defaultJWKSFetchBurst)
	}
	if got := a.JWKSFetchTokensLeft(); got != 0 {
		t.Errorf("tokens left = %v, want 0: the bucket should be spent", got)
	}
}

// The other half of the same story: a REAL rotation lands, the bucket has a
// token, and nobody is refused. One fetch serves it, and the retired key stops
// working — the cache was replaced, not accumulated.
func TestRotationIsPickedUpOnOneFetchAndNobodyIs401ed(t *testing.T) {
	idp := newFakeIDP(t)
	clk := newTestClock()
	a := newTestAuth(t, testConfig(t, idp.url()), Options{
		Now:               clk.now,
		JWKSFetchInterval: defaultJWKSFetchInterval,
		JWKSFetchBurst:    defaultJWKSFetchBurst,
	})
	h := testStack(a, idp.url())

	// Warm-up: served straight from the cache startup primed, which is so far
	// the only fetch there has been.
	if rec := do(t, h, http.MethodGet, "/api/v1/me", idp.mint(t, idp.claims())); rec.Code != http.StatusOK {
		t.Fatalf("warm-up status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if got := idp.keyFetches.Load(); got != 1 {
		t.Fatalf("JWKS fetches after warm-up = %d, want 1 (priming, and nothing since)", got)
	}

	idp.useKey("key-b", keyB())
	// A whole shift's worth of operators, every one carrying the new key.
	for i := range 8 {
		rec := do(t, h, http.MethodGet, "/api/v1/me", idp.mint(t, idp.claims()))
		if rec.Code != http.StatusOK {
			t.Fatalf("post-rotation request %d = %d, want 200 (body %q)", i, rec.Code, rec.Body.String())
		}
	}
	if got := idp.keyFetches.Load(); got != 2 {
		t.Errorf("JWKS fetches = %d, want 2 (priming and one for the rotation)", got)
	}

	stale := idp.mintWith(t, keyA(), "key-a", idp.claims())
	if rec := do(t, h, http.MethodGet, "/api/v1/me", stale); rec.Code != http.StatusUnauthorized {
		t.Errorf("retired key = %d, want 401", rec.Code)
	}
}

// THE WORST CASE, pinned: a rotation that lands while an attacker has already
// emptied the bucket waits one refill interval — 20 seconds at the defaults —
// and then goes through. Not longer, and not a permanent lockout.
func TestRotationDuringAnAttackWaitsExactlyOneRefillInterval(t *testing.T) {
	idp := newFakeIDP(t)
	clk := newTestClock()
	a := newTestAuth(t, testConfig(t, idp.url()), Options{
		Now:               clk.now,
		JWKSFetchInterval: defaultJWKSFetchInterval,
		JWKSFetchBurst:    defaultJWKSFetchBurst,
	})
	h := testStack(a, idp.url())

	// Drain the bucket the way an attacker does.
	for i := range defaultJWKSFetchBurst + 5 {
		token := idp.mintWith(t, keyB(), "forged-kid-"+strconv.Itoa(i), idp.claims())
		if rec := do(t, h, http.MethodGet, "/api/v1/me", token); rec.Code != http.StatusUnauthorized {
			t.Fatalf("drain %d: status = %d, want 401", i, rec.Code)
		}
	}
	if got := a.JWKSFetchTokensLeft(); got != 0 {
		t.Fatalf("tokens left = %v after the drain, want 0", got)
	}

	// Now the IdP rotates for real. The operator's next token is signed by a
	// key nothing here has, and there is no budget to go and get it.
	idp.useKey("key-b", keyB())
	rotated := idp.mint(t, idp.claims())
	fetches := idp.keyFetches.Load()
	if rec := do(t, h, http.MethodGet, "/api/v1/me", rotated); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status with an empty bucket = %d, want 401", rec.Code)
	}
	if got := idp.keyFetches.Load(); got != fetches {
		t.Errorf("a throttled verification still reached the IdP (%d fetches, was %d)", got, fetches)
	}

	// Still refused a hair short of the interval...
	clk.advance(defaultJWKSFetchInterval - time.Second)
	if rec := do(t, h, http.MethodGet, "/api/v1/me", rotated); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status before the refill = %d, want 401", rec.Code)
	}
	// ...and in, on one fetch, once it has elapsed.
	//
	// Retried briefly, and the retry is load-bearing knowledge, not a shrug:
	// go-oidc's RemoteKeySet clears its in-flight fetch entry from a goroutine
	// AFTER unblocking the waiters, so on a loaded runner this verify can
	// still JOIN the previous throttled fetch's failed in-flight — a 401 that
	// spends no token and touches no network — instead of starting the fetch
	// the refilled token pays for. (Two CI runners under the e2e tier hit
	// exactly this, at 0.1s per failure; the fake clock plays no part.) A
	// joined failure consumes nothing, so retrying preserves both properties
	// asserted here: the eventual 200, and exactly ONE fetch for the refill.
	clk.advance(time.Second)
	rec := do(t, h, http.MethodGet, "/api/v1/me", rotated)
	for range 50 {
		if rec.Code == http.StatusOK {
			break
		}
		time.Sleep(20 * time.Millisecond)
		rec = do(t, h, http.MethodGet, "/api/v1/me", rotated)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status after %v = %d, want 200 (body %q)",
			defaultJWKSFetchInterval, rec.Code, rec.Body.String())
	}
	if got := idp.keyFetches.Load(); got != fetches+1 {
		t.Errorf("JWKS fetches = %d, want %d: the refill buys exactly one", got, fetches+1)
	}
}

// A throttled fetch must not poison the cache it could not refresh: the keys
// already held keep verifying tokens throughout.
func TestAThrottledFetchLeavesTheCachedKeysWorking(t *testing.T) {
	idp := newFakeIDP(t)
	clk := newTestClock()
	a := newTestAuth(t, testConfig(t, idp.url()), Options{
		Now:               clk.now,
		JWKSFetchInterval: defaultJWKSFetchInterval,
		JWKSFetchBurst:    defaultJWKSFetchBurst,
	})
	h := testStack(a, idp.url())

	good := idp.mint(t, idp.claims())
	if rec := do(t, h, http.MethodGet, "/api/v1/me", good); rec.Code != http.StatusOK {
		t.Fatalf("warm-up status = %d, want 200", rec.Code)
	}
	for i := range 10 {
		token := idp.mintWith(t, keyB(), "forged-kid-"+strconv.Itoa(i), idp.claims())
		do(t, h, http.MethodGet, "/api/v1/me", token)
	}
	if got := a.JWKSFetchTokensLeft(); got != 0 {
		t.Fatalf("tokens left = %v, want 0 — the test is not exercising the throttled path", got)
	}

	fetches := idp.keyFetches.Load()
	if rec := do(t, h, http.MethodGet, "/api/v1/me", idp.mint(t, idp.claims())); rec.Code != http.StatusOK {
		t.Fatalf("status for a good token with the bucket empty = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if got := idp.keyFetches.Load(); got != fetches {
		t.Errorf("a cached-key verification fetched (%d, was %d): the steady state must be offline", got, fetches)
	}
}
