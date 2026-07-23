package hub

// R21 DV1 (docs/26): the per-broadcast DVR ring and the cursors into it.
// Everything here is pure — no sessions, no I/O — because the ring's whole job
// is to be a correct data structure under concurrent append and N readers.

import (
	"bytes"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/wire"
)

func gopBytes(n int) []byte { return bytes.Repeat([]byte{byte(n)}, 16) }

func recordBytes(gop, idx int) []byte {
	return []byte(fmt.Sprintf("g%03d-r%03d", gop, idx))
}

// appendGop writes one complete GOP: a keyframe, then n delta records.
func appendGop(r *DVRRing, seq int, records int, at time.Time) {
	r.AppendKeyframe(gopBytes(seq), at)
	for i := range records {
		r.AppendRecord(recordBytes(seq, i), at)
	}
}

func TestDVRRingReplaysGopsVerbatim(t *testing.T) {
	r := NewDVRRing(DVROptions{Window: time.Minute, MaxBytes: 1 << 20})
	t0 := time.Now()
	// Take the cursor while GOP 0 is the newest, then let the broadcast run
	// on: that is exactly the state a subscriber is in after a stall, and it
	// is the only way to hold a cursor into history (a joiner always starts at
	// the live edge).
	appendGop(r, 0, 4, t0)
	c := r.NewCursor()
	for g := 1; g < 3; g++ {
		appendGop(r, g, 4, t0)
	}

	for g := range 3 {
		kf, ok := r.Keyframe(c)
		if !ok {
			t.Fatalf("gop %d: no keyframe at cursor", g)
		}
		if !bytes.Equal(kf, gopBytes(g)) {
			t.Fatalf("gop %d keyframe = %x, want %x", g, kf, gopBytes(g))
		}
		c = c.AtFirstRecord()
		for i := range 4 {
			rec, ok := r.Record(c)
			if !ok {
				t.Fatalf("gop %d record %d missing", g, i)
			}
			if !bytes.Equal(rec, recordBytes(g, i)) {
				t.Fatalf("gop %d record %d = %q, want %q", g, i, rec, recordBytes(g, i))
			}
			c = c.Next()
		}
		if _, ok := r.Record(c); ok {
			t.Fatalf("gop %d yielded a 5th record", g)
		}
		var advanced bool
		c, advanced = r.NextGop(c)
		if g < 2 && !advanced {
			t.Fatalf("gop %d: cursor would not advance to the next gop", g)
		}
	}
}

func TestDVRRingOwnsItsBytes(t *testing.T) {
	// The ring must not alias the caller's buffer. Today's per-subscriber queue
	// already assumes per-datagram ownership; DV1 makes that checked rather
	// than assumed, because a ring holds bytes for seconds rather than
	// milliseconds and would turn a reused buffer into silent corruption.
	r := NewDVRRing(DVROptions{Window: time.Minute, MaxBytes: 1 << 20})
	t0 := time.Now()
	kf := []byte("KEYFRAME")
	rec := []byte("RECORD")
	r.AppendKeyframe(kf, t0)
	r.AppendRecord(rec, t0)

	for i := range kf {
		kf[i] = 'X'
	}
	for i := range rec {
		rec[i] = 'X'
	}

	c := r.NewCursor()
	got, _ := r.Keyframe(c)
	if !bytes.Equal(got, []byte("KEYFRAME")) {
		t.Errorf("keyframe = %q, want KEYFRAME — the ring aliased the caller's buffer", got)
	}
	gotRec, _ := r.Record(c.AtFirstRecord())
	if !bytes.Equal(gotRec, []byte("RECORD")) {
		t.Errorf("record = %q, want RECORD — the ring aliased the caller's buffer", gotRec)
	}
}

func TestDVRRingEvictsByWindow(t *testing.T) {
	r := NewDVRRing(DVROptions{Window: 2 * time.Second, MaxBytes: 1 << 20})
	t0 := time.Now()
	// One GOP per 500 ms, six of them (t0 … t0+2500 ms). The window is measured
	// against the newest GOP and the edge is inclusive, so GOP 1 at t0+500 is
	// exactly 2000 ms old and stays; only GOP 0 is dropped.
	for g := range 6 {
		appendGop(r, g, 2, t0.Add(time.Duration(g)*500*time.Millisecond))
	}
	if got := r.OldestGopSeq(); got != 1 {
		t.Errorf("OldestGopSeq = %d, want 1 (2 s window, inclusive edge)", got)
	}
	// One more GOP pushes the frontier along by exactly one.
	appendGop(r, 6, 2, t0.Add(3*time.Second))
	if got := r.OldestGopSeq(); got != 2 {
		t.Errorf("OldestGopSeq = %d, want 2 after the window slid", got)
	}
}

func TestDVRRingEvictsByBytesWhicheverBindsFirst(t *testing.T) {
	// A byte cap well below what the window would allow: it must bind instead.
	// This is the bound that protects the pod from a high-bitrate broadcaster.
	r := NewDVRRing(DVROptions{Window: time.Hour, MaxBytes: 200})
	t0 := time.Now()
	for g := range 20 {
		appendGop(r, g, 4, t0)
	}
	if r.Bytes() > 200 {
		t.Errorf("ring holds %d bytes, want <= 200 — the byte cap did not bind", r.Bytes())
	}
	if r.OldestGopSeq() == 0 {
		t.Error("nothing was evicted despite the byte cap")
	}
}

func TestDVRRingCursorFallsOffTheTail(t *testing.T) {
	r := NewDVRRing(DVROptions{Window: time.Hour, MaxBytes: 1 << 20})
	t0 := time.Now()
	appendGop(r, 0, 2, t0)
	c := r.NewCursor()

	// Evict everything the cursor pointed at.
	for g := 1; g < 40; g++ {
		appendGop(r, g, 2, t0)
	}
	r.EvictTo(30)

	if _, ok := r.Keyframe(c); ok {
		t.Fatal("a cursor into an evicted GOP still resolved — that is a read of freed history")
	}
	if !r.FellOffTail(c) {
		t.Fatal("FellOffTail did not report the stale cursor")
	}
	// Resync lands on the newest complete GOP, which is the only decodable
	// entry point (docs/26 Decision 4).
	c = r.ResyncCursor()
	kf, ok := r.Keyframe(c)
	if !ok {
		t.Fatal("resynced cursor does not resolve")
	}
	if !bytes.Equal(kf, gopBytes(39)) {
		t.Errorf("resync landed on %x, want the newest GOP %x", kf, gopBytes(39))
	}
}

func TestDVRRingLagTracksTheLiveEdge(t *testing.T) {
	r := NewDVRRing(DVROptions{Window: time.Hour, MaxBytes: 1 << 20})
	t0 := time.Now()
	appendGop(r, 0, 2, t0)
	c := r.NewCursor()
	if lag := r.LagMs(c, t0); lag != 0 {
		t.Errorf("fresh cursor lag = %d ms, want 0", lag)
	}
	appendGop(r, 1, 2, t0.Add(2*time.Second))
	if lag := r.LagMs(c, t0.Add(2*time.Second)); lag != 2000 {
		t.Errorf("lag = %d ms, want 2000 — a cursor two seconds behind the newest GOP", lag)
	}
}

func TestDVRRingConcurrentAppendAndReaders(t *testing.T) {
	// The shape the drain actually runs in: one appender under the registry
	// lock, N drains reading their own cursors. Run under -race.
	r := NewDVRRing(DVROptions{Window: time.Hour, MaxBytes: 1 << 22})
	t0 := time.Now()
	const gops = 60
	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for g := range gops {
			appendGop(r, g, 8, t0)
		}
		close(stop)
	}()

	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := r.NewCursor()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, ok := r.Record(c); ok {
					c = c.Next()
					continue
				}
				if next, ok := r.NextGop(c); ok {
					c = next.AtFirstRecord()
				}
			}
		}()
	}
	wg.Wait()
}

// DV2's headline property (docs/26): a DVR subscriber whose link stalls loses
// nothing. The relay keeps appending to the ring while the write is parked,
// and when the link returns the drain resumes FROM THE CURSOR — so every delta
// captured during the stall is still delivered, in order, verbatim.
//
// This is the exact scenario the 2026-07-22 capture froze on, where the 500 ms
// carrier deadline killed the GOP and docs/24 finding 17's purge discarded it.
func TestDVRSubscriberLosesNothingAcrossAStall(t *testing.T) {
	r := NewRegistry(discardLog, Options{
		DVR: DVROptions{Window: 30 * time.Second, MaxBytes: 1 << 20},
	})
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}

	f := &fakeSender{}
	block := make(chan struct{})
	f.setCarBlock(block) // the link is down: every carrier write parks
	sub, err := r.SubscribeDVR(id, f, 3000)
	if err != nil {
		t.Fatalf("SubscribeDVR: %v", err)
	}

	// Same-conditions control: an R19 reliable subscriber, equally stalled.
	// Without it this test proves the property but not the difference — and
	// the difference is the entire milestone.
	control := &fakeSender{}
	controlBlock := make(chan struct{})
	control.setCarBlock(controlBlock)
	if _, err := r.SubscribeReliable(id, control); err != nil {
		t.Fatalf("SubscribeReliable: %v", err)
	}

	// Four GOPs go by while the link is down — 2 s at the 500 ms default.
	var want [][]byte
	for g := range 4 {
		ingestKeyframe(t, p, keyframeMsg(t, uint32(g*100), "vp8", "KEY"))
		for i := range 5 {
			d := chunkDgram(t, false, uint32(g*100+i+1), 0, 1, fmt.Sprintf("g%d-d%d", g, i))
			want = append(want, d)
			p.HandleDatagram(d)
		}
	}

	// The stall must outlive CarrierWriteTimeout, or the old path never gets
	// the chance to fail and the control proves nothing.
	time.Sleep(CarrierWriteTimeout + 200*time.Millisecond)

	// Both links come back.
	close(block)
	close(controlBlock)

	waitFor(t, 10*time.Second, func() bool { return len(f.carrierRecords(t)) >= len(want) },
		"every stalled delta to be replayed from the ring")

	got := f.carrierRecords(t)
	if len(got) != len(want) {
		t.Fatalf("delivered %d records, want %d", len(got), len(want))
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Fatalf("record %d = %q, want %q — the replay is not verbatim or not in order", i, got[i], want[i])
		}
	}
	if n := sub.DVRResyncs(); n != 0 {
		t.Errorf("dvrResyncs = %d, want 0 — nothing should have fallen off a 30 s ring", n)
	}

	// The control lost GOPs to the 500 ms carrier deadline, which is exactly
	// the behaviour R21 exists to replace. If this ever stops being true, the
	// assertion above has stopped distinguishing the two paths.
	if got := len(control.carrierRecords(t)); got >= len(want) {
		t.Errorf("control delivered %d/%d records — it was supposed to lose some", got, len(want))
	}
}

// A stall longer than the ring is the mode's one frame loss (docs/26
// Decision 4): the cursor falls off the tail and the subscriber is resynced to
// the newest keyframe. It must recover there rather than wedge or spin.
func TestDVRSubscriberResyncsWhenItFallsOffTheTail(t *testing.T) {
	r := NewRegistry(discardLog, Options{
		// A ring far too small to hold what follows.
		DVR: DVROptions{Window: 30 * time.Second, MaxBytes: 512},
	})
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	f := &fakeSender{}
	block := make(chan struct{})
	f.setCarBlock(block)
	sub, err := r.SubscribeDVR(id, f, 3000)
	if err != nil {
		t.Fatalf("SubscribeDVR: %v", err)
	}

	for g := range 12 {
		ingestKeyframe(t, p, keyframeMsg(t, uint32(g*100), "vp8", "KEY"))
		for i := range 8 {
			p.HandleDatagram(chunkDgram(t, false, uint32(g*100+i+1), 0, 1, fmt.Sprintf("g%02d-d%d", g, i)))
		}
	}
	close(block)

	waitFor(t, 10*time.Second, func() bool { return sub.DVRResyncs() > 0 },
		"the subscriber to notice it fell off the tail")
	// Recovery, not a wedge: fresh content still flows afterwards.
	ingestKeyframe(t, p, keyframeMsg(t, 9000, "vp8", "KEY"))
	p.HandleDatagram(chunkDgram(t, false, 9001, 0, 1, "after-resync"))
	waitFor(t, 10*time.Second, func() bool {
		for _, rec := range f.carrierRecords(t) {
			if bytes.Contains(rec, []byte("after-resync")) {
				return true
			}
		}
		return false
	}, "delivery to resume after the resync")
}

// DV3 (docs/26 Decision 7): the `buffer` param is a hint from a client that
// may be older, newer or simply wrong, so NO value may reject the session.
// Below the minimum it downgrades to plain carrier delivery; above the ring it
// clamps; garbage reads as absent.
func TestDVRBufferNegotiation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		raw      string
		wantMode wire.DeliveryMode
		wantMs   int
	}{
		{"absent", "", wire.DeliveryReliable, 0},
		{"below minimum downgrades", "150", wire.DeliveryReliable, 0},
		{"exactly the minimum", "1000", wire.DeliveryDVR, 1000},
		{"in range", "3000", wire.DeliveryDVR, 3000},
		{"above the window clamps", "600000", wire.DeliveryDVR, 5000},
		{"zero", "0", wire.DeliveryReliable, 0},
		{"negative", "-1", wire.DeliveryReliable, 0},
		{"not a number", "abc", wire.DeliveryReliable, 0},
		{"overflows", "99999999999999999999", wire.DeliveryReliable, 0},
		{"float", "1500.5", wire.DeliveryReliable, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mode, ms := NegotiateDelivery(true, tc.raw, 5*time.Second)
			if mode != tc.wantMode || ms != tc.wantMs {
				t.Errorf("NegotiateDelivery(%q) = %d/%d, want %d/%d", tc.raw, mode, ms, tc.wantMode, tc.wantMs)
			}
		})
	}

	// Without ?delivery=reliable, `buffer` is meaningless and must not
	// silently upgrade a datagram viewer into a mode it never asked for.
	if mode, ms := NegotiateDelivery(false, "3000", 5*time.Second); mode != wire.DeliveryDatagrams || ms != 0 {
		t.Errorf("datagram viewer with a buffer param = %d/%d, want datagrams/0", mode, ms)
	}
}
