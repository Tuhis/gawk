package auth

import (
	"sync"
	"time"
)

// maxTrackedIPs caps the failure table. Reaching it triggers an inline sweep;
// what survives a sweep is only IPs that are actually spending their budget.
// Residual, stated: a distributed source can still grow the table between
// sweeps, but each entry is a few dozen bytes and the Ingress in front of this
// service collapses most sources onto one address anyway.
const maxTrackedIPs = 10_000

// bucketIdleTTL is how long a full (unpenalised) bucket is kept before it is
// swept. Anything still holding tokens back is kept: forgetting it would hand
// the client a fresh budget.
const bucketIdleTTL = 10 * time.Minute

type bucket struct {
	tokens float64
	last   time.Time
}

// ipLimiter is a mutex-guarded token bucket per client IP, the same shape as
// the relay's transport limiter (gawk-server/internal/transport/limiter.go) —
// house precedent, no new dependency, and small enough to read in one sitting.
//
// It differs from that one in two ways, both deliberate: eviction is driven by
// the owner's existing background goroutine rather than one of its own (Auth
// already has a ticker), and `now` is injectable so the tests can prove refill
// and eviction without sleeping.
type ipLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rate    float64 // tokens per second
	burst   int     // bucket capacity
	now     func() time.Time
}

func newIPLimiter(rate float64, burst int, now func() time.Time) *ipLimiter {
	if now == nil {
		now = time.Now
	}
	return &ipLimiter{
		buckets: make(map[string]*bucket),
		rate:    rate,
		burst:   burst,
		now:     now,
	}
}

// Allow spends one token for ip and reports whether it had one. Only callers
// that are already answering a failed credential call it (auth.go), so a
// well-behaved client never touches its budget.
func (l *ipLimiter) Allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	b, ok := l.buckets[ip]
	if !ok {
		if len(l.buckets) >= maxTrackedIPs {
			l.sweepLocked(now)
		}
		b = &bucket{tokens: float64(l.burst), last: now}
		l.buckets[ip] = b
	}

	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * l.rate
		if b.tokens > float64(l.burst) {
			b.tokens = float64(l.burst)
		}
	}
	b.last = now

	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

// sweep drops buckets that are back at full capacity and idle. Called from
// Auth's background goroutine.
func (l *ipLimiter) sweep() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweepLocked(l.now())
}

func (l *ipLimiter) sweepLocked(now time.Time) {
	for ip, b := range l.buckets {
		refilled := b.tokens + now.Sub(b.last).Seconds()*l.rate
		if refilled >= float64(l.burst) && now.Sub(b.last) > bucketIdleTTL {
			delete(l.buckets, ip)
		}
	}
}
