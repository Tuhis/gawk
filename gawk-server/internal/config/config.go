// Package config holds the server configuration and its flag/env parsing.
//
// Precedence: command-line flag > environment variable > default.
// Every flag has a GAWK_*-prefixed environment fallback so the same binary
// is convenient both on the command line and in a k8s Deployment.
package config

import (
	"encoding/hex"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/Tuhis/gawk/gawk-server/internal/moderationsrc"
	"github.com/Tuhis/gawk/gawk-server/oidcroles"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// Telemetry reporting-cadence bounds (R28, docs/33). The floor is the client
// stats tick — reporting faster only resends the same numbers; the ceiling is
// what a uint16 of milliseconds can carry, and past it a session's shape is
// lost between samples anyway.
const (
	MinTelemetryReportInterval = 500 * time.Millisecond
	MaxTelemetryReportInterval = 60 * time.Second
)

// Admin-API authorization defaults (R39, docs/42 §4.5). The roles-claim
// default is Keycloak's client-roles path, with "{audience}" substituted by
// the configured audience at use time — the same role model the portal uses
// (docs/42 §4.8), so one IdP role grants both. The string itself lives in
// oidcroles, which is also what parses it: gawk-admin carries the same
// default, and a default that could drift between them is half of the bug
// this package exists to prevent.
const (
	DefaultAdminOIDCRolesClaim = oidcroles.DefaultClaim
	DefaultAdminOIDCRole       = "operator"
)

// Config is the fully-resolved server configuration.
type Config struct {
	Addr           string // UDP listen address, e.g. ":4433"
	CertFile       string // path to PEM cert; empty in dev-cert mode
	KeyFile        string // path to PEM key; empty in dev-cert mode
	DevCert        bool   // generate an in-memory ephemeral cert at startup
	DevCertHosts   string // comma-separated SANs for the dev cert
	LogLevel       slog.Level
	LogFormat      string // "text" or "json"
	MaxSubscribers int
	AllowedOrigins []string // empty = allow all (dev); checked on CONNECT

	MaxBroadcasts       int
	MaxTotalSubscribers int
	PublishSecret       string
	ConnRateLimit       float64
	ConnBurstLimit      int
	MaxBandwidthBytes   int64

	// MaxKeyframeBytes caps a single keyframe stream (R8); a publisher stream
	// exceeding it is reset and not cached.
	MaxKeyframeBytes int
	// KeyframeWriteTimeout bounds a keyframe write to one subscriber before the
	// stream is cancelled and the subscriber recovers at the next keyframe.
	KeyframeWriteTimeout time.Duration

	// MetricsAddr is the TCP listen address of the plain-HTTP ops endpoint
	// (/metrics, /healthz, /readyz, /statusz). Empty disables it. This is
	// separate from Addr because the WebTransport server is HTTP/3-over-UDP
	// only — Prometheus (and curl) need a TCP listener. Never expose this
	// port publicly.
	MetricsAddr string

	// ClusterMode enables the R17 federation layer (docs/22 Decision 1):
	// per-broadcast origin Leases in Kubernetes, edge pulls, drain lease
	// release. Off (the default) constructs no Kubernetes client at all —
	// single-pod behavior is byte-identical to pre-R17. Requires POD_NAME,
	// POD_IP and POD_NAMESPACE in the environment (downward API), plus
	// InternalPSK and InternalServerName below.
	ClusterMode bool

	// InternalPSK gates the pod-to-pod /internal/subscribe route (R17 W4,
	// docs/22 Decision 9). The route rides the same public UDP port as
	// viewers (there is only one listener), so the PSK is what keeps
	// non-fleet clients out. Required when ClusterMode is on.
	InternalPSK string

	// InternalServerName is the TLS server name edge pods verify when
	// dialing an origin's pod IP (docs/22 Decision 9): the public cert
	// hostname — no per-pod certs, no InsecureSkipVerify. Required when
	// ClusterMode is on.
	InternalServerName string

	// TrustedCIDRs bypass the per-IP connection rate limiter (R17 W5,
	// docs/22 Decision 13): under MetalLB L2 + externalTrafficPolicy:
	// Cluster, cross-node traffic is SNAT'd to node IPs — at a rollout an
	// entire pod's audience reconnects within ~1 s through a handful of
	// those, and the 3/s bucket would fail fresh joiners fatally. List the
	// node/pod CIDRs here; per-IP limiting is honestly best-effort under
	// etp=Cluster (real client IPs return with BGP/ECMP — deferred).
	TrustedCIDRs []*net.IPNet

	// StatsKey keys the /statusz + metrics broadcast-ID obfuscation (R17 W6,
	// docs/22 Decision 14): 32 bytes from 64 hex chars, shared fleet-wide so
	// one broadcast keeps one obfuscated identity across pods. Empty =
	// per-process random (the pre-R17 single-pod behavior).
	StatsKey []byte

	// ResumeTokenKey keys the resume-token HMAC (R17 W2, docs/22 Decision 7).
	// When set it WINS over the publish-secret derivation (PR #47 security
	// review): the publish secret is distributed to every broadcaster, so a
	// key derived from it is computable by every broadcaster — only this
	// independent, server-side key makes the resume token a real
	// per-broadcast ownership proof between secret-holders. Rotating it
	// revokes all tokens. 32 bytes from 64 hex chars, shared across all
	// relay pods. Empty: HKDF from the publish secret when one is set, else
	// a per-process random key (dev parity with process-lifetime reclaim).
	ResumeTokenKey []byte

	// TelemetryKey keys the R28 telemetry session token (docs/33 D2/§4.2):
	// 32 bytes from 64 hex chars, shared by every relay pod AND the
	// gawk-telemetry service, which verifies tokens statelessly with it.
	// Its presence IS the feature switch — with no key the relay cannot mint
	// a token, so it sends no TelemetryHello and every client collects
	// nothing, which is byte-identical to a relay predating R28. Never
	// logged. Rotating it revokes every outstanding token.
	TelemetryKey []byte

	// TelemetryReportInterval is the sampling cadence the relay asks clients
	// to use, carried in the hello so a fleet can turn the volume down
	// without shipping a new frontend. Clamped to something a uint16 of
	// milliseconds can carry and a client can honour.
	TelemetryReportInterval time.Duration

	// TelemetryAdvertiseURL is the fleet's telemetry ingest URL a
	// TelemetryEndpoint (wire 0x12) advertises to clients (R37, docs/40
	// §4.10 D14): it names infrastructure the relay does NOT itself serve —
	// ingest rides the operator's frontend Ingress — so it can only be
	// configured, never derived. Validated at parse (an invalid URL fails
	// startup, not a silent no-send); empty means nothing is advertised.
	// Only meaningful while telemetry is enabled (the key is present).
	TelemetryAdvertiseURL string

	// ServerName is the operator display name a RelayIdentity (wire 0x11)
	// carries on /echo sessions (R37, docs/40 §4.4) so server pickers can
	// label this relay. Validated at parse against the wire limits (UTF-8,
	// ≤ wire.MaxRelayIdentityNameLen bytes); empty means unset.
	ServerName string

	// ReleaseVersion is the build version stamped by main (-ldflags), not a
	// flag — carried here so the transport can put it in RelayIdentity
	// without importing main.
	ReleaseVersion string

	// StatelessResetKey is the 32-byte QUIC stateless reset key (R17 W1,
	// docs/22 Decision 3), decoded from 64 hex chars. Shared across every
	// relay pod, it lets ANY pod answer packets for a connection it doesn't
	// know with a stateless reset the client accepts — turning an abrupt pod
	// death (or a kube-proxy conntrack re-DNAT) into ~1 RTT of detection
	// instead of the ~30 s idle timeout. Empty disables (today's behavior).
	// Never logged.
	StatelessResetKey []byte

	// Suppresses the INFO "session started"/"session ended" logs for /echo
	// sessions from loopback (the k8s exec probe hitting 127.0.0.1, which
	// otherwise logs on every startup/liveness/readiness probe forever).
	// Off by default so plain binary/local-dev runs log everything as
	// usual; the Helm chart turns it on.
	QuietProbeLogs bool

	// R21 DVR ring (docs/26). DVRWindow is how much history a broadcast
	// retains for resilient subscribers and is also the ceiling a viewer's
	// requested buffer is clamped to; DVRMaxBytes is the bound that actually
	// protects the pod, since 3 s of a 50 Mbps broadcaster is 18 MB.
	DVRWindow   time.Duration
	DVRMaxBytes int
	// DVRMaxCatchup caps a recovering DVR subscriber's send rate as a multiple
	// of the broadcast's own bitrate. Negative disables the ceiling.
	DVRMaxCatchup float64
	// DVRAudio puts audio in the ring too. On by default: a video-only DVR
	// fixes the picture and leaves the sound full of holes, which is the half
	// users notice. Off restores docs/20 field finding 5's behaviour exactly.
	DVRAudio bool
	// LiveEdgeAudioOnReliableStream extends the audio carrier to plain live-edge viewers.
	// Off by default: reliable and DVR viewers buffer enough that a retransmit
	// is free, a live-edge viewer may not, and which way that lands depends on
	// its RTT. Measure before flipping it fleet-wide.
	LiveEdgeAudioOnReliableStream bool

	// ParityDefault is the fleet's R29 forward-parity level (docs/34 §5.3):
	// how many parity symbols producers emit per delta frame, and the ceiling
	// on what any subscriber can be served. Default 2 — the owner's
	// quality-first call: a viewer gets full protection without finding a
	// menu, at ~22% egress, and the menu offers only opt-DOWN because a
	// viewer cannot conjure symbols the producer never emitted.
	//
	// 0 disables the feature fleet-wide from one value: no capability is
	// advertised, so producers emit nothing and the wire is byte-identical
	// to a relay predating R29.
	ParityDefault int

	// StripedDelivery enables R30 stripe legs (docs/35): ?stripe=N&leg=j
	// subscribe sessions and the StripeState suppression signal. On by
	// default — the relay cost is zero until a viewer actually engages
	// (only viewers whose path measures the burst threshold do, plus manual
	// opt-ins). Off: the capability bit is never advertised (so a
	// well-behaved viewer never dials a leg), leg dials get 400, StripeState
	// is ignored, and the relay is byte-identical to pre-R30. The stripe
	// width cap and burst target are constants, not knobs (docs/35 §11).
	StripedDelivery bool

	// ModerationSource selects the R39 ban source (docs/42 §4.3), kept
	// verbatim so the startup log states exactly what the operator asked
	// for. One of:
	//
	//	off            (default) nothing is constructed; the ban set stays
	//	               empty and every publish-path check is a cheap miss —
	//	               byte-identical to a relay predating R39.
	//	k8s            informer on Ban CRs in POD_NAMESPACE. Independent of
	//	               ClusterMode: enforcement is not a federation feature.
	//	file:<path>    JSON array of moderation.Records, reloaded on change
	//	               and on SIGHUP — the dev/compose lane (docs/42 §4.14).
	//
	// Parsed (and rejected) at startup by internal/moderationsrc.Parse, the
	// same parser the source itself uses.
	ModerationSource string

	// AdminAPIToken is the static bearer credential for the R39 relay admin
	// API on the ops listener (docs/42 §4.5) — the machine path gawk-admin
	// uses. Deliberately NOT InternalPSK (docs/42 §5): different trust domain
	// (admin service vs. peer pods), independent rotation, and the PSK travels
	// in URLs on the media path where this token travels in a header.
	// Compared in constant time. Never logged.
	AdminAPIToken string

	// AdminOIDCIssuer / AdminOIDCAudience are the alternative credential for
	// the same routes: an OIDC JWT with this issuer and audience, verified
	// offline against a background-refreshed JWKS. Both set or both empty —
	// a half-configured pair is rejected at parse, because "issuer set,
	// audience empty" would otherwise mean "accept any audience", which is
	// the failure mode nobody notices.
	//
	// When BOTH this and AdminAPIToken are empty the admin routes are not
	// registered at all: the surface stays dark (404), not merely locked.
	AdminOIDCIssuer   string
	AdminOIDCAudience string

	// AdminOIDCRolesClaim is the dot-path to the token's roles array.
	// Defaults to the Keycloak client-roles path
	// "resource_access.{audience}.roles"; "{audience}" is substituted with
	// AdminOIDCAudience. AdminOIDCRole is the role a token must carry
	// (default "operator"). Neither may be empty while OIDC is configured —
	// blanking either would turn authorization off silently.
	AdminOIDCRolesClaim string
	AdminOIDCRole       string

	// The effective QUIC idle timeout is the minimum of both endpoints'
	// advertised values (browsers advertise ~30s), so raising this alone
	// does not keep idle viewers alive — KeepAlivePeriod is the mechanism.
	MaxIdleTimeout  time.Duration // QUIC idle timeout for all sessions
	KeepAlivePeriod time.Duration // server-sent QUIC PING interval; 0 disables
	BroadcastGrace  time.Duration // broadcast GC grace period after publisher disconnects
}

// ParseFlags parses args (without the program name) into a Config.
// getenv supplies environment lookups, injectable for tests.
func ParseFlags(args []string, getenv func(string) string) (Config, error) {
	// envBool reads a boolean env var that defaults to TRUE. The plain
	// `env(...) == "true"` idiom the default-false flags use cannot express
	// that: an unset variable and an explicit "false" both read as empty
	// there, so a default-true flag written that way could never be turned
	// off. Uses the injected getenv like env does, so tests can drive it.
	envBool := func(key string, def bool) bool {
		switch strings.ToLower(strings.TrimSpace(getenv(key))) {
		case "true", "1", "yes":
			return true
		case "false", "0", "no":
			return false
		default:
			return def
		}
	}
	env := func(key, def string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return def
	}

	fs := flag.NewFlagSet("gawk-server", flag.ContinueOnError)
	addr := fs.String("addr", env("GAWK_ADDR", ":4433"), "UDP listen address")
	certFile := fs.String("cert-file", env("GAWK_CERT_FILE", ""), "path to PEM certificate")
	keyFile := fs.String("key-file", env("GAWK_KEY_FILE", ""), "path to PEM private key")
	devCert := fs.Bool("dev-cert", env("GAWK_DEV_CERT", "") == "true" || env("GAWK_DEV_CERT", "") == "1",
		"generate an in-memory ephemeral dev certificate")
	devCertHosts := fs.String("dev-cert-hosts", env("GAWK_DEV_CERT_HOSTS", "localhost,127.0.0.1"),
		"comma-separated hosts for the dev certificate")
	logLevel := fs.String("log-level", env("GAWK_LOG_LEVEL", "info"), "log level: debug|info|warn|error")
	logFormat := fs.String("log-format", env("GAWK_LOG_FORMAT", "text"), "log format: text|json")
	maxSubs := fs.String("max-subscribers", env("GAWK_MAX_SUBSCRIBERS", "15"), "maximum concurrent subscribers")
	origins := fs.String("allowed-origins", env("GAWK_ALLOWED_ORIGINS", ""),
		"comma-separated allowed Origin values; empty allows all")
	maxIdle := fs.String("max-idle-timeout", env("GAWK_MAX_IDLE_TIMEOUT", "30s"),
		"QUIC idle timeout for all sessions")
	keepalive := fs.String("keepalive-period", env("GAWK_KEEPALIVE_PERIOD", "10s"),
		"QUIC keepalive PING interval; keeps idle viewers alive while the broadcaster is away; 0 disables")
	quietProbeLogs := fs.Bool("quiet-probe-logs",
		env("GAWK_QUIET_PROBE_LOGS", "") == "true" || env("GAWK_QUIET_PROBE_LOGS", "") == "1",
		"suppress INFO logs for loopback /echo sessions (k8s exec probes)")
	broadcastGrace := fs.String("broadcast-grace", env("GAWK_BROADCAST_GRACE", "5m"),
		"broadcast GC grace period after publisher disconnects")
	maxBroadcasts := fs.String("max-broadcasts", env("GAWK_MAX_BROADCASTS", "5"),
		"maximum concurrent broadcasts")
	maxTotalSubs := fs.String("max-total-subscribers", env("GAWK_MAX_TOTAL_SUBSCRIBERS", "50"),
		"maximum total subscribers across all broadcasts")
	pubSecret := fs.String("publish-secret", env("GAWK_PUBLISH_SECRET", ""),
		"shared secret required to publish")
	connRateLimit := fs.String("conn-rate-limit", env("GAWK_CONN_RATE_LIMIT", "3.0"),
		"connection attempts rate limit per client IP per second; 0 disables")
	connBurstLimit := fs.String("conn-burst-limit", env("GAWK_CONN_BURST_LIMIT", "10"),
		"connection attempts burst limit per client IP")
	maxBandwidth := fs.String("max-bandwidth", env("GAWK_MAX_BANDWIDTH", "0"),
		"global egress bandwidth limit; e.g. 10mbps")
	maxKeyframeBytes := fs.String("max-keyframe-bytes", env("GAWK_MAX_KEYFRAME_BYTES", "8388608"),
		"maximum bytes for a single reliable keyframe stream (default 8 MiB)")
	keyframeWriteTimeout := fs.String("keyframe-write-timeout", env("GAWK_KEYFRAME_WRITE_TIMEOUT", "1s"),
		"how long a keyframe write to one subscriber may block before the stream is cancelled")
	// "off" (not just "") disables, because an empty env var reads as unset
	// and would silently fall back to the default instead of disabling.
	dvrWindow := fs.String("dvr-window", env("GAWK_DVR_WINDOW", "3s"),
		"R21 DVR ring depth per broadcast: how long a stall a resilient viewer can ride out")
	dvrMaxBytes := fs.String("dvr-max-bytes", env("GAWK_DVR_MAX_BYTES", "25165824"),
		"maximum bytes one broadcast's DVR ring may retain (default 24 MiB)")
	dvrMaxCatchup := fs.String("dvr-max-catchup", env("GAWK_DVR_MAX_CATCHUP", "4"),
		"how much faster than live a recovering DVR subscriber may send, as a multiple of the broadcast bitrate; negative disables")
	dvrAudio := fs.Bool("dvr-audio", envBool("GAWK_DVR_AUDIO", true),
		"put audio in the DVR ring too, on its own stream (R21 DV5); off leaves audio live-edge")
	liveEdgeAudioOnReliableStream := fs.Bool("live-edge-audio-on-reliable-stream", envBool("GAWK_LIVE_EDGE_AUDIO_ON_RELIABLE_STREAM", false),
		"deliver live-edge viewers' audio on its own reliable stream instead of datagrams; video stays unreliable")
	parityDefault := fs.String("parity-default", env("GAWK_PARITY_DEFAULT", "2"),
		"forward-parity symbols producers emit per delta frame, and the per-subscriber ceiling (R29); 0 disables fleet-wide")
	stripedDelivery := fs.Bool("striped-delivery", envBool("GAWK_STRIPED_DELIVERY", true),
		"accept R30 stripe-leg subscribe sessions (?stripe=N&leg=j) and StripeState suppression; off is byte-identical to pre-R30")
	metricsAddr := fs.String("metrics-addr", env("GAWK_METRICS_ADDR", ":2112"),
		"TCP listen address for the ops endpoint (/metrics, /healthz, /statusz); \"off\" disables")
	statelessResetKey := fs.String("stateless-reset-key", env("GAWK_STATELESS_RESET_KEY", ""),
		"QUIC stateless reset key as 64 hex chars (32 bytes), shared across all relay pods; empty disables")
	resumeTokenKey := fs.String("resume-token-key", env("GAWK_RESUME_TOKEN_KEY", ""),
		"resume-token HMAC key as 64 hex chars (32 bytes), shared across relay pods; wins over the publish-secret derivation when set; empty = derive from the publish secret, else per-process random")
	clusterMode := fs.Bool("cluster-mode",
		env("GAWK_CLUSTER_MODE", "") == "true" || env("GAWK_CLUSTER_MODE", "") == "1",
		"enable multi-pod federation (per-broadcast k8s origin Leases, edge pulls); off = single-pod behavior")
	internalPSK := fs.String("internal-psk", env("GAWK_INTERNAL_PSK", ""),
		"pre-shared key gating the pod-to-pod /internal/subscribe route; required with -cluster-mode")
	internalServerName := fs.String("internal-server-name", env("GAWK_INTERNAL_SERVER_NAME", ""),
		"TLS server name edge pods verify when dialing an origin's pod IP (the public cert hostname); required with -cluster-mode")
	trustedCIDRs := fs.String("trusted-cidrs", env("GAWK_TRUSTED_CIDRS", ""),
		"comma-separated CIDRs that bypass the per-IP connection rate limiter (node/pod CIDRs under SNAT)")
	statsKey := fs.String("stats-key", env("GAWK_STATS_KEY", ""),
		"statusz/metrics broadcast-ID obfuscation key as 64 hex chars (32 bytes), shared fleet-wide; empty = per-process random")
	telemetryKey := fs.String("telemetry-key", env("GAWK_TELEMETRY_KEY", ""),
		"R28 telemetry session-token HMAC key as 64 hex chars (32 bytes), shared with the gawk-telemetry service; empty disables telemetry entirely (no hello is sent)")
	telemetryReportInterval := fs.String("telemetry-report-interval", env("GAWK_TELEMETRY_REPORT_INTERVAL", "2s"),
		"sampling cadence the relay asks telemetry clients to use")
	telemetryAdvertiseURL := fs.String("telemetry-advertise-url", env("GAWK_TELEMETRY_ADVERTISE_URL", ""),
		"absolute https URL of this fleet's telemetry ingest, advertised to clients in-band (R37 wire 0x12); empty advertises nothing")
	serverName := fs.String("server-name", env("GAWK_SERVER_NAME", ""),
		"operator display name advertised to server pickers over /echo (R37 wire 0x11); empty leaves the relay unnamed")
	moderationSource := fs.String("moderation-source", env("GAWK_MODERATION_SOURCE", "off"),
		"R39 ban source: off | k8s (Ban CRs in POD_NAMESPACE) | file:<path> (JSON array, reloaded on change and SIGHUP)")
	adminAPIToken := fs.String("admin-api-token", env("GAWK_ADMIN_API_TOKEN", ""),
		"static bearer token for the R39 admin API on the ops listener (/internal/admin/*); empty and no OIDC issuer leaves those routes unregistered (404)")
	adminOIDCIssuer := fs.String("admin-oidc-issuer", env("GAWK_ADMIN_OIDC_ISSUER", ""),
		"OIDC issuer URL whose JWTs are accepted on /internal/admin/*; must be set together with -admin-oidc-audience")
	adminOIDCAudience := fs.String("admin-oidc-audience", env("GAWK_ADMIN_OIDC_AUDIENCE", ""),
		"OIDC audience (client ID) a JWT must carry on /internal/admin/*; must be set together with -admin-oidc-issuer")
	adminOIDCRolesClaim := fs.String("admin-oidc-roles-claim", env("GAWK_ADMIN_OIDC_ROLES_CLAIM", DefaultAdminOIDCRolesClaim),
		"dot-path to the roles array inside an admin JWT; {audience} is substituted into each segment")
	adminOIDCRole := fs.String("admin-oidc-role", env("GAWK_ADMIN_OIDC_ROLE", DefaultAdminOIDCRole),
		"role an admin JWT must carry in the roles claim")

	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}

	// THE ADMIN-API KNOBS ARE TRIMMED ONCE, HERE, so every later reader —
	// the validation below and the Config literal at the end — sees the same
	// string. They did not: the all-or-nothing pair check ran on the raw flag
	// values while the literal stored trimmed ones, so a stray space or
	// newline out of a templated Secret (GAWK_ADMIN_OIDC_ISSUER=" ") looked
	// set to the check and arrived empty in the Config. The result was not the
	// refusal the check exists to produce but silence: no verifier, no static
	// token, Configured() false, and /internal/admin/* never registered at
	// all — a mystery 404 (docs/42 §4.5).
	//
	// The token is trimmed for the same reason it is compared trimmed:
	// AdminAuth.authorize trims the PRESENTED credential before the
	// constant-time compare, so a configured token carrying whitespace could
	// never be matched by anybody. It would register the routes and then
	// refuse every caller, which is the same silent failure wearing a 401.
	for _, p := range []*string{
		adminAPIToken, adminOIDCIssuer, adminOIDCAudience, adminOIDCRolesClaim, adminOIDCRole,
	} {
		*p = strings.TrimSpace(*p)
	}

	level, err := parseLogLevel(*logLevel)
	if err != nil {
		return Config{}, err
	}
	if *logFormat != "text" && *logFormat != "json" {
		return Config{}, fmt.Errorf("invalid log format %q: want text or json", *logFormat)
	}
	n, err := strconv.Atoi(*maxSubs)
	if err != nil || n < 1 {
		return Config{}, fmt.Errorf("invalid max-subscribers %q: want a positive integer", *maxSubs)
	}
	if (*certFile == "") != (*keyFile == "") {
		return Config{}, fmt.Errorf("cert-file and key-file must be set together")
	}
	idleTimeout, err := time.ParseDuration(*maxIdle)
	if err != nil || idleTimeout <= 0 {
		return Config{}, fmt.Errorf("invalid max-idle-timeout %q: want a positive duration", *maxIdle)
	}
	keepalivePeriod, err := time.ParseDuration(*keepalive)
	if err != nil || keepalivePeriod < 0 {
		return Config{}, fmt.Errorf("invalid keepalive-period %q: want a non-negative duration", *keepalive)
	}
	if keepalivePeriod > 0 && keepalivePeriod >= idleTimeout {
		return Config{}, fmt.Errorf("keepalive-period %v must be less than max-idle-timeout %v", keepalivePeriod, idleTimeout)
	}
	graceDuration, err := time.ParseDuration(*broadcastGrace)
	if err != nil || graceDuration <= 0 {
		return Config{}, fmt.Errorf("invalid broadcast-grace %q: want a positive duration", *broadcastGrace)
	}
	parityDef, err := strconv.Atoi(*parityDefault)
	if err != nil || parityDef < 0 || parityDef > wire.MaxParitySymbols {
		return Config{}, fmt.Errorf("invalid parity-default %q: want an integer in [0, %d]", *parityDefault, wire.MaxParitySymbols)
	}
	maxB, err := strconv.Atoi(*maxBroadcasts)
	if err != nil || maxB < 1 {
		return Config{}, fmt.Errorf("invalid max-broadcasts %q: want a positive integer", *maxBroadcasts)
	}
	maxTotal, err := strconv.Atoi(*maxTotalSubs)
	if err != nil || maxTotal < 1 {
		return Config{}, fmt.Errorf("invalid max-total-subscribers %q: want a positive integer", *maxTotalSubs)
	}
	rateLimit, err := strconv.ParseFloat(*connRateLimit, 64)
	if err != nil || rateLimit < 0 {
		return Config{}, fmt.Errorf("invalid conn-rate-limit %q: want a non-negative float", *connRateLimit)
	}
	burstLimit, err := strconv.Atoi(*connBurstLimit)
	if err != nil || burstLimit < 1 {
		return Config{}, fmt.Errorf("invalid conn-burst-limit %q: want a positive integer", *connBurstLimit)
	}
	bandwidthBytes, err := parseBandwidth(*maxBandwidth)
	if err != nil {
		return Config{}, err
	}
	kfBytes, err := strconv.Atoi(*maxKeyframeBytes)
	if err != nil || kfBytes < 1 {
		return Config{}, fmt.Errorf("invalid max-keyframe-bytes %q: want a positive integer", *maxKeyframeBytes)
	}
	kfWriteTimeout, err := time.ParseDuration(*keyframeWriteTimeout)
	if err != nil || kfWriteTimeout <= 0 {
		return Config{}, fmt.Errorf("invalid keyframe-write-timeout %q: want a positive duration", *keyframeWriteTimeout)
	}
	dvrWin, err := time.ParseDuration(*dvrWindow)
	if err != nil || dvrWin <= 0 {
		return Config{}, fmt.Errorf("invalid dvr-window %q: want a positive duration", *dvrWindow)
	}
	dvrBytes, err := strconv.Atoi(*dvrMaxBytes)
	if err != nil || dvrBytes < 1 {
		return Config{}, fmt.Errorf("invalid dvr-max-bytes %q: want a positive integer", *dvrMaxBytes)
	}
	dvrCatchup, err := strconv.ParseFloat(*dvrMaxCatchup, 64)
	if err != nil {
		return Config{}, fmt.Errorf("invalid dvr-max-catchup %q: want a number", *dvrMaxCatchup)
	}
	if dvrCatchup >= 0 && dvrCatchup < 1 {
		// Below 1x the ceiling would throttle a subscriber BELOW live and
		// manufacture the backlog it exists to bound — always an operator
		// mistake, never a policy.
		return Config{}, fmt.Errorf("invalid dvr-max-catchup %q: want >= 1 (or negative to disable)", *dvrMaxCatchup)
	}
	mAddr := strings.TrimSpace(*metricsAddr)
	if strings.EqualFold(mAddr, "off") {
		mAddr = ""
	}
	resetKey, err := parseHexKey32("stateless-reset-key", *statelessResetKey)
	if err != nil {
		return Config{}, err
	}
	resumeKey, err := parseHexKey32("resume-token-key", *resumeTokenKey)
	if err != nil {
		return Config{}, err
	}
	if *clusterMode && (*internalPSK == "" || *internalServerName == "") {
		return Config{}, fmt.Errorf("cluster-mode requires -internal-psk and -internal-server-name")
	}
	cidrs, err := parseCIDRs(*trustedCIDRs)
	if err != nil {
		return Config{}, err
	}
	statsKeyBytes, err := parseHexKey32("stats-key", *statsKey)
	if err != nil {
		return Config{}, err
	}
	telemetryKeyBytes, err := parseHexKey32("telemetry-key", *telemetryKey)
	if err != nil {
		return Config{}, err
	}
	telemetryInterval, err := time.ParseDuration(*telemetryReportInterval)
	if err != nil {
		return Config{}, fmt.Errorf("invalid telemetry-report-interval %q: %w", *telemetryReportInterval, err)
	}
	// The hello carries the interval as a uint16 of milliseconds, and a client
	// that reports faster than the stats tick just resends the same numbers.
	if telemetryInterval < MinTelemetryReportInterval || telemetryInterval > MaxTelemetryReportInterval {
		return Config{}, fmt.Errorf("invalid telemetry-report-interval %q: want %v-%v",
			*telemetryReportInterval, MinTelemetryReportInterval, MaxTelemetryReportInterval)
	}

	if *telemetryAdvertiseURL != "" {
		// The wire package owns the one URL rule (absolute https, bounded);
		// failing startup here is SP11's fail-fast acceptance criterion.
		if _, err := wire.AppendTelemetryEndpoint(nil, *telemetryAdvertiseURL); err != nil {
			return Config{}, fmt.Errorf("invalid telemetry-advertise-url %q: %w", *telemetryAdvertiseURL, err)
		}
	}
	// R39 (docs/42 §4.3): validated with the very parser the source uses, so
	// a value that starts the process is a value the source can honour.
	if _, _, err := moderationsrc.Parse(*moderationSource); err != nil {
		return Config{}, err
	}
	// R39 AP3 (docs/42 §4.5): the OIDC pair is all-or-nothing. An issuer with
	// no audience would verify signatures and then accept a token minted for
	// ANY client of that IdP; an audience with no issuer is inert. Both are
	// silent failures, so neither starts.
	if (*adminOIDCIssuer == "") != (*adminOIDCAudience == "") {
		return Config{}, fmt.Errorf("admin-oidc-issuer and admin-oidc-audience must be set together")
	}
	if *adminOIDCIssuer != "" {
		// Blanking either of these would leave signature+issuer+audience
		// checked and AUTHORIZATION off — every token holder an operator.
		// Whitespace counts as blank: these are already trimmed above, so a
		// value that survives here is one oidcroles can actually address.
		if *adminOIDCRolesClaim == "" {
			return Config{}, fmt.Errorf("admin-oidc-roles-claim must not be empty when admin OIDC is configured")
		}
		if *adminOIDCRole == "" {
			return Config{}, fmt.Errorf("admin-oidc-role must not be empty when admin OIDC is configured")
		}
	}
	if *serverName != "" {
		// Same source of truth for the name limits ("x" stands in for the
		// version, which main stamps later).
		if _, err := wire.AppendRelayIdentity(nil, wire.RelayIdentity{ServerVersion: "x", Name: *serverName}); err != nil {
			return Config{}, fmt.Errorf("invalid server-name %q: %w", *serverName, err)
		}
	}

	return Config{
		Addr:           *addr,
		CertFile:       *certFile,
		KeyFile:        *keyFile,
		DevCert:        *devCert,
		DevCertHosts:   *devCertHosts,
		LogLevel:       level,
		LogFormat:      *logFormat,
		MaxSubscribers: n,
		AllowedOrigins: splitNonEmpty(*origins),
		QuietProbeLogs: *quietProbeLogs,

		MaxBroadcasts:       maxB,
		MaxTotalSubscribers: maxTotal,
		PublishSecret:       *pubSecret,
		ConnRateLimit:       rateLimit,
		ConnBurstLimit:      burstLimit,
		MaxBandwidthBytes:   bandwidthBytes,

		MaxKeyframeBytes:              kfBytes,
		KeyframeWriteTimeout:          kfWriteTimeout,
		DVRWindow:                     dvrWin,
		DVRMaxBytes:                   dvrBytes,
		DVRMaxCatchup:                 dvrCatchup,
		DVRAudio:                      *dvrAudio,
		LiveEdgeAudioOnReliableStream: *liveEdgeAudioOnReliableStream,
		ParityDefault:                 parityDef,
		StripedDelivery:               *stripedDelivery,

		TelemetryAdvertiseURL: *telemetryAdvertiseURL,
		ServerName:            *serverName,
		ModerationSource:      strings.TrimSpace(*moderationSource),

		// Trimmed once, right after fs.Parse — never again here, or the
		// validation above and this literal could disagree a second time.
		AdminAPIToken:       *adminAPIToken,
		AdminOIDCIssuer:     *adminOIDCIssuer,
		AdminOIDCAudience:   *adminOIDCAudience,
		AdminOIDCRolesClaim: *adminOIDCRolesClaim,
		AdminOIDCRole:       *adminOIDCRole,

		MetricsAddr:        mAddr,
		ClusterMode:        *clusterMode,
		InternalPSK:        *internalPSK,
		InternalServerName: *internalServerName,
		TrustedCIDRs:       cidrs,
		StatsKey:           statsKeyBytes,
		ResumeTokenKey:     resumeKey,
		StatelessResetKey:  resetKey,

		TelemetryKey:            telemetryKeyBytes,
		TelemetryReportInterval: telemetryInterval,

		MaxIdleTimeout:  idleTimeout,
		KeepAlivePeriod: keepalivePeriod,
		BroadcastGrace:  graceDuration,
	}, nil
}

func parseLogLevel(s string) (slog.Level, error) {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("invalid log level %q: want debug, info, warn or error", s)
}

func splitNonEmpty(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// parseCIDRs parses a comma-separated CIDR list ("" = none).
func parseCIDRs(s string) ([]*net.IPNet, error) {
	var out []*net.IPNet
	for _, part := range splitNonEmpty(s) {
		_, ipnet, err := net.ParseCIDR(part)
		if err != nil {
			return nil, fmt.Errorf("invalid trusted-cidrs entry %q: %w", part, err)
		}
		out = append(out, ipnet)
	}
	return out, nil
}

// parseHexKey32 decodes a 32-byte hex-encoded key flag: empty (disabled) or
// exactly 64 hex chars. The decoded bytes are never logged.
func parseHexKey32(name, s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	key, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("invalid %s: want 64 hex chars: %w", name, err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("invalid %s: got %d bytes, want exactly 32", name, len(key))
	}
	return key, nil
}

func parseBandwidth(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" || s == "0" || s == "unlimited" {
		return 0, nil
	}
	var multiplier int64 = 1
	if strings.HasSuffix(s, "mbps") {
		multiplier = 1000 * 1000 / 8
		s = strings.TrimSuffix(s, "mbps")
	} else if strings.HasSuffix(s, "kbps") {
		multiplier = 1000 / 8
		s = strings.TrimSuffix(s, "kbps")
	} else if strings.HasSuffix(s, "m") {
		multiplier = 1000 * 1000 / 8
		s = strings.TrimSuffix(s, "m")
	} else if strings.HasSuffix(s, "k") {
		multiplier = 1000 / 8
		s = strings.TrimSuffix(s, "k")
	}
	val, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || val < 0 {
		return 0, fmt.Errorf("invalid bandwidth format %q", s)
	}
	return val * multiplier, nil
}
