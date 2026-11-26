// Package config holds gawk-mmproxy's configuration (docs/92 §5.1).
//
// Precedence: command-line flag > GAWK_MMPROXY_* environment variable >
// default, the contract every gawk binary keeps. Every knob is validated at
// start-up and handed by Options to the object that consumes it; the
// carry-all test walks every flag there (CODE-REVIEW.md, the R2 lesson).
package config

import (
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Tuhis/gawk/gawk-mmproxy/internal/ops"
	"github.com/Tuhis/gawk/gawk-mmproxy/internal/proxy"
	"github.com/Tuhis/gawk/gawk-mmproxy/internal/routes"
)

// EnvPrefix prefixes every environment twin: -max-conns is
// GAWK_MMPROXY_MAX_CONNS.
const EnvPrefix = "GAWK_MMPROXY_"

// Defaults (docs/92 §5.1).
const (
	DefaultListen        = ":64739"
	DefaultOpsListen     = ":2114"
	DefaultMurmurPort    = 64738
	DefaultHeaderTimeout = 2 * time.Second
	DefaultMaxConns      = 1024
)

// Routes is `gawk-mmproxy routes`.
type Routes struct {
	RouteTable uint32
	LogLevel   slog.Level
	LogFormat  string
}

// Serve is `gawk-mmproxy serve`.
type Serve struct {
	Listen        string
	OpsListen     string
	MurmurPort    uint16
	RouteTable    uint32
	RequireRoutes bool
	HeaderTimeout time.Duration
	MaxConns      int
	LogLevel      slog.Level
	LogFormat     string
}

func envName(flagName string) string {
	return EnvPrefix + strings.ToUpper(strings.ReplaceAll(flagName, "-", "_"))
}

type flags struct {
	fs     *flag.FlagSet
	getenv func(string) string
	envErr error
}

func (f *flags) str(name, def, usage string) *string {
	if v := f.getenv(envName(name)); v != "" {
		def = v
	}
	return f.fs.String(name, def, usage+" (env "+envName(name)+")")
}

func (f *flags) boolean(name string, def bool, usage string) *bool {
	if v := f.getenv(envName(name)); v != "" {
		b, err := strconv.ParseBool(strings.TrimSpace(v))
		if err != nil && f.envErr == nil {
			f.envErr = fmt.Errorf("invalid %s %q (want true or false)", envName(name), v)
		}
		def = b
	}
	return f.fs.Bool(name, def, usage+" (env "+envName(name)+")")
}

func (f *flags) parse(args []string) error {
	if err := f.fs.Parse(args); err != nil {
		return err
	}
	if f.fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", f.fs.Arg(0))
	}
	return f.envErr
}

// Output receives -h and flag errors; main points it at its stderr.
var Output io.Writer = os.Stderr

func newFlags(cmd string, getenv func(string) string) *flags {
	fs := flag.NewFlagSet("gawk-mmproxy "+cmd, flag.ContinueOnError)
	fs.SetOutput(Output)
	return &flags{fs: fs, getenv: getenv}
}

func common(f *flags) (table, level, format *string) {
	table = f.str("route-table", strconv.Itoa(routes.DefaultTable), "the policy-routing table of the loopback routes (docs/92 D4)")
	level = f.str("log-level", "info", "log level: debug|info|warn|error")
	format = f.str("log-format", "text", "log format: text|json")
	return
}

func parseCommon(table, level, format string) (uint32, slog.Level, string, error) {
	t, err := strconv.ParseUint(strings.TrimSpace(table), 10, 32)
	// 0 and 253-255 are the kernel's unspec, default, main and local tables.
	if err != nil || t == 0 || (t >= 253 && t <= 255) {
		return 0, 0, "", fmt.Errorf("invalid -route-table %q (want 1-252 or above 255)", table)
	}
	var l slog.Level
	if err := l.UnmarshalText([]byte(strings.TrimSpace(level))); err != nil {
		return 0, 0, "", fmt.Errorf("invalid -log-level %q", level)
	}
	format = strings.TrimSpace(format)
	if format != "text" && format != "json" {
		return 0, 0, "", fmt.Errorf("invalid -log-format %q (want text or json)", format)
	}
	return uint32(t), l, format, nil
}

// ParseRoutes resolves `gawk-mmproxy routes`.
func ParseRoutes(args []string, getenv func(string) string) (Routes, error) {
	f := newFlags("routes", getenv)
	table, level, format := common(f)
	if err := f.parse(args); err != nil {
		return Routes{}, err
	}
	t, l, fm, err := parseCommon(*table, *level, *format)
	if err != nil {
		return Routes{}, err
	}
	return Routes{RouteTable: t, LogLevel: l, LogFormat: fm}, nil
}

type serveFlags struct {
	listen, ops, murmurPort, headerTimeout, maxConns, table, level, format *string
	require                                                                *bool
}

func defineServe(f *flags) serveFlags {
	var v serveFlags
	v.listen = f.str("listen", DefaultListen, "where bridges send PROXY v2 headers; reachable from the bridge only (docs/92 D9, D16)")
	v.ops = f.str("ops-listen", DefaultOpsListen, "ops listener: /healthz, /readyz, /metrics; never routed publicly")
	v.murmurPort = f.str("murmur-port", strconv.Itoa(DefaultMurmurPort), "murmur's port on loopback (docs/92 D3)")
	v.require = f.boolean("require-routes", true, "fail closed without the loopback routes (docs/92 D5); false only where every source is a loopback address")
	v.headerTimeout = f.str("header-timeout", DefaultHeaderTimeout.String(), "the deadline for a complete PROXY v2 header, 100ms to 30s")
	v.maxConns = f.str("max-conns", strconv.Itoa(DefaultMaxConns), "slots for proxied and draining connections (docs/92 D6)")
	v.table, v.level, v.format = common(f)
	return v
}

// ParseServe resolves `gawk-mmproxy serve`.
func ParseServe(args []string, getenv func(string) string) (Serve, error) {
	f := newFlags("serve", getenv)
	v := defineServe(f)
	listen, ops, murmurPort, require, headerTimeout, maxConns := v.listen, v.ops, v.murmurPort, v.require, v.headerTimeout, v.maxConns
	table, level, format := v.table, v.level, v.format
	if err := f.parse(args); err != nil {
		return Serve{}, err
	}
	s := Serve{Listen: strings.TrimSpace(*listen), OpsListen: strings.TrimSpace(*ops), RequireRoutes: *require}
	for _, l := range []struct{ name, v string }{{"-listen", s.Listen}, {"-ops-listen", s.OpsListen}} {
		if _, _, err := net.SplitHostPort(l.v); err != nil {
			return Serve{}, fmt.Errorf("invalid %s %q: %v", l.name, l.v, err)
		}
	}
	p, err := strconv.ParseUint(strings.TrimSpace(*murmurPort), 10, 16)
	if err != nil || p == 0 {
		return Serve{}, fmt.Errorf("invalid -murmur-port %q", *murmurPort)
	}
	s.MurmurPort = uint16(p)
	if s.HeaderTimeout, err = time.ParseDuration(strings.TrimSpace(*headerTimeout)); err != nil || s.HeaderTimeout < 100*time.Millisecond || s.HeaderTimeout > 30*time.Second {
		return Serve{}, fmt.Errorf("invalid -header-timeout %q (want 100ms to 30s)", *headerTimeout)
	}
	if s.MaxConns, err = strconv.Atoi(strings.TrimSpace(*maxConns)); err != nil || s.MaxConns < 1 {
		return Serve{}, fmt.Errorf("invalid -max-conns %q (want a positive integer)", *maxConns)
	}
	if s.RouteTable, s.LogLevel, s.LogFormat, err = parseCommon(*table, *level, *format); err != nil {
		return Serve{}, err
	}
	return s, nil
}

// Options hands every serve knob to the proxy (the production call site).
func (s Serve) Options() proxy.Options {
	return proxy.Options{
		Listen:        s.Listen,
		MurmurPort:    s.MurmurPort,
		RouteTable:    s.RouteTable,
		RequireRoutes: s.RequireRoutes,
		HeaderTimeout: s.HeaderTimeout,
		MaxConns:      s.MaxConns,
	}
}

// OpsOptions hands the ops knobs to the ops listener; routesFn is the
// proxy's last routes check.
func (s Serve) OpsOptions(routesFn func() (routes.State, error)) ops.Options {
	return ops.Options{Listen: s.OpsListen, MurmurPort: s.MurmurPort, RequireRoutes: s.RequireRoutes, Routes: routesFn}
}

// LogAttrs is the start-up log line. Nothing here is secret, and no client
// address exists yet.
func (s Serve) LogAttrs() []any {
	return []any{"listen", s.Listen, "ops_listen", s.OpsListen, "murmur_port", s.MurmurPort,
		"route_table", s.RouteTable, "require_routes", s.RequireRoutes,
		"header_timeout", s.HeaderTimeout.String(), "max_conns", s.MaxConns}
}
