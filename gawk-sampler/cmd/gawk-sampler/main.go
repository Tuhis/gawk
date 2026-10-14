// Command gawk-sampler is R40's automatic content screener (docs/71): every
// few seconds it scores each live broadcast's cached keyframe in its shard
// with a nudity detector and tells gawk-admin when a sustained explicit
// signal trips a broadcast's window. gawk-admin pages, pins and — only if the
// operator switched it on — kills; the sampler itself never acts on a
// broadcast (D14).
//
//	gawk-sampler [run] [flags]     the service (docs/71 §5)
//	gawk-sampler score [flags] DIR offline recall over local images (D19)
//	gawk-sampler fetch-model …     the chart's init container (D5)
//
// The binary also answers a hidden `ffmpeg-exec`: it is how each decode runs
// under its rlimits (internal/decode).
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/Tuhis/gawk/gawk-sampler/internal/admin"
	"github.com/Tuhis/gawk/gawk-sampler/internal/config"
	"github.com/Tuhis/gawk/gawk-sampler/internal/decode"
	"github.com/Tuhis/gawk/gawk-sampler/internal/detect"
	"github.com/Tuhis/gawk/gawk-sampler/internal/fetchmodel"
	"github.com/Tuhis/gawk/gawk-sampler/internal/relay"
	"github.com/Tuhis/gawk/gawk-sampler/internal/sampler"
	"github.com/Tuhis/gawk/gawk-sampler/internal/scorecmd"
	"github.com/Tuhis/gawk/gawk-sampler/internal/server"
)

// version is stamped at build time; release-please keeps the chart and image
// tag in step with it.
var version = "dev"

func main() {
	args := os.Args[1:]
	// The decode helper first and alone: it must exec ffmpeg before anything
	// else in this process allocates under the limits it just set.
	if len(args) > 0 && args[0] == decode.HelperCommand {
		err := decode.ExecHelper(args[1:])
		fmt.Fprintln(os.Stderr, err)
		os.Exit(127)
	}
	if err := run(args, os.Getenv, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, getenv func(string) string, stdout io.Writer) error {
	cmd := "run"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch cmd {
	case "run":
		return serve(ctx, args, getenv)
	case "score":
		return scorecmd.Run(ctx, args, getenv, stdout)
	case "fetch-model":
		return fetchmodel.Run(ctx, args, stdout)
	case "version":
		fmt.Fprintln(stdout, version)
		return nil
	}
	return fmt.Errorf("unknown subcommand %q (want run, score, fetch-model or version)", cmd)
}

func newLogger(c config.Config) *slog.Logger {
	opts := &slog.HandlerOptions{Level: c.LogLevel}
	if c.LogFormat == "json" {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}

func serve(ctx context.Context, args []string, getenv func(string) string) error {
	cfg, err := config.Parse(args, getenv)
	if err != nil {
		return err
	}
	log := newLogger(cfg)
	log.Info("gawk-sampler starting", append([]any{"version", version}, cfg.LogAttrs()...)...)

	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	build := prometheus.NewGauge(prometheus.GaugeOpts{
		Name:        "gawk_sampler_build_info",
		Help:        "The running version and mode; always 1.",
		ConstLabels: prometheus.Labels{"version": version, "mode": cfg.Mode, "model_input": fmt.Sprint(cfg.Model.Input)},
	})
	build.Set(1)
	reg.MustRegister(build)
	metrics := sampler.NewMetrics(reg)
	ready := server.NewReady("loading the model")

	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(ctx)
	defer func() { cancel(); wg.Wait() }()
	if cfg.MetricsAddr != "" {
		l, err := net.Listen("tcp", cfg.MetricsAddr)
		if err != nil {
			return fmt.Errorf("metrics listen %s: %w", cfg.MetricsAddr, err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := server.Serve(ctx, l, server.Handler(reg, ready), log); err != nil {
				log.Error("metrics listener stopped", "err", err)
			}
		}()
	}

	// Fail fast on what the sampler cannot work without: the model, ONNX
	// Runtime, ffmpeg, and its own executable as the decode helper.
	ffmpeg, err := exec.LookPath(cfg.FFmpeg)
	if err != nil {
		return fmt.Errorf("ffmpeg: %w", err)
	}
	if _, err := os.Executable(); err != nil {
		return fmt.Errorf("locate own executable (the decode helper): %w", err)
	}
	if err := detect.Init(cfg.Model.ONNXRuntimeLib); err != nil {
		return err
	}
	det, err := detect.Load(cfg.Model.Path, cfg.Model.Input)
	if err != nil {
		return err
	}
	defer det.Close()

	tokens := &admin.TokenSource{
		Issuer:       cfg.OIDCIssuer,
		ClientID:     cfg.OIDCClientID,
		ClientSecret: cfg.OIDCClientSecret,
		Scope:        cfg.OIDCScope,
		Client:       &http.Client{Timeout: 10 * time.Second},
	}
	engine, err := sampler.New(sampler.Options{
		Mode:           cfg.Mode,
		SamplerID:      cfg.SamplerID,
		ShardIndex:     cfg.ShardIndex,
		ShardCount:     cfg.ShardCount,
		Labels:         cfg.Model.Labels,
		Rule:           cfg.Rule(),
		ReportSamples:  cfg.ReportSamples,
		Floor:          cfg.SampleFloor,
		Interval:       cfg.Interval,
		Workers:        cfg.Workers,
		TargetsRefresh: cfg.TargetsRefresh,
		Admin:          &admin.Client{Base: cfg.AdminURL, Tokens: tokens, HTTP: &http.Client{Timeout: 15 * time.Second}},
		// A keyframe fetch is a LAN round trip to a pod; anything slower than
		// a decode is a sample better skipped than waited for.
		Keyframes: relay.New(cfg.ScreeningToken, cfg.DecodeTimeout+time.Second),
		Decoder: &decode.Decoder{
			FFmpeg:      ffmpeg,
			Size:        cfg.Model.Input,
			Timeout:     cfg.DecodeTimeout,
			MemoryLimit: cfg.DecodeMemoryLimit,
		},
		Scorer:  detect.Scorer{Detector: det, Labels: cfg.Model.Labels},
		Metrics: metrics,
		Log:     log,
	})
	if err != nil {
		return err
	}
	ready.Set()
	log.Info("gawk-sampler ready", "mode", cfg.Mode, "shard", cfg.ShardIndex, "of", cfg.ShardCount)
	if err := engine.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	log.Info("gawk-sampler stopped")
	return nil
}
