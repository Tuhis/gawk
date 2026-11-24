package hub

// R21 DV3 (docs/26 Decision 6): the per-subscriber catch-up ceiling. A viewer
// recovering from a stall must send faster than live to close its backlog —
// that is the whole point — but not without bound: the egress budget is one
// process-wide bucket shared with every other viewer on the pod, and a network
// event that stalls one viewer usually stalls them all, so they all try to
// recover at once.

import (
	"testing"
	"time"
)

func TestDVRPacerAllowsUpToTheMultipleOfLiveRate(t *testing.T) {
	now := time.Now()
	p := newDVRPacer(4.0, func() time.Time { return now })

	// Live rate 100 kB/s ⇒ ceiling 400 kB/s. Over one second the pacer must
	// hand out ~400 kB and no more.
	const live = 100_000
	var granted int
	for range 1000 {
		if !p.allow(1000, live) {
			now = now.Add(time.Millisecond)
			continue
		}
		granted += 1000
	}
	if ceiling := 4*float64(live) + p.burst(live); float64(granted) > ceiling {
		t.Errorf("granted %d bytes in ~1 s, want <= %.0f (4x live plus one burst)", granted, ceiling)
	}
	if granted < 2*live {
		t.Errorf("granted only %d bytes, want well above the live rate — a ceiling that blocks catch-up is not a ceiling", granted)
	}
}

func TestDVRPacerDoesNotThrottleAtLiveRate(t *testing.T) {
	// The steady state: a cursor at the live edge writes exactly what arrives.
	// Throttling there would turn the ceiling into a bug that manufactures the
	// backlog it exists to bound.
	now := time.Now()
	p := newDVRPacer(4.0, func() time.Time { return now })
	const live = 100_000
	for range 100 {
		now = now.Add(10 * time.Millisecond) // 1 s total
		if !p.allow(live/100, live) {
			t.Fatal("a subscriber writing at exactly the live rate was throttled")
		}
	}
}

func TestDVRPacerDisabledAndUnknownRate(t *testing.T) {
	now := time.Now()
	// A non-positive multiple disables the ceiling entirely.
	off := newDVRPacer(0, func() time.Time { return now })
	for range 100 {
		if !off.allow(1<<20, 100_000) {
			t.Fatal("a disabled pacer throttled")
		}
	}
	// An unknown live rate (too little history to estimate) must not throttle:
	// guessing low here would stall every viewer on a fresh broadcast.
	p := newDVRPacer(4.0, func() time.Time { return now })
	for range 100 {
		if !p.allow(1<<20, 0) {
			t.Fatal("an unknown live rate throttled")
		}
	}
}

// A write larger than the whole burst must still pass once the bucket is
// full, or a keyframe bigger than a quarter second at the ceiling wedges the
// cursor for good. A striped connection's ceiling is 1/N of an unstriped one
// (docs/91 D4), which puts ordinary keyframes in that range.
func TestDVRPacerPassesAWriteLargerThanTheBurst(t *testing.T) {
	now := time.Now()
	p := newDVRPacer(4.0, func() time.Time { return now })
	const live = 100_000
	big := int(2 * p.burst(live))
	passed := false
	for range 100 {
		if p.allow(big, live) {
			passed = true
			break
		}
		now = now.Add(100 * time.Millisecond)
	}
	if !passed {
		t.Fatalf("a %d-byte write never passed a pacer whose burst is %.0f", big, p.burst(live))
	}
	// The overdraft is debt, not a free pass: the next write waits until
	// the ceiling's rate has repaid it.
	if p.allow(1, live) {
		t.Fatal("a write passed straight after an overdraft")
	}
	now = now.Add(time.Duration(float64(big) / (4 * live) * float64(time.Second)))
	if !p.allow(1, live) {
		t.Fatal("the overdraft was not repaid at the ceiling's rate")
	}
}
