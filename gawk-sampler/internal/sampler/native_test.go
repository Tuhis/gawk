package sampler

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Tuhis/gawk/gawk-sampler/internal/bitstream/bitstreamtest"
	"github.com/Tuhis/gawk/gawk-sampler/internal/decode"
	"github.com/Tuhis/gawk/gawk-sampler/internal/detect"
	"github.com/Tuhis/gawk/gawk-sampler/internal/nativetest"
	"github.com/Tuhis/gawk/gawk-sampler/internal/relay"
	"github.com/Tuhis/gawk/gawk-server/screening"
)

const helperEnv = "GAWK_SAMPLER_TEST_EXEC_HELPER"

// TestMain lets this test binary stand in for the sampler binary as the
// decode helper.
func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) == "1" && len(os.Args) > 1 && os.Args[1] == decode.HelperCommand {
		err := decode.ExecHelper(os.Args[2:])
		os.Stderr.WriteString(err.Error() + "\n")
		os.Exit(127)
	}
	os.Exit(m.Run())
}

// The whole pipeline with nothing faked but the relay's HTTP server and
// gawk-admin: real StreamFrames over HTTP with ETags, the real bitstream
// conversion, the real ffmpeg under its limits, ONNX Runtime with the test
// model, and the engine. A magenta broadcast trips and is flagged, a
// broadcast of ordinary frames never is, and one sending garbage fails only
// its own samples.
func TestNativePipeline(t *testing.T) {
	ffmpeg := nativetest.FFmpeg(t)
	lib := nativetest.ONNXRuntime(t)
	td := nativetest.Testdata(t)
	if err := detect.Init(lib); err != nil {
		t.Fatal(err)
	}
	det, err := detect.Load(filepath.Join(td, "models", "test-320.onnx"), 320)
	if err != nil {
		t.Fatal(err)
	}
	defer det.Close()

	magenta := bitstreamtest.Fixture(td, "magenta.h264")
	record, sample := bitstreamtest.AVCC(bitstreamtest.Fixture(td, "quadrants.h264"))
	frames := map[string][]byte{
		"MAGNTA": bitstreamtest.StreamFrame(1, "avc1.42C00B", nil, magenta),       // desktop Annex B
		"QUADS2": bitstreamtest.StreamFrame(1, "avc1.42C00B", record, sample),     // web AVCC
		"GRBAGE": bitstreamtest.StreamFrame(1, "", nil, magenta[:len(magenta)/3]), // truncated
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer screening-tok" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		for id, f := range frames {
			if r.URL.Path == screening.KeyframePath(id) {
				tag := screening.KeyframeETag("e-"+id, 1)
				w.Header().Set("ETag", tag)
				w.Header().Set(screening.HeaderPublisherEpoch, "e-"+id)
				w.Header().Set(screening.HeaderCodec, "h264")
				if r.Header.Get("If-None-Match") == tag {
					w.WriteHeader(http.StatusNotModified)
					return
				}
				w.Write(f)
				return
			}
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	adm := &fakeAdmin{flagRoute: true}
	for id := range frames {
		adm.targets = append(adm.targets, screening.Target{BroadcastID: id, BroadcastKey: "0123456789ab", PublisherEpoch: "e-" + id, Relay: srv.URL})
	}
	reg := prometheus.NewRegistry()
	metrics := NewMetrics(reg)
	var logs syncBuffer
	e, err := New(Options{
		Mode:           screening.ModeFlag,
		SamplerID:      "gawk-sampler-0",
		ShardIndex:     0,
		ShardCount:     1,
		Labels:         screening.DefaultLabels(),
		Rule:           screening.WindowRule{Threshold: 0.8, Size: 6, Hits: 4},
		ReportSamples:  12,
		Floor:          0.3,
		Interval:       150 * time.Millisecond,
		Workers:        2,
		TargetsRefresh: time.Hour,
		BatchInterval:  200 * time.Millisecond,
		Admin:          adm,
		Keyframes:      relay.New("screening-tok", 2*time.Second),
		Decoder: &decode.Decoder{FFmpeg: ffmpeg, Size: 320, Timeout: 2 * time.Second, MemoryLimit: 1 << 30,
			Helper: exe, HelperEnv: []string{helperEnv + "=1"}},
		Scorer:  detect.Scorer{Detector: det, Labels: screening.DefaultLabels()},
		Metrics: metrics,
		Log:     slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()
	time.Sleep(2500 * time.Millisecond)
	cancel()
	<-done

	adm.mu.Lock()
	defer adm.mu.Unlock()
	byID := map[string][]screening.ContentFlag{}
	for _, f := range adm.flags {
		byID[f.BroadcastID] = append(byID[f.BroadcastID], f)
	}
	mf := byID["MAGNTA"]
	if len(mf) == 0 || mf[0].State != screening.StateTripped || mf[0].Scores.Max < 0.9 || mf[0].Labels[0] != "FEMALE_GENITALIA_EXPOSED" {
		t.Fatalf("magenta flags %+v\nlogs:\n%s", mf, logs.String())
	}
	if len(byID["QUADS2"]) != 0 || len(byID["GRBAGE"]) != 0 {
		t.Fatalf("ordinary or garbage frames were flagged: %+v", byID)
	}
	taken := map[string]int{}
	for _, b := range adm.batches {
		for _, s := range b.Sessions {
			taken[s.BroadcastID] += s.Taken
		}
	}
	if taken["MAGNTA"] < 4 || taken["QUADS2"] < 4 || taken["GRBAGE"] != 0 {
		t.Fatalf("samples taken %v", taken)
	}
	// After the first fetch the relay answered 304s: each repeated the score.
	if testutil.ToFloat64(metrics.samples.WithLabelValues("unchanged")) == 0 {
		t.Fatal("no 304 was counted as a sample")
	}
	if testutil.ToFloat64(metrics.failures.WithLabelValues("decode")) == 0 {
		t.Fatal("the truncated keyframe did not fail its decode")
	}
	if strings.Contains(logs.String(), "panic") {
		t.Fatalf("logs:\n%s", logs.String())
	}
}
