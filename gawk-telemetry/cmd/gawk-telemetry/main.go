// Command gawk-telemetry is R28's optional per-session diagnostics service
// (docs/33).
//
// It runs TWO listeners, and the split is the security posture (D1/D14):
//
//   - **The ingest listener is public.** It is the only thing a viewer's
//     browser can reach, routed from a same-origin path on the frontend's own
//     Ingress. Its only defence is the relay-minted session token, which is
//     what makes an unauthenticated public write surface tolerable.
//   - **The read listener is not.** The dashboard, the read API and the MCP
//     server all sit here. The read side aggregates every broadcast on the
//     fleet, and it should be no more reachable than /statusz is today (R9 D1's
//     posture).
//
// The relay is SCRAPED from here and never pushes: the process carrying every
// broadcast's hot path must not grow an outbound HTTP client or a queue (D5).
package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Tuhis/gawk/gawk-server/wire"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/dashboard"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/ingest"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/live"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/mcp"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/readapi"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/relayscrape"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/rollup"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/sessions"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/store"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := parseFlags(os.Args[1:], os.Getenv)
	if err != nil {
		return err
	}
	log := newLogger(cfg)

	st, err := store.New(store.Options{Root: cfg.dataDir})
	if err != nil {
		return fmt.Errorf("data dir: %w", err)
	}
	defer st.Close()

	// Crash recovery, at startup: a process that died mid-session left plain
	// .ndjson files behind, and a directory scan that gzips them is obviously
	// correct in a way appending to a gzip stream would not be (D3).
	if n, err := st.SweepOrphans(cfg.sessionIdle); err != nil {
		log.Warn("orphan sweep failed", "err", err)
	} else if n > 0 {
		log.Info("finalized orphaned sessions from a previous run", "count", n)
	}

	projection := live.New(nil)
	api, err := readapi.New(readapi.Options{
		Store: st, Live: projection, DashboardBase: cfg.dashboardBase,
	})
	if err != nil {
		return err
	}

	writer := sessions.NewWriter(sessions.Options{
		Store:       st,
		Log:         log,
		IdleTimeout: cfg.sessionIdle,
		// The stored verdict and a later diagnose() of the same session come
		// out of ONE code path, so "has this got better since the R15 fix?"
		// compares like with like.
		Finalize: sessions.RollupFinalizer(st, log, func(row rollup.Row, in rollup.Input) json.RawMessage {
			relayLines, _ := st.ReadRelay(time.UnixMilli(in.EndedAtMs).UTC().Format(store.DateLayout))
			rep := api.DiagnoseRow(row, in, relayLines)
			b, err := json.Marshal(rep)
			if err != nil {
				return nil
			}
			return b
		}, nil),
		Observe: func(l sessions.Live, a ingest.Accepted) {
			projection.ObserveClient(a, l.App.Browser, l.App.OS, l.App.Version)
		},
	})

	handler, err := ingest.New(ingest.Options{
		Key: cfg.key, Sink: writer, Log: log,
		RatePerSec: cfg.rateLimit, Burst: cfg.rateBurst,
	})
	if err != nil {
		return err
	}

	scraper, err := relayscrape.New(relayscrape.Options{
		Resolve:  cfg.resolver(),
		Sink:     &scrapeSink{store: st, projection: projection},
		Log:      log,
		Interval: cfg.scrapeInterval,
	})
	if err != nil {
		return err
	}

	log.Info("starting",
		"version", version,
		"ingest_addr", cfg.ingestAddr,
		"read_addr", cfg.readAddr,
		"data_dir", cfg.dataDir,
		"retention_days", cfg.retentionDays,
		"scrape_interval", cfg.scrapeInterval,
		"relay_targets", cfg.relayTargetDescription(),
		"mcp_enabled", cfg.mcpEnabled,
		"query_sql_enabled", cfg.enableSQL,
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go scraper.Run(ctx)
	go maintenance(ctx, log, st, writer, cfg)

	// --- public listener: ingest ONLY -------------------------------------
	ingestMux := http.NewServeMux()
	ingestMux.Handle("POST /api/telemetry/v1/ingest", handler)
	// Also mounted without the Ingress prefix, so a path-stripping proxy and a
	// direct dial both work without a second deployment shape.
	ingestMux.Handle("POST /v1/ingest", handler)
	ingestMux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	// --- read listener: dashboard + read API + MCP -------------------------
	dash, err := dashboard.Handler()
	if err != nil {
		return err
	}
	readMux := http.NewServeMux()
	readMux.Handle("/", dash)
	readMux.Handle("/v1/", api.Handler())
	readMux.Handle("GET /live", api.Handler())
	if cfg.mcpEnabled {
		mcpSrv, err := mcp.New(mcp.Options{API: api, EnableSQL: cfg.enableSQL})
		if err != nil {
			return err
		}
		readMux.Handle("/mcp", mcpSrv)
	}

	var readHandler http.Handler = readMux
	if cfg.basicAuthUser != "" {
		readHandler = basicAuth(readMux, cfg.basicAuthUser, cfg.basicAuthPass)
	}

	ingestSrv := &http.Server{Addr: cfg.ingestAddr, Handler: ingestMux, ReadHeaderTimeout: 10 * time.Second}
	readSrv := &http.Server{Addr: cfg.readAddr, Handler: readHandler, ReadHeaderTimeout: 10 * time.Second}

	errCh := make(chan error, 2)
	go func() { errCh <- serve(ingestSrv, "ingest", log) }()
	go func() { errCh <- serve(readSrv, "read", log) }()

	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err := <-errCh:
		if err != nil {
			return err
		}
	}

	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = ingestSrv.Shutdown(shutCtx)
	_ = readSrv.Shutdown(shutCtx)
	// Every open session ends properly rather than being left to the next
	// process's orphan sweep.
	writer.FinalizeAll()
	return nil
}

func serve(s *http.Server, name string, log *slog.Logger) error {
	if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("listener failed", "listener", name, "err", err)
		return err
	}
	return nil
}

// maintenance runs the periodic housekeeping: finalize vanished sessions,
// release idle file handles, and prune expired raw partitions. Rollups are
// never pruned — that split is the whole point of D4.
func maintenance(ctx context.Context, log *slog.Logger, st *store.Store, w *sessions.Writer, cfg config) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	lastPrune := time.Time{}
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if n := w.SweepIdle(); n > 0 {
				log.Info("finalized idle sessions", "count", n)
			}
			st.CloseIdle(5 * time.Minute)
			if _, err := st.SweepOrphans(cfg.sessionIdle * 4); err != nil {
				log.Warn("orphan sweep failed", "err", err)
			}
			if now.Sub(lastPrune) < time.Hour {
				continue
			}
			lastPrune = now
			cutoff := now.AddDate(0, 0, -cfg.retentionDays)
			if n, err := st.Prune(cutoff); err != nil {
				log.Warn("prune failed", "err", err)
			} else if n > 0 {
				log.Info("pruned raw partitions", "count", n, "before", cutoff.Format(store.DateLayout))
			}
		}
	}
}

// scrapeSink stores relay observations and refreshes the live projection.
type scrapeSink struct {
	store      *store.Store
	projection *live.Projection
}

func (s *scrapeSink) StoreRelay(date, pod string, lines [][]byte) error {
	return s.store.AppendRelay(date, pod, lines)
}

func (s *scrapeSink) ObserveRelay(obs []relayscrape.Observation) {
	s.projection.ObserveRelay(obs)
}

// basicAuth is the optional gate for operators who route the read listener
// through an Ingress. Off by default: cluster-internal exposure is R9 D1's
// posture, and this is the escape hatch for going beyond it.
func basicAuth(next http.Handler, user, pass string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != user || p != pass {
			w.Header().Set("WWW-Authenticate", `Basic realm="gawk-telemetry"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// --- configuration --------------------------------------------------------

type config struct {
	ingestAddr     string
	readAddr       string
	dataDir        string
	key            []byte
	retentionDays  int
	scrapeInterval time.Duration
	sessionIdle    time.Duration
	relayHeadless  string
	relayPort      int
	relayStatic    []string
	dashboardBase  string
	mcpEnabled     bool
	enableSQL      bool
	basicAuthUser  string
	basicAuthPass  string
	rateLimit      float64
	rateBurst      float64
	logFormat      string
	logLevel       string
}

func (c config) resolver() relayscrape.Resolver {
	if len(c.relayStatic) > 0 {
		return relayscrape.StaticResolver(c.relayStatic)
	}
	if c.relayHeadless != "" {
		// D5: the HEADLESS Service, resolved each interval, so pods appearing
		// and disappearing during a rollout are followed without a restart.
		// The plain metrics Service is a ClusterIP and would load-balance —
		// scraping it hits one random pod.
		return relayscrape.DNSResolver(c.relayHeadless, c.relayPort)
	}
	// No relay configured: client-only telemetry, and every rollup honestly
	// says relayCoverage "none" rather than pretending.
	return relayscrape.StaticResolver(nil)
}

func (c config) relayTargetDescription() string {
	switch {
	case len(c.relayStatic) > 0:
		return strings.Join(c.relayStatic, ",")
	case c.relayHeadless != "":
		return fmt.Sprintf("%s:%d (headless)", c.relayHeadless, c.relayPort)
	default:
		return "none (client-only telemetry)"
	}
}

func parseFlags(args []string, env func(string) string) (config, error) {
	fs := flag.NewFlagSet("gawk-telemetry", flag.ContinueOnError)
	var c config
	fs.StringVar(&c.ingestAddr, "ingest-addr", or(env("GAWK_TELEMETRY_INGEST_ADDR"), ":8080"),
		"public listener for the ingest path")
	fs.StringVar(&c.readAddr, "read-addr", or(env("GAWK_TELEMETRY_READ_ADDR"), ":8081"),
		"NON-public listener for the dashboard, read API and MCP")
	fs.StringVar(&c.dataDir, "data-dir", or(env("GAWK_TELEMETRY_DATA_DIR"), "/data"),
		"data directory (a PVC in a cluster)")
	keyHex := fs.String("telemetry-key", env("GAWK_TELEMETRY_KEY"),
		"session-token HMAC key, 64 hex chars — the SAME key the relay mints with")
	fs.IntVar(&c.retentionDays, "retention-days", orInt(env("GAWK_TELEMETRY_RETENTION_DAYS"), 14),
		"how long raw sessions are kept; rollups are permanent regardless")
	scrape := fs.String("scrape-interval", or(env("GAWK_TELEMETRY_SCRAPE_INTERVAL"), "5s"),
		"how often each relay pod's /statusz is polled")
	idle := fs.String("session-idle", or(env("GAWK_TELEMETRY_SESSION_IDLE"), "2m"),
		"how long a session may go unheard-from before it is finalized")
	fs.StringVar(&c.relayHeadless, "relay-headless-service", env("GAWK_TELEMETRY_RELAY_HEADLESS"),
		"headless Service DNS name enumerating the relay pods")
	fs.IntVar(&c.relayPort, "relay-metrics-port", orInt(env("GAWK_TELEMETRY_RELAY_PORT"), 2112),
		"the relay's ops port")
	relayStatic := fs.String("relay-addrs", env("GAWK_TELEMETRY_RELAY_ADDRS"),
		"comma-separated relay ops addresses; overrides the headless Service (single-pod, dev)")
	fs.StringVar(&c.dashboardBase, "dashboard-base", env("GAWK_TELEMETRY_DASHBOARD_BASE"),
		"base URL used in the deep links a verdict carries")
	fs.BoolVar(&c.mcpEnabled, "mcp", orBool(env("GAWK_TELEMETRY_MCP"), true),
		"serve the MCP endpoint on the read listener")
	fs.BoolVar(&c.enableSQL, "query-sql", orBool(env("GAWK_TELEMETRY_QUERY_SQL"), false),
		"expose the optional DuckDB passthrough tool (default off)")
	fs.StringVar(&c.basicAuthUser, "read-user", env("GAWK_TELEMETRY_READ_USER"),
		"optional basic-auth user for the read listener (empty = no auth, cluster-internal)")
	fs.StringVar(&c.basicAuthPass, "read-password", env("GAWK_TELEMETRY_READ_PASSWORD"),
		"basic-auth password for the read listener")
	rate := fs.Float64("ingest-rate", orFloat(env("GAWK_TELEMETRY_INGEST_RATE"), 5),
		"per-IP ingest requests per second (the IP is used and never stored)")
	burst := fs.Float64("ingest-burst", orFloat(env("GAWK_TELEMETRY_INGEST_BURST"), 20),
		"per-IP ingest burst")
	fs.StringVar(&c.logLevel, "log-level", or(env("GAWK_TELEMETRY_LOG_LEVEL"), "info"), "debug|info|warn|error")
	fs.StringVar(&c.logFormat, "log-format", or(env("GAWK_TELEMETRY_LOG_FORMAT"), "json"), "text|json")

	if err := fs.Parse(args); err != nil {
		return config{}, err
	}

	key, err := hex.DecodeString(strings.TrimSpace(*keyHex))
	if err != nil || len(key) != wire.TelemetryKeySize {
		// Refusing to start is deliberate: without the fleet key no token can
		// be verified, so the service could only either reject everything or
		// accept anything. Neither is a mode worth having.
		return config{}, fmt.Errorf("-telemetry-key must be %d hex chars (the same key the relay mints with)",
			wire.TelemetryKeySize*2)
	}
	c.key = key

	if c.scrapeInterval, err = time.ParseDuration(*scrape); err != nil || c.scrapeInterval <= 0 {
		return config{}, fmt.Errorf("invalid -scrape-interval %q", *scrape)
	}
	if c.sessionIdle, err = time.ParseDuration(*idle); err != nil || c.sessionIdle <= 0 {
		return config{}, fmt.Errorf("invalid -session-idle %q", *idle)
	}
	if c.retentionDays < 1 {
		return config{}, fmt.Errorf("-retention-days must be at least 1")
	}
	for _, a := range strings.Split(*relayStatic, ",") {
		if a = strings.TrimSpace(a); a != "" {
			c.relayStatic = append(c.relayStatic, a)
		}
	}
	c.rateLimit, c.rateBurst = *rate, *burst
	return c, nil
}

func newLogger(c config) *slog.Logger {
	level := slog.LevelInfo
	switch strings.ToLower(c.logLevel) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	opts := &slog.HandlerOptions{Level: level}
	if c.logFormat == "text" {
		return slog.New(slog.NewTextHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, opts))
}

func or(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func orInt(v string, def int) int {
	var n int
	if _, err := fmt.Sscanf(strings.TrimSpace(v), "%d", &n); err != nil {
		return def
	}
	return n
}

func orFloat(v string, def float64) float64 {
	var f float64
	if _, err := fmt.Sscanf(strings.TrimSpace(v), "%g", &f); err != nil {
		return def
	}
	return f
}

func orBool(v string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1", "yes":
		return true
	case "false", "0", "no":
		return false
	}
	return def
}
