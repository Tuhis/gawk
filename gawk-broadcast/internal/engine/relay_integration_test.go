package engine_test

// An end-to-end test against the *real* relay binary.
//
// Why not an in-process transport.Server, as docs/19 V1 specifies: Decision 1
// moved the broadcaster into its own module, and Go's internal/ rule puts
// gawk-server/internal/transport permanently out of reach from here. That is a
// real consequence of the module split the design doc did not price in.
//
// The alternative — a hand-written fake relay — would test the engine against
// our *belief* about the relay, which is exactly the belief most worth
// doubting in a second implementation. So this builds and runs the actual
// gawk-server, publishes the committed H.264 fixture through the actual engine,
// and reads the actual /statusz to see what arrived. It is skipped when the
// relay source isn't present or `go` isn't available (`-short` skips it too).

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-broadcast/internal/engine"
	"github.com/Tuhis/gawk/gawk-broadcast/internal/fixture"
	"github.com/Tuhis/gawk/gawk-broadcast/internal/mpegts"
	"github.com/Tuhis/gawk/gawk-broadcast/internal/pubsim"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// fixtureSource replays the committed MPEG-TS fixture as access units — the
// same bytes the demuxer tests use, standing in for a GPU we do not have here.
type fixtureSource struct {
	aus    []engine.AccessUnit
	clock  engine.Clock
	frames chan engine.AccessUnit
	stop   chan struct{}
	done   chan struct{}
}

func newFixtureSource(t *testing.T, clock engine.Clock) *fixtureSource {
	t.Helper()
	ts := fixture.TS
	var aus []engine.AccessUnit
	d := mpegts.NewDemuxer(wire.MaxKeyframeBytes, func(au mpegts.AU) error {
		aus = append(aus, engine.AccessUnit{
			Data:     bytes.Clone(au.Data),
			Keyframe: engine.HasIDR(au.Data),
		})
		return nil
	})
	if _, err := d.Write(ts); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	return &fixtureSource{
		aus:    aus,
		clock:  clock,
		frames: make(chan engine.AccessUnit, 4),
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
}

func (s *fixtureSource) factory() engine.MediaSourceFactory {
	return func(cfg engine.MediaConfig, clock engine.Clock, log *slog.Logger) (engine.MediaSource, error) {
		s.clock = clock
		return s, nil
	}
}

func (s *fixtureSource) Start(ctx context.Context) (<-chan engine.AccessUnit, error) {
	go func() {
		defer close(s.done)
		defer close(s.frames)
		tick := time.NewTicker(10 * time.Millisecond) // ~100fps: finish quickly
		defer tick.Stop()
		for _, au := range s.aus {
			select {
			case <-s.stop:
				return
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			au.TimestampUs = s.clock.NowUs()
			select {
			case s.frames <- au:
			case <-s.stop:
				return
			}
		}
		<-s.stop // hold the stream open so the relay keeps the broadcast
	}()
	return s.frames, nil
}

func (s *fixtureSource) Stop() error {
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
	<-s.done
	return nil
}

func (s *fixtureSource) Encoder() string     { return "fixture" }
func (s *fixtureSource) CapturePath() string { return "fixture" }
func (s *fixtureSource) Err() error          { return nil }

// statusz mirrors the fields this test reads (names from hub.BroadcastStats —
// they are the relay's, not ours to invent).
type statusz struct {
	Totals struct {
		Broadcasts int `json:"broadcasts"`
	} `json:"totals"`
	Broadcasts map[string]struct {
		PublisherActive     bool   `json:"publisherActive"`
		FramesRelayed       uint64 `json:"framesRelayed"`
		DatagramsRelayed    uint64 `json:"datagramsRelayed"`
		BadDatagrams        uint64 `json:"badDatagrams"`
		HasConfig           bool   `json:"hasConfig"`
		CachedKeyframeBytes int    `json:"cachedKeyframeBytes"`
		KeyframeStreamsIn   uint64 `json:"keyframeStreamsIn"`
		IngressFramesLost   uint64 `json:"ingressFramesLost"`
		IngressChunksLost   uint64 `json:"ingressChunksLost"`
	} `json:"broadcasts"`
}

func freePort(t *testing.T) int {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	return pc.LocalAddr().(*net.UDPAddr).Port
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// startRelay builds and runs the real gawk-server, returning its URL and ops
// address.
func startRelay(t *testing.T, extraArgs ...string) (relayURL string, opsAddr string) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping the relay integration test in -short mode")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain unavailable")
	}
	serverDir, err := filepath.Abs("../../../gawk-server")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(serverDir, "go.mod")); err != nil {
		t.Skipf("gawk-server source not present: %v", err)
	}

	bin := filepath.Join(t.TempDir(), "gawk-server")
	build := exec.Command("go", "build", "-o", bin, "./cmd/gawk-server")
	build.Dir = serverDir
	if out, err := build.CombinedOutput(); err != nil {
		t.Skipf("cannot build gawk-server: %v\n%s", err, out)
	}
	certDir := filepath.Join(t.TempDir(), "cert")
	dcBin := filepath.Join(t.TempDir(), "gawk-devcert")
	build = exec.Command("go", "build", "-o", dcBin, "./cmd/gawk-devcert")
	build.Dir = serverDir
	if out, err := build.CombinedOutput(); err != nil {
		t.Skipf("cannot build gawk-devcert: %v\n%s", err, out)
	}
	if out, err := exec.Command(dcBin, "-out", certDir).CombinedOutput(); err != nil {
		t.Fatalf("gawk-devcert: %v\n%s", err, out)
	}

	port, ops := freePort(t), freeTCPPort(t)
	args := append([]string{
		"-addr", fmt.Sprintf("127.0.0.1:%d", port),
		"-metrics-addr", fmt.Sprintf("127.0.0.1:%d", ops),
		"-cert-file", filepath.Join(certDir, "cert.pem"),
		"-key-file", filepath.Join(certDir, "key.pem"),
	}, extraArgs...)
	cmd := exec.Command(bin, args...)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	opsAddr = fmt.Sprintf("http://127.0.0.1:%d", ops)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := http.Get(opsAddr + "/healthz"); err == nil {
			resp.Body.Close()
			return fmt.Sprintf("https://127.0.0.1:%d", port), opsAddr
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("relay did not become healthy")
	return "", ""
}

func fetchStatusz(t *testing.T, opsAddr string) statusz {
	t.Helper()
	resp, err := http.Get(opsAddr + "/statusz")
	if err != nil {
		t.Fatalf("statusz: %v", err)
	}
	defer resp.Body.Close()
	var s statusz
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		t.Fatalf("statusz decode: %v", err)
	}
	return s
}

// The whole publisher protocol, against the real relay: dial, announce,
// keyframes on reliable streams, deltas on datagrams, TimeSync, ClockMapping.
func TestPublishesToRealRelay(t *testing.T) {
	relayURL, opsAddr := startRelay(t)

	src := newFixtureSource(t, engine.NewClock())
	gotID := make(chan string, 1)
	errs := make(chan error, 8)

	sess := engine.New(
		engine.Config{RelayURL: relayURL, Insecure: true, Media: engine.DefaultMediaConfig()},
		engine.Callbacks{
			OnBroadcastID: func(id string) { gotID <- id },
			OnError:       func(err error) { errs <- err },
		},
		engine.Options{MediaFactory: src.factory(), StatsInterval: 100 * time.Millisecond},
	)

	if err := sess.Start(context.Background()); err != nil {
		t.Fatalf("Start against the real relay: %v", err)
	}
	defer sess.Stop()

	var id string
	select {
	case id = <-gotID:
	case err := <-errs:
		t.Fatalf("engine error: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the relay never announced a broadcast code")
	}
	if len(id) != 6 {
		t.Errorf("broadcast code %q is not 6 characters", id)
	}

	// Wait for the relay to have ingested frames and a keyframe.
	var st statusz
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		st = fetchStatusz(t, opsAddr)
		if len(st.Broadcasts) == 1 {
			for _, b := range st.Broadcasts {
				if b.FramesRelayed > 10 && b.KeyframeStreamsIn > 0 && b.CachedKeyframeBytes > 0 {
					goto arrived
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
arrived:
	if len(st.Broadcasts) != 1 {
		t.Fatalf("relay shows %d broadcasts, want 1", len(st.Broadcasts))
	}
	for _, b := range st.Broadcasts {
		if !b.PublisherActive {
			t.Error("relay does not consider the publisher active")
		}
		if b.FramesRelayed == 0 {
			t.Error("relay relayed no frames")
		}
		if b.DatagramsRelayed == 0 {
			t.Error("relay relayed no datagrams: the delta path is not working")
		}
		if b.KeyframeStreamsIn == 0 {
			t.Error("relay ingested no keyframe streams: the reliable-keyframe path is not working")
		}
		if b.CachedKeyframeBytes == 0 {
			t.Error("relay cached no keyframe: late joiners would see nothing")
		}
		// The cached keyframe embeds our DecoderConfig. This is the whole
		// empty-extradata Annex-B bet: the relay accepted a config with no
		// avcC record, and a late joiner is primed with it.
		if !b.HasConfig {
			t.Error("the cached keyframe carries no decoder config: a primed late joiner could not decode it")
		}
		// Everything we sent parsed. A ClockMapping or config the relay
		// rejected would land here rather than anywhere more obvious.
		if b.BadDatagrams != 0 {
			t.Errorf("relay rejected %d of our datagrams as unparseable", b.BadDatagrams)
		}
		// On loopback nothing should be lost. Non-zero means our frame IDs are
		// not what the relay's R9 ingress-loss window expects.
		if b.IngressFramesLost != 0 || b.IngressChunksLost != 0 {
			t.Errorf("relay reports %d frames / %d chunks lost on loopback: our frame IDs disagree with its ingress window",
				b.IngressFramesLost, b.IngressChunksLost)
		}
	}

	// The engine's own view should agree with the relay's.
	s := sess.Stats()
	if s.KeyframeStreamsSent == 0 {
		t.Error("engine sent no keyframe streams")
	}
	if s.DatagramsSent == 0 {
		t.Error("engine sent no datagrams")
	}
	if s.FramesDroppedAtSend != 0 {
		t.Errorf("engine dropped %d frames at send on loopback", s.FramesDroppedAtSend)
	}
	if s.Codec != "avc1.42C00D" {
		t.Errorf("codec = %q, want the fixture's avc1.42C00D parsed from its SPS", s.Codec)
	}
	// TimeSync is a real round trip against the relay's monotonic clock.
	if !s.TimeSyncAvailable {
		t.Error("no TimeSync sample: the relay's inline reply path is not reaching us")
	}

	select {
	case err := <-errs:
		t.Errorf("engine reported an error during a healthy session: %v", err)
	default:
	}
}

// What a *viewer* actually receives.
//
// The relay's own accounting can only say "bytes arrived and parsed". This
// attaches a real subscriber to the real relay and inspects what comes out the
// other side, because the interop risks in a second broadcaster all live here:
// whether the keyframe is self-sufficient, whether the codec string is right,
// whether the Annex-B/empty-extradata bet holds, and whether ClockMapping —
// which /statusz does not expose at all — reaches viewers.
func TestSubscriberReceivesAUsableStream(t *testing.T) {
	relayURL, _ := startRelay(t)

	src := newFixtureSource(t, engine.NewClock())
	gotID := make(chan string, 1)
	sess := engine.New(
		engine.Config{RelayURL: relayURL, Insecure: true, Media: engine.DefaultMediaConfig()},
		engine.Callbacks{OnBroadcastID: func(id string) { gotID <- id }},
		engine.Options{MediaFactory: src.factory()},
	)
	if err := sess.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer sess.Stop()

	var id string
	select {
	case id = <-gotID:
	case <-time.After(10 * time.Second):
		t.Fatal("no broadcast code")
	}

	// Join as a late viewer, exactly as the browser does.
	d := &webtransport.Dialer{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true, NextProtos: []string{http3.NextProtoH3}},
		QUICConfig:      &quic.Config{EnableDatagrams: true, EnableStreamResetPartialDelivery: true},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	rsp, viewer, err := d.Dial(ctx, relayURL+"/subscribe/"+id, nil)
	if err != nil {
		t.Fatalf("subscribe dial: %v (status %v)", err, rsp)
	}
	defer viewer.CloseWithError(0, "")

	// 1. The keyframe arrives on a reliable uni stream, and is self-sufficient.
	kfDone := make(chan error, 1)
	go func() {
		// Dispatch by wire type, never by accept order: webtransport-go does
		// not accept in open order (docs/22 finding 9), and the relay sends a
		// viewer other server-initiated streams besides the keyframe —
		// RelayCapabilities (R29), TelemetryHello (R28). Taking the first
		// stream to be the keyframe is the bug this loop exists to avoid, and
		// it is what the production viewer's readServerStreams already does.
		var msg []byte
		var hdr wire.StreamFrameHeader
		for {
			str, err := viewer.AcceptUniStream(ctx)
			if err != nil {
				kfDone <- fmt.Errorf("no keyframe stream: %w", err)
				return
			}
			b, err := io.ReadAll(io.LimitReader(str, wire.MaxKeyframeBytes))
			if err != nil {
				kfDone <- fmt.Errorf("keyframe read: %w", err)
				return
			}
			if len(b) < 2 {
				continue
			}
			if b[1] != wire.TypeStreamFrame {
				continue
			}
			h, err := wire.ParseStreamFrameHeader(b)
			if err != nil {
				kfDone <- fmt.Errorf("keyframe header: %w", err)
				return
			}
			msg, hdr = b, h
			break
		}
		if !hdr.Keyframe {
			kfDone <- fmt.Errorf("stream frame is not marked keyframe")
			return
		}
		if hdr.ConfigLen == 0 {
			kfDone <- fmt.Errorf("primed keyframe carries no config: a late joiner could not decode it")
			return
		}
		cfg, err := wire.ParseDecoderConfig(msg[wire.StreamFrameHeaderSize : wire.StreamFrameHeaderSize+int(hdr.ConfigLen)])
		if err != nil {
			kfDone <- fmt.Errorf("embedded config: %w", err)
			return
		}
		if cfg.Codec != "avc1.42C00D" {
			kfDone <- fmt.Errorf("codec = %q, want the fixture's avc1.42C00D", cfg.Codec)
			return
		}
		// The Annex-B bet: empty extradata, and the payload starts with a
		// start code — which is what routes the viewer's isAnnexB sniff into
		// the branch that ignores extradata entirely.
		if len(cfg.Extradata) != 0 {
			kfDone <- fmt.Errorf("extradata = %x, want empty on the Annex-B path", cfg.Extradata)
			return
		}
		payload := msg[wire.StreamFrameHeaderSize+int(hdr.ConfigLen):]
		if !bytes.HasPrefix(payload, []byte{0, 0, 0, 1}) && !bytes.HasPrefix(payload, []byte{0, 0, 1}) {
			kfDone <- fmt.Errorf("keyframe payload does not start with an Annex-B start code: %x", payload[:min(8, len(payload))])
			return
		}
		if !engine.HasIDR(payload) {
			kfDone <- fmt.Errorf("the keyframe contains no IDR slice")
			return
		}
		kfDone <- nil
	}()

	// 2. Deltas arrive as datagrams, and so does the ClockMapping.
	var sawDelta, sawMapping bool
	dgDone := make(chan error, 1)
	go func() {
		for !sawDelta || !sawMapping {
			dgram, err := viewer.ReceiveDatagram(ctx)
			if err != nil {
				dgDone <- fmt.Errorf("datagram read: %w", err)
				return
			}
			_, typ, err := wire.PeekType(dgram)
			if err != nil {
				continue
			}
			switch typ {
			case wire.TypeVideoChunk:
				if _, _, err := wire.ParseVideoChunk(dgram); err != nil {
					dgDone <- fmt.Errorf("bad video chunk: %w", err)
					return
				}
				sawDelta = true
			case wire.TypeClockMapping:
				if _, err := wire.ParseClockMapping(dgram); err != nil {
					dgDone <- fmt.Errorf("bad clock mapping: %w", err)
					return
				}
				sawMapping = true
			}
		}
		dgDone <- nil
	}()

	select {
	case err := <-kfDone:
		if err != nil {
			t.Errorf("keyframe path: %v", err)
		}
	case <-ctx.Done():
		t.Error("no keyframe reached the viewer")
	}
	select {
	case err := <-dgDone:
		if err != nil {
			t.Errorf("datagram path: %v", err)
		}
	case <-ctx.Done():
		t.Errorf("viewer saw delta=%v mapping=%v, want both", sawDelta, sawMapping)
	}
}

// What a viewer receives on the *audio* lane (R25, docs/28 NA5).
//
// The same reasoning as the video test above, one lane over: the relay's own
// accounting says only that datagrams arrived, and every interop risk in a
// second audio producer lives at the viewer's end — whether the config leads
// the frames (a frame that arrives first is thrown away, because there is
// nothing to configure the decoder with), whether the sequence space is its
// own, and whether the packets are single, unchunked Opus.
//
// It publishes through the real relay with pubsim's audio source, which is the
// same lane gawk-pubsim -audio drives in CI.
func TestSubscriberReceivesAudio(t *testing.T) {
	relayURL, _ := startRelay(t)

	packets, err := fixture.SplitAudio(fixture.Audio)
	if err != nil {
		t.Fatalf("SplitAudio: %v", err)
	}
	aus, err := pubsim.Demux(fixture.TS)
	if err != nil {
		t.Fatalf("Demux: %v", err)
	}

	gotID := make(chan string, 1)
	sess := engine.New(
		engine.Config{RelayURL: relayURL, Insecure: true, Media: engine.DefaultMediaConfig()},
		engine.Callbacks{OnBroadcastID: func(id string) { gotID <- id }},
		engine.Options{MediaFactory: pubsim.Factory(aus, 30, packets)},
	)
	if err := sess.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer sess.Stop()

	var id string
	select {
	case id = <-gotID:
	case <-time.After(10 * time.Second):
		t.Fatal("no broadcast code")
	}

	d := &webtransport.Dialer{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true, NextProtos: []string{http3.NextProtoH3}},
		QUICConfig:      &quic.Config{EnableDatagrams: true, EnableStreamResetPartialDelivery: true},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	rsp, viewer, err := d.Dial(ctx, relayURL+"/subscribe/"+id, nil)
	if err != nil {
		t.Fatalf("subscribe dial: %v (status %v)", err, rsp)
	}
	defer viewer.CloseWithError(0, "")

	var (
		sawConfig bool
		seqs      []uint32
	)
	for len(seqs) < 5 {
		dgram, err := viewer.ReceiveDatagram(ctx)
		if err != nil {
			t.Fatalf("datagram read after %d audio frames (config seen: %v): %v", len(seqs), sawConfig, err)
		}
		_, typ, err := wire.PeekType(dgram)
		if err != nil {
			continue
		}
		switch typ {
		case wire.TypeAudioConfig:
			cfg, err := wire.ParseAudioConfig(dgram)
			if err != nil {
				t.Fatalf("bad audio config: %v", err)
			}
			if cfg.Codec != engine.AudioCodec || cfg.SampleRate != engine.AudioSampleRate || cfg.Channels != engine.AudioChannels {
				t.Errorf("audio config = %+v, want 48 kHz stereo opus", cfg)
			}
			sawConfig = true
		case wire.TypeAudioFrame:
			if !sawConfig {
				t.Fatal("an audio frame arrived before any config: the viewer would have nothing to configure its decoder with")
			}
			h, payload, err := wire.ParseAudioFrame(dgram)
			if err != nil {
				t.Fatalf("bad audio frame: %v", err)
			}
			// One packet per datagram, never chunked — and it starts at the
			// Opus TOC, so the container's control header really was stripped.
			if len(payload) == 0 || len(payload) > wire.MaxAudioPayload {
				t.Errorf("audio payload = %d bytes, want a single Opus packet", len(payload))
			}
			if payload[0]>>3 != 31 || payload[0]&0x04 == 0 {
				t.Errorf("payload does not begin with a stereo fullband TOC: %#02x", payload[0])
			}
			seqs = append(seqs, h.Seq)
		}
	}

	// Audio's own sequence space, monotonic and gapless on a loopback link —
	// anchored on the FIRST seq observed, never on 0.
	//
	// Audio frames are unreliable datagrams with no per-frame join prime (the
	// hub caches the audio *config*, not the packets), so anything the
	// publisher emitted before this subscriber attached was never fanned out
	// to it. Demanding seqs[0] == 0 asserts that the viewer won a startup race
	// the relay never promised; it lost that race on a loaded CI runner
	// (2026-07-28, seqs 1..5) and turned an unrelated PR red. Contiguity of
	// what the viewer *does* receive is the real claim, and it still fails on
	// a genuine drop.
	for i, seq := range seqs {
		if want := seqs[0] + uint32(i); seq != want {
			t.Errorf("audio seq[%d] = %d, want %d (gap in a loopback stream)", i, seq, want)
		}
	}

	if st := sess.Stats(); st.AudioState != engine.AudioActive {
		t.Errorf("AudioState = %q, want %q while packets are flowing", st.AudioState, engine.AudioActive)
	}
}

// Decision 10's claim path under R17 rules, revised per docs/06 (2026-07-18):
// every /publish/{id} claim carries the relay-minted resume token (a bare
// claim is 403, the designed graced-ID-hijack fix), and a token-bearing claim
// of a live ID now SUPERSEDES the incumbent session — the relay cannot tell a
// zombie publisher from a live one inside the QUIC idle window, and the old
// 409 there forced the mint fallback that orphaned every viewer.
func TestReclaimSupersedesAgainstRealRelay(t *testing.T) {
	relayURL, _ := startRelay(t)

	src := newFixtureSource(t, engine.NewClock())
	gotID := make(chan string, 1)
	gotToken := make(chan string, 1)
	ended := make(chan struct{}, 1)
	sess := engine.New(
		engine.Config{RelayURL: relayURL, Insecure: true, Media: engine.DefaultMediaConfig()},
		engine.Callbacks{
			OnBroadcastID: func(id string) { gotID <- id },
			OnResumeToken: func(token string) { gotToken <- token },
			OnEnded:       func() { ended <- struct{}{} },
		},
		engine.Options{MediaFactory: src.factory()},
	)
	if err := sess.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	var id, token string
	select {
	case id = <-gotID:
	case <-time.After(10 * time.Second):
		t.Fatal("no broadcast code")
	}
	select {
	case token = <-gotToken:
	case <-time.After(10 * time.Second):
		t.Fatal("no resume token — an R17 relay mints one on every publish")
	}

	// A second publisher claiming the same live ID with the token wins the
	// slot (newest publisher wins) and announces the same code.
	src2 := newFixtureSource(t, engine.NewClock())
	takenOver := make(chan string, 1)
	sess2 := engine.New(
		engine.Config{RelayURL: relayURL, BroadcastID: id, ResumeToken: token, Insecure: true, Media: engine.DefaultMediaConfig()},
		engine.Callbacks{OnBroadcastID: func(id string) { takenOver <- id }},
		engine.Options{MediaFactory: src2.factory()},
	)
	if err := sess2.Start(context.Background()); err != nil {
		t.Fatalf("superseding publisher Start = %v, want success", err)
	}
	select {
	case got := <-takenOver:
		if got != id {
			t.Errorf("takeover announced %q, want the original %q", got, id)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("takeover announced no code")
	}

	// The deposed first session ends on its own — the relay closes it with
	// CloseCodePublisherSuperseded — without Stop ever being called on it.
	select {
	case <-ended:
	case <-time.After(10 * time.Second):
		t.Fatal("first session did not end after being superseded")
	}

	// Without the token the same claim never reaches the takeover: 403, the
	// R17 hijack fix working as designed — supersede is owner-only.
	src2b := newFixtureSource(t, engine.NewClock())
	sess2b := engine.New(
		engine.Config{RelayURL: relayURL, BroadcastID: id, Insecure: true, Media: engine.DefaultMediaConfig()},
		engine.Callbacks{},
		engine.Options{MediaFactory: src2b.factory()},
	)
	err := sess2b.Start(context.Background())
	se, ok := engine.AsStartError(err)
	if !ok {
		t.Fatalf("tokenless claim error = %v, want *StartError", err)
	}
	if se.Status != http.StatusForbidden {
		t.Errorf("tokenless claim status = %d, want 403 (resume token required)", se.Status)
	}
	// src2b is deliberately not Stop()ped: the claim failed at connect, so
	// the engine never Start()ed the source, and fixtureSource.Stop blocks
	// on a done channel only its (never-spawned) feeder goroutine closes.
	src.Stop()

	// A clean stop still leaves the ID reclaimable.
	sess2.Stop()
	src2.Stop()

	src3 := newFixtureSource(t, engine.NewClock())
	reclaimed := make(chan string, 1)
	sess3 := engine.New(
		engine.Config{RelayURL: relayURL, BroadcastID: id, ResumeToken: token, Insecure: true, Media: engine.DefaultMediaConfig()},
		engine.Callbacks{OnBroadcastID: func(id string) { reclaimed <- id }},
		engine.Options{MediaFactory: src3.factory()},
	)
	if err := sess3.Start(context.Background()); err != nil {
		t.Fatalf("reclaim failed: %v", err)
	}
	defer sess3.Stop()
	select {
	case got := <-reclaimed:
		if got != id {
			t.Errorf("reclaimed %q, want the original %q", got, id)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("reclaim announced no code")
	}
}

// Sending a custom-scheme Origin end to end does not break the dial.
//
// This does NOT test origin *rejection*: the relay bypasses CheckOrigin for
// loopback (transport/server.go, for in-pod health probes), and this harness
// runs both ends on 127.0.0.1, so the whitelist is never consulted here. That
// enforcement is the relay's own concern and is verified over a real network
// (the field bug this fixes appeared gaming-PC → homelab, not on loopback); the
// engine's half — that it sends the configured/default Origin at all — is the
// unit test TestDialSendsOrigin. What this adds is the interop assurance that a
// gawk-broadcast://native Origin header is one the QUIC/H3 stack transmits and
// a real relay accepts, rather than choking on the unusual scheme.
func TestCustomOriginDoesNotBreakTheDial(t *testing.T) {
	relayURL, _ := startRelay(t, "-allowed-origins", engine.DefaultOrigin)

	src := newFixtureSource(t, engine.NewClock())
	gotID := make(chan string, 1)
	sess := engine.New(
		engine.Config{RelayURL: relayURL, Insecure: true, Media: engine.DefaultMediaConfig()},
		engine.Callbacks{OnBroadcastID: func(id string) { gotID <- id }},
		engine.Options{MediaFactory: src.factory()},
	)
	if err := sess.Start(context.Background()); err != nil {
		t.Fatalf("Start while sending %q as Origin: %v", engine.DefaultOrigin, err)
	}
	defer sess.Stop()
	select {
	case <-gotID:
	case <-time.After(10 * time.Second):
		t.Fatal("no code: sending the native Origin header broke the dial")
	}
}

// R2's publish secret, end to end: the query param the browser is forced to
// use, and the 401 the browser cannot see.
func TestPublishSecretAgainstRealRelay(t *testing.T) {
	relayURL, _ := startRelay(t, "-publish-secret", "s3cret")

	// Wrong secret → 401, as a sentence.
	src := newFixtureSource(t, engine.NewClock())
	sess := engine.New(
		engine.Config{RelayURL: relayURL, PublishSecret: "wrong", Insecure: true, Media: engine.DefaultMediaConfig()},
		engine.Callbacks{},
		engine.Options{MediaFactory: src.factory()},
	)
	se, ok := engine.AsStartError(sess.Start(context.Background()))
	if !ok {
		t.Fatal("want *StartError for a bad secret")
	}
	if se.Status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", se.Status)
	}

	// Right secret → published.
	src2 := newFixtureSource(t, engine.NewClock())
	gotID := make(chan string, 1)
	sess2 := engine.New(
		engine.Config{RelayURL: relayURL, PublishSecret: "s3cret", Insecure: true, Media: engine.DefaultMediaConfig()},
		engine.Callbacks{OnBroadcastID: func(id string) { gotID <- id }},
		engine.Options{MediaFactory: src2.factory()},
	)
	if err := sess2.Start(context.Background()); err != nil {
		t.Fatalf("Start with the right secret: %v", err)
	}
	defer sess2.Stop()
	select {
	case <-gotID:
	case <-time.After(10 * time.Second):
		t.Fatal("no code with the correct secret")
	}
}
