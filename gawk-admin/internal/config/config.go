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
	"net/url"
	"strconv"
	"strings"
	"time"

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
}

// IsEnabled reports whether this webhook should receive deliveries. Absent
// means enabled — the common case should need no ceremony.
func (w StaticWebhook) IsEnabled() bool { return w.Enabled == nil || *w.Enabled }

// Config is the fully-resolved gawk-admin configuration.
type Config struct {
	Mode Mode

	Addr        string // the single HTTP listener: SPA, /api/v1, /auth/config, /healthz, /readyz
	ExternalURL string // portal base URL: the OIDC redirect base and the portalUrl in webhook payloads

	OIDCIssuer     string
	OIDCClientID   string
	OIDCAudience   string
	OIDCRolesClaim string // dot-path template to the roles array; oidcroles.Placeholder is substituted per segment
	OperatorRole   string // the role every R39 route requires
	FlaggerRole    string // reserved for R40's service identity; unused by any R39 route

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
		"reserved for R40: the role granting flag-only rights; unused by any R39 route")
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
	roomsOn := fs.String("rooms", env("GAWK_ADMIN_ROOMS", "false"),
		"enable room management (R42, docs/44 D20): /api/v1/rooms, the rooms view and the room sweep; needs the chart's rooms.enabled RBAC")
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
		ExternalURL:      strings.TrimRight(*externalURL, "/"),
		OIDCIssuer:       strings.TrimRight(*issuer, "/"),
		OIDCClientID:     *clientID,
		OIDCAudience:     *audience,
		OIDCRolesClaim:   strings.TrimSpace(*rolesClaim),
		OperatorRole:     strings.TrimSpace(*operatorRole),
		FlaggerRole:      strings.TrimSpace(*flaggerRole),
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
		if h.Secret == "" {
			return nil, fmt.Errorf("-static-webhooks[%s]: environment variable %s is empty", h.Name, h.SecretEnv)
		}
	}
	return hooks, nil
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
		"externalUrl", c.ExternalURL,
		"oidcIssuer", c.OIDCIssuer,
		"oidcClientId", c.OIDCClientID,
		"oidcAudience", c.OIDCAudience,
		"oidcRolesClaim", c.RolesClaimPath(),
		"operatorRole", c.OperatorRole,
		"flaggerRole", c.FlaggerRole,
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
		"logLevel", c.LogLevel.String(),
		"logFormat", c.LogFormat,
	}
}
