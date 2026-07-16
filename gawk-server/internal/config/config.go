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

	// ResumeTokenKey keys the resume-token HMAC (R17 W2, docs/22 Decision 7)
	// when no publish secret is set (a publish secret always wins — rotating
	// it revokes all tokens). 32 bytes from 64 hex chars, shared across all
	// relay pods. Empty (and no publish secret) falls back to a per-process
	// random key: dev parity with the old process-lifetime reclaim.
	ResumeTokenKey []byte

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
	metricsAddr := fs.String("metrics-addr", env("GAWK_METRICS_ADDR", ":2112"),
		"TCP listen address for the ops endpoint (/metrics, /healthz, /statusz); \"off\" disables")
	statelessResetKey := fs.String("stateless-reset-key", env("GAWK_STATELESS_RESET_KEY", ""),
		"QUIC stateless reset key as 64 hex chars (32 bytes), shared across all relay pods; empty disables")
	resumeTokenKey := fs.String("resume-token-key", env("GAWK_RESUME_TOKEN_KEY", ""),
		"resume-token HMAC key as 64 hex chars (32 bytes), used only when no publish secret is set; empty = per-process random")
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

	if err := fs.Parse(args); err != nil {
		return Config{}, err
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

		MaxKeyframeBytes:     kfBytes,
		KeyframeWriteTimeout: kfWriteTimeout,

		MetricsAddr:        mAddr,
		ClusterMode:        *clusterMode,
		InternalPSK:        *internalPSK,
		InternalServerName: *internalServerName,
		TrustedCIDRs:       cidrs,
		StatsKey:           statsKeyBytes,
		ResumeTokenKey:     resumeKey,
		StatelessResetKey:  resetKey,

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
