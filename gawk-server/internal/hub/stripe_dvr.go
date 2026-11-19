package hub

// Striped DVR delivery (R87, docs/91 §6). A deep-buffer viewer whose single
// connection cannot carry the stream spreads its GOPs over up to
// wire.MaxStripeLegs connections: each GOP still travels whole, keyframe
// then records, on the one connection whose residue it hashes to
// (wire.StripeGOPResidue). The primary serves residue 0 and legs j = 1…N−1
// the rest (D6). Every connection is a full DVR subscriber with its own
// cursor into the shared ring (D3), so one that falls behind resyncs alone.
//
// Like R30's datagram legs, nothing here coordinates across connections or
// pods: a GOP's owner follows from its keyframe's frameID alone, which is
// the same on every pod, so a leg on another pod serves from its own ring
// exactly the GOPs it would serve beside the primary.

import (
	"time"

	"github.com/Tuhis/gawk/gawk-server/wire"
)

// dvrStripeLeg reports a DVR stripe leg: it reads only the ring, so the
// fan-out sends it nothing at all.
func (s *Subscriber) dvrStripeLeg() bool { return s.stripeLeg && s.dvr != nil }

// dvrStripeState is one striped period of a DVR primary: its width, when
// the period began and when it lapses without a refresh (all UnixNano).
// Published whole, never mutated.
type dvrStripeState struct {
	n            int
	since, until int64
}

// publishDVRStripe records a striped StripeState on a DVR primary. A new
// period starts at an arm, after an expiry, or at a new width, whose legs
// are dialed before the state is sent (D11); a refresh of the same width
// keeps the period's start. Only the session's read loop calls this, so
// Load-then-Store is safe; the drain sees the old value or the new one,
// never a mix.
func (s *Subscriber) publishDVRStripe(n int, now, until int64) {
	since := now
	if old := s.dvrStripe.Load(); old != nil && old.until > now && old.n == n {
		since = old.since
	}
	s.dvrStripe.Store(&dvrStripeState{n: n, since: since, until: until})
}

// dvrStripeShare is this DVR connection's residue and stripe width at
// nowNanos, and for a primary the start of its striped period. A leg's
// share is fixed at dial. A primary's is (0, StripeN) while its StripeState
// is fresh and (0, 1), every GOP, otherwise (D7): an expired or lost state
// fails open toward duplicates, never holes.
func (s *Subscriber) dvrStripeShare(nowNanos int64) (member, n int, since int64) {
	if s.stripeLeg {
		return int(s.stripeMember), int(s.stripeN), 0
	}
	st := s.dvrStripe.Load()
	if st == nil || nowNanos >= st.until || st.n < 2 {
		return 0, 1, 0
	}
	return 0, st.n, st.since
}

// dvrServesGop reports whether the GOP at the cursor is this connection's to
// serve. known is false when the GOP is not in the ring (evicted or not yet
// arrived); the drain's own checks handle both.
//
// Two rules keep a GOP from being served by nobody:
//   - A keyframe that did not parse has no frameID to hash. Residue 0 takes
//     it, so the primary (unstriped, or striped as residue 0) serves it.
//   - A primary filters only GOPs that reached the ring after its current
//     striped period began (an arm, or a new width). A leg starts at the
//     newest GOP when it is dialed, which is before the viewer sends the
//     StripeState that arms the primary or changes its width
//     (make-before-break, D11), so every GOP the primary skips is one its
//     current legs cover. A primary that engages while seconds behind live
//     serves its backlog in full instead of skipping GOPs no leg will ever
//     see.
func (s *Subscriber) dvrServesGop() (serve, known bool) {
	member, n, since := s.dvrStripeShare(time.Now().UnixNano())
	if n < 2 {
		return true, true
	}
	id, ok := s.dvr.GopIdentity(s.dvrCursor)
	if !ok {
		return false, false
	}
	if !s.stripeLeg && id.at.UnixNano() < since {
		return true, true
	}
	if !id.kfIDOK {
		return member == 0, true
	}
	return wire.StripeGOPResidue(id.kfID, n) == member, true
}

// dvrSkipGop moves the cursor past a GOP another connection serves, or
// waits for the next GOP to arrive. It returns false when the subscriber is
// stopping.
//
// A skip is idleness, not a stall (D3): a connection whose residue had no
// GOP for a while must not be evicted for it. Except while the connection's
// last write failed: then a skip proves nothing about reachability, and
// counting it would let a dead connection that keeps resyncing onto other
// connections' GOPs dodge DVRProgressTimeout forever.
func (s *Subscriber) dvrSkipGop(wake <-chan struct{}) bool {
	if next, ok := s.dvr.NextGop(s.dvrCursor); ok {
		s.dvrCursor = next
		s.dvrNoteCursor()
	} else {
		select {
		case <-wake:
		case <-s.dvrStop:
			return false
		}
	}
	if !s.dvrFailing {
		s.dvrNoteIdle()
	}
	return true
}

// dvrPaceBps is the live rate this connection's catch-up pacer is scaled
// by: the ring's rate divided by the stripe width (D4), so a stripe's
// connections together never catch up faster than one unstriped viewer.
func (s *Subscriber) dvrPaceBps() int {
	live := s.dvr.LiveRateBps()
	if _, n, _ := s.dvrStripeShare(time.Now().UnixNano()); n > 1 {
		return live / n
	}
	return live
}
