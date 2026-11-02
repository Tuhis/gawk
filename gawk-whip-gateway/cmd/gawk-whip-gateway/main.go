// Command gawk-whip-gateway takes WHIP ingest from OBS (or any WHIP sender)
// and republishes it to a gawk relay (R33, docs/83). See the module README
// for the flags.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Tuhis/gawk/gawk-whip-gateway/internal/config"
	"github.com/Tuhis/gawk/gawk-whip-gateway/internal/gateway"
)

// version is set at build time (-ldflags "-X main.version=…").
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stderr, nil))
}

// Listening is what run reports once every listener is up (tests).
type Listening struct {
	Public, Internal, Ops string
	MediaPort             int
}

// run is main with its inputs injectable; ready, when set, receives the
// bound addresses.
func run(args []string, getenv func(string) string, stderr io.Writer, ready chan<- Listening) int {
	cfg, err := config.Parse(args, getenv)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintln(stderr, "gawk-whip-gateway:", err)
		return 2
	}
	hopts := &slog.HandlerOptions{Level: cfg.LogLevel}
	var log *slog.Logger
	if cfg.LogFormat == "json" {
		log = slog.New(slog.NewJSONHandler(stderr, hopts))
	} else {
		log = slog.New(slog.NewTextHandler(stderr, hopts))
	}
	log.Info("gawk-whip-gateway starting", append([]any{"version", version}, cfg.LogAttrs()...)...)
	opts := cfg.Options(version, log)
	opts.Metrics = gateway.NewMetrics(version)
	srv, err := gateway.New(opts)
	if err != nil {
		log.Error("start-up failed", "err", err)
		return 1
	}
	defer srv.Close()

	type listener struct {
		name string
		addr string
		h    http.Handler
		tls  bool
	}
	ls := []listener{
		{"public", cfg.Listen, srv.PublicHandler(), cfg.TLSCertFile != ""},
		{"internal", cfg.InternalListen, srv.InternalHandler(), false},
		{"ops", cfg.OpsListen, srv.OpsHandler(), false},
	}
	servers := make([]*http.Server, len(ls))
	bound := make([]string, len(ls))
	errc := make(chan error, len(ls))
	for i, l := range ls {
		ln, err := net.Listen("tcp", l.addr)
		if err != nil {
			log.Error("start-up failed", "listener", l.name, "err", err)
			return 1
		}
		bound[i] = ln.Addr().String()
		hs := &http.Server{
			Handler:           l.h,
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       30 * time.Second,
			WriteTimeout:      30 * time.Second,
			IdleTimeout:       2 * time.Minute,
		}
		servers[i] = hs
		go func() {
			var err error
			if l.tls {
				err = hs.ServeTLS(ln, cfg.TLSCertFile, cfg.TLSKeyFile)
			} else {
				err = hs.Serve(ln)
			}
			if !errors.Is(err, http.ErrServerClosed) {
				errc <- fmt.Errorf("%s listener: %w", l.name, err)
			}
		}()
	}
	log.Info("listening", "public", bound[0], "internal", bound[1], "ops", bound[2], "media_port", srv.MediaPort())
	if ready != nil {
		ready <- Listening{Public: bound[0], Internal: bound[1], Ops: bound[2], MediaPort: srv.MediaPort()}
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	code := 0
	select {
	case <-ctx.Done():
	case err := <-errc:
		log.Error("serve failed", "err", err)
		code = 1
	}
	// A pod restart is an ICE failure for its sessions: they close with
	// code 0, the broadcasts wait in the relay's grace and OBS reconnects
	// to another pod (D10).
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, hs := range servers {
		_ = hs.Shutdown(shutdown)
	}
	srv.Close()
	log.Info("stopped")
	return code
}
