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

	"github.com/Tuhis/gawk/gawk-server/wire"
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
	// Closed and replaced on every append; see Wait.
	wake chan struct{}
}

func NewDVRRing(opts DVROptions) *DVRRing {
	return &DVRRing{opts: opts, wake: make(chan struct{})}
}

// Wait returns a channel closed by the next append. A drain that has consumed
// everything currently in its GOP selects on this rather than polling — take
// the channel BEFORE the final read, or an append landing in between is missed
// and the drain sleeps on data that already arrived.
func (r *DVRRing) Wait() <-chan struct{} {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.wake
}

// wakeLocked releases everyone waiting and arms the next wait. Called on every
// append, under the write lock.
func (r *DVRRing) wakeLocked() {
	close(r.wake)
	r.wake = make(chan struct{})
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
	r.wakeLocked()
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
	r.wakeLocked()
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

// --- DV2: the cursor drain -------------------------------------------------

// dvrRetryFloor keeps the drain from spinning on failures that return
// instantly (egress cap, refused stream open). A parked write already costs a
// full carrierWriteTimeout and needs no floor of its own.
const dvrRetryFloor = 20 * time.Millisecond

// drainDVR is the R21 replacement for drainReliable on a DVR subscriber. It
// reads the broadcast's ring at this subscriber's own cursor instead of a
// per-subscriber queue, so a stalled write costs delay rather than data: the
// ring keeps growing behind it, and when the link returns the drain simply
// resumes where it was.
//
// Each GOP is served exactly as the live path serves one — keyframe on its own
// stream, then length-prefixed records on a carrier — so a replayed GOP is
// byte-identical on the wire to a live one and the viewer needs no concept of
// replay (docs/26 Decision 2).
func (s *Subscriber) drainDVR() {
	defer s.retireCarrier()
	var scratch []byte
	for {
		if s.closed.Load() {
			return
		}
		// Take the wake channel BEFORE reading, so an append landing between
		// the read and the wait is not missed.
		wake := s.dvr.Wait()

		if s.dvrFellBehind() {
			s.dvrResync()
			continue
		}
		if s.dvrCursor.NeedsKeyframe() {
			if !s.dvrSendKeyframe() {
				select {
				case <-wake:
				case <-s.dvrStop:
					return
				}
				continue
			}
			continue
		}
		rec, ok := s.dvr.Record(s.dvrCursor)
		if ok {
			if s.dvrWriteRecord(rec, &scratch) {
				continue
			}
			// The write did not land — a parked carrier, a failed open, or the
			// egress cap. Do NOT skip the GOP: waiting is the entire point of
			// the ring, the record is still in it, and the two checks at the
			// top of this loop (fell off the tail, lag past the viewer's
			// buffer) are what bound the wait. Skipping here reintroduces
			// exactly the loss R21 exists to remove — and did, until the
			// control subscriber in TestDVRSubscriberLosesNothingAcrossAStall
			// caught it. writeCarrier already cancelled the dead stream, so
			// the next attempt opens a fresh one.
			//
			// Pacing: a parked write costs a full carrierWriteTimeout, so the
			// loop is self-limiting there. The cheap failures (over cap, open
			// refused) return instantly, hence the short floor.
			select {
			case <-wake:
			case <-s.dvrStop:
				return
			case <-time.After(dvrRetryFloor):
			}
			continue
		}
		// Out of records: either this GOP is still growing (wait) or it is
		// complete and the next one is ready (advance). Conflating the two
		// either skips live records or wedges at a GOP boundary.
		if s.dvr.GopComplete(s.dvrCursor) {
			if next, advanced := s.dvr.NextGop(s.dvrCursor); advanced {
				s.retireCarrier()
				s.dvrCursor = next
				s.dvrNoteCursor()
				continue
			}
		}
		select {
		case <-wake:
		case <-s.dvrStop:
			return
		}
	}
}

// drainControlSideband delivers a DVR subscriber's non-video datagrams —
// ClockMapping, the R18 ViewerCount keepalive, DecoderConfig — over the
// unreliable path, immediately. They must never queue behind the video cursor:
// the keepalive in particular is what a viewer's dead-session watchdog reads as
// proof its session is alive (BUGS.md), and a DVR cursor is *designed* to sit
// seconds behind.
func (s *Subscriber) drainControlSideband() {
	for dgram := range s.queue {
		s.sendSidebandDatagram(dgram)
	}
}

// dvrFellBehind reports the two ways a cursor stops being worth serving: the
// ring evicted its GOP, or it has fallen further behind than the viewer's
// declared buffer, so everything it would replay is already past due there
// (docs/26 Decisions 4 and 7).
func (s *Subscriber) dvrFellBehind() bool {
	if s.dvr.FellOffTail(s.dvrCursor) {
		return true
	}
	lag := s.dvr.LagMs(s.dvrCursor, time.Now())
	s.dvrLagMs.Store(lag)
	return s.dvrBufferMs > 0 && lag > int64(s.dvrBufferMs)
}

// dvrResync jumps the cursor to the newest keyframe — the mode's only frame
// loss, and the one signal operators should watch (docs/26 Decision 4).
func (s *Subscriber) dvrResync() {
	s.retireCarrier()
	s.dvrCursor = s.dvr.ResyncCursor()
	s.dvrResyncs.Add(1)
	s.dvrNoteCursor()
}

func (s *Subscriber) dvrNoteCursor() {
	s.dvrGopSeq.Store(s.dvrCursor.GopSeq())
}

// dvrSendKeyframe writes the cursor's keyframe on its own stream, reusing the
// live path's accounting. Returns false when the keyframe is not available yet
// (the caller waits) — a failed *write* still advances, because the GOP's
// records are useless without it and blocking here would wedge the cursor.
func (s *Subscriber) dvrSendKeyframe() bool {
	msg, ok := s.dvr.Keyframe(s.dvrCursor)
	if !ok {
		return false
	}
	if !s.hub.registry.consumeBandwidth(len(msg)) {
		s.kfDroppedBandwidth.Add(1)
		s.hub.countBandwidthDropBytes(len(msg))
		s.dvrCursor = s.dvrCursor.AtFirstRecord()
		s.dvrNoteCursor()
		return true
	}
	st, err := s.sender.OpenKeyframeStream()
	if err != nil {
		s.kfDroppedOpenFailed.Add(1)
		s.dvrCursor = s.dvrCursor.AtFirstRecord()
		s.dvrNoteCursor()
		return true
	}
	_ = st.SetWriteDeadline(time.Now().Add(s.hub.registry.opts.KeyframeWriteTimeout))
	if _, err := st.Write(msg); err != nil {
		st.CancelWrite()
		s.kfDroppedSlow.Add(1)
	} else if err := st.Close(); err != nil {
		st.CancelWrite()
		s.kfDroppedSlow.Add(1)
	} else {
		s.keyframesSent.Add(1)
		s.egressKeyframeBytes.Add(uint64(len(msg)))
	}
	s.dvrCursor = s.dvrCursor.AtFirstRecord()
	s.dvrNoteCursor()
	return true
}

// dvrWriteRecord frames and writes one record, opening this GOP's carrier
// lazily. Returns false when the carrier could not take it.
func (s *Subscriber) dvrWriteRecord(rec []byte, scratch *[]byte) bool {
	framed, err := wire.AppendCarrierRecord((*scratch)[:0], rec)
	if err != nil {
		s.carrierRecordsDropped.Add(1)
		s.dvrCursor = s.dvrCursor.Next()
		return true
	}
	*scratch = framed
	needsOpen := s.currentCarrier() == nil
	n := len(framed)
	if needsOpen {
		n += wire.CarrierPrologueSize
	}
	if !s.hub.registry.consumeBandwidth(n) {
		s.dropped.Add(1)
		s.hub.countBandwidthDrop(n)
		return false
	}
	deadline := time.Now().Add(s.hub.registry.opts.carrierWriteTimeout())
	if needsOpen && !s.openCarrier(deadline) {
		return false
	}
	if !s.writeCarrier(framed, deadline) {
		return false
	}
	s.carrierRecords.Add(1)
	s.egressCarrierBytes.Add(uint64(len(framed)))
	s.dvrCursor = s.dvrCursor.Next()
	return true
}

// DVRResyncs is how many times this subscriber fell off the ring's tail — the
// mode's only frame loss (docs/26 Decision 4).
func (s *Subscriber) DVRResyncs() uint64 { return s.dvrResyncs.Load() }

// DVRLagMs is how far behind the live edge this subscriber's cursor sits.
func (s *Subscriber) DVRLagMs() int64 { return s.dvrLagMs.Load() }
