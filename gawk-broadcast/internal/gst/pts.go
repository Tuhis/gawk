package gst

// ptsAnchor maps PES PTS onto the engine clock, so frame timestamps carry the
// *capture* cadence instead of the pipe's delivery pattern (Decision 6's
// upgrade path, taken 2026-07-17).
//
// Why it exists: arrival stamping reads the clock when an AU comes off the
// child's stdout — after encode, mux and ~64 kB of pipe buffering — so
// timestamps clump. The viewer trusts timestamps for pacing (R12): clumped
// stamps inflate its arrival-jitter measurement and the adaptive playout
// offset, and schedule decode bursts that spike the decoder queue past its
// bound, whereupon the viewer resyncs and discards every delta until the next
// keyframe — decoded fps intermittently cratering on native streams while
// browser streams (capture-stamped at source) were fine on the same device.
// The PES PTS is stamped by pipewiresrc (do-timestamp=true) at capture
// delivery, upstream of all of that buffering.
//
// The mapping: keep the minimum observed (arrival − pts). Queuing delay only
// ever adds, so the minimum approaches the true constant bias and the stamped
// timestamps stay on the engine clock — the same clock TimeSync reads, which
// is what keeps the viewer's capture→render latency math valid. By
// construction pts + minOffset ≤ that frame's own arrival, so a stamp never
// sits in the engine clock's future. Clock drift between the child's clock
// and ours is ppm-scale (ms per hour); the viewer's live-edge baseline is a
// 60 s windowed min and absorbs it.
//
// Owned by the pump goroutine of one child: a new cascade attempt gets a
// fresh handle, so a fresh anchor.
type ptsAnchor struct {
	haveOffset bool
	offsetUs   int64
	lastPtsUs  int64
}

// ptsReanchorGapUs: a PTS this far behind its predecessor is a new timeline
// (the 33-bit wrap after ~26.5 h, or a child clock restart), not reordering —
// the pipeline has no B-frames, so PTS order is delivery order.
const ptsReanchorGapUs = 5_000_000

// stamp returns the engine-clock timestamp for one AU.
func (a *ptsAnchor) stamp(arrivalUs uint64, pts90k uint64, hasPTS bool) uint64 {
	if !hasPTS {
		// No cadence to recover; the arrival stamp is the honest fallback.
		return arrivalUs
	}
	ptsUs := int64(pts90k) * 1000 / 90
	if a.haveOffset && ptsUs+ptsReanchorGapUs < a.lastPtsUs {
		a.haveOffset = false
	}
	a.lastPtsUs = ptsUs

	offset := int64(arrivalUs) - ptsUs
	if !a.haveOffset || offset < a.offsetUs {
		a.offsetUs, a.haveOffset = offset, true
	}
	ts := ptsUs + a.offsetUs
	if ts < 0 {
		ts = 0
	}
	return uint64(ts)
}
