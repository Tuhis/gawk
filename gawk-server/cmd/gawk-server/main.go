// gawk-server is the WebTransport relay for the gawk game stream:
// one publisher fans out encoded video datagrams to a small set of
// subscribers.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/Tuhis/gawk/gawk-server/internal/cluster"
	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/internal/metrics"
	"github.com/Tuhis/gawk/gawk-server/internal/ops"
	"github.com/Tuhis/gawk/gawk-server/internal/tlsutil"
	"github.com/Tuhis/gawk/gawk-server/internal/transport"
)

// Stamped at build time via -ldflags "-X main.version=..." (see
// deploy/Dockerfile); "dev" for plain go build/run.
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "gawk-server:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.ParseFlags(os.Args[1:], os.Getenv)
	if err != nil {
		return err
	}
	// R37: the build version rides config so the transport's RelayIdentity
	// (wire 0x11) can carry it without importing main.
	cfg.ReleaseVersion = version

	var handler slog.Handler
	opts := &slog.HandlerOptions{Level: cfg.LogLevel}
	if cfg.LogFormat == "json" {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		handler = slog.NewTextHandler(os.Stdout, opts)
	}
	log := slog.New(handler)
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Info("starting",
		"version", version,
		"addr", cfg.Addr,
		"dev_cert", cfg.DevCert,
		"max_subscribers", cfg.MaxSubscribers,
		"max_broadcasts", cfg.MaxBroadcasts,
		"max_total_subscribers", cfg.MaxTotalSubscribers,
		"publish_secret_set", cfg.PublishSecret != "",
		"conn_rate_limit", cfg.ConnRateLimit,
		"conn_burst_limit", cfg.ConnBurstLimit,
		"max_bandwidth_bytes", cfg.MaxBandwidthBytes,
		"max_keyframe_bytes", cfg.MaxKeyframeBytes,
		"keyframe_write_timeout", cfg.KeyframeWriteTimeout,
		"dvr_window", cfg.DVRWindow,
		"dvr_max_bytes", cfg.DVRMaxBytes,
		"dvr_max_catchup", cfg.DVRMaxCatchup,
		"dvr_audio", cfg.DVRAudio,
		"live_edge_audio_on_reliable_stream", cfg.LiveEdgeAudioOnReliableStream,
		"parity_default", cfg.ParityDefault,
		"striped_delivery", cfg.StripedDelivery,
		"max_idle_timeout", cfg.MaxIdleTimeout,
		"keepalive_period", cfg.KeepAlivePeriod,
		"broadcast_grace", cfg.BroadcastGrace,
		"metrics_addr", cfg.MetricsAddr,
		"stateless_reset_key_set", len(cfg.StatelessResetKey) > 0,
		"resume_token_key_mode", resumeTokenKeyMode(cfg),
		// R28: the key's presence is the feature switch, so logging whether it
		// is set is how an operator confirms a fleet is collecting at all —
		// the key itself is never logged.
		"telemetry_enabled", len(cfg.TelemetryKey) > 0,
		"telemetry_report_interval", cfg.TelemetryReportInterval,
		"telemetry_advertise_url", cfg.TelemetryAdvertiseURL,
		"server_name", cfg.ServerName,
		"cluster_mode", cfg.ClusterMode,
	)

	getCert, err := certSource(cfg, log)
	if err != nil {
		return err
	}

	// Cluster mode (R17 W3, docs/22): the hub's lifecycle hooks and the
	// coordinator reference each other, so the hooks close over this pointer,
	// which is assigned right after the registry exists. Both hook paths are
	// nil-safe until then (no publisher can connect before Run anyway).
	var coord *cluster.Coordinator
	hubOpts := registryOptions(cfg)
	if cfg.ClusterMode {
		hubOpts.OnPublisherClosed = func(id string) {
			if coord == nil {
				return
			}
			opCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := coord.EnterGrace(opCtx, id); err != nil {
				log.Warn("lease grace stamp failed", "broadcast_id", id, "err", err)
			}
		}
		hubOpts.OnBroadcastExpired = func(id string) {
			if coord == nil {
				return
			}
			opCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := coord.Delete(opCtx, id); err != nil {
				log.Warn("lease delete failed", "broadcast_id", id, "err", err)
			}
		}
	}

	r := hub.NewRegistry(log, hubOpts)

	// Prometheus wiring (R9, docs/13): runtime collectors + build info, the
	// hub registry collector, and the transport connection counters — all
	// served by the TCP ops endpoint alongside /healthz and /statusz.
	promReg := metrics.NewBaseRegistry(version)
	promReg.MustRegister(metrics.NewRegistryCollector(r))
	sm := metrics.NewServerMetrics(promReg)

	// The WebTransport (UDP) server and the ops (TCP) listener run together;
	// either one failing tears the other down.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	srv := transport.New(cfg, r, getCert, log, sm)

	if cfg.ClusterMode {
		var podName string
		coord, podName, err = buildCoordinator(cfg, srv.HandleLeaseDeleted, srv.HandleLeaseLost, log)
		if err != nil {
			return err
		}
		go coord.Run(runCtx)
		srv.SetCluster(coord, podName)
	}

	// The R18 viewer-count pump (docs/23 Decision 4): one registry-wide
	// goroutine, started explicitly here — never inside NewRegistry — so
	// tests drive PumpViewerCounts ticks directly.
	go r.RunViewerCountPump(runCtx)

	errCh := make(chan error, 2)
	go func() { errCh <- srv.Run(runCtx) }()
	go func() { errCh <- ops.Run(runCtx, cfg.MetricsAddr, ops.Handler(r, promReg, log, srv.Ready), log) }()

	var firstErr error
	for range 2 {
		if err := <-errCh; err != nil && firstErr == nil {
			firstErr = err
			cancel()
		}
	}
	if firstErr != nil {
		return firstErr
	}

	log.Info("shutting down")
	return nil
}

// registryOptions maps the parsed config onto hub.Options. Every limit knob
// in config.Config must cross here — a knob that parses but isn't mapped is
// silently inert in production while wired-by-hand tests stay green.
func registryOptions(cfg config.Config) hub.Options {
	return hub.Options{
		MaxSubscribers:                cfg.MaxSubscribers,
		BroadcastGrace:                cfg.BroadcastGrace,
		MaxBroadcasts:                 cfg.MaxBroadcasts,
		MaxTotalSubscribers:           cfg.MaxTotalSubscribers,
		MaxBandwidthBytes:             cfg.MaxBandwidthBytes,
		MaxKeyframeBytes:              cfg.MaxKeyframeBytes,
		KeyframeWriteTimeout:          cfg.KeyframeWriteTimeout,
		DVR:                           hub.DVROptions{Window: cfg.DVRWindow, MaxBytes: cfg.DVRMaxBytes},
		DVRMaxCatchup:                 cfg.DVRMaxCatchup,
		DVRAudio:                      cfg.DVRAudio,
		LiveEdgeAudioOnReliableStream: cfg.LiveEdgeAudioOnReliableStream,
		// R29: plumbed here, not only into the test helper — the R2
		// post-implementation review's finding, and docs/34's FP4 acceptance
		// criterion asserts this production path specifically.
		ParityDefault: cfg.ParityDefault,
		// R30: same rule (docs/35 ST3). The transport reads cfg directly for
		// the dial gate and capability bit; this mirror keeps the hub's
		// options honest and the carry-all-limits test complete.
		StripedDelivery: cfg.StripedDelivery,
		StatsKey:        cfg.StatsKey,
	}
}

// buildCoordinator constructs the cluster coordinator from the in-cluster
// Kubernetes config and the downward-API pod identity (R17 W3). Only called
// when -cluster-mode is on: single-pod deployments never touch the k8s API.
// onLeaseDeleted is the cluster-wide "broadcast ended" dispatch (edge stop +
// local hub expiry); onLeaseLost is the W5 demote path (stale publisher
// close, 4003 to edges, self-demote to edge).
func buildCoordinator(cfg config.Config, onLeaseDeleted func(string), onLeaseLost func(string, cluster.Origin), log *slog.Logger) (*cluster.Coordinator, string, error) {
	restCfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, "", fmt.Errorf("cluster-mode requires in-cluster kubernetes config: %w", err)
	}
	client, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, "", err
	}
	podName := os.Getenv("POD_NAME")
	podIP := os.Getenv("POD_IP")
	namespace := os.Getenv("POD_NAMESPACE")
	if podName == "" || podIP == "" || namespace == "" {
		return nil, "", fmt.Errorf("cluster-mode requires POD_NAME, POD_IP and POD_NAMESPACE (downward API)")
	}
	_, port, err := net.SplitHostPort(cfg.Addr)
	if err != nil {
		return nil, "", fmt.Errorf("cluster-mode: cannot derive advertise port from -addr %q: %w", cfg.Addr, err)
	}
	coord, err := cluster.New(cluster.Options{
		Client:         client,
		Namespace:      namespace,
		PodName:        podName,
		AdvertiseAddr:  net.JoinHostPort(podIP, port),
		BroadcastGrace: cfg.BroadcastGrace,
		MaxBroadcasts:  cfg.MaxBroadcasts,
		Log:            log,
		OnLeaseDeleted: onLeaseDeleted,
		OnLeaseLost:    onLeaseLost,
	})
	if err != nil {
		return nil, "", err
	}
	return coord, podName, nil
}

// resumeTokenKeyMode names where the resume-token key comes from (R17 W2) —
// logged so a fleet misconfiguration (per-process keys on multiple pods,
// which silently breaks cross-pod resume) is visible at startup. Order
// mirrors newResumeTokens: the explicit key wins over the publish-secret
// derivation (PR #47 security review — a secret-derived key is computable
// by every broadcaster holding the secret; "explicit-key" is the mode that
// actually closes the graced-ID hijack between broadcasters).
func resumeTokenKeyMode(cfg config.Config) string {
	switch {
	case len(cfg.ResumeTokenKey) > 0:
		return "explicit-key"
	case cfg.PublishSecret != "":
		return "derived-from-publish-secret"
	default:
		return "per-process-random"
	}
}

// certSource returns the per-handshake certificate callback: an ephemeral
// in-memory dev cert (hashes logged for the browser side), a *persisted* dev
// cert generated once into -cert-file/-key-file, or a reloading file-backed
// pair for production. The full truth table is docs/41 §4.2.1.
func certSource(cfg config.Config, log *slog.Logger) (func(*tls.ClientHelloInfo) (*tls.Certificate, error), error) {
	switch {
	// R38 (docs/41 D3): -dev-cert AND -cert-file means "generate into these
	// paths if absent, otherwise load them". Persisting the pair is what stops
	// every restart invalidating the hash a browser was given, and it moves
	// local dev onto the file-backed path production actually uses.
	case cfg.DevCert && cfg.CertFile != "":
		if cfg.KeyFile == "" {
			return nil, fmt.Errorf("-dev-cert with -cert-file also needs -key-file")
		}
		cert, generated, err := tlsutil.LoadOrGenerate(cfg.CertFile, cfg.KeyFile,
			strings.Split(cfg.DevCertHosts, ","), tlsutil.MaxDevCertValidity)
		if err != nil {
			return nil, err
		}
		// Not `docker compose down -v`: the stack bind-mounts ./certs, so the
		// pair outlives the volumes. One command covers all three lanes.
		logCertIdentity(log, cert.Leaf, "persisted dev certificate",
			"./dev/certs.sh renew (or delete the pair) to regenerate",
			"cert_file", cfg.CertFile, "generated", generated)
		if generated {
			log.Info("chrome flags for this cert",
				"flags", fmt.Sprintf("--origin-to-force-quic-on=localhost%s --ignore-certificate-errors-spki-list=%s",
					cfg.Addr, tlsutil.SPKIFingerprint(cert.Leaf)),
			)
		}
		// The reloader, not the pair just loaded: a developer who replaces the
		// files (./dev/certs.sh renew) gets the new ones without a restart,
		// exactly as production does.
		r, err := tlsutil.NewReloader(cfg.CertFile, cfg.KeyFile, log)
		if err != nil {
			return nil, err
		}
		return r.GetCertificate, nil
	case cfg.DevCert:
		cert, err := tlsutil.GenerateDevCert(strings.Split(cfg.DevCertHosts, ","), tlsutil.MaxDevCertValidity)
		if err != nil {
			return nil, err
		}
		log.Info("generated ephemeral dev certificate",
			"hosts", cfg.DevCertHosts,
			"not_after", cert.Leaf.NotAfter,
			"spki_fingerprint", tlsutil.SPKIFingerprint(cert.Leaf),
			"cert_hash_hex", tlsutil.CertHashHex(cert.Leaf),
		)
		log.Info("chrome flags for this cert",
			"flags", fmt.Sprintf("--origin-to-force-quic-on=localhost%s --ignore-certificate-errors-spki-list=%s",
				cfg.Addr, tlsutil.SPKIFingerprint(cert.Leaf)),
		)
		return func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &cert, nil }, nil
	case cfg.CertFile != "":
		r, err := tlsutil.NewReloader(cfg.CertFile, cfg.KeyFile, log)
		if err != nil {
			return nil, err
		}
		// R38: the file-backed arm used to log nothing identifying, which left
		// a developer running against a persisted dev cert with no way to
		// obtain its hash — the ephemeral arm's log was the only source.
		if cert, err := r.GetCertificate(nil); err == nil && cert != nil && cert.Leaf != nil {
			logCertIdentity(log, cert.Leaf, "loaded certificate",
				"./dev/certs.sh renew", "cert_file", cfg.CertFile)
		}
		return r.GetCertificate, nil
	default:
		return nil, fmt.Errorf("no certificate configured: pass -dev-cert or -cert-file/-key-file")
	}
}

// certExpiryWarning is how close to NotAfter a certificate has to be before
// the relay says so at startup. 72 h is comfortably longer than a working day
// and comfortably shorter than the 14-day dev-cert life, so it fires while
// there is still time to act rather than on the morning it breaks.
const certExpiryWarning = 72 * time.Hour

// logCertIdentity logs what the browser side of a local stack needs (the hex
// DER hash) plus the two values that make an expiry failure legible, and
// warns when the certificate is nearly out of time. remedy names the command
// that replaces it, which differs per lane (docs/41 §4.2.1).
func logCertIdentity(log *slog.Logger, leaf *x509.Certificate, msg, remedy string, extra ...any) {
	if leaf == nil {
		return
	}
	args := append([]any{
		"not_after", leaf.NotAfter,
		"cert_hash_hex", tlsutil.CertHashHex(leaf),
		"spki_fingerprint", tlsutil.SPKIFingerprint(leaf),
	}, extra...)
	log.Info(msg, args...)

	if remaining := time.Until(leaf.NotAfter); remaining < certExpiryWarning {
		log.Warn("certificate expires soon",
			"not_after", leaf.NotAfter,
			"remaining", remaining.Round(time.Minute),
			"remedy", remedy,
		)
	}
}
