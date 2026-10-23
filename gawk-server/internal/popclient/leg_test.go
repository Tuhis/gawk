package popclient

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/internal/metrics"
	"github.com/Tuhis/gawk/gawk-server/internal/tlsutil"
	"github.com/Tuhis/gawk/gawk-server/internal/transport"
)

// fakeLeg is an uplink leg session as transport.ServePopUplinkLeg reads
// it: datagrams out are counted, home sends no capabilities, and home ends
// it with a close code.
type fakeLeg struct {
	ctx    context.Context
	cancel context.CancelFunc

	mu     sync.Mutex
	err    *webtransport.SessionError
	dgrams int
}

func newFakeLeg() *fakeLeg {
	l := &fakeLeg{}
	l.ctx, l.cancel = context.WithCancel(context.Background())
	return l
}

func (l *fakeLeg) end(code uint32, msg string, remote bool) {
	l.mu.Lock()
	if l.err == nil {
		l.err = &webtransport.SessionError{Remote: remote, ErrorCode: webtransport.SessionErrorCode(code), Message: msg}
	}
	l.mu.Unlock()
	l.cancel()
}

// homeClose is home ending the leg with code.
func (l *fakeLeg) homeClose(code uint32) { l.end(code, "", true) }

func (l *fakeLeg) sessErr() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err == nil {
		return nil
	}
	return l.err
}

func (l *fakeLeg) SendDatagram(b []byte) error {
	if err := l.sessErr(); err != nil {
		return err
	}
	l.mu.Lock()
	l.dgrams++
	l.mu.Unlock()
	return nil
}

func (l *fakeLeg) ReceiveDatagram(ctx context.Context) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-l.ctx.Done():
		return nil, l.sessErr()
	}
}

func (l *fakeLeg) OpenUniStream() (*webtransport.SendStream, error) {
	if err := l.sessErr(); err != nil {
		return nil, err
	}
	return nil, errors.New("fake leg: no streams")
}

func (l *fakeLeg) AcceptUniStream(context.Context) (*webtransport.ReceiveStream, error) {
	if err := l.sessErr(); err != nil {
		return nil, err
	}
	return nil, errors.New("fake leg: no capabilities")
}

func (l *fakeLeg) OpenStream(context.Context) (io.ReadWriteCloser, error) {
	return nil, errors.New("fake leg: no bidi streams")
}

func (l *fakeLeg) AcceptStream(ctx context.Context) (io.ReadWriteCloser, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (l *fakeLeg) CloseWithError(code uint32, msg string) error {
	l.end(code, msg, false)
	return nil
}

func (l *fakeLeg) Context() context.Context { return l.ctx }
func (l *fakeLeg) RemoteAddr() net.Addr     { return udpAddr(homeAddr) }
func (l *fakeLeg) LocalAddr() net.Addr      { return udpAddr(popAddr) }

func (l *fakeLeg) datagrams() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.dgrams
}

// relayParts are a PoP relay's hub and transport server (never Run: the
// client only calls into them).
func relayParts(t *testing.T) (*hub.Registry, *transport.Server) {
	t.Helper()
	r := hub.NewRegistry(discard(), hub.Options{MaxBroadcasts: 20, MaxSubscribers: 8, BroadcastGrace: time.Minute})
	cert, err := tlsutil.GenerateDevCert([]string{"localhost"}, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	srv := transport.New(config.Config{MaxBroadcasts: 20, MaxSubscribers: 8}, r,
		func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &cert, nil }, discard(),
		metrics.NewServerMetrics(prometheus.NewRegistry()))
	return r, srv
}
