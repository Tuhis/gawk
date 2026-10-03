package oidcauth

// The JWKS rate floor (keyset.go): the bucket's own arithmetic. What it does
// to real requests once oidc.RemoteKeySet is behind it is asserted by each
// consumer's suite — gawk-admin's internal/auth (R39 AP5) and the relay's
// internal/ops — which R53 (docs/55 D2) moved onto this package unchanged.
//
// These tests were in both suites before the verifier was shared; the two
// copies were identical apart from the zero-options case, which the relay ran
// with a nil clock and the portal with a real one. Both variants are kept.

import (
	"sync"
	"testing"
	"time"
)

// The names the moved tests were written against.
const (
	defaultJWKSFetchInterval = DefaultJWKSFetchInterval
	defaultJWKSFetchBurst    = DefaultJWKSFetchBurst
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

// The relay suite's variant of the case above: with a nil clock as well.
func TestJWKSThrottleZeroOptionsWithANilClockMeanTheDefaults(t *testing.T) {
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
