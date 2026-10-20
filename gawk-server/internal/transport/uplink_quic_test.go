package transport

// R73 RU1 (docs/75 D10, §10): what the relay's QUIC stack does with a
// carrier — flow control on a session's first GOP, and the memory a hostile
// publisher can pin behind a hole. Both need a view of the publisher's QUIC
// packets, so the publisher here is a quic-go client whose packets go
// through a test PacketConn (a fixed one-way delay, and a hole that drops
// every packet carrying a chosen stream byte) while a qlog tracer reports
// what each packet carried.

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/qlog"
	"github.com/quic-go/quic-go/qlogwriter"
	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// --- the publisher's packets ------------------------------------------------

// sentPacket is what the tracer saw for one packet the publisher sent.
type sentPacket struct {
	short  bool
	length int
	drop   bool
}

// packetTap is the publisher's view of its own QUIC packets. The tracer
// appends one entry per packet in send order; the PacketConn pops as many
// entries as the datagram it is writing holds packets (coalesced long-header
// packets are counted from their clear-text Length fields), and drops the
// datagram if any of them carries the hole. quic-go logs every packet before
// it queues it, and its send queue is FIFO, so the two stay aligned; a
// short-header packet's logged length is checked against the datagram to
// prove it.
type packetTap struct {
	mu      sync.Mutex
	pending []sentPacket
	misfit  error

	// hole: any packet carrying byte holeOffset of a stream in holeStreams is
	// dropped. holeStreams is filled as the test opens its carriers.
	holeOffset  int64
	holeStreams map[quic.StreamID]bool
	// maxEnd is, per hole stream, the highest stream offset delivered (in a
	// packet that was not dropped) — what the relay holds beyond the hole.
	maxEnd  map[quic.StreamID]int64
	dropped int

	blocked atomic.Int64 // DATA_BLOCKED + STREAM_DATA_BLOCKED frames sent

	// The relay's initial windows, from its transport parameters.
	peerStreamWindow, peerConnWindow int64
}

func newPacketTap(holeOffset int64) *packetTap {
	return &packetTap{
		holeOffset:  holeOffset,
		holeStreams: map[quic.StreamID]bool{},
		maxEnd:      map[quic.StreamID]int64{},
	}
}

func (p *packetTap) addHoleStream(id quic.StreamID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.holeStreams[id] = true
}

// heldBeyondHoles sums, over the hole streams, the bytes delivered past the
// hole: the data the relay's QUIC stack must buffer and cannot release.
func (p *packetTap) heldBeyondHoles() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	var n int64
	for id := range p.holeStreams {
		if end := p.maxEnd[id]; end > p.holeOffset {
			n += end - p.holeOffset
		}
	}
	return n
}

func (p *packetTap) err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.misfit
}

func (p *packetTap) droppedCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.dropped
}

// record is the tracer side: called on quic-go's connection goroutine.
func (p *packetTap) record(ev qlog.PacketSent) {
	sp := sentPacket{short: ev.Header.PacketType == qlog.PacketType1RTT, length: ev.Raw.Length}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, f := range ev.Frames {
		switch fr := f.Frame.(type) {
		case *qlog.DataBlockedFrame, *qlog.StreamDataBlockedFrame:
			p.blocked.Add(1)
		case *qlog.StreamFrame:
			if p.holeStreams[fr.StreamID] && fr.Offset <= p.holeOffset && p.holeOffset < fr.Offset+fr.Length {
				sp.drop = true
			}
		}
	}
	p.pending = append(p.pending, sp)
	if sp.drop {
		return
	}
	for _, f := range ev.Frames {
		if fr, ok := f.Frame.(*qlog.StreamFrame); ok && p.holeStreams[fr.StreamID] {
			p.maxEnd[fr.StreamID] = max(p.maxEnd[fr.StreamID], fr.Offset+fr.Length)
		}
	}
}

// admit is the PacketConn side: whether to send this datagram.
func (p *packetTap) admit(dgram []byte) bool {
	packets, lastShortLen := countQUICPackets(dgram)
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.pending) < packets {
		p.misfit = fmt.Errorf("datagram of %d packets with %d logged", packets, len(p.pending))
		p.pending = nil
		return true
	}
	popped := p.pending[:packets]
	p.pending = p.pending[packets:]
	drop := false
	for i, sp := range popped {
		if sp.drop {
			drop = true
		}
		if i == packets-1 && lastShortLen > 0 && (!sp.short || sp.length != lastShortLen) && p.misfit == nil {
			p.misfit = fmt.Errorf("short packet of %d bytes aligned with a logged %+v", lastShortLen, sp)
		}
	}
	if drop {
		p.dropped++
	}
	return !drop
}

// countQUICPackets counts the QUIC packets in a UDP datagram: long-header
// packets carry their length in clear text; a short-header packet runs to
// the end (its length is returned for the alignment check).
func countQUICPackets(b []byte) (n, shortLen int) {
	for len(b) > 0 {
		if b[0]&0x80 == 0 {
			return n + 1, len(b)
		}
		// Long header: flags, version, DCID len+DCID, SCID len+SCID,
		// [Initial: token length + token], Length, packet number + payload.
		i := 5
		i += 1 + int(b[i])
		i += 1 + int(b[i])
		if (b[0]>>4)&0x3 == 0 { // Initial
			tl, l := quicVarint(b[i:])
			i += l + int(tl)
		}
		length, l := quicVarint(b[i:])
		i += l + int(length)
		n++
		if i >= len(b) {
			return n, 0
		}
		b = b[i:]
	}
	return n, 0
}

func quicVarint(b []byte) (uint64, int) {
	l := 1 << (b[0] >> 6)
	v := uint64(b[0] & 0x3f)
	for i := 1; i < l; i++ {
		v = v<<8 | uint64(b[i])
	}
	return v, l
}

// tapConn is the publisher's PacketConn: a fixed one-way delay on every
// datagram it sends (order preserved), and the tap's hole. It implements
// only net.PacketConn, so quic-go sends one packet per write (no GSO).
type tapConn struct {
	net.PacketConn
	tap   *packetTap
	delay time.Duration
	out   chan delayed
}

type delayed struct {
	due  time.Time
	b    []byte
	addr net.Addr
}

func newTapConn(t *testing.T, tap *packetTap, delay time.Duration) *tapConn {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	c := &tapConn{PacketConn: pc, tap: tap, delay: delay, out: make(chan delayed, 1<<16)}
	go func() {
		for d := range c.out {
			time.Sleep(time.Until(d.due))
			_, _ = c.PacketConn.WriteTo(d.b, d.addr)
		}
	}()
	t.Cleanup(func() { pc.Close() })
	return c
}

func (c *tapConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	if !c.tap.admit(b) {
		return len(b), nil
	}
	if c.delay == 0 {
		return c.PacketConn.WriteTo(b, addr)
	}
	c.out <- delayed{due: time.Now().Add(c.delay), b: append([]byte(nil), b...), addr: addr}
	return len(b), nil
}

// tapTrace is the qlog trace feeding the tap; it claims no schema, so http3
// adds no producer of its own.
type tapTrace struct{ tap *packetTap }

func (t tapTrace) AddProducer() qlogwriter.Recorder { return tapRecorder(t) }
func (tapTrace) SupportsSchemas(string) bool        { return false }

type tapRecorder tapTrace

func (r tapRecorder) RecordEvent(ev qlogwriter.Event) {
	switch e := ev.(type) {
	case qlog.PacketSent:
		r.tap.record(e)
	case qlog.ParametersSet:
		if e.Initiator == qlog.InitiatorRemote {
			r.tap.mu.Lock()
			r.tap.peerStreamWindow = int64(e.InitialMaxStreamDataUni)
			r.tap.peerConnWindow = int64(e.InitialMaxData)
			r.tap.mu.Unlock()
		}
	}
}
func (tapRecorder) Close() error { return nil }

// dialTappedPublisher dials /publish through a tapConn and returns the
// session and broadcast ID.
func dialTappedPublisher(t *testing.T, ctx context.Context, port int, clientTLS *tls.Config, tap *packetTap, delay time.Duration) (*webtransport.Session, string) {
	t.Helper()
	conn := newTapConn(t, tap, delay)
	qtr := &quic.Transport{Conn: conn}
	t.Cleanup(func() { qtr.Close() })
	d := webtransport.Transport{
		TLSClientConfig: clientTLS,
		QUICConfig: &quic.Config{
			EnableDatagrams:                  true,
			EnableStreamResetPartialDelivery: true,
			Tracer: func(context.Context, bool, quic.ConnectionID) qlogwriter.Trace {
				return tapTrace{tap: tap}
			},
		},
		DialAddr: func(ctx context.Context, addr string, tlsCfg *tls.Config, cfg *quic.Config) (*quic.Conn, error) {
			ua, err := net.ResolveUDPAddr("udp", addr)
			if err != nil {
				return nil, err
			}
			return qtr.DialEarly(ctx, ua, tlsCfg, cfg)
		},
	}
	t.Cleanup(func() { d.Close() })
	_, sess, err := d.Dial(ctx, fmt.Sprintf("https://127.0.0.1:%d/publish", port), nil)
	if err != nil {
		t.Fatalf("Dial /publish: %v", err)
	}
	id, _ := readPublisherHandshake(t, ctx, sess)
	return sess, id
}

// --- a session's first GOP --------------------------------------------------

// gop1440pBytes is one GOP of deltas at the 1440p rung, as docs/75 D10 sizes
// it (~1.8 MB); it rides one carrier in full-size records.
const gop1440pBytes = 1_800_000

// firstGop is what the publisher saw on a session's first GOP.
type firstGop struct {
	carrierBytes int   // the carrier's application bytes
	streamWindow int64 // the relay's initial_max_stream_data_uni
	connWindow   int64 // the relay's initial_max_data
	blocked      int64 // DATA_BLOCKED + STREAM_DATA_BLOCKED frames sent
}

// sendFirstGop opens a fresh publisher session behind a 10 ms RTT (table H's
// path; the delay is on the publisher's side), writes a keyframe and then one
// 1440p GOP of records on one carrier, and reports the relay's initial
// windows as the publisher received them and how often the publisher's QUIC
// stack was blocked by flow control.
func sendFirstGop(t *testing.T, bitrateBytes int64) firstGop {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := uplinkTestConfig(true)
	cfg.UplinkMaxBitrateBytes = bitrateBytes
	port, clientTLS, r, _ := startTestServerCfg(t, ctx, cfg)
	tap := newPacketTap(-1)
	pub, id := dialTappedPublisher(t, ctx, port, clientTLS, tap, 10*time.Millisecond)
	defer pub.CloseWithError(0, "")

	sendKeyframeStream(t, pub, buildStreamKeyframe(t, 0, "avc1.42E02A", 200_000))
	n := gop1440pBytes / wire.MaxDatagramSize
	carrier := carrierStreamBytes(t, uplinkRecords(t, 1, n))
	car, err := pub.OpenUniStream()
	if err != nil {
		t.Fatalf("OpenUniStream: %v", err)
	}
	if _, err := car.Write(carrier); err != nil {
		t.Fatalf("write carrier: %v", err)
	}
	if err := car.Close(); err != nil {
		t.Fatalf("close carrier: %v", err)
	}
	waitFor(t, 20*time.Second, func() bool {
		return r.Stats().Broadcasts[r.ObfuscateID(id)].UplinkRecords == uint64(n)
	}, "the whole GOP ingested")
	if err := tap.err(); err != nil {
		t.Fatalf("packet tap: %v", err)
	}
	tap.mu.Lock()
	defer tap.mu.Unlock()
	return firstGop{
		carrierBytes: len(carrier),
		streamWindow: tap.peerStreamWindow,
		connWindow:   tap.peerConnWindow,
		blocked:      tap.blocked.Load(),
	}
}

// docs/75 D10: with the windows derived from -uplink-max-bitrate, a full
// 1440p GOP fits the relay's initial windows, so it passes one carrier on a
// session's first GOP without a single write blocked by flow control. With
// quic-go's defaults the same GOP does not fit, and whether it blocks is a
// race between the relay's window updates and the sender (it did, on 10 and
// 25 ms paths, while this was written) — so the control asserts the misfit,
// which is deterministic, and only logs the blocking.
func TestUplinkFirstGopIsNotFlowControlBlocked(t *testing.T) {
	got := sendFirstGop(t, 50_000_000/8)
	if got.streamWindow != 2_000_000 || got.connWindow != 3_000_000 {
		t.Fatalf("relay advertised %d / %d, want D10's 2000000 / 3000000", got.streamWindow, got.connWindow)
	}
	if int64(got.carrierBytes) > got.streamWindow {
		t.Fatalf("a %d-byte GOP carrier exceeds the %d-byte stream window", got.carrierBytes, got.streamWindow)
	}
	if got.blocked != 0 {
		t.Fatalf("with D10's windows the first GOP sent %d BLOCKED frames, want 0", got.blocked)
	}

	ctl := sendFirstGop(t, 0)
	if int64(ctl.carrierBytes) <= ctl.streamWindow {
		t.Fatalf("control: the %d-byte GOP fits quic-go's default %d-byte window; the test cannot tell the windows apart",
			ctl.carrierBytes, ctl.streamWindow)
	}
	t.Logf("control: quic-go's default windows (%d / %d) blocked the same GOP %d times", ctl.streamWindow, ctl.connWindow, ctl.blocked)
}

// --- a hostile publisher ----------------------------------------------------

// docs/75 §10: a publisher can hold relay memory by leaving a hole on
// purpose — out-of-order data the relay's QUIC stack must buffer and can
// neither read nor drop. Both carriers a publisher may open get a hole a
// little way in, and the publisher keeps writing until flow control stops
// it. What the relay holds must stay within its connection receive window;
// the measured number is recorded in docs/75 §8 RU1.
func TestUplinkHostilePublisherHoleIsBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("writes until flow control stops a hostile publisher")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, r, _ := startTestServerCfg(t, ctx, uplinkTestConfig(true))
	const holeOffset = 64 << 10
	tap := newPacketTap(holeOffset)
	pub, id := dialTappedPublisher(t, ctx, port, clientTLS, tap, 0)
	defer pub.CloseWithError(0, "")

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	recs := carrierStreamBytes(t, uplinkRecords(t, 1, 64))[wire.CarrierPrologueSize:]
	var writers sync.WaitGroup
	for range hub.MaxUplinkCarriers {
		car, err := pub.OpenUniStream()
		if err != nil {
			t.Fatalf("OpenUniStream: %v", err)
		}
		tap.addHoleStream(car.StreamID())
		writers.Add(1)
		go func() {
			defer writers.Done()
			if _, err := car.Write(wire.AppendCarrierPrologue(nil)); err != nil {
				return
			}
			for {
				if _, err := car.Write(recs); err != nil {
					return // the session closing at the end of the test
				}
			}
		}()
	}

	// quic-go's own loss recovery would otherwise stall the publisher: once
	// the only packets in flight are probes carrying the hole, nothing is
	// acknowledged and the probe timeout backs off without end. A hostile
	// stack would not back off; a trickle of datagrams the relay does
	// acknowledge keeps quic-go's loss detection, and so its sending, going.
	keepalive := uplinkRecords(t, 1_000_000, 1)[0]
	stopKeepalive := make(chan struct{})
	defer close(stopKeepalive)
	go func() {
		tick := time.NewTicker(time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stopKeepalive:
				return
			case <-tick.C:
				_ = pub.SendDatagram(keepalive)
			}
		}
	}()

	// Wait until the publisher can push no more: the held bytes stop growing.
	var held int64
	stableSince := time.Now()
	deadline := time.Now().Add(30 * time.Second)
	for time.Since(stableSince) < 2*time.Second {
		if time.Now().After(deadline) {
			t.Fatalf("held bytes never settled (last %d)", held)
		}
		time.Sleep(50 * time.Millisecond)
		if h := tap.heldBeyondHoles(); h != held {
			held, stableSince = h, time.Now()
		}
	}
	if err := tap.err(); err != nil {
		t.Fatalf("packet tap: %v", err)
	}

	runtime.GC()
	var during runtime.MemStats
	runtime.ReadMemStats(&during)
	heapGrowth := int64(during.HeapAlloc) - int64(before.HeapAlloc)

	st := r.Stats().Broadcasts[r.ObfuscateID(id)]
	t.Logf("hostile publisher: %d bytes held behind two holes (stream window %d, connection window %d, connection maximum %d); process heap grew %d bytes; %d datagrams dropped to keep the holes; %d records ingested before them",
		held, 2_000_000, 3_000_000, quicDefaultMaxConnectionWindow, heapGrowth, tap.droppedCount(), st.UplinkRecords)

	if held == 0 {
		t.Fatal("no data reached the relay past the holes; the test measured nothing")
	}
	if held > quicDefaultMaxConnectionWindow {
		t.Fatalf("relay holds %d bytes behind the holes, more than the %d-byte connection receive window", held, quicDefaultMaxConnectionWindow)
	}
	if st.UplinkRecords == 0 || st.UplinkCarriers != hub.MaxUplinkCarriers {
		t.Fatalf("carriers %d records %d: the records before the holes should have been ingested", st.UplinkCarriers, st.UplinkRecords)
	}
	pub.CloseWithError(0, "")
	writers.Wait()
}
