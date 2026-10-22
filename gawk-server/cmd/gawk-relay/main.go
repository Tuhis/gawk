// gawk-relay is the standalone LAN relay (R74, docs/76 §6.1): the same relay
// as gawk-server, with LAN-shaped defaults, a state directory and a rolling
// pinned certificate, so a party host can start it with no flags at all.
// It is a separate binary precisely so gawk-server keeps refusing to start
// without a certificate (docs/41 §4.2.1).
package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/lanaddr"
	"github.com/Tuhis/gawk/gawk-server/internal/relaystate"
	"github.com/Tuhis/gawk/gawk-server/internal/serve"
	"github.com/Tuhis/gawk/gawk-server/internal/tlsutil"
)

// version is stamped at build time via -ldflags "-X main.version=..."; "dev"
// for plain go build/run.
var version = "dev"

// identity names this process in events when there is no POD_NAME and no
// hostname, and is the 0x11 product a client sees in logs.
const identity = "gawk-relay"

// lanRefresh is how often the LAN address is re-detected (a DHCP change is
// republished, docs/76 §6.1 item 4).
const lanRefresh = 30 * time.Second

// certTick drives the rolling certificate's rotation when no handshake
// does, so the published hashes never lag the schedule by more than this.
const certTick = time.Minute

// certsSubdir holds the rolling certificate's slots inside the state dir.
const certsSubdir = "certs"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Getenv)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "gawk-relay:", err)
		os.Exit(1)
	}
}

// Relay is what this binary's parts see: the resolved configuration and the
// relay-only subsystems built before serve.Run. Rolling is nil when an
// explicit -cert-file/-key-file pair overrides the pinned certificate.
type Relay struct {
	Cfg      config.Config
	Relay    config.RelayConfig
	StateDir string
	Rolling  *tlsutil.Rolling
	LAN      *lanaddr.Watcher
	Log      *slog.Logger
}

// Part is one feature of the binary in its own file (the console, the PoP
// link): it returns goroutines and pre-run hooks for serve.Run. Parts
// register from init so adding one never edits this file.
type Part func(r *Relay) (extra []func(ctx context.Context, w serve.Wired) error, preRun []func(w serve.Wired) error, err error)

var parts []Part

// registerPart adds a Part; call it from an init func.
func registerPart(p Part) { parts = append(parts, p) }

// run is main without the process, so the tests drive the real startup.
func run(ctx context.Context, args []string, getenv func(string) string) error {
	cfg, rc, err := config.ParseRelayFlags(args, getenv)
	if err != nil {
		return err
	}
	cfg.ReleaseVersion = version

	log := serve.NewLogger(cfg, os.Stdout)
	slog.SetDefault(log)

	if rc.StateDir == "" {
		if rc.StateDir, err = relaystate.DefaultDir(runtime.GOOS, getenv); err != nil {
			return err
		}
	}
	if err := relaystate.Ensure(rc.StateDir); err != nil {
		return err
	}
	// Before serve.Run: the transport, the hub and the resume-token minter
	// read the keys once, at construction.
	if err := relaystate.ApplyPersistedKeys(&cfg, rc.StateDir); err != nil {
		return err
	}
	if rc.DevMode {
		log.Warn("-dev-cert is ignored: gawk-relay always serves its rolling pinned certificate (or -cert-file/-key-file)")
		cfg.DevCert = false
	}

	lan := lanaddr.NewWatcher(rc.LANAddress, lanRefresh)
	// Synchronously, so the first certificate already names the LAN address.
	lan.Refresh()

	r := &Relay{Cfg: cfg, Relay: rc, StateDir: rc.StateDir, LAN: lan, Log: log}
	getCert, err := certificate(r)
	if err != nil {
		return err
	}

	serve.LogStartup(log, cfg, version, "relay", rc.Sanitized(), "lan_address", lan.Current().String())
	if r.Rolling != nil {
		cur, next := r.Rolling.Hashes()
		log.Info("pinned certificate", "cert_hash_hex", cur, "next_cert_hash_hex", next,
			"dir", filepath.Join(rc.StateDir, certsSubdir))
	}

	opts := serve.Options{
		Version:  version,
		Identity: identity,
		Extra:    []func(ctx context.Context, w serve.Wired) error{func(ctx context.Context, _ serve.Wired) error { return lan.Run(ctx) }},
	}
	if r.Rolling != nil {
		opts.Extra = append(opts.Extra, func(ctx context.Context, _ serve.Wired) error {
			t := time.NewTicker(certTick)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return nil
				case <-t.C:
					r.Rolling.Tick()
				}
			}
		})
	}
	for _, p := range parts {
		extra, pre, err := p(r)
		if err != nil {
			return err
		}
		opts.Extra = append(opts.Extra, extra...)
		opts.PreRun = append(opts.PreRun, pre...)
	}
	return serve.Run(ctx, cfg, getCert, log, opts)
}

// certificate picks the relay's certificate: an explicit -cert-file and
// -key-file pair wins (a host with a real certificate for the venue's name);
// otherwise the rolling pinned certificate in the state directory.
func certificate(r *Relay) (serve.CertFunc, error) {
	if r.Cfg.CertFile != "" {
		rl, err := tlsutil.NewReloader(r.Cfg.CertFile, r.Cfg.KeyFile, r.Log)
		if err != nil {
			return nil, err
		}
		if cert, err := rl.GetCertificate(nil); err == nil && cert != nil && cert.Leaf != nil {
			serve.LogCertIdentity(r.Log, cert.Leaf, "loaded certificate",
				"replace the files; they are reloaded without a restart", "cert_file", r.Cfg.CertFile)
		}
		return rl.GetCertificate, nil
	}
	rolling, err := tlsutil.NewRolling(tlsutil.RollingOptions{
		Dir:   filepath.Join(r.StateDir, certsSubdir),
		Hosts: func() []string { return certHosts(r.LAN) },
		Log:   r.Log,
	})
	if err != nil {
		return nil, err
	}
	r.Rolling = rolling
	return func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) { return rolling.GetCertificate(hello) }, nil
}

// certHosts is the rolling certificate's SANs: every LAN candidate plus the
// loopback names for the host's own machine. Pinned clients do not check
// them (docs/76 M5); they are there for tools that do.
func certHosts(lan *lanaddr.Watcher) []string {
	var hosts []string
	seen := map[string]bool{}
	add := func(s string) {
		if s != "" && !seen[s] {
			seen[s] = true
			hosts = append(hosts, s)
		}
	}
	if cur := lan.Current(); cur.IsValid() {
		add(cur.String())
	}
	for _, c := range lan.Candidates() {
		add(c.Addr.String())
	}
	add("localhost")
	add("127.0.0.1")
	return hosts
}
