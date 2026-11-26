package proxy

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-mmproxy/internal/metrics"
	"github.com/Tuhis/gawk/gawk-mmproxy/internal/netnstest"
	"github.com/Tuhis/gawk/gawk-mmproxy/internal/proxyproto"
	"github.com/Tuhis/gawk/gawk-mmproxy/internal/routes"
)

// header encodes a v2 header the way the bridge does (docs/92 D2).
func header(src netip.AddrPort) []byte {
	b := append([]byte{}, proxyproto.Signature...)
	if src.Addr().Is4() {
		b = append(b, 0x21, 0x11, 0, 12)
		a := src.Addr().As4()
		b = append(b, a[:]...)
		b = append(b, 0, 0, 0, 0)
	} else {
		b = append(b, 0x21, 0x21, 0, 36)
		a := src.Addr().As16()
		b = append(b, a[:]...)
		b = append(b, make([]byte, 16)...)
	}
	b = binary.BigEndian.AppendUint16(b, src.Port())
	return binary.BigEndian.AppendUint16(b, 0)
}

var clientHello = []byte("\x16\x03\x01\x00\x10 a ClientHello ")

// fakeMurmur accepts on loopback and echoes, or resets when told.
type fakeMurmur struct {
	ln    net.Listener
	mu    sync.Mutex
	got   [][]byte
	reset bool
}

func newFakeMurmur(t *testing.T, reset bool) *fakeMurmur {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeMurmur{ln: ln, reset: reset}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c *net.TCPConn) {
				buf := make([]byte, len(clientHello))
				if _, err := io.ReadFull(c, buf); err != nil {
					c.Close()
					return
				}
				f.mu.Lock()
				f.got = append(f.got, buf)
				f.mu.Unlock()
				if f.reset {
					c.SetLinger(0) // like murmur refusing a banned address (F8)
					c.Close()
					return
				}
				c.Write([]byte("server hello"))
				io.Copy(c, c)
				c.Close()
			}(c.(*net.TCPConn))
		}
	}()
	return f
}

func (f *fakeMurmur) port() uint16 { return uint16(f.ln.Addr().(*net.TCPAddr).Port) }

type rig struct {
	s       *Server
	m       *metrics.Metrics
	srcs    chan netip.Addr
	dialErr error
	routes  routes.State
	cancel  context.CancelFunc
	done    chan struct{}
}

func start(t *testing.T, f *fakeMurmur, mutate func(*Options, *rig)) *rig {
	t.Helper()
	r := &rig{m: metrics.New(), srcs: make(chan netip.Addr, 100), routes: routes.State{V4: true, V6: true}, done: make(chan struct{})}
	port := uint16(1)
	if f != nil {
		port = f.port()
	}
	o := Options{
		Listen: "127.0.0.1:0", MurmurPort: port, RouteTable: routes.DefaultTable, RequireRoutes: true,
		HeaderTimeout: 2 * time.Second, MaxConns: 1024,
		CheckRoutes: func() (routes.State, error) { return r.routes, nil },
		Dial: func(ctx context.Context, src netip.Addr, target netip.AddrPort) (net.Conn, error) {
			r.srcs <- src
			if r.dialErr != nil {
				return nil, r.dialErr
			}
			var d net.Dialer
			return d.DialContext(ctx, "tcp4", netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), target.Port()).String())
		},
	}
	if mutate != nil {
		mutate(&o, r)
	}
	r.s = New(o, r.m)
	if err := r.s.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	go func() { r.s.Serve(ctx); close(r.done) }()
	t.Cleanup(func() { cancel(); <-r.done })
	return r
}

func (r *rig) dial(t *testing.T) *net.TCPConn {
	t.Helper()
	c, err := net.Dial("tcp", r.s.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c.(*net.TCPConn)
}

// readEnd reads until the connection ends and says how: io.EOF for a FIN,
// syscall.ECONNRESET for a reset.
func readEnd(t *testing.T, c net.Conn, sentFIN ...bool) error {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err := io.Copy(io.Discard, c)
	// A concurrent writer on this side can consume the reset, leaving the
	// read a plain EOF; the TCP state still says which it was.
	if tc, ok := c.(*net.TCPConn); ok && peerReset(tc, err, len(sentFIN) > 0 && sentFIN[0]) {
		return syscall.ECONNRESET
	}
	if err == nil {
		return io.EOF
	}
	if errors.Is(err, syscall.ECONNRESET) {
		return syscall.ECONNRESET
	}
	return err
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 300; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestProxiesWithTheHeadersSourceAndNothingLost(t *testing.T) {
	f := newFakeMurmur(t, false)
	r := start(t, f, nil)
	c := r.dial(t)
	c.Write(append(header(netip.MustParseAddrPort("203.0.113.7:40001")), clientHello...))
	buf := make([]byte, len("server hello"))
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "server hello" {
		t.Fatalf("read %q, %v", buf, err)
	}
	if src := <-r.srcs; src != netip.MustParseAddr("203.0.113.7") {
		t.Errorf("dialled as %v", src)
	}
	f.mu.Lock()
	if len(f.got) != 1 || !bytes.Equal(f.got[0], clientHello) {
		t.Errorf("murmur got %q, want the ClientHello intact", f.got)
	}
	f.mu.Unlock()
	c.Write([]byte("ping"))
	if _, err := io.ReadFull(c, buf[:4]); err != nil || string(buf[:4]) != "ping" {
		t.Fatalf("echo %q, %v", buf[:4], err)
	}
	c.CloseWrite()
	if err := readEnd(t, c, true); err != io.EOF {
		t.Errorf("end %v, want a half-close passed on as EOF", err)
	}
	waitFor(t, "active to drop", func() bool { return r.m.Snapshot().Active == 0 })
	if s := r.m.Snapshot(); s.Conns["proxied"] != 1 || s.Resets != 0 {
		t.Errorf("%+v", s)
	}
}

func TestMurmurResetIsPassedOnAsAReset(t *testing.T) {
	f := newFakeMurmur(t, true)
	r := start(t, f, nil)
	c := r.dial(t)
	c.Write(append(header(netip.MustParseAddrPort("198.51.100.9:1")), clientHello...))
	if err := readEnd(t, c); err != syscall.ECONNRESET {
		t.Fatalf("end %v, want a reset (a ban, F8)", err)
	}
	waitFor(t, "the reset count", func() bool { return r.m.Snapshot().Resets == 1 })
	if s := r.m.Snapshot(); s.Conns["proxied"] != 1 {
		t.Errorf("a reset after proxied is counted once, as proxied: %+v", s.Conns)
	}
}

// Every refusal is an EOF, never a reset, even with the ClientHello queued
// behind the header (docs/92 D6, PX1 criterion 2).
func TestRefusalsAreAFINNeverAReset(t *testing.T) {
	f := newFakeMurmur(t, false)
	good := header(netip.MustParseAddrPort("203.0.113.7:1"))
	badCmd := append([]byte{}, good...)
	badCmd[12] = 0x20 // LOCAL
	for _, c := range []struct {
		name   string
		send   []byte
		mutate func(*Options, *rig)
		want   string
	}{
		{"bad header", append(badCmd, clientHello...), nil, "bad_header"},
		{"v1 text", append([]byte("PROXY TCP4 1.2.3.4 5.6.7.8 1 2\r\n"), clientHello...), nil, "bad_header"},
		{"refused source", append(header(netip.MustParseAddrPort("0.0.0.0:1")), clientHello...), nil, "refused_source"},
		{"murmur unreachable", append(good, clientHello...), func(_ *Options, r *rig) { r.dialErr = syscall.ECONNREFUSED }, "murmur_unreachable"},
		{"no routes", append(good, clientHello...), func(_ *Options, r *rig) { r.routes = routes.State{} }, "no_routes"},
		{"no IPv6 routes for an IPv6 source", append(header(netip.MustParseAddrPort("[2001:db8::7]:1")), clientHello...), func(_ *Options, r *rig) { r.routes = routes.State{V4: true} }, "no_routes"},
		{"header timeout", good[:10], func(o *Options, _ *rig) { o.HeaderTimeout = 100 * time.Millisecond }, "header_timeout"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := start(t, f, c.mutate)
			conn := r.dial(t)
			conn.Write(c.send)
			// More bytes while the proxy decides, like a client that
			// doesn't wait.
			if c.want != "header_timeout" { // there, more bytes would complete a bad signature
				conn.Write(bytes.Repeat([]byte("x"), 4096))
			}
			if err := readEnd(t, conn); err != io.EOF {
				t.Fatalf("end %v, want EOF", err)
			}
			conn.Close()
			waitFor(t, "the drain to finish", func() bool { return r.m.Snapshot().Draining["slots"] == 0 })
			s := r.m.Snapshot()
			if s.Conns[c.want] != 1 {
				t.Errorf("counts %v, want one %s", s.Conns, c.want)
			}
			total := uint64(0)
			for _, n := range s.Conns {
				total += n
			}
			if total != 1 || s.Active != 0 {
				t.Errorf("each connection is counted once and leaves its slot: total %d, active %d", total, s.Active)
			}
		})
	}
}

// RequireRoutes=false (the e2e) proxies without them.
func TestRequireRoutesFalse(t *testing.T) {
	f := newFakeMurmur(t, false)
	r := start(t, f, func(o *Options, r *rig) { o.RequireRoutes = false; r.routes = routes.State{} })
	c := r.dial(t)
	c.Write(append(header(netip.MustParseAddrPort("127.0.0.5:1")), clientHello...))
	buf := make([]byte, len("server hello"))
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatalf("no proxying without the routes check: %v", err)
	}
}

// Slots, the over-cap pool and `undrained` (docs/92 D6).
func TestAccounting(t *testing.T) {
	const maxConns = 3
	r := start(t, nil, func(o *Options, _ *rig) { o.MaxConns = maxConns })
	slow := make([]*net.TCPConn, maxConns)
	for i := range slow {
		slow[i] = r.dial(t) // no header yet: holding a slot
	}
	waitFor(t, "three slot holders", func() bool { return r.m.Snapshot().Active == maxConns })
	over := make([]*net.TCPConn, OverCapPool)
	for i := range over {
		over[i] = r.dial(t)
		over[i].Write(clientHello) // unread data: a plain close would reset
	}
	waitFor(t, "a full over-cap pool", func() bool { return r.m.Snapshot().Draining["over_cap"] == OverCapPool })
	extra := r.dial(t)
	extra.Write(clientHello)
	if err := readEnd(t, extra); err == nil {
		t.Fatal("no end")
	}
	waitFor(t, "undrained", func() bool { return r.m.Snapshot().Conns["undrained"] == 1 })
	s := r.m.Snapshot()
	if s.Active != maxConns || s.Draining["slots"] != 0 {
		t.Errorf("slots in use %d = active %d + draining{slots} %d", maxConns, s.Active, s.Draining["slots"])
	}
	for i, c := range over {
		if err := readEnd(t, c); err != io.EOF {
			t.Fatalf("over-cap %d: end %v, want EOF", i, err)
		}
	}
	waitFor(t, "the pool to empty", func() bool { return r.m.Snapshot().Draining["over_cap"] == 0 })
	if s := r.m.Snapshot(); s.Conns["over_cap"] != OverCapPool {
		t.Errorf("over_cap %d, want %d", s.Conns["over_cap"], OverCapPool)
	}
	for _, c := range slow {
		c.Close()
	}
	waitFor(t, "every slot back", func() bool { return r.m.Snapshot().Active == 0 && r.m.Snapshot().Draining["slots"] == 0 })
	total := uint64(0)
	for _, n := range r.m.Snapshot().Conns {
		total += n
	}
	if want := uint64(maxConns + OverCapPool + 1); total != want {
		t.Errorf("results add up to %d, want the %d connections accepted", total, want)
	}
	// A slot is free again.
	c := r.dial(t)
	c.Write(append(header(netip.MustParseAddrPort("203.0.113.7:1")), clientHello...))
	if err := readEnd(t, c); err != io.EOF {
		t.Errorf("after the burst: %v", err)
	}
}

// The real thing in a fresh namespace: routes installed, murmur stand-ins on
// 127.0.0.1 and ::1, and the spoofed source as the accepted peer (F1, F6).
func TestTransparentDialSpoofsTheSource(t *testing.T) {
	netnstest.Run(t, func(t *testing.T) {
		for _, r := range routes.Install(routes.DefaultTable) {
			if r.Err != nil {
				t.Fatalf("%s: %v", r.Family, r.Err)
			}
		}
		ln, err := net.Listen("tcp", "[::]:0") // dual-stack, as murmur's
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		port := uint16(ln.Addr().(*net.TCPAddr).Port)
		peers := make(chan netip.Addr, 4)
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				peers <- c.RemoteAddr().(*net.TCPAddr).AddrPort().Addr().Unmap()
				c.Write([]byte("hi"))
				c.Close()
			}
		}()
		m := metrics.New()
		s := New(Options{Listen: "127.0.0.1:0", MurmurPort: port, RouteTable: routes.DefaultTable, RequireRoutes: true, HeaderTimeout: time.Second, MaxConns: 8}, m)
		if err := s.Listen(); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go s.Serve(ctx)
		for _, src := range []string{"203.0.113.7:40001", "[2001:db8::7]:40020"} {
			c, err := net.Dial("tcp", s.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			c.Write(header(netip.MustParseAddrPort(src)))
			buf := make([]byte, 2)
			if _, err := io.ReadFull(c, buf); err != nil {
				t.Fatalf("%s: %v (counts %v)", src, err, m.Snapshot().Conns)
			}
			c.Close()
			if got, want := <-peers, netip.MustParseAddrPort(src).Addr(); got != want {
				t.Errorf("murmur saw %v, want %v", got, want)
			}
		}
	})
}

// The bridge keeps writing (a TLS handshake in flight) while murmur resets.
// Whichever direction meets the reset first, the bridge must see a reset,
// never a FIN (F8, F9): found against real murmur's autoban, where the
// bridge→murmur write consumed the reset and the other direction read EOF.
func TestMurmurResetWhileTheBridgeWritesIsStillAReset(t *testing.T) {
	f := newFakeMurmur(t, true)
	r := start(t, f, nil)
	for i := 0; i < 50; i++ {
		c := r.dial(t)
		c.Write(append(header(netip.MustParseAddrPort("198.51.100.9:1")), clientHello...))
		stop := make(chan struct{})
		go func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := c.Write(bytes.Repeat([]byte("y"), 512)); err != nil {
					return
				}
				time.Sleep(50 * time.Microsecond)
			}
		}()
		err := readEnd(t, c)
		close(stop)
		if err != syscall.ECONNRESET {
			t.Fatalf("run %d: end %v, want a reset", i, err)
		}
	}
}

// Murmur turns a banned address away at once. Whether that close is a reset
// or a FIN depends on whether the ClientHello had arrived (unread data makes
// it a reset): a race, lost on a CI runner where real murmur 1.5's ban came
// back as a FIN and the bridge read murmur_unreachable. A murmur that
// accepts and closes before sending a byte is murmur refusing, however it
// closed; the bridge must see a reset (docs/92 §3.1 F22).
func TestMurmurClosingBeforeAByteIsARefusalEvenAsAFIN(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close() // nothing read yet: a FIN
		}
	}()
	port := uint16(ln.Addr().(*net.TCPAddr).Port)
	r := start(t, nil, func(o *Options, _ *rig) { o.MurmurPort = port })
	for i := 0; i < 20; i++ {
		c := r.dial(t)
		c.Write(header(netip.MustParseAddrPort("198.51.100.20:1")))
		time.Sleep(20 * time.Millisecond) // murmur closes before the ClientHello
		c.Write(clientHello)
		if err := readEnd(t, c); err != syscall.ECONNRESET {
			t.Fatalf("run %d: end %v, want a reset", i, err)
		}
	}
}

// F22's "closed before a byte" rule is murmur's refusal only when murmur
// ended it. When the bridge ends the connection first (a reset, or a FIN
// from an expired dial or a pod going away), the proxy must not count a
// murmur reset nor reset the bridge: gawk_mmproxy_resets_total is what an
// operator reads as "murmur is refusing addresses" (round 1 review).
func TestBridgeEndingFirstIsNotAMurmurRefusal(t *testing.T) {
	// A murmur that reads until EOF, never writes, then closes.
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(io.Discard, c); c.Close() }()
		}
	}()
	port := uint16(ln.Addr().(*net.TCPAddr).Port)

	t.Run("the bridge resets", func(t *testing.T) {
		r := start(t, nil, func(o *Options, _ *rig) { o.MurmurPort = port })
		c := r.dial(t)
		c.Write(append(header(netip.MustParseAddrPort("198.51.100.30:1")), clientHello...))
		waitFor(t, "the dial", func() bool { return r.m.Snapshot().Conns["proxied"] == 1 })
		c.SetLinger(0)
		c.Close()
		waitFor(t, "the slot back", func() bool { return r.m.Snapshot().Active == 0 })
		if n := r.m.Snapshot().Resets; n != 0 {
			t.Errorf("resets_total %d after the bridge's own reset, want 0", n)
		}
	})
	t.Run("the bridge half-closes", func(t *testing.T) {
		r := start(t, nil, func(o *Options, _ *rig) { o.MurmurPort = port })
		c := r.dial(t)
		c.Write(append(header(netip.MustParseAddrPort("198.51.100.31:1")), clientHello...))
		waitFor(t, "the dial", func() bool { return r.m.Snapshot().Conns["proxied"] == 1 })
		c.CloseWrite()
		if err := readEnd(t, c, true); err != io.EOF {
			t.Errorf("end %v, want the half-close passed back as EOF", err)
		}
		waitFor(t, "the slot back", func() bool { return r.m.Snapshot().Active == 0 })
		if n := r.m.Snapshot().Resets; n != 0 {
			t.Errorf("resets_total %d after the bridge's own FIN, want 0", n)
		}
	})
}
