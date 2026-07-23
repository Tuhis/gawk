package hub

// R21 DV3 (docs/26 Decision 6): the per-subscriber catch-up ceiling.
//
// A DVR subscriber recovering from a stall must send faster than live or it
// never closes its backlog — covering a stall S with buffer B needs
// B/(B−S) times the stream bitrate. But the egress budget is one process-wide
// bucket shared with every other viewer on the pod, and the network events
// that stall one viewer usually stall them all, so without a ceiling a
// recovering herd takes the whole pipe at exactly the moment everyone needs
// it. The ceiling is expressed as a multiple of the broadcast's OWN live rate
// rather than as an absolute, because that is the only number that means the
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
// ceiling. Enough that a single large keyframe is never chopped into a stutter
// by the pacer itself, small enough that banking a whole stall's worth of
// credit — which would defeat the ceiling on the one occasion it matters —
// is impossible.
func (p *dvrPacer) burst(liveBps int) float64 {
	return p.multiple * float64(liveBps) * 0.25
}

// allow reports whether n bytes may go out now. A false return means the
// caller should wait and retry; the drain's existing retry path does that.
//
// Two deliberate non-throttles: a non-positive multiple disables the ceiling
// entirely, and an unknown live rate (too little history to estimate) passes
// everything. Guessing low on a fresh broadcast would stall every viewer on
// it, which is a far worse failure than briefly missing a ceiling.
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
