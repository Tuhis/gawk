// Package proxy is gawk-mmproxy's data path (docs/92 D3, D5, D6): it takes a
// PROXY v2 header from the bridge, dials murmur over loopback with the
// header's source address as its own (IP_TRANSPARENT), and copies both ways.
//
// Error signalling (D6). A murmur reset is passed to the bridge as a reset,
// whichever direction meets it first (pipe, peerReset), and so is murmur
// closing a dialled connection before it sent a byte (a refusal, F22).
// Every refusal (a bad or late header, a refused source, no routes, murmur
// unreachable, over the cap) is a FIN before any byte is sent: SHUT_WR, then
// a drain until EOF or DrainDeadline, then close. A plain close(2) would not
// do: Linux resets a socket whose receive queue still holds the ClientHello
// that follows the header, and the bridge reads a reset as `autoban`.
//
// Accounting (D6). Every accepted connection holds one of MaxConns slots
// until it is closed: while it waits for its header, dials murmur, is
// proxied, or drains after a refusal. A connection accepted while every slot
// is taken drains in a separate pool of OverCapPool; past that it is closed
// at once (`undrained`). Each accepted connection is counted exactly once.
//
// Nothing in this package logs, and no error it keeps carries an address
// (docs/92 D10): a client's address only ever reaches the kernel.
package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Tuhis/gawk/gawk-mmproxy/internal/metrics"
	"github.com/Tuhis/gawk/gawk-mmproxy/internal/proxyproto"
	"github.com/Tuhis/gawk/gawk-mmproxy/internal/routes"
)

// Constants of D6, deliberately not knobs.
const (
	OverCapPool   = 64
	DrainDeadline = time.Second
	DialTimeout   = 2 * time.Second
	RoutesEvery   = 5 * time.Second
	ShutdownWait  = 2 * time.Second
)

// Options configure the server. Listen through MaxConns are §5.1's knobs.
type Options struct {
	Listen        string
	MurmurPort    uint16
	RouteTable    uint32
	RequireRoutes bool
	HeaderTimeout time.Duration
	MaxConns      int

	// Dial replaces the transparent loopback dial (tests).
	Dial func(ctx context.Context, src netip.Addr, target netip.AddrPort) (net.Conn, error)
	// CheckRoutes replaces routes.Check (tests).
	CheckRoutes func() (routes.State, error)
}

// Server is one listener.
type Server struct {
	opts  Options
	m     *metrics.Metrics
	ln    net.Listener
	slots chan struct{}
	over  chan struct{}

	routes atomic.Pointer[routeState]
	wg     sync.WaitGroup
}

type routeState struct {
	st  routes.State
	err error
}

// New returns a server; call Listen, then Serve.
func New(opts Options, m *metrics.Metrics) *Server {
	if opts.CheckRoutes == nil {
		t := opts.RouteTable
		opts.CheckRoutes = func() (routes.State, error) { return routes.Check(t) }
	}
	if opts.Dial == nil {
		opts.Dial = TransparentDial
	}
	s := &Server{opts: opts, m: m, slots: make(chan struct{}, opts.MaxConns), over: make(chan struct{}, OverCapPool)}
	s.refreshRoutes()
	return s
}

// Listen binds the listener.
func (s *Server) Listen() error {
	ln, err := net.Listen("tcp", s.opts.Listen)
	if err != nil {
		return err
	}
	s.ln = ln
	return nil
}

// Addr is the bound address.
func (s *Server) Addr() net.Addr { return s.ln.Addr() }

// Routes is the last routes check.
func (s *Server) Routes() (routes.State, error) {
	r := s.routes.Load()
	return r.st, r.err
}

func (s *Server) refreshRoutes() {
	st, err := s.opts.CheckRoutes()
	s.routes.Store(&routeState{st: st, err: err})
	s.m.SetRoutesOK(err == nil && st.V4)
}

// Serve accepts until ctx ends, then closes the listener and waits for the
// connections in flight. It never returns on a per-connection condition
// (D15): only the listener failing ends it.
func (s *Server) Serve(ctx context.Context) error {
	go func() {
		t := time.NewTicker(RoutesEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.refreshRoutes()
			}
		}
	}()
	go func() {
		<-ctx.Done()
		s.ln.Close()
	}()
	for {
		c, err := s.ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				// Shutting down: give connections in flight a moment, no
				// more. Murmur's pod is going too, and the kernel closes
				// whatever is left when the process exits.
				waited := make(chan struct{})
				go func() { s.wg.Wait(); close(waited) }()
				select {
				case <-waited:
				case <-time.After(ShutdownWait):
				}
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() || errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENFILE) {
				time.Sleep(50 * time.Millisecond)
				continue
			}
			return err
		}
		tc := c.(*net.TCPConn)
		select {
		case s.slots <- struct{}{}:
			s.m.Active(1)
			s.wg.Add(1)
			go s.handle(tc)
		default:
			select {
			case s.over <- struct{}{}:
				s.m.Count(metrics.OverCap)
				s.m.Draining(metrics.OverCapPool, 1)
				s.wg.Add(1)
				go func() {
					defer s.wg.Done()
					finClose(tc)
					s.m.Draining(metrics.OverCapPool, -1)
					<-s.over
				}()
			default:
				// Past both bounds: close without draining. The client may
				// see a reset; the metric says what it was.
				s.m.Count(metrics.Undrained)
				tc.Close()
			}
		}
	}
}

func (s *Server) handle(c *net.TCPConn) {
	defer s.wg.Done()
	defer func() { <-s.slots }()
	_ = c.SetReadDeadline(time.Now().Add(s.opts.HeaderTimeout))
	h, err := proxyproto.Read(c)
	if err != nil {
		s.refuse(c, classify(err))
		return
	}
	_ = c.SetReadDeadline(time.Time{})
	src := h.Source.Addr()
	target := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), s.opts.MurmurPort)
	if src.Is6() {
		target = netip.AddrPortFrom(netip.IPv6Loopback(), s.opts.MurmurPort)
	}
	if s.opts.RequireRoutes {
		st, err := s.Routes()
		if err != nil || (src.Is4() && !st.V4) || (src.Is6() && !st.V6) {
			s.refuse(c, metrics.NoRoutes)
			return
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), DialTimeout)
	m, err := s.opts.Dial(ctx, src, target)
	cancel()
	if err != nil {
		s.refuse(c, metrics.MurmurUnreachable)
		return
	}
	s.m.Count(metrics.Proxied)
	pipe(c, m.(*net.TCPConn), s.m)
	s.m.Active(-1)
}

func classify(err error) metrics.Result {
	switch {
	case errors.Is(err, os.ErrDeadlineExceeded):
		return metrics.HeaderTimeout
	case errors.Is(err, proxyproto.ErrSource):
		return metrics.RefusedSource
	}
	return metrics.BadHeader
}

// refuse counts the outcome and closes with a FIN, draining in the
// connection's own slot.
func (s *Server) refuse(c *net.TCPConn, r metrics.Result) {
	s.m.Count(r)
	s.m.Active(-1)
	s.m.Draining(metrics.Slots, 1)
	finClose(c)
	s.m.Draining(metrics.Slots, -1)
}

// finClose sends a FIN, drains for a fresh DrainDeadline, then closes, so
// nothing unread is left to turn the close into a reset.
func finClose(c *net.TCPConn) {
	_ = c.CloseWrite()
	_ = c.SetReadDeadline(time.Now().Add(DrainDeadline))
	_, _ = io.Copy(io.Discard, c)
	_ = c.Close()
}

func isReset(err error) bool { return errors.Is(err, syscall.ECONNRESET) }

// peerReset reports whether c's peer reset it, after a read ended. A reset
// is reported once, as ECONNRESET, to whichever call meets it first; when
// the other direction's write consumed it, this read sees a plain EOF. The
// TCP state still tells: a reset leaves the socket in TCP_CLOSE, a FIN in
// TCP_CLOSE_WAIT. One other path reaches TCP_CLOSE: both FINs exchanged
// (the socket hands over to a timewait sock), which needs our own FIN
// first, so sentFIN rules it out.
func peerReset(c *net.TCPConn, readErr error, sentFIN bool) bool {
	if isReset(readErr) {
		return true
	}
	if sentFIN {
		return false
	}
	rc, err := c.SyscallConn()
	if err != nil {
		return false
	}
	reset := false
	_ = rc.Control(func(fd uintptr) {
		if info, err := unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO); err == nil {
			reset = info.State == tcpClose
		}
	})
	return reset
}

const tcpClose = 7 // TCP_CLOSE

// pipe copies both ways with explicit loops, so each side's read and write
// errors stay apart (docs/92 D6). Murmur's reset is decided on the
// murmur→bridge side alone, from peerReset, and passed on as a reset; the
// bridge→murmur side, meeting it as a write error, just stops. A bridge
// reset is passed to murmur as a reset. Anything else is a half-close.
func pipe(c, m *net.TCPConn, mt *metrics.Metrics) {
	done := make(chan struct{}, 2)
	var halfClosed atomic.Bool // we sent murmur our FIN
	go func() {                // bridge → murmur
		buf := make([]byte, 32<<10)
		for {
			n, rerr := c.Read(buf)
			if n > 0 {
				if _, werr := m.Write(buf[:n]); werr != nil {
					break // murmur's side: the other direction reports it
				}
			}
			if rerr != nil {
				if isReset(rerr) {
					_ = m.SetLinger(0)
					_ = m.Close()
				} else {
					halfClosed.Store(true)
					_ = m.CloseWrite()
				}
				break
			}
		}
		done <- struct{}{}
	}()
	go func() { // murmur → bridge
		buf := make([]byte, 32<<10)
		sent := false
		for {
			n, rerr := m.Read(buf)
			if n > 0 {
				sent = true
				if _, werr := c.Write(buf[:n]); werr != nil {
					_ = m.Close() // the bridge is gone
					break
				}
			}
			if rerr != nil {
				// Murmur closing before it sent a byte is murmur refusing
				// the address, however it closed: a healthy murmur answers
				// with its ServerHello first, and whether its refusal is a
				// reset or a FIN depends only on whether the ClientHello
				// had arrived (F22).
				if !sent || peerReset(m, rerr, halfClosed.Load()) {
					mt.Reset()
					_ = c.SetLinger(0)
					_ = c.Close()
				} else {
					_ = c.CloseWrite()
				}
				break
			}
		}
		done <- struct{}{}
	}()
	<-done
	<-done
	_ = c.Close()
	_ = m.Close()
}

// TransparentDial dials target from src, a non-local address, with
// IP_TRANSPARENT (IPV6_TRANSPARENT): CAP_NET_RAW is enough (F11).
func TransparentDial(ctx context.Context, src netip.Addr, target netip.AddrPort) (net.Conn, error) {
	network := "tcp4"
	if target.Addr().Is6() {
		network = "tcp6"
	}
	d := net.Dialer{
		LocalAddr: net.TCPAddrFromAddrPort(netip.AddrPortFrom(src, 0)),
		Control: func(network, _ string, c syscall.RawConn) error {
			var serr error
			err := c.Control(func(fd uintptr) {
				if network == "tcp6" {
					serr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IPV6, unix.IPV6_TRANSPARENT, 1)
				} else {
					serr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_TRANSPARENT, 1)
				}
			})
			if err != nil {
				return err
			}
			return serr
		},
	}
	return d.DialContext(ctx, network, target.String())
}
