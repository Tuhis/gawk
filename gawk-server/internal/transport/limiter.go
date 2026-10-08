package transport

import (
	"net"
	"sync"
	"time"
)

type tokenBucket struct {
	tokens float64
	last   time.Time
}

type ipRateLimiter struct {
	mu     sync.Mutex
	ips    map[string]*tokenBucket
	rate   float64 // tokens per second
	burst  int     // max tokens
	closed chan struct{}
}

func newIPRateLimiter(rate float64, burst int) *ipRateLimiter {
	l := &ipRateLimiter{
		ips:    make(map[string]*tokenBucket),
		rate:   rate,
		burst:  burst,
		closed: make(chan struct{}),
	}
	go l.cleanupLoop()
	return l
}

func (l *ipRateLimiter) Allow(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	tb, exists := l.ips[host]
	if !exists {
		tb = &tokenBucket{
			tokens: float64(l.burst),
			last:   now,
		}
		l.ips[host] = tb
	}

	// Refill tokens
	elapsed := now.Sub(tb.last).Seconds()
	tb.last = now
	tb.tokens += elapsed * l.rate
	if tb.tokens > float64(l.burst) {
		tb.tokens = float64(l.burst)
	}

	if tb.tokens >= 1.0 {
		tb.tokens -= 1.0
		return true
	}
	return false
}

func (l *ipRateLimiter) Close() {
	close(l.closed)
}

func (l *ipRateLimiter) cleanupLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case now := <-ticker.C:
			l.sweep(now)
		case <-l.closed:
			return
		}
	}
}

// sweep evicts buckets that are full and have been idle for > 10 minutes.
// Fullness is judged on the refilled value: tokens is only written by Allow,
// which always leaves it below burst, so the stored count alone never
// qualifies and the map would grow by one entry per client IP forever.
func (l *ipRateLimiter) sweep(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for ip, tb := range l.ips {
		idle := now.Sub(tb.last)
		if idle > 10*time.Minute && tb.tokens+idle.Seconds()*l.rate >= float64(l.burst) {
			delete(l.ips, ip)
		}
	}
}
