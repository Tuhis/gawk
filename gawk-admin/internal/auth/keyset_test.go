package auth

// The JWKS rate floor (keyset.go). Two layers: the bucket's own arithmetic,
// and what it does to real requests once oidc.RemoteKeySet is behind it.

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

// --- the bucket -------------------------------------------------------------

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

// The bucket starts FULL, so a key rotation on an otherwise idle process gets
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

	// One token short of the interval is still refused; the interval exactly
	// is allowed. This is the worst-case rotation delay the comment claims.
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
	clk := newTestClock()
	th := newJWKSThrottle(0, 0, clk.now)
	if th.burst != float64(defaultJWKSFetchBurst) || th.interval != defaultJWKSFetchInterval {
		t.Fatalf("zero options gave burst %v interval %v, want the defaults", th.burst, th.interval)
	}
	th = newJWKSThrottle(-time.Second, -4, clk.now)
	if th.burst != float64(defaultJWKSFetchBurst) || th.interval != defaultJWKSFetchInterval {
		t.Fatalf("negative options gave burst %v interval %v, want the defaults", th.burst, th.interval)
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
	if got := a.throttle.tokensLeft(); got != 0 {
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
	if got := a.throttle.tokensLeft(); got != 0 {
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
	if got := a.throttle.tokensLeft(); got != 0 {
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
