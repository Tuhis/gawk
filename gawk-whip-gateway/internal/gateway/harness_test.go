package gateway

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/publisher"
	"github.com/Tuhis/gawk/gawk-server/wire"

	"github.com/Tuhis/gawk/gawk-whip-gateway/internal/media"
	"github.com/Tuhis/gawk/gawk-whip-gateway/internal/status"
	"github.com/Tuhis/gawk/gawk-whip-gateway/internal/streamkey"
	"github.com/Tuhis/gawk/gawk-whip-gateway/internal/testmedia"
)

// ---------------------------------------------------------------------------
// The fake relay: a publisher.DialFunc seam that behaves like the relay's
// publish route for D8's rows. One current publisher per broadcast ID; a new
// reclaim closes the old one with 4004 ("newest publisher wins"); the
// engine's own close code (0 or 4014) is recorded per ID.

type closeRecord struct {
	id   string
	code webtransport.SessionErrorCode
}

type fakeRelay struct {
	mu       sync.Mutex
	dials    []string
	refuse   map[string]int // broadcast ID → HTTP status to answer
	current  map[string]*fakeRelaySession
	closes   []closeRecord
	sessions []*fakeRelaySession
}

func newFakeRelay() *fakeRelay {
	return &fakeRelay{refuse: map[string]int{}, current: map[string]*fakeRelaySession{}}
}

func (f *fakeRelay) dial(ctx context.Context, rawURL, origin string, insecure bool) (publisher.RelaySession, int, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, 0, err
	}
	id := strings.TrimPrefix(u.Path, "/publish/")
	f.mu.Lock()
	f.dials = append(f.dials, rawURL)
	if st := f.refuse[id]; st != 0 {
		f.mu.Unlock()
		return nil, st, fmt.Errorf("relay refused with %d", st)
	}
	s := newFakeRelaySession(f, id)
	old := f.current[id]
	f.current[id] = s
	f.sessions = append(f.sessions, s)
	f.mu.Unlock()
	if old != nil {
		old.remoteClose(wire.CloseCodePublisherSuperseded)
	}
	return s, http.StatusOK, nil
}

func (f *fakeRelay) setRefuse(id string, st int) {
	f.mu.Lock()
	f.refuse[id] = st
	f.mu.Unlock()
}

func (f *fakeRelay) dialCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.dials)
}

func (f *fakeRelay) lastDial() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.dials) == 0 {
		return ""
	}
	return f.dials[len(f.dials)-1]
}

func (f *fakeRelay) cur(id string) *fakeRelaySession {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.current[id]
}

// closeCodes returns the close codes the engine sent for id, in order.
func (f *fakeRelay) closeCodes(id string) []webtransport.SessionErrorCode {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []webtransport.SessionErrorCode
	for _, c := range f.closes {
		if c.id == id {
			out = append(out, c.code)
		}
	}
	return out
}

type fakeRelaySession struct {
	relay *fakeRelay
	id    string

	ctx    context.Context
	cancel context.CancelFunc
	dgrams chan []byte

	mu         sync.Mutex
	remoteCode *webtransport.SessionErrorCode
	sent       int
	keyframes  int
	closed     bool
}

func newFakeRelaySession(f *fakeRelay, id string) *fakeRelaySession {
	ctx, cancel := context.WithCancel(context.Background())
	return &fakeRelaySession{relay: f, id: id, ctx: ctx, cancel: cancel, dgrams: make(chan []byte, 16)}
}

func (s *fakeRelaySession) SendDatagram(b []byte) error {
	if s.ctx.Err() != nil {
		return errors.New("closed")
	}
	s.mu.Lock()
	s.sent++
	s.mu.Unlock()
	return nil
}

func (s *fakeRelaySession) ReceiveDatagram(ctx context.Context) ([]byte, error) {
	select {
	case d := <-s.dgrams:
		return d, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.ctx.Done():
		return nil, s.ctx.Err()
	}
}

type discardStream struct{}

func (discardStream) Write(b []byte) (int, error)              { return len(b), nil }
func (discardStream) Close() error                             { return nil }
func (discardStream) CancelWrite(webtransport.StreamErrorCode) {}

func (s *fakeRelaySession) OpenUniStream() (publisher.SendStream, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.remoteCode != nil {
		return nil, &webtransport.SessionError{Remote: true, ErrorCode: *s.remoteCode}
	}
	if s.ctx.Err() != nil {
		return nil, errors.New("session gone")
	}
	s.keyframes++
	return discardStream{}, nil
}

func (s *fakeRelaySession) AcceptUniStream(ctx context.Context) (publisher.ReceiveStream, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.ctx.Done():
		return nil, s.ctx.Err()
	}
}

func (s *fakeRelaySession) CloseWithError(code webtransport.SessionErrorCode, _ string) error {
	s.mu.Lock()
	already := s.closed
	s.closed = true
	s.mu.Unlock()
	if !already {
		s.relay.mu.Lock()
		s.relay.closes = append(s.relay.closes, closeRecord{id: s.id, code: code})
		if s.relay.current[s.id] == s {
			delete(s.relay.current, s.id)
		}
		s.relay.mu.Unlock()
	}
	s.cancel()
	return nil
}

func (s *fakeRelaySession) Context() context.Context { return s.ctx }

// remoteClose is the relay closing the publisher with code.
func (s *fakeRelaySession) remoteClose(code uint32) {
	c := webtransport.SessionErrorCode(code)
	s.mu.Lock()
	s.remoteCode = &c
	s.closed = true
	s.mu.Unlock()
	s.cancel()
}

// drop is the relay going away without a code (a drain's abrupt end, or a
// dead pod): the engine auto-resumes.
func (s *fakeRelaySession) drop() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.relay.mu.Lock()
	if s.relay.current[s.id] == s {
		delete(s.relay.current, s.id)
	}
	s.relay.mu.Unlock()
	s.cancel()
}

func (s *fakeRelaySession) counts() (dgrams, keyframes int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sent, s.keyframes
}

// ---------------------------------------------------------------------------
// Gateway instances.

type gw struct {
	t      *testing.T
	srv    *Server
	public *httptest.Server
	intl   *httptest.Server
	relay  *fakeRelay
	plis   *pliLog
	clock  *testClock
}

type pliLog struct {
	mu  sync.Mutex
	got []string
}

func (p *pliLog) add(_, trigger string) {
	p.mu.Lock()
	p.got = append(p.got, trigger)
	p.mu.Unlock()
}

func (p *pliLog) list() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.got...)
}

// testClock is the records' wall clock: real time plus an offset tests
// advance, so backoffs and record lifetimes pass without sleeping while
// pion and the engine keep real time.
type testClock struct{ off atomic.Int64 }

func (c *testClock) now() time.Time          { return time.Now().Add(time.Duration(c.off.Load())) }
func (c *testClock) advance(d time.Duration) { c.off.Add(int64(d)) }

const testToken = "internal-token-for-tests"

func testLogger(t *testing.T) *slog.Logger {
	if os.Getenv("GAWK_WHIP_TEST_LOG") != "" {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return slog.New(slog.DiscardHandler)
}

func baseOptions(t *testing.T, relay *fakeRelay) Options {
	reorder := media.DefaultReorderConfig()
	return Options{
		Relay:                  RelayOptions{URL: "https://relay.test:4433", PublishSecret: "s3cret", PublishProxyToken: "proxy-token"},
		MediaBind:              "127.0.0.1",
		PublicAddress:          "127.0.0.1",
		IncludeLoopback:        true,
		ICEDisconnectedTimeout: 500 * time.Millisecond,
		ICEFailedTimeout:       time.Second,
		ICEKeepalive:           200 * time.Millisecond,
		MaxSessions:            20,
		MaxSessionsHard:        25,
		FullRetryAfter:         30 * time.Second,
		RelayBusyRetryAfter:    10 * time.Second, // distinct from FullRetryAfter, so a test can tell them apart
		Reorder:                reorder,
		GOP:                    media.DefaultGOPConfig(),
		Backoff:                status.BackoffPolicy{Initial: 30 * time.Second, Max: 10 * time.Minute},
		RecordTTL:              10 * time.Minute,
		MaxRecords:             4096,
		PeerTimeout:            200 * time.Millisecond,
		Log:                    testLogger(t),
		Dial:                   relay.dial,
	}
}

func startGW(t *testing.T, relay *fakeRelay, mutate func(*Options)) *gw {
	t.Helper()
	g := &gw{t: t, relay: relay, plis: &pliLog{}, clock: &testClock{}}
	o := baseOptions(t, relay)
	o.Now = g.clock.now
	o.Hooks.PLI = g.plis.add
	if mutate != nil {
		mutate(&o)
	}
	srv, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	g.srv = srv
	g.public = httptest.NewServer(srv.PublicHandler())
	g.intl = httptest.NewServer(srv.InternalHandler())
	t.Cleanup(func() {
		g.public.Close()
		g.intl.Close()
		srv.Close()
	})
	return g
}

// pair starts two gateways that are each other's peers (D10).
func pair(t *testing.T, relay *fakeRelay, mutate func(*Options)) (*gw, *gw) {
	t.Helper()
	// Peers are wired after both internal listeners exist.
	a := startGW(t, relay, func(o *Options) {
		o.InternalToken = testToken
		o.Peers = []string{"http://peer-b.invalid"}
		if mutate != nil {
			mutate(o)
		}
	})
	b := startGW(t, relay, func(o *Options) {
		o.InternalToken = testToken
		o.Peers = []string{"http://peer-a.invalid"}
		if mutate != nil {
			mutate(o)
		}
	})
	a.srv.peers = newPeerClient([]string{b.intl.URL}, testToken, a.srv.opts.PeerTimeout, a.srv.m)
	b.srv.peers = newPeerClient([]string{a.intl.URL}, testToken, b.srv.opts.PeerTimeout, b.srv.m)
	return a, b
}

// ---------------------------------------------------------------------------
// Keys.

var keySeq atomic.Uint32

// newKey makes a well-formed key with a fresh ID.
func newKey(t *testing.T) streamkey.Key {
	n := keySeq.Add(1)
	const alpha = "23456789ABCDEFGHJKMNPQRSTUVWXYZ"
	id := []byte("Q22222")
	for i := 5; i >= 1 && n > 0; i-- {
		id[i] = alpha[n%31]
		n /= 31
	}
	tok := make([]byte, 16)
	tok[0], tok[15] = byte(n), byte(keySeq.Load())
	k, err := streamkey.Parse("gawk1_" + string(id) + "_" + hex.EncodeToString(tok))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// otherTokenKey is the same ID with a different token.
func otherTokenKey(t *testing.T, k streamkey.Key) streamkey.Key {
	k2, err := streamkey.Parse("gawk1_" + k.ID + "_ffffffffffffffffffffffffffffffff")
	if err != nil {
		t.Fatal(err)
	}
	return k2
}

// ---------------------------------------------------------------------------
// HTTP helpers.

type resp struct {
	code int
	hdr  http.Header
	body string
}

func do(t *testing.T, method, rawURL, key, contentType, body string, extra http.Header) resp {
	t.Helper()
	req, err := http.NewRequest(method, rawURL, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range extra {
		req.Header[k] = v
	}
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return resp{code: r.StatusCode, hdr: r.Header, body: string(b)}
}

func (g *gw) status(key streamkey.Key) status.Status {
	g.t.Helper()
	return getStatus(g.t, g.public.URL, key)
}

func getStatus(t *testing.T, base string, key streamkey.Key) status.Status {
	t.Helper()
	r := do(t, http.MethodGet, base+"/whip/status", key.String(), "", "", nil)
	if r.code != http.StatusOK {
		t.Fatalf("GET /whip/status: %d %s", r.code, r.body)
	}
	return decodeStatus(t, r.body)
}

func decodeStatus(t *testing.T, body string) status.Status {
	t.Helper()
	var st status.Status
	if err := jsonUnmarshal(body, &st); err != nil {
		t.Fatalf("status JSON %q: %v", body, err)
	}
	return st
}

func eventually(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// ---------------------------------------------------------------------------
// The pion WHIP client: what OBS does, in process.

type clientOpts struct {
	videoMime string // default H.264
	fmtp      string
	noAudio   bool
}

type whipClient struct {
	t        *testing.T
	pc       *webrtc.PeerConnection
	video    *webrtc.TrackLocalStaticRTP
	audio    *webrtc.TrackLocalStaticRTP
	conn     *cuttableConn
	location string
	base     string
	key      string
	stream   testmedia.Stream
	aseq     uint16
	ats      uint32
	sps      []byte
	plis     atomic.Int32
	nacks    atomic.Int32
	sent     []*rtp.Packet
	sentMu   sync.Mutex
}

// cuttableConn lets a test make OBS vanish without a goodbye: writes are
// silently dropped once cut, exactly like a machine that lost its network.
type cuttableConn struct {
	net.PacketConn
	cut atomic.Bool
}

func (c *cuttableConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	if c.cut.Load() {
		return len(b), nil
	}
	return c.PacketConn.WriteTo(b, addr)
}

func newClientPC(t *testing.T, o clientOpts) (*whipClient, string) {
	t.Helper()
	me := &webrtc.MediaEngine{}
	mime := o.videoMime
	if mime == "" {
		mime = webrtc.MimeTypeH264
	}
	fmtp := o.fmtp
	if fmtp == "" && mime == webrtc.MimeTypeH264 {
		fmtp = "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f"
	}
	if err := me.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: mime, ClockRate: 90000, SDPFmtpLine: fmtp,
			RTCPFeedback: []webrtc.RTCPFeedback{{Type: "nack"}, {Type: "nack", Parameter: "pli"}}},
		PayloadType: 102,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		t.Fatal(err)
	}
	if err := me.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2, SDPFmtpLine: "minptime=10;useinbandfec=1"},
		PayloadType:        111,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		t.Fatal(err)
	}
	udp, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cc := &cuttableConn{PacketConn: udp}
	quiet := pionLogs{log: testLogger(t)}
	se := webrtc.SettingEngine{LoggerFactory: quiet}
	se.SetIncludeLoopbackCandidate(true)
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	se.SetICEUDPMux(webrtc.NewICEUDPMux(quiet.NewLogger("client"), cc))
	se.SetICETimeouts(time.Second, 2*time.Second, 200*time.Millisecond)
	api := webrtc.NewAPI(webrtc.WithMediaEngine(me), webrtc.WithSettingEngine(se))
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	c := &whipClient{t: t, pc: pc, conn: cc, stream: testmedia.Stream{FPS: 60, MTU: 1200}}
	c.sps = testmedia.SPS(testmedia.SPSOptions{Width: 1280, Height: 720})
	c.video, err = webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: mime, ClockRate: 90000, SDPFmtpLine: fmtp}, "video", "obs")
	if err != nil {
		t.Fatal(err)
	}
	vs, err := pc.AddTransceiverFromTrack(c.video, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			pkts, _, err := vs.Sender().ReadRTCP()
			if err != nil {
				return
			}
			for _, p := range pkts {
				switch p := p.(type) {
				case *rtcp.PictureLossIndication:
					c.plis.Add(1)
				case *rtcp.TransportLayerNack:
					for _, pair := range p.Nacks {
						c.nacks.Add(int32(len(pair.PacketList())))
					}
				}
			}
		}
	}()
	if !o.noAudio {
		c.audio, err = webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}, "audio", "obs")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pc.AddTransceiverFromTrack(c.audio, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly}); err != nil {
			t.Fatal(err)
		}
	}
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	<-gathered
	t.Cleanup(func() { _ = pc.Close(); _ = udp.Close() })
	return c, pc.LocalDescription().SDP
}

// post sends the offer; on 201 it applies the answer.
func (c *whipClient) post(base string, key streamkey.Key, offer string) resp {
	c.t.Helper()
	r := do(c.t, http.MethodPost, base+"/whip", key.String(), "application/sdp", offer, nil)
	c.base, c.key = base, key.String()
	if r.code == http.StatusCreated {
		c.location = r.hdr.Get("Location")
		if err := c.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: r.body}); err != nil {
			c.t.Fatalf("answer: %v\n%s", err, r.body)
		}
	}
	return r
}

func (c *whipClient) waitConnected() {
	c.t.Helper()
	eventually(c.t, "client connected", 10*time.Second, func() bool {
		return c.pc.ConnectionState() == webrtc.PeerConnectionStateConnected
	})
}

// connect is newClientPC + post + waitConnected for the common case.
func connect(t *testing.T, base string, key streamkey.Key) *whipClient {
	t.Helper()
	c, offer := newClientPC(t, clientOpts{})
	if r := c.post(base, key, offer); r.code != http.StatusCreated {
		t.Fatalf("POST: %d %s", r.code, r.body)
	}
	c.waitConnected()
	return c
}

func (c *whipClient) sendAU(a testmedia.AU) {
	c.t.Helper()
	for _, p := range c.stream.Packets(a) {
		c.sentMu.Lock()
		c.sent = append(c.sent, p)
		c.sentMu.Unlock()
		if err := c.video.WriteRTP(p); err != nil {
			c.t.Fatalf("WriteRTP: %v", err)
		}
	}
}

// sendGOP sends one IDR and n deltas, pacing lightly so the loopback
// doesn't drop.
func (c *whipClient) sendGOP(n int) {
	c.sendAU(testmedia.IDR(c.sps, 4000))
	for i := 0; i < n; i++ {
		c.sendAU(testmedia.P(1500, byte(i)))
		time.Sleep(time.Millisecond)
	}
}

func (c *whipClient) sendAudio(n int) {
	for i := 0; i < n; i++ {
		// A stereo, 20 ms CELT TOC (config 31, s=1, c=0): what OBS sends.
		pkt := &rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: c.aseq, Timestamp: c.ats}, Payload: []byte{0xfc, 1, 2, 3}}
		c.aseq++
		c.ats += 960
		_ = c.audio.WriteRTP(pkt)
	}
}

func (c *whipClient) delete() resp {
	return do(c.t, http.MethodDelete, c.base+c.location, c.key, "", "", nil)
}

func (c *whipClient) sessionID() string { return strings.TrimPrefix(c.location, "/whip/session/") }

func jsonUnmarshal(s string, v any) error { return json.Unmarshal([]byte(s), v) }

func sdpAnswer(s string) webrtc.SessionDescription {
	return webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: s}
}
