// gawk-server is the WebTransport relay for the gawk game stream:
// one publisher fans out encoded video datagrams to a small set of
// subscribers.
package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/hub"
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
		"max_idle_timeout", cfg.MaxIdleTimeout,
		"keepalive_period", cfg.KeepAlivePeriod,
		"broadcast_grace", cfg.BroadcastGrace,
	)

	getCert, err := certSource(cfg, log)
	if err != nil {
		return err
	}

	r := hub.NewRegistry(log, registryOptions(cfg))
	if err := transport.New(cfg, r, getCert, log).Run(ctx); err != nil {
		return err
	}

	log.Info("shutting down")
	return nil
}

// registryOptions maps the parsed config onto hub.Options. Every limit knob
// in config.Config must cross here — a knob that parses but isn't mapped is
// silently inert in production while wired-by-hand tests stay green.
func registryOptions(cfg config.Config) hub.Options {
	return hub.Options{
		MaxSubscribers:      cfg.MaxSubscribers,
		BroadcastGrace:      cfg.BroadcastGrace,
		MaxBroadcasts:       cfg.MaxBroadcasts,
		MaxTotalSubscribers: cfg.MaxTotalSubscribers,
		MaxBandwidthBytes:   cfg.MaxBandwidthBytes,
	}
}

// certSource returns the per-handshake certificate callback: an ephemeral
// in-memory dev cert (hashes logged for the browser side), or a reloading
// file-backed pair for production.
func certSource(cfg config.Config, log *slog.Logger) (func(*tls.ClientHelloInfo) (*tls.Certificate, error), error) {
	switch {
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
		return r.GetCertificate, nil
	default:
		return nil, fmt.Errorf("no certificate configured: pass -dev-cert or -cert-file/-key-file")
	}
}
