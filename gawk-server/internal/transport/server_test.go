package transport

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/internal/metrics"
	"github.com/Tuhis/gawk/gawk-server/internal/tlsutil"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

var discardLog = slog.New(slog.NewTextHandler(io.Discard, nil))

func startTestServer(t *testing.T, ctx context.Context, maxSubs int) (port int, clientTLS *tls.Config, r *hub.Registry, done chan error) {
	t.Helper()
	return startTestServerCfgLog(t, ctx, config.Config{
		MaxSubscribers:  maxSubs,
		MaxIdleTimeout:  30 * time.Second,
		KeepAlivePeriod: 10 * time.Second,
		BroadcastGrace:  5 * time.Minute,
	}, discardLog)
}

func startTestServerCfg(t *testing.T, ctx context.Context, cfg config.Config) (port int, clientTLS *tls.Config, r *hub.Registry, done chan error) {
	t.Helper()
	if cfg.BroadcastGrace <= 0 {
		cfg.BroadcastGrace = 5 * time.Minute
	}
	return startTestServerCfgLog(t, ctx, cfg, discardLog)
}

func startTestServerCfgLog(t *testing.T, ctx context.Context, cfg config.Config, log *slog.Logger) (port int, clientTLS *tls.Config, r *hub.Registry, done chan error) {
	t.Helper()
	port, clientTLS, r, done, _ = startTestServerCfgLogSrv(t, ctx, cfg, log)
	return
}

// startTestServerCfgLogSrv additionally returns the *Server so tests can set
// its unexported test hooks.
func startTestServerCfgLogSrv(t *testing.T, ctx context.Context, cfg config.Config, log *slog.Logger) (port int, clientTLS *tls.Config, r *hub.Registry, done chan error, srv *Server) {
	t.Helper()

	cert, err := tlsutil.GenerateDevCert([]string{"localhost", "127.0.0.1"}, time.Hour)
	if err != nil {
		t.Fatalf("GenerateDevCert: %v", err)
	}

	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port = pc.LocalAddr().(*net.UDPAddr).Port
	pc.Close()

	cfg.Addr = fmt.Sprintf("127.0.0.1:%d", port)
	r = hub.NewRegistry(log, hub.Options{
		MaxSubscribers:       cfg.MaxSubscribers,
		BroadcastGrace:       cfg.BroadcastGrace,
		MaxBroadcasts:        cfg.MaxBroadcasts,
		MaxTotalSubscribers:  cfg.MaxTotalSubscribers,
		MaxBandwidthBytes:    cfg.MaxBandwidthBytes,
		MaxKeyframeBytes:     cfg.MaxKeyframeBytes,
		KeyframeWriteTimeout: cfg.KeyframeWriteTimeout,
	})
	srv = New(cfg, r, func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &cert, nil }, log,
		metrics.NewServerMetrics(prometheus.NewRegistry()))

	done = make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()

	pool := x509.NewCertPool()
	pool.AddCert(cert.Leaf)
	clientTLS = &tls.Config{RootCAs: pool, ServerName: "localhost", NextProtos: []string{http3.NextProtoH3}}
	return port, clientTLS, r, done, srv
}

func dial(t *testing.T, ctx context.Context, url string, clientTLS *tls.Config) *webtransport.Session {
	t.Helper()
	return dialWithOrigin(t, ctx, url, clientTLS, "")
}

// dialWithOrigin is dial with an explicit Origin header — for tests running
// the production config shape (AllowedOrigins set + the loopback origin
// bypass hook-disabled), where a browser-shaped client must present a
// listed origin.
func dialWithOrigin(t *testing.T, ctx context.Context, url string, clientTLS *tls.Config, origin string) *webtransport.Session {
	t.Helper()
	d := webtransport.Dialer{
		TLSClientConfig: clientTLS,
		QUICConfig:      &quic.Config{EnableDatagrams: true, EnableStreamResetPartialDelivery: true},
	}
	t.Cleanup(func() { d.Close() })

	var hdr http.Header
	if origin != "" {
		hdr = http.Header{"Origin": []string{origin}}
	}
	var sess *webtransport.Session
	var err error
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, sess, err = d.Dial(ctx, url, hdr)
		if err == nil {
			return sess
		}
		if time.Now().After(deadline) {
			t.Fatalf("Dial %s: %v", url, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func dialOnce(t *testing.T, ctx context.Context, url string, clientTLS *tls.Config) (*http.Response, *webtransport.Session, error) {
	t.Helper()
	d := webtransport.Dialer{
		TLSClientConfig: clientTLS,
		QUICConfig:      &quic.Config{EnableDatagrams: true, EnableStreamResetPartialDelivery: true},
	}
	t.Cleanup(func() { d.Close() })
	return d.Dial(ctx, url, nil)
}

// dialPublisherHandshake mints a broadcast and returns the session, its ID
// and the resume token (R17 W2: announce + token arrive on two uni streams
// in unspecified order — readPublisherHandshake dispatches by type).
func dialPublisherHandshake(t *testing.T, ctx context.Context, port int, clientTLS *tls.Config) (*webtransport.Session, string, string) {
	t.Helper()
	url := fmt.Sprintf("https://127.0.0.1:%d/publish", port)
	pub := dial(t, ctx, url, clientTLS)
	id, tokenHex := readPublisherHandshake(t, ctx, pub)
	return pub, id, tokenHex
}

func dialPublisherAndGetID(t *testing.T, ctx context.Context, port int, clientTLS *tls.Config) (*webtransport.Session, string) {
	t.Helper()
	pub, id, _ := dialPublisherHandshake(t, ctx, port, clientTLS)
	return pub, id
}

func dialPublisherReclaim(t *testing.T, ctx context.Context, port int, id, tokenHex string, clientTLS *tls.Config) *webtransport.Session {
	t.Helper()
	url := fmt.Sprintf("https://127.0.0.1:%d/publish/%s?resume=%s", port, id, tokenHex)
	pub := dial(t, ctx, url, clientTLS)
	gotID, _ := readPublisherHandshake(t, ctx, pub)
	if gotID != id {
		pub.CloseWithError(0, "")
		t.Fatalf("reclaim announce got ID %q, want %q", gotID, id)
	}
	return pub
}

func dialSubscriber(t *testing.T, ctx context.Context, port int, id string, clientTLS *tls.Config) *webtransport.Session {
	t.Helper()
	url := fmt.Sprintf("https://127.0.0.1:%d/subscribe/%s", port, id)
	return dial(t, ctx, url, clientTLS)
}

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

func encodeFrame(t *testing.T, frameID uint32, keyframe bool, chunkCount int) [][]byte {
	t.Helper()
	chunks := make([][]byte, 0, chunkCount)
	for i := range chunkCount {
		payload := bytes.Repeat([]byte{byte(frameID), byte(i)}, 300)
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

// buildStreamKeyframe assembles a full StreamFrame message (header + embedded
// config + payload) as the broadcaster writes it to a uni stream (R8).
func buildStreamKeyframe(t *testing.T, frameID uint32, codec string, payloadLen int) []byte {
	t.Helper()
	var config []byte
	if codec != "" {
		var err error
		config, err = wire.AppendDecoderConfig(nil, wire.DecoderConfig{
			Codec:     codec,
			Extradata: []byte{0x01, 0x42, 0xE0, 0x2A},
		})
		if err != nil {
			t.Fatalf("AppendDecoderConfig: %v", err)
		}
	}
	payload := bytes.Repeat([]byte{byte(frameID)}, payloadLen)
	hdr := wire.StreamFrameHeader{
		Keyframe:    true,
		FrameID:     frameID,
		TimestampUs: uint64(frameID) * 16_667,
		ConfigLen:   uint32(len(config)),
		PayloadLen:  uint32(len(payload)),
	}
	msg, err := wire.AppendStreamFrameHeader(nil, hdr)
	if err != nil {
		t.Fatalf("AppendStreamFrameHeader: %v", err)
	}
	msg = append(msg, config...)
	msg = append(msg, payload...)
	return msg
}

// sendKeyframeStream opens a publisher-initiated uni stream and writes one
// keyframe message to it, exactly as the broadcaster does.
func sendKeyframeStream(t *testing.T, pub *webtransport.Session, msg []byte) {
	t.Helper()
	str, err := pub.OpenUniStream()
	if err != nil {
		t.Fatalf("OpenUniStream: %v", err)
	}
	if _, err := str.Write(msg); err != nil {
		t.Fatalf("write keyframe stream: %v", err)
	}
	if err := str.Close(); err != nil {
		t.Fatalf("close keyframe stream: %v", err)
	}
}

// readNextKeyframeStream accepts one server-initiated uni stream on the
// subscriber and returns the parsed keyframe header plus the raw message.
func readNextKeyframeStream(t *testing.T, ctx context.Context, sub *webtransport.Session) (wire.StreamFrameHeader, []byte) {
	t.Helper()
	str, err := sub.AcceptUniStream(ctx)
	if err != nil {
		t.Fatalf("AcceptUniStream (keyframe): %v", err)
	}
	data, err := io.ReadAll(str)
	if err != nil {
		t.Fatalf("read keyframe stream: %v", err)
	}
	hdr, err := wire.ParseStreamFrameHeader(data)
	if err != nil {
		t.Fatalf("ParseStreamFrameHeader: %v", err)
	}
	return hdr, data
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

func TestRelayPublishToSubscribe(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, r, _, srv := startTestServerCfgLogSrv(t, ctx, config.Config{
		MaxSubscribers:  15,
		MaxIdleTimeout:  30 * time.Second,
		KeepAlivePeriod: 10 * time.Second,
		BroadcastGrace:  5 * time.Minute,
	}, discardLog)

	pub, id := dialPublisherAndGetID(t, ctx, port, clientTLS)
	sub := dialSubscriber(t, ctx, port, id, clientTLS)

	waitFor(t, 5*time.Second, func() bool { return r.Stats().Totals.Subscribers == 1 }, "subscriber registered")

	const totalFrames = 40
	const chunksPerFrame = 3

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
		time.Sleep(2 * time.Millisecond)
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

	// R9 M4: both sessions were accepted, and each accepted session counts
	// exactly once.
	if got := srv.metrics.ConnectionCount("publish", metrics.OutcomeAccepted); got != 1 {
		t.Errorf("publish/accepted = %v, want 1", got)
	}
	if got := srv.metrics.ConnectionCount("subscribe", metrics.OutcomeAccepted); got != 1 {
		t.Errorf("subscribe/accepted = %v, want 1", got)
	}
}

// The zombie-publisher lockout (docs/06 revision 2026-07-18): the field logs
// showed reclaims 409ing against the broadcaster's own silently-dead previous
// session — still holding the slot inside the QUIC idle window — which forced
// the client into a mint fallback that orphaned every viewer (the old
// broadcast was GC'd mid-stream). A reclaim that completes its upgrade must
// instead depose the incumbent session: newest publisher wins.
func TestReclaimSupersedesActivePublisher(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, r, _ := startTestServer(t, ctx, 15)

	first, id, token := dialPublisherHandshake(t, ctx, port, clientTLS)
	waitFor(t, 5*time.Second, func() bool {
		return r.Stats().Broadcasts[r.ObfuscateID(id)].PublisherActive
	}, "first publisher registered")

	sub := dialSubscriber(t, ctx, port, id, clientTLS)
	defer sub.CloseWithError(0, "")
	waitFor(t, 5*time.Second, func() bool { return r.Stats().Totals.Subscribers == 1 }, "subscriber registered")

	// Token-bearing reclaim while the first session still holds the slot (the
	// zombie shape: the relay cannot tell it apart from a live one).
	second := dialPublisherReclaim(t, ctx, port, id, token, clientTLS)
	defer second.CloseWithError(0, "")

	// The first session is kicked with the superseded close code.
	actx, acancel := context.WithTimeout(ctx, 5*time.Second)
	defer acancel()
	_, err := first.AcceptUniStream(actx)
	if err == nil {
		t.Fatal("first publisher session still alive after takeover, want superseded close")
	}
	var serr *webtransport.SessionError
	if !errors.As(err, &serr) || !serr.Remote || serr.ErrorCode != webtransport.SessionErrorCode(wire.CloseCodePublisherSuperseded) {
		t.Fatalf("first publisher close = %v, want remote session error %d", err, wire.CloseCodePublisherSuperseded)
	}

	// The new session owns a working slot: its frames reach the subscriber
	// that was attached before the takeover.
	before := r.Stats().Broadcasts[r.ObfuscateID(id)].FramesRelayed
	for _, d := range encodeFrame(t, 1, false, 1) {
		if err := second.SendDatagram(d); err != nil {
			t.Fatalf("SendDatagram from reclaimed session: %v", err)
		}
	}
	waitFor(t, 5*time.Second, func() bool {
		return r.Stats().Broadcasts[r.ObfuscateID(id)].FramesRelayed > before
	}, "frame relayed from the reclaimed session")
	rctx, rcancel := context.WithTimeout(ctx, 5*time.Second)
	defer rcancel()
	if _, err := sub.ReceiveDatagram(rctx); err != nil {
		t.Fatalf("subscriber did not receive the reclaimed session's frame: %v", err)
	}
}

func TestPublisherDisconnectFreesSlot(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, r, _ := startTestServer(t, ctx, 15)

	first, id, token := dialPublisherHandshake(t, ctx, port, clientTLS)
	waitFor(t, 5*time.Second, func() bool { return r.Stats().Broadcasts[r.ObfuscateID(id)].PublisherActive }, "publisher registered")
	first.CloseWithError(0, "done")
	waitFor(t, 5*time.Second, func() bool { return !r.Stats().Broadcasts[r.ObfuscateID(id)].PublisherActive }, "publisher slot freed")

	second := dialPublisherReclaim(t, ctx, port, id, token, clientTLS)
	_ = second
}

func TestSubscriberLimitRejected(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, r, _ := startTestServer(t, ctx, 1)

	pub, id := dialPublisherAndGetID(t, ctx, port, clientTLS)
	_ = pub

	sub1 := dialSubscriber(t, ctx, port, id, clientTLS)
	_ = sub1
	waitFor(t, 5*time.Second, func() bool { return r.Stats().Totals.Subscribers == 1 }, "subscriber registered")

	url := fmt.Sprintf("https://127.0.0.1:%d/subscribe/%s", port, id)
	rsp, sess, err := dialOnce(t, ctx, url, clientTLS)
	if err == nil {
		sess.CloseWithError(0, "")
		t.Fatal("second subscriber dial succeeded, want 429 rejection")
	}
	if rsp == nil || rsp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second subscriber response = %v (err %v), want status 429", rsp, err)
	}
}

func TestLateJoinerPrimedOverNetwork(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, r, _ := startTestServer(t, ctx, 15)

	pub, id := dialPublisherAndGetID(t, ctx, port, clientTLS)
	// A multi-datagram-sized keyframe: the whole point of the stream path is
	// that it arrives intact regardless of size.
	kf := buildStreamKeyframe(t, 0, "avc1.42E02A", 5000)
	sendKeyframeStream(t, pub, kf)

	waitFor(t, 5*time.Second, func() bool {
		st := r.Stats().Broadcasts[r.ObfuscateID(id)]
		return st.HasConfig && st.CachedKeyframeBytes == len(kf) && st.KeyframeStreamsIn == 1
	}, "keyframe cached")

	sub := dialSubscriber(t, ctx, port, id, clientTLS)

	recvCtx, recvCancel := context.WithTimeout(ctx, 5*time.Second)
	defer recvCancel()
	hdr, data := readNextKeyframeStream(t, recvCtx, sub)
	if hdr.FrameID != 0 || !hdr.Keyframe || hdr.ConfigLen == 0 {
		t.Errorf("primed keyframe header = %+v, want frameID 0 keyframe with config", hdr)
	}
	if !bytes.Equal(data, kf) {
		t.Errorf("primed keyframe = %d bytes, want the %d-byte cached keyframe verbatim", len(data), len(kf))
	}
}

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

func TestStatuszReachableAndNumbersMove(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, r, _ := startTestServer(t, ctx, 15)
	url := fmt.Sprintf("https://127.0.0.1:%d/statusz", port)

	rsp, body := h3Get(t, ctx, clientTLS, url)
	if rsp.StatusCode != http.StatusOK {
		t.Fatalf("GET /statusz status = %d, want 200", rsp.StatusCode)
	}
	if ct := rsp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var before hub.RegistryStats
	if err := json.Unmarshal(body, &before); err != nil {
		t.Fatalf("unmarshal initial /statusz %q: %v", body, err)
	}
	if before.Totals.Broadcasts != 0 {
		t.Errorf("initial stats = %+v, want totals.broadcasts 0", before)
	}

	pub, id := dialPublisherAndGetID(t, ctx, port, clientTLS)
	sub := dialSubscriber(t, ctx, port, id, clientTLS)
	_ = sub
	waitFor(t, 5*time.Second, func() bool { return r.Stats().Totals.Subscribers == 1 }, "subscriber registered")

	kf := buildStreamKeyframe(t, 0, "avc1.42E02A", 2000)
	sendKeyframeStream(t, pub, kf)
	for _, chunk := range encodeFrame(t, 1, false, 1) {
		if err := pub.SendDatagram(chunk); err != nil {
			t.Fatalf("send delta chunk: %v", err)
		}
	}
	waitFor(t, 5*time.Second, func() bool {
		st := r.Stats().Broadcasts[r.ObfuscateID(id)]
		return st.HasConfig && st.KeyframeStreamsIn == 1 && st.FramesRelayed >= 2
	}, "stream observed by registry")

	_, body = h3Get(t, ctx, clientTLS, url)
	var after hub.RegistryStats
	if err := json.Unmarshal(body, &after); err != nil {
		t.Fatalf("unmarshal /statusz %q: %v", body, err)
	}
	if after.Totals.Broadcasts != 1 {
		t.Errorf("totals.broadcasts = %d, want 1", after.Totals.Broadcasts)
	}
	if after.Totals.Subscribers != 1 {
		t.Errorf("totals.subscribers = %d, want 1", after.Totals.Subscribers)
	}
	// R2: /statusz must not leak joinable broadcast IDs — only obfuscated keys.
	if _, ok := after.Broadcasts[id]; ok {
		t.Error("statusz leaked the raw broadcast ID as a key")
	}
	bst := after.Broadcasts[r.ObfuscateID(id)]
	switch {
	case !bst.PublisherActive:
		t.Error("statusz publisherActive = false, want true")
	case bst.Subscribers != 1:
		t.Errorf("statusz subscribers = %d, want 1", bst.Subscribers)
	case bst.FramesRelayed < 2 || bst.DatagramsRelayed < 1:
		t.Errorf("statusz counters did not move: %+v", bst)
	case bst.KeyframeStreamsIn != 1 || bst.KeyframeBytesIn != uint64(len(kf)):
		t.Errorf("statusz keyframe-stream counters wrong: %+v", bst)
	case !bst.HasConfig || bst.CachedKeyframeID != 0 || bst.CachedKeyframeBytes != len(kf):
		t.Errorf("statusz cache fields wrong: %+v", bst)
	}
}

func TestPublisherRestartPrimesWithNewConfig(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, r, _ := startTestServer(t, ctx, 15)

	pub1, id, token := dialPublisherHandshake(t, ctx, port, clientTLS)
	sendKeyframeStream(t, pub1, buildStreamKeyframe(t, 7, "avc1.42E02A", 1000))
	waitFor(t, 5*time.Second, func() bool {
		st := r.Stats().Broadcasts[r.ObfuscateID(id)]
		return st.HasConfig && st.CachedKeyframeID == 7
	}, "session 1 keyframe cached")

	pub1.CloseWithError(0, "restart")
	waitFor(t, 5*time.Second, func() bool { return !r.Stats().Broadcasts[r.ObfuscateID(id)].PublisherActive }, "publisher slot freed")
	if st := r.Stats().Broadcasts[r.ObfuscateID(id)]; !st.HasConfig {
		t.Fatal("caches must persist while the broadcaster is away")
	}

	pub2 := dialPublisherReclaim(t, ctx, port, id, token, clientTLS)
	waitFor(t, 5*time.Second, func() bool {
		st := r.Stats().Broadcasts[r.ObfuscateID(id)]
		return st.PublisherActive && !st.HasConfig && st.CachedKeyframeBytes == 0
	}, "caches invalidated by new publisher session")

	const newCodec = "vp09.00.40.08"
	newKf := buildStreamKeyframe(t, 0, newCodec, 1500)
	sendKeyframeStream(t, pub2, newKf)
	waitFor(t, 5*time.Second, func() bool {
		st := r.Stats().Broadcasts[r.ObfuscateID(id)]
		return st.HasConfig && st.CachedKeyframeID == 0 && st.CachedKeyframeBytes == len(newKf)
	}, "session 2 keyframe cached")

	sub := dialSubscriber(t, ctx, port, id, clientTLS)
	recvCtx, recvCancel := context.WithTimeout(ctx, 5*time.Second)
	defer recvCancel()
	hdr, data := readNextKeyframeStream(t, recvCtx, sub)
	if hdr.FrameID == 7 {
		t.Fatal("primed with the keyframe from the previous publisher session")
	}
	if hdr.FrameID != 0 || !bytes.Equal(data, newKf) {
		t.Fatalf("primed keyframe = frameID %d (%d bytes), want the new session's frame 0", hdr.FrameID, len(data))
	}
	cfg, err := wire.ParseDecoderConfig(data[wire.StreamFrameHeaderSize : wire.StreamFrameHeaderSize+int(hdr.ConfigLen)])
	if err != nil {
		t.Fatalf("ParseDecoderConfig from primed keyframe: %v", err)
	}
	if cfg.Codec != newCodec {
		t.Errorf("primed config codec = %q, want %q", cfg.Codec, newCodec)
	}
}

func TestSubscriberSurvivesPublisherRestart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, r, _ := startTestServerCfg(t, ctx, config.Config{
		MaxSubscribers:  15,
		MaxIdleTimeout:  2 * time.Second,
		KeepAlivePeriod: 250 * time.Millisecond,
	})

	pub1, id, token := dialPublisherHandshake(t, ctx, port, clientTLS)
	sub := dialSubscriber(t, ctx, port, id, clientTLS)
	waitFor(t, 5*time.Second, func() bool { return r.Stats().Totals.Subscribers == 1 }, "subscriber registered")

	// Session 1: a live keyframe is fanned out to the connected subscriber over
	// a server-initiated uni stream.
	sendKeyframeStream(t, pub1, buildStreamKeyframe(t, 7, "avc1.42E02A", 800))
	recvCtx, recvCancel := context.WithTimeout(ctx, 5*time.Second)
	if hdr, _, _ := readNextKeyframeStreamNoFatal(recvCtx, sub); hdr.FrameID != 7 {
		recvCancel()
		t.Fatalf("session 1 keyframe = frameID %d, want 7", hdr.FrameID)
	}
	recvCancel()

	pub1.CloseWithError(0, "broadcaster gone")
	waitFor(t, 5*time.Second, func() bool { return !r.Stats().Broadcasts[r.ObfuscateID(id)].PublisherActive }, "publisher slot freed")
	time.Sleep(3 * time.Second)
	if got := r.Stats().Totals.Subscribers; got != 1 {
		t.Fatalf("subscribers after idle gap = %d, want 1 (session idled out?)", got)
	}
	select {
	case <-sub.Context().Done():
		t.Fatal("subscriber session closed during the broadcaster-away gap")
	default:
	}

	pub2 := dialPublisherReclaim(t, ctx, port, id, token, clientTLS)
	const newCodec = "vp09.00.40.08"
	newKf := buildStreamKeyframe(t, 0, newCodec, 1200)
	sendKeyframeStream(t, pub2, newKf)

	recvCtx, recvCancel = context.WithTimeout(ctx, 5*time.Second)
	defer recvCancel()
	hdr, data := readNextKeyframeStream(t, recvCtx, sub)
	if hdr.FrameID != 0 {
		t.Fatalf("resumed keyframe = frameID %d, want 0", hdr.FrameID)
	}
	cfg, err := wire.ParseDecoderConfig(data[wire.StreamFrameHeaderSize : wire.StreamFrameHeaderSize+int(hdr.ConfigLen)])
	if err != nil {
		t.Fatalf("ParseDecoderConfig from resumed keyframe: %v", err)
	}
	if cfg.Codec != newCodec {
		t.Errorf("resumed keyframe codec = %q, want %q", cfg.Codec, newCodec)
	}
}

// readNextKeyframeStreamNoFatal is readNextKeyframeStream without t.Fatalf, for
// call sites that must clean up (cancel a context) before failing.
func readNextKeyframeStreamNoFatal(ctx context.Context, sub *webtransport.Session) (wire.StreamFrameHeader, []byte, error) {
	str, err := sub.AcceptUniStream(ctx)
	if err != nil {
		return wire.StreamFrameHeader{}, nil, err
	}
	data, err := io.ReadAll(str)
	if err != nil {
		return wire.StreamFrameHeader{}, nil, err
	}
	hdr, err := wire.ParseStreamFrameHeader(data)
	return hdr, data, err
}

func TestIdleSubscriberTimesOutWithoutKeepalive(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, r, _ := startTestServerCfg(t, ctx, config.Config{
		MaxSubscribers:  15,
		MaxIdleTimeout:  time.Second,
		KeepAlivePeriod: 0,
	})

	pub, id := dialPublisherAndGetID(t, ctx, port, clientTLS)
	_ = pub

	sub := dialSubscriber(t, ctx, port, id, clientTLS)
	_ = sub
	waitFor(t, 5*time.Second, func() bool { return r.Stats().Totals.Subscribers == 1 }, "subscriber registered")
	waitFor(t, 5*time.Second, func() bool { return r.Stats().Totals.Subscribers == 0 }, "idle subscriber timed out")
}

func TestCheckOriginLoopbackBypassesAllowlist(t *testing.T) {
	r := hub.NewRegistry(discardLog, hub.Options{MaxSubscribers: 1})
	srv := New(config.Config{MaxSubscribers: 1, AllowedOrigins: []string{"https://gawk.example.com"}},
		r, func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return nil, nil }, discardLog, nil)

	cases := []struct {
		name       string
		remoteAddr string
		origin     string
		want       bool
	}{
		{"loopback probe, no origin header", "127.0.0.1:53211", "", true},
		{"loopback IPv6, no origin header", "[::1]:53211", "", true},
		{"remote client, allowed origin", "10.0.0.5:53211", "https://gawk.example.com", true},
		{"remote client, disallowed origin", "10.0.0.5:53211", "https://evil.example.com", false},
		{"remote client, no origin header", "10.0.0.5:53211", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodConnect, "https://gawk.example.com/echo", nil)
			req.RemoteAddr = tc.remoteAddr
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			if got := srv.wt.CheckOrigin(req); got != tc.want {
				t.Errorf("CheckOrigin(remote=%s, origin=%q) = %v, want %v", tc.remoteAddr, tc.origin, got, tc.want)
			}
		})
	}
}

// R17 W4 + docs/22 finding 12: pod-to-pod edge pulls announce
// internalEdgeOrigin, and no deployment should have to whitelist it — but
// it must buy nothing outside the PSK-gated /internal/* routes, and the
// origin check stays live on those routes: a wrong (or missing — the
// pre-0.16.2 field bug) origin is still rejected and logged.
func TestCheckOriginInternalEdgeOrigin(t *testing.T) {
	r := hub.NewRegistry(discardLog, hub.Options{MaxSubscribers: 1})
	srv := New(config.Config{MaxSubscribers: 1, AllowedOrigins: []string{"https://gawk.example.com"}},
		r, func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return nil, nil }, discardLog, nil)

	cases := []struct {
		name   string
		path   string
		origin string
		want   bool
	}{
		{"edge origin on internal route", "/internal/subscribe/NSTMWB", internalEdgeOrigin, true},
		{"edge origin buys nothing on a public route", "/subscribe/NSTMWB", internalEdgeOrigin, false},
		{"wrong origin on internal route still rejected", "/internal/subscribe/NSTMWB", "https://evil.example.com", false},
		{"no origin on internal route (the pre-0.16.2 field bug)", "/internal/subscribe/NSTMWB", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Built by hand, not httptest.NewRequest: that helper parses a
			// CONNECT target in authority form and mangles URL.Path, while
			// http3's extended CONNECT populates it from :path — the same
			// URL.Path the "CONNECT /publish" mux patterns route on.
			u, err := url.Parse("https://relay.example" + tc.path + "?proto=1")
			if err != nil {
				t.Fatal(err)
			}
			req := &http.Request{Method: http.MethodConnect, URL: u, Header: http.Header{}, RemoteAddr: "10.11.2.91:33035"}
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			if got := srv.wt.CheckOrigin(req); got != tc.want {
				t.Errorf("CheckOrigin(path=%s, origin=%q) = %v, want %v", tc.path, tc.origin, got, tc.want)
			}
		})
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestQuietProbeLogsSuppressesLoopbackEchoSessions(t *testing.T) {
	run := func(t *testing.T, quiet bool) string {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		var buf syncBuffer
		log := slog.New(slog.NewTextHandler(&buf, nil))
		port, clientTLS, _, _ := startTestServerCfgLog(t, ctx, config.Config{
			MaxSubscribers: 15,
			QuietProbeLogs: quiet,
		}, log)

		sess := dial(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/echo", port), clientTLS)
		if err := sess.SendDatagram([]byte("ping")); err != nil {
			t.Fatalf("SendDatagram: %v", err)
		}
		recvCtx, recvCancel := context.WithTimeout(ctx, time.Second)
		defer recvCancel()
		if _, err := sess.ReceiveDatagram(recvCtx); err != nil {
			t.Fatalf("ReceiveDatagram: %v", err)
		}
		return buf.String()
	}

	if got := run(t, true); strings.Contains(got, "session started") {
		t.Errorf("QuietProbeLogs=true: loopback echo session logged, want silence:\n%s", got)
	}
	if got := run(t, false); !strings.Contains(got, "session started") {
		t.Errorf("QuietProbeLogs=false: loopback echo session not logged, want \"session started\":\n%s", got)
	}
}

// TestOriginRejectedIsLogged verifies that a disallowed Origin is not merely
// rejected silently — the rejection is logged with the offending Origin and
// the remote address so operators can see who was blocked and from where.
func TestOriginRejectedIsLogged(t *testing.T) {
	var buf syncBuffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	r := hub.NewRegistry(discardLog, hub.Options{MaxSubscribers: 1})
	sm := metrics.NewServerMetrics(prometheus.NewRegistry())
	srv := New(config.Config{MaxSubscribers: 1, AllowedOrigins: []string{"https://gawk.example.com"}},
		r, func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return nil, nil }, log, sm)

	// Disallowed origin from a real (non-loopback) client: rejected + logged.
	req := httptest.NewRequest(http.MethodConnect, "https://gawk.example.com/subscribe/abc", nil)
	req.RemoteAddr = "203.0.113.7:44001"
	req.Header.Set("Origin", "https://evil.example.com")
	if srv.wt.CheckOrigin(req) {
		t.Fatal("CheckOrigin allowed a disallowed origin")
	}
	if got := buf.String(); !strings.Contains(got, "origin rejected") ||
		!strings.Contains(got, "https://evil.example.com") || !strings.Contains(got, "203.0.113.7") {
		t.Errorf("expected origin-rejection log with the origin and remote, got:\n%s", got)
	}
	if got := sm.OriginRejectedCount(); got != 1 {
		t.Errorf("gawk_origin_rejected_total = %v, want 1", got)
	}

	// Allowed origin: accepted, and no rejection log emitted.
	var okBuf syncBuffer
	okLog := slog.New(slog.NewTextHandler(&okBuf, nil))
	okSrv := New(config.Config{MaxSubscribers: 1, AllowedOrigins: []string{"https://gawk.example.com"}},
		r, func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return nil, nil }, okLog, nil)
	okReq := httptest.NewRequest(http.MethodConnect, "https://gawk.example.com/subscribe/abc", nil)
	okReq.RemoteAddr = "203.0.113.7:44002"
	okReq.Header.Set("Origin", "https://gawk.example.com")
	if !okSrv.wt.CheckOrigin(okReq) {
		t.Fatal("CheckOrigin rejected an allowed origin")
	}
	if got := okBuf.String(); strings.Contains(got, "origin rejected") {
		t.Errorf("allowed origin should not log a rejection, got:\n%s", got)
	}
}

// TestRateLimitBlockLogsOrigin verifies a rate-limited connection attempt is
// logged with the request's origin and remote address rather than being
// dropped with a silent 429.
func TestRateLimitBlockLogsOrigin(t *testing.T) {
	var buf syncBuffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	r := hub.NewRegistry(discardLog, hub.Options{MaxSubscribers: 1})
	sm := metrics.NewServerMetrics(prometheus.NewRegistry())
	srv := New(config.Config{MaxSubscribers: 1, ConnRateLimit: 1, ConnBurstLimit: 1},
		r, func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return nil, nil }, log, sm)
	// Test requests come from 127.0.0.1-style addrs; disable the loopback
	// bypass so the limiter actually engages (as production does off-pod).
	srv.testHookRateLimitLoopback.Store(true)

	req := httptest.NewRequest(http.MethodConnect, "https://gawk.example.com/subscribe/abc", nil)
	req.RemoteAddr = "203.0.113.9:5000"
	req.Header.Set("Origin", "https://app.example.com")

	if srv.rateLimited(req) {
		t.Fatal("first attempt rate-limited, want allowed (burst=1)")
	}
	if !srv.rateLimited(req) {
		t.Fatal("second attempt allowed, want rate-limited")
	}
	if got := buf.String(); !strings.Contains(got, "connection rate limited") ||
		!strings.Contains(got, "https://app.example.com") || !strings.Contains(got, "203.0.113.9") {
		t.Errorf("expected rate-limit log with the origin and remote, got:\n%s", got)
	}
	if got := sm.RateLimitedCount(); got != 1 {
		t.Errorf("gawk_rate_limited_total = %v, want 1", got)
	}
}

func TestGracefulShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	port, clientTLS, _, done := startTestServer(t, ctx, 15)

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

func TestE4Specifics(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// short grace of 100ms
	port, clientTLS, r, _ := startTestServerCfg(t, ctx, config.Config{
		MaxSubscribers: 15,
		BroadcastGrace: 100 * time.Millisecond,
	})

	// 1. ID-less subscribe -> 404
	subURLNoID := fmt.Sprintf("https://127.0.0.1:%d/subscribe", port)
	rsp, sess, err := dialOnce(t, ctx, subURLNoID, clientTLS)
	if err == nil {
		sess.CloseWithError(0, "")
		t.Fatal("ID-less subscribe dial succeeded, want 404")
	}
	if rsp == nil || rsp.StatusCode != http.StatusNotFound {
		t.Fatalf("ID-less subscribe status = %v (err %v), want 404", rsp, err)
	}

	// 2. Subscribe with trailing slash but no ID -> 404
	subURLSlash := fmt.Sprintf("https://127.0.0.1:%d/subscribe/", port)
	rsp, sess, err = dialOnce(t, ctx, subURLSlash, clientTLS)
	if err == nil {
		sess.CloseWithError(0, "")
		t.Fatal("subscribe trailing slash dial succeeded, want 404")
	}
	if rsp == nil || rsp.StatusCode != http.StatusNotFound {
		t.Fatalf("subscribe trailing slash status = %v (err %v), want 404", rsp, err)
	}

	// 3. ID-less publish (mint) -> success and returns ID
	pubSess, id := dialPublisherAndGetID(t, ctx, port, clientTLS)
	if len(id) != 6 {
		pubSess.CloseWithError(0, "")
		t.Fatalf("expected 6 char ID from publish mint, got %q", id)
	}

	// 4. Subscribe to bogus ID -> 404
	bogusSubURL := fmt.Sprintf("https://127.0.0.1:%d/subscribe/ZZZZZZ", port)
	rsp, sess, err = dialOnce(t, ctx, bogusSubURL, clientTLS)
	if err == nil {
		sess.CloseWithError(0, "")
		t.Fatal("bogus subscribe dial succeeded, want 404")
	}
	if rsp == nil || rsp.StatusCode != http.StatusNotFound {
		t.Fatalf("bogus subscribe status = %v (err %v), want 404", rsp, err)
	}

	// 4. GC (short grace) closes subscriber with code 4000, then sub is 404
	subSess := dialSubscriber(t, ctx, port, id, clientTLS)
	pubSess.CloseWithError(0, "")
	waitFor(t, 5*time.Second, func() bool { return !r.Stats().Broadcasts[r.ObfuscateID(id)].PublisherActive }, "publisher inactive")

	// Wait for grace timeout (100ms) to GC the broadcast
	time.Sleep(200 * time.Millisecond)

	// Subscriber session must have been closed
	recvCtx, recvCancel := context.WithTimeout(ctx, 2*time.Second)
	defer recvCancel()
	_, err = subSess.AcceptStream(recvCtx)
	if err == nil {
		t.Fatal("expected subscriber session to be closed by GC, but got no error")
	}

	var se *webtransport.SessionError
	if !errors.As(err, &se) {
		t.Fatalf("expected webtransport.SessionError, got %v", err)
	}
	if se.ErrorCode != webtransport.SessionErrorCode(wire.CloseCodeBroadcastEnded) {
		t.Errorf("expected close code %d, got %v", wire.CloseCodeBroadcastEnded, se.ErrorCode)
	}

	// Subscribing again to the expired ID must 404
	rsp, sess, err = dialOnce(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/subscribe/%s", port, id), clientTLS)
	if err == nil {
		sess.CloseWithError(0, "")
		t.Fatal("expired subscribe dial succeeded, want 404")
	}
	if rsp == nil || rsp.StatusCode != http.StatusNotFound {
		t.Fatalf("expired subscribe status = %v (err %v), want 404", rsp, err)
	}
}

// A subscriber that upgrades successfully but loses the race against GC
// (broadcast deleted between CheckSubscribe and Subscribe) must be closed
// with the terminal CloseCodeBroadcastEnded, not the 429 "full" code —
// otherwise the viewer burns its reconnect budget against a 404 instead of
// showing "broadcast ended". The server's test hook widens the race window
// deterministically.
func TestSubscribeLostRaceToGCClosesWithBroadcastEnded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	port, clientTLS, r, _, srv := startTestServerCfgLogSrv(t, ctx, config.Config{
		MaxSubscribers:  15,
		MaxIdleTimeout:  30 * time.Second,
		KeepAlivePeriod: 10 * time.Second,
		BroadcastGrace:  100 * time.Millisecond,
	}, discardLog)

	pubSess, id := dialPublisherAndGetID(t, ctx, port, clientTLS)

	// Between the subscriber's upgrade and its registry.Subscribe, kill the
	// publisher and wait for the grace timer to GC the broadcast. No t.Fatalf
	// here: this runs on the server's handler goroutine.
	hook := func(string) {
		pubSess.CloseWithError(0, "")
		deadline := time.Now().Add(5 * time.Second)
		for !errors.Is(r.CheckSubscribe(id), hub.ErrNotFound) && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
	}
	srv.testHookPostUpgradeSubscribe.Store(&hook)

	subSess := dialSubscriber(t, ctx, port, id, clientTLS)

	recvCtx, recvCancel := context.WithTimeout(ctx, 5*time.Second)
	defer recvCancel()
	_, err := subSess.AcceptStream(recvCtx)
	if err == nil {
		t.Fatal("expected subscriber session to be closed, but got no error")
	}
	var se *webtransport.SessionError
	if !errors.As(err, &se) {
		t.Fatalf("expected webtransport.SessionError, got %v", err)
	}
	if se.ErrorCode != webtransport.SessionErrorCode(wire.CloseCodeBroadcastEnded) {
		t.Errorf("close code = %v, want %d (broadcast ended)", se.ErrorCode, wire.CloseCodeBroadcastEnded)
	}
}

// E4: two concurrent publisher→subscriber pairs relay independently — no
// datagram or config cross-talk between broadcasts.
func TestTwoBroadcastsRelayIndependently(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, r, _ := startTestServer(t, ctx, 15)

	pubA, idA := dialPublisherAndGetID(t, ctx, port, clientTLS)
	pubB, idB := dialPublisherAndGetID(t, ctx, port, clientTLS)
	if idA == idB {
		t.Fatalf("both publishers got the same ID %q", idA)
	}
	subA := dialSubscriber(t, ctx, port, idA, clientTLS)
	subB := dialSubscriber(t, ctx, port, idB, clientTLS)
	waitFor(t, 5*time.Second, func() bool {
		st := r.Stats()
		return st.Broadcasts[r.ObfuscateID(idA)].Subscribers == 1 && st.Broadcasts[r.ObfuscateID(idB)].Subscribers == 1
	}, "both subscribers registered")

	// Disjoint frameID ranges and codecs per broadcast make cross-talk
	// detectable on both the chunk and the config path.
	const totalFrames = 20
	const chunksPerFrame = 2
	const baseA, baseB = uint32(100), uint32(200)

	type recvState struct {
		codecs    map[string]bool
		frames    map[uint32]map[uint16]bool
		badFrames []uint32
	}
	collect := func(sub *webtransport.Session, wantBase uint32) chan recvState {
		ch := make(chan recvState, 1)
		go func() {
			st := recvState{codecs: make(map[string]bool), frames: make(map[uint32]map[uint16]bool)}
			recvCtx, recvCancel := context.WithTimeout(ctx, 10*time.Second)
			defer recvCancel()
			defer func() { ch <- st }()
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
					if c, err := wire.ParseDecoderConfig(dgram); err == nil {
						st.codecs[c.Codec] = true
					}
				case wire.TypeVideoChunk:
					hdr, _, err := wire.ParseVideoChunk(dgram)
					if err != nil {
						continue
					}
					if hdr.FrameID < wantBase || hdr.FrameID >= wantBase+totalFrames {
						st.badFrames = append(st.badFrames, hdr.FrameID)
						continue
					}
					m := st.frames[hdr.FrameID]
					if m == nil {
						m = make(map[uint16]bool)
						st.frames[hdr.FrameID] = m
					}
					m[hdr.ChunkIndex] = true
				}
				complete := 0
				for _, m := range st.frames {
					if len(m) == chunksPerFrame {
						complete++
					}
				}
				if len(st.codecs) > 0 && complete == totalFrames {
					return
				}
			}
		}()
		return ch
	}
	chA := collect(subA, baseA)
	chB := collect(subB, baseB)

	if err := pubA.SendDatagram(testConfigDgram(t, "avc1.42E02A")); err != nil {
		t.Fatalf("send config A: %v", err)
	}
	if err := pubB.SendDatagram(testConfigDgram(t, "vp8")); err != nil {
		t.Fatalf("send config B: %v", err)
	}
	for i := uint32(0); i < totalFrames; i++ {
		for _, chunk := range encodeFrame(t, baseA+i, i == 0, chunksPerFrame) {
			if err := pubA.SendDatagram(chunk); err != nil {
				t.Fatalf("send A frame %d: %v", i, err)
			}
		}
		for _, chunk := range encodeFrame(t, baseB+i, i == 0, chunksPerFrame) {
			if err := pubB.SendDatagram(chunk); err != nil {
				t.Fatalf("send B frame %d: %v", i, err)
			}
		}
		time.Sleep(2 * time.Millisecond)
	}

	check := func(name string, st recvState, wantCodec string) {
		t.Helper()
		if len(st.badFrames) > 0 {
			t.Errorf("subscriber %s received %d frames from the other broadcast: %v", name, len(st.badFrames), st.badFrames)
		}
		if !st.codecs[wantCodec] || len(st.codecs) != 1 {
			t.Errorf("subscriber %s codecs = %v, want exactly {%q}", name, st.codecs, wantCodec)
		}
		complete := 0
		for _, m := range st.frames {
			if len(m) == chunksPerFrame {
				complete++
			}
		}
		if complete < totalFrames*95/100 {
			t.Errorf("subscriber %s reassembled %d/%d frames, want >= 95%%", name, complete, totalFrames)
		}
	}
	check("A", <-chA, "avc1.42E02A")
	check("B", <-chB, "vp8")
}

// E4: /statusz graceRemainingSeconds is 0 while the publisher is active and
// counts down after it disconnects.
func TestStatuszGraceRemainingMoves(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, r, _ := startTestServerCfg(t, ctx, config.Config{
		MaxSubscribers:  15,
		MaxIdleTimeout:  30 * time.Second,
		KeepAlivePeriod: 10 * time.Second,
		BroadcastGrace:  10 * time.Second,
	})
	url := fmt.Sprintf("https://127.0.0.1:%d/statusz", port)

	pub, id := dialPublisherAndGetID(t, ctx, port, clientTLS)

	fetch := func() hub.Stats {
		t.Helper()
		_, body := h3Get(t, ctx, clientTLS, url)
		var st hub.RegistryStats
		if err := json.Unmarshal(body, &st); err != nil {
			t.Fatalf("unmarshal /statusz %q: %v", body, err)
		}
		return st.Broadcasts[r.ObfuscateID(id)]
	}

	if g := fetch().GraceRemainingSeconds; g != 0 {
		t.Errorf("graceRemainingSeconds with active publisher = %d, want 0", g)
	}

	pub.CloseWithError(0, "")
	waitFor(t, 5*time.Second, func() bool { return !r.Stats().Broadcasts[r.ObfuscateID(id)].PublisherActive }, "publisher inactive")

	g1 := fetch().GraceRemainingSeconds
	if g1 <= 0 || g1 > 10 {
		t.Fatalf("graceRemainingSeconds after publisher close = %d, want in (0, 10]", g1)
	}
	time.Sleep(1500 * time.Millisecond)
	g2 := fetch().GraceRemainingSeconds
	if g2 <= 0 || g2 >= g1 {
		t.Errorf("graceRemainingSeconds did not count down: first %d, then %d", g1, g2)
	}
}

func TestPublishSecret(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	port, clientTLS, _, _ := startTestServerCfg(t, ctx, config.Config{
		PublishSecret:   "supersecret",
		MaxSubscribers:  15,
		MaxIdleTimeout:  30 * time.Second,
		KeepAlivePeriod: 10 * time.Second,
	})

	// 1. Dial without secret -> expect 401 Unauthorized
	urlNoSecret := fmt.Sprintf("https://127.0.0.1:%d/publish", port)
	rsp, sess, err := dialOnce(t, ctx, urlNoSecret, clientTLS)
	if err == nil {
		sess.CloseWithError(0, "")
		t.Fatal("publish without secret should have failed")
	}
	if rsp == nil || rsp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected status 401, got %v (err: %v)", rsp, err)
	}

	// 2. Dial with invalid secret -> expect 401 Unauthorized
	urlBadSecret := fmt.Sprintf("https://127.0.0.1:%d/publish?secret=wrong", port)
	rsp, sess, err = dialOnce(t, ctx, urlBadSecret, clientTLS)
	if err == nil {
		sess.CloseWithError(0, "")
		t.Fatal("publish with wrong secret should have failed")
	}
	if rsp == nil || rsp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected status 401, got %v (err: %v)", rsp, err)
	}

	// 3. Dial with correct secret -> expect success
	urlGoodSecret := fmt.Sprintf("https://127.0.0.1:%d/publish?secret=supersecret", port)
	pub := dial(t, ctx, urlGoodSecret, clientTLS)
	pub.CloseWithError(0, "")
}

func TestBandwidthLimitingE2E(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Limiter: 100 bytes/sec limit
	port, clientTLS, r, _ := startTestServerCfg(t, ctx, config.Config{
		MaxSubscribers:    15,
		MaxIdleTimeout:    30 * time.Second,
		KeepAlivePeriod:   10 * time.Second,
		MaxBandwidthBytes: 100, // 100 bytes/sec
	})

	pub, id := dialPublisherAndGetID(t, ctx, port, clientTLS)
	sub := dialSubscriber(t, ctx, port, id, clientTLS)
	defer pub.CloseWithError(0, "")
	defer sub.CloseWithError(0, "")

	// Send large datagrams (e.g. 200 bytes each) to subscriber.
	// Since limit is 100 bytes/sec, subsequent packets will exceed it and be dropped.
	largeDgram, err := wire.AppendVideoChunk(nil, wire.VideoChunkHeader{ChunkCount: 1}, make([]byte, 200))
	if err != nil {
		t.Fatal(err)
	}

	// Send multiple datagrams quickly
	for range 10 {
		if err := pub.SendDatagram(largeDgram); err != nil {
			t.Fatal(err)
		}
	}

	// Wait for stats to show bandwidth drops
	waitFor(t, 2*time.Second, func() bool {
		stats := r.Stats()
		bst := stats.Broadcasts[r.ObfuscateID(id)]
		return bst.BandwidthDroppedDatagrams > 0 && bst.BandwidthDroppedBytes > 0
	}, "bandwidth drops recorded")
}

// R2 review finding F6: at the broadcast limit, an ID-less /publish dial
// must be rejected pre-upgrade with HTTP 429 (symmetric with the subscribe
// path's CheckSubscribe), not accepted and then session-closed — the
// frontend's never-connected-fatal semantics depend on the dial failing.
func TestMintRejected429PreUpgradeAtBroadcastLimit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, _, _ := startTestServerCfg(t, ctx, config.Config{
		MaxSubscribers:  15,
		MaxIdleTimeout:  30 * time.Second,
		KeepAlivePeriod: 10 * time.Second,
		MaxBroadcasts:   1,
	})
	pub, _ := dialPublisherAndGetID(t, ctx, port, clientTLS)
	defer pub.CloseWithError(0, "")

	url := fmt.Sprintf("https://127.0.0.1:%d/publish", port)
	rsp, sess, err := dialOnce(t, ctx, url, clientTLS)
	if err == nil {
		sess.CloseWithError(0, "")
		t.Fatal("mint at broadcast limit should have failed")
	}
	if rsp == nil || rsp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected status 429 pre-upgrade, got %v (err: %v)", rsp, err)
	}
}

// Design-doc verification item (R2): rate-limited connection attempts are
// rejected with 429 pre-upgrade. Test dials come from 127.0.0.1, which the
// limiter bypasses in production, so the bypass is disabled via the test hook.
func TestConnRateLimit429OverNetwork(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	port, clientTLS, _, _, srv := startTestServerCfgLogSrv(t, ctx, config.Config{
		MaxSubscribers:  15,
		MaxIdleTimeout:  30 * time.Second,
		KeepAlivePeriod: 10 * time.Second,
		BroadcastGrace:  5 * time.Minute,
		ConnRateLimit:   0.001, // effectively no refill within the test
		ConnBurstLimit:  2,
	}, discardLog)
	srv.testHookRateLimitLoopback.Store(true)

	url := fmt.Sprintf("https://127.0.0.1:%d/echo", port)
	for i := range 2 {
		rsp, sess, err := dialOnce(t, ctx, url, clientTLS)
		if err != nil {
			t.Fatalf("dial %d within burst: %v (rsp %v)", i+1, err, rsp)
		}
		sess.CloseWithError(0, "")
	}
	rsp, sess, err := dialOnce(t, ctx, url, clientTLS)
	if err == nil {
		sess.CloseWithError(0, "")
		t.Fatal("third dial should have been rate limited")
	}
	if rsp == nil || rsp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected status 429, got %v (err: %v)", rsp, err)
	}
}

// Design-doc verification item (R2): loopback dials (k8s exec probes)
// bypass the connection rate limiter.
func TestConnRateLimitLoopbackBypass(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	port, clientTLS, _, _ := startTestServerCfg(t, ctx, config.Config{
		MaxSubscribers:  15,
		MaxIdleTimeout:  30 * time.Second,
		KeepAlivePeriod: 10 * time.Second,
		ConnRateLimit:   0.001,
		ConnBurstLimit:  2,
	})

	url := fmt.Sprintf("https://127.0.0.1:%d/echo", port)
	for i := range 5 { // well past the burst of 2
		rsp, sess, err := dialOnce(t, ctx, url, clientTLS)
		if err != nil {
			t.Fatalf("loopback dial %d should bypass the rate limit: %v (rsp %v)", i+1, err, rsp)
		}
		sess.CloseWithError(0, "")
	}
}

func TestIPRateLimiter(t *testing.T) {
	// Rate limit: 5 attempts per second, burst 2
	lim := newIPRateLimiter(5.0, 2)
	defer lim.Close()

	ip := "192.168.1.1:12345"

	// First two attempts should be allowed (burst = 2)
	if !lim.Allow(ip) {
		t.Error("expected first attempt to be allowed")
	}
	if !lim.Allow(ip) {
		t.Error("expected second attempt to be allowed")
	}
	// Third attempt should be rejected (limit exceeded)
	if lim.Allow(ip) {
		t.Error("expected third attempt to be rate limited")
	}

	// Different IP should be allowed
	if !lim.Allow("192.168.1.2:12345") {
		t.Error("expected different IP to be allowed")
	}

	// Wait for refill (200ms should refill 1 token at 5/sec)
	time.Sleep(250 * time.Millisecond)
	if !lim.Allow(ip) {
		t.Error("expected refilled attempt to be allowed")
	}
	if lim.Allow(ip) {
		t.Error("expected subsequent attempt to be rate limited again")
	}
}

// receiveTimeSyncReply reads datagrams until a TimeSync arrives, returning its
// parsed fields. Fails the test on timeout.
func receiveTimeSyncReply(t *testing.T, ctx context.Context, sess *webtransport.Session) (clientUs, serverUs uint64) {
	t.Helper()
	recvCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for {
		dgram, err := sess.ReceiveDatagram(recvCtx)
		if err != nil {
			t.Fatalf("ReceiveDatagram (awaiting time sync reply): %v", err)
		}
		if _, typ, err := wire.PeekType(dgram); err != nil || typ != wire.TypeTimeSync {
			continue
		}
		clientUs, serverUs, err := wire.ParseTimeSync(dgram)
		if err != nil {
			t.Fatalf("ParseTimeSync: %v", err)
		}
		return clientUs, serverUs
	}
}

// R5 Q2 (docs/15): both the publisher and subscriber sessions get TimeSync
// pings answered inline — echoed client time, relay monotonic server time.
func TestTimeSyncRepliesOnBothRoutes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, _, _ := startTestServer(t, ctx, 15)

	pub, id := dialPublisherAndGetID(t, ctx, port, clientTLS)
	sub := dialSubscriber(t, ctx, port, id, clientTLS)

	if err := sub.SendDatagram(wire.AppendTimeSync(nil, 42_000, 0)); err != nil {
		t.Fatalf("subscriber SendDatagram: %v", err)
	}
	clientUs, serverUs := receiveTimeSyncReply(t, ctx, sub)
	if clientUs != 42_000 {
		t.Errorf("subscriber reply clientTimeUs = %d, want 42000 (echo)", clientUs)
	}
	if serverUs == 0 {
		t.Errorf("subscriber reply serverTimeUs = 0, want relay monotonic time")
	}

	if err := pub.SendDatagram(wire.AppendTimeSync(nil, 7_000, 0)); err != nil {
		t.Fatalf("publisher SendDatagram: %v", err)
	}
	clientUs, serverUs = receiveTimeSyncReply(t, ctx, pub)
	if clientUs != 7_000 {
		t.Errorf("publisher reply clientTimeUs = %d, want 7000 (echo)", clientUs)
	}
	if serverUs == 0 {
		t.Errorf("publisher reply serverTimeUs = 0, want relay monotonic time")
	}
}

// A ping flood is answered at most at the reply limiter's rate — the excess is
// silently dropped, and the session stays healthy.
func TestTimeSyncReplyRateLimited(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, _, _ := startTestServer(t, ctx, 15)

	pub, id := dialPublisherAndGetID(t, ctx, port, clientTLS)
	defer pub.CloseWithError(0, "done")
	sub := dialSubscriber(t, ctx, port, id, clientTLS)

	const flood = 40
	for i := 0; i < flood; i++ {
		if err := sub.SendDatagram(wire.AppendTimeSync(nil, uint64(i+1), 0)); err != nil {
			t.Fatalf("SendDatagram %d: %v", i, err)
		}
	}

	replies := 0
	recvCtx, recvCancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer recvCancel()
	for {
		dgram, err := sub.ReceiveDatagram(recvCtx)
		if err != nil {
			break
		}
		if _, typ, err := wire.PeekType(dgram); err == nil && typ == wire.TypeTimeSync {
			replies++
		}
	}
	if replies == 0 {
		t.Fatalf("no time sync replies at all")
	}
	// Burst 5 + at most ~2.5 refills over the 500ms window; anything near the
	// flood size means the limiter is not engaged.
	if replies > 10 {
		t.Errorf("replies = %d for a %d-ping flood, want <= 10 (rate limited)", replies, flood)
	}
}

// R19 (docs/24): a subscriber dialing with ?delivery=reliable receives its
// deltas as length-prefixed records on carrier uni streams (discriminated
// from keyframe streams by the two-byte prologue) and no video datagrams;
// keyframe streams are untouched.
func TestSubscribeReliableDeliversCarrierRecords(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, r, _ := startTestServer(t, ctx, 15)

	pub, id := dialPublisherAndGetID(t, ctx, port, clientTLS)
	defer pub.CloseWithError(0, "")
	url := fmt.Sprintf("https://127.0.0.1:%d/subscribe/%s?delivery=reliable", port, id)
	sub := dial(t, ctx, url, clientTLS)
	defer sub.CloseWithError(0, "")

	waitFor(t, 5*time.Second, func() bool { return r.Stats().Totals.Subscribers == 1 }, "subscriber registered")

	// One GOP: a stream keyframe (which rotates the carrier), then deltas.
	sendKeyframeStream(t, pub, buildStreamKeyframe(t, 0, "avc1.42E02A", 512))
	delta := encodeFrame(t, 1, false, 1)[0]
	delta2 := encodeFrame(t, 2, false, 1)[0]

	// Accept server uni streams and dispatch by prologue type until both the
	// keyframe and the two deltas (as carrier records) have arrived. Every
	// read is bounded by `deadline`: deltas reach the relay as unreliable
	// datagrams, so a carrier can legitimately sit one record short with the
	// stream still open, and an unbounded Read then hangs the whole package
	// (CI run 29663246454 — runners cap UDP buffers below what quic-go asks
	// for and drop loopback datagrams under -race load).
	deadline := time.Now().Add(10 * time.Second)
	acceptCtx, acceptCancel := context.WithDeadline(ctx, deadline)
	defer acceptCancel()
	type result struct {
		keyframes int
		sawDelta  bool
		sawDelta2 bool
		err       error
	}
	resultCh := make(chan result, 1)
	go func() {
		var res result
		defer func() { resultCh <- res }()
		for res.keyframes < 1 || !res.sawDelta || !res.sawDelta2 {
			str, err := sub.AcceptUniStream(acceptCtx)
			if err != nil {
				res.err = err
				return
			}
			_ = str.SetReadDeadline(deadline)
			prologue := make([]byte, 2)
			if _, err := io.ReadFull(str, prologue); err != nil {
				res.err = err
				return
			}
			switch prologue[1] {
			case wire.TypeStreamFrame:
				// Keyframe stream: drain it (header already partially read).
				if _, err := io.ReadAll(str); err != nil {
					res.err = err
					return
				}
				res.keyframes++
			case wire.TypeReliableCarrier:
				if err := wire.ParseCarrierPrologue(prologue); err != nil {
					res.err = err
					return
				}
				// Read records off the live carrier until both deltas are in
				// hand (the stream stays open until the next rotation).
				// Resends may duplicate a record — the relay forwards
				// verbatim, so match by byte equality rather than position.
				buf := make([]byte, 0, 4096)
				tmp := make([]byte, 2048)
				for !res.sawDelta || !res.sawDelta2 {
					n, err := str.Read(tmp)
					if n > 0 {
						buf = append(buf, tmp[:n]...)
						for {
							record, rest, perr := wire.ParseCarrierRecord(buf)
							if perr != nil {
								break // incomplete — read more
							}
							switch {
							case bytes.Equal(record, delta):
								res.sawDelta = true
							case bytes.Equal(record, delta2):
								res.sawDelta2 = true
							default:
								res.err = fmt.Errorf("unexpected carrier record (%d bytes)", len(record))
								return
							}
							buf = append(buf[:0], rest...)
						}
					}
					if err != nil {
						break
					}
				}
			default:
				res.err = fmt.Errorf("unexpected stream type 0x%02x", prologue[1])
				return
			}
		}
	}()

	// No video datagrams may reach a reliable subscriber. TimeSync replies
	// are the only datagrams it could legitimately see; we send none.
	dgramCtx, dgramCancel := context.WithTimeout(ctx, 3*time.Second)
	defer dgramCancel()
	dgramCh := make(chan []byte, 1)
	go func() {
		if d, err := sub.ReceiveDatagram(dgramCtx); err == nil {
			dgramCh <- d
		}
	}()

	// Send the deltas, and resend until the reader has both — datagrams are
	// droppable even on loopback, and a drop must cost a resend, not the
	// package timeout.
	sendDeltas := func() {
		if err := pub.SendDatagram(delta); err != nil {
			t.Logf("SendDatagram delta: %v", err)
		}
		if err := pub.SendDatagram(delta2); err != nil {
			t.Logf("SendDatagram delta2: %v", err)
		}
	}
	sendDeltas()
	var res result
waitForRecords:
	for {
		select {
		case res = <-resultCh:
			break waitForRecords
		case <-time.After(250 * time.Millisecond):
			sendDeltas()
		}
	}
	if res.err != nil {
		t.Fatalf("subscriber stream read: %v", res.err)
	}
	if res.keyframes != 1 {
		t.Errorf("keyframe streams = %d, want 1", res.keyframes)
	}
	if !res.sawDelta || !res.sawDelta2 {
		t.Errorf("carrier records incomplete: sawDelta=%v sawDelta2=%v", res.sawDelta, res.sawDelta2)
	}
	// R21: every subscriber's first datagram is now the DeliveryAck (docs/26
	// Decision 7a). What this test cares about is that no *video* arrives as a
	// datagram on the reliable path, so skip the ack and fail on anything else.
	for {
		select {
		case d := <-dgramCh:
			if len(d) >= 2 && d[1] == wire.TypeDeliveryAck {
				mode, _, err := wire.ParseDeliveryAck(d)
				if err != nil {
					t.Errorf("malformed delivery ack: %v", err)
				} else if mode != wire.DeliveryReliable {
					t.Errorf("delivery ack says mode %d, want reliable", mode)
				}
				continue
			}
			t.Errorf("reliable subscriber received a datagram (type 0x%02x)", d[1])
		default:
		}
		break
	}

	stats := r.Stats().Broadcasts[r.ObfuscateID(id)]
	if stats.ReliableSubscribers != 1 {
		t.Errorf("ReliableSubscribers = %d, want 1", stats.ReliableSubscribers)
	}
	if stats.CarrierRecords < 2 {
		t.Errorf("CarrierRecords = %d, want >= 2", stats.CarrierRecords)
	}
}

// An unknown delivery value falls back to datagram delivery (docs/24
// Decision 6) — never an error, never reliable.
func TestSubscribeUnknownDeliveryFallsBackToDatagrams(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, r, _ := startTestServer(t, ctx, 15)

	pub, id := dialPublisherAndGetID(t, ctx, port, clientTLS)
	defer pub.CloseWithError(0, "")
	url := fmt.Sprintf("https://127.0.0.1:%d/subscribe/%s?delivery=carrier-pigeon", port, id)
	sub := dial(t, ctx, url, clientTLS)
	defer sub.CloseWithError(0, "")

	waitFor(t, 5*time.Second, func() bool { return r.Stats().Totals.Subscribers == 1 }, "subscriber registered")
	if n := r.Stats().Broadcasts[r.ObfuscateID(id)].ReliableSubscribers; n != 0 {
		t.Fatalf("ReliableSubscribers = %d, want 0", n)
	}

	delta := encodeFrame(t, 1, false, 1)[0]
	if err := pub.SendDatagram(delta); err != nil {
		t.Fatalf("SendDatagram: %v", err)
	}
	recvCtx, recvCancel := context.WithTimeout(ctx, 5*time.Second)
	defer recvCancel()
	// R21: the DeliveryAck leads (docs/26 Decision 7a) and reports the
	// fallback honestly — which is the whole point of the message here, since
	// an unknown ?delivery value must not silently look like a success.
	for {
		got, err := sub.ReceiveDatagram(recvCtx)
		if err != nil {
			t.Fatalf("ReceiveDatagram: %v", err)
		}
		if len(got) >= 2 && got[1] == wire.TypeDeliveryAck {
			mode, _, err := wire.ParseDeliveryAck(got)
			if err != nil {
				t.Fatalf("malformed delivery ack: %v", err)
			}
			if mode != wire.DeliveryDatagrams {
				t.Errorf("delivery ack says mode %d, want datagrams for an unknown ?delivery", mode)
			}
			continue
		}
		if !bytes.Equal(got, delta) {
			t.Errorf("datagram mismatch")
		}
		return
	}
}

// R21 (docs/26 Decision 7a). Two defects in one announcement, both found while
// chasing the R20 tier-1 deep-buffer pass that failed on 3 of 4 runs (and
// blocked a release PR) with "the ?buffer= negotiation did not take".
//
// First: the hub defaults a zero DVR window to DefaultDVRWindow when it builds
// the ring, but the negotiation used cfg.DVRWindow raw — so a server whose
// window was never set granted DeliveryDVR while clamping the buffer to 0. The
// viewer is then told it is ring-backed and deepens its playout to a depth the
// relay does not hold. A grant must be coherent or it must be a downgrade.
func TestNegotiateDeliveryNeverGrantsDVRWithoutABuffer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, r, _ := startTestServer(t, ctx, 15)

	pub, id := dialPublisherAndGetID(t, ctx, port, clientTLS)
	defer pub.CloseWithError(0, "")
	url := fmt.Sprintf("https://127.0.0.1:%d/subscribe/%s?delivery=reliable&buffer=3000", port, id)
	sub := dial(t, ctx, url, clientTLS)
	defer sub.CloseWithError(0, "")

	waitFor(t, 5*time.Second, func() bool { return r.Stats().Totals.Subscribers == 1 }, "subscriber registered")

	recvCtx, recvCancel := context.WithTimeout(ctx, 5*time.Second)
	defer recvCancel()
	for {
		got, err := sub.ReceiveDatagram(recvCtx)
		if err != nil {
			t.Fatalf("no DeliveryAck: %v", err)
		}
		if len(got) < 2 || got[1] != wire.TypeDeliveryAck {
			continue
		}
		mode, bufferMs, err := wire.ParseDeliveryAck(got)
		if err != nil {
			t.Fatalf("malformed delivery ack: %v", err)
		}
		if mode == wire.DeliveryDVR && bufferMs == 0 {
			t.Fatal("granted dvr with a 0 ms buffer: the viewer would deepen its playout to a depth the ring never holds")
		}
		return
	}
}

// Second: the ack was sent exactly once, at the instant the CONNECT was
// accepted — the moment a viewer is least likely to be draining its datagram
// queue — and it rides an unreliable datagram with no way to ask again (the
// one-way data flow is deliberate, docs/15 Decision 6). A single loss left the
// viewer naming the wrong mode for the whole session. It must be re-announced,
// the way the cached DecoderConfig and the 1 Hz audio config already are.
func TestDeliveryAckIsReAnnounced(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, r, _ := startTestServer(t, ctx, 15)

	pub, id := dialPublisherAndGetID(t, ctx, port, clientTLS)
	defer pub.CloseWithError(0, "")
	url := fmt.Sprintf("https://127.0.0.1:%d/subscribe/%s?delivery=reliable", port, id)
	sub := dial(t, ctx, url, clientTLS)
	defer sub.CloseWithError(0, "")

	waitFor(t, 5*time.Second, func() bool { return r.Stats().Totals.Subscribers == 1 }, "subscriber registered")

	recvCtx, recvCancel := context.WithTimeout(ctx, 3*time.Second)
	defer recvCancel()
	acks := 0
	for acks < 2 {
		got, err := sub.ReceiveDatagram(recvCtx)
		if err != nil {
			t.Fatalf("saw %d DeliveryAck(s) in the window, want the announcement repeated: %v", acks, err)
		}
		if len(got) >= 2 && got[1] == wire.TypeDeliveryAck {
			acks++
		}
	}
}
