// Package config holds gawk-admin's configuration and its flag/env parsing
// (R39, docs/42 §4.12).
//
// Precedence: command-line flag > environment variable > default, the same
// contract gawk-server and gawk-telemetry use. Every knob has a
// GAWK_ADMIN_*-prefixed environment fallback so one binary is convenient both
// on a command line and in a k8s Deployment, and every knob lands in the
// startup log — the operator's only confirmation surface for what a pod
// actually parsed (the R2 lesson, CLAUDE.md).
package config

import (
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Tuhis/gawk/gawk-admin/internal/store"
	"github.com/Tuhis/gawk/gawk-server/oidcroles"
)

// Mode is what the process was asked to do. The migrate subcommand shares the
// binary and the image so the schema a release needs travels with the release
// (docs/42 §4.15), but it needs one knob where serving needs a dozen — so
// validation is mode-aware rather than "everything is always required".
type Mode string

const (
	// ModeServe is the default: the API + portal listener.
	ModeServe Mode = "serve"
	// ModeMigrate applies pending migrations and exits. Invoked by the Helm
	// pre-install/pre-upgrade hook Job, and by hand as the break-glass path.
	ModeMigrate Mode = "migrate"
)

// StaticWebhook is one chart-defined webhook (docs/42 D9): visible in the
// portal, immutable there, with its signing secret sourced from a Kubernetes
// Secret through an environment variable rather than sitting in the values
// file.
type StaticWebhook struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	// SecretEnv names the environment variable holding this webhook's HMAC
	// signing key. The key itself never appears in configuration.
	SecretEnv string `json:"secretEnv"`
	// Secret is resolved from SecretEnv at parse time. It is never rendered
	// by the API and never logged.
	Secret string `json:"-"`
	// Enabled defaults to true; a chart-defined webhook can be parked
	// without deleting its values entry.
	Enabled *bool `json:"enabled,omitempty"`
	// Events is the webhook's event filter (R49, docs/50 D8). Absent means
	// every moderation event and no activity event — what every webhook
	// received before R49; a list is exact and is validated at start-up.
	Events []string `json:"events,omitempty"`
}

// IsEnabled reports whether this webhook should receive deliveries. Absent
// means enabled — the common case should need no ceremony.
func (w StaticWebhook) IsEnabled() bool { return w.Enabled == nil || *w.Enabled }

// Config is the fully-resolved gawk-admin configuration.
type Config struct {
	Mode Mode

	Addr        string // the portal listener: SPA, /api/v1, /auth/config, /healthz, /readyz
	ExternalURL string // portal base URL: the OIDC redirect base and the portalUrl in webhook payloads

	// MetricsAddr is the Prometheus listener (R59, docs/61 D9): its own port,
	// never routed by the Ingress. Empty when disabled ("off").
	MetricsAddr string

	OIDCIssuer     string
	OIDCClientID   string
	OIDCAudience   string
	OIDCRolesClaim string // dot-path template to the roles array; oidcroles.Placeholder is substituted per segment
	OperatorRole   string // the role every R39 route requires
	// FlaggerRole grants R40's sampler identity exactly the three flagger
	// routes (docs/71 D18, docs/42 §4.11) — flag-only rights, never a kill.
	// Empty grants it nowhere.
	FlaggerRole string
	// RoomsReaderRole grants exactly the two room reads and /me (R49,
	// docs/50 D1) — a bot's service identity. Empty grants it nowhere.
	RoomsReaderRole string
	// RoomsManagerRole grants static room create, delete of the rooms the
	// caller created, and /me (R60, docs/62 D1). Empty grants it nowhere.
	RoomsManagerRole string

	PGDSN string

	RelayScanTarget  string // DNS name of the relay headless metrics Service
	RelayOpsPort     int
	RelayAdminToken  string
	Namespace        string // where Ban CRs live
	KillCooldown     time.Duration
	StaticWebhooks   []StaticWebhook
	AppBaseURL       string // watch deep links; empty hides them
	TelemetryBaseURL string // telemetry deep links; empty hides them

	// Rooms enables R42's room management (docs/44 D20): the /api/v1/rooms
	// routes, the rooms view and the reconciler's room sweep. Default OFF,
	// like the relay's -rooms, and rendered from the chart's rooms.enabled —
	// the same value that grants the Role its Room CR and Secret verbs, so
	// the binary never serves routes its ServiceAccount cannot act on.
	Rooms bool

	// Screening enables R40's content flagging (docs/71 §9.4): the flagger
	// routes the sampler calls, the operator's flag list and actions, the
	// calibration replay, and the leader's reminder/deadline sweep. Default
	// OFF: with it off every one of those paths answers 404, exactly as R39's
	// reserved POST /content-flags did.
	Screening bool
	// ContentFlagRepage is how often an open flag re-pages (D9).
	ContentFlagRepage time.Duration
	// ContentFlagMute is how long a dismissal mutes re-flagging (D10, OD11).
	ContentFlagMute time.Duration
	// ContentFlagStaleAfter is how long after the last tripped POST a flag's
	// signal turns from live to unknown (D9).
	ContentFlagStaleAfter time.Duration
	// ContentFlagAutoKill is the auto-kill tier (D14); nil is off, the
	// default.
	ContentFlagAutoKill *AutoKillPolicy
	// ContentFlagDeadline kills an unacknowledged flag whose signal is live
	// after this long (D15); 0 is off, the default.
	ContentFlagDeadline time.Duration
	// ScreeningSampleRetention is how long calibration data is kept: the
	// samples, observed flags and their events (D17). Flag-mode flags are
	// never pruned.
	ScreeningSampleRetention time.Duration
	// ScreeningCritical is the timeline's critical score level (docs/72 D2):
	// the chart's top band, "time at critical" and the 24-hour suggestion.
	// It flags and kills nothing.
	ScreeningCritical float64
	// ScreeningSustainedAfter is how long a run above the threshold lasts
	// before the timeline calls it sustained (docs/72 D8).
	ScreeningSustainedAfter time.Duration
	// ContentFlagRejoin holds a clear of an unacknowledged flag this long,
	// so a re-trip inside it is the same incident and does not page again
	// (docs/72 D3); 0 resolves clears at once, as R40 did.
	ContentFlagRejoin time.Duration

	// EventBus* configure the R50 consumer (docs/51 D6). Operator-provided
	// NATS, default off: an empty URL means no client, no stream, no activity
	// rows — and the reconciler's room sweep keeps running, which is what
	// makes "off is byte-identical" true.
	EventBusURL       string
	EventBusCredsFile string
	// EventBusTLSCert / EventBusTLSKey are a client certificate, the other way
	// a NATS deployment identifies a workload: with `verify_and_map` the
	// subject DN IS the NATS username. EventBusCAFile verifies the server,
	// which a bus on its own private CA requires.
	EventBusTLSCert string
	EventBusTLSKey  string
	EventBusCAFile  string
	EventBusStream  string
	// EventBusManageStream lets this portal create and update the stream and
	// its durable consumer (the default, and what docs/51 D2 describes).
	//
	// Off where the JetStream objects are declared elsewhere and reconciled
	// from git — a NACK Stream/Consumer CR, say. Such a deployment grants
	// workloads no JetStream API at all, so managing them is not a duplicate
	// of the operator's intent but a retry loop against a permission that will
	// never be granted. The portal then binds and consumes.
	EventBusManageStream bool
	// EventBusConsumer is the durable to use; empty means the built-in name.
	EventBusConsumer string
	EventBusMaxBytes int64
	EventBusReplicas int
	// EventBusInsecure skips NATS TLS verification: the docs/41 compose lane
	// only, no chart value, warns at every start.
	EventBusInsecure bool
	// ActivityRetention is how long ingested activity rows are kept. The audit
	// trail (moderation rows) is never pruned by it.
	ActivityRetention time.Duration

	// DevOIDCProxy, when non-empty, mounts a reverse proxy at /idp/ towards
	// this base URL — the docs/41 compose lane's answer to the OIDC
	// frontend/backchannel split: with the issuer set to <externalUrl>/idp,
	// ONE URL reaches the dev IdP from the developer's browser (the published
	// port) and from this container (its own listener). DEV ONLY, the
	// GAWK_DEV_CERT precedent: it is deliberately not a chart value, and
	// serving it logs a warning at startup.
	DevOIDCProxy string

	LogLevel  slog.Level
	LogFormat string // "text" or "json"
}

// RolesClaimPath renders the roles-claim path for the startup log. The default
// is the Keycloak client-roles shape; other providers override the whole path
// (docs/42 §4.8).
//
// DISPLAY ONLY. Authorization parses the template itself (internal/auth, via
// oidcroles.ParsePath), because a rendered string cannot express which dots
// are separators and which came out of the audience. This joins the parsed
// segments back together so the log agrees with what was parsed, and falls
// back to the raw template when the configuration is unusable — refusing that
// belongs to validate() and auth.New, not to a log line.
func (c Config) RolesClaimPath() string {
	path, err := oidcroles.ParsePath(c.OIDCRolesClaim, c.OIDCAudience)
	if err != nil {
		return c.OIDCRolesClaim
	}
	return path.String()
}

// ParseFlags resolves configuration from args and getenv. args must NOT
// include the program name; a leading "migrate" selects ModeMigrate.
func ParseFlags(args []string, getenv func(string) string) (Config, error) {
	mode := ModeServe
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		switch args[0] {
		case string(ModeMigrate):
			mode, args = ModeMigrate, args[1:]
		case string(ModeServe):
			mode, args = ModeServe, args[1:]
		default:
			return Config{}, fmt.Errorf("unknown subcommand %q (want %q or %q)", args[0], ModeServe, ModeMigrate)
		}
	}

	env := func(key, def string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return def
	}

	fs := flag.NewFlagSet("gawk-admin", flag.ContinueOnError)
	addr := fs.String("addr", env("GAWK_ADMIN_ADDR", ":8090"), "HTTP listen address for the portal and API")
	metricsAddr := fs.String("metrics-addr", env("GAWK_ADMIN_METRICS_ADDR", ":8091"),
		"HTTP listen address for /metrics, a separate listener never routed publicly; \"off\" disables it")
	externalURL := fs.String("external-url", env("GAWK_ADMIN_EXTERNAL_URL", ""),
		"portal base URL; the OIDC redirect base and the portalUrl in webhook payloads (required)")
	issuer := fs.String("oidc-issuer", env("GAWK_ADMIN_OIDC_ISSUER", ""), "OIDC issuer URL (required)")
	clientID := fs.String("oidc-client-id", env("GAWK_ADMIN_OIDC_CLIENT_ID", ""),
		"OIDC public client ID used by the portal SPA (required)")
	audience := fs.String("oidc-audience", env("GAWK_ADMIN_OIDC_AUDIENCE", ""),
		"audience every accepted JWT must carry (required)")
	rolesClaim := fs.String("oidc-roles-claim", env("GAWK_ADMIN_OIDC_ROLES_CLAIM", oidcroles.DefaultClaim),
		"dot-path to the roles array in the JWT; {audience} is substituted into each segment")
	operatorRole := fs.String("operator-role", env("GAWK_ADMIN_OPERATOR_ROLE", "operator"),
		"role every R39 route requires")
	flaggerRole := fs.String("flagger-role", env("GAWK_ADMIN_FLAGGER_ROLE", "flagger"),
		"role granting R40's sampler the three flagger routes only — POST /api/v1/content-flags, GET /api/v1/screening/targets, POST /api/v1/screening/samples; \"off\" grants it nowhere")
	roomsReaderRole := fs.String("rooms-reader-role", env("GAWK_ADMIN_ROOMS_READER_ROLE", "rooms-reader"),
		"role granting GET /api/v1/rooms, GET /api/v1/rooms/{name} and GET /api/v1/me only (R49); \"off\" grants it nowhere")
	roomsManagerRole := fs.String("rooms-manager-role", env("GAWK_ADMIN_ROOMS_MANAGER_ROLE", "rooms-manager"),
		"role granting POST /api/v1/rooms, DELETE /api/v1/rooms/{name} on rooms the caller created, and GET /api/v1/me only (R60); \"off\" grants it nowhere")
	pgDSN := fs.String("pg-dsn", env("GAWK_ADMIN_PG_DSN", ""), "PostgreSQL DSN (required)")
	relayScanTarget := fs.String("relay-scan-target", env("GAWK_ADMIN_RELAY_SCAN_TARGET", ""),
		"DNS name of the relay headless metrics Service; its A records are the pods (required)")
	relayOpsPort := fs.String("relay-ops-port", env("GAWK_ADMIN_RELAY_OPS_PORT", "2112"),
		"ops listener port on relay pods")
	relayAdminToken := fs.String("relay-admin-token", env("GAWK_ADMIN_RELAY_ADMIN_TOKEN", ""),
		"bearer token for the relay's /internal/admin/* routes (required)")
	namespace := fs.String("namespace", env("GAWK_ADMIN_NAMESPACE", env("POD_NAMESPACE", "")),
		"namespace holding Ban CRs; defaults to POD_NAMESPACE")
	killCooldown := fs.String("kill-cooldown", env("GAWK_ADMIN_KILL_COOLDOWN", "10m"),
		"default plain-kill ID-ban duration")
	staticWebhooks := fs.String("static-webhooks", env("GAWK_ADMIN_STATIC_WEBHOOKS", ""),
		`chart-defined webhooks as JSON: [{"name":"…","url":"…","secretEnv":"…"}]`)
	appBaseURL := fs.String("app-base-url", env("GAWK_ADMIN_APP_BASE_URL", ""),
		"frontend base URL for watch deep links; empty hides them")
	telemetryBaseURL := fs.String("telemetry-base-url", env("GAWK_ADMIN_TELEMETRY_BASE_URL", ""),
		"telemetry UI base URL for deep links; empty hides them")
	eventBusURL := fs.String("eventbus-url", env("GAWK_ADMIN_EVENTBUS_URL", ""),
		"R50 NATS JetStream URL to consume relay events from; empty disables the bus")
	eventBusCredsFile := fs.String("eventbus-creds-file", env("GAWK_ADMIN_EVENTBUS_CREDS_FILE", ""),
		"path to the NATS .creds file for the portal's stream user")
	eventBusTLSCert := fs.String("eventbus-tls-cert", env("GAWK_ADMIN_EVENTBUS_TLS_CERT", ""),
		"client certificate for NATS mTLS; with verify_and_map its subject DN is the NATS user")
	eventBusTLSKey := fs.String("eventbus-tls-key", env("GAWK_ADMIN_EVENTBUS_TLS_KEY", ""),
		"private key for -eventbus-tls-cert")
	eventBusCAFile := fs.String("eventbus-ca-file", env("GAWK_ADMIN_EVENTBUS_CA_FILE", ""),
		"CA bundle verifying the NATS server; empty uses the platform trust store")
	eventBusManageStream := fs.String("eventbus-manage-stream", env("GAWK_ADMIN_EVENTBUS_MANAGE_STREAM", "true"),
		"create and update the stream and its durable consumer; false binds to objects an operator declares (e.g. NACK CRs)")
	eventBusConsumer := fs.String("eventbus-consumer", env("GAWK_ADMIN_EVENTBUS_CONSUMER", ""),
		"durable consumer name; empty uses the built-in default")
	eventBusStream := fs.String("eventbus-stream", env("GAWK_ADMIN_EVENTBUS_STREAM", "GAWK_EVENTS"),
		"JetStream stream this portal creates and consumes")
	eventBusMaxBytes := fs.String("eventbus-max-bytes", env("GAWK_ADMIN_EVENTBUS_MAX_BYTES", "268435456"),
		"stream size limit in bytes (discard old)")
	eventBusReplicas := fs.String("eventbus-replicas", env("GAWK_ADMIN_EVENTBUS_REPLICAS", "0"),
		"stream replicas; 0 leaves the server's default")
	eventBusInsecure := fs.String("eventbus-insecure", env("GAWK_ADMIN_EVENTBUS_INSECURE", "false"),
		"skip NATS TLS verification (local development only; warns at startup)")
	activityRetention := fs.String("activity-retention", env("GAWK_ADMIN_ACTIVITY_RETENTION", "72h"),
		"how long ingested activity events are kept; moderation events are never pruned")
	roomsOn := fs.String("rooms", env("GAWK_ADMIN_ROOMS", "false"),
		"enable room management (R42, docs/44 D20): /api/v1/rooms, the rooms view and the room sweep; needs the chart's rooms.enabled RBAC")
	screeningOn := fs.String("screening", env("GAWK_ADMIN_SCREENING", "false"),
		"enable R40 content flagging (docs/71): the flagger routes, the flag list and actions, the calibration replay and the reminder sweep")
	contentFlagRepage := fs.String("content-flag-repage", env("GAWK_ADMIN_CONTENT_FLAG_REPAGE", "10m"),
		"how often an unacknowledged content flag re-pages")
	contentFlagMute := fs.String("content-flag-mute", env("GAWK_ADMIN_CONTENT_FLAG_MUTE", "30m"),
		"how long dismissing a content flag mutes re-flagging of its broadcast")
	contentFlagStaleAfter := fs.String("content-flag-stale-after", env("GAWK_ADMIN_CONTENT_FLAG_STALE_AFTER", "2m"),
		"how long after the sampler's last report a flag's signal turns from live to unknown")
	contentFlagAutoKill := fs.String("content-flag-auto-kill", env("GAWK_ADMIN_CONTENT_FLAG_AUTO_KILL", "off"),
		"auto-kill tier: off, or a policy like score>=0.9,hits>=10/12 over a flag's recent per-sample scores")
	contentFlagDeadline := fs.String("content-flag-deadline", env("GAWK_ADMIN_CONTENT_FLAG_DEADLINE", "off"),
		"kill an unacknowledged flag whose signal is still live after this long: off, or a duration like 30m")
	sampleRetention := fs.String("screening-sample-retention", env("GAWK_ADMIN_SCREENING_SAMPLE_RETENTION", "30d"),
		"how long calibration data (samples, observed flags) is kept; flag-mode flags are never pruned")
	screeningCritical := fs.String("screening-critical", env("GAWK_ADMIN_SCREENING_CRITICAL", "0.7"),
		"the moderation timeline's critical score level, in (0, 1]: its top band and the 24-hour ban suggestion; it flags nothing")
	sustainedAfter := fs.String("screening-sustained-after", env("GAWK_ADMIN_SCREENING_SUSTAINED_AFTER", "3m"),
		"how long a run above the threshold lasts before the moderation timeline calls it sustained")
	contentFlagRejoin := fs.String("content-flag-rejoin", env("GAWK_ADMIN_CONTENT_FLAG_REJOIN", "10m"),
		"hold a clear of an unacknowledged flag this long, so a re-trip is the same incident and does not page again: off, or a duration")
	logLevel := fs.String("log-level", env("GAWK_ADMIN_LOG_LEVEL", "info"), "log level: debug|info|warn|error")
	logFormat := fs.String("log-format", env("GAWK_ADMIN_LOG_FORMAT", "text"), "log format: text|json")
	devOIDCProxy := fs.String("dev-oidc-proxy", env("GAWK_ADMIN_DEV_OIDC_PROXY", ""),
		"DEV ONLY: reverse-proxy /idp/ to this base URL so one issuer URL works in the browser and in the container (docs/41)")

	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}

	cfg := Config{
		Mode:             mode,
		Addr:             *addr,
		MetricsAddr:      metricsListen(*metricsAddr),
		ExternalURL:      strings.TrimRight(*externalURL, "/"),
		OIDCIssuer:       strings.TrimRight(*issuer, "/"),
		OIDCClientID:     *clientID,
		OIDCAudience:     *audience,
		OIDCRolesClaim:   strings.TrimSpace(*rolesClaim),
		OperatorRole:     strings.TrimSpace(*operatorRole),
		FlaggerRole:      roleOrOff(*flaggerRole),
		RoomsReaderRole:  roleOrOff(*roomsReaderRole),
		RoomsManagerRole: roleOrOff(*roomsManagerRole),
		PGDSN:            *pgDSN,
		RelayScanTarget:  *relayScanTarget,
		RelayAdminToken:  *relayAdminToken,
		Namespace:        *namespace,
		AppBaseURL:       strings.TrimRight(*appBaseURL, "/"),
		TelemetryBaseURL: strings.TrimRight(*telemetryBaseURL, "/"),
		DevOIDCProxy:     strings.TrimRight(strings.TrimSpace(*devOIDCProxy), "/"),
		LogFormat:        *logFormat,
	}

	port, err := strconv.Atoi(strings.TrimSpace(*relayOpsPort))
	if err != nil || port < 1 || port > 65535 {
		return Config{}, fmt.Errorf("invalid -relay-ops-port %q", *relayOpsPort)
	}
	cfg.RelayOpsPort = port

	cooldown, err := time.ParseDuration(strings.TrimSpace(*killCooldown))
	if err != nil || cooldown <= 0 {
		return Config{}, fmt.Errorf("invalid -kill-cooldown %q (want a positive duration, e.g. 10m)", *killCooldown)
	}
	cfg.KillCooldown = cooldown

	rooms, err := strconv.ParseBool(strings.TrimSpace(*roomsOn))
	if err != nil {
		return Config{}, fmt.Errorf("invalid -rooms %q (want true|false)", *roomsOn)
	}
	cfg.Rooms = rooms

	if err := parseScreening(&cfg, *screeningOn, *contentFlagRepage, *contentFlagMute, *contentFlagStaleAfter,
		*contentFlagAutoKill, *contentFlagDeadline, *sampleRetention); err != nil {
		return Config{}, err
	}
	if err := parseTimeline(&cfg, *screeningCritical, *sustainedAfter, *contentFlagRejoin); err != nil {
		return Config{}, err
	}

	cfg.EventBusURL = strings.TrimSpace(*eventBusURL)
	cfg.EventBusCredsFile = strings.TrimSpace(*eventBusCredsFile)
	cfg.EventBusTLSCert = strings.TrimSpace(*eventBusTLSCert)
	cfg.EventBusTLSKey = strings.TrimSpace(*eventBusTLSKey)
	cfg.EventBusCAFile = strings.TrimSpace(*eventBusCAFile)
	cfg.EventBusConsumer = strings.TrimSpace(*eventBusConsumer)
	if (cfg.EventBusTLSCert == "") != (cfg.EventBusTLSKey == "") {
		// Half a client identity authenticates as nobody, and the failure is a
		// silent reconnect loop rather than an error.
		return Config{}, fmt.Errorf("-eventbus-tls-cert and -eventbus-tls-key must be set together")
	}
	manageStream, err := strconv.ParseBool(strings.TrimSpace(*eventBusManageStream))
	if err != nil {
		return Config{}, fmt.Errorf("invalid -eventbus-manage-stream %q (want true|false)", *eventBusManageStream)
	}
	cfg.EventBusManageStream = manageStream
	cfg.EventBusStream = strings.TrimSpace(*eventBusStream)
	if cfg.EventBusStream == "" {
		return Config{}, fmt.Errorf("invalid -eventbus-stream: want a stream name")
	}
	maxBytes, err := strconv.ParseInt(strings.TrimSpace(*eventBusMaxBytes), 10, 64)
	if err != nil || maxBytes < 0 {
		return Config{}, fmt.Errorf("invalid -eventbus-max-bytes %q (want a non-negative integer)", *eventBusMaxBytes)
	}
	cfg.EventBusMaxBytes = maxBytes
	replicas, err := strconv.Atoi(strings.TrimSpace(*eventBusReplicas))
	if err != nil || replicas < 0 {
		return Config{}, fmt.Errorf("invalid -eventbus-replicas %q (want a non-negative integer)", *eventBusReplicas)
	}
	cfg.EventBusReplicas = replicas
	busInsecure, err := strconv.ParseBool(strings.TrimSpace(*eventBusInsecure))
	if err != nil {
		return Config{}, fmt.Errorf("invalid -eventbus-insecure %q (want true|false)", *eventBusInsecure)
	}
	cfg.EventBusInsecure = busInsecure
	retention, err := time.ParseDuration(strings.TrimSpace(*activityRetention))
	if err != nil || retention <= 0 {
		return Config{}, fmt.Errorf("invalid -activity-retention %q (want a positive duration, e.g. 72h)", *activityRetention)
	}
	cfg.ActivityRetention = retention

	hooks, err := parseStaticWebhooks(*staticWebhooks, getenv)
	if err != nil {
		return Config{}, err
	}
	cfg.StaticWebhooks = hooks

	switch strings.ToLower(strings.TrimSpace(*logLevel)) {
	case "debug":
		cfg.LogLevel = slog.LevelDebug
	case "info", "":
		cfg.LogLevel = slog.LevelInfo
	case "warn", "warning":
		cfg.LogLevel = slog.LevelWarn
	case "error":
		cfg.LogLevel = slog.LevelError
	default:
		return Config{}, fmt.Errorf("invalid -log-level %q (want debug|info|warn|error)", *logLevel)
	}
	if cfg.LogFormat != "text" && cfg.LogFormat != "json" {
		return Config{}, fmt.Errorf("invalid -log-format %q (want text|json)", cfg.LogFormat)
	}

	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// validate enforces the required knobs for the resolved mode. Refusing to
// start beats serving an unauthenticated or unbootable portal: every OIDC knob
// is required because the alternative — a portal that comes up with
// authentication silently off — is the one failure this service must never
// have (docs/42 D7).
func (c Config) validate() error {
	if c.PGDSN == "" {
		return fmt.Errorf("-pg-dsn is required (GAWK_ADMIN_PG_DSN)")
	}
	if c.Mode == ModeMigrate {
		// The migrate step needs the database and nothing else. Demanding the
		// OIDC knobs here would make the Helm hook Job carry configuration it
		// has no use for.
		return nil
	}
	for _, req := range []struct{ flag, val string }{
		{"-external-url", c.ExternalURL},
		{"-oidc-issuer", c.OIDCIssuer},
		{"-oidc-client-id", c.OIDCClientID},
		{"-oidc-audience", c.OIDCAudience},
		{"-relay-scan-target", c.RelayScanTarget},
		{"-relay-admin-token", c.RelayAdminToken},
		{"-namespace", c.Namespace},
	} {
		if strings.TrimSpace(req.val) == "" {
			return fmt.Errorf("%s is required", req.flag)
		}
	}
	// A blanked roles-claim path or operator role would authorize every valid
	// token, which is worse than not starting (docs/42 §4.8, AP5).
	if c.OIDCRolesClaim == "" {
		return fmt.Errorf("-oidc-roles-claim must not be empty: with no roles claim every valid token would be an operator")
	}
	if c.OperatorRole == "" {
		return fmt.Errorf("-operator-role must not be empty: with no required role every valid token would be an operator")
	}
	// R40's sampler routes require the flagger role alone: with it off,
	// screening would be on with nothing able to post a flag or a sample.
	if c.Screening && c.FlaggerRole == "" {
		return fmt.Errorf("-screening needs a flagger role: with -flagger-role off no sampler can reach the screening routes")
	}
	for _, u := range []struct{ flag, val string }{
		{"-external-url", c.ExternalURL},
		{"-oidc-issuer", c.OIDCIssuer},
		{"-app-base-url", c.AppBaseURL},
		{"-telemetry-base-url", c.TelemetryBaseURL},
	} {
		if u.val == "" {
			continue
		}
		parsed, err := url.Parse(u.val)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return fmt.Errorf("invalid %s %q (want an absolute URL)", u.flag, u.val)
		}
	}
	return nil
}

// parseScreening resolves the R40 knobs (docs/71 §5). They are parsed and
// validated whether or not -screening is on, so a typo in a value is a
// start-up error the day it is written rather than the day screening is
// switched on.
func parseScreening(cfg *Config, on, repage, mute, staleAfter, autoKill, deadline, retention string) error {
	enabled, err := strconv.ParseBool(strings.TrimSpace(on))
	if err != nil {
		return fmt.Errorf("invalid -screening %q (want true|false)", on)
	}
	cfg.Screening = enabled
	for _, d := range []struct {
		flag, raw string
		dst       *time.Duration
	}{
		{"-content-flag-repage", repage, &cfg.ContentFlagRepage},
		{"-content-flag-mute", mute, &cfg.ContentFlagMute},
		{"-content-flag-stale-after", staleAfter, &cfg.ContentFlagStaleAfter},
	} {
		v, err := time.ParseDuration(strings.TrimSpace(d.raw))
		if err != nil || v <= 0 {
			return fmt.Errorf("invalid %s %q (want a positive duration)", d.flag, d.raw)
		}
		*d.dst = v
	}
	policy, err := ParseAutoKillPolicy(autoKill)
	if err != nil {
		return fmt.Errorf("invalid -content-flag-auto-kill: %w", err)
	}
	cfg.ContentFlagAutoKill = policy
	if dl := strings.TrimSpace(deadline); dl != "off" && dl != "" {
		v, err := time.ParseDuration(dl)
		if err != nil || v <= 0 {
			return fmt.Errorf("invalid -content-flag-deadline %q (want off or a positive duration)", deadline)
		}
		cfg.ContentFlagDeadline = v
	}
	keep, err := parseRetention(retention)
	if err != nil {
		return fmt.Errorf("invalid -screening-sample-retention: %w", err)
	}
	cfg.ScreeningSampleRetention = keep
	return nil
}

// parseTimeline resolves the R70 knobs (docs/72 §4), validated whether or not
// -screening is on, like parseScreening's.
func parseTimeline(cfg *Config, critical, sustained, rejoin string) error {
	c, err := strconv.ParseFloat(strings.TrimSpace(critical), 64)
	if err != nil || math.IsNaN(c) || c <= 0 || c > 1 {
		return fmt.Errorf("invalid -screening-critical %q (want a score in (0, 1])", critical)
	}
	cfg.ScreeningCritical = c
	v, err := time.ParseDuration(strings.TrimSpace(sustained))
	if err != nil || v <= 0 {
		return fmt.Errorf("invalid -screening-sustained-after %q (want a positive duration)", sustained)
	}
	cfg.ScreeningSustainedAfter = v
	cfg.ContentFlagRejoin = 0
	if r := strings.TrimSpace(rejoin); r != "off" && r != "" {
		v, err := time.ParseDuration(r)
		if err != nil || v <= 0 {
			return fmt.Errorf("invalid -content-flag-rejoin %q (want off or a positive duration)", rejoin)
		}
		cfg.ContentFlagRejoin = v
	}
	return nil
}

func parseStaticWebhooks(raw string, getenv func(string) string) ([]StaticWebhook, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var hooks []StaticWebhook
	if err := json.Unmarshal([]byte(raw), &hooks); err != nil {
		return nil, fmt.Errorf("invalid -static-webhooks JSON: %w", err)
	}
	seen := make(map[string]bool, len(hooks))
	for i := range hooks {
		h := &hooks[i]
		h.Name = strings.TrimSpace(h.Name)
		if h.Name == "" {
			return nil, fmt.Errorf("-static-webhooks[%d]: name is required", i)
		}
		if seen[h.Name] {
			return nil, fmt.Errorf("-static-webhooks: duplicate name %q", h.Name)
		}
		seen[h.Name] = true
		if parsed, err := url.Parse(h.URL); err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return nil, fmt.Errorf("-static-webhooks[%s]: invalid url %q", h.Name, h.URL)
		}
		if h.SecretEnv == "" {
			return nil, fmt.Errorf("-static-webhooks[%s]: secretEnv is required so the signing key never sits in configuration", h.Name)
		}
		h.Secret = getenv(h.SecretEnv)
		if _, err := SigningKey(h.Secret); err != nil {
			// A secret that does not base64-decode can never verify at any
			// receiver's library (docs/52 D5); refusing to start is the
			// visible outcome.
			return nil, fmt.Errorf("-static-webhooks[%s]: environment variable %s: %w", h.Name, h.SecretEnv, err)
		}
		if h.Secret == "" {
			return nil, fmt.Errorf("-static-webhooks[%s]: environment variable %s is empty", h.Name, h.SecretEnv)
		}
		// An unknown event name is a typo that would silently never page;
		// refusing to start is the visible outcome (docs/50 D8).
		if err := store.ValidateWebhookEvents(h.Events); err != nil {
			return nil, fmt.Errorf("-static-webhooks[%s]: %w", h.Name, err)
		}
	}
	return hooks, nil
}

// ConfigWebhooks is the enabled chart-defined webhooks as the recording path
// hands them to the store: name and filter (docs/50 D8). Config webhooks are
// not rows, so the enqueue cannot read their filters itself.
func (c Config) ConfigWebhooks() []store.ConfigWebhook {
	var out []store.ConfigWebhook
	for _, h := range c.StaticWebhooks {
		if h.IsEnabled() {
			out = append(out, store.ConfigWebhook{Name: h.Name, Events: h.Events})
		}
	}
	return out
}

// LogAttrs is the startup log line's payload: every knob a pod resolved, with
// secret-bearing values reduced to whether they are set. It is the operator's
// only confirmation surface for what this pod actually parsed.
func (c Config) LogAttrs() []any {
	set := func(v string) string {
		if v == "" {
			return "<unset>"
		}
		return "<set>"
	}
	names := make([]string, 0, len(c.StaticWebhooks))
	for _, h := range c.StaticWebhooks {
		names = append(names, h.Name)
	}
	return []any{
		"mode", string(c.Mode),
		"addr", c.Addr,
		"metricsAddr", c.MetricsAddr,
		"externalUrl", c.ExternalURL,
		"oidcIssuer", c.OIDCIssuer,
		"oidcClientId", c.OIDCClientID,
		"oidcAudience", c.OIDCAudience,
		"oidcRolesClaim", c.RolesClaimPath(),
		"operatorRole", c.OperatorRole,
		"flaggerRole", c.FlaggerRole,
		"roomsReaderRole", c.RoomsReaderRole,
		"roomsManagerRole", c.RoomsManagerRole,
		"pgDsn", set(c.PGDSN),
		"relayScanTarget", c.RelayScanTarget,
		"relayOpsPort", c.RelayOpsPort,
		"relayAdminToken", set(c.RelayAdminToken),
		"namespace", c.Namespace,
		"killCooldown", c.KillCooldown.String(),
		"staticWebhooks", strings.Join(names, ","),
		"appBaseUrl", c.AppBaseURL,
		"telemetryBaseUrl", c.TelemetryBaseURL,
		"rooms", c.Rooms,
		"screening", c.Screening,
		"contentFlagRepage", c.ContentFlagRepage.String(),
		"contentFlagMute", c.ContentFlagMute.String(),
		"contentFlagStaleAfter", c.ContentFlagStaleAfter.String(),
		"contentFlagAutoKill", c.ContentFlagAutoKill.String(),
		"contentFlagDeadline", durationOrOff(c.ContentFlagDeadline),
		"screeningSampleRetention", c.ScreeningSampleRetention.String(),
		"screeningCritical", strconv.FormatFloat(c.ScreeningCritical, 'g', -1, 64),
		"screeningSustainedAfter", c.ScreeningSustainedAfter.String(),
		"contentFlagRejoin", durationOrOff(c.ContentFlagRejoin),
		// The creds PATH is not a secret; whether one is configured is the
		// question an operator debugging a silent bus is asking.
		"eventBusUrl", c.EventBusURL,
		"eventBusCredsFile", set(c.EventBusCredsFile),
		"eventBusStream", c.EventBusStream,
		"eventBusManageStream", c.EventBusManageStream,
		"eventBusTlsCert", c.EventBusTLSCert,
		"eventBusCaFile", c.EventBusCAFile,
		"eventBusInsecure", c.EventBusInsecure,
		"activityRetention", c.ActivityRetention.String(),
		"logLevel", c.LogLevel.String(),
		"logFormat", c.LogFormat,
	}
}

// roleOrOff maps an optional role knob to its claim value; "off" (or an empty
// value) grants the role nowhere. "off" is the spelling that survives the env:
// an empty GAWK_ADMIN_* variable reads as unset, i.e. the default role — the
// same reason -metrics-addr has it (R59).
func roleOrOff(v string) string {
	v = strings.TrimSpace(v)
	if v == "off" {
		return ""
	}
	return v
}

func durationOrOff(d time.Duration) string {
	if d <= 0 {
		return "off"
	}
	return d.String()
}

// metricsListen maps the -metrics-addr value to a listen address; "off" (or an
// empty value) disables the listener.
func metricsListen(v string) string {
	v = strings.TrimSpace(v)
	if v == "off" {
		return ""
	}
	return v
}
