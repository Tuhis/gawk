package hub

// R21 DV1 (docs/26): the per-broadcast DVR ring and the cursors into it.
// Everything here is pure — no sessions, no I/O — because the ring's whole job
// is to be a correct data structure under concurrent append and N readers.

import (
	"bytes"
	"errors"
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

// DV4 (docs/26 Decision 9): a DVR subscriber legitimately sits behind live —
// that is the feature. Health must therefore ask "is the cursor advancing?",
// never "is it at live?", or the eviction machinery starts killing exactly the
// viewers the mode exists for.
func TestDVRLaggingButAdvancingSubscriberIsNotEvicted(t *testing.T) {
	r := NewRegistry(discardLog, Options{
		DVR: DVROptions{Window: 30 * time.Second, MaxBytes: 1 << 20},
	})
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	f := &fakeSender{}
	sub, err := r.SubscribeDVR(id, f, 30000)
	if err != nil {
		t.Fatalf("SubscribeDVR: %v", err)
	}

	// Well past every eviction threshold in GOP count, and always making
	// progress. A subscriber like this is healthy no matter how far back it is.
	for g := range KeyframeSlowEvictThreshold + CarrierOpenFailEvictThreshold + 5 {
		ingestKeyframe(t, p, keyframeMsg(t, uint32(g*100), "vp8", "KEY"))
		p.HandleDatagram(chunkDgram(t, false, uint32(g*100+1), 0, 1, fmt.Sprintf("g%02d", g)))
	}
	waitFor(t, 10*time.Second, func() bool { return len(f.carrierRecords(t)) > 0 }, "records to flow")

	if _, closed := f.getCloseInfo(); closed {
		t.Fatal("a DVR subscriber that was advancing got evicted — lag is not sickness in this mode")
	}
	_ = sub
}

// The other half: a subscriber whose cursor has NOT moved is unreachable
// however healthy its lag looks, and must be evicted so the relay stops
// burning fan-out on a ghost.
func TestDVRStuckSubscriberIsEvicted(t *testing.T) {
	r := NewRegistry(discardLog, Options{
		DVR: DVROptions{Window: 30 * time.Second, MaxBytes: 1 << 20},
		// Short enough to keep the test quick; the production default is
		// measured in tens of seconds.
		DVRProgressTimeout: 300 * time.Millisecond,
	})
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	f := &fakeSender{}
	f.setCarBlock(make(chan struct{})) // writes park forever: no progress, ever
	if _, err := r.SubscribeDVR(id, f, 30000); err != nil {
		t.Fatalf("SubscribeDVR: %v", err)
	}

	for g := range 6 {
		ingestKeyframe(t, p, keyframeMsg(t, uint32(g*100), "vp8", "KEY"))
		p.HandleDatagram(chunkDgram(t, false, uint32(g*100+1), 0, 1, "d"))
		time.Sleep(50 * time.Millisecond)
	}

	waitFor(t, 10*time.Second, func() bool {
		code, closed := f.getCloseInfo()
		return closed && code == uint32(wire.CloseCodeSubscriberUnresponsive)
	}, "a stuck DVR subscriber to be evicted with 4001")
}

// The DEFAULT progress timeout, on a registry configured with nothing at all —
// the one thing every other test in this file opts out of by setting its own.
// Without this, DefaultDVRProgressTimeout could go back to the 30 s it was
// until 2026-07-26 (BUGS.md, docs/26 finding 7) and the whole suite would stay
// green, because a viewer's recovery time is not expressed anywhere else.
//
// EXPENSIVE, deliberately: it waits out a real DefaultDVRProgressTimeout, so it
// costs ~7 s of wall clock on its own — by far the slowest test in the package.
// That is the price of asserting a duration rather than a symbol. The bounds are
// hard-coded on purpose: deriving them from DefaultDVRProgressTimeout would make
// the test pass at *any* value of it, which is precisely what it exists to stop.
func TestDVRDefaultProgressTimeoutBracketsSixSeconds(t *testing.T) {
	r := NewRegistry(discardLog, Options{})
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	f := &fakeSender{}
	f.setCarBlock(make(chan struct{})) // every record write parks: no progress, ever
	if _, err := r.SubscribeDVR(id, f, 30000); err != nil {
		t.Fatalf("SubscribeDVR: %v", err)
	}

	// A short burst, then the publisher goes quiet. Keyframe writes succeed in
	// the fake, so it is the burst *ending* that stops progress being noted and
	// starts the clock — the same shape as TestDVRStuckSubscriberIsEvicted.
	for g := range 6 {
		ingestKeyframe(t, p, keyframeMsg(t, uint32(g*100), "vp8", "KEY"))
		p.HandleDatagram(chunkDgram(t, false, uint32(g*100+1), 0, 1, "d"))
		time.Sleep(20 * time.Millisecond)
	}

	// Lower bound: a viewer riding out a few seconds of trouble keeps its
	// session. Costs no extra wall clock — it is inside the wait below.
	time.Sleep(3 * time.Second)
	if _, closed := f.getCloseInfo(); closed {
		t.Fatal("evicted within 3 s — the default is far more aggressive than 6 s")
	}

	// Upper bound: comfortably past 6 s and comfortably short of the old 30 s.
	waitFor(t, 12*time.Second, func() bool {
		code, closed := f.getCloseInfo()
		return closed && code == uint32(wire.CloseCodeSubscriberUnresponsive)
	}, "the default progress timeout to evict an unreachable DVR subscriber")
}

// A broadcaster who steps away longer than DVRProgressTimeout must not cost
// every Deep-buffer viewer its session the instant they come back.
//
// The stall check is "nothing was written for DVRProgressTimeout", and a
// caught-up drain writes nothing because there is nothing to write: it parks on
// the ring's wake channel with its progress stamp frozen at the last record.
// The check is only ever re-evaluated when an append wakes it — so the first
// frame of the *returning* broadcaster is what trips it, and the viewer is
// evicted at the exact moment its broadcast resumes. "No progress because there
// is nothing to send" is not the unreachability this timer exists to catch.
func TestDVRSubscriberSurvivesAnAwayBroadcaster(t *testing.T) {
	r := NewRegistry(discardLog, Options{
		DVR:                DVROptions{Window: 30 * time.Second, MaxBytes: 1 << 20},
		DVRProgressTimeout: 300 * time.Millisecond,
	})
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	f := &fakeSender{}
	if _, err := r.SubscribeDVR(id, f, 30000); err != nil {
		t.Fatalf("SubscribeDVR: %v", err)
	}

	ingestKeyframe(t, p, keyframeMsg(t, 100, "vp8", "KEY"))
	p.HandleDatagram(chunkDgram(t, false, 101, 0, 1, "d"))
	waitFor(t, 10*time.Second, func() bool { return len(f.carrierRecords(t)) > 0 }, "records to flow")

	// Away: the ring receives nothing, so the drain has nothing to write and
	// nothing wakes it. Well past the timeout.
	time.Sleep(3 * 300 * time.Millisecond)
	if _, closed := f.getCloseInfo(); closed {
		t.Fatal("evicted while merely idle — nothing woke the drain, so this cannot be right")
	}

	// Back: the first append must be served, not answered with a 4001.
	ingestKeyframe(t, p, keyframeMsg(t, 200, "vp8", "BACK"))
	p.HandleDatagram(chunkDgram(t, false, 201, 0, 1, "d2"))
	waitFor(t, 10*time.Second, func() bool { return len(f.receivedKeyframes()) >= 2 },
		"the returning broadcaster's keyframe to reach the DVR subscriber")

	if _, closed := f.getCloseInfo(); closed {
		t.Error("a Deep-buffer viewer was evicted the moment its away broadcaster returned")
	}
}

// A DVR subscriber that cannot accept keyframe streams is the same zombie the
// live path evicts on KeyframeOpenFailEvictThreshold (exhausted stream credit,
// R10 field finding) — but drainDVR opens its keyframe streams itself and never
// touched that streak, so in this mode the signal was counted (kfDroppedOpenFailed)
// and then ignored. The progress timeout is set out of reach here so only the
// streak can end this session.
func TestDVRKeyframeOpenFailuresEvict(t *testing.T) {
	r := NewRegistry(discardLog, Options{
		DVR:                DVROptions{Window: 30 * time.Second, MaxBytes: 1 << 20},
		DVRProgressTimeout: time.Minute,
	})
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	f := &fakeSender{}
	f.setKfOpenErr(errors.New("no stream credit"))
	if _, err := r.SubscribeDVR(id, f, 30000); err != nil {
		t.Fatalf("SubscribeDVR: %v", err)
	}

	for g := range KeyframeOpenFailEvictThreshold + 2 {
		ingestKeyframe(t, p, keyframeMsg(t, uint32(g*100), "vp8", "KEY"))
		p.HandleDatagram(chunkDgram(t, false, uint32(g*100+1), 0, 1, "d"))
	}

	waitFor(t, 10*time.Second, func() bool {
		code, closed := f.getCloseInfo()
		return closed && code == uint32(wire.CloseCodeSubscriberUnresponsive)
	}, "a DVR subscriber whose keyframe opens all fail to be evicted with 4001")
}

// Mode isolation (docs/26 Decision 12): the DVR subscriber's lag must not
// influence when the other two delivery modes are evicted. They share the
// eviction code, and that shared rewrite is the one place R21 can silently
// change behaviour for viewers who never opted in.
func TestDVRDoesNotDisturbOtherModesEviction(t *testing.T) {
	r := NewRegistry(discardLog, Options{
		DVR:                DVROptions{Window: 30 * time.Second, MaxBytes: 1 << 20},
		DVRProgressTimeout: 300 * time.Millisecond,
	})
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	// A stuck DVR subscriber alongside healthy viewers on the other two modes.
	stuck := &fakeSender{}
	stuck.setCarBlock(make(chan struct{}))
	if _, err := r.SubscribeDVR(id, stuck, 30000); err != nil {
		t.Fatalf("SubscribeDVR: %v", err)
	}
	plain := &fakeSender{}
	if _, err := r.Subscribe(id, plain); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	reliable := &fakeSender{}
	if _, err := r.SubscribeReliable(id, reliable); err != nil {
		t.Fatalf("SubscribeReliable: %v", err)
	}

	for g := range 8 {
		ingestKeyframe(t, p, keyframeMsg(t, uint32(g*100), "vp8", "KEY"))
		p.HandleDatagram(chunkDgram(t, false, uint32(g*100+1), 0, 1, "d"))
		time.Sleep(50 * time.Millisecond)
	}
	waitFor(t, 10*time.Second, func() bool { _, closed := stuck.getCloseInfo(); return closed },
		"the stuck DVR subscriber to be evicted")

	if _, closed := plain.getCloseInfo(); closed {
		t.Error("the datagram subscriber was evicted — R21 changed a mode that never opted in")
	}
	if _, closed := reliable.getCloseInfo(); closed {
		t.Error("the reliable subscriber was evicted — R21 changed a mode that never opted in")
	}
}

// A DVR subscriber must receive keyframes ONLY from its own cursor. The live
// fan-out path (onKeyframe → sendKeyframe) and the join prime both send the
// *current* keyframe to every subscriber, which for a cursor sitting seconds
// behind is a second, contradictory timeline: the viewer's reorder buffer sees
// frameIds jump forward to live and back to the cursor, parks in
// waiting-for-keyframe and ages out every delta. Video freezes completely
// while data keeps arriving.
//
// Found on hardware 2026-07-23 (broadcast 3YPR53): the client's own numbers
// named it — keyframeStreamsReceived climbing at ~2x the carrierStreams rate,
// i.e. two keyframes per GOP, with decodedFps 0 and reorderKeyframeWaitDrops
// climbing at the full frame rate.
func TestDVRSubscriberGetsKeyframesOnlyFromItsCursor(t *testing.T) {
	r := NewRegistry(discardLog, Options{
		DVR: DVROptions{Window: 30 * time.Second, MaxBytes: 1 << 20},
	})
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	// A keyframe before anyone joins, so the join prime has something to send.
	ingestKeyframe(t, p, keyframeMsg(t, 1, "vp8", "PRIME"))

	f := &fakeSender{}
	if _, err := r.SubscribeDVR(id, f, 3000); err != nil {
		t.Fatalf("SubscribeDVR: %v", err)
	}

	const gops = 5
	for g := range gops {
		ingestKeyframe(t, p, keyframeMsg(t, uint32((g+1)*100), "vp8", fmt.Sprintf("KEY%d", g)))
		p.HandleDatagram(chunkDgram(t, false, uint32((g+1)*100+1), 0, 1, "d"))
	}

	// One keyframe per GOP served, plus at most the seeded one the cursor
	// starts on — never two timelines' worth.
	waitFor(t, 5*time.Second, func() bool { return len(f.receivedKeyframes()) >= gops }, "keyframes to flow")
	time.Sleep(200 * time.Millisecond) // let any duplicate land before counting
	got := len(f.receivedKeyframes())
	if got > gops+1 {
		t.Errorf("DVR subscriber received %d keyframes for %d GOPs — the live fan-out is "+
			"sending a second, contradictory timeline alongside the cursor's", got, gops)
	}
}
