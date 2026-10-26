// Command gawk-mumble-bridge carries a gawk room's voice to and from a
// Mumble server (R79, docs/80). See the module README for the flags.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/Tuhis/gawk/gawk-mumble-bridge/internal/bridge"
	"github.com/Tuhis/gawk/gawk-mumble-bridge/internal/config"
)

// version is set at build time (-ldflags "-X main.version=…").
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stderr, nil))
}

// run is main with its inputs injectable; ready, when set, receives the
// server once it is listening (tests).
func run(args []string, getenv func(string) string, stderr io.Writer, ready chan<- *bridge.Server) int {
	cfg, err := config.Parse(args, getenv)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintln(stderr, "gawk-mumble-bridge:", err)
		return 2
	}
	hopts := &slog.HandlerOptions{Level: cfg.LogLevel}
	var log *slog.Logger
	if cfg.LogFormat == "json" {
		log = slog.New(slog.NewJSONHandler(stderr, hopts))
	} else {
		log = slog.New(slog.NewTextHandler(stderr, hopts))
	}
	log.Info("gawk-mumble-bridge starting", append([]any{"version", version}, cfg.LogAttrs()...)...)
	srv, err := bridge.New(cfg.Options(version, log))
	if err != nil {
		log.Error("start-up failed", "err", err)
		return 1
	}
	if err := srv.Listen(); err != nil {
		log.Error("start-up failed", "err", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	if ready != nil {
		ready <- srv
	}
	if err := srv.Serve(ctx); err != nil {
		log.Error("serve failed", "err", err)
		return 1
	}
	log.Info("drained, exiting")
	return 0
}
