package main

import (
	"math/rand/v2"
	"sync/atomic"
	"testing"
	"time"
)

// testShaper builds a shaper whose epoch (first packet) was ago in the past
// and whose writer hands packets to the returned channel.
func testShaper(t *testing.T, spec pathSpec, ago time.Duration, armed bool) (*shaper, *proxyCounters, <-chan []byte) {
	t.Helper()
	var epoch atomic.Int64
	epoch.Store(time.Now().Add(-ago).UnixNano())
	c := &proxyCounters{}
	out := make(chan []byte, 1024)
	s := newShaper(spec, rand.New(rand.NewPCG(1, 1)), c, func(time.Time) bool { return armed }, &epoch,
		func(b []byte) { out <- b })
	t.Cleanup(func() { close(s.out) })
	return s, c, out
}

func TestShaperOutageWindow(t *testing.T) {
	s, _, _ := testShaper(t, pathSpec{outagePer: time.Second, outageLen: 200 * time.Millisecond}, 0, true)
	e := time.Unix(0, s.epoch.Load())
	if in, end := s.outageEnd(e.Add(50 * time.Millisecond)); !in || !end.Equal(e.Add(200*time.Millisecond)) {
		t.Fatalf("50ms in: in=%v end=%v; want an outage ending at 200ms", in, end.Sub(e))
	}
	if in, _ := s.outageEnd(e.Add(500 * time.Millisecond)); in {
		t.Fatal("500ms in is between outages")
	}
	if in, end := s.outageEnd(e.Add(2*time.Second + 100*time.Millisecond)); !in || !end.Equal(e.Add(2*time.Second+200*time.Millisecond)) {
		t.Fatalf("2.1s in: in=%v end=%v; the outage repeats every period", in, end.Sub(e))
	}
	off, _, _ := testShaper(t, pathSpec{outagePer: time.Second}, 0, true)
	if in, _ := off.outageEnd(time.Now()); in {
		t.Fatal("a zero -outage-len still made an outage")
	}
}

// In drop mode an outage eats the packet; in hold mode it leaves at the
// outage's end plus the path delay.
func TestShaperOutageDropAndHold(t *testing.T) {
	spec := pathSpec{outagePer: time.Hour, outageLen: time.Hour}
	s, c, out := testShaper(t, spec, time.Minute, true)
	s.offer([]byte{1})
	if c.outageDrop.Load() != 1 || c.forwarded.Load() != 0 {
		t.Fatalf("drop mode: dropped %d, forwarded %d", c.outageDrop.Load(), c.forwarded.Load())
	}
	select {
	case <-out:
		t.Fatal("a dropped packet was written")
	case <-time.After(20 * time.Millisecond):
	}

	spec = pathSpec{outagePer: time.Second, outageLen: 100 * time.Millisecond, outageHold: true, delay: 5 * time.Millisecond}
	h, hc, hout := testShaper(t, spec, 0, true)
	start := time.Now()
	h.offer([]byte{2})
	if hc.outageHeld.Load() != 1 || hc.forwarded.Load() != 1 {
		t.Fatalf("hold mode: held %d, forwarded %d", hc.outageHeld.Load(), hc.forwarded.Load())
	}
	select {
	case b := <-hout:
		if b[0] != 2 {
			t.Fatalf("wrote %v", b)
		}
		if el := time.Since(start); el < 90*time.Millisecond {
			t.Fatalf("held packet left after %v, before the outage ended", el)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("held packet never left")
	}
}

// Before arming nothing is impaired, however lossy the spec.
func TestShaperUnarmedForwardsEverything(t *testing.T) {
	s, c, out := testShaper(t, pathSpec{loss: 1, outagePer: time.Hour, outageLen: time.Hour}, time.Minute, false)
	buf := []byte{1, 2, 3}
	s.offer(buf)
	buf[0] = 9 // the shaper copied it
	b := <-out
	if c.forwarded.Load() != 1 || b[0] != 1 {
		t.Fatalf("forwarded %d, wrote %v", c.forwarded.Load(), b)
	}
}

func TestShaperBottleneckQueueDrops(t *testing.T) {
	// 8 kbit/s: a 100-byte packet takes 100ms; the queue holds 150ms.
	s, c, out := testShaper(t, pathSpec{rateBps: 8000, queueLimit: 150 * time.Millisecond}, time.Minute, true)
	for range 4 {
		s.offer(make([]byte, 100))
	}
	// Departures 100, 200 (queued 100ms), then 300 (queued 200ms > 150) drops.
	if c.forwarded.Load() != 2 || c.queueDrop.Load() != 2 {
		t.Fatalf("forwarded %d, queue drops %d; want 2 and 2", c.forwarded.Load(), c.queueDrop.Load())
	}
	start := time.Now()
	<-out
	<-out
	if el := time.Since(start); el < 150*time.Millisecond {
		t.Fatalf("two packets at 8 kbit/s left within %v", el)
	}
}

// Jitter delays packets but never reorders them.
func TestShaperJitterKeepsOrder(t *testing.T) {
	s, _, out := testShaper(t, pathSpec{jitter: 3 * time.Millisecond}, time.Minute, true)
	for i := range 50 {
		s.offer([]byte{byte(i)})
	}
	for i := range 50 {
		if b := <-out; b[0] != byte(i) {
			t.Fatalf("packet %d left as %d", i, b[0])
		}
	}
}

func TestShaperRandomLoss(t *testing.T) {
	s, c, _ := testShaper(t, pathSpec{loss: 0.5}, time.Minute, true)
	for range 2000 {
		s.offer([]byte{0})
	}
	if l := c.randomLoss.Load(); l < 800 || l > 1200 {
		t.Fatalf("Bernoulli 0.5 lost %d of 2000", l)
	}

	// Gilbert-Elliott, a bad state that loses everything: losses come in
	// bursts, and never above the bad-state share.
	g, gc, _ := testShaper(t, pathSpec{loss: 0.99, geGood2Bad: 0.05, geBad2Good: 0.2, geBadLoss: 1}, time.Minute, true)
	runs, prev := 0, false
	for range 4000 {
		before := gc.randomLoss.Load()
		g.offer([]byte{0})
		lost := gc.randomLoss.Load() > before
		if lost && !prev {
			runs++
		}
		prev = lost
	}
	l := gc.randomLoss.Load()
	// Stationary bad share 0.05/(0.05+0.2) = 20%; -loss is ignored under GE.
	if l < 500 || l > 1200 {
		t.Fatalf("GE lost %d of 4000, want ~800 (and not -loss's 99%%)", l)
	}
	if float64(l)/float64(runs) < 2.5 {
		t.Fatalf("GE losses: %d in %d runs; want bursts (mean run ~5)", l, runs)
	}
}

// With episodes, random loss applies only in the first episodeLen of each
// episodePer after arming.
func TestShaperLossEpisodes(t *testing.T) {
	spec := pathSpec{loss: 1, episodePer: time.Hour, episodeLen: time.Minute, armAfter: 3 * time.Second}
	in, _, _ := testShaper(t, spec, 30*time.Second, true)
	if !in.lost(time.Now()) {
		t.Fatal("27s after arming is inside the first episode")
	}
	before, _, _ := testShaper(t, spec, time.Second, true)
	if before.lost(time.Now()) {
		t.Fatal("before -arm-after there is no episode")
	}
	after, _, _ := testShaper(t, spec, 2*time.Minute, true)
	if after.lost(time.Now()) {
		t.Fatal("2 minutes in is past the minute-long episode")
	}
	none, _, _ := testShaper(t, pathSpec{}, time.Minute, true)
	if none.lost(time.Now()) {
		t.Fatal("a lossless spec lost a packet")
	}
}

func TestSleepPreciseShort(t *testing.T) {
	start := time.Now()
	sleepPrecise(500 * time.Microsecond)
	if el := time.Since(start); el < 500*time.Microsecond {
		t.Fatalf("slept %v, want >= 500µs", el)
	}
}
