// gawk-server is the WebTransport relay for the gawk game stream:
// one publisher fans out encoded video datagrams to a small set of
// subscribers. The relay itself lives in internal/serve, shared with
// gawk-relay (docs/76); this binary owns only its profile and its
// certificate selection, which fails closed (docs/41 §4.2.1).
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
	"github.com/Tuhis/gawk/gawk-server/internal/serve"
	"github.com/Tuhis/gawk/gawk-server/internal/tlsutil"
)

// version is stamped at build time via -ldflags "-X main.version=..." (see
// deploy/Dockerfile); "dev" for plain go build/run.
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Getenv)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "gawk-server:", err)
		os.Exit(1)
	}
}

// run is main without the process: flags and environment are injected so a
// test can drive the real startup path (TestZeroFlagsRefuse).
func run(ctx context.Context, args []string, getenv func(string) string) error {
	cfg, err := config.ParseFlags(args, getenv)
	if err != nil {
		return err
	}
	cfg.ReleaseVersion = version

	log := serve.NewLogger(cfg, os.Stdout)
	slog.SetDefault(log)

	serve.LogStartup(log, cfg, version)

	getCert, err := certSource(cfg, log)
	if err != nil {
		return err
	}
	return serve.Run(ctx, cfg, getCert, log, serve.Options{Version: version, Identity: "gawk-server"})
}

// certSource returns the per-handshake certificate callback: an ephemeral
// in-memory dev cert (hashes logged for the browser side), a *persisted* dev
// cert generated once into -cert-file/-key-file, or a reloading file-backed
// pair for production. The full truth table is docs/41 §4.2.1.
func certSource(cfg config.Config, log *slog.Logger) (func(*tls.ClientHelloInfo) (*tls.Certificate, error), error) {
	switch {
	// -dev-cert AND -cert-file means "generate into these paths if absent,
	// otherwise load them". Persisting the pair stops every restart
	// invalidating the hash a browser was given, and puts local dev on the
	// file-backed path production uses.
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
		serve.LogCertIdentity(log, cert.Leaf, "persisted dev certificate",
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
		// Logged so a developer on a persisted dev cert can obtain its hash.
		if cert, err := r.GetCertificate(nil); err == nil && cert != nil && cert.Leaf != nil {
			// This arm is both the dev stack's ACME lane and every real
			// deployment, so the remedy has to name both: ./dev/certs.sh does
			// not exist inside the image and means nothing for a mounted
			// Secret.
			serve.LogCertIdentity(log, cert.Leaf, "loaded certificate",
				"locally: ./dev/certs.sh renew — in a deployment the CA/cert-manager renews the mounted Secret (docs/self-hosting.md)",
				"cert_file", cfg.CertFile)
		}
		return r.GetCertificate, nil
	default:
		return nil, fmt.Errorf("no certificate configured: pass -dev-cert or -cert-file/-key-file")
	}
}
