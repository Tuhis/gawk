// R30 ST6 (docs/35 §8): the burst-threshold mechanism, end to end, in CI.
//
// The measured failure (docs/34 finding 4) is a per-connection receive buffer
// ~8 packets deep that evicts oldest-first when a frame's datagram burst
// overruns it — head-of-burst loss that rises with frame size. That buffer
// sits below anything a test can reach on a real browser path, so this test
// MODELS it: a userspace UDP forwarder whose relay→client direction runs a
// bounded FIFO per client connection, drained at a fixed rate, evicting the
// oldest packet on overflow. Legs are separate UDP 5-tuples, so each gets its
// own queue with no extra work — exactly the per-connection property finding 5
// measured.
//
// The shape then follows docs/24 finding 10's control-lane lesson: a striped
// viewer (primary + 3 legs) and a plain control ride the SAME forwarder while
// the publisher (wired direct — ingress stays clean) sends 18-chunk frames.
// The control must lose material chunk volume to head eviction (the forwarder
// provably bites) while the striped viewer — every leg's share under the
// queue depth — completes its frames. What stays manual is the real Firefox
// buffer on the affected machine (ST1/ST7, docs/35 §2).
package transport

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/wire"
)

// burstLink is the burst-threshold buffer model. Unarmed it forwards at line
// rate (handshakes must not spend the budget); armed, each client connection's
// downlink becomes a depth-bounded FIFO drained at drainEvery, evicting the
// OLDEST queued packet when a newcomer finds it full — the head-drop shape.
type burstLink struct {
	front    *net.UDPConn
	upstream *net.UDPAddr

	armed      atomic.Bool
	depth      int
	drainEvery time.Duration
	evicted    atomic.Uint64
	// closed is read by every drain goroutine off the lock, hence atomic.
	closed atomic.Bool

	mu    sync.Mutex
	peers map[string]*net.UDPConn
}

func startBurstLink(t *testing.T, relayPort, depth int, drainEvery time.Duration) *burstLink {
	t.Helper()
	front, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("burst link listen: %v", err)
	}
	_ = front.SetReadBuffer(1 << 20)
	_ = front.SetWriteBuffer(1 << 20)
	l := &burstLink{
		front:      front,
		upstream:   &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: relayPort},
		depth:      depth,
		drainEvery: drainEvery,
		peers:      make(map[string]*net.UDPConn),
	}
	t.Cleanup(l.close)
	go l.pumpUplink()
	return l
}

func (l *burstLink) port() int { return l.front.LocalAddr().(*net.UDPAddr).Port }
func (l *burstLink) arm()      { l.armed.Store(true) }

func (l *burstLink) pumpUplink() {
	buf := make([]byte, 2048)
	for {
		n, addr, err := l.front.ReadFromUDP(buf)
		if err != nil {
			return
		}
		up := l.peer(addr)
		if up == nil {
			return
		}
		if _, err := up.Write(buf[:n]); err != nil {
			return
		}
	}
}

func (l *burstLink) peer(addr *net.UDPAddr) *net.UDPConn {
	key := addr.String()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed.Load() {
		return nil
	}
	if c, ok := l.peers[key]; ok {
		return c
	}
	c, err := net.DialUDP("udp", nil, l.upstream)
	if err != nil {
		return nil
	}
	_ = c.SetReadBuffer(1 << 20)
	_ = c.SetWriteBuffer(1 << 20)
	l.peers[key] = c
	go l.pumpDownlink(c, addr)
	return c
}

// pumpDownlink models the per-connection buffer: a bounded FIFO between the
// relay's replies and the client, drained on a fixed cadence. One goroutine
// reads into the queue, one drains it — eviction happens at the queue, which
// is the whole model.
func (l *burstLink) pumpDownlink(up *net.UDPConn, client *net.UDPAddr) {
	type packet []byte
	var qmu sync.Mutex
	queue := make([]packet, 0, l.depth+1)

	go func() {
		tick := time.NewTicker(l.drainEvery)
		defer tick.Stop()
		for range tick.C {
			qmu.Lock()
			if l.closed.Load() && len(queue) == 0 {
				qmu.Unlock()
				return
			}
			var out packet
			if len(queue) > 0 {
				out = queue[0]
				queue = queue[1:]
			}
			qmu.Unlock()
			if out != nil {
				if _, err := l.front.WriteToUDP(out, client); err != nil {
					return
				}
			}
		}
	}()

	buf := make([]byte, 2048)
	for {
		n, err := up.Read(buf)
		if err != nil {
			return
		}
		if !l.armed.Load() {
			// Line rate before arming: the handshakes and the join prime must
			// not spend the eviction budget.
			if _, err := l.front.WriteToUDP(buf[:n], client); err != nil {
				return
			}
			continue
		}
		pkt := make(packet, n)
		copy(pkt, buf[:n])
		qmu.Lock()
		queue = append(queue, pkt)
		if len(queue) > l.depth {
			// Evict OLDEST — the head of the burst dies, its tail survives
			// (docs/34 finding 4's signature, parity-spared included).
			queue = queue[1:]
			l.evicted.Add(1)
		}
		qmu.Unlock()
	}
}

func (l *burstLink) close() {
	if !l.closed.CompareAndSwap(false, true) {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, up := range l.peers {
		up.Close()
	}
	l.front.Close()
}

// arrivalSet tracks per-frame chunk arrivals for one logical viewer (possibly
// merged across stripe legs).
type arrivalSet struct {
	mu     sync.Mutex
	frames map[uint32]map[uint16]bool // frameID -> chunk indices arrived
	counts map[uint32]uint16          // frameID -> frame-global chunkCount
	mismap atomic.Uint64
}

func newArrivalSet() *arrivalSet {
	return &arrivalSet{frames: make(map[uint32]map[uint16]bool), counts: make(map[uint32]uint16)}
}

// readInto drains a session's datagrams into the set. legMember/legN < 0 means
// "not a leg" (no mapping assertion).
func (a *arrivalSet) readInto(ctx context.Context, sess *webtransport.Session, legMember, legN int) {
	for {
		dgram, err := sess.ReceiveDatagram(ctx)
		if err != nil {
			return
		}
		h, _, err := wire.ParseVideoChunk(dgram)
		if err != nil {
			continue // config, count, ack — not chunk accounting
		}
		if legN > 0 && int(wire.StripeOrdinal(h.ChunkIndex, h.ChunkCount, -1))%legN != legMember {
			a.mismap.Add(1)
			continue
		}
		a.mu.Lock()
		if a.frames[h.FrameID] == nil {
			a.frames[h.FrameID] = make(map[uint16]bool)
			a.counts[h.FrameID] = h.ChunkCount
		}
		a.frames[h.FrameID][h.ChunkIndex] = true
		a.mu.Unlock()
	}
}

// tally reports (expected, got, complete, total) over the frame range sent.
func (a *arrivalSet) tally(firstID, lastID uint32) (exp, got, complete, total int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for id := firstID; id <= lastID; id++ {
		count := int(a.counts[id])
		if count == 0 {
			// Never saw any chunk of it: charge the full frame at the known
			// send size so total silence cannot read as health.
			count = burstTestChunks
		}
		total++
		exp += count
		g := len(a.frames[id])
		got += g
		if g == count {
			complete++
		}
	}
	return exp, got, complete, total
}

const (
	burstTestChunks = 18 // every delta in this test — well past the depth
	burstTestDepth  = 8  // the modeled buffer (docs/34 finding 4's ~8)
	burstTestFrames = 60
)

// TestStripedDeliveryBeatsBurstThresholdLoss is the CI proof of the R30
// mechanism: through a per-connection head-drop buffer, a striped viewer
// completes the large frames a plain viewer loses. The control lane is what
// proves the forwarder bites (docs/24 finding 10 — a protection test whose
// hazard is unverified ships green forever).
func TestStripedDeliveryBeatsBurstThresholdLoss(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, r, _ := startTestServerCfg(t, ctx, stripedTestConfig(15))
	pub, id := dialPublisherAndGetID(t, ctx, port, clientTLS)
	defer pub.CloseWithError(0, "")

	link := startBurstLink(t, port, burstTestDepth, 3*time.Millisecond)
	base := fmt.Sprintf("https://127.0.0.1:%d/subscribe/%s", link.port(), id)

	control := dial(t, ctx, base, clientTLS)
	defer control.CloseWithError(0, "")
	primary := dial(t, ctx, base, clientTLS)
	defer primary.CloseWithError(0, "")
	const stripeN = 3
	legs := make([]*webtransport.Session, stripeN)
	for j := range legs {
		legs[j] = dial(t, ctx, fmt.Sprintf("%s?stripe=%d&leg=%d", base, stripeN, j), clientTLS)
		defer legs[j].CloseWithError(0, "")
	}
	waitFor(t, 10*time.Second, func() bool { return r.Stats().Totals.Subscribers == 5 }, "all sessions registered")

	striped, err := wire.AppendStripeState(nil, wire.StripeState{Striped: true, StripeN: stripeN})
	if err != nil {
		t.Fatalf("AppendStripeState: %v", err)
	}
	waitFor(t, 10*time.Second, func() bool {
		_ = primary.SendDatagram(striped)
		for _, b := range r.Stats().Broadcasts {
			if b.StripedPrimaries == 1 {
				return true
			}
		}
		return false
	}, "primary suppression armed")

	stripedSet := newArrivalSet()
	controlSet := newArrivalSet()
	readCtx, readCancel := context.WithCancel(ctx)
	defer readCancel()
	go stripedSet.readInto(readCtx, primary, -1, -1) // should see ~nothing while suppressed
	for j, leg := range legs {
		go stripedSet.readInto(readCtx, leg, j, stripeN)
	}
	go controlSet.readInto(readCtx, control, -1, -1)

	link.arm()
	// 18-chunk frames sent back-to-back (the burst), one frame per 100 ms so
	// the modeled queue fully drains between frames — eviction then measures
	// burst length, not average rate, which is the threshold's whole point.
	const firstID = 100
	payload := make([]byte, 1100)
	for f := 0; f < burstTestFrames; f++ {
		frameID := uint32(firstID + f)
		for i := 0; i < burstTestChunks; i++ {
			d, err := wire.AppendVideoChunk(nil, wire.VideoChunkHeader{
				FrameID: frameID, ChunkIndex: uint16(i), ChunkCount: burstTestChunks,
				TimestampUs: uint64(frameID) * 33_000,
			}, payload)
			if err != nil {
				t.Fatalf("AppendVideoChunk: %v", err)
			}
			if err := pub.SendDatagram(d); err != nil {
				t.Fatalf("SendDatagram: %v", err)
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Let the drains flush the queued tails before tallying.
	time.Sleep(2 * time.Second)
	readCancel()

	sExp, sGot, sComplete, sTotal := stripedSet.tally(firstID, firstID+burstTestFrames-1)
	cExp, cGot, cComplete, cTotal := controlSet.tally(firstID, firstID+burstTestFrames-1)
	controlLossPct := 100 * float64(cExp-cGot) / float64(cExp)
	stripedLossPct := 100 * float64(sExp-sGot) / float64(sExp)
	t.Logf("control: %.1f%% chunk loss, %d/%d frames complete; striped: %.1f%% loss, %d/%d complete; evicted=%d, mismapped=%d",
		controlLossPct, cComplete, cTotal, stripedLossPct, sComplete, sTotal, link.evicted.Load(), stripedSet.mismap.Load())

	// The hazard must bite the control, or the protection above proves
	// nothing (docs/24 finding 10).
	if controlLossPct < 5 {
		t.Fatalf("control lost only %.1f%% of chunks — the burst forwarder is not evicting; the striped assertion below would be vacuous", controlLossPct)
	}
	if cComplete == cTotal {
		t.Fatal("control completed every 18-chunk frame through an 8-deep head-drop buffer — the model is not engaged")
	}
	// The mechanism: every leg's share sits under the queue depth, so the
	// striped viewer completes what the control loses. Loopback datagram loss
	// is real on CI runners (see the QUIC test gotchas), so the bar is a wide
	// gap, not perfection.
	if sComplete < sTotal*9/10 {
		t.Fatalf("striped viewer completed %d/%d frames, want >= 90%%", sComplete, sTotal)
	}
	if stripedLossPct > controlLossPct/4 {
		t.Fatalf("striped loss %.1f%% is not clearly under the control's %.1f%% — the split is not buying headroom", stripedLossPct, controlLossPct)
	}
	if m := stripedSet.mismap.Load(); m != 0 {
		t.Fatalf("mismapped datagrams on legs: %d — the receiver-derived mapping is broken", m)
	}
}
