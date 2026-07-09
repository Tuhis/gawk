package transport

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/tlsutil"
)

var discardLog = slog.New(slog.NewTextHandler(io.Discard, nil))

// startTestServer runs a Server with an in-memory dev cert on a random UDP
// port. It returns the port and a client TLS config trusting the cert.
func startTestServer(t *testing.T, ctx context.Context) (port int, clientTLS *tls.Config, done chan error) {
	t.Helper()

	cert, err := tlsutil.GenerateDevCert([]string{"localhost", "127.0.0.1"}, time.Hour)
	if err != nil {
		t.Fatalf("GenerateDevCert: %v", err)
	}

	// Reserve a random free UDP port.
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port = pc.LocalAddr().(*net.UDPAddr).Port
	pc.Close()

	cfg := config.Config{Addr: fmt.Sprintf("127.0.0.1:%d", port), MaxSubscribers: 15}
	srv := New(cfg, func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &cert, nil }, discardLog)

	done = make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()

	pool := x509.NewCertPool()
	pool.AddCert(cert.Leaf)
	clientTLS = &tls.Config{RootCAs: pool, ServerName: "localhost", NextProtos: []string{http3.NextProtoH3}}
	return port, clientTLS, done
}

func dial(t *testing.T, ctx context.Context, url string, clientTLS *tls.Config) *webtransport.Session {
	t.Helper()
	d := webtransport.Dialer{
		TLSClientConfig: clientTLS,
		QUICConfig:      &quic.Config{EnableDatagrams: true, EnableStreamResetPartialDelivery: true},
	}
	t.Cleanup(func() { d.Close() })

	var sess *webtransport.Session
	var err error
	// The server goroutine may not be listening yet on the first attempt.
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, sess, err = d.Dial(ctx, url, nil)
		if err == nil {
			return sess
		}
		if time.Now().After(deadline) {
			t.Fatalf("Dial %s: %v", url, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestEchoRoundTrip(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, _ := startTestServer(t, ctx)

	sess := dial(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/echo", port), clientTLS)

	payload := []byte("hello gawk")
	if err := sess.SendDatagram(payload); err != nil {
		t.Fatalf("SendDatagram: %v", err)
	}

	recvCtx, recvCancel := context.WithTimeout(ctx, time.Second)
	defer recvCancel()
	got, err := sess.ReceiveDatagram(recvCtx)
	if err != nil {
		t.Fatalf("ReceiveDatagram: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("echoed %q, want %q", got, payload)
	}
}

func TestGracefulShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	port, clientTLS, done := startTestServer(t, ctx)

	// Make sure it is actually serving before we shut it down.
	sess := dial(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/echo", port), clientTLS)
	_ = sess

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v after ctx cancel, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("Run did not return within 5s of ctx cancel")
	}
}
