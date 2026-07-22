// R19 resilient mode under real packet loss (docs/24; review finding
// PRODUCT-1). Every other test of the carrier path either runs against
// in-memory fakes (hub_test.go's fakeCarrierStream) or over a zero-loss
// loopback (TestSubscribeReliableDeliversCarrierRecords) — so the one claim
// the mode exists to make, "the deltas a datagram viewer loses, a reliable
// viewer still gets", had no automated coverage at all. A regression that
// silently degraded the carrier back to lossy delivery would have shipped
// green.
//
// The loss is injected where R19 says it lives (docs/24: "the lossy leg is
// relay→viewer"): a userspace UDP forwarder in front of the relay drops a
// percentage of the packets travelling relay → subscriber, while the
// publisher stays wired straight to the relay so ingress stays clean and
// every absence downstream is attributable to the injected loss. Both
// subscribers — one `?delivery=reliable`, one plain — sit behind the same
// forwarder on their own QUIC connections, which makes the plain one a
// same-conditions control rather than a separate experiment.
//
// tc/netem was not an option: CI runners are unprivileged containers with no
// NET_ADMIN, and the drop model wanted here (downlink only, armed after the
// handshakes) is a dozen lines in-process.
package transport

import (
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/wire"
)

// lossyLink forwards UDP between a client and the relay, dropping a settable
// percentage of the packets in the relay → client direction only. The client →
// relay direction is never touched: QUIC acknowledgements have to get through
// for retransmission to be prompt, and a one-way-lossy downlink is the shape
// of the mobile links R19 targets (the viewer's uplink carries almost nothing).
type lossyLink struct {
	front    *net.UDPConn // client-facing socket; the relay URL points here
	upstream *net.UDPAddr // the real relay

	lossPercent atomic.Int64
	dropped     atomic.Uint64
	forwarded   atomic.Uint64

	mu     sync.Mutex
	peers  map[string]*net.UDPConn // client addr → its own socket to the relay
	closed bool
}

// startLossyLink brings the forwarder up in front of relayPort and returns it
// unarmed (0 % loss) — arm it with setLoss once the sessions are established,
// so the drop budget is spent on media rather than on handshakes.
func startLossyLink(t *testing.T, relayPort int) *lossyLink {
	t.Helper()
	front, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("lossy link listen: %v", err)
	}
	// The runners cap UDP buffers below what quic-go asks for (see ci.yml), and
	// an overflowing forwarder socket would inject loss this test does not
	// control — including on the uplink, which is supposed to be clean.
	_ = front.SetReadBuffer(1 << 20)
	_ = front.SetWriteBuffer(1 << 20)
	l := &lossyLink{
		front:    front,
		upstream: &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: relayPort},
		peers:    make(map[string]*net.UDPConn),
	}
	t.Cleanup(l.close)
	go l.pumpUplink()
	return l
}

func (l *lossyLink) port() int { return l.front.LocalAddr().(*net.UDPAddr).Port }

func (l *lossyLink) setLoss(percent int) { l.lossPercent.Store(int64(percent)) }

// pumpUplink forwards client → relay verbatim, opening one upstream socket per
// client address so the relay sees a stable peer per session and the replies
// come back on a socket that knows which client they belong to.
func (l *lossyLink) pumpUplink() {
	buf := make([]byte, 2048) // > quic-go's 1452-byte packet ceiling, MTU probes included
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

func (l *lossyLink) peer(addr *net.UDPAddr) *net.UDPConn {
	key := addr.String()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
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
	// Seeded per peer, so the two subscribers get independent drop patterns
	// (identical ones would make the control's loss suspiciously correlated
	// with the reliable path's recovery) without depending on wall-clock time.
	go l.pumpDownlink(c, addr, uint64(len(l.peers)))
	return c
}

// pumpDownlink is the lossy direction: relay → client, dropping lossPercent of
// packets once armed.
func (l *lossyLink) pumpDownlink(up *net.UDPConn, client *net.UDPAddr, seed uint64) {
	rng := rand.New(rand.NewPCG(seed, 0x9E3779B97F4A7C15))
	buf := make([]byte, 2048)
	for {
		n, err := up.Read(buf)
		if err != nil {
			return
		}
		if p := l.lossPercent.Load(); p > 0 && rng.IntN(100) < int(p) {
			l.dropped.Add(1)
			continue
		}
		if _, err := l.front.WriteToUDP(buf[:n], client); err != nil {
			return
		}
		l.forwarded.Add(1)
	}
}

func (l *lossyLink) close() {
	l.mu.Lock()
	l.closed = true
	peers := l.peers
	l.peers = nil
	l.mu.Unlock()
	_ = l.front.Close()
	for _, c := range peers {
		_ = c.Close()
	}
}

// carrierSink reads a reliable subscriber's server-initiated uni streams and
// collects the carrier records in delivery order. Streams are accepted in one
// goroutine (quic-go accepts in stream-ID order, so accept order is the order
// the relay opened them — and the drain opens carriers one after another, so
// records concatenated in accept order are in send order) and each is read in
// its own goroutine, because a carrier stays open until its GOP rotates.
type carrierSink struct {
	mu       sync.Mutex
	streams  [][][]byte // records per accepted stream, indexed by accept order
	kfCount  int
	records  int
	readErrs []error
}

func (c *carrierSink) claimSlot() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.streams = append(c.streams, nil)
	return len(c.streams) - 1
}

func (c *carrierSink) addRecord(slot int, record []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.streams[slot] = append(c.streams[slot], record)
	c.records++
}

func (c *carrierSink) noteKeyframe() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.kfCount++
}

func (c *carrierSink) noteErr(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.readErrs = append(c.readErrs, err)
}

func (c *carrierSink) recordCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.records
}

// runs returns the records of each carrier stream, each slice in the order
// that carrier delivered them. The runs themselves are in accept order, which
// is NOT open order (docs/22 finding 9) — callers must not read anything into
// it.
func (c *carrierSink) runs() [][][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([][][]byte, len(c.streams))
	copy(out, c.streams)
	return out
}

func (c *carrierSink) keyframes() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.kfCount
}

func (c *carrierSink) errs() []error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]error(nil), c.readErrs...)
}

// readCarriers accepts everything the relay opens toward sub until ctx ends,
// dispatching by the two-byte stream prologue exactly as the browser's
// readServerStreams does.
func readCarriers(ctx context.Context, sub *webtransport.Session, deadline time.Time, sink *carrierSink) {
	for {
		str, err := sub.AcceptUniStream(ctx)
		if err != nil {
			return
		}
		slot := sink.claimSlot()
		go func(str *webtransport.ReceiveStream, slot int) {
			// Every read is bounded: the last carrier of the run never gets a
			// FIN (the next rotation is what closes it), so an unbounded read
			// would hang the goroutine past the test.
			_ = str.SetReadDeadline(deadline)
			prologue := make([]byte, 2)
			if _, err := io.ReadFull(str, prologue); err != nil {
				sink.noteErr(fmt.Errorf("stream prologue: %w", err))
				return
			}
			switch prologue[1] {
			case wire.TypeStreamFrame:
				// A keyframe stream: reliable delivery is R8's, not R19's, but
				// it shares the uni-stream space and must keep working.
				if _, err := io.ReadAll(str); err != nil {
					sink.noteErr(fmt.Errorf("keyframe stream: %w", err))
					return
				}
				sink.noteKeyframe()
			case wire.TypeReliableCarrier:
				if err := wire.ParseCarrierPrologue(prologue); err != nil {
					sink.noteErr(fmt.Errorf("carrier prologue: %w", err))
					return
				}
				buf := make([]byte, 0, 8192)
				tmp := make([]byte, 4096)
				for {
					n, err := str.Read(tmp)
					if n > 0 {
						buf = append(buf, tmp[:n]...)
						for {
							record, rest, perr := wire.ParseCarrierRecord(buf)
							if perr != nil {
								break // incomplete record — read more
							}
							sink.addRecord(slot, append([]byte(nil), record...))
							buf = append(buf[:0], rest...)
						}
					}
					if err != nil {
						return
					}
				}
			default:
				sink.noteErr(fmt.Errorf("unexpected stream type 0x%02x", prologue[1]))
			}
		}(str, slot)
	}
}

// waitUntil is waitFor without the t.Fatal: it reports whether cond held
// before the timeout, leaving the verdict to the caller's own assertions.
func waitUntil(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
	return true
}

// chunkID names one delta chunk for ordering assertions.
type chunkID struct {
	frame uint32
	index uint16
}

func (c chunkID) String() string { return fmt.Sprintf("frame %d chunk %d", c.frame, c.index) }

// before reports whether c sorts strictly before o in send order.
func (c chunkID) before(o chunkID) bool {
	if c.frame != o.frame {
		return c.frame < o.frame
	}
	return c.index < o.index
}

// The headline R19 claim, under real loss: on a link dropping 15 % of the
// relay's packets, a `?delivery=reliable` subscriber receives every delta the
// relay fanned out — byte-identical, in order, with no holes — while a plain
// datagram subscriber behind the same link loses chunks and would freeze to
// the next keyframe (docs/24 Decisions 4/5).
//
// Two GOPs on purpose: the carrier rotation at keyframe fan-out is R19's
// designated drop point, so the test only means something if a rotation
// happens while the link is lossy.
func TestReliableCarrierRecoversDeltasLostOnALossyDownlink(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	port, clientTLS, r, _ := startTestServer(t, ctx, 15)

	// The publisher goes straight to the relay: R19's lossy leg is downstream
	// of the relay, and a clean ingress is what lets "the reliable subscriber
	// is missing a delta" mean "the carrier lost it" and nothing else.
	pub, id := dialPublisherAndGetID(t, ctx, port, clientTLS)
	defer pub.CloseWithError(0, "")

	link := startLossyLink(t, port)
	reliable := dial(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/subscribe/%s?delivery=reliable", link.port(), id), clientTLS)
	defer reliable.CloseWithError(0, "")
	plain := dial(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/subscribe/%s", link.port(), id), clientTLS)
	defer plain.CloseWithError(0, "")

	waitFor(t, 10*time.Second, func() bool { return r.Stats().Totals.Subscribers == 2 }, "both subscribers registered")
	if n := r.Stats().Broadcasts[r.ObfuscateID(id)].ReliableSubscribers; n != 1 {
		t.Fatalf("ReliableSubscribers = %d, want 1 (the ?delivery=reliable negotiation did not take)", n)
	}

	// The frames of the run: two GOPs of two-chunk deltas. Two chunks per frame
	// is the realistic shape (a full delta is 1200 B) and it is what makes a
	// single lost datagram cost the plain subscriber a whole frame.
	//
	// 15 % downlink loss over 96 datagrams puts the odds of the datagram
	// control losing nothing — the one way assertion 4 could flake — at ~1e-7,
	// while staying far from the congestion collapse that would turn the
	// carrier's recovery into a timeout.
	const (
		gops         = 2
		deltasPerGOP = 24
		chunks       = 2
		lossPercent  = 15
	)
	type delta struct {
		id    chunkID
		dgram []byte
	}
	var sent []delta
	byBytes := make(map[string]chunkID)
	frameID := uint32(1)
	var gopKeyframes []uint32
	for range gops {
		gopKeyframes = append(gopKeyframes, frameID)
		frameID++
		for range deltasPerGOP {
			for i, dgram := range encodeFrame(t, frameID, false, chunks) {
				cid := chunkID{frame: frameID, index: uint16(i)}
				sent = append(sent, delta{id: cid, dgram: dgram})
				byBytes[string(dgram)] = cid
			}
			frameID++
		}
	}

	// Arm the loss only now: a dropped handshake packet costs a retransmit and
	// proves nothing, and the point is to lose *media*.
	link.setLoss(lossPercent)

	deadline := time.Now().Add(30 * time.Second)
	sink := &carrierSink{}
	readCtx, readCancel := context.WithDeadline(ctx, deadline)
	defer readCancel()
	go readCarriers(readCtx, reliable, deadline, sink)

	// The datagram control: same link, same loss, unreliable delivery.
	var plainMu sync.Mutex
	plainSeen := make(map[chunkID]bool)
	go func() {
		for {
			dgram, err := plain.ReceiveDatagram(readCtx)
			if err != nil {
				return
			}
			cid, ok := byBytes[string(dgram)]
			if !ok {
				continue // ClockMapping/TimeSync and friends
			}
			plainMu.Lock()
			plainSeen[cid] = true
			plainMu.Unlock()
		}
	}()

	// Publish, synchronously: a keyframe stream opens each GOP (and rotates the
	// carrier), then its deltas, paced so nothing overruns a receive buffer on
	// the way in — an ingress drop here would be loss this test does not
	// control. Sending on this goroutine (rather than a background one) is what
	// makes the DatagramsRelayed reading below a count of everything sent
	// rather than a race with the sender.
	next := 0
	for g := range gops {
		sendKeyframeStream(t, pub, buildStreamKeyframe(t, gopKeyframes[g], "avc1.42E02A", 4096))
		for range deltasPerGOP {
			for range chunks {
				if err := pub.SendDatagram(sent[next].dgram); err != nil {
					t.Fatalf("SendDatagram %v: %v", sent[next].id, err)
				}
				next++
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	// Wait for the carrier to deliver everything the relay took in. Poll on
	// what the relay says it fanned out rather than on what we sent, so an
	// ingress drop degrades this into a smaller — still meaningful — run
	// instead of a hang.
	//
	// Deliberately not fatal on timeout: a carrier that delivers nothing is
	// precisely the regression this test exists to catch, and "missing 96 of
	// 96 deltas" from the assertions below names it far better than "timed out
	// waiting" would. 10 s is ~20× the healthy settle.
	if !waitUntil(10*time.Second, func() bool {
		relayed := r.Stats().Broadcasts[r.ObfuscateID(id)].DatagramsRelayed
		return relayed > 0 && sink.recordCount() >= int(relayed)
	}) {
		t.Logf("gave up waiting for the carrier: %d records for %d relayed deltas",
			sink.recordCount(), r.Stats().Broadcasts[r.ObfuscateID(id)].DatagramsRelayed)
	}
	// Give the datagram control the same wall-clock chance to receive.
	time.Sleep(500 * time.Millisecond)

	stats := r.Stats().Broadcasts[r.ObfuscateID(id)]
	relayed := int(stats.DatagramsRelayed)
	ingressLost := len(sent) - relayed
	if ingressLost > 0 {
		// Not fatal: it only shrinks the run, and the assertions below account
		// for it explicitly. Loud, because a systematic ingress drop would
		// quietly weaken all of them.
		t.Logf("NOTE: relay fanned out %d of %d delta datagrams — %d were lost publisher→relay (loopback buffer pressure, not the injected loss)",
			relayed, len(sent), ingressLost)
	}

	// The loss injection actually fired, and hard enough to matter: without
	// this, every assertion below would also pass on a clean link.
	if link.dropped.Load() == 0 {
		t.Fatalf("the lossy link dropped nothing (%d packets forwarded) — the test proves nothing", link.forwarded.Load())
	}
	t.Logf("lossy link: %d packets dropped, %d forwarded (%.1f%%)",
		link.dropped.Load(), link.forwarded.Load(),
		100*float64(link.dropped.Load())/float64(link.dropped.Load()+link.forwarded.Load()))

	// 1. Every record is a delta we sent, verbatim, and none arrives twice.
	runs := sink.runs()
	total := 0
	seen := make(map[chunkID]bool)
	ordered := make([][]chunkID, 0, len(runs))
	for _, records := range runs {
		ids := make([]chunkID, 0, len(records))
		for i, record := range records {
			cid, ok := byBytes[string(record)]
			if !ok {
				t.Fatalf("carrier record %d (%d bytes) is not one of the deltas we sent — the relay is not forwarding verbatim", i, len(record))
			}
			if seen[cid] {
				t.Errorf("carrier delivered %v twice", cid)
			}
			seen[cid] = true
			ids = append(ids, cid)
		}
		total += len(ids)
		if len(ids) > 0 {
			ordered = append(ordered, ids)
		}
	}

	// 2. In order. Reliable *in-order* delivery is the whole mechanism: if
	//    records can arrive out of order the viewer's reorder buffer is doing
	//    work R19 promised it would not have to.
	//
	//    Within a carrier that is QUIC's guarantee. Across carriers it is the
	//    drain's: it retires one carrier before opening the next, so the GOPs
	//    partition the sequence into contiguous, non-interleaved runs. Accept
	//    order is deliberately NOT used to order the runs — webtransport-go
	//    does not deliver accepted streams in the order the peer opened them
	//    (docs/22 finding 9) — so they are sorted by their first record and the
	//    assertion is that the concatenation is still strictly ascending. An
	//    interleaving carrier makes that impossible to satisfy.
	slices.SortFunc(ordered, func(a, b []chunkID) int {
		switch {
		case a[0].before(b[0]):
			return -1
		case b[0].before(a[0]):
			return 1
		}
		return 0
	})
	var order []chunkID
	for _, ids := range ordered {
		order = append(order, ids...)
	}
	for i := 1; i < len(order); i++ {
		if !order[i-1].before(order[i]) {
			t.Errorf("carrier records out of order: %v arrived after %v", order[i], order[i-1])
			break
		}
	}

	// 3. No holes — the "zero gap resyncs" claim. Anything the relay fanned
	//    out and the carrier did not deliver is exactly the freeze-to-keyframe
	//    stutter resilient mode exists to prevent. Absences are held against
	//    the carrier unless ingress loss already accounts for them.
	var missing []chunkID
	for _, d := range sent {
		if !seen[d.id] {
			missing = append(missing, d.id)
		}
	}
	if len(missing) > ingressLost {
		t.Errorf("reliable subscriber is missing %d of %d relayed deltas (first: %v) — the carrier lost data QUIC should have retransmitted",
			len(missing)-ingressLost, relayed, missing[0])
	}
	if stats.CarrierRecordsDropped != 0 {
		t.Errorf("CarrierRecordsDropped = %d, want 0: the relay itself dropped records on a link that was only lossy, not slow", stats.CarrierRecordsDropped)
	}
	if stats.CarrierStreams < gops {
		t.Errorf("CarrierStreams = %d, want >= %d (one per GOP — no rotation happened, so the rotation drop point went untested)", stats.CarrierStreams, gops)
	}
	if kf := sink.keyframes(); kf < gops {
		t.Errorf("reliable subscriber received %d keyframe streams, want >= %d", kf, gops)
	}

	// 4. The control: the same loss on the same link does hurt a datagram
	//    subscriber, and hurts it strictly more than it hurt the carrier. This
	//    is what makes the run above evidence of recovery rather than evidence
	//    of a link that happened to behave.
	plainMu.Lock()
	plainMissing := 0
	for _, d := range sent {
		if !plainSeen[d.id] {
			plainMissing++
		}
	}
	plainMu.Unlock()
	switch {
	case plainMissing == 0:
		t.Errorf("the datagram control received all %d deltas at %d%% downlink loss — the injected loss is not reaching the subscribers, so the reliable path was never tested against anything",
			len(sent), lossPercent)
	case plainMissing <= len(missing):
		t.Errorf("the carrier missed %d deltas and the datagram control only %d — reliable delivery bought nothing over unreliable on this link",
			len(missing), plainMissing)
	}
	t.Logf("carrier delivered %d/%d relayed deltas (%d missing); the datagram control missed %d of %d",
		total, relayed, len(missing), plainMissing, len(sent))

	for _, err := range sink.errs() {
		t.Errorf("carrier stream read: %v", err)
	}
}
