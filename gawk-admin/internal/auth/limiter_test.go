package auth

import (
	"sync"
	"testing"
	"time"
)

// fakeClock lets the limiter tests assert refill and eviction without sleeping.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func TestIPLimiterBurstThenRefill(t *testing.T) {
	clock := newFakeClock()
	l := newIPLimiter(1, 3, clock.now)

	for i := range 3 {
		if !l.Allow("10.0.0.1") {
			t.Fatalf("attempt %d denied inside the burst", i+1)
		}
	}
	if l.Allow("10.0.0.1") {
		t.Fatal("burst was not exhausted")
	}
	// A different source is unaffected.
	if !l.Allow("10.0.0.2") {
		t.Fatal("a second IP was denied on its first request: the budget is not per IP")
	}

	clock.advance(time.Second)
	if !l.Allow("10.0.0.1") {
		t.Fatal("one token did not refill after a second at 1/s")
	}
	if l.Allow("10.0.0.1") {
		t.Fatal("more than one token refilled in one second")
	}
}

func TestIPLimiterSweepDropsOnlyIdleFullBuckets(t *testing.T) {
	clock := newFakeClock()
	l := newIPLimiter(1, 3, clock.now)

	l.Allow("10.0.0.1") // spends one token
	l.Allow("10.0.0.2")
	l.Allow("10.0.0.2")
	l.Allow("10.0.0.2") // 10.0.0.2 is now empty

	l.sweep()
	if len(l.buckets) != 2 {
		t.Fatalf("buckets = %d, want 2: nothing is idle yet", len(l.buckets))
	}

	// Past the idle TTL both have refilled to full, so both are forgettable.
	clock.advance(bucketIdleTTL + time.Minute)
	l.sweep()
	if len(l.buckets) != 0 {
		t.Fatalf("buckets = %d, want 0 after the idle TTL", len(l.buckets))
	}
}

func TestIPLimiterKeepsPenalisedBucketsAcrossASweep(t *testing.T) {
	clock := newFakeClock()
	// Slow refill: after the TTL the bucket is still short of full, so
	// forgetting it would hand the client a brand-new budget.
	l := newIPLimiter(0.0001, 3, clock.now)
	for range 3 {
		l.Allow("10.0.0.3")
	}
	clock.advance(bucketIdleTTL + time.Minute)
	l.sweep()
	if len(l.buckets) != 1 {
		t.Fatalf("buckets = %d, want 1: an exhausted bucket must survive eviction", len(l.buckets))
	}
	if l.Allow("10.0.0.3") {
		t.Fatal("the exhausted bucket was reset by the sweep")
	}
}
