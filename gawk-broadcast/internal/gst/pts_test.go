package gst

import "testing"

// Frame timestamps must carry the *capture* cadence, not the pipe's delivery
// pattern. Arrival stamping (the original Decision 6 shortcut) stamped AUs as
// they came off the child's stdout — after encode, mux and ~64 kB of pipe
// buffering — so timestamps arrived in clumps. The viewer trusts timestamps
// for pacing: clumped stamps inflate its arrival-jitter measurement and the
// R12 adaptive offset, and release decode bursts that spike the decoder queue
// past its bound — whereupon the viewer resyncs and discards every delta
// until the next keyframe (field finding 2026-07-17: native streams
// intermittently cratering viewer decode fps while browser streams on the
// same device are fine). The PES PTS is stamped by pipewiresrc at capture
// delivery, upstream of all of that; ptsAnchor maps it onto the engine clock.
func TestPTSAnchorRecoversCaptureCadenceFromBurstyArrivals(t *testing.T) {
	var a ptsAnchor
	// PTS ticks a clean 60 fps grid (90 kHz units: 1500/frame). Arrivals come
	// in clumps. Feed one frame whose transit is minimal so the anchor
	// settles, then assert the clumped frames' stamps follow the PTS grid.
	const frame90k = 1500 // 90_000 / 60
	stamps := []uint64{
		a.stamp(50_000, 0*frame90k, true),  // transit 50 ms
		a.stamp(51_000, 1*frame90k, true),  // clumped right behind
		a.stamp(52_000, 2*frame90k, true),  // anchor keeps improving
		a.stamp(70_000, 3*frame90k, true),  // this one near-minimal transit
		a.stamp(120_000, 4*frame90k, true), // burst: three frames land
		a.stamp(120_500, 5*frame90k, true), // within a millisecond
		a.stamp(121_000, 6*frame90k, true), // of each other
		a.stamp(140_000, 7*frame90k, true),
	}
	// From frame 3 on the anchor is settled: consecutive deltas must be the
	// capture grid (16_666 µs), regardless of the bursty arrivals.
	for i := 4; i < len(stamps); i++ {
		delta := int64(stamps[i]) - int64(stamps[i-1])
		if delta < 16_600 || delta > 16_700 {
			t.Errorf("frame %d→%d: stamped delta %d µs, want the ~16667 µs capture grid", i-1, i, delta)
		}
	}
	// A stamp must never sit in the engine clock's future.
	if stamps[3] > 70_000 {
		t.Errorf("frame 3 stamped %d, after its own arrival 70000", stamps[3])
	}
}

// No PTS means no cadence to recover: fall back to the arrival stamp rather
// than inventing one.
func TestPTSAnchorFallsBackToArrivalWithoutPTS(t *testing.T) {
	var a ptsAnchor
	if got := a.stamp(42_000, 0, false); got != 42_000 {
		t.Errorf("stamp without PTS = %d, want the arrival 42000", got)
	}
}

// A PTS that jumps far backwards is a new timeline (33-bit wrap after ~26.5 h,
// or a child that restarted its clock): re-anchor instead of producing stamps
// from a stale offset.
func TestPTSAnchorReanchorsOnABackwardsJump(t *testing.T) {
	var a ptsAnchor
	a.stamp(1_000_000, 90_000*100, true) // 100 s into the old timeline
	got := a.stamp(2_000_000, 90, true)  // timeline restarted near zero
	if got != 2_000_000 {
		t.Errorf("stamp after PTS restart = %d, want re-anchored to arrival 2000000", got)
	}
}
