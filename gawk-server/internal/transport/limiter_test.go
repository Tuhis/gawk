package transport

import (
	"testing"
	"time"
)

// TestLimiterSweepEvictsIdleBuckets pins SRV-2: Allow always leaves a bucket
// at most burst-1 tokens and nothing else refills it, so a sweep that only
// compared the stored token count never evicted anything and the map grew
// by one entry per client IP forever.
func TestLimiterSweepEvictsIdleBuckets(t *testing.T) {
	l := newIPRateLimiter(10, 30)
	defer l.Close()

	if !l.Allow("198.51.100.7:1") {
		t.Fatal("first Allow denied")
	}
	l.sweep(time.Now().Add(11 * time.Minute))

	if n := len(l.ips); n != 0 {
		t.Fatalf("len(ips) after sweeping an 11-minute-idle bucket = %d, want 0", n)
	}
}

// TestLimiterSweepKeepsRecentBuckets guards the other side: a bucket idle for
// less than 10 minutes survives the sweep.
func TestLimiterSweepKeepsRecentBuckets(t *testing.T) {
	l := newIPRateLimiter(10, 30)
	defer l.Close()

	l.Allow("198.51.100.7:1")
	l.sweep(time.Now().Add(9 * time.Minute))

	if n := len(l.ips); n != 1 {
		t.Fatalf("len(ips) after sweeping a 9-minute-idle bucket = %d, want 1", n)
	}
}

// TestLimiterSweepKeepsDrainedSlowRefillBuckets: with a rate slow enough that
// 10 idle minutes don't refill the bucket, evicting it would hand the client a
// fresh full bucket early — the sweep must keep it.
func TestLimiterSweepKeepsDrainedSlowRefillBuckets(t *testing.T) {
	l := newIPRateLimiter(0.01, 30) // 11 min * 0.01/s = 6.6 tokens, short of full
	defer l.Close()

	for i := 0; i < 30; i++ {
		l.Allow("198.51.100.7:1")
	}
	l.sweep(time.Now().Add(11 * time.Minute))

	if n := len(l.ips); n != 1 {
		t.Fatalf("len(ips) after sweeping a still-refilling bucket = %d, want 1", n)
	}
}
