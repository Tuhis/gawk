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
	"net/http"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/internal/tlsutil"
	"github.com/Tuhis/gawk/gawk-server/internal/wire"
)

var discardLog = slog.New(slog.NewTextHandler(io.Discard, nil))

// startTestServer runs a Server with an in-memory dev cert on a random UDP
// port. It returns the port, a client TLS config trusting the cert, the
// hub (for state assertions) and Run's completion channel.
func startTestServer(t *testing.T, ctx context.Context, maxSubscribers int) (port int, clientTLS *tls.Config, h *hub.Hub, done chan error) {
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

	cfg := config.Config{Addr: fmt.Sprintf("127.0.0.1:%d", port), MaxSubscribers: maxSubscribers}
	h = hub.New(discardLog, hub.Options{MaxSubscribers: cfg.MaxSubscribers})
	srv := New(cfg, h, func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &cert, nil }, discardLog)

	done = make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()

	pool := x509.NewCertPool()
	pool.AddCert(cert.Leaf)
	clientTLS = &tls.Config{RootCAs: pool, ServerName: "localhost", NextProtos: []string{http3.NextProtoH3}}
	return port, clientTLS, h, done
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
	port, clientTLS, _, _ := startTestServer(t, ctx, 15)

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

// dialOnce dials without retrying and returns the HTTP response and error,
// for asserting rejection status codes. Callers must know the server is
// already up (e.g. after a successful dial helper call).
func dialOnce(t *testing.T, ctx context.Context, url string, clientTLS *tls.Config) (*http.Response, *webtransport.Session, error) {
	t.Helper()
	d := webtransport.Dialer{
		TLSClientConfig: clientTLS,
		QUICConfig:      &quic.Config{EnableDatagrams: true, EnableStreamResetPartialDelivery: true},
	}
	t.Cleanup(func() { d.Close() })
	return d.Dial(ctx, url, nil)
}

// waitFor polls cond until it is true or the timeout elapses.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool, desc string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", desc)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// encodeFrame builds the chunk datagrams of one synthetic frame.
func encodeFrame(t *testing.T, frameID uint32, keyframe bool, chunkCount int) [][]byte {
	t.Helper()
	chunks := make([][]byte, 0, chunkCount)
	for i := range chunkCount {
		payload := bytes.Repeat([]byte{byte(frameID), byte(i)}, 300) // 600 bytes
		d, err := wire.AppendVideoChunk(nil, wire.VideoChunkHeader{
			Keyframe:    keyframe,
			FrameID:     frameID,
			ChunkIndex:  uint16(i),
			ChunkCount:  uint16(chunkCount),
			TimestampUs: uint64(frameID) * 16_667,
		}, payload)
		if err != nil {
			t.Fatalf("AppendVideoChunk: %v", err)
		}
		chunks = append(chunks, d)
	}
	return chunks
}

func testConfigDgram(t *testing.T) []byte {
	t.Helper()
	d, err := wire.AppendDecoderConfig(nil, wire.DecoderConfig{
		Codec:     "avc1.42E02A",
		Extradata: []byte{0x01, 0x42, 0xE0, 0x2A},
	})
	if err != nil {
		t.Fatalf("AppendDecoderConfig: %v", err)
	}
	return d
}

// TestRelayPublishToSubscribe streams synthetic chunked frames through
// /publish and asserts a /subscribe session reassembles at least 95% of
// them intact (loopback; datagrams may still drop in the stack).
func TestRelayPublishToSubscribe(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, h, _ := startTestServer(t, ctx, 15)

	sub := dial(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/subscribe", port), clientTLS)
	// Dial returns on the 200 response; Subscribe runs just after the
	// upgrade, so wait until the hub actually has the subscriber.
	waitFor(t, 5*time.Second, func() bool { return h.Stats().Subscribers == 1 }, "subscriber registered")

	pub := dial(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/publish", port), clientTLS)

	const totalFrames = 40
	const chunksPerFrame = 3

	// Receiver: reassemble frames until complete or timed out.
	type recvState struct {
		gotConfig bool
		frames    map[uint32]map[uint16]bool
	}
	resultCh := make(chan recvState, 1)
	go func() {
		st := recvState{frames: make(map[uint32]map[uint16]bool)}
		recvCtx, recvCancel := context.WithTimeout(ctx, 10*time.Second)
		defer recvCancel()
		defer func() { resultCh <- st }()
		for {
			dgram, err := sub.ReceiveDatagram(recvCtx)
			if err != nil {
				return
			}
			_, typ, err := wire.PeekType(dgram)
			if err != nil {
				continue
			}
			switch typ {
			case wire.TypeDecoderConfig:
				st.gotConfig = true
			case wire.TypeVideoChunk:
				hdr, _, err := wire.ParseVideoChunk(dgram)
				if err != nil {
					continue
				}
				m := st.frames[hdr.FrameID]
				if m == nil {
					m = make(map[uint16]bool)
					st.frames[hdr.FrameID] = m
				}
				m[hdr.ChunkIndex] = true
			}
			// Done once every frame is complete.
			complete := 0
			for _, m := range st.frames {
				if len(m) == chunksPerFrame {
					complete++
				}
			}
			if st.gotConfig && complete == totalFrames {
				return
			}
		}
	}()

	if err := pub.SendDatagram(testConfigDgram(t)); err != nil {
		t.Fatalf("send config: %v", err)
	}
	for frameID := uint32(0); frameID < totalFrames; frameID++ {
		for _, chunk := range encodeFrame(t, frameID, frameID%10 == 0, chunksPerFrame) {
			if err := pub.SendDatagram(chunk); err != nil {
				t.Fatalf("send frame %d: %v", frameID, err)
			}
		}
		time.Sleep(2 * time.Millisecond) // ~realistic frame pacing, avoids queue overrun
	}

	st := <-resultCh
	if !st.gotConfig {
		t.Error("subscriber never received a decoder config datagram")
	}
	complete := 0
	for _, m := range st.frames {
		if len(m) == chunksPerFrame {
			complete++
		}
	}
	if complete < totalFrames*95/100 {
		t.Errorf("subscriber reassembled %d/%d frames intact, want >= 95%%", complete, totalFrames)
	}
}

func TestSecondPublisherConflict(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, _, _ := startTestServer(t, ctx, 15)
	url := fmt.Sprintf("https://127.0.0.1:%d/publish", port)

	first := dial(t, ctx, url, clientTLS)
	_ = first

	rsp, sess, err := dialOnce(t, ctx, url, clientTLS)
	if err == nil {
		sess.CloseWithError(0, "")
		t.Fatal("second publisher dial succeeded, want 409 rejection")
	}
	if rsp == nil || rsp.StatusCode != http.StatusConflict {
		t.Fatalf("second publisher response = %v (err %v), want status 409", rsp, err)
	}
}

func TestPublisherDisconnectFreesSlot(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, h, _ := startTestServer(t, ctx, 15)
	url := fmt.Sprintf("https://127.0.0.1:%d/publish", port)

	first := dial(t, ctx, url, clientTLS)
	waitFor(t, 5*time.Second, func() bool { return h.Stats().PublisherActive }, "publisher registered")
	first.CloseWithError(0, "done")
	waitFor(t, 5*time.Second, func() bool { return !h.Stats().PublisherActive }, "publisher slot freed")

	second := dial(t, ctx, url, clientTLS)
	_ = second
}

func TestSubscriberLimitRejected(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, h, _ := startTestServer(t, ctx, 1)
	url := fmt.Sprintf("https://127.0.0.1:%d/subscribe", port)

	first := dial(t, ctx, url, clientTLS)
	_ = first
	waitFor(t, 5*time.Second, func() bool { return h.Stats().Subscribers == 1 }, "subscriber registered")

	rsp, sess, err := dialOnce(t, ctx, url, clientTLS)
	if err == nil {
		sess.CloseWithError(0, "")
		t.Fatal("second subscriber dial succeeded, want 429 rejection")
	}
	if rsp == nil || rsp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second subscriber response = %v (err %v), want status 429", rsp, err)
	}
}

// TestLateJoinerPrimedOverNetwork publishes a config and a complete
// keyframe, then subscribes and asserts the primed datagrams arrive.
func TestLateJoinerPrimedOverNetwork(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, h, _ := startTestServer(t, ctx, 15)

	pub := dial(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/publish", port), clientTLS)
	if err := pub.SendDatagram(testConfigDgram(t)); err != nil {
		t.Fatalf("send config: %v", err)
	}
	const kfChunks = 4
	for _, chunk := range encodeFrame(t, 0, true, kfChunks) {
		if err := pub.SendDatagram(chunk); err != nil {
			t.Fatalf("send keyframe chunk: %v", err)
		}
	}
	waitFor(t, 5*time.Second, func() bool {
		st := h.Stats()
		return st.HasConfig && st.CachedKeyframeChunks == kfChunks
	}, "config and keyframe cached")

	sub := dial(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/subscribe", port), clientTLS)

	gotConfig := false
	gotChunks := make(map[uint16]bool)
	recvCtx, recvCancel := context.WithTimeout(ctx, 5*time.Second)
	defer recvCancel()
	for !gotConfig || len(gotChunks) < kfChunks {
		dgram, err := sub.ReceiveDatagram(recvCtx)
		if err != nil {
			t.Fatalf("priming incomplete (config %v, %d/%d keyframe chunks): %v",
				gotConfig, len(gotChunks), kfChunks, err)
		}
		_, typ, err := wire.PeekType(dgram)
		if err != nil {
			continue
		}
		switch typ {
		case wire.TypeDecoderConfig:
			gotConfig = true
		case wire.TypeVideoChunk:
			hdr, _, err := wire.ParseVideoChunk(dgram)
			if err == nil && hdr.Keyframe && hdr.FrameID == 0 {
				gotChunks[hdr.ChunkIndex] = true
			}
		}
	}
}

func TestGracefulShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	port, clientTLS, _, done := startTestServer(t, ctx, 15)

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
