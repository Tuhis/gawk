package bridge

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-mumble-bridge/internal/mumble"
	"github.com/Tuhis/gawk/gawk-server/voice"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// The harness plays the two parties the bridge talks to besides murmur: the
// relay (a WebTransport server for the session links, and a dialler for
// observer links) and the browser (a WebTransport voice client).

const (
	testPSK    = "test-psk-value"
	testSecret = "0123456789abcdef-ticket-secret"
	testOrigin = "https://gawk.test"
)

var (
	certOnce sync.Once
	testCert tls.Certificate
	testPool *x509.CertPool
)

func harnessCert(t testing.TB) (tls.Certificate, *x509.CertPool) {
	certOnce.Do(func() {
		c, _, err := NewDevCert([]string{"localhost", "127.0.0.1"})
		if err != nil {
			panic(err)
		}
		testCert = c
		testPool = x509.NewCertPool()
		testPool.AddCert(c.Leaf)
	})
	return testCert, testPool
}

func testLogger(t testing.TB) *slog.Logger {
	if os.Getenv("GAWK_TEST_VERBOSE") != "" {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeRelay serves /internal/voice/{code}.
type fakeRelay struct {
	t     testing.TB
	addr  string
	links chan *relaySide
	// onRegister, when set, answers each registration; default Accepted
	// for a temporary room.
	onRegister func(rs *relaySide)
	badPSK     int
}

// relaySide is the relay's end of one session link.
type relaySide struct {
	code string
	reg  voice.Message
	l    *link
	msgs chan voice.Message
}

func (rs *relaySide) send(t testing.TB, m voice.Message) {
	if err := rs.l.send(m); err != nil {
		t.Errorf("relay send %s: %v", m.Type, err)
	}
}

// next waits for the next message of type typ from the bridge.
func (rs *relaySide) next(t testing.TB, typ string) voice.Message {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case m, ok := <-rs.msgs:
			if !ok {
				t.Fatalf("session link closed waiting for %s", typ)
			}
			if m.Type == typ {
				return m
			}
		case <-deadline:
			t.Fatalf("no %s from the bridge", typ)
		}
	}
}

// waitClosed waits for the bridge to close the link, returning what it
// sent first.
func (rs *relaySide) waitClosed(t testing.TB) []voice.Message {
	t.Helper()
	var got []voice.Message
	deadline := time.After(5 * time.Second)
	for {
		select {
		case m, ok := <-rs.msgs:
			if !ok {
				return got
			}
			got = append(got, m)
		case <-deadline:
			t.Fatal("session link not closed")
		}
	}
}

func startRelay(t testing.TB) *fakeRelay {
	cert, _ := harnessCert(t)
	fr := &fakeRelay{t: t, links: make(chan *relaySide, 16)}
	mux := http.NewServeMux()
	wt := &webtransport.Server{
		H3: &http3.Server{
			TLSConfig:       http3.ConfigureTLSConfig(&tls.Config{Certificates: []tls.Certificate{cert}}),
			Handler:         mux,
			EnableDatagrams: true,
			QUICConfig:      &quic.Config{EnableDatagrams: true, EnableStreamResetPartialDelivery: true},
		},
		CheckOrigin: func(r *http.Request) bool { return r.Header.Get("Origin") == voice.LinkOrigin },
	}
	webtransport.ConfigureHTTP3Server(wt.H3)
	mux.HandleFunc("CONNECT "+voice.SessionPath+"{code}", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("psk") != testPSK {
			fr.badPSK++
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		sess, err := wt.Upgrade(w, r)
		if err != nil {
			return
		}
		str, err := sess.AcceptStream(context.Background())
		if err != nil {
			return
		}
		l := &link{sess: sess, str: str}
		reg, err := l.read()
		if err != nil {
			return
		}
		rs := &relaySide{code: r.PathValue("code"), reg: reg, l: l, msgs: make(chan voice.Message, 64)}
		go func() {
			defer close(rs.msgs)
			for {
				m, err := l.read()
				if err != nil {
					return
				}
				rs.msgs <- m
			}
		}()
		if fr.onRegister != nil {
			fr.onRegister(rs)
		} else {
			rs.send(t, voice.Message{Type: voice.TypeAccepted, Code: rs.code, DisplayCode: strings.ToUpper(rs.code), Temporary: true})
		}
		fr.links <- rs
		<-sess.Context().Done()
	})
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fr.addr = conn.LocalAddr().String()
	go func() { _ = wt.Serve(conn) }()
	t.Cleanup(func() { wt.Close(); conn.Close() })
	return fr
}

func (fr *fakeRelay) nextLink(t testing.TB) *relaySide {
	t.Helper()
	select {
	case rs := <-fr.links:
		return rs
	case <-time.After(5 * time.Second):
		t.Fatal("no session link reached the relay")
		return nil
	}
}

type testBridge struct {
	*Server
	public, internal, ops string
	cancel                context.CancelFunc
	done                  chan error
}

// baseOptions is a bridge on 127.0.0.1:0 talking to relay and murmur.
func baseOptions(t testing.TB, relayAddr, murmurAddr string) Options {
	_, pool := harnessCert(t)
	_ = pool
	return Options{
		Listen:         "127.0.0.1:0",
		InternalListen: "127.0.0.1:0",
		OpsListen:      "127.0.0.1:0",
		DevCert:        true,
		AllowedOrigins: []string{testOrigin},
		PSK:            testPSK,
		TicketSecret:   []byte(testSecret),
		Relay:          RelayOptions{URL: "https://" + relayAddr, ServerName: "127.0.0.1"},
		Mumble: MumbleOptions{
			Addr:                murmurAddr,
			TLS:                 mumble.TLSOptions{InsecureSkipVerify: true},
			ClientVersion:       mumble.DefaultVersion,
			NameTemplate:        "{nick} [web]",
			ObserverName:        "gawkBridgeBot",
			TempParent:          "Gawk",
			TempChannelTemplate: voice.DefaultTempChannelTemplate,
		},
		MaxVoiceSessions: 10,
		MaxTempChannels:  5,
		DrainTimeout:     5 * time.Second,
		StatsKey:         []byte("stats-key"),
		Version:          "test",
		Logger:           testLogger(t),
		Timings: Timings{
			HelloTimeout:       2 * time.Second,
			LinkTimeout:        3 * time.Second,
			NoticeSettle:       50 * time.Millisecond,
			TempChannelWait:    300 * time.Millisecond,
			RosterCoalesce:     10 * time.Millisecond,
			ObserverBackoff:    50 * time.Millisecond,
			ObserverBackoffMax: 200 * time.Millisecond,
			MumbleDial:         3 * time.Second,
			ChannelEnter:       3 * time.Second,
		},
	}
}

func startBridge(t testing.TB, opts Options) *testBridge {
	t.Helper()
	srv, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	// The dev cert is per server; trust the harness's instead so every
	// client verifies one root.
	cert, pool := harnessCert(t)
	srv.certs = staticCert{cert: &cert}
	srv.relayRoot = pool
	srv.linkTr.TLSClientConfig.RootCAs = pool
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	tb := &testBridge{Server: srv, cancel: cancel, done: make(chan error, 1)}
	tb.public, tb.internal, tb.ops = srv.Addrs()
	go func() { tb.done <- srv.Serve(ctx) }()
	deadline := time.Now().Add(3 * time.Second)
	for !srv.Ready() {
		if time.Now().After(deadline) {
			t.Fatal("bridge not ready")
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Cleanup(tb.stop)
	return tb
}

func (tb *testBridge) stop() {
	tb.cancel()
	select {
	case <-tb.done:
	case <-time.After(10 * time.Second):
	}
}

func (tb *testBridge) get(t testing.TB, path string) (int, string) {
	t.Helper()
	rsp, err := http.Get("http://" + tb.ops + path)
	if err != nil {
		t.Fatal(err)
	}
	defer rsp.Body.Close()
	b, _ := io.ReadAll(rsp.Body)
	return rsp.StatusCode, string(b)
}

func mintTicket(t testing.TB, code, nick string, expiry time.Time) []byte {
	t.Helper()
	n, _ := voice.NewNonce()
	raw, err := voice.MintTicket([]byte(testSecret), voice.Ticket{Code: code, Participant: 7, Nickname: nick, Nonce: n, Epoch: 1, Expiry: expiry})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// browser is a voice client.
type browser struct {
	t       testing.TB
	sess    *webtransport.Session
	str     *webtransport.Stream
	states  chan wire.VoiceState
	notices chan uint32
	frames  chan []byte
}

func dialBrowser(t testing.TB, addr, origin string) (*browser, error) {
	_, pool := harnessCert(t)
	tr := &webtransport.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "127.0.0.1"},
		QUICConfig:      &quic.Config{EnableDatagrams: true, EnableStreamResetPartialDelivery: true},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, sess, err := tr.Dial(ctx, "https://"+addr+VoicePath, http.Header{"Origin": []string{origin}})
	if err != nil {
		return nil, err
	}
	b := &browser{t: t, sess: sess, states: make(chan wire.VoiceState, 64), notices: make(chan uint32, 4), frames: make(chan []byte, 1024)}
	t.Cleanup(func() { _ = sess.CloseWithError(0, "") })
	go func() {
		for {
			u, err := sess.AcceptUniStream(context.Background())
			if err != nil {
				return
			}
			msg, _ := io.ReadAll(u)
			if code, err := wire.ParseSessionClosing(msg); err == nil {
				b.notices <- code
			}
		}
	}()
	go func() {
		for {
			d, err := sess.ReceiveDatagram(context.Background())
			if err != nil {
				return
			}
			b.frames <- d
		}
	}()
	return b, nil
}

// hello opens the stream and sends a VoiceHello.
func (b *browser) hello(ticket []byte, flags uint8) {
	b.t.Helper()
	str, err := b.sess.OpenStreamSync(context.Background())
	if err != nil {
		b.t.Fatal(err)
	}
	b.str = str
	msg, err := wire.AppendVoiceHello(nil, wire.VoiceHello{Protocol: wire.VoiceProtocolVersion, Flags: flags, Ticket: ticket})
	if err != nil {
		b.t.Fatal(err)
	}
	b.writeRaw(msg)
	go func() {
		for {
			rec, err := readRecord(str)
			if err != nil {
				return
			}
			if st, err := wire.ParseVoiceState(rec); err == nil {
				b.states <- st
			}
		}
	}()
}

func (b *browser) writeRaw(msg []byte) {
	rec, err := wire.AppendRoomRecord(nil, msg)
	if err != nil {
		b.t.Fatal(err)
	}
	if _, err := b.str.Write(rec); err != nil {
		b.t.Fatal(err)
	}
}

func (b *browser) control(flags uint8) {
	msg, _ := wire.AppendVoiceControl(nil, flags)
	b.writeRaw(msg)
}

func (b *browser) sendFrame(seq uint32, payload []byte, end bool) {
	d, err := wire.AppendVoiceFrame(nil, wire.VoiceFrameHeader{End: end, Seq: seq}, payload)
	if err != nil {
		b.t.Fatal(err)
	}
	_ = b.sess.SendDatagram(d)
}

// waitState waits for a VoiceState satisfying cond.
func (b *browser) waitState(cond func(wire.VoiceState) bool) wire.VoiceState {
	b.t.Helper()
	deadline := time.After(8 * time.Second)
	for {
		select {
		case st := <-b.states:
			if cond(st) {
				return st
			}
		case <-deadline:
			b.t.Fatal("no matching VoiceState")
			return wire.VoiceState{}
		}
	}
}

// closed waits for the session's end and returns the in-band notice code
// and the close code.
func (b *browser) closed() (notice uint32, code uint32) {
	b.t.Helper()
	select {
	case <-b.sess.Context().Done():
	case <-time.After(10 * time.Second):
		b.t.Fatal("voice session not closed")
	}
	select {
	case notice = <-b.notices:
	case <-time.After(200 * time.Millisecond):
	}
	_, err := b.sess.AcceptStream(context.Background())
	var se *webtransport.SessionError
	if errors.As(err, &se) {
		code = uint32(se.ErrorCode)
	}
	return notice, code
}

// expectClose asserts both the notice and the close carry code.
func (b *browser) expectClose(code uint32) {
	b.t.Helper()
	notice, got := b.closed()
	if notice != code || got != code {
		b.t.Fatalf("closed with notice %d code %d, want %d", notice, got, code)
	}
}

// observerLink is the relay's end of an observer link.
type observerLink struct {
	sess    *webtransport.Session
	l       *link
	rosters chan voice.Message
}

func dialObserver(t testing.TB, addr, psk string, m voice.Message) (*observerLink, *http.Response, error) {
	_, pool := harnessCert(t)
	tr := &webtransport.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "127.0.0.1"},
		QUICConfig:      &quic.Config{EnableDatagrams: true, EnableStreamResetPartialDelivery: true},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rsp, sess, err := tr.Dial(ctx, fmt.Sprintf("https://%s%s?psk=%s", addr, voice.ObservePath, psk), http.Header{"Origin": []string{voice.LinkOrigin}})
	if err != nil {
		return nil, rsp, err
	}
	str, err := sess.OpenStreamSync(ctx)
	if err != nil {
		return nil, rsp, err
	}
	ol := &observerLink{sess: sess, l: &link{sess: sess, str: str}, rosters: make(chan voice.Message, 64)}
	if err := ol.l.send(m); err != nil {
		return nil, rsp, err
	}
	go func() {
		defer close(ol.rosters)
		for {
			m, err := ol.l.read()
			if err != nil {
				return
			}
			ol.rosters <- m
		}
	}()
	t.Cleanup(func() { _ = sess.CloseWithError(0, "") })
	return ol, rsp, nil
}

// waitRoster waits for a Roster satisfying cond.
func (ol *observerLink) waitRoster(t testing.TB, cond func(voice.Message) bool) voice.Message {
	t.Helper()
	deadline := time.After(10 * time.Second)
	var last voice.Message
	for {
		select {
		case m, ok := <-ol.rosters:
			if !ok {
				t.Fatalf("observer link closed; last roster %+v", last)
			}
			last = m
			if cond(m) {
				return m
			}
		case <-deadline:
			t.Fatalf("no matching roster; last %+v", last)
		}
	}
}

func rosterHas(m voice.Message, name string) bool {
	for _, u := range m.Users {
		if u.Name == name {
			return true
		}
	}
	return false
}

func eventually(t testing.TB, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
