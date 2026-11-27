// Command gawk-mmproxy gives murmur each bridged web user's real address
// (R88, docs/92). It runs in murmur's network namespace (a container in
// murmur's pod) and has two subcommands:
//
//	gawk-mmproxy routes   install the loopback policy routes (an init
//	                      container with CAP_NET_ADMIN); always exits 0
//	gawk-mmproxy serve    take PROXY v2 headers from gawk-mumble-bridge
//	                      and dial murmur over loopback from each header's
//	                      source address (CAP_NET_RAW only)
//
// Neither ever logs a client address (docs/92 D10).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/Tuhis/gawk/gawk-mmproxy/internal/config"
	"github.com/Tuhis/gawk/gawk-mmproxy/internal/metrics"
	"github.com/Tuhis/gawk/gawk-mmproxy/internal/ops"
	"github.com/Tuhis/gawk/gawk-mmproxy/internal/proxy"
	"github.com/Tuhis/gawk/gawk-mmproxy/internal/routes"
)

var version = "dev"

const usage = `usage: gawk-mmproxy <routes|serve|version> [flags]

  routes   install the loopback policy routes; run as an init container
           with CAP_NET_ADMIN in murmur's pod. Always exits 0 (docs/92 D4).
  serve    the proxy; CAP_NET_RAW only (docs/92 D5).
  version  print the version.

See gawk-mmproxy/README.md and docs/92.
`

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stderr))
}

func run(args []string, getenv func(string) string, stderr io.Writer) int {
	config.Output = stderr
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "routes":
		c, err := config.ParseRoutes(args[1:], getenv)
		if err != nil {
			return flagErr(err, stderr)
		}
		return runRoutes(c, logger(stderr, c.LogLevel, c.LogFormat))
	case "serve":
		c, err := config.ParseServe(args[1:], getenv)
		if err != nil {
			return flagErr(err, stderr)
		}
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
		defer stop()
		return runServe(ctx, c, logger(stderr, c.LogLevel, c.LogFormat), nil)
	case "version", "-version", "--version":
		fmt.Fprintln(stderr, "gawk-mmproxy", version)
		return 0
	}
	fmt.Fprint(stderr, usage)
	return 2
}

func flagErr(err error, stderr io.Writer) int {
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	fmt.Fprintln(stderr, "gawk-mmproxy:", err)
	return 2
}

func logger(w io.Writer, level slog.Level, format string) *slog.Logger {
	o := &slog.HandlerOptions{Level: level}
	if format == "json" {
		return slog.New(slog.NewJSONHandler(w, o))
	}
	return slog.New(slog.NewTextHandler(w, o))
}

// runRoutes never fails the pod (docs/92 D4, D15): a refusal is logged and
// `serve` then fails closed and reports why.
func runRoutes(c config.Routes, log *slog.Logger) int {
	for _, r := range routes.Install(c.RouteTable) {
		switch {
		case r.Skipped != "":
			log.Warn("routes: family skipped", "family", r.Family, "why", r.Skipped)
		case r.Err != nil:
			log.Error("routes: not installed; serve will refuse every connection until they are (does this container have CAP_NET_ADMIN?)",
				"family", r.Family, "err", r.Err.Error())
		default:
			log.Info("routes: in place", "family", r.Family, "table", c.RouteTable)
		}
	}
	return 0
}

// runServe serves until ctx ends. ready, when set, receives the bound
// proxy and ops addresses (tests).
func runServe(ctx context.Context, c config.Serve, log *slog.Logger, ready func(proxyAddr, opsAddr string)) int {
	m := metrics.New()
	s := proxy.New(c.Options(), m)
	if err := s.Listen(); err != nil {
		log.Error("listen", "err", err.Error())
		return 1
	}
	opsLn, err := net.Listen("tcp", c.OpsListen)
	if err != nil {
		log.Error("ops listen", "err", err.Error())
		return 1
	}
	oo := c.OpsOptions(s.Routes)
	log.Info("gawk-mmproxy serving", append([]any{"version", version}, c.LogAttrs()...)...)
	if st, err := s.Routes(); c.RequireRoutes && (err != nil || !st.V4) {
		// Not an exit (docs/92 D15): serve fails closed until they appear.
		log.Error("the loopback policy routes are missing; every connection is refused until they appear (run `gawk-mmproxy routes` as an init container with CAP_NET_ADMIN)")
	}
	if ok, why := ops.Ready(oo); !ok {
		log.Warn("not ready yet", "why", why)
	}
	if ready != nil {
		ready(s.Addr().String(), opsLn.Addr().String())
	}
	go func() {
		if err := ops.Serve(ctx, opsLn, ops.Handler(oo, m)); err != nil {
			log.Error("ops listener", "err", err.Error())
		}
	}()
	if err := s.Serve(ctx); err != nil {
		log.Error("listener failed", "err", err.Error())
		return 1
	}
	log.Info("gawk-mmproxy stopped")
	return 0
}
