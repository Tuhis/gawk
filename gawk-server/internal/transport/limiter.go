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
		case <-ticker.C:
			l.mu.Lock()
			now := time.Now()
			for ip, tb := range l.ips {
				// If bucket is full and idle for > 10 minutes, clean it up
				if tb.tokens >= float64(l.burst) && now.Sub(tb.last) > 10*time.Minute {
					delete(l.ips, ip)
				}
			}
			l.mu.Unlock()
		case <-l.closed:
			return
		}
	}
}
