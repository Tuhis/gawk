package transport

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
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
	return startTestServerCfg(t, ctx, config.Config{MaxSubscribers: maxSubscribers})
}

// startTestServerCfg is startTestServer with a caller-supplied config, for
// tests that need non-default timeouts. cfg.Addr is overwritten with a
// random free port.
func startTestServerCfg(t *testing.T, ctx context.Context, cfg config.Config) (port int, clientTLS *tls.Config, h *hub.Hub, done chan error) {
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

	cfg.Addr = fmt.Sprintf("127.0.0.1:%d", port)
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

func testConfigDgram(t *testing.T, codec string) []byte {
	t.Helper()
	d, err := wire.AppendDecoderConfig(nil, wire.DecoderConfig{
		Codec:     codec,
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

	if err := pub.SendDatagram(testConfigDgram(t, "avc1.42E02A")); err != nil {
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
	if err := pub.SendDatagram(testConfigDgram(t, "avc1.42E02A")); err != nil {
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

// h3Get performs an HTTP/3 GET against the server, retrying until it is up.
func h3Get(t *testing.T, ctx context.Context, clientTLS *tls.Config, url string) (*http.Response, []byte) {
	t.Helper()
	tr := &http3.Transport{TLSClientConfig: clientTLS}
	t.Cleanup(func() { tr.Close() })
	client := &http.Client{Transport: tr}

	deadline := time.Now().Add(5 * time.Second)
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			t.Fatalf("NewRequest %s: %v", url, err)
		}
		rsp, err := client.Do(req)
		if err == nil {
			body, err := io.ReadAll(rsp.Body)
			rsp.Body.Close()
			if err != nil {
				t.Fatalf("read %s body: %v", url, err)
			}
			return rsp, body
		}
		if time.Now().After(deadline) {
			t.Fatalf("GET %s: %v", url, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestStatuszReachableAndNumbersMove is the C3 acceptance test: /statusz is
// served over HTTP/3, starts zeroed, and reflects hub activity.
func TestStatuszReachableAndNumbersMove(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, h, _ := startTestServer(t, ctx, 15)
	url := fmt.Sprintf("https://127.0.0.1:%d/statusz", port)

	rsp, body := h3Get(t, ctx, clientTLS, url)
	if rsp.StatusCode != http.StatusOK {
		t.Fatalf("GET /statusz status = %d, want 200", rsp.StatusCode)
	}
	if ct := rsp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var before hub.Stats
	if err := json.Unmarshal(body, &before); err != nil {
		t.Fatalf("unmarshal initial /statusz %q: %v", body, err)
	}
	if before != (hub.Stats{}) {
		t.Errorf("initial stats = %+v, want all zero", before)
	}

	sub := dial(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/subscribe", port), clientTLS)
	_ = sub
	waitFor(t, 5*time.Second, func() bool { return h.Stats().Subscribers == 1 }, "subscriber registered")
	pub := dial(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/publish", port), clientTLS)

	const kfChunks = 3
	if err := pub.SendDatagram(testConfigDgram(t, "avc1.42E02A")); err != nil {
		t.Fatalf("send config: %v", err)
	}
	for _, chunk := range encodeFrame(t, 0, true, kfChunks) {
		if err := pub.SendDatagram(chunk); err != nil {
			t.Fatalf("send keyframe chunk: %v", err)
		}
	}
	for _, chunk := range encodeFrame(t, 1, false, 1) {
		if err := pub.SendDatagram(chunk); err != nil {
			t.Fatalf("send delta chunk: %v", err)
		}
	}
	waitFor(t, 5*time.Second, func() bool {
		st := h.Stats()
		return st.HasConfig && st.CachedKeyframeChunks == kfChunks && st.FramesRelayed >= 2
	}, "stream observed by hub")

	_, body = h3Get(t, ctx, clientTLS, url)
	var after hub.Stats
	if err := json.Unmarshal(body, &after); err != nil {
		t.Fatalf("unmarshal /statusz %q: %v", body, err)
	}
	switch {
	case !after.PublisherActive:
		t.Error("statusz publisherActive = false, want true")
	case after.Subscribers != 1:
		t.Errorf("statusz subscribers = %d, want 1", after.Subscribers)
	case after.FramesRelayed < 2 || after.DatagramsRelayed < kfChunks+1:
		t.Errorf("statusz counters did not move: %+v", after)
	case !after.HasConfig || after.CachedKeyframeChunks != kfChunks || after.CachedKeyframeBytes == 0:
		t.Errorf("statusz cache fields wrong: %+v", after)
	}
}

// TestPublisherRestartPrimesWithNewConfig is the C2 acceptance test over the
// network: after the publisher reconnects with a new config, a joiner is
// primed with the new config and the new session's keyframe — never with
// cached data from the previous session.
func TestPublisherRestartPrimesWithNewConfig(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, h, _ := startTestServer(t, ctx, 15)
	pubURL := fmt.Sprintf("https://127.0.0.1:%d/publish", port)

	// Session 1: old codec, keyframe with frameID 7.
	pub1 := dial(t, ctx, pubURL, clientTLS)
	if err := pub1.SendDatagram(testConfigDgram(t, "avc1.42E02A")); err != nil {
		t.Fatalf("send config: %v", err)
	}
	for _, chunk := range encodeFrame(t, 7, true, 2) {
		if err := pub1.SendDatagram(chunk); err != nil {
			t.Fatalf("send keyframe chunk: %v", err)
		}
	}
	waitFor(t, 5*time.Second, func() bool {
		st := h.Stats()
		return st.HasConfig && st.CachedKeyframeChunks == 2
	}, "session 1 config and keyframe cached")

	pub1.CloseWithError(0, "restart")
	waitFor(t, 5*time.Second, func() bool { return !h.Stats().PublisherActive }, "publisher slot freed")
	if st := h.Stats(); !st.HasConfig {
		t.Fatal("caches must persist while the broadcaster is away")
	}

	// Session 2: new codec, frameIDs restart at 0.
	pub2 := dial(t, ctx, pubURL, clientTLS)
	waitFor(t, 5*time.Second, func() bool {
		st := h.Stats()
		return st.PublisherActive && !st.HasConfig && st.CachedKeyframeChunks == 0
	}, "caches invalidated by new publisher session")

	const newCodec = "vp09.00.40.08"
	const kfChunks = 3
	if err := pub2.SendDatagram(testConfigDgram(t, newCodec)); err != nil {
		t.Fatalf("send new config: %v", err)
	}
	for _, chunk := range encodeFrame(t, 0, true, kfChunks) {
		if err := pub2.SendDatagram(chunk); err != nil {
			t.Fatalf("send new keyframe chunk: %v", err)
		}
	}
	waitFor(t, 5*time.Second, func() bool {
		st := h.Stats()
		return st.HasConfig && st.CachedKeyframeChunks == kfChunks
	}, "session 2 config and keyframe cached")

	// A late joiner must be primed with the new session's data only.
	sub := dial(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/subscribe", port), clientTLS)
	gotCodec := ""
	gotChunks := make(map[uint16]bool)
	recvCtx, recvCancel := context.WithTimeout(ctx, 5*time.Second)
	defer recvCancel()
	for gotCodec == "" || len(gotChunks) < kfChunks {
		dgram, err := sub.ReceiveDatagram(recvCtx)
		if err != nil {
			t.Fatalf("priming incomplete (codec %q, %d/%d keyframe chunks): %v",
				gotCodec, len(gotChunks), kfChunks, err)
		}
		_, typ, err := wire.PeekType(dgram)
		if err != nil {
			continue
		}
		switch typ {
		case wire.TypeDecoderConfig:
			cfg, err := wire.ParseDecoderConfig(dgram)
			if err != nil {
				t.Fatalf("ParseDecoderConfig: %v", err)
			}
			if gotCodec == "" {
				gotCodec = cfg.Codec
			}
		case wire.TypeVideoChunk:
			hdr, _, err := wire.ParseVideoChunk(dgram)
			if err != nil {
				continue
			}
			if hdr.FrameID == 7 {
				t.Fatal("primed with a keyframe chunk from the previous publisher session")
			}
			if hdr.Keyframe && hdr.FrameID == 0 {
				gotChunks[hdr.ChunkIndex] = true
			}
		}
	}
	if gotCodec != newCodec {
		t.Errorf("first primed config codec = %q, want %q", gotCodec, newCodec)
	}
}

// TestSubscriberSurvivesPublisherRestart is the D1 acceptance test: a
// connected viewer must ride out the broadcaster leaving (including an idle
// gap longer than the QUIC idle timeout — the server keepalive is what keeps
// the session alive) and then receive the new session's stream with no
// action of its own.
func TestSubscriberSurvivesPublisherRestart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, h, _ := startTestServerCfg(t, ctx, config.Config{
		MaxSubscribers:  15,
		MaxIdleTimeout:  2 * time.Second,
		KeepAlivePeriod: 250 * time.Millisecond,
	})

	sub := dial(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/subscribe", port), clientTLS)
	waitFor(t, 5*time.Second, func() bool { return h.Stats().Subscribers == 1 }, "subscriber registered")

	// Session 1: stream a config + keyframe and make sure the subscriber
	// actually receives it (proves the fan-out path before the restart).
	pubURL := fmt.Sprintf("https://127.0.0.1:%d/publish", port)
	pub1 := dial(t, ctx, pubURL, clientTLS)
	if err := pub1.SendDatagram(testConfigDgram(t, "avc1.42E02A")); err != nil {
		t.Fatalf("send config: %v", err)
	}
	for _, chunk := range encodeFrame(t, 7, true, 2) {
		if err := pub1.SendDatagram(chunk); err != nil {
			t.Fatalf("send keyframe chunk: %v", err)
		}
	}
	gotChunks := make(map[uint16]bool)
	recvCtx, recvCancel := context.WithTimeout(ctx, 5*time.Second)
	for len(gotChunks) < 2 {
		dgram, err := sub.ReceiveDatagram(recvCtx)
		if err != nil {
			recvCancel()
			t.Fatalf("session 1 stream incomplete (%d/2 keyframe chunks): %v", len(gotChunks), err)
		}
		if _, typ, err := wire.PeekType(dgram); err != nil || typ != wire.TypeVideoChunk {
			continue
		}
		if hdr, _, err := wire.ParseVideoChunk(dgram); err == nil && hdr.FrameID == 7 {
			gotChunks[hdr.ChunkIndex] = true
		}
	}
	recvCancel()

	// Broadcaster leaves. The subscriber must stay connected through a
	// quiet window longer than the idle timeout: only the keepalive PINGs
	// hold the session open, since no media flows.
	pub1.CloseWithError(0, "broadcaster gone")
	waitFor(t, 5*time.Second, func() bool { return !h.Stats().PublisherActive }, "publisher slot freed")
	time.Sleep(3 * time.Second)
	if got := h.Stats().Subscribers; got != 1 {
		t.Fatalf("subscribers after idle gap = %d, want 1 (session idled out?)", got)
	}
	select {
	case <-sub.Context().Done():
		t.Fatal("subscriber session closed during the broadcaster-away gap")
	default:
	}

	// Session 2: new codec, frameIDs restart at 0. The same subscriber
	// session must receive the new config and the full new keyframe.
	pub2 := dial(t, ctx, pubURL, clientTLS)
	const newCodec = "vp09.00.40.08"
	const kfChunks = 3
	if err := pub2.SendDatagram(testConfigDgram(t, newCodec)); err != nil {
		t.Fatalf("send new config: %v", err)
	}
	for _, chunk := range encodeFrame(t, 0, true, kfChunks) {
		if err := pub2.SendDatagram(chunk); err != nil {
			t.Fatalf("send new keyframe chunk: %v", err)
		}
	}

	gotCodec := ""
	newChunks := make(map[uint16]bool)
	recvCtx, recvCancel = context.WithTimeout(ctx, 5*time.Second)
	defer recvCancel()
	for gotCodec != newCodec || len(newChunks) < kfChunks {
		dgram, err := sub.ReceiveDatagram(recvCtx)
		if err != nil {
			t.Fatalf("resumed stream incomplete (codec %q, %d/%d keyframe chunks): %v",
				gotCodec, len(newChunks), kfChunks, err)
		}
		_, typ, err := wire.PeekType(dgram)
		if err != nil {
			continue
		}
		switch typ {
		case wire.TypeDecoderConfig:
			if cfg, err := wire.ParseDecoderConfig(dgram); err == nil && cfg.Codec == newCodec {
				gotCodec = cfg.Codec
			}
		case wire.TypeVideoChunk:
			// Chunks from session 1 (frameID 7) may still be in flight
			// early on — only the new session's keyframe counts.
			if hdr, _, err := wire.ParseVideoChunk(dgram); err == nil && hdr.Keyframe && hdr.FrameID == 0 {
				newChunks[hdr.ChunkIndex] = true
			}
		}
	}
}

// TestIdleSubscriberTimesOutWithoutKeepalive is the negative control for
// the test above: with the keepalive disabled, an idle subscriber session
// hits the QUIC idle timeout and is dropped.
func TestIdleSubscriberTimesOutWithoutKeepalive(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, h, _ := startTestServerCfg(t, ctx, config.Config{
		MaxSubscribers:  15,
		MaxIdleTimeout:  time.Second,
		KeepAlivePeriod: 0,
	})

	sub := dial(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/subscribe", port), clientTLS)
	_ = sub
	waitFor(t, 5*time.Second, func() bool { return h.Stats().Subscribers == 1 }, "subscriber registered")
	waitFor(t, 5*time.Second, func() bool { return h.Stats().Subscribers == 0 }, "idle subscriber timed out")
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
