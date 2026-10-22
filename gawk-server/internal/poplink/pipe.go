package poplink

import (
	"context"
	"io"
	"net"
	"net/netip"
	"sync"

	"github.com/quic-go/webtransport-go"
)

// Pipe returns two connected in-memory Sessions, a stand-in for one QUIC
// link in unit tests: a stream one end opens is the next the other end
// accepts, and CloseWithError ends both, the peer reading a remote
// *webtransport.SessionError with the code (CloseCode reads it).
func Pipe(popAddr, homeAddr netip.AddrPort) (pop, home Session) {
	shared := &pipeShared{}
	shared.ctx, shared.cancel = context.WithCancel(context.Background())
	a := &pipeEnd{shared: shared, local: popAddr, remote: homeAddr, incoming: make(chan io.ReadWriteCloser, 16)}
	b := &pipeEnd{shared: shared, local: homeAddr, remote: popAddr, incoming: make(chan io.ReadWriteCloser, 16)}
	a.peer, b.peer = b, a
	return a, b
}

type pipeShared struct {
	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.Mutex
	closed bool
	by     *pipeEnd
	code   uint32
	msg    string
	conns  []net.Conn
}

type pipeEnd struct {
	shared        *pipeShared
	peer          *pipeEnd
	local, remote netip.AddrPort
	incoming      chan io.ReadWriteCloser
}

func (e *pipeEnd) err() error {
	s := e.shared
	s.mu.Lock()
	defer s.mu.Unlock()
	return &webtransport.SessionError{Remote: s.by != e, ErrorCode: webtransport.SessionErrorCode(s.code), Message: s.msg}
}

func (e *pipeEnd) OpenStream(ctx context.Context) (io.ReadWriteCloser, error) {
	s := e.shared
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, e.err()
	}
	c1, c2 := net.Pipe()
	s.conns = append(s.conns, c1, c2)
	s.mu.Unlock()
	select {
	case e.peer.incoming <- c2:
		return c1, nil
	case <-s.ctx.Done():
		return nil, e.err()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (e *pipeEnd) AcceptStream(ctx context.Context) (io.ReadWriteCloser, error) {
	select {
	case c := <-e.incoming:
		return c, nil
	case <-e.shared.ctx.Done():
		return nil, e.err()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (e *pipeEnd) CloseWithError(code uint32, msg string) error {
	s := e.shared
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed, s.by, s.code, s.msg = true, e, code, msg
	conns := s.conns
	s.mu.Unlock()
	s.cancel()
	for _, c := range conns {
		_ = c.Close()
	}
	return nil
}

func (e *pipeEnd) Context() context.Context { return e.shared.ctx }

func (e *pipeEnd) RemoteAddr() net.Addr { return net.UDPAddrFromAddrPort(e.remote) }

func (e *pipeEnd) LocalAddr() net.Addr { return net.UDPAddrFromAddrPort(e.local) }

// SessionEndErr is the error a pipe end reports once its session closed,
// for tests asserting the close code.
func SessionEndErr(s Session) error {
	if e, ok := s.(*pipeEnd); ok {
		select {
		case <-e.shared.ctx.Done():
			return e.err()
		default:
		}
	}
	return nil
}
