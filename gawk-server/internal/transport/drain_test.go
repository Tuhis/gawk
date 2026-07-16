// R17 W1 (docs/22): SIGTERM drain + fast death detection.
//
//   - drain sends wire.CloseCodeServerDraining to every open session,
//     staggered evenly across drainWindow (unit, fake sessions);
//   - a cancelled server context drains real sessions before closing (integration);
//   - a shared QUIC StatelessResetKey lets a *different* process reject a
//     surviving client in ~1 RTT, while a mismatched key leaves the client
//     hanging (integration, UDP proxy standing in for kube-proxy re-DNAT).
package transport

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

type fakeDrainSession struct {
	close func(webtransport.SessionErrorCode, string) error

	mu     sync.Mutex
	code   webtransport.SessionErrorCode
	reason string
	closed bool
	order  int
	sleeps time.Duration // cumulative fake sleep before this close
}

func (s *fakeDrainSession) CloseWithError(code webtransport.SessionErrorCode, reason string) error {
	if s.close == nil {
		return nil
	}
	return s.close(code, reason)
}

// TestDrainClosesEverySessionStaggered: every tracked session gets 4002 with
// the drain reason, spread evenly across drainWindow (never exceeding it),
// and sessions untracked before the drain are left alone.
func TestDrainClosesEverySessionStaggered(t *testing.T) {
	srv := New(config.Config{Addr: "127.0.0.1:0"}, nil, nil, discardLog, nil)

	var slept time.Duration
	var order atomic.Int32
	srv.drainSleep = func(d time.Duration) { slept += d }

	const n = 4
	sessions := make([]*fakeDrainSession, n)
	for i := range sessions {
		s := &fakeDrainSession{}
		sessions[i] = s
		srv.trackSession(s)
	}
	gone := &fakeDrainSession{}
	untrack := srv.trackSession(gone)
	untrack() // handler returned before the drain

	closeRecorder := func(s *fakeDrainSession) func(webtransport.SessionErrorCode, string) error {
		return func(code webtransport.SessionErrorCode, reason string) error {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.closed = true
			s.code = code
			s.reason = reason
			s.order = int(order.Add(1))
			s.sleeps = slept
			return nil
		}
	}
	for _, s := range sessions {
		s.close = closeRecorder(s)
	}
	gone.close = closeRecorder(gone)

	if !srv.Ready() {
		t.Fatal("server not ready before drain")
	}
	start := time.Now()
	srv.drain()
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("drain wall time %v with fake sleep; should be immediate", elapsed)
	}
	if srv.Ready() {
		t.Error("server still ready after drain began")
	}

	if gone.closed {
		t.Error("untracked session was closed by the drain")
	}
	wantInterval := drainWindow / time.Duration(n)
	for i, s := range sessions {
		if !s.closed {
			t.Fatalf("session %d not closed", i)
		}
		if s.code != webtransport.SessionErrorCode(wire.CloseCodeServerDraining) {
			t.Errorf("session %d close code = %d, want %d", i, s.code, wire.CloseCodeServerDraining)
		}
		if s.reason != "server draining" {
			t.Errorf("session %d reason = %q", i, s.reason)
		}
		// The i-th close (per recorded order) happens after (order-1) sleeps.
		wantSlept := time.Duration(s.order-1) * wantInterval
		if s.sleeps != wantSlept {
			t.Errorf("session closed as #%d after %v of stagger, want %v", s.order, s.sleeps, wantSlept)
		}
	}
	// Total sleep = the (n-1) staggers plus the final flush delay — the whole
	// drain stays comfortably inside terminationGracePeriodSeconds.
	wantTotal := time.Duration(n-1)*wantInterval + drainFlushDelay
	if slept != wantTotal {
		t.Errorf("total drain sleep %v, want %v (stagger + flush)", slept, wantTotal)
	}
	if slept >= drainWindow+drainFlushDelay {
		t.Errorf("total drain sleep %v, must stay under %v", slept, drainWindow+drainFlushDelay)
	}
}

// The onDrain hook (W3's lease-release seam) runs after the sessions closed.
func TestDrainRunsOnDrainHookAfterCloses(t *testing.T) {
	srv := New(config.Config{Addr: "127.0.0.1:0"}, nil, nil, discardLog, nil)
	srv.drainSleep = func(time.Duration) {}

	s := &fakeDrainSession{}
	closed := false
	s.close = func(webtransport.SessionErrorCode, string) error {
		closed = true
		return nil
	}
	srv.trackSession(s)

	hookRan := false
	srv.onDrain = func() {
		hookRan = true
		if !closed {
			t.Error("onDrain ran before the sessions were closed")
		}
	}
	srv.drain()
	if !hookRan {
		t.Error("onDrain hook never ran")
	}
}

// TestServerDrainsOnShutdown: cancelling the server context sends 4002 to
// live client sessions before the process would exit — the wiring from
// Run(ctx) through drain() with real WebTransport sessions.
func TestServerDrainsOnShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srvCtx, srvCancel := context.WithCancel(ctx)
	port, clientTLS, _, done := startTestServer(t, srvCtx, 2)

	url := fmt.Sprintf("https://127.0.0.1:%d/echo", port)
	sessA := dial(t, ctx, url, clientTLS)
	sessB := dial(t, ctx, url, clientTLS)

	srvCancel()

	for i, sess := range []*webtransport.Session{sessA, sessB} {
		// AcceptUniStream blocks until the session ends and then returns the
		// definitive close error — unlike ReceiveDatagram, whose stream-end
		// error races the close-capsule parse (the Go twin of the JS
		// wt.closed settle race, README gotcha).
		acceptCtx, acceptCancel := context.WithTimeout(ctx, 5*time.Second)
		_, err := sess.AcceptUniStream(acceptCtx)
		acceptCancel()
		if err == nil {
			t.Fatalf("session %d still alive after server drain", i)
		}
		var se *webtransport.SessionError
		if !errors.As(err, &se) {
			t.Fatalf("session %d error = %v, want webtransport.SessionError", i, err)
		}
		if se.ErrorCode != webtransport.SessionErrorCode(wire.CloseCodeServerDraining) {
			t.Errorf("session %d close code = %d, want %d", i, se.ErrorCode, wire.CloseCodeServerDraining)
		}
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v, want nil after graceful drain", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not exit after drain")
	}
}

// New CONNECTs are rejected with 503 once the drain has begun.
func TestConnectRejectedWhileDraining(t *testing.T) {
	srv := New(config.Config{Addr: "127.0.0.1:0"}, nil, nil, discardLog, nil)
	srv.drainSleep = func(time.Duration) {}
	srv.drain()

	for _, route := range []string{"/publish", "/subscribe/ABC234", "/echo"} {
		// Constructed directly (httptest.NewRequest rejects CONNECT targets);
		// the draining check runs before anything else touches the request.
		req := &http.Request{Method: http.MethodConnect, URL: &url.URL{Path: route}, Header: make(http.Header)}
		w := &recordingResponseWriter{header: make(http.Header)}
		switch route {
		case "/publish":
			srv.handlePublish(w, req)
		case "/echo":
			srv.handleEcho(w, req)
		default:
			srv.handleSubscribe(w, req)
		}
		if w.status != http.StatusServiceUnavailable {
			t.Errorf("CONNECT %s while draining = %d, want 503", route, w.status)
		}
	}
}

type recordingResponseWriter struct {
	header http.Header
	status int
}

func (w *recordingResponseWriter) Header() http.Header         { return w.header }
func (w *recordingResponseWriter) Write(b []byte) (int, error) { return len(b), nil }
func (w *recordingResponseWriter) WriteHeader(status int)      { w.status = status }

// udpProxy is a single-client UDP forwarder standing in for the LoadBalancer:
// packets from the first client observed are forwarded to the current backend,
// backend replies go back to that client. Retargeting the backend mid-flow
// models kube-proxy's conntrack flush re-DNATing an established flow to a
// different pod.
type udpProxy struct {
	front   *net.UDPConn
	backend atomic.Pointer[net.UDPAddr]
	client  atomic.Pointer[net.UDPAddr]
	// one socket per backend direction; created lazily toward the current backend
	up *net.UDPConn
}

func newUDPProxy(t *testing.T, backend *net.UDPAddr) *udpProxy {
	t.Helper()
	front, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	up, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	p := &udpProxy{front: front, up: up}
	p.backend.Store(backend)
	t.Cleanup(func() {
		front.Close()
		up.Close()
	})

	// client → backend
	go func() {
		buf := make([]byte, 65535)
		for {
			n, addr, err := front.ReadFromUDP(buf)
			if err != nil {
				return
			}
			p.client.Store(addr)
			if _, err := up.WriteToUDP(buf[:n], p.backend.Load()); err != nil {
				return
			}
		}
	}()
	// backend → client
	go func() {
		buf := make([]byte, 65535)
		for {
			n, _, err := up.ReadFromUDP(buf)
			if err != nil {
				return
			}
			client := p.client.Load()
			if client == nil {
				continue
			}
			if _, err := front.WriteToUDP(buf[:n], client); err != nil {
				return
			}
		}
	}()
	return p
}

func (p *udpProxy) addr() string { return p.front.LocalAddr().String() }

func (p *udpProxy) retarget(backend *net.UDPAddr) { p.backend.Store(backend) }

// startResetServer starts a transport.Server with the given stateless reset
// key and returns its UDP addr plus the client TLS config trusting its cert.
func startResetServer(t *testing.T, ctx context.Context, key []byte) (*net.UDPAddr, *tls.Config) {
	t.Helper()
	cfg := config.Config{
		MaxSubscribers:    2,
		MaxIdleTimeout:    30 * time.Second,
		KeepAlivePeriod:   10 * time.Second,
		BroadcastGrace:    5 * time.Minute,
		StatelessResetKey: key,
	}
	port, clientTLS, _, _ := startTestServerCfg(t, ctx, cfg)
	return &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port}, clientTLS
}

// TestStatelessResetDetectsDeadConnection: a client whose flow is re-DNAT'd
// to a different process sharing the StatelessResetKey sees its session die
// within ~1 RTT of its next packets; with a mismatched key it hangs until the
// idle timeout (docs/22 Decision 3).
func TestStatelessResetDetectsDeadConnection(t *testing.T) {
	sharedKey := make([]byte, 32)
	for i := range sharedKey {
		sharedKey[i] = byte(i + 1)
	}
	otherKey := make([]byte, 32)
	for i := range otherKey {
		otherKey[i] = byte(0xF0 - i)
	}

	cases := []struct {
		name      string
		keyB      []byte
		wantReset bool
	}{
		{"shared key: reset accepted", sharedKey, true},
		{"different key: reset token rejected, client keeps waiting", otherKey, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			// The client only ever handshakes with A; B's cert never comes
			// into play (a stateless reset has no TLS), so A's trust config
			// is the one the dial needs.
			addrA, clientTLS := startResetServer(t, ctx, sharedKey)
			addrB, _ := startResetServer(t, ctx, tc.keyB)
			proxy := newUDPProxy(t, addrA)

			sess := dial(t, ctx, fmt.Sprintf("https://%s/echo", proxy.addr()), clientTLS)
			// Prove liveness through the proxy.
			if err := sess.SendDatagram([]byte("ping")); err != nil {
				t.Fatalf("SendDatagram: %v", err)
			}
			pingCtx, pingCancel := context.WithTimeout(ctx, 5*time.Second)
			if _, err := sess.ReceiveDatagram(pingCtx); err != nil {
				pingCancel()
				t.Fatalf("echo through proxy: %v", err)
			}
			pingCancel()

			// The "pod death + re-DNAT": all further client packets land on B,
			// which has never seen this connection ID.
			proxy.retarget(addrB)

			deadline := time.Now().Add(4 * time.Second)
			errCh := make(chan error, 1)
			go func() {
				for {
					if err := sess.SendDatagram([]byte("probe")); err != nil {
						errCh <- err
						return
					}
					// The session surfaces the reset via its context/read path.
					rcvCtx, rcvCancel := context.WithTimeout(ctx, 100*time.Millisecond)
					_, err := sess.ReceiveDatagram(rcvCtx)
					rcvCancel()
					if err != nil && rcvCtx.Err() == nil {
						errCh <- err
						return
					}
					if time.Now().After(deadline) {
						errCh <- nil
						return
					}
				}
			}()

			select {
			case err := <-errCh:
				if tc.wantReset && err == nil {
					t.Fatal("session survived re-DNAT to a key-sharing process; want stateless reset")
				}
				if !tc.wantReset && err != nil {
					t.Fatalf("session died (%v) despite mismatched reset key; want it to keep waiting", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("probe loop stuck")
			}
		})
	}
}
