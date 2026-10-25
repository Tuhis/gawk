package transport

// R74 PP9 (docs/76 §6.6, item 23) at the transport level: a PoP relay whose
// downlink dials a home server's /pop/subscribe with the production leg
// dialer — the link's QUIC timers and R73's receive windows — through a
// test PacketConn that delays and can black out the PoP's packets.

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/quic-go/qlog"
	"github.com/quic-go/quic-go/qlogwriter"
	"github.com/quic-go/webtransport-go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/internal/metrics"
	"github.com/Tuhis/gawk/gawk-server/internal/poplink"
	"github.com/Tuhis/gawk/gawk-server/internal/tlsutil"
	"github.com/Tuhis/gawk/gawk-server/pops"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

const testPopCredential = "test-credential"

// legPopHome authenticates one PoP by its credential.
type legPopHome struct{ limits pops.Limits }

func (h legPopHome) Authenticate(r *http.Request) (*pops.Pop, int, error) {
	if poplink.BearerToken(r.Header.Get("Authorization")) != testPopCredential {
		return nil, http.StatusUnauthorized, pops.ErrCredentialNotCurrent
	}
	return &pops.Pop{ObjectMeta: metav1.ObjectMeta{Name: "party"}, Spec: pops.PopSpec{Limits: h.limits}}, http.StatusOK, nil
}

func (legPopHome) CheckPairing(*http.Request) (*pops.Pop, int, error) {
	return nil, http.StatusNotFound, errors.New("no pairing")
}

func (legPopHome) RedeemPairing(context.Context, *pops.Pop) (*pops.Pop, string, int, error) {
	return nil, "", http.StatusNotFound, errors.New("no pairing")
}

func (legPopHome) ServeLink(context.Context, *pops.Pop, poplink.Session, string) error { return nil }
func (legPopHome) RecordAddr(string, netip.Addr)                                       {}

// fakeDownlinkSource is a linked PoP's link, as popclient.Client reports it.
type fakeDownlinkSource struct {
	mu  sync.Mutex
	t   PopDownlinkTarget
	ok  bool
	got int
}

func (f *fakeDownlinkSource) PopDownlink() (PopDownlinkTarget, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got++
	return f.t, f.ok
}

// popConn is the PoP leg's PacketConn: a one-way delay on what the PoP
// sends, and an outage that drops everything both ways.
type popConn struct {
	net.PacketConn
	delay  time.Duration
	outage atomic.Bool
	out    chan delayed
}

func newPopConn(t *testing.T, delay time.Duration) *popConn {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	c := &popConn{PacketConn: pc, delay: delay, out: make(chan delayed, 1<<16)}
	go func() {
		for d := range c.out {
			time.Sleep(time.Until(d.due))
			if !c.outage.Load() {
				_, _ = c.PacketConn.WriteTo(d.b, d.addr)
			}
		}
	}()
	t.Cleanup(func() { pc.Close() })
	return c
}

func (c *popConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	if c.outage.Load() {
		return len(b), nil
	}
	if c.delay == 0 {
		return c.PacketConn.WriteTo(b, addr)
	}
	c.out <- delayed{due: time.Now().Add(c.delay), b: append([]byte(nil), b...), addr: addr}
	return len(b), nil
}

func (c *popConn) ReadFrom(b []byte) (int, net.Addr, error) {
	for {
		n, addr, err := c.PacketConn.ReadFrom(b)
		if err != nil || !c.outage.Load() {
			return n, addr, err
		}
	}
}

// legTrace is what the PoP's QUIC stack saw on its legs: the receive
// windows it advertised and the flow-control BLOCKED frames home sent.
type legTrace struct {
	streamWindow, connWindow atomic.Int64
	blocked                  atomic.Int64
}

func (l *legTrace) AddProducer() qlogwriter.Recorder { return legRecorder{l} }
func (*legTrace) SupportsSchemas(string) bool        { return false }

type legRecorder struct{ l *legTrace }

func (r legRecorder) RecordEvent(ev qlogwriter.Event) {
	switch e := ev.(type) {
	case qlog.ParametersSet:
		if e.Initiator == qlog.InitiatorLocal {
			r.l.streamWindow.Store(int64(e.InitialMaxStreamDataUni))
			r.l.connWindow.Store(int64(e.InitialMaxData))
		}
	case qlog.PacketReceived:
		for _, f := range e.Frames {
			switch f.Frame.(type) {
			case *qlog.DataBlockedFrame, *qlog.StreamDataBlockedFrame:
				r.l.blocked.Add(1)
			}
		}
	}
}
func (legRecorder) Close() error { return nil }

// popRig is a home server serving /pop/subscribe and a PoP relay whose
// downlink dials it through conn.
type popRig struct {
	homePort, popPort int
	homeTLS, popTLS   *tls.Config
	home, pop         *hub.Registry
	homeSrv, popSrv   *Server
	conn              *popConn
	trace             *legTrace
	src               *fakeDownlinkSource
}

func startPopRig(t *testing.T, ctx context.Context, popBitrate int64, delay time.Duration, carrier bool) *popRig {
	t.Helper()
	homeCfg := uplinkTestConfig(true)
	homeCfg.Pops = true
	homePort, homeTLS, home, _, homeSrv := startTestServerCfgLogSrv(t, ctx, homeCfg, discardLog)
	homeSrv.SetPopHome(legPopHome{})

	popCfg := uplinkTestConfig(true)
	popCfg.UplinkMaxBitrateBytes = popBitrate
	popPort, popTLS, pop, _, popSrv := startTestServerCfgLogSrv(t, ctx, popCfg, discardLog)
	rig := &popRig{homePort: homePort, popPort: popPort, homeTLS: homeTLS, popTLS: popTLS, home: home, pop: pop,
		homeSrv: homeSrv, popSrv: popSrv, conn: newPopConn(t, delay), trace: &legTrace{}}
	rig.src = &fakeDownlinkSource{ok: true, t: PopDownlinkTarget{
		Endpoint: fmt.Sprintf("127.0.0.1:%d", homePort), Credential: testPopCredential, Pop: "party",
		Carrier: carrier, RootCAs: homeTLS.RootCAs,
	}}
	popSrv.SetPopDownlink(rig.src)
	hook := func(tr *webtransport.Transport) {
		tr.QUICConfig.Tracer = func(context.Context, bool, quic.ConnectionID) qlogwriter.Trace { return rig.trace }
		qtr := &quic.Transport{Conn: rig.conn}
		tr.DialAddr = func(ctx context.Context, addr string, tlsCfg *tls.Config, cfg *quic.Config) (*quic.Conn, error) {
			ua, err := net.ResolveUDPAddr("udp", addr)
			if err != nil {
				return nil, err
			}
			return qtr.DialEarly(ctx, ua, tlsCfg, cfg)
		}
	}
	popSrv.testHookPopLegTransport.Store(&hook)
	return rig
}

// popGop is what crossed one leg for a 1440p GOP.
type popGop struct {
	carrierBytes             int
	streamWindow, connWindow int64
	blocked                  int64
}

// sendPopGop publishes on home, joins a party viewer on the PoP, then
// writes one 1440p GOP of deltas on the publisher's carrier, paced at the
// stream's 50 Mbps ceiling, and waits for all of it at the PoP.
func sendPopGop(t *testing.T, popBitrate int64) popGop {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rig := startPopRig(t, ctx, popBitrate, 40*time.Millisecond, true)
	pub, id := dialPublisherAndGetID(t, ctx, rig.homePort, rig.homeTLS)
	defer pub.CloseWithError(0, "")
	sendKeyframeStream(t, pub, buildStreamKeyframe(t, 0, "avc1.42E02A", 200_000))
	viewer := dialSubscriber(t, ctx, rig.popPort, id, rig.popTLS)
	defer viewer.CloseWithError(0, "")
	obf := rig.pop.ObfuscateID(id)
	waitFor(t, 10*time.Second, func() bool { return rig.pop.Stats().Broadcasts[obf].KeyframeStreamsIn > 0 }, "the PoP primed")

	n := gop1440pBytes / wire.MaxDatagramSize
	recs := uplinkRecords(t, 1, n)
	carrier := carrierStreamBytes(t, recs)
	car, err := pub.OpenUniStream()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := car.Write(wire.AppendCarrierPrologue(nil)); err != nil {
		t.Fatal(err)
	}
	// 50 Mbps: a 1200-byte record every 192 µs; 26 every 5 ms.
	const batch = 26
	for i := 0; i < n; i += batch {
		var b []byte
		for _, r := range recs[i:min(i+batch, n)] {
			b, _ = wire.AppendCarrierRecord(b, r)
		}
		if _, err := car.Write(b); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := car.Close(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 20*time.Second, func() bool {
		return rig.pop.Stats().Broadcasts[obf].UplinkRecords == uint64(n)
	}, "the whole GOP at the PoP, on the leg's carrier")
	return popGop{
		carrierBytes: len(carrier),
		streamWindow: rig.trace.streamWindow.Load(),
		connWindow:   rig.trace.connWindow.Load(),
		blocked:      rig.trace.blocked.Load(),
	}
}

// docs/76 item 23, PP9: the PoP's leg dialer sizes its receive windows
// with R73's function, so a 1440p home stream's GOP crosses a 40 ms RTT on
// one carrier without home's sender ever blocking on flow control. With
// quic-go's default windows the same GOP does not fit (asserted, as in
// TestUplinkFirstGopIsNotFlowControlBlocked; the blocking is only logged).
func TestPopLegCarriesA1440pGopWithoutFlowControlBlocking(t *testing.T) {
	got := sendPopGop(t, 50_000_000/8)
	if got.streamWindow != 2_000_000 || got.connWindow != 3_000_000 {
		t.Fatalf("the PoP advertised %d / %d, want D10's 2000000 / 3000000", got.streamWindow, got.connWindow)
	}
	if int64(got.carrierBytes) > got.streamWindow {
		t.Fatalf("a %d-byte GOP exceeds the %d-byte window", got.carrierBytes, got.streamWindow)
	}
	if got.blocked != 0 {
		t.Fatalf("home sent %d BLOCKED frames on the leg, want 0", got.blocked)
	}

	ctl := sendPopGop(t, 0)
	if int64(ctl.carrierBytes) <= ctl.streamWindow {
		t.Fatalf("control: the %d-byte GOP fits quic-go's default %d-byte window", ctl.carrierBytes, ctl.streamWindow)
	}
	t.Logf("control: quic-go's default windows (%d / %d) saw %d BLOCKED frames", ctl.streamWindow, ctl.connWindow, ctl.blocked)
}

// docs/76 item 23, PP9: a 10 s outage on the leg's path does not end it —
// the link's 15 s idle timeout, not the fleet edge's 4 s — so the PoP keeps
// the one leg and its prime, and the stream flows again on it.
func TestPopLegSurvivesA10sOutage(t *testing.T) {
	if testing.Short() {
		t.Skip("holds a 10 s outage")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rig := startPopRig(t, ctx, 50_000_000/8, 0, true)
	pub, id := dialPublisherAndGetID(t, ctx, rig.homePort, rig.homeTLS)
	defer pub.CloseWithError(0, "")
	sendKeyframeStream(t, pub, buildStreamKeyframe(t, 0, "avc1.42E02A", 2000))
	viewer := dialSubscriber(t, ctx, rig.popPort, id, rig.popTLS)
	defer viewer.CloseWithError(0, "")
	obf := rig.pop.ObfuscateID(id)
	waitFor(t, 10*time.Second, func() bool { return rig.pop.Stats().Broadcasts[obf].KeyframeStreamsIn > 0 }, "the PoP primed")

	rig.conn.outage.Store(true)
	time.Sleep(10 * time.Second)
	rig.conn.outage.Store(false)

	// The prime is still there: an invalidated one (a pump that ended)
	// leaves CachedKeyframeBytes at 0 until a new leg re-primes it.
	if st := rig.pop.Stats().Broadcasts[obf]; st.CachedKeyframeBytes == 0 {
		t.Fatalf("the PoP's prime did not survive the outage: %+v", st)
	}
	for i := uint32(1); ; i++ {
		for _, d := range encodeFrame(t, i, false, 1) {
			_ = pub.SendDatagram(d)
		}
		if rig.pop.Stats().Broadcasts[obf].FramesRelayed > 0 {
			break
		}
		if i > 200 {
			t.Fatal("no media reached the PoP after the outage")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if use := rig.popSrv.PopDownlinkUse(); use.LegsOpened != 1 || use.Streams != 1 {
		t.Fatalf("downlink use = %+v, want the one leg to have survived", use)
	}
}

// A broadcast home lacks is a 404 at the PoP and ends the pull; an
// unlinked PoP answers a miss 404 without dialing.
func TestPopDownlinkRefusalsReachTheViewer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rig := startPopRig(t, ctx, 0, 0, true)
	url := fmt.Sprintf("https://127.0.0.1:%d/subscribe/ZZZZZZ", rig.popPort)
	rsp, _, err := dialOnce(t, ctx, url, rig.popTLS)
	if err == nil || rsp == nil || rsp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown at home: %v %v, want 404", rsp, err)
	}
	waitFor(t, 5*time.Second, func() bool { return rig.popSrv.PopDownlinkUse().Streams == 0 }, "the refused pull ended")

	rig.src.mu.Lock()
	rig.src.ok = false
	rig.src.mu.Unlock()
	pub, id := dialPublisherAndGetID(t, ctx, rig.homePort, rig.homeTLS)
	defer pub.CloseWithError(0, "")
	rsp, _, err = dialOnce(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/subscribe/%s", rig.popPort, id), rig.popTLS)
	if err == nil || rsp == nil || rsp.StatusCode != http.StatusNotFound {
		t.Fatalf("unlinked: %v %v, want 404", rsp, err)
	}
	if got := rig.popSrv.PopDownlinkUse().LegsOpened; got != 0 {
		t.Fatalf("legs opened = %d, want none", got)
	}
}

func TestPopDialRefusal(t *testing.T) {
	for _, tc := range []struct {
		err   error
		want  error
		final bool
	}{
		{&PopLegStatusError{Status: 404}, hub.ErrNotFound, true},
		{&PopLegStatusError{Status: 401}, hub.ErrNotFound, true},
		{&PopLegStatusError{Status: 403}, hub.ErrNotFound, true},
		{&PopLegStatusError{Status: 426}, hub.ErrNotFound, true},
		{&PopLegStatusError{Status: 501}, hub.ErrNotFound, true},
		{&PopLegStatusError{Status: 429}, ErrPopDownlinkFull, true},
		{&PopLegStatusError{Status: 503}, nil, false},
		{&PopLegStatusError{Status: 502}, nil, false},
		{errors.New("timeout"), nil, false},
		{fmt.Errorf("wrapped: %w", &PopLegStatusError{Status: 404}), hub.ErrNotFound, true},
	} {
		got, final := popDialRefusal(tc.err)
		if final != tc.final || !errors.Is(got, tc.want) && !(got == nil && tc.want == nil) {
			t.Errorf("popDialRefusal(%v) = %v, %v; want %v, %v", tc.err, got, final, tc.want, tc.final)
		}
	}
}

func TestRateMeter(t *testing.T) {
	now := time.Unix(1000, 0)
	m := newRateMeter(func() time.Time { return now })
	m.add(1_250_000) // 10 Mbit in one second
	if got := m.mbps(); got != 1 {
		t.Fatalf("mbps = %v, want 1 (10 Mbit over the 10 s window)", got)
	}
	for range 9 {
		now = now.Add(time.Second)
		m.add(1_250_000)
	}
	if got := m.mbps(); got != 10 {
		t.Fatalf("mbps = %v, want 10", got)
	}
	now = now.Add(10 * time.Second)
	if got := m.mbps(); got != 0 {
		t.Fatalf("mbps after the window = %v, want 0", got)
	}
}

// The downlink budget admits by stream count and measured rate; zero is
// unlimited; unlinked admits nothing.
func TestPopDownlinkAdmit(t *testing.T) {
	src := &fakeDownlinkSource{ok: true}
	s := &Server{}
	s.SetPopDownlink(src)
	now := time.Unix(1000, 0)
	d := &popDownlink{s: s, meter: newRateMeter(func() time.Time { return now })}
	if err := d.admit("A", 100); err != nil {
		t.Fatalf("unlimited: %v", err)
	}
	src.t.Budget = PopBudget{MaxStreams: 2}
	if err := d.admit("A", 1); err != nil {
		t.Fatalf("1 of 2: %v", err)
	}
	if err := d.admit("A", 2); !errors.Is(err, ErrPopDownlinkFull) {
		t.Fatalf("2 of 2: %v, want ErrPopDownlinkFull", err)
	}
	src.t.Budget = PopBudget{MaxMbps: 8}
	d.meter.add(10_000_000) // 8 Mbps over the window
	if err := d.admit("A", 0); !errors.Is(err, ErrPopDownlinkFull) {
		t.Fatalf("at 8 of 8 Mbps: %v, want ErrPopDownlinkFull", err)
	}
	src.ok = false
	if err := d.admit("A", 0); !errors.Is(err, hub.ErrNotFound) {
		t.Fatalf("unlinked: %v, want ErrNotFound", err)
	}
}

// A relay's LocalRelayProvider that is also a PopDownlinkSource (as
// popclient.Client is) wires the downlink without SetPopDownlink.
func TestPopDownlinkSourceFromTheLocalRelay(t *testing.T) {
	s := &Server{}
	if s.popDownlinkSource() != nil {
		t.Fatal("a plain server has a downlink source")
	}
	s.SetLocalRelay(relayWithDownlink{})
	if s.popDownlinkSource() == nil {
		t.Fatal("the local relay's downlink source was not picked up")
	}
	s.SetLocalRelay(relayOnly{})
	if s.popDownlinkSource() != nil {
		t.Fatal("a provider without PopDownlink became a source")
	}
}

type relayOnly struct{}

func (relayOnly) LocalRelayInfo(string) wire.LocalRelayInfo { return wire.LocalRelayInfo{} }

type relayWithDownlink struct{ relayOnly }

func (relayWithDownlink) PopDownlink() (PopDownlinkTarget, bool) { return PopDownlinkTarget{}, false }

// garbageHome is a "home" whose /pop/subscribe opens one uni stream that is
// neither a keyframe stream nor a carrier and keeps writing to it, never
// finishing it. reset receives the error its write ends with.
func garbageHome(t *testing.T, ctx context.Context) (port int, clientTLS *tls.Config, reset <-chan error) {
	t.Helper()
	cert, err := tlsutil.GenerateDevCert([]string{"localhost", "127.0.0.1"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port = pc.LocalAddr().(*net.UDPAddr).Port
	pc.Close()
	cfg := uplinkTestConfig(true)
	cfg.Addr = fmt.Sprintf("127.0.0.1:%d", port)
	srv := New(cfg, hub.NewRegistry(discardLog, hub.Options{}), func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &cert, nil },
		discardLog, metrics.NewServerMetrics(prometheus.NewRegistry()))
	resetCh := make(chan error, 1)
	inner := srv.wt.H3.Handler
	srv.wt.H3.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/pop/subscribe/") {
			inner.ServeHTTP(w, r)
			return
		}
		sess, err := srv.wt.Upgrade(w, r)
		if err != nil {
			return
		}
		st, err := sess.OpenUniStream()
		if err != nil {
			return
		}
		junk := bytes.Repeat([]byte{0xEE}, 512)
		for {
			if _, err := st.Write(junk); err != nil {
				resetCh <- err
				<-sess.Context().Done()
				return
			}
			select {
			case <-sess.Context().Done():
				resetCh <- sess.Context().Err()
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	})
	go func() { _ = srv.Run(ctx) }()
	pool := x509.NewCertPool()
	pool.AddCert(cert.Leaf)
	return port, &tls.Config{RootCAs: pool, ServerName: "localhost", NextProtos: []string{http3.NextProtoH3}}, resetCh
}

// docs/76 item 23, PP9: a uni stream on the leg that is neither a keyframe
// stream nor a carrier is refused by the PoP's pump and reset (CancelRead:
// home's write ends with the PoP's stream error), never left unread
// holding home's flow-control credit.
func TestPopDownlinkResetsARefusedStream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rig := startPopRig(t, ctx, 0, 0, true)
	homePort, homeTLS, reset := garbageHome(t, ctx)
	rig.src.mu.Lock()
	rig.src.t.Endpoint = fmt.Sprintf("127.0.0.1:%d", homePort)
	rig.src.t.RootCAs = homeTLS.RootCAs
	rig.src.mu.Unlock()

	pullCtx, stop := context.WithTimeout(ctx, 3*time.Second)
	defer stop()
	go func() { _ = rig.popSrv.ensurePopDownlink(pullCtx, "K7XQ2M") }()
	select {
	case err := <-reset:
		var se *webtransport.StreamError
		if !errors.As(err, &se) || !se.Remote {
			t.Fatalf("home's write ended with %v, want the PoP's stream reset", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the PoP never reset the refused stream")
	}
	if got := rig.popSrv.PopDownlinkUse().LegsOpened; got != 1 {
		t.Fatalf("legs opened = %d, want 1", got)
	}
}

// R78 (docs/79): a home broadcaster's Stop reaches a party viewer as 4014,
// through the downlink leg, like any edge viewer: the viewer waits for the
// code to come back instead of being told the broadcast is over for good.
func TestPartyViewerOfAHomeBroadcastIsToldItStopped(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rig := startPopRig(t, ctx, 50_000_000/8, 0, true)
	pub, id, _ := dialStoppablePublisher(t, ctx, rig.homePort, rig.homeTLS)
	sendKeyframeStream(t, pub, buildStreamKeyframe(t, 0, "avc1.42E02A", 2000))
	viewer := dialSubscriber(t, ctx, rig.popPort, id, rig.popTLS)
	obf := rig.pop.ObfuscateID(id)
	waitFor(t, 10*time.Second, func() bool { return rig.pop.Stats().Broadcasts[obf].KeyframeStreamsIn > 0 }, "the PoP primed")

	pub.CloseWithError(webtransport.SessionErrorCode(wire.CloseCodeBroadcasterStopped), "broadcaster stopped")
	n, nAt, code, cAt := drainUntilClosed(t, ctx, viewer)
	assertNoticedClose(t, "party viewer", wire.CloseCodeBroadcasterStopped, n, nAt, code, cAt)
}
