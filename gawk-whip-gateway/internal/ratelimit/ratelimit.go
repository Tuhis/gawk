// Package ratelimit is the gateway's per-client-IP POST /whip limit (docs/83
// D4, §9: "rate-limited per IP at the gateway, then gated again by the
// relay").
//
// A token bucket per canonical address, kept in a bounded LRU so a flood of
// spoofed-looking sources (each a distinct IPv6 /128) cannot grow memory
// without bound. Evicting an idle bucket only ever forgets a limit, which is
// the safe direction for a gate in front of the relay's own limit.
package ratelimit

import (
	"container/list"
	"net/netip"
	"sync"
	"time"
)

// Limiter is a set of token buckets keyed by address.
type Limiter struct {
	mu    sync.Mutex
	rate  float64 // tokens per second
	burst float64
	max   int
	ll    *list.List
	m     map[netip.Addr]*list.Element
}

type bucket struct {
	addr   netip.Addr
	tokens float64
	last   time.Time
}

// New builds a limiter: perSecond sustained, burst at once, at most maxKeys
// buckets. perSecond <= 0 disables it (Allow always true).
func New(perSecond float64, burst int, maxKeys int) *Limiter {
	if burst < 1 {
		burst = 1
	}
	if maxKeys < 1 {
		maxKeys = 1
	}
	return &Limiter{rate: perSecond, burst: float64(burst), max: maxKeys, ll: list.New(), m: make(map[netip.Addr]*list.Element)}
}

// Allow takes a token for addr at now. On refusal it returns how long until
// a token is available, for Retry-After.
func (l *Limiter) Allow(addr netip.Addr, now time.Time) (bool, time.Duration) {
	if l == nil || l.rate <= 0 {
		return true, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	var b *bucket
	if el, ok := l.m[addr]; ok {
		l.ll.MoveToFront(el)
		b = el.Value.(*bucket)
		if dt := now.Sub(b.last).Seconds(); dt > 0 {
			b.tokens += dt * l.rate
			if b.tokens > l.burst {
				b.tokens = l.burst
			}
		}
		b.last = now
	} else {
		b = &bucket{addr: addr, tokens: l.burst, last: now}
		l.m[addr] = l.ll.PushFront(b)
		for l.ll.Len() > l.max {
			old := l.ll.Back()
			l.ll.Remove(old)
			delete(l.m, old.Value.(*bucket).addr)
		}
	}
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	wait := time.Duration((1 - b.tokens) / l.rate * float64(time.Second))
	return false, wait
}
