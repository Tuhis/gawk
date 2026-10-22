package hub

import (
	"bytes"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/wire"
)

// R74 leg writer (docs/76 item 18, PP8/PP9): a PoP leg is an internal
// subscriber whose deltas and parity ride R73 uplink carriers under R73's
// sender rules (docs/75 D2, D3), and whose every other datagram stays a
// datagram. The timer that resets a finished carrier runs on legClock, so the
// 150 ms rule is checked without sleeping.

// legClock is a fake clock for LegOptions: Now reads it, AfterFunc schedules
// on it, and Advance fires what falls due, synchronously.
type legClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*legTimer
}

type legTimer struct {
	at      time.Time
	fn      func()
	stopped bool
	fired   bool
}

func newLegClock() *legClock { return &legClock{now: time.Unix(1_000_000, 0)} }

func (c *legClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *legClock) AfterFunc(d time.Duration, fn func()) func() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &legTimer{at: c.now.Add(d), fn: fn}
	c.timers = append(c.timers, t)
	return func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		if t.stopped || t.fired {
			return false
		}
		t.stopped = true
		return true
	}
}

func (c *legClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	var due []func()
	for _, t := range c.timers {
		if !t.stopped && !t.fired && !t.at.After(c.now) {
			t.fired = true
			due = append(due, t.fn)
		}
	}
	c.mu.Unlock()
	for _, fn := range due {
		fn()
	}
}

// pending counts timers neither fired nor stopped.
func (c *legClock) pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, t := range c.timers {
		if !t.stopped && !t.fired {
			n++
		}
	}
	return n
}

func (c *legClock) opts() LegOptions {
	return LegOptions{Carrier: true, Now: c.Now, AfterFunc: c.AfterFunc}
}

// legSender is a fakeSender that records the most carriers ever live at once:
// opened and not reset. A finished (FIN'd) carrier is still live until its
// reset, which is the point of the 150 ms rule.
type legSender struct {
	*fakeSender
	mu      sync.Mutex
	maxLive int
}

func newLegSender() *legSender { return &legSender{fakeSender: &fakeSender{}} }

func (l *legSender) liveCarriers() int {
	n := 0
	for _, c := range l.carrierStreams() {
		if _, cancelled := c.state(); !cancelled {
			n++
		}
	}
	return n
}

func (l *legSender) OpenCarrierStream() (KeyframeStream, error) {
	st, err := l.fakeSender.OpenCarrierStream()
	if err != nil {
		return nil, err
	}
	live := l.liveCarriers()
	l.mu.Lock()
	if live > l.maxLive {
		l.maxLive = live
	}
	l.mu.Unlock()
	return st, nil
}

func (l *legSender) maxLiveCarriers() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.maxLive
}

func newLegBroadcast(t *testing.T, opts Options) (*Registry, string, *Publisher) {
	t.Helper()
	r := NewRegistry(discardLog, opts)
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	return r, id, p
}

func subscribeLeg(t *testing.T, r *Registry, id string, conn Conn, o LegOptions) *Subscriber {
	t.Helper()
	sub, err := r.SubscribeLeg(id, conn, o)
	if err != nil {
		t.Fatalf("SubscribeLeg: %v", err)
	}
	return sub
}

func carrierState(c *fakeCarrierStream) (closed, cancelled bool) { return c.state() }

func waitCarriers(t *testing.T, f *fakeSender, n int) []*fakeCarrierStream {
	t.Helper()
	waitFor(t, 5*time.Second, func() bool { return len(f.carrierStreams()) >= n }, "carrier opens")
	return f.carrierStreams()
}

func waitStreamRecords(t *testing.T, c *fakeCarrierStream, n int) {
	t.Helper()
	waitFor(t, 5*time.Second, func() bool { return len(c.records(t)) >= n }, "records on one carrier")
}

func wantRecords(t *testing.T, c *fakeCarrierStream, want ...[]byte) {
	t.Helper()
	got := c.records(t)
	if len(got) != len(want) {
		t.Fatalf("carrier holds %d records, want %d", len(got), len(want))
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Errorf("record %d = %x, want %x", i, got[i], want[i])
		}
	}
}

// A datagram leg (Carrier false, as with home's -uplink-carriers=false or
// -pop-leg-delivery=datagram) is SubscribeInternal: the same datagrams and the
// same keyframe streams, byte for byte, in the same order.
func TestLegDatagramIsByteIdenticalToSubscribeInternal(t *testing.T) {
	r, id, p := newLegBroadcast(t, Options{})

	internal := &fakeSender{}
	leg := &fakeSender{}
	si, err := r.SubscribeInternal(id, internal)
	if err != nil {
		t.Fatalf("SubscribeInternal: %v", err)
	}
	sl := subscribeLeg(t, r, id, leg, LegOptions{})
	// Keyframe fan-out runs off the ingest path and a newer keyframe may
	// supersede an unwritten older one, so each is awaited on both before
	// the next, keeping the comparison deterministic.
	kfs := func(n int) {
		t.Helper()
		waitFor(t, 5*time.Second, func() bool {
			return len(leg.receivedKeyframes()) >= n && len(internal.receivedKeyframes()) >= n
		}, "keyframes on both subscribers")
	}
	ingestKeyframe(t, p, keyframeMsg(t, 0, "vp8", "KEY0"))
	kfs(1)

	feed := [][]byte{
		chunkDgram(t, false, 1, 0, 1, "d1"),
		parityDgram(t, 1, 0),
		configDgram(t, "vp8"),
		audioConfigDgram(t, 48000),
		audioDgram(t, 1, "a1"),
		chunkDgram(t, false, 2, 0, 1, "d2"),
	}
	for _, d := range feed {
		p.HandleDatagram(d)
	}
	ingestKeyframe(t, p, keyframeMsg(t, 3, "vp8", "KEY3"))
	kfs(2)
	p.HandleDatagram(chunkDgram(t, false, 4, 0, 1, "d4"))
	r.PumpViewerCounts(time.Now())

	// The feed, d4 and the pump's ViewerCount.
	wantDgrams := len(feed) + 2
	waitFor(t, 5*time.Second, func() bool {
		return len(leg.received()) >= wantDgrams && len(internal.received()) >= wantDgrams
	}, "both subscribers to drain")
	si.Close()
	sl.Close()

	a, b := internal.received(), leg.received()
	if len(a) != len(b) {
		t.Fatalf("leg got %d datagrams, internal %d", len(b), len(a))
	}
	for i := range a {
		if !bytes.Equal(a[i], b[i]) {
			t.Errorf("datagram %d: leg %x, internal %x", i, b[i], a[i])
		}
	}
	ka, kb := internal.receivedKeyframes(), leg.receivedKeyframes()
	if len(ka) != len(kb) {
		t.Fatalf("leg got %d keyframes, internal %d", len(kb), len(ka))
	}
	for i := range ka {
		if !bytes.Equal(ka[i], kb[i]) {
			t.Errorf("keyframe %d differs", i)
		}
	}
	if n := leg.carrierOpens(); n != 0 {
		t.Errorf("a datagram leg opened %d carriers, want 0", n)
	}
}

// A leg is an internal subscriber: exempt from the viewer caps and out of the
// local viewer count, in either delivery.
func TestLegIsInternal(t *testing.T) {
	for _, carrier := range []bool{false, true} {
		r, id, _ := newLegBroadcast(t, Options{MaxSubscribers: 1})
		v, err := r.Subscribe(id, &fakeSender{})
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		sl := subscribeLeg(t, r, id, &fakeSender{}, LegOptions{Carrier: carrier})
		if got := r.ExternalSubscribers(id); got != 1 {
			t.Errorf("carrier=%v: ExternalSubscribers = %d, want 1 (the leg is not a viewer)", carrier, got)
		}
		if got := r.ViewerSubscribers(id); got != 1 {
			t.Errorf("carrier=%v: ViewerSubscribers = %d, want 1", carrier, got)
		}
		sl.Close()
		v.Close()
	}
}

// Only VideoChunk and ParityChunk go on the carrier: R73's reader ends a
// carrier on anything else (hub/uplink.go), so a ViewerCount from the count
// pump or a live DecoderConfig mid-GOP goes as a datagram and the carrier
// carries on. Audio, too, is a datagram: a leg has no audio lane.
func TestLegCarrierCarriesOnlyVideoAndParity(t *testing.T) {
	r, id, p := newLegBroadcast(t, Options{})
	clk := newLegClock()
	f := newLegSender()
	sub := subscribeLeg(t, r, id, f, clk.opts())
	defer sub.Close()

	ingestKeyframe(t, p, keyframeMsg(t, 0, "vp8", "KEY0"))
	d1 := chunkDgram(t, false, 1, 0, 1, "d1")
	par1 := parityDgram(t, 1, 0)
	p.HandleDatagram(d1)
	p.HandleDatagram(par1)
	cars := waitCarriers(t, f.fakeSender, 1)
	waitStreamRecords(t, cars[0], 2)

	// Mid-GOP: a count, a config, audio.
	r.PumpViewerCounts(time.Now())
	cfg := configDgram(t, "vp8")
	p.HandleDatagram(cfg)
	ac := audioConfigDgram(t, 48000)
	a1 := audioDgram(t, 1, "a1")
	p.HandleDatagram(ac)
	p.HandleDatagram(a1)
	d2 := chunkDgram(t, false, 2, 0, 1, "d2")
	p.HandleDatagram(d2)
	waitStreamRecords(t, cars[0], 3)
	waitFor(t, 5*time.Second, hasDgram(f.fakeSender, a1), "audio as a datagram")

	if got := len(f.carrierStreams()); got != 1 {
		t.Fatalf("carriers = %d, want 1: a sideband mid-GOP must not rotate or end the carrier", got)
	}
	if closed, cancelled := carrierState(cars[0]); closed || cancelled {
		t.Fatalf("carrier closed=%v cancelled=%v mid-GOP, want open", closed, cancelled)
	}
	wantRecords(t, cars[0], d1, par1, d2)
	for _, want := range [][]byte{cfg, ac, a1} {
		if !hasDgram(f.fakeSender, want)() {
			t.Errorf("datagram %x not sent as a datagram", want)
		}
	}
	if n := countType(f.fakeSender, wire.TypeViewerCount); n == 0 {
		t.Error("ViewerCount not sent as a datagram")
	}
	for _, d := range f.received() {
		if isCarrierMediaDatagram(d) {
			t.Errorf("media datagram %x went as a datagram on a carrier leg", d)
		}
	}

	// And R73's reader takes the carrier as written: every record admitted.
	home, hid, hp := newLegBroadcast(t, Options{UplinkCarriers: true})
	_ = hid
	ingestKeyframe(t, hp, keyframeMsg(t, 0, "vp8", "KEY0"))
	if err := hp.IngestUplinkCarrier(bytes.NewReader(cars[0].bytes())); err != nil {
		t.Fatalf("IngestUplinkCarrier on the leg's carrier: %v", err)
	}
	if st := uplinkStats(t, home, hid); st.UplinkRecords != 3 {
		t.Errorf("home admitted %d records, want 3", st.UplinkRecords)
	}
}

// At rotation the superseded carrier is finished (FIN) and reset ResetDelay
// later: not sooner, so its tail can land; not never, so it cannot hold one
// of home's three slots.
func TestLegSupersededCarrierResetAfterResetDelay(t *testing.T) {
	r, id, p := newLegBroadcast(t, Options{})
	clk := newLegClock()
	f := newLegSender()
	sub := subscribeLeg(t, r, id, f, clk.opts())
	defer sub.Close()

	ingestKeyframe(t, p, keyframeMsg(t, 0, "vp8", "KEY0"))
	p.HandleDatagram(chunkDgram(t, false, 1, 0, 1, "d1"))
	cars := waitCarriers(t, f.fakeSender, 1)
	waitStreamRecords(t, cars[0], 1)

	ingestKeyframe(t, p, keyframeMsg(t, 2, "vp8", "KEY2"))
	p.HandleDatagram(chunkDgram(t, false, 3, 0, 1, "d3"))
	cars = waitCarriers(t, f.fakeSender, 2)
	waitStreamRecords(t, cars[1], 1)

	if closed, cancelled := carrierState(cars[0]); !closed || cancelled {
		t.Fatalf("superseded carrier closed=%v cancelled=%v, want finished and not yet reset", closed, cancelled)
	}
	clk.Advance(LegResetDelay - time.Millisecond)
	if _, cancelled := carrierState(cars[0]); cancelled {
		t.Fatalf("superseded carrier reset before %s", LegResetDelay)
	}
	clk.Advance(time.Millisecond)
	if _, cancelled := carrierState(cars[0]); !cancelled {
		t.Fatalf("superseded carrier not reset at %s", LegResetDelay)
	}
	if closed, cancelled := carrierState(cars[1]); closed || cancelled {
		t.Errorf("current carrier closed=%v cancelled=%v, want open", closed, cancelled)
	}
	if LegResetDelay != 150*time.Millisecond {
		t.Errorf("LegResetDelay = %s, want 150ms (docs/76 item 18)", LegResetDelay)
	}
}

// GOPs faster than ResetDelay: a third carrier never opens beside two live
// ones; the oldest is reset first.
func TestLegNeverMoreThanTwoLiveCarriers(t *testing.T) {
	r, id, p := newLegBroadcast(t, Options{})
	clk := newLegClock()
	f := newLegSender()
	sub := subscribeLeg(t, r, id, f, clk.opts())
	defer sub.Close()

	for gop := range 5 {
		k := uint32(gop * 2)
		ingestKeyframe(t, p, keyframeMsg(t, k, "vp8", "KEY"))
		p.HandleDatagram(chunkDgram(t, false, k+1, 0, 1, "d"))
		cars := waitCarriers(t, f.fakeSender, gop+1)
		waitStreamRecords(t, cars[gop], 1)
	}
	if got := f.maxLiveCarriers(); got > UplinkSenderCarriers {
		t.Errorf("max live carriers = %d, want <= %d", got, UplinkSenderCarriers)
	}
	if got := f.liveCarriers(); got != 2 {
		t.Errorf("live carriers now = %d, want 2 (current + finishing)", got)
	}
	cars := f.carrierStreams()
	for i := range 3 {
		if _, cancelled := carrierState(cars[i]); !cancelled {
			t.Errorf("carrier %d not reset; the oldest must go first", i)
		}
	}
}

// No reset within a GOP: a write that stalls longer than the viewer drain's
// 500 ms CarrierWriteTimeout (and 400 ms on the leg's clock) waits, and the
// record lands on the same carrier when the line recovers.
func TestLegWriteStallWithinGOPDoesNotReset(t *testing.T) {
	r, id, p := newLegBroadcast(t, Options{})
	clk := newLegClock()
	f := newLegSender()
	sub := subscribeLeg(t, r, id, f, clk.opts())
	defer sub.Close()

	ingestKeyframe(t, p, keyframeMsg(t, 0, "vp8", "KEY0"))
	d1 := chunkDgram(t, false, 1, 0, 1, "d1")
	p.HandleDatagram(d1)
	cars := waitCarriers(t, f.fakeSender, 1)
	waitStreamRecords(t, cars[0], 1)

	block := make(chan struct{})
	f.setCarBlock(block)
	d2 := chunkDgram(t, false, 2, 0, 1, "d2")
	d3 := chunkDgram(t, false, 3, 0, 1, "d3")
	p.HandleDatagram(d2)
	p.HandleDatagram(d3)
	time.Sleep(CarrierWriteTimeout + 100*time.Millisecond)
	clk.Advance(400 * time.Millisecond)
	if _, cancelled := carrierState(cars[0]); cancelled {
		t.Fatal("a write stall within a GOP reset the carrier")
	}
	f.setCarBlock(nil)
	close(block)
	waitStreamRecords(t, cars[0], 3)
	wantRecords(t, cars[0], d1, d2, d3)
	if n := len(f.carrierStreams()); n != 1 {
		t.Errorf("carriers = %d, want 1", n)
	}
}

// F10: a write still blocked on an older GOP's carrier is abandoned the
// moment the next keyframe fans out, and that carrier is reset at once — not
// ResetDelay later. Nothing queued for the older GOP is written afterwards.
func TestLegBlockedWriteAbandonedAtRotation(t *testing.T) {
	r, id, p := newLegBroadcast(t, Options{})
	clk := newLegClock()
	f := newLegSender()
	sub := subscribeLeg(t, r, id, f, clk.opts())
	defer sub.Close()

	ingestKeyframe(t, p, keyframeMsg(t, 0, "vp8", "KEY0"))
	d1 := chunkDgram(t, false, 1, 0, 1, "d1")
	p.HandleDatagram(d1)
	cars := waitCarriers(t, f.fakeSender, 1)
	waitStreamRecords(t, cars[0], 1)

	block := make(chan struct{})
	defer close(block)
	f.setCarBlock(block)
	p.HandleDatagram(chunkDgram(t, false, 2, 0, 1, "d2")) // parks in Write
	p.HandleDatagram(chunkDgram(t, false, 3, 0, 1, "d3")) // queued behind it
	// Let the drain reach the parked write.
	time.Sleep(50 * time.Millisecond)
	f.setCarBlock(nil)

	ingestKeyframe(t, p, keyframeMsg(t, 4, "vp8", "KEY4"))
	waitFor(t, 2*time.Second, func() bool {
		_, cancelled := carrierState(cars[0])
		return cancelled
	}, "the blocked carrier to be reset at the rotation, without the clock moving")

	d5 := chunkDgram(t, false, 5, 0, 1, "d5")
	p.HandleDatagram(d5)
	cars = waitCarriers(t, f.fakeSender, 2)
	waitStreamRecords(t, cars[1], 1)
	wantRecords(t, cars[1], d5)
	wantRecords(t, cars[0], d1)
	if n := len(f.carrierStreams()); n != 2 {
		t.Errorf("carriers = %d, want 2", n)
	}
	if got := f.maxLiveCarriers(); got > UplinkSenderCarriers {
		t.Errorf("max live carriers = %d", got)
	}
}

// A GOP superseded before its carrier opened never gets one: its late deltas
// are dropped, not sent on a carrier home would read as stale.
func TestLegNeverOpensCarrierForSupersededGOP(t *testing.T) {
	r, id, p := newLegBroadcast(t, Options{})
	clk := newLegClock()
	f := newLegSender()
	sub := subscribeLeg(t, r, id, f, clk.opts())
	defer sub.Close()

	ingestKeyframe(t, p, keyframeMsg(t, 0, "vp8", "KEY0"))
	ingestKeyframe(t, p, keyframeMsg(t, 5, "vp8", "KEY5"))
	p.HandleDatagram(chunkDgram(t, false, 1, 0, 1, "late"))
	p.HandleDatagram(parityDgram(t, 2, 0))
	d6 := chunkDgram(t, false, 6, 0, 1, "d6")
	p.HandleDatagram(d6)
	cars := waitCarriers(t, f.fakeSender, 1)
	waitStreamRecords(t, cars[0], 1)
	wantRecords(t, cars[0], d6)
	if n := f.carrierOpens(); n != 1 {
		t.Errorf("carrier opens = %d, want 1 (none for GOP 0)", n)
	}
}

// Close resets both carriers and stops the pending reset timer.
func TestLegCloseCancelsBothCarriersAndStopsTimer(t *testing.T) {
	r, id, p := newLegBroadcast(t, Options{})
	clk := newLegClock()
	f := newLegSender()
	sub := subscribeLeg(t, r, id, f, clk.opts())

	ingestKeyframe(t, p, keyframeMsg(t, 0, "vp8", "KEY0"))
	p.HandleDatagram(chunkDgram(t, false, 1, 0, 1, "d1"))
	waitCarriers(t, f.fakeSender, 1)
	ingestKeyframe(t, p, keyframeMsg(t, 2, "vp8", "KEY2"))
	p.HandleDatagram(chunkDgram(t, false, 3, 0, 1, "d3"))
	cars := waitCarriers(t, f.fakeSender, 2)
	waitStreamRecords(t, cars[1], 1)
	if clk.pending() != 1 {
		t.Fatalf("pending reset timers = %d, want 1", clk.pending())
	}

	sub.Close()
	for i, c := range cars {
		if _, cancelled := carrierState(c); !cancelled {
			t.Errorf("carrier %d not reset by Close", i)
		}
	}
	if n := clk.pending(); n != 0 {
		t.Errorf("pending reset timers after Close = %d, want 0", n)
	}
}

// A leg crosses a WAN: a run of slow keyframe writes is counted, not an
// eviction. A viewer with the same run is evicted
// (TestStalledSubscriberEvictedAfterConsecutiveSlowKeyframes).
func TestLegSlowKeyframesDoNotEvict(t *testing.T) {
	for _, carrier := range []bool{false, true} {
		r, id, p := newLegBroadcast(t, Options{})
		f := &fakeSender{kfWriteErr: errors.New("write deadline exceeded")}
		clk := newLegClock()
		o := clk.opts()
		o.Carrier = carrier
		sub := subscribeLeg(t, r, id, f, o)
		n := KeyframeSlowEvictThreshold + 2
		for i := range n {
			ingestKeyframe(t, p, keyframeMsg(t, uint32(i*2), "vp8", "KEY"))
			waitFor(t, 5*time.Second, func() bool {
				return singleBroadcastStats(t, r).KeyframeDrops.Slow == uint64(i+1)
			}, "slow keyframe counted")
		}
		time.Sleep(50 * time.Millisecond)
		if _, closed := f.getCloseInfo(); closed {
			t.Errorf("carrier=%v: a leg was evicted for slow keyframes", carrier)
		}
		if sub.closed.Load() {
			t.Errorf("carrier=%v: leg subscriber closed", carrier)
		}
		sub.Close()
	}
}
