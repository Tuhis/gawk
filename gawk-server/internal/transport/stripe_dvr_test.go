package transport

// R87 SG1 (docs/91 §6) against a real relay over real QUIC: the DVR leg dial
// gate, the capability bit, and a striped DVR viewer — a primary and its
// legs, through a lossy link — receiving every GOP exactly once between its
// connections, each whole on one connection.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/quic-go/webtransport-go"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/tlsutil"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

func stripedDVRTestConfig(maxSubs int) config.Config {
	cfg := stripedTestConfig(maxSubs)
	cfg.StripedDVR = true
	return cfg
}

// TestStripedDVRLegDialValidation (docs/91 D8): with striped DVR on, a leg
// whose delivery negotiates DVR is accepted as leg 1…N−1. Leg 0, a plain
// reliable leg and a buffer that would degrade are refused pre-upgrade, as
// is every DVR leg on a relay without the flag.
func TestStripedDVRLegDialValidation(t *testing.T) {
	t.Parallel()
	const owner = "&owner=aabbccdd00112233"
	const dvr = "&delivery=reliable&buffer=2500"
	for _, tc := range []struct {
		name       string
		striped    bool
		stripedDVR bool
		params     string
		wantOK     bool
	}{
		{"dvr leg", true, true, "?stripe=3&leg=2" + dvr + owner, true},
		{"dvr leg zero", true, true, "?stripe=2&leg=0" + dvr + owner, false},
		{"plain reliable leg", true, true, "?stripe=2&leg=1&delivery=reliable" + owner, false},
		{"degrading buffer", true, true, "?stripe=2&leg=1&delivery=reliable&buffer=500" + owner, false},
		{"buffer without reliable", true, true, "?stripe=2&leg=1&buffer=2500" + owner, false},
		{"dvr leg without owner", true, true, "?stripe=2&leg=1" + dvr, false},
		{"striped dvr off", true, false, "?stripe=2&leg=1" + dvr + owner, false},
		{"striped delivery off", false, true, "?stripe=2&leg=1" + dvr + owner, false},
		{"datagram leg unchanged", true, true, "?stripe=2&leg=0" + owner, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cfg := stripedTestConfig(15)
			cfg.StripedDelivery = tc.striped
			cfg.StripedDVR = tc.stripedDVR
			port, clientTLS, _, _ := startTestServerCfg(t, ctx, cfg)
			pub, id := dialPublisherAndGetID(t, ctx, port, clientTLS)
			defer pub.CloseWithError(0, "")

			url := fmt.Sprintf("https://127.0.0.1:%d/subscribe/%s%s", port, id, tc.params)
			_, sess, err := dialOnce(t, ctx, url, clientTLS)
			if err == nil {
				sess.CloseWithError(0, "")
			}
			if ok := err == nil; ok != tc.wantOK {
				t.Fatalf("dial accepted = %v, want %v (err %v)", ok, tc.wantOK, err)
			}
		})
	}
}

// TestStripedDVRCapabilityBit: CapStripedDVR is advertised to viewers only
// when striped delivery and striped DVR are both on.
func TestStripedDVRCapabilityBit(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                string
		striped, stripedDVR bool
		wantFlags           uint16
	}{
		{"both on", true, true, wire.CapParityChunks | wire.CapStripedDelivery | wire.CapStripedDVR},
		{"striped DVR off", true, false, wire.CapParityChunks | wire.CapStripedDelivery},
		{"striped delivery off", false, true, wire.CapParityChunks},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cfg := stripedTestConfig(15)
			cfg.StripedDelivery = tc.striped
			cfg.StripedDVR = tc.stripedDVR
			port, clientTLS, _, _ := startTestServerCfg(t, ctx, cfg)
			pub, id := dialPublisherAndGetID(t, ctx, port, clientTLS)
			defer pub.CloseWithError(0, "")
			sub := dialSubscriber(t, ctx, port, id, clientTLS)
			defer sub.CloseWithError(0, "")
			if got := readCapabilityFlags(t, ctx, sub); got != tc.wantFlags {
				t.Fatalf("flags = %#04x, want %#04x", got, tc.wantFlags)
			}
		})
	}
}

// readCapabilityFlags returns the flags of the first RelayCapabilities
// message the relay opens toward sess.
func readCapabilityFlags(t *testing.T, ctx context.Context, sess *webtransport.Session) uint16 {
	t.Helper()
	streamCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	for {
		str, err := sess.AcceptUniStream(streamCtx)
		if err != nil {
			t.Fatal("no capabilities stream arrived")
		}
		payload, err := io.ReadAll(str)
		if err != nil {
			t.Fatalf("read uni stream: %v", err)
		}
		if len(payload) >= 2 && payload[1] == wire.TypeRelayCapabilities {
			caps, err := wire.ParseRelayCapabilities(payload)
			if err != nil {
				t.Fatalf("ParseRelayCapabilities: %v", err)
			}
			return caps.Flags
		}
	}
}

// gopSink collects what one connection of a striped DVR viewer received:
// the frameIDs of its keyframe streams and, per carrier, the frameIDs of
// its records in carrier order.
type gopSink struct {
	mu        sync.Mutex
	keyframes []uint32
	carriers  [][]uint32
	errs      []error
}

func (g *gopSink) read(ctx context.Context, sess *webtransport.Session, deadline time.Time) {
	for {
		str, err := sess.AcceptUniStream(ctx)
		if err != nil {
			return
		}
		go func(str *webtransport.ReceiveStream) {
			_ = str.SetReadDeadline(deadline)
			prologue := make([]byte, 2)
			if _, err := io.ReadFull(str, prologue); err != nil {
				return
			}
			switch prologue[1] {
			case wire.TypeStreamFrame:
				rest, err := io.ReadAll(str)
				if err != nil {
					g.note(fmt.Errorf("keyframe stream: %w", err))
					return
				}
				hdr, err := wire.ParseStreamFrameHeader(append(prologue, rest...))
				if err != nil {
					g.note(fmt.Errorf("keyframe header: %w", err))
					return
				}
				g.mu.Lock()
				g.keyframes = append(g.keyframes, hdr.FrameID)
				g.mu.Unlock()
			case wire.TypeReliableCarrier:
				g.mu.Lock()
				slot := len(g.carriers)
				g.carriers = append(g.carriers, nil)
				g.mu.Unlock()
				var buf []byte
				tmp := make([]byte, 4096)
				for {
					n, err := str.Read(tmp)
					buf = append(buf, tmp[:n]...)
					for {
						rec, rest, perr := wire.ParseCarrierRecord(buf)
						if perr != nil {
							break
						}
						if h, _, err := wire.ParseVideoChunk(rec); err == nil {
							g.mu.Lock()
							g.carriers[slot] = append(g.carriers[slot], h.FrameID)
							g.mu.Unlock()
						}
						buf = append(buf[:0], rest...)
					}
					if err != nil {
						return
					}
				}
			}
		}(str)
	}
}

func (g *gopSink) note(err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.errs = append(g.errs, err)
}

func (g *gopSink) snapshot() (keyframes []uint32, carriers [][]uint32, errs []error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	keyframes = slices.Clone(g.keyframes)
	for _, c := range g.carriers {
		carriers = append(carriers, slices.Clone(c))
	}
	return keyframes, carriers, slices.Clone(g.errs)
}

func (g *gopSink) records() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, c := range g.carriers {
		n += len(c)
	}
	return n
}

// TestStripedDVRDeliversEveryGOPOnce is SG1's end-to-end shape over real
// QUIC, through a lossy downlink: a DVR primary striped ×3 and its two legs
// receive every GOP exactly once between them, each GOP's keyframe and
// records on the connection its residue names, and every record in order
// on its GOP's carrier.
func TestStripedDVRDeliversEveryGOPOnce(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	port, clientTLS, r, _ := startTestServerCfg(t, ctx, stripedDVRTestConfig(15))
	pub, id := dialPublisherAndGetID(t, ctx, port, clientTLS)
	defer pub.CloseWithError(0, "")

	link := startLossyLink(t, port)
	const owner, n = "aabbccdd00112233", 3
	base := fmt.Sprintf("https://127.0.0.1:%d/subscribe/%s?delivery=reliable&buffer=2500&owner=%s", link.port(), id, owner)
	conns := []*webtransport.Session{dial(t, ctx, base, clientTLS)}
	for j := 1; j < n; j++ {
		conns = append(conns, dial(t, ctx, fmt.Sprintf("%s&stripe=%d&leg=%d", base, n, j), clientTLS))
	}
	for _, c := range conns {
		defer c.CloseWithError(0, "")
	}
	waitFor(t, 10*time.Second, func() bool { return r.Stats().Totals.Subscribers == n }, "the primary and its legs registered")

	striped, err := wire.AppendStripeState(nil, wire.StripeState{Striped: true, StripeN: n})
	if err != nil {
		t.Fatalf("AppendStripeState: %v", err)
	}
	waitFor(t, 10*time.Second, func() bool {
		_ = conns[0].SendDatagram(striped)
		return r.Stats().Totals.StripedPrimaries == 1
	}, "the DVR primary striped")
	// The viewer's 1 Hz refresh (docs/35 §5.3), so the state outlives the
	// run's StripeStateTTL.
	refreshCtx, refreshStop := context.WithCancel(ctx)
	defer refreshStop()
	go func() {
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-refreshCtx.Done():
				return
			case <-tick.C:
				_ = conns[0].SendDatagram(striped)
			}
		}
	}()

	deadline := time.Now().Add(60 * time.Second)
	readCtx, readCancel := context.WithDeadline(ctx, deadline)
	defer readCancel()
	sinks := make([]*gopSink, n)
	for j, c := range conns {
		sinks[j] = &gopSink{}
		go sinks[j].read(readCtx, c, deadline)
	}
	// Loss on every connection's downlink: carriers retransmit, so it may
	// cost time but never a record.
	link.setLoss(3)

	const gops, deltas, gopLen = 30, 4, 30
	for k := range gops {
		kf := uint32(1 + gopLen*k)
		sendKeyframeStream(t, pub, buildStreamKeyframe(t, kf, "avc1.42E02A", 2048))
		// A delta that beats its keyframe into the ring joins the previous
		// GOP. That is correct there, and a race this test does not measure.
		time.Sleep(80 * time.Millisecond)
		for i := range deltas {
			if err := pub.SendDatagram(encodeFrame(t, kf+uint32(i)+1, false, 1)[0]); err != nil {
				t.Fatalf("SendDatagram: %v", err)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	if !waitUntil(30*time.Second, func() bool {
		total := 0
		for _, s := range sinks {
			total += s.records()
		}
		return total >= gops*deltas
	}) {
		t.Log("gave up waiting for every record")
	}
	link.setLoss(0)
	time.Sleep(300 * time.Millisecond)

	gopOf := func(frameID uint32) uint32 { return 1 + (frameID-1)/gopLen*gopLen }
	servedBy := map[uint32]int{}
	recordsSeen := map[uint32]int{}
	for j, s := range sinks {
		keyframes, carriers, errs := s.snapshot()
		for _, err := range errs {
			t.Errorf("connection %d: %v", j, err)
		}
		mine := map[uint32]bool{}
		for _, kf := range keyframes {
			if prev, dup := servedBy[kf]; dup {
				t.Errorf("GOP %d served by connection %d and %d", kf, prev, j)
			}
			servedBy[kf] = j
			mine[kf] = true
			if want := wire.StripeGOPResidue(kf, n); want != j {
				t.Errorf("GOP %d on connection %d, its residue is %d", kf, j, want)
			}
		}
		for c, recs := range carriers {
			for i, fid := range recs {
				recordsSeen[fid]++
				if gopOf(fid) != gopOf(recs[0]) {
					t.Errorf("connection %d carrier %d mixes GOPs: %v", j, c, recs)
					break
				}
				if i > 0 && fid <= recs[i-1] {
					t.Errorf("connection %d carrier %d out of order: %v", j, c, recs)
					break
				}
				if !mine[gopOf(fid)] {
					t.Errorf("connection %d carried a record of GOP %d without its keyframe", j, gopOf(fid))
				}
			}
		}
	}
	for k := range gops {
		kf := uint32(1 + gopLen*k)
		if _, ok := servedBy[kf]; !ok {
			t.Errorf("GOP %d served by no connection", kf)
		}
		for i := range deltas {
			if got := recordsSeen[kf+uint32(i)+1]; got != 1 {
				t.Errorf("delta %d arrived %d times across the stripe, want 1", kf+uint32(i)+1, got)
			}
		}
	}
	for j, s := range sinks {
		if kfs, _, _ := s.snapshot(); len(kfs) == 0 {
			t.Errorf("connection %d served no GOP", j)
		}
	}
	if link.dropped.Load() == 0 {
		t.Error("the link dropped nothing; the loss half of this test proves nothing")
	}
	st := r.Stats().Broadcasts[r.ObfuscateID(id)]
	if st.DVRResyncs != 0 {
		t.Errorf("dvrResyncs = %d, want 0", st.DVRResyncs)
	}
	var legRows int
	for _, d := range st.SubscriberDetails {
		if d.StripeLeg && d.DVR && d.StripeN == n && d.StripeOwner != "" {
			legRows++
		}
	}
	if legRows != n-1 {
		t.Errorf("/statusz shows %d DVR leg rows, want %d", legRows, n-1)
	}
}

// TestStripedDVRLegOnAnotherPod (docs/91 D2, docs/35 §5.7): a leg that lands
// on another pod than its primary serves from that pod's own ring, and the
// keyframe frameID alone keeps the split consistent: between the primary on
// the origin and the leg on an edge, every GOP is served exactly once.
// Keyframes only: the origin→edge hop carries deltas as datagrams, so a
// record lost there is the edge pull's loss, not the stripe's.
func TestStripedDVRLegOnAnotherPod(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cs := fake.NewClientset()
	cert, err := tlsutil.GenerateDevCert([]string{"localhost", "127.0.0.1"}, time.Hour)
	if err != nil {
		t.Fatalf("GenerateDevCert: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert.Leaf)
	clientTLS := &tls.Config{RootCAs: pool, ServerName: "localhost", NextProtos: []string{"h3"}}
	striped := func(c *config.Config) { c.StripedDelivery, c.StripedDVR = true, true }
	podA := startClusteredPod(t, ctx, cs, "pod-a", cert, pool, 0, striped)
	podB := startClusteredPod(t, ctx, cs, "pod-b", cert, pool, 0, striped)

	pub := dial(t, ctx, fmt.Sprintf("https://%s/publish", podA.addr()), clientTLS)
	defer pub.CloseWithError(0, "")
	id, _ := readPublisherHandshake(t, ctx, pub)

	const owner, n = "aabbccdd00112233", 2
	q := "?delivery=reliable&buffer=2500&owner=" + owner
	primary := dial(t, ctx, fmt.Sprintf("https://%s/subscribe/%s%s", podA.addr(), id, q), clientTLS)
	defer primary.CloseWithError(0, "")
	leg := dial(t, ctx, fmt.Sprintf("https://%s/subscribe/%s%s&stripe=%d&leg=1", podB.addr(), id, q, n), clientTLS)
	defer leg.CloseWithError(0, "")
	waitForCascade(t, func() (bool, string) {
		stB := podB.registry.Stats().Broadcasts[podB.registry.ObfuscateID(id)]
		return stB.Role == "edge" && stB.StripeLegs == 1, fmt.Sprintf("B: role=%s legs=%d", stB.Role, stB.StripeLegs)
	}, "the DVR leg attached on the edge pod")

	state, err := wire.AppendStripeState(nil, wire.StripeState{Striped: true, StripeN: n})
	if err != nil {
		t.Fatalf("AppendStripeState: %v", err)
	}
	waitFor(t, 10*time.Second, func() bool {
		_ = primary.SendDatagram(state)
		return podA.registry.Stats().Totals.StripedPrimaries == 1
	}, "the DVR primary striped on the origin")
	// The 1 Hz refresh on the primary and the leg's heartbeat (its lease).
	refreshCtx, refreshStop := context.WithCancel(ctx)
	defer refreshStop()
	go func() {
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-refreshCtx.Done():
				return
			case <-tick.C:
				_ = primary.SendDatagram(state)
				_ = leg.SendDatagram(state)
			}
		}
	}()

	deadline := time.Now().Add(40 * time.Second)
	readCtx, readCancel := context.WithDeadline(ctx, deadline)
	defer readCancel()
	sinks := []*gopSink{{}, {}}
	go sinks[0].read(readCtx, primary, deadline)
	go sinks[1].read(readCtx, leg, deadline)

	const gops, gopLen = 20, 30
	for k := range gops {
		kf := uint32(1 + gopLen*k)
		sendKeyframeStream(t, pub, buildStreamKeyframe(t, kf, "avc1.42E02A", 1024))
		time.Sleep(60 * time.Millisecond)
		_ = pub.SendDatagram(encodeFrame(t, kf+1, false, 1)[0])
		time.Sleep(40 * time.Millisecond)
	}
	if !waitUntil(15*time.Second, func() bool {
		a, _, _ := sinks[0].snapshot()
		b, _, _ := sinks[1].snapshot()
		return len(a)+len(b) >= gops
	}) {
		t.Log("gave up waiting for every keyframe")
	}

	servedBy := map[uint32]int{}
	for j, s := range sinks {
		keyframes, _, errs := s.snapshot()
		for _, err := range errs {
			t.Errorf("connection %d: %v", j, err)
		}
		for _, kf := range keyframes {
			if prev, dup := servedBy[kf]; dup {
				t.Errorf("GOP %d served by connection %d and %d", kf, prev, j)
			}
			servedBy[kf] = j
			if want := wire.StripeGOPResidue(kf, n); want != j {
				t.Errorf("GOP %d on connection %d (pod %s), its residue is %d", kf, j, []string{"a", "b"}[j], want)
			}
		}
	}
	for k := range gops {
		if _, ok := servedBy[uint32(1+gopLen*k)]; !ok {
			t.Errorf("GOP %d served by no connection", 1+gopLen*k)
		}
	}
	if a, _, _ := sinks[1].snapshot(); len(a) == 0 {
		t.Error("the leg on the edge pod served no GOP")
	}
}

// TestStripedDVRLegDrainOverQUIC: an unstriped StripeState on a DVR leg
// session drains it. The leg serves nothing newer than the ring's newest GOP
// when the drain arrived, and then the relay closes the leg session, while
// the primary carries on.
func TestStripedDVRLegDrainOverQUIC(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	port, clientTLS, r, _ := startTestServerCfg(t, ctx, stripedDVRTestConfig(15))
	pub, id := dialPublisherAndGetID(t, ctx, port, clientTLS)
	defer pub.CloseWithError(0, "")

	const owner = "aabbccdd00112233"
	base := fmt.Sprintf("https://127.0.0.1:%d/subscribe/%s?delivery=reliable&buffer=2500&owner=%s", port, id, owner)
	primary := dial(t, ctx, base, clientTLS)
	defer primary.CloseWithError(0, "")
	leg := dial(t, ctx, base+"&stripe=2&leg=1", clientTLS)
	defer leg.CloseWithError(0, "")
	waitFor(t, 10*time.Second, func() bool { return r.Stats().Totals.Subscribers == 2 }, "the primary and its leg registered")

	deadline := time.Now().Add(40 * time.Second)
	readCtx, readCancel := context.WithDeadline(ctx, deadline)
	defer readCancel()
	sink := &gopSink{}
	go sink.read(readCtx, leg, deadline)

	const gopLen, before, after = 30, 8, 8
	publish := func(k int) {
		kf := uint32(1 + gopLen*k)
		sendKeyframeStream(t, pub, buildStreamKeyframe(t, kf, "avc1.42E02A", 1024))
		time.Sleep(60 * time.Millisecond)
		_ = pub.SendDatagram(encodeFrame(t, kf+1, false, 1)[0])
		time.Sleep(20 * time.Millisecond)
	}
	for k := range before {
		publish(k)
	}
	drain, err := wire.AppendStripeState(nil, wire.StripeState{})
	if err != nil {
		t.Fatalf("AppendStripeState: %v", err)
	}
	waitFor(t, 10*time.Second, func() bool {
		_ = leg.SendDatagram(drain)
		for _, d := range r.Stats().Broadcasts[r.ObfuscateID(id)].SubscriberDetails {
			if d.StripeLeg && d.StripeDraining {
				return true
			}
		}
		return false
	}, "the leg draining on /statusz")
	for k := before; k < before+after; k++ {
		publish(k)
	}
	select {
	case <-leg.Context().Done():
	case <-time.After(15 * time.Second):
		t.Fatal("the relay never closed the drained leg")
	}
	select {
	case <-primary.Context().Done():
		t.Fatal("draining the leg closed the primary")
	default:
	}
	keyframes, _, _ := sink.snapshot()
	for _, kf := range keyframes {
		if kf > uint32(1+gopLen*(before-1)) {
			t.Errorf("the drained leg served GOP %d, newer than the drain point %d", kf, 1+gopLen*(before-1))
		}
	}
	if len(keyframes) == 0 {
		t.Error("the leg served nothing before its drain; the test proves nothing")
	}
}
