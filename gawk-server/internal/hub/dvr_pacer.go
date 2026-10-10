package hub

// The per-subscriber DVR catch-up ceiling (docs/26).
//
// A DVR subscriber recovering from a stall must send faster than live or it
// never closes its backlog, but the egress budget is one process-wide bucket
// and the network events that stall one viewer usually stall them all: without
// a ceiling a recovering herd takes the whole pipe at once. The ceiling is a
// multiple of the broadcast's OWN live rate, the only number that means the
// same thing for a 500 kbps phone capture and a 50 Mbps desktop one.

import (
	"sync"
	"time"
)

// dvrPacer is a token bucket refilled at multiple × the live rate.
//
// One bucket per SUBSCRIBER, shared by its video and audio drains — the
// ceiling bounds what the relay sends to that viewer, and splitting it per
// lane would let a subscriber draw 2x the configured multiple. Both drains
// are separate goroutines, hence the mutex.
type dvrPacer struct {
	mu       sync.Mutex
	multiple float64
	now      func() time.Time
	tokens   float64
	last     time.Time
}

func newDVRPacer(multiple float64, now func() time.Time) *dvrPacer {
	return &dvrPacer{multiple: multiple, now: now}
}

// burst is how much the bucket may bank while idle: a quarter second at the
// ceiling. Enough that one large keyframe is never chopped into a stutter,
// small enough that banking a whole stall's worth of credit (defeating the
// ceiling exactly when it matters) is impossible.
func (p *dvrPacer) burst(liveBps int) float64 {
	return p.multiple * float64(liveBps) * 0.25
}

// allow reports whether n bytes may go out now; false means wait and retry.
//
// A non-positive multiple disables the ceiling, and an unknown live rate (too
// little history) passes everything: guessing low on a fresh broadcast would
// stall every viewer on it, far worse than briefly missing a ceiling.
func (p *dvrPacer) allow(n int, liveBps int) bool {
	if p.multiple <= 0 || liveBps <= 0 {
		return true
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	if p.last.IsZero() {
		// Start full: the first write after a join or a resync should not be
		// made to wait for a bucket that has never been filled.
		p.last = now
		p.tokens = p.burst(liveBps)
	}
	rate := p.multiple * float64(liveBps)
	p.tokens += rate * now.Sub(p.last).Seconds()
	p.last = now
	if max := p.burst(liveBps); p.tokens > max {
		p.tokens = max
	}
	if p.tokens < float64(n) {
		return false
	}
	p.tokens -= float64(n)
	return true
}
