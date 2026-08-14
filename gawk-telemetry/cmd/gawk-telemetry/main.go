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
	"crypto/subtle"
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
	"github.com/Tuhis/gawk/gawk-telemetry/internal/annotations"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/dashboard"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/ingest"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/live"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/mcp"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/readapi"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/relayscrape"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/rollup"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/sessions"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/sqlengine"
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

	projection := live.New(nil)

	// TH8's annotation store. A sibling of rollups/, never under sessions/ or
	// relay/, so the prune loop cannot reach it — an annotation outliving the
	// samples it describes is the normal case and the whole point (UD16).
	notes, err := annotations.New(annotations.Options{Root: cfg.dataDir})
	if err != nil {
		return fmt.Errorf("annotations: %w", err)
	}

	// TH10's engine (§8 Q1's resolution). A build without `-tags duckdb` gets
	// nil here and the console says so plainly — which is a different message
	// from a query error, and the UI renders it as one.
	var engine readapi.SQLEngine
	if cfg.enableSQL {
		e, err := sqlengine.Open(sqlengine.Options{Root: cfg.dataDir})
		switch {
		case errors.Is(err, sqlengine.ErrNoEngine):
			log.Info("query-sql is enabled but this build has no engine compiled in; " +
				"rebuild with -tags duckdb (the deployed image does)")
		case err != nil:
			log.Warn("query engine failed to open; the console will report itself unavailable", "err", err)
		default:
			engine = e
			defer e.Close()
		}
	}

	api, err := readapi.New(readapi.Options{
		Store: st, Live: projection, DashboardBase: cfg.dashboardBase,
		StatsKey:       cfg.statsKey,
		RetentionDays:  cfg.retentionDays,
		ScrapeInterval: cfg.scrapeInterval,
		Annotations:    notes,
		SQL:            engine,
	})
	if err != nil {
		return err
	}

	writer := newWriter(st, log, cfg, api, projection)

	if n, err := startupRecovery(st, log, cfg, api); err != nil {
		log.Warn("orphan sweep failed", "err", err)
	} else if n > 0 {
		log.Info("recovered orphaned sessions from a previous run", "count", n)
	}

	// R37 (docs/40 D17): ingest CORS is wildcard and unconditional now; the
	// old -cors-origins allowlist is parsed but inert (see the deprecation
	// warning below) so existing deployments upgrade without a flag change.
	handler, err := ingest.New(ingest.Options{
		Key: cfg.key, Sink: writer, Log: log,
		RatePerSec: cfg.rateLimit, Burst: cfg.rateBurst,
		SessionRatePerSec: cfg.sessionRate, SessionBurst: cfg.sessionBurst,
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
		// Whether the code -> broadcast-key lookup is available. The key itself
		// is never logged; only that one was supplied.
		"resolve_enabled", len(cfg.statsKey) > 0,
	)
	if len(cfg.corsOrigins) > 0 {
		log.Warn("-cors-origins is deprecated and ignored since R37: the ingest listener serves wildcard CORS (docs/40 D17)")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go scraper.Run(ctx)
	go maintenance(ctx, log, st, writer, cfg)

	// --- public listener: ingest ONLY -------------------------------------
	ingestMux := http.NewServeMux()
	// Registered WITHOUT a method, deliberately. A method-scoped pattern
	// ("POST /v1/ingest") makes Go's mux 404 the CORS preflight, because an
	// OPTIONS request does not match a POST route — which would leave the
	// handler's own OPTIONS branch unreachable and every cross-origin POST
	// blocked by the browser before it was ever sent. The handler does the
	// method check itself and answers 405 with an Allow header.
	ingestMux.Handle("/api/telemetry/v1/ingest", handler)
	// Also mounted without the Ingress prefix, so a path-stripping proxy and a
	// direct dial both work without a second deployment shape.
	ingestMux.Handle("/v1/ingest", handler)
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
	// UD22's SSE endpoint. A separate pattern because "/live" is an exact match
	// in Go's mux and would not carry the sub-path.
	readMux.Handle("GET /live/", api.Handler())
	if cfg.mcpEnabled {
		mcpSrv, err := mcp.New(mcp.Options{
			API: api, EnableSQL: cfg.enableSQL,
			// TH10: the engine reaches MCP by the same path it reaches the
			// console. `internal/mcp` has accepted a SQL func since R28 and
			// nothing ever supplied one, so the tool answered "enabled but no
			// engine is wired in this deployment" — true, and a stub.
			SQL: sqlTool(engine),
		})
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

// sqlTool adapts the query engine to the MCP tool's signature, or returns nil
// where there is no engine — which is what makes the tool's own "no engine is
// wired in this deployment" message true rather than a stub's excuse.
func sqlTool(engine readapi.SQLEngine) func(string) (any, error) {
	if engine == nil {
		return nil
	}
	return func(q string) (any, error) { return engine.Query(q) }
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
	t := time.NewTicker(sweepInterval(cfg.sessionIdle))
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

// sweepInterval derives the housekeeping cadence from the idle timeout rather
// than pinning it to a constant. A fixed 30 s ticker silently dominates any
// shorter -session-idle, so a deployment (or a test) that asks for a 6 s idle
// would still wait half a minute for the sweep — the knob would appear to do
// nothing. Bounded at both ends: never busier than every 2 s, never lazier
// than every 30 s.
func sweepInterval(idle time.Duration) time.Duration {
	d := idle / 4
	if d < 2*time.Second {
		d = 2 * time.Second
	}
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	return d
}

// newWriter builds the session writer with its two hooks into the rest of the
// service. It is a named function, not an inline literal in run(), so the
// wiring itself is reachable from a test: review finding 1 was a hook with no
// production caller, and the projection's own lifecycle tests could not have
// caught it while the only thing connecting the two lived inside run().
func newWriter(st *store.Store, log *slog.Logger, cfg config, api *readapi.API, projection *live.Projection) *sessions.Writer {
	rollupRow := rollupFinalize(st, log, cfg, api)
	return sessions.NewWriter(sessions.Options{
		Store:       st,
		Log:         log,
		IdleTimeout: cfg.sessionIdle,
		Finalize: func(l sessions.Live, lines [][]byte) {
			rollupRow(l, lines)
			// The live view drops the row in the same breath that writes the
			// permanent one. Every end converges here — a `final` batch, the
			// idle sweep, and shutdown — so no ending leaves a row behind,
			// which is what made the dashboard grow without bound.
			projection.EndSession(l.Ref.BroadcastKey, l.Ref.SessionID)
		},
		Observe: func(l sessions.Live, a ingest.Accepted) {
			projection.ObserveClient(a, l.App.Browser, l.App.OS, l.App.Version)
		},
	})
}

// rollupFinalize is the one path from a session's stored lines to its
// permanent row. Shared by the writer's finalize and by crash recovery, so the
// stored verdict and a later diagnose() of the same session come out of ONE
// code path — "has this got better since the R15 fix?" then compares like with
// like — and a recovered session is indistinguishable from a gracefully ended
// one except in the `endedCleanly` flag that says so.
func rollupFinalize(st *store.Store, log *slog.Logger, cfg config, api *readapi.API) sessions.Finalizer {
	return sessions.RollupFinalizer(st, log, func(row *rollup.Row, in rollup.Input) json.RawMessage {
		relayLines, _ := st.ReadRelay(time.UnixMilli(in.EndedAtMs).UTC().Format(store.DateLayout))
		// TM4's join, then TM6's verdict over the joined row.
		readapi.JoinRelay(row, relayLines, cfg.scrapeInterval)
		rep := api.DiagnoseRow(*row, in, relayLines)
		b, err := json.Marshal(rep)
		if err != nil {
			return nil
		}
		return b
	}, nil)
}

// startupRecovery installs the rollup hook and runs the first sweep. It is one
// function because the two halves are only correct together: a sweep without
// the hook archives a crashed session with no permanent row — which is what
// review finding 3 was — and the hook without a sweep does nothing. Installing
// it here also covers the periodic sweep in maintenance(), which shares the
// store.
//
// It runs after the writer exists because recovery goes through the same
// finalize path a graceful end uses (D3/D4).
func startupRecovery(st *store.Store, log *slog.Logger, cfg config, api *readapi.API) (int, error) {
	st.SetOrphanHook(orphanRecovery(st, log, cfg, api))
	return st.SweepOrphans(cfg.sessionIdle)
}

// orphanRecovery turns a crashed process's leftover session file into the same
// permanent row a graceful end would have written. Identity comes from the
// records themselves (every stored line is self-describing), so an empty Live
// is enough — and `EndedCleanly` stays false, which is exactly what happened.
func orphanRecovery(st *store.Store, log *slog.Logger, cfg config, api *readapi.API) func(store.SessionRef, [][]byte) {
	rollupRow := rollupFinalize(st, log, cfg, api)
	return func(ref store.SessionRef, lines [][]byte) {
		if len(lines) == 0 {
			return
		}
		rollupRow(sessions.Live{Ref: ref}, lines)
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

func (s *scrapeSink) ObserveRelay(r relayscrape.Round) {
	s.projection.ObserveRelay(r)
}

// basicAuth is the optional gate for operators who route the read listener
// through an Ingress. Off by default: cluster-internal exposure is R9 D1's
// posture, and this is the escape hatch for going beyond it.
func basicAuth(next http.Handler, user, pass string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		// Constant-time, and BOTH halves evaluated: `!=` short-circuits, which
		// leaks which field was wrong and how far the match got. This gate is
		// what stands between a public Ingress and a surface that aggregates
		// every broadcast on the fleet.
		okUser := subtle.ConstantTimeCompare([]byte(u), []byte(user)) == 1
		okPass := subtle.ConstantTimeCompare([]byte(p), []byte(pass)) == 1
		if !ok || !okUser || !okPass {
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
	// statsKey is OPTIONAL and unrelated to `key`: it is the relay's stats key,
	// and its only use is resolving an operator-supplied broadcast code to the
	// obfuscated key the dashboard shows (readapi/resolve.go). Unset by
	// default, because holding it lets this process enumerate join codes for
	// the broadcasts it stores.
	statsKey      []byte
	mcpEnabled    bool
	enableSQL     bool
	basicAuthUser string
	basicAuthPass string
	rateLimit     float64
	rateBurst     float64
	sessionRate   float64
	sessionBurst  float64
	corsOrigins   []string
	logFormat     string
	logLevel      string
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
	// 30 days, raised from 14 by owner decision (docs/36 UD15). A release cycle
	// fits inside it, so "compare this session to one from before the R30
	// change" stays answerable at FULL resolution instead of rollup-only.
	// ~160 MB at current volume. Rollups stay permanent regardless.
	fs.IntVar(&c.retentionDays, "retention-days", orInt(env("GAWK_TELEMETRY_RETENTION_DAYS"), 30),
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
	statsKeyHex := fs.String("stats-key", env("GAWK_TELEMETRY_STATS_KEY"),
		"OPTIONAL fleet stats key as 64 hex chars, the SAME value the relay's -stats-key carries; "+
			"enables the dashboard's find-a-stream-by-code lookup. Empty disables it")
	fs.StringVar(&c.dashboardBase, "dashboard-base", env("GAWK_TELEMETRY_DASHBOARD_BASE"),
		"base URL used in the deep links a verdict carries")
	fs.BoolVar(&c.mcpEnabled, "mcp", orBool(env("GAWK_TELEMETRY_MCP"), true),
		"serve the MCP endpoint on the read listener")
	// Default ON by owner decision (docs/36 UD18), which reverses D11's
	// "arbitrary SQL should be a deliberate act". The flag survives as the gate;
	// only its default moved. What actually answers a query is the build: a
	// binary without `-tags duckdb` has no engine, and the console and the MCP
	// tool both say so rather than pretending (§8 Q1).
	fs.BoolVar(&c.enableSQL, "query-sql", orBool(env("GAWK_TELEMETRY_QUERY_SQL"), true),
		"expose the ad-hoc SQL surface (default on; needs a build with -tags duckdb to answer)")
	fs.StringVar(&c.basicAuthUser, "read-user", env("GAWK_TELEMETRY_READ_USER"),
		"optional basic-auth user for the read listener (empty = no auth, cluster-internal)")
	fs.StringVar(&c.basicAuthPass, "read-password", env("GAWK_TELEMETRY_READ_PASSWORD"),
		"basic-auth password for the read listener")
	rate := fs.Float64("ingest-rate", orFloat(env("GAWK_TELEMETRY_INGEST_RATE"), ingest.DefaultGlobalRatePerSec),
		"process-wide ingest requests per second — size for the whole fleet (no client IP is consulted or stored)")
	burst := fs.Float64("ingest-burst", orFloat(env("GAWK_TELEMETRY_INGEST_BURST"), ingest.DefaultGlobalBurst),
		"process-wide ingest burst; covers the fleet flushing at once after a rollout")
	sessionRate := fs.Float64("ingest-session-rate", orFloat(env("GAWK_TELEMETRY_INGEST_SESSION_RATE"), ingest.DefaultSessionRatePerSec),
		"per-VERIFIED-SESSION ingest requests per second — the fairness tier, applied after the token check")
	sessionBurst := fs.Float64("ingest-session-burst", orFloat(env("GAWK_TELEMETRY_INGEST_SESSION_BURST"), ingest.DefaultSessionBurst),
		"per-verified-session ingest burst")
	corsOrigins := fs.String("cors-origin", env("GAWK_TELEMETRY_CORS_ORIGIN"),
		"comma-separated origins allowed to POST cross-origin — the SPLIT-ORIGIN deployment only; empty (the default, same-origin) adds no CORS surface at all")
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

	// Optional, so empty is a valid answer — but a MALFORMED one is not: a
	// silently-ignored bad key would leave the lookup answering with digests
	// that match nothing and no way to tell that from a wrong code.
	if sk := strings.TrimSpace(*statsKeyHex); sk != "" {
		statsKey, err := hex.DecodeString(sk)
		if err != nil || len(statsKey) != wire.TelemetryKeySize {
			return config{}, fmt.Errorf("-stats-key must be %d hex chars (the same key the relay obfuscates /statusz with)",
				wire.TelemetryKeySize*2)
		}
		c.statsKey = statsKey
	}

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
	c.sessionRate, c.sessionBurst = *sessionRate, *sessionBurst
	for _, o := range strings.Split(*corsOrigins, ",") {
		if o = strings.TrimSpace(o); o != "" {
			c.corsOrigins = append(c.corsOrigins, o)
		}
	}
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
