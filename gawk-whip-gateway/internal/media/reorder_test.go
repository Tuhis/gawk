package media

import (
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-whip-gateway/internal/testmedia"
)

var t0 = time.Unix(1_800_000_000, 0)

func ms(n float64) time.Duration { return time.Duration(n * float64(time.Millisecond)) }

// frameStream yields media.Packets for synthetic AUs.
type frameStream struct{ s testmedia.Stream }

func (f *frameStream) packets(a testmedia.AU) []Packet {
	var out []Packet
	for _, p := range f.s.Packets(a) {
		out = append(out, Packet{Seq: p.SequenceNumber, Timestamp: p.Timestamp, Marker: p.Marker, Payload: p.Payload})
	}
	return out
}

// bigP is a delta spanning several packets.
func bigP(seed byte) testmedia.AU { return testmedia.P(3000, seed) }

func pushAll(r *Reorder, pkts []Packet, at time.Time) ReorderResult {
	var res ReorderResult
	for _, p := range pkts {
		x := r.Push(p, at)
		res.Frames = append(res.Frames, x.Frames...)
		res.NACK = append(res.NACK, x.NACK...)
		res.Dropped += x.Dropped
		res.Gap = res.Gap || x.Gap
	}
	return res
}

// WG5 D6: "a whole frame with no incomplete frame ahead of it is never
// delayed" — it leaves on the push of its last packet, at that instant.
func TestD6WholeFrameWithNothingIncompleteAheadIsNeverDelayed(t *testing.T) {
	r := NewReorder(DefaultReorderConfig())
	fs := &frameStream{}
	pk := fs.packets(bigP(1))
	if len(pk) < 3 {
		t.Fatalf("want a multi-packet frame, got %d", len(pk))
	}
	for i, p := range pk {
		res := r.Push(p, t0)
		if i < len(pk)-1 && len(res.Frames) != 0 {
			t.Fatalf("frame released before its marker")
		}
		if i == len(pk)-1 && len(res.Frames) != 1 {
			t.Fatalf("whole frame not released on its marker push: %+v", res)
		}
	}
	if _, ok := r.Deadline(); ok {
		t.Fatal("no deadline should be pending")
	}
}

// WG5 D6: "a NACKed packet that returns within the window recovers its frame".
func TestD6NACKedPacketReturningWithinWindowRecoversFrame(t *testing.T) {
	r := NewReorder(DefaultReorderConfig())
	fs := &frameStream{}
	pk := fs.packets(bigP(1))
	lost := pk[1]
	res := pushAll(r, append([]Packet{pk[0]}, pk[2:]...), t0)
	if len(res.Frames) != 0 {
		t.Fatal("incomplete frame released")
	}
	// NACKed once the reorder tolerance (3 ms) has passed.
	res = r.Tick(t0.Add(3 * time.Millisecond))
	if len(res.NACK) != 1 || res.NACK[0] != lost.Seq {
		t.Fatalf("NACK = %v, want [%d]", res.NACK, lost.Seq)
	}
	if w := r.Window(); w != ms(67.5) {
		t.Fatalf("pre-sample window = %v, want 67.5ms (D6)", w)
	}
	got := r.Push(lost, t0.Add(33*time.Millisecond))
	if len(got.Frames) != 1 || got.Dropped != 0 || got.Frames[0].AfterGap {
		t.Fatalf("retransmit within window did not recover the frame: %+v", got)
	}
	if n := len(got.Frames[0].Payloads); n != len(pk) {
		t.Fatalf("recovered frame has %d packets, want %d", n, len(pk))
	}
	if r.RTT().RTT() != 30*time.Millisecond {
		t.Fatalf("first RTT sample should set the estimate: %v", r.RTT().RTT())
	}
}

// WG5 D6 / WG6: "one past it drops the frame and the deltas to the next IDR"
// — through the whole pipeline, so the drop-until-IDR is the assembler's.
func TestD6PacketPastWindowDropsFrameAndDeltasUntilNextIDR(t *testing.T) {
	p := NewPipeline(PipelineConfig{Reorder: DefaultReorderConfig(), GOP: DefaultGOPConfig()})
	fs := &frameStream{s: testmedia.Stream{FPS: 60}}
	sps := testmedia.SPS(testmedia.SPSOptions{})
	var emitted []bool // keyframe flags of emitted AUs
	feed := func(pkts []Packet, at time.Time) Output {
		var out Output
		for _, pk := range pkts {
			o := p.Video(pk, at, uint64(at.Sub(t0).Microseconds()))
			out.AUs = append(out.AUs, o.AUs...)
			out.WindowDrops += o.WindowDrops
			out.IDRWaitDrops += o.IDRWaitDrops
			if o.Violation != nil {
				t.Fatalf("violation: %v", o.Violation)
			}
		}
		for _, au := range out.AUs {
			emitted = append(emitted, au.Keyframe)
		}
		return out
	}
	now := t0
	feed(fs.packets(testmedia.IDR(sps, 5000)), now)
	feed(fs.packets(bigP(1)), now)
	// The lossy delta: its middle packet never comes back.
	lossy := fs.packets(bigP(2))
	now = now.Add(ms(16))
	feed(append([]Packet{lossy[0]}, lossy[2:]...), now)
	for i := 0; i < 3; i++ { // deltas queued behind it
		now = now.Add(ms(16))
		feed(fs.packets(bigP(byte(3+i))), now)
	}
	if len(emitted) != 2 {
		t.Fatalf("emitted %d AUs before the window closed, want 2", len(emitted))
	}
	// The window closes 67.5 ms after the hole was seen.
	o := p.Tick(t0.Add(ms(16+67.5)), 0)
	if o.WindowDrops != 1 || o.IDRWaitDrops != 3 || len(o.AUs) != 0 {
		t.Fatalf("window close: %+v, want 1 window drop and 3 deltas dropped until the IDR", o)
	}
	now = t0.Add(ms(100))
	feed(fs.packets(bigP(9)), now) // still waiting for an IDR
	feed(fs.packets(testmedia.IDR(sps, 5000)), now)
	feed(fs.packets(bigP(10)), now)
	want := []bool{true, false, true, false}
	if len(emitted) != len(want) {
		t.Fatalf("emitted keyframe flags %v, want %v", emitted, want)
	}
	for i := range want {
		if emitted[i] != want[i] {
			t.Fatalf("emitted keyframe flags %v, want %v", emitted, want)
		}
	}
}

// WG5 D6: "a frame queued behind a loss waits at most one window".
func TestD6FrameQueuedBehindLossWaitsAtMostOneWindow(t *testing.T) {
	r := NewReorder(DefaultReorderConfig())
	fs := &frameStream{}
	a := fs.packets(bigP(1))
	b := fs.packets(bigP(2))
	pushAll(r, append([]Packet{a[0]}, a[2:]...), t0) // hole seen at t0
	res := pushAll(r, b, t0.Add(ms(10)))             // B whole, behind A
	if len(res.Frames) != 0 {
		t.Fatal("B must wait behind the incomplete A")
	}
	dl, ok := r.Deadline()
	if !ok || !dl.Equal(t0.Add(r.Window())) {
		t.Fatalf("deadline = %v %v, want hole time + window", dl, ok)
	}
	if res := r.Tick(dl.Add(-time.Microsecond)); len(res.Frames) != 0 || res.Dropped != 0 {
		t.Fatal("released before the window closed")
	}
	res = r.Tick(dl)
	if res.Dropped != 1 || len(res.Frames) != 1 || !res.Frames[0].AfterGap {
		t.Fatalf("at the deadline A must drop and B leave after the gap: %+v", res)
	}
	if waited := dl.Sub(t0.Add(ms(10))); waited > r.Window() {
		t.Fatalf("B waited %v, more than one window %v", waited, r.Window())
	}
}

// WG5 D6: "the window follows the measured RTT within its clamp".
func TestD6WindowFollowsMeasuredRTTWithinClamp(t *testing.T) {
	cfg := DefaultWindowConfig()
	e := NewRTTEstimator(cfg)
	if e.Window() != ms(67.5) {
		t.Fatalf("initial window %v, want 1.25×50+5 = 67.5ms", e.Window())
	}
	e.Sample(60 * time.Millisecond) // first sample sets the RTT
	if e.RTT() != 60*time.Millisecond || e.Window() != 80*time.Millisecond {
		t.Fatalf("after 60ms: rtt %v window %v, want 60ms / 80ms", e.RTT(), e.Window())
	}
	e.Sample(140 * time.Millisecond) // EWMA α=1/8: 60 + 80/8 = 70
	if e.RTT() != 70*time.Millisecond {
		t.Fatalf("EWMA rtt %v, want 70ms", e.RTT())
	}
	for i := 0; i < 100; i++ {
		e.Sample(time.Second)
	}
	if e.Window() != cfg.Cap {
		t.Fatalf("window %v not capped at %v", e.Window(), cfg.Cap)
	}
	lan := NewRTTEstimator(cfg)
	lan.Sample(time.Millisecond)
	if lan.Window() != cfg.Floor {
		t.Fatalf("LAN window %v, want the floor %v", lan.Window(), cfg.Floor)
	}

	// And through the reorder window itself: a NACK answered after 100 ms
	// sets the RTT, and the next hole waits 1.25×100+5 = 130 ms.
	r := NewReorder(DefaultReorderConfig())
	fs := &frameStream{}
	a := fs.packets(bigP(1))
	pushAll(r, append([]Packet{a[0]}, a[2:]...), t0)
	r.Tick(t0.Add(3 * time.Millisecond)) // the NACK
	r.Push(a[1], t0.Add(103*time.Millisecond))
	if r.Window() != 130*time.Millisecond {
		t.Fatalf("reorder window %v after a 100ms retransmit, want 130ms", r.Window())
	}
}

// A retransmit that arrives after its frame was dropped is still an RTT
// sample (the path is slow; the window should grow).
func TestD6LateRetransmitStillSamplesRTT(t *testing.T) {
	r := NewReorder(DefaultReorderConfig())
	fs := &frameStream{}
	a := fs.packets(bigP(1))
	pushAll(r, append([]Packet{a[0]}, a[2:]...), t0)
	r.Tick(t0.Add(3 * time.Millisecond)) // the NACK
	r.Tick(t0.Add(ms(67.5)))             // the frame drops
	r.Push(a[1], t0.Add(203*time.Millisecond))
	if r.RTT().RTT() != 200*time.Millisecond {
		t.Fatalf("late retransmit not sampled: %v", r.RTT().RTT())
	}
}

// The marker packet of A is lost while B arrives whole: A drops at its
// deadline, and B, whose first packet opens a frame, leaves after the gap.
func TestD6LostMarkerDropsOnlyThatFrame(t *testing.T) {
	r := NewReorder(DefaultReorderConfig())
	fs := &frameStream{}
	a := fs.packets(bigP(1))
	b := fs.packets(bigP(2))
	pushAll(r, a[:len(a)-1], t0)
	pushAll(r, b, t0.Add(ms(5)))
	res := r.Tick(t0.Add(time.Second))
	if res.Dropped != 1 || len(res.Frames) != 1 || !res.Frames[0].AfterGap || len(res.Frames[0].Payloads) != len(b) {
		t.Fatalf("got %+v", res)
	}
}

// B loses its first packet: at the deadline B can't be opened (its first
// present packet doesn't start a frame) and drops too.
func TestD6FrameMissingItsHeadIsDropped(t *testing.T) {
	r := NewReorder(DefaultReorderConfig())
	fs := &frameStream{}
	a := fs.packets(bigP(1))
	b := fs.packets(bigP(2))
	c := fs.packets(bigP(3))
	pushAll(r, a, t0)
	pushAll(r, b[1:], t0)
	pushAll(r, c, t0)
	res := r.Tick(t0.Add(time.Second))
	if res.Dropped != 1 || len(res.Frames) != 1 || !res.Frames[0].AfterGap {
		t.Fatalf("got %+v", res)
	}
}

func TestD6SequenceWrapAndDuplicates(t *testing.T) {
	r := NewReorder(DefaultReorderConfig())
	fs := &frameStream{s: testmedia.Stream{Seq: 65533}}
	pk := fs.packets(bigP(1))
	pk = append(pk, fs.packets(bigP(2))...)
	frames := 0
	for _, p := range pk {
		frames += len(r.Push(p, t0).Frames)
		frames += len(r.Push(p, t0).Frames) // duplicate
	}
	if frames != 2 {
		t.Fatalf("frames across the wrap = %d, want 2", frames)
	}
}

func TestD6BufferBoundDropsHeadWithoutWaiting(t *testing.T) {
	cfg := DefaultReorderConfig()
	cfg.MaxPackets = 8
	r := NewReorder(cfg)
	fs := &frameStream{}
	a := fs.packets(bigP(1))
	pushAll(r, append([]Packet{a[0]}, a[2:]...), t0)
	var dropped int
	for i := 0; i < 4; i++ {
		dropped += pushAll(r, fs.packets(bigP(byte(i))), t0).Dropped
	}
	if dropped == 0 {
		t.Fatal("over the buffer bound the head must drop without waiting")
	}
}

func TestD6SenderRestartResets(t *testing.T) {
	r := NewReorder(DefaultReorderConfig())
	fs := &frameStream{}
	pushAll(r, fs.packets(bigP(1)), t0)
	fs.s.Seq += 20000
	res := pushAll(r, fs.packets(testmedia.IDR(testmedia.SPS(testmedia.SPSOptions{}), 3000)), t0)
	if len(res.Frames) != 1 || !res.Frames[0].AfterGap {
		t.Fatalf("got %+v", res)
	}
}

// D6, PR #688 review round 1: a hole met while dropping a frame can also
// cover the head of the next one. It gets its own window, as any hole does:
// here A loses a middle packet at t0 and, in a later burst, its tail and the
// head of the IDR behind it. When A's window closes, the burst's retransmits
// are still in flight, and they must still recover B.
func TestD6SkippedFrameWaitsOnAHoleThatCoversTheNextFrame(t *testing.T) {
	r := NewReorder(DefaultReorderConfig())
	fs := &frameStream{s: testmedia.Stream{FPS: 60}}
	a := fs.packets(testmedia.P(5000, 1))
	b := fs.packets(testmedia.IDR(testmedia.SPS(testmedia.SPSOptions{}), 5000))
	if len(a) < 4 || len(b) < 3 {
		t.Fatalf("want multi-packet frames, got %d and %d", len(a), len(b))
	}
	last := len(a) - 1
	pushAll(r, append([]Packet{a[0]}, a[2:last]...), t0) // a[1] lost at t0
	r.Tick(t0.Add(ms(3)))                                // its NACK
	pushAll(r, b[2:], t0.Add(ms(50)))                    // a[last], b[0] and b[1] lost at t0+50
	r.Tick(t0.Add(ms(53)))                               // their NACKs
	res := r.Tick(t0.Add(r.Window()))                    // A's window closes
	if res.Dropped != 1 || len(res.Frames) != 0 {
		t.Fatalf("at A's deadline: dropped %d, %d frames; want A dropped and nothing emitted", res.Dropped, len(res.Frames))
	}
	res = pushAll(r, []Packet{a[last], b[0], b[1]}, t0.Add(ms(70))) // retransmits, in their window
	if len(res.Frames) != 1 || res.Frames[0].Timestamp != b[0].Timestamp || len(res.Frames[0].Payloads) != len(b) {
		t.Fatalf("after the retransmits: %d frames, dropped %d; want the whole IDR B emitted", len(res.Frames), res.Dropped)
	}
}

// D6's bound, PR #688 review round 1: with holes in consecutive frames, the
// frame behind them still waits at most one window from its own arrival. A
// hole's window runs from when it was seen, which is when the first packet
// past it arrived, so no hole ahead of a frame is seen after that frame
// arrives. Measured from the first hole, the chain is longer; no frame waits
// longer.
func TestD6ChainedHolesHoldAFrameAtMostOneWindowFromItsArrival(t *testing.T) {
	r := NewReorder(DefaultReorderConfig())
	fs := &frameStream{s: testmedia.Stream{FPS: 60}}
	f1, f2, f3 := fs.packets(bigP(1)), fs.packets(bigP(2)), fs.packets(bigP(3))
	pushAll(r, append([]Packet{f1[0]}, f1[2:]...), t0) // F1's hole, seen at t0
	arrive := t0.Add(ms(60))
	pushAll(r, append([]Packet{f2[0]}, f2[2:]...), arrive) // F2's hole, seen at t0+60
	pushAll(r, f3, arrive)                                 // F3 whole, behind both
	w := r.Window()
	var released time.Time
	for now := t0; now.Before(t0.Add(ms(400))); now = now.Add(time.Millisecond) {
		res := r.Tick(now)
		for _, f := range res.Frames {
			if f.Timestamp == f3[0].Timestamp {
				released = now
			}
		}
		if !released.IsZero() {
			break
		}
	}
	if released.IsZero() {
		t.Fatal("F3 was never released")
	}
	if waited := released.Sub(arrive); waited > w+time.Millisecond {
		t.Fatalf("F3 waited %v from its arrival, more than one window %v", waited, w)
	}
}

// WG11: the window's outcome per NACK is counted, so a session's telemetry
// can tell "retransmits come back too late for the window" (window too
// tight) from "retransmits never come back" (sender ignores NACKs, or the
// loss is a burst): the two need opposite fixes. The slowest NACK →
// arrival since the last read is what the window would have needed.
func TestWG11RetransmitOutcomesAreCounted(t *testing.T) {
	r := NewReorder(DefaultReorderConfig())
	fs := &frameStream{}

	// A: NACKed, answered after 30 ms, inside the 67.5 ms first window.
	a := fs.packets(bigP(1))
	pushAll(r, append([]Packet{a[0]}, a[2:]...), t0)
	r.Tick(t0.Add(3 * time.Millisecond))
	r.Push(a[1], t0.Add(33*time.Millisecond))
	if got := r.RetransmitsRecovered(); got != 1 {
		t.Fatalf("recovered = %d after an in-window retransmit, want 1", got)
	}
	if got := r.TakeRetransmitMax(); got != 30*time.Millisecond {
		t.Fatalf("retransmit max = %v, want 30ms", got)
	}
	if got := r.TakeRetransmitMax(); got != 0 {
		t.Fatalf("retransmit max not reset by the read: %v", got)
	}

	// B: NACKed, its frame dropped when the window (the floor, above
	// 1.25×30+5) closes, the retransmit arriving 80 ms after the NACK.
	now := t0.Add(100 * time.Millisecond)
	b := fs.packets(bigP(2))
	pushAll(r, append([]Packet{b[0]}, b[2:]...), now)
	r.Tick(now.Add(3 * time.Millisecond))
	if res := r.Tick(now.Add(r.Window())); res.Dropped != 1 {
		t.Fatalf("B not dropped at its window: %+v", res)
	}
	r.Push(b[1], now.Add(83*time.Millisecond))
	if got := r.RetransmitsLate(); got != 1 {
		t.Fatalf("late = %d after a retransmit past the window, want 1", got)
	}
	if got := r.RetransmitsRecovered(); got != 1 {
		t.Fatalf("a late retransmit counted as recovered: %d", got)
	}
	if got := r.TakeRetransmitMax(); got != 80*time.Millisecond {
		t.Fatalf("retransmit max = %v, want the late one's 80ms", got)
	}

	// C: NACKed and never answered.
	now = now.Add(200 * time.Millisecond)
	c := fs.packets(bigP(3))
	pushAll(r, append([]Packet{c[0]}, c[2:]...), now)
	r.Tick(now.Add(3 * time.Millisecond))
	r.Tick(now.Add(time.Second))
	if got := r.NACKsSent(); got != 3 {
		t.Fatalf("nacks = %d, want 3", got)
	}
	if rec, late := r.RetransmitsRecovered(), r.RetransmitsLate(); rec != 1 || late != 1 {
		t.Fatalf("recovered %d late %d after an unanswered NACK, want 1 and 1", rec, late)
	}
}
