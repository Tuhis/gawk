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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/internal/broadcastid"
	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/internal/tlsutil"
	"github.com/Tuhis/gawk/gawk-server/internal/wire"
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
		MaxSubscribers:      cfg.MaxSubscribers,
		BroadcastGrace:      cfg.BroadcastGrace,
		MaxBroadcasts:       cfg.MaxBroadcasts,
		MaxTotalSubscribers: cfg.MaxTotalSubscribers,
		MaxBandwidthBytes:   cfg.MaxBandwidthBytes,
	})
	srv = New(cfg, r, func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &cert, nil }, log)

	done = make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()

	pool := x509.NewCertPool()
	pool.AddCert(cert.Leaf)
	clientTLS = &tls.Config{RootCAs: pool, ServerName: "localhost", NextProtos: []string{http3.NextProtoH3}}
	return port, clientTLS, r, done, srv
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

func dialOnce(t *testing.T, ctx context.Context, url string, clientTLS *tls.Config) (*http.Response, *webtransport.Session, error) {
	t.Helper()
	d := webtransport.Dialer{
		TLSClientConfig: clientTLS,
		QUICConfig:      &quic.Config{EnableDatagrams: true, EnableStreamResetPartialDelivery: true},
	}
	t.Cleanup(func() { d.Close() })
	return d.Dial(ctx, url, nil)
}

func dialPublisherAndGetID(t *testing.T, ctx context.Context, port int, clientTLS *tls.Config) (*webtransport.Session, string) {
	t.Helper()
	url := fmt.Sprintf("https://127.0.0.1:%d/publish", port)
	pub := dial(t, ctx, url, clientTLS)
	str, err := pub.AcceptUniStream(ctx)
	if err != nil {
		pub.CloseWithError(0, "")
		t.Fatalf("AcceptUniStream failed: %v", err)
	}
	data, err := io.ReadAll(str)
	if err != nil {
		pub.CloseWithError(0, "")
		t.Fatalf("failed to read announce stream: %v", err)
	}
	id, err := wire.ParseBroadcastAnnounce(data)
	if err != nil {
		pub.CloseWithError(0, "")
		t.Fatalf("failed to parse announce: %v", err)
	}
	return pub, id
}

func dialPublisherReclaim(t *testing.T, ctx context.Context, port int, id string, clientTLS *tls.Config) *webtransport.Session {
	t.Helper()
	url := fmt.Sprintf("https://127.0.0.1:%d/publish/%s", port, id)
	pub := dial(t, ctx, url, clientTLS)
	str, err := pub.AcceptUniStream(ctx)
	if err != nil {
		pub.CloseWithError(0, "")
		t.Fatalf("AcceptUniStream reclaim failed: %v", err)
	}
	data, err := io.ReadAll(str)
	if err != nil {
		pub.CloseWithError(0, "")
		t.Fatalf("failed to read reclaim announce: %v", err)
	}
	gotID, err := wire.ParseBroadcastAnnounce(data)
	if err != nil {
		pub.CloseWithError(0, "")
		t.Fatalf("failed to parse reclaim announce: %v", err)
	}
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
	port, clientTLS, r, _ := startTestServer(t, ctx, 15)

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
}

func TestSecondPublisherConflict(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, _, _ := startTestServer(t, ctx, 15)

	first, id := dialPublisherAndGetID(t, ctx, port, clientTLS)
	_ = first

	reclaimURL := fmt.Sprintf("https://127.0.0.1:%d/publish/%s", port, id)
	rsp, sess, err := dialOnce(t, ctx, reclaimURL, clientTLS)
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
	port, clientTLS, r, _ := startTestServer(t, ctx, 15)

	first, id := dialPublisherAndGetID(t, ctx, port, clientTLS)
	waitFor(t, 5*time.Second, func() bool { return r.Stats().Broadcasts[broadcastid.Obfuscate(id)].PublisherActive }, "publisher registered")
	first.CloseWithError(0, "done")
	waitFor(t, 5*time.Second, func() bool { return !r.Stats().Broadcasts[broadcastid.Obfuscate(id)].PublisherActive }, "publisher slot freed")

	second := dialPublisherReclaim(t, ctx, port, id, clientTLS)
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
		st := r.Stats().Broadcasts[broadcastid.Obfuscate(id)]
		return st.HasConfig && st.CachedKeyframeChunks == kfChunks
	}, "config and keyframe cached")

	sub := dialSubscriber(t, ctx, port, id, clientTLS)

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
		st := r.Stats().Broadcasts[broadcastid.Obfuscate(id)]
		return st.HasConfig && st.CachedKeyframeChunks == kfChunks && st.FramesRelayed >= 2
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
	bst := after.Broadcasts[broadcastid.Obfuscate(id)]
	switch {
	case !bst.PublisherActive:
		t.Error("statusz publisherActive = false, want true")
	case bst.Subscribers != 1:
		t.Errorf("statusz subscribers = %d, want 1", bst.Subscribers)
	case bst.FramesRelayed < 2 || bst.DatagramsRelayed < kfChunks+1:
		t.Errorf("statusz counters did not move: %+v", bst)
	case !bst.HasConfig || bst.CachedKeyframeChunks != kfChunks || bst.CachedKeyframeBytes == 0:
		t.Errorf("statusz cache fields wrong: %+v", bst)
	}
}

func TestPublisherRestartPrimesWithNewConfig(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, r, _ := startTestServer(t, ctx, 15)

	pub1, id := dialPublisherAndGetID(t, ctx, port, clientTLS)
	if err := pub1.SendDatagram(testConfigDgram(t, "avc1.42E02A")); err != nil {
		t.Fatalf("send config: %v", err)
	}
	for _, chunk := range encodeFrame(t, 7, true, 2) {
		if err := pub1.SendDatagram(chunk); err != nil {
			t.Fatalf("send keyframe chunk: %v", err)
		}
	}
	waitFor(t, 5*time.Second, func() bool {
		st := r.Stats().Broadcasts[broadcastid.Obfuscate(id)]
		return st.HasConfig && st.CachedKeyframeChunks == 2
	}, "session 1 config and keyframe cached")

	pub1.CloseWithError(0, "restart")
	waitFor(t, 5*time.Second, func() bool { return !r.Stats().Broadcasts[broadcastid.Obfuscate(id)].PublisherActive }, "publisher slot freed")
	if st := r.Stats().Broadcasts[broadcastid.Obfuscate(id)]; !st.HasConfig {
		t.Fatal("caches must persist while the broadcaster is away")
	}

	pub2 := dialPublisherReclaim(t, ctx, port, id, clientTLS)
	waitFor(t, 5*time.Second, func() bool {
		st := r.Stats().Broadcasts[broadcastid.Obfuscate(id)]
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
		st := r.Stats().Broadcasts[broadcastid.Obfuscate(id)]
		return st.HasConfig && st.CachedKeyframeChunks == kfChunks
	}, "session 2 config and keyframe cached")

	sub := dialSubscriber(t, ctx, port, id, clientTLS)
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

func TestSubscriberSurvivesPublisherRestart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, r, _ := startTestServerCfg(t, ctx, config.Config{
		MaxSubscribers:  15,
		MaxIdleTimeout:  2 * time.Second,
		KeepAlivePeriod: 250 * time.Millisecond,
	})

	pub1, id := dialPublisherAndGetID(t, ctx, port, clientTLS)
	sub := dialSubscriber(t, ctx, port, id, clientTLS)
	waitFor(t, 5*time.Second, func() bool { return r.Stats().Totals.Subscribers == 1 }, "subscriber registered")

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

	pub1.CloseWithError(0, "broadcaster gone")
	waitFor(t, 5*time.Second, func() bool { return !r.Stats().Broadcasts[broadcastid.Obfuscate(id)].PublisherActive }, "publisher slot freed")
	time.Sleep(3 * time.Second)
	if got := r.Stats().Totals.Subscribers; got != 1 {
		t.Fatalf("subscribers after idle gap = %d, want 1 (session idled out?)", got)
	}
	select {
	case <-sub.Context().Done():
		t.Fatal("subscriber session closed during the broadcaster-away gap")
	default:
	}

	pub2 := dialPublisherReclaim(t, ctx, port, id, clientTLS)
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
			if hdr, _, err := wire.ParseVideoChunk(dgram); err == nil && hdr.Keyframe && hdr.FrameID == 0 {
				newChunks[hdr.ChunkIndex] = true
			}
		}
	}
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
		r, func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return nil, nil }, discardLog)

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
	waitFor(t, 5*time.Second, func() bool { return !r.Stats().Broadcasts[broadcastid.Obfuscate(id)].PublisherActive }, "publisher inactive")

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
	srv.testHookPostUpgradeSubscribe = func(string) {
		pubSess.CloseWithError(0, "")
		deadline := time.Now().Add(5 * time.Second)
		for !errors.Is(r.CheckSubscribe(id), hub.ErrNotFound) && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
	}

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
		return st.Broadcasts[broadcastid.Obfuscate(idA)].Subscribers == 1 && st.Broadcasts[broadcastid.Obfuscate(idB)].Subscribers == 1
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
		return st.Broadcasts[broadcastid.Obfuscate(id)]
	}

	if g := fetch().GraceRemainingSeconds; g != 0 {
		t.Errorf("graceRemainingSeconds with active publisher = %d, want 0", g)
	}

	pub.CloseWithError(0, "")
	waitFor(t, 5*time.Second, func() bool { return !r.Stats().Broadcasts[broadcastid.Obfuscate(id)].PublisherActive }, "publisher inactive")

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
		bst := stats.Broadcasts[broadcastid.Obfuscate(id)]
		return bst.BandwidthDroppedDatagrams > 0 && bst.BandwidthDroppedBytes > 0
	}, "bandwidth drops recorded")
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

