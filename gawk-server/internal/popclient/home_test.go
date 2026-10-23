package popclient

// home_test.go: a scripted home for the link tests. Every dial goes
// through fakeHome.dial: a /pop/link dial is answered with one end of a
// poplink.Pipe, whose other end the test drives as home (read the hello,
// write config, credential, want, bye, close with a code).

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/internal/poplink"
	"github.com/Tuhis/gawk/gawk-server/internal/relaystate"
)

var (
	popAddr  = netip.MustParseAddrPort("192.168.1.20:50000")
	homeAddr = netip.MustParseAddrPort("203.0.113.1:4433")
)

const waitFor = 5 * time.Second

// homeLink is one link as home sees it.
type homeLink struct {
	t     *testing.T
	sess  Session
	url   *url.URL
	hdr   http.Header
	ctrl  io.ReadWriteCloser
	hello poplink.Hello
	recs  chan any
}

// send writes rec to the PoP.
func (l *homeLink) send(rec any) {
	l.t.Helper()
	if err := poplink.Write(l.ctrl, rec); err != nil {
		l.t.Errorf("home: write %T: %v", rec, err)
	}
}

// close ends the link with code.
func (l *homeLink) close(code uint32, msg string) { _ = l.sess.CloseWithError(code, msg) }

// expect waits for the next record of type T from the PoP, skipping others.
func expect[T any](t *testing.T, l *homeLink) T {
	t.Helper()
	deadline := time.After(waitFor)
	for {
		select {
		case rec := <-l.recs:
			if r, ok := rec.(T); ok {
				return r
			}
		case <-deadline:
			var zero T
			t.Fatalf("home: no %T record from the PoP", zero)
			return zero
		}
	}
}

// none checks no record of type T arrives within d.
func none[T any](t *testing.T, l *homeLink, d time.Duration) {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case rec := <-l.recs:
			if _, ok := rec.(T); ok {
				t.Fatalf("home: unexpected %+v", rec)
			}
		case <-deadline:
			return
		}
	}
}

type dialRecord struct {
	network string
	url     *url.URL
	hdr     http.Header
}

// fakeHome answers the PoP's dials.
type fakeHome struct {
	t     *testing.T
	links chan *homeLink

	mu    sync.Mutex
	dials []dialRecord
	// override, when set, answers a dial instead of the default: a non-nil
	// session, response or error is the answer.
	override func(d dialRecord) (Session, *http.Response, error)
}

func newFakeHome(t *testing.T) *fakeHome {
	return &fakeHome{t: t, links: make(chan *homeLink, 8)}
}

func (f *fakeHome) route(fn func(d dialRecord) (Session, *http.Response, error)) {
	f.mu.Lock()
	f.override = fn
	f.mu.Unlock()
}

// dialsTo lists the dials whose path starts with prefix.
func (f *fakeHome) dialsTo(prefix string) []dialRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []dialRecord
	for _, d := range f.dials {
		if strings.HasPrefix(d.url.Path, prefix) {
			out = append(out, d)
		}
	}
	return out
}

func (f *fakeHome) dial(ctx context.Context, network, target string, h http.Header) (Session, *http.Response, error) {
	u, err := url.Parse(target)
	if err != nil {
		return nil, nil, err
	}
	d := dialRecord{network: network, url: u, hdr: h.Clone()}
	f.mu.Lock()
	f.dials = append(f.dials, d)
	override := f.override
	f.mu.Unlock()
	if override != nil {
		if s, rsp, err := override(d); s != nil || rsp != nil || err != nil {
			return s, rsp, err
		}
	}
	switch u.Path {
	case "/pop/link":
		pop, home := poplink.Pipe(popAddr, homeAddr)
		go f.serveLink(home, d)
		return wtLike{pop}, nil, nil
	case "/pop/addr":
		pop, home := poplink.Pipe(netip.MustParseAddrPort("[2001:db8::20]:50000"), netip.MustParseAddrPort("[2001:db8::1]:4433"))
		go func() {
			// Home records the address and ends the session.
			time.Sleep(10 * time.Millisecond)
			_ = home.CloseWithError(0, "")
		}()
		return pop, nil, nil
	}
	return nil, &http.Response{StatusCode: http.StatusNotFound, Header: http.Header{}}, errors.New("fake home: no such route")
}

func (f *fakeHome) serveLink(home Session, d dialRecord) {
	ctx, cancel := context.WithTimeout(context.Background(), waitFor)
	defer cancel()
	ctrl, err := home.AcceptStream(ctx)
	if err != nil {
		return
	}
	rec, err := poplink.Read(ctrl)
	if err != nil {
		return
	}
	hello, ok := rec.(*poplink.Hello)
	if !ok {
		f.t.Errorf("home: first record %T, want a hello", rec)
		return
	}
	l := &homeLink{t: f.t, sess: home, url: d.url, hdr: d.hdr, ctrl: ctrl, hello: *hello, recs: make(chan any, 1024)}
	go func() {
		for {
			rec, err := poplink.Read(ctrl)
			if err != nil {
				return
			}
			select {
			case l.recs <- rec:
			default:
			}
		}
	}()
	f.links <- l
}

// nextLink waits for the PoP's next link.
func (f *fakeHome) nextLink(t *testing.T) *homeLink {
	t.Helper()
	select {
	case l := <-f.links:
		return l
	case <-time.After(waitFor):
		t.Fatal("home: the PoP did not link")
		return nil
	}
}

// noLink checks the PoP does not link within d.
func (f *fakeHome) noLink(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case l := <-f.links:
		t.Fatalf("home: an unexpected link %s", l.url)
	case <-time.After(d):
	}
}

// testOptions are fast-retry Options dialling f.
func testOptions(t *testing.T, f *fakeHome) Options {
	return Options{
		StateDir:      t.TempDir(),
		Version:       "test",
		Dial:          f.dial,
		Log:           slog.New(slog.DiscardHandler),
		RetryMin:      10 * time.Millisecond,
		RetryMax:      50 * time.Millisecond,
		TerminalRetry: 50 * time.Millisecond,
		StatsEvery:    time.Hour,
	}
}

// run starts c.Run until the test ends.
func run(t *testing.T, c *Client) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run = %v, want nil on cancellation", err)
			}
		case <-time.After(waitFor):
			t.Error("Run did not return after cancellation")
		}
	})
}

// waitState waits until the client's state satisfies ok.
func waitState(t *testing.T, c *Client, ok func(LinkState) bool) LinkState {
	t.Helper()
	deadline := time.Now().Add(waitFor)
	for {
		st := c.State()
		if ok(st) {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("state never reached; last %+v", st)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func headerIs(h string) func(LinkState) bool {
	return func(s LinkState) bool { return s.Header == h }
}

// withCredential stores a credential for home.example:4433 in o.StateDir.
func withCredential(t *testing.T, o Options, pop string) {
	t.Helper()
	if err := saveCredential(o.StateDir, &storedCredential{Endpoint: "home.example:4433", Credential: "cred-1", Pop: pop}); err != nil {
		t.Fatal(err)
	}
}

// linkUp runs a client with a stored credential and brings its link up
// with cfg.
func linkUp(t *testing.T, f *fakeHome, o Options, cfg poplink.Config) (*Client, *homeLink) {
	t.Helper()
	withCredential(t, o, "")
	c := New(o)
	run(t, c)
	l := f.nextLink(t)
	l.send(cfg)
	waitState(t, c, headerIs(HeaderLinked))
	return c, l
}

// wtLike is a pipe end whose streams fail with the session's error once
// it closed, as a WebTransport session's do (a bare pipe reports
// io.ErrClosedPipe, which hides home's close code).
type wtLike struct{ Session }

func (s wtLike) OpenStream(ctx context.Context) (io.ReadWriteCloser, error) {
	st, err := s.Session.OpenStream(ctx)
	if err != nil {
		return nil, err
	}
	return wtStream{st, s.Session}, nil
}

type wtStream struct {
	io.ReadWriteCloser
	sess Session
}

func (s wtStream) Read(p []byte) (int, error) {
	n, err := s.ReadWriteCloser.Read(p)
	if err != nil {
		if se := poplink.SessionEndErr(s.sess); se != nil {
			return n, se
		}
	}
	return n, err
}

func (s wtStream) SetWriteDeadline(t time.Time) error {
	if d, ok := s.ReadWriteCloser.(interface{ SetWriteDeadline(time.Time) error }); ok {
		return d.SetWriteDeadline(t)
	}
	return nil
}

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

// netAddr is a net.Addr that is not a UDP address.
type netAddr struct{}

func (netAddr) Network() string { return "fake" }
func (netAddr) String() string  { return "fake" }

func udpAddr(a netip.AddrPort) net.Addr { return net.UDPAddrFromAddrPort(a) }

func saveCredentialRaw(dir string, b []byte) error {
	return relaystate.SaveSecret(dir, CredentialSecret, b)
}
