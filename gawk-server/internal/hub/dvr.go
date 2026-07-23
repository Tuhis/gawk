package hub

// R21 DV1 (docs/26): the per-broadcast DVR ring.
//
// Resilient mode trades latency for smoothness, but until R21 the relay only
// held the viewer's *undelivered* bytes and destroyed them the moment a write
// stalled (docs/24 finding 17). A viewer's playout buffer is made of delay,
// not of pre-fetched data, so during a stall it needs exactly the frames
// captured during that stall — the ones that were being thrown away. This ring
// keeps them.
//
// Shape, and why:
//
//   - ONE ring per broadcast, N cursors. The bytes are identical for every
//     subscriber, so copying them per subscriber would multiply memory by the
//     audience for no reason (docs/26 Decision 1).
//   - Entries are whole GOPs, not loose datagrams. A cursor must be able to
//     *start*, and the only decodable start is a keyframe (Decision 2). It
//     also lines up with the carrier's existing per-GOP rotation, so replaying
//     a GOP is byte-identical on the wire to serving it live.
//   - Bounded by duration AND bytes, whichever binds first (Decision 10). The
//     duration expresses the product intent; the byte cap is what keeps a
//     50 Mbps broadcaster from taking the pod down.
//
// Concurrency: one appender (the publisher's fan-out, already under
// registry.mu) and N readers (the drains). Guarded by its own RWMutex rather
// than registry.mu so a drain reading history never contends with fan-out.

import (
	"sync"
	"time"
)

// DVROptions bounds one ring. Both bounds are enforced; whichever binds first
// evicts.
type DVROptions struct {
	// Window is how much wall-clock history to retain.
	Window time.Duration
	// MaxBytes caps the retained payload (keyframes + records), excluding
	// per-entry overhead.
	MaxBytes int
}

// dvrGop is one GOP's retained bytes: the keyframe message as it would be
// written to a keyframe stream, then the delta records in fan-out order.
type dvrGop struct {
	seq      int64
	at       time.Time
	keyframe []byte
	records  [][]byte
	bytes    int
	// complete is set when the next keyframe arrives — i.e. no more records
	// will ever be appended to this GOP. A reader at the end of an incomplete
	// GOP must wait rather than skip.
	complete bool
}

// DVRCursor is one subscriber's position. Values are immutable; every advance
// returns a new cursor, which keeps the drain's bookkeeping free of aliasing
// bugs and makes the whole thing safe to read under an RLock.
type DVRCursor struct {
	gopSeq int64
	// recordIdx is the index of the NEXT record to send. -1 means "the
	// keyframe has not been sent yet", which is where every cursor starts and
	// where a resync lands.
	recordIdx int
}

// AtFirstRecord moves past the keyframe to this GOP's first delta record.
func (c DVRCursor) AtFirstRecord() DVRCursor {
	if c.recordIdx < 0 {
		c.recordIdx = 0
	}
	return c
}

// Next advances one record within the current GOP.
func (c DVRCursor) Next() DVRCursor {
	c.recordIdx++
	return c
}

// GopSeq is the GOP this cursor is in — the /statusz and metrics identity.
func (c DVRCursor) GopSeq() int64 { return c.gopSeq }

// NeedsKeyframe reports whether this cursor still owes its GOP's keyframe.
func (c DVRCursor) NeedsKeyframe() bool { return c.recordIdx < 0 }

// DVRRing is a bounded window of one broadcast, read by cursor.
type DVRRing struct {
	opts DVROptions

	mu      sync.RWMutex
	gops    []*dvrGop
	nextSeq int64
	bytes   int
}

func NewDVRRing(opts DVROptions) *DVRRing {
	return &DVRRing{opts: opts}
}

// AppendKeyframe starts a new GOP, completing the previous one. msg is the
// full StreamFrame message bytes, exactly as they would go on a keyframe
// stream — copied, because the ring outlives the caller's buffer by seconds
// (see TestDVRRingOwnsItsBytes).
func (r *DVRRing) AppendKeyframe(msg []byte, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n := len(r.gops); n > 0 {
		r.gops[n-1].complete = true
	}
	g := &dvrGop{
		seq:      r.nextSeq,
		at:       at,
		keyframe: append([]byte(nil), msg...),
		bytes:    len(msg),
	}
	r.nextSeq++
	r.gops = append(r.gops, g)
	r.bytes += g.bytes
	r.evictLocked(at)
}

// AppendRecord adds one delta record to the current GOP. Records arriving
// before any keyframe are dropped: they are undecodable for every possible
// cursor, so retaining them would only cost memory.
func (r *DVRRing) AppendRecord(rec []byte, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := len(r.gops)
	if n == 0 {
		return
	}
	g := r.gops[n-1]
	g.records = append(g.records, append([]byte(nil), rec...))
	g.bytes += len(rec)
	r.bytes += len(rec)
	r.evictLocked(at)
}

// NewCursor returns a cursor at the newest GOP's keyframe — where a joining
// subscriber starts, and the same place a resync lands.
func (r *DVRRing) NewCursor() DVRCursor {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.newestCursorLocked()
}

// ResyncCursor is NewCursor under its failure name: what a subscriber that
// fell off the tail is given (docs/26 Decision 4).
func (r *DVRRing) ResyncCursor() DVRCursor { return r.NewCursor() }

func (r *DVRRing) newestCursorLocked() DVRCursor {
	if len(r.gops) == 0 {
		return DVRCursor{gopSeq: r.nextSeq, recordIdx: -1}
	}
	return DVRCursor{gopSeq: r.gops[len(r.gops)-1].seq, recordIdx: -1}
}

// Keyframe returns the keyframe bytes at the cursor's GOP. ok is false when
// the GOP has been evicted (the cursor fell off the tail) or does not exist
// yet. The returned slice is the ring's own and must not be modified.
func (r *DVRRing) Keyframe(c DVRCursor) ([]byte, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	g := r.findLocked(c.gopSeq)
	if g == nil {
		return nil, false
	}
	return g.keyframe, true
}

// Record returns the record at the cursor. ok is false at the end of the GOP's
// currently-known records — which for an incomplete GOP means "not yet", and
// for a complete one means "advance to the next GOP".
func (r *DVRRing) Record(c DVRCursor) ([]byte, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	g := r.findLocked(c.gopSeq)
	if g == nil || c.recordIdx < 0 || c.recordIdx >= len(g.records) {
		return nil, false
	}
	return g.records[c.recordIdx], true
}

// GopComplete reports whether the cursor's GOP can still grow. A reader that
// has run out of records must wait on an incomplete GOP and advance past a
// complete one; conflating the two either skips live records or wedges.
func (r *DVRRing) GopComplete(c DVRCursor) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	g := r.findLocked(c.gopSeq)
	return g != nil && g.complete
}

// NextGop advances to the following GOP's keyframe. ok is false when there is
// no newer GOP yet.
func (r *DVRRing) NextGop(c DVRCursor) (DVRCursor, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, g := range r.gops {
		if g.seq > c.gopSeq {
			return DVRCursor{gopSeq: g.seq, recordIdx: -1}, true
		}
	}
	return c, false
}

// FellOffTail reports that the cursor's GOP has been evicted — the one frame
// loss this mode has (docs/26 Decision 4).
func (r *DVRRing) FellOffTail(c DVRCursor) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.gops) == 0 {
		return false
	}
	return c.gopSeq < r.gops[0].seq
}

// LagMs is how far behind the newest GOP this cursor sits, in ms. The staleness
// bound (docs/26 Decision 7) compares it against the viewer's declared buffer.
func (r *DVRRing) LagMs(c DVRCursor, now time.Time) int64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	g := r.findLocked(c.gopSeq)
	if g == nil || len(r.gops) == 0 {
		return 0
	}
	newest := r.gops[len(r.gops)-1]
	return newest.at.Sub(g.at).Milliseconds()
}

// OldestGopSeq is the tail's sequence — the eviction frontier.
func (r *DVRRing) OldestGopSeq() int64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.gops) == 0 {
		return r.nextSeq
	}
	return r.gops[0].seq
}

// Bytes is the retained payload size, for the byte cap and /statusz.
func (r *DVRRing) Bytes() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.bytes
}

// Gops is the retained GOP count, for /statusz.
func (r *DVRRing) Gops() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.gops)
}

// EvictTo drops every GOP older than seq. Exported for tests; production
// eviction is driven by the bounds on append.
func (r *DVRRing) EvictTo(seq int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for len(r.gops) > 0 && r.gops[0].seq < seq {
		r.dropOldestLocked()
	}
}

func (r *DVRRing) findLocked(seq int64) *dvrGop {
	for _, g := range r.gops {
		if g.seq == seq {
			return g
		}
	}
	return nil
}

// evictLocked enforces both bounds. The newest GOP is never evicted, however
// large it is: dropping it would leave the ring with no decodable entry point
// at all, which is worse than briefly exceeding the cap.
func (r *DVRRing) evictLocked(now time.Time) {
	for len(r.gops) > 1 && now.Sub(r.gops[0].at) > r.opts.Window {
		r.dropOldestLocked()
	}
	for len(r.gops) > 1 && r.opts.MaxBytes > 0 && r.bytes > r.opts.MaxBytes {
		r.dropOldestLocked()
	}
}

func (r *DVRRing) dropOldestLocked() {
	r.bytes -= r.gops[0].bytes
	r.gops[0] = nil // let the bytes go before the slice header does
	r.gops = r.gops[1:]
}
