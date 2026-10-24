package transport

// R76 (docs/77 BE2): a publisher that closes its session with 4014 stopped on
// purpose, and the relay ends the broadcast at once instead of holding it
// through the grace. Driven through the real relay with a Go client, which
// sends the close code exactly as Chrome does (docs/77 §1).

import (
	"context"
	"crypto/tls"
	"fmt"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

func stopTestConfig() config.Config {
	return config.Config{
		MaxSubscribers:  15,
		MaxIdleTimeout:  30 * time.Second,
		KeepAlivePeriod: 10 * time.Second,
		BroadcastGrace:  5 * time.Minute,
	}
}

// dialStoppablePublisher mints a broadcast over a QUIC connection the test
// owns, so the session's close reaches the relay the way Chrome's does.
// webtransport-go's Transport.Dial closes the connection the instant its
// session ends, and that CONNECTION_CLOSE usually beats the close capsule
// carrying the code (a client that closes like that never stops a broadcast;
// docs/77 §1). The connection is closed at cleanup.
func dialStoppablePublisher(t *testing.T, ctx context.Context, port int, clientTLS *tls.Config) (*webtransport.Session, string, string) {
	t.Helper()
	return dialStoppablePublisherAt(t, ctx, fmt.Sprintf("127.0.0.1:%d", port), clientTLS)
}

func dialStoppablePublisherAt(t *testing.T, ctx context.Context, addr string, clientTLS *tls.Config) (*webtransport.Session, string, string) {
	t.Helper()
	tlsConf := clientTLS.Clone()
	tlsConf.NextProtos = []string{"h3"}
	qconn, err := quic.DialAddrEarly(ctx, addr, tlsConf,
		&quic.Config{EnableDatagrams: true, EnableStreamResetPartialDelivery: true})
	if err != nil {
		t.Fatalf("quic dial: %v", err)
	}
	t.Cleanup(func() { _ = qconn.CloseWithError(0, "") })
	var d webtransport.Transport
	cc, err := d.NewClientConn(qconn)
	if err != nil {
		t.Fatalf("NewClientConn: %v", err)
	}
	_, sess, err := cc.Dial(ctx, "https://"+addr+"/publish", nil)
	if err != nil {
		t.Fatalf("publish dial: %v", err)
	}
	id, token := readPublisherHandshake(t, ctx, sess)
	return sess, id, token
}

func TestPublisherStopEndsTheBroadcastNow(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, r, _ := startTestServerCfg(t, ctx, stopTestConfig())
	pub, id, _ := dialStoppablePublisher(t, ctx, port, clientTLS)
	sub := dialSubscriber(t, ctx, port, id, clientTLS)
	waitFor(t, 5*time.Second, func() bool { return r.Stats().Totals.Subscribers == 1 }, "subscriber registered")

	stoppedAt := time.Now()
	pub.CloseWithError(webtransport.SessionErrorCode(wire.CloseCodeBroadcasterStopped), "broadcaster stopped")
	n, nAt, code, cAt := drainUntilClosed(t, ctx, sub)
	assertNoticedClose(t, "viewer", wire.CloseCodeBroadcasterStopped, n, nAt, code, cAt)
	// The grace is five minutes; the stop must not wait for any of it.
	if took := cAt.Sub(stoppedAt); took > 2*time.Second {
		t.Errorf("viewer closed %v after the stop, want well under the grace", took)
	}
	waitFor(t, 2*time.Second, func() bool { return r.CheckSubscribe(id) != nil }, "the broadcast gone, not in grace")
}

func TestPublisherPlainCloseKeepsTheGrace(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, r, _ := startTestServerCfg(t, ctx, stopTestConfig())
	pub, id := dialPublisherAndGetID(t, ctx, port, clientTLS)
	sub := dialSubscriber(t, ctx, port, id, clientTLS)
	waitFor(t, 5*time.Second, func() bool { return r.Stats().Totals.Subscribers == 1 }, "subscriber registered")

	pub.CloseWithError(0, "")
	waitFor(t, 5*time.Second, func() bool {
		b, ok := r.Stats().Broadcasts[r.ObfuscateID(id)]
		return ok && !b.PublisherActive
	}, "publisher released")
	select {
	case <-sub.Context().Done():
		t.Fatal("a plain publisher close ended the viewer's session; the grace should hold it")
	case <-time.After(500 * time.Millisecond):
	}
	if err := r.CheckSubscribe(id); err != nil {
		t.Errorf("broadcast not joinable in grace: %v", err)
	}
}

// docs/77 D4: only a publish session's 4014 means anything.
func TestViewerStopCodeEndsNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, r, _ := startTestServerCfg(t, ctx, stopTestConfig())
	_, id := dialPublisherAndGetID(t, ctx, port, clientTLS)
	sub := dialSubscriber(t, ctx, port, id, clientTLS)
	waitFor(t, 5*time.Second, func() bool { return r.Stats().Totals.Subscribers == 1 }, "subscriber registered")

	sub.CloseWithError(webtransport.SessionErrorCode(wire.CloseCodeBroadcasterStopped), "not mine to stop")
	waitFor(t, 5*time.Second, func() bool { return r.Stats().Totals.Subscribers == 0 }, "subscriber released")
	if err := r.CheckSubscribe(id); err != nil {
		t.Errorf("a viewer's 4014 ended the broadcast: %v", err)
	}
}

// Web Stop then Start, and desktop End's undo: a token-bearing reclaim of the
// stopped code starts the broadcast again, and a viewer can watch it.
func TestPublisherStopThenReclaimRecreates(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, r, _ := startTestServerCfg(t, ctx, stopTestConfig())
	pub, id, token := dialStoppablePublisher(t, ctx, port, clientTLS)
	pub.CloseWithError(webtransport.SessionErrorCode(wire.CloseCodeBroadcasterStopped), "broadcaster stopped")
	waitFor(t, 5*time.Second, func() bool { return r.CheckSubscribe(id) != nil }, "the broadcast ended")

	again := dialPublisherReclaim(t, ctx, port, id, token, clientTLS)
	defer again.CloseWithError(0, "")
	sub := dial(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/subscribe/%s", port, id), clientTLS)
	defer sub.CloseWithError(0, "")
	waitFor(t, 5*time.Second, func() bool { return r.Stats().Totals.Subscribers == 1 }, "a viewer of the restarted broadcast")
}
