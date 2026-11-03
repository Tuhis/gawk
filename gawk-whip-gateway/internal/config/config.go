// Package config holds gawk-whip-gateway's configuration (docs/83 §6).
//
// Precedence: command-line flag > GAWK_WHIP_* environment variable >
// default, the contract every gawk binary keeps. Every knob is validated at
// start-up and Options() hands each one to the object that consumes it;
// TestEveryFlagReachesOptions walks every flag to that object
// (CODE-REVIEW.md, the R2 lesson). Secrets are logged only as set/unset.
//
// Defaults marked "provisional until WG0" are docs/83's design numbers; the
// lab measurement with real OBS (WG0) has not run yet.
package config

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Tuhis/gawk/gawk-whip-gateway/internal/clientip"
	"github.com/Tuhis/gawk/gawk-whip-gateway/internal/gateway"
	"github.com/Tuhis/gawk/gawk-whip-gateway/internal/media"
	"github.com/Tuhis/gawk/gawk-whip-gateway/internal/status"
)

// EnvPrefix prefixes every environment twin: -relay-url is
// GAWK_WHIP_RELAY_URL.
const EnvPrefix = "GAWK_WHIP_"

// Defaults.
const (
	DefaultListen         = ":8080"
	DefaultInternalListen = ":8081"
	DefaultOpsListen      = ":9090"
	DefaultMediaBasePort  = 8189
	DefaultMaxSessions    = 20 // provisional until WG0 (D10)
	DefaultPOSTRate       = 1.0
	DefaultPOSTBurst      = 5
	// The status limit: the OBS page polls every 2 s (U7), and a few tabs
	// or a broadcaster page's one read on load fit the burst.
	DefaultStatusRate  = 2.0
	DefaultStatusBurst = 10
	DefaultMaxRecords  = 4096
)

// Config is the fully resolved configuration.
type Config struct {
	Listen         string
	InternalListen string
	OpsListen      string
	TLSCertFile    string
	TLSKeyFile     string

	RelayURL           string
	RelayPublishSecret string
	RelayProxyToken    string
	RelayOrigin        string
	RelayInsecure      bool
	RelayDialTimeout   time.Duration

	PublicAddress   string
	MediaBind       string
	MediaBasePort   int
	PodName         string
	Ordinal         int
	MediaPort       int // MediaBasePort + Ordinal
	ICEDisconnected time.Duration
	ICEFailed       time.Duration
	ICEKeepalive    time.Duration

	AllowedOrigins      []string
	TrustedProxyCIDRs   []string
	POSTRate            float64
	POSTBurst           int
	StatusRate          float64
	StatusBurst         int
	MaxSessions         int
	MaxSessionsHard     int
	FullRetryAfter      time.Duration
	RelayBusyRetryAfter time.Duration

	Window     media.WindowConfig
	MaxPackets int
	NACKDelay  time.Duration
	GOP        media.GOPConfig
	Backoff    status.BackoffPolicy
	RecordTTL  time.Duration
	MaxRecords int

	Peers         []string
	PeerTemplate  string
	Replicas      int
	InternalToken string
	PeerTimeout   time.Duration

	// TelemetryURL is the R28 ingest endpoint each session reports to
	// (WG11); empty reports nowhere unless the relay advertises one.
	TelemetryURL string

	LogLevel  slog.Level
	LogFormat string
}

func envName(flagName string) string {
	return EnvPrefix + strings.ToUpper(strings.ReplaceAll(flagName, "-", "_"))
}

// onFlagSet lets the carry-all test see every registered flag.
var onFlagSet func(*flag.FlagSet)

var ordinalRE = regexp.MustCompile(`-(\d+)$`)

// Ordinal reads a StatefulSet pod's ordinal from its name's trailing -N
// (gawk-whip-gateway-2 → 2); a name without one is ordinal 0.
func Ordinal(podName string) int {
	m := ordinalRE.FindStringSubmatch(strings.TrimSpace(podName))
	if m == nil {
		return 0
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0
	}
	return n
}

// Parse resolves the configuration from args (without the program name)
// and getenv.
func Parse(args []string, getenv func(string) string) (Config, error) {
	fs := flag.NewFlagSet("gawk-whip-gateway", flag.ContinueOnError)
	str := func(name, def, usage string) *string {
		if v := getenv(envName(name)); v != "" {
			def = v
		}
		return fs.String(name, def, usage+" (env "+envName(name)+")")
	}
	var envErr error
	boolean := func(name, usage string) *bool {
		def := false
		if v := getenv(envName(name)); v != "" {
			b, err := strconv.ParseBool(strings.TrimSpace(v))
			if err != nil && envErr == nil {
				envErr = fmt.Errorf("invalid %s %q (want true or false)", envName(name), v)
			}
			def = b
		}
		return fs.Bool(name, def, usage+" (env "+envName(name)+")")
	}
	dur := func(name string, def time.Duration, usage string) *string {
		return str(name, def.String(), usage)
	}
	listen := str("listen", DefaultListen, "public HTTP listener: POST/DELETE/OPTIONS /whip…, GET/DELETE /whip/status, /healthz (behind the Ingress, which terminates TLS)")
	internalListen := str("internal-listen", DefaultInternalListen, "internal HTTP listener for D10's peer lookup; only other gateway pods may reach it, never route it publicly")
	opsListen := str("ops-listen", DefaultOpsListen, "ops HTTP listener: /healthz, /readyz, /metrics; never route it publicly")
	certFile := str("tls-cert-file", "", "serve the public listener over TLS with this certificate (PEM); empty serves plain HTTP for a TLS-terminating Ingress")
	keyFile := str("tls-key-file", "", "TLS key (PEM) for -tls-cert-file")

	relayURL := str("relay-url", "", "the relay's https URL the gateway publishes to, e.g. https://gawk-server:4433")
	relaySecret := str("relay-publish-secret", "", "the relay's -publish-secret, if it has one; prefer the env")
	relayProxy := str("relay-publish-proxy-token", "", "the relay's -publish-proxy-token (D4): lets the relay's bans and rate limits see the OBS user's IP; prefer the env")
	relayOrigin := str("relay-origin", "", "Origin sent on the publish dial; empty sends gawk-broadcast://native (already in the relay's allow-list convention)")
	relayInsecure := boolean("relay-insecure", "skip TLS verification of the relay (dev certificates only)")
	relayDial := dur("relay-dial-timeout", 10*time.Second, "how long a POST waits for the relay to accept the publish")

	publicAddr := str("public-address", "", "the IP put in every ICE host candidate: the media LoadBalancer's shared address (D10); empty advertises the pod's own addresses")
	mediaBind := str("media-bind", "", "host the media UDP mux and ICE-TCP listener bind; empty binds all")
	basePort := str("media-base-port", strconv.Itoa(DefaultMediaBasePort), "media port of ordinal 0; each pod uses basePort + its ordinal, UDP and ICE-TCP (D10)")
	podName := str("pod-name", "", "this pod's name; its trailing -N is the StatefulSet ordinal (default 0)")
	iceDisc := dur("ice-disconnected-timeout", 5*time.Second, "silence before a session counts as disconnected (provisional until WG0)")
	iceFailed := dur("ice-failed-timeout", 15*time.Second, "silence before a session counts as failed: lost (D8) (provisional until WG0)")
	iceKeep := dur("ice-keepalive", 2*time.Second, "ICE keepalive interval")

	origins := str("allowed-origins", "", "comma-separated Origins allowed to read and clear /whip/status cross-origin (the gawk-app origins, D9); empty allows any (development)")
	trusted := str("trusted-proxy-cidrs", "", "comma-separated CIDRs of the proxy that writes X-Forwarded-For (the ingress controller's pods, or exact node addresses under hostNetwork) (D4); empty uses the peer address only")
	postRate := str("post-rate", strconv.FormatFloat(DefaultPOSTRate, 'g', -1, 64), "POST /whip per client IP per second, sustained; 0 disables the gateway's own limit")
	postBurst := str("post-burst", strconv.Itoa(DefaultPOSTBurst), "POST /whip per client IP at once")
	statusRate := str("status-rate", strconv.FormatFloat(DefaultStatusRate, 'g', -1, 64), "GET/DELETE /whip/status per client IP per second, sustained; 0 disables it")
	statusBurst := str("status-burst", strconv.Itoa(DefaultStatusBurst), "GET/DELETE /whip/status per client IP at once")
	maxSessions := str("max-sessions", strconv.Itoa(DefaultMaxSessions), "sessions per pod for a new key (D10; provisional until WG0)")
	maxHard := str("max-sessions-hard", "0", "hard per-pod ceiling, even for a reconnect whose session is on a peer (D10); 0 is ceil(1.25 × max-sessions)")
	fullRetry := dur("full-retry-after", 30*time.Second, "Retry-After of a 503 at the cap (D8)")
	busyRetry := dur("relay-busy-retry-after", 30*time.Second, "Retry-After passed to OBS when the relay answers 429 or is unreachable")

	defW := media.DefaultWindowConfig()
	initRTT := dur("reorder-initial-rtt", defW.InitialRTT, "RTT assumed before the first NACK → retransmit sample (D6; provisional until WG0)")
	wFloor := dur("reorder-window-floor", defW.Floor, "reorder window floor (D6; provisional until WG0)")
	wCap := dur("reorder-window-cap", defW.Cap, "reorder window cap: no frame waits longer (D6; provisional until WG0)")
	wMult := str("reorder-window-multiplier", strconv.FormatFloat(defW.Multiplier, 'g', -1, 64), "window = multiplier × RTT + offset (D6; provisional until WG0)")
	wOff := dur("reorder-window-offset", defW.Offset, "window = multiplier × RTT + offset (D6; provisional until WG0)")
	nackDelay := dur("reorder-nack-delay", media.DefaultReorderConfig().NACKDelay, "a hole is NACKed only if still missing this long, so a merely reordered packet is never NACKed or RTT-sampled (D6; below the window floor; provisional until WG0)")
	maxPkts := str("reorder-max-packets", "4096", "packets the reorder window may hold before it drops without waiting")

	defG := media.DefaultGOPConfig()
	gopWarn := dur("gop-warn", defG.WarnAbove, "a measured keyframe interval above it warns gop_long (D7, OD5)")
	gopMax := dur("gop-max", defG.EndAbove, "a measured keyframe interval above it ends the session gop_too_long (D7, OD5)")
	gopEarly := dur("gop-early-gap", defG.EarlyGap, "before gop-samples spacings exist, two consecutive gaps above it end the session (D7)")
	gopSamples := str("gop-samples", strconv.Itoa(defG.Samples), "the keyframe interval is the median of this many spacings (D7)")
	boInit := dur("backoff-initial", 30*time.Second, "first backoff after a session the gateway ends itself (D7)")
	boMax := dur("backoff-max", 10*time.Minute, "longest backoff; each repeat doubles up to it (D7)")
	recordTTL := dur("record-ttl", 10*time.Minute, "how long a refusal or an ended session is reported (D9)")
	maxRecords := str("max-records", strconv.Itoa(DefaultMaxRecords), "per-key records kept, least recently used evicted first (D9)")

	peers := str("peers", "", "comma-separated internal base URLs of the other gateway pods (D10), e.g. http://gw-1.gw-peers:8081; exclusive with -peer-template")
	peerTmpl := str("peer-template", "", "internal base URL of every pod with {ordinal} for its ordinal, e.g. http://gawk-whip-gateway-{ordinal}.gawk-whip-gateway-peers:8081; with -replicas, every ordinal but this pod's is a peer")
	replicas := str("replicas", "1", "StatefulSet replicas, for -peer-template")
	internalToken := str("internal-token", "", "shared token authenticating the internal listener (D10); required with peers; prefer the env")
	peerTimeout := dur("peer-timeout", 200*time.Millisecond, "a peer slower than this counts as holding nothing (D10)")

	telemetryURL := str("telemetry-url", "", "R28 ingest endpoint each OBS session's broadcaster telemetry is posted to (WG11), e.g. http://gawk-telemetry/api/telemetry/v1/ingest; empty reports only where the relay advertises an endpoint")
	logLevel := str("log-level", "info", "log level: debug|info|warn|error")
	logFormat := str("log-format", "text", "log format: text|json")
	if onFlagSet != nil {
		onFlagSet(fs)
	}
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	if fs.NArg() > 0 {
		return Config{}, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if envErr != nil {
		return Config{}, envErr
	}

	c := Config{
		Listen:             strings.TrimSpace(*listen),
		InternalListen:     strings.TrimSpace(*internalListen),
		OpsListen:          strings.TrimSpace(*opsListen),
		TLSCertFile:        strings.TrimSpace(*certFile),
		TLSKeyFile:         strings.TrimSpace(*keyFile),
		RelayURL:           strings.TrimRight(strings.TrimSpace(*relayURL), "/"),
		RelayPublishSecret: *relaySecret,
		RelayProxyToken:    strings.TrimSpace(*relayProxy),
		RelayOrigin:        strings.TrimSpace(*relayOrigin),
		RelayInsecure:      *relayInsecure,
		PublicAddress:      strings.TrimSpace(*publicAddr),
		MediaBind:          strings.TrimSpace(*mediaBind),
		PodName:            strings.TrimSpace(*podName),
		PeerTemplate:       strings.TrimSpace(*peerTmpl),
		InternalToken:      strings.TrimSpace(*internalToken),
		LogFormat:          strings.TrimSpace(*logFormat),
		TelemetryURL:       strings.TrimSpace(*telemetryURL),
	}
	c.AllowedOrigins = splitList(*origins)
	c.TrustedProxyCIDRs = splitList(*trusted)
	c.Peers = splitList(*peers)

	var err error
	durs := []struct {
		name string
		v    string
		dst  *time.Duration
		zero bool // zero allowed
	}{
		{"-relay-dial-timeout", *relayDial, &c.RelayDialTimeout, false},
		{"-ice-disconnected-timeout", *iceDisc, &c.ICEDisconnected, false},
		{"-ice-failed-timeout", *iceFailed, &c.ICEFailed, false},
		{"-ice-keepalive", *iceKeep, &c.ICEKeepalive, false},
		{"-full-retry-after", *fullRetry, &c.FullRetryAfter, false},
		{"-relay-busy-retry-after", *busyRetry, &c.RelayBusyRetryAfter, false},
		{"-reorder-initial-rtt", *initRTT, &c.Window.InitialRTT, false},
		{"-reorder-window-floor", *wFloor, &c.Window.Floor, false},
		{"-reorder-window-cap", *wCap, &c.Window.Cap, false},
		{"-reorder-window-offset", *wOff, &c.Window.Offset, true},
		{"-reorder-nack-delay", *nackDelay, &c.NACKDelay, true},
		{"-gop-warn", *gopWarn, &c.GOP.WarnAbove, false},
		{"-gop-max", *gopMax, &c.GOP.EndAbove, false},
		{"-gop-early-gap", *gopEarly, &c.GOP.EarlyGap, false},
		{"-backoff-initial", *boInit, &c.Backoff.Initial, false},
		{"-backoff-max", *boMax, &c.Backoff.Max, false},
		{"-record-ttl", *recordTTL, &c.RecordTTL, false},
		{"-peer-timeout", *peerTimeout, &c.PeerTimeout, false},
	}
	for _, d := range durs {
		v, err := time.ParseDuration(strings.TrimSpace(d.v))
		if err != nil || v < 0 || (v == 0 && !d.zero) {
			return Config{}, fmt.Errorf("invalid %s %q (want a positive duration)", d.name, d.v)
		}
		*d.dst = v
	}
	if c.Window.Multiplier, err = strconv.ParseFloat(strings.TrimSpace(*wMult), 64); err != nil || c.Window.Multiplier <= 0 {
		return Config{}, fmt.Errorf("invalid -reorder-window-multiplier %q", *wMult)
	}
	if c.Window.Floor > c.Window.Cap {
		return Config{}, errors.New("-reorder-window-floor must not exceed -reorder-window-cap")
	}
	if c.NACKDelay >= c.Window.Floor {
		return Config{}, errors.New("-reorder-nack-delay must be below -reorder-window-floor, or a NACK could never be answered within the window")
	}
	if c.GOP.WarnAbove > c.GOP.EndAbove {
		return Config{}, errors.New("-gop-warn must not exceed -gop-max")
	}
	if c.Backoff.Initial > c.Backoff.Max {
		return Config{}, errors.New("-backoff-initial must not exceed -backoff-max")
	}
	if c.POSTRate, err = strconv.ParseFloat(strings.TrimSpace(*postRate), 64); err != nil || c.POSTRate < 0 || math.IsInf(c.POSTRate, 0) {
		return Config{}, fmt.Errorf("invalid -post-rate %q", *postRate)
	}
	if c.StatusRate, err = strconv.ParseFloat(strings.TrimSpace(*statusRate), 64); err != nil || c.StatusRate < 0 || math.IsInf(c.StatusRate, 0) {
		return Config{}, fmt.Errorf("invalid -status-rate %q", *statusRate)
	}
	ints := []struct {
		name string
		v    string
		dst  *int
		min  int
	}{
		{"-post-burst", *postBurst, &c.POSTBurst, 1},
		{"-status-burst", *statusBurst, &c.StatusBurst, 1},
		{"-max-sessions", *maxSessions, &c.MaxSessions, 1},
		{"-max-sessions-hard", *maxHard, &c.MaxSessionsHard, 0},
		{"-media-base-port", *basePort, &c.MediaBasePort, 1},
		{"-reorder-max-packets", *maxPkts, &c.MaxPackets, 64},
		{"-gop-samples", *gopSamples, &c.GOP.Samples, 1},
		{"-max-records", *maxRecords, &c.MaxRecords, 1},
		{"-replicas", *replicas, &c.Replicas, 1},
	}
	for _, i := range ints {
		n, err := strconv.Atoi(strings.TrimSpace(i.v))
		if err != nil || n < i.min {
			return Config{}, fmt.Errorf("invalid %s %q (want an integer ≥ %d)", i.name, i.v, i.min)
		}
		*i.dst = n
	}
	if c.MaxSessionsHard == 0 {
		c.MaxSessionsHard = int(math.Ceil(1.25 * float64(c.MaxSessions)))
	}
	if c.MaxSessionsHard < c.MaxSessions {
		return Config{}, errors.New("-max-sessions-hard must not be below -max-sessions")
	}

	for _, l := range []struct{ name, v string }{{"-listen", c.Listen}, {"-internal-listen", c.InternalListen}, {"-ops-listen", c.OpsListen}} {
		if _, _, err := net.SplitHostPort(l.v); err != nil {
			return Config{}, fmt.Errorf("invalid %s %q: %v", l.name, l.v, err)
		}
	}
	if (c.TLSCertFile == "") != (c.TLSKeyFile == "") {
		return Config{}, errors.New("-tls-cert-file and -tls-key-file go together")
	}
	u, err := url.Parse(c.RelayURL)
	if c.RelayURL == "" || err != nil || u.Scheme != "https" || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.User != nil {
		return Config{}, fmt.Errorf("invalid -relay-url %q (want https://host:port)", c.RelayURL)
	}
	if c.TelemetryURL != "" {
		tu, err := url.Parse(c.TelemetryURL)
		if err != nil || (tu.Scheme != "http" && tu.Scheme != "https") || tu.Host == "" || tu.User != nil {
			return Config{}, fmt.Errorf("invalid -telemetry-url %q (want an http(s) URL)", c.TelemetryURL)
		}
	}
	if c.PublicAddress != "" {
		if _, err := netip.ParseAddr(c.PublicAddress); err != nil {
			return Config{}, fmt.Errorf("invalid -public-address %q (want an IP address)", c.PublicAddress)
		}
	}
	if _, err := clientip.New(c.TrustedProxyCIDRs); err != nil {
		return Config{}, fmt.Errorf("invalid -trusted-proxy-cidrs: %v", err)
	}
	c.Ordinal = Ordinal(c.PodName)
	c.MediaPort = c.MediaBasePort + c.Ordinal
	if c.MediaPort > 65535 {
		return Config{}, fmt.Errorf("-media-base-port %d + ordinal %d is past 65535", c.MediaBasePort, c.Ordinal)
	}
	if len(c.Peers) > 0 && c.PeerTemplate != "" {
		return Config{}, errors.New("-peers and -peer-template are exclusive")
	}
	if c.PeerTemplate != "" {
		if !strings.Contains(c.PeerTemplate, "{ordinal}") {
			return Config{}, fmt.Errorf("invalid -peer-template %q: it must contain {ordinal}", c.PeerTemplate)
		}
		for i := 0; i < c.Replicas; i++ {
			if i != c.Ordinal {
				c.Peers = append(c.Peers, strings.ReplaceAll(c.PeerTemplate, "{ordinal}", strconv.Itoa(i)))
			}
		}
	}
	for _, p := range c.Peers {
		pu, err := url.Parse(p)
		if err != nil || (pu.Scheme != "http" && pu.Scheme != "https") || pu.Host == "" {
			return Config{}, fmt.Errorf("invalid peer URL %q (want http://host:port)", p)
		}
	}
	if len(c.Peers) > 0 && c.InternalToken == "" {
		return Config{}, errors.New("-internal-token (GAWK_WHIP_INTERNAL_TOKEN) is required with peers")
	}
	if c.InternalToken != "" && c.InternalToken == c.RelayProxyToken {
		return Config{}, errors.New("-internal-token must differ from -relay-publish-proxy-token")
	}
	if err := c.LogLevel.UnmarshalText([]byte(strings.TrimSpace(*logLevel))); err != nil {
		return Config{}, fmt.Errorf("invalid -log-level %q", *logLevel)
	}
	if c.LogFormat != "text" && c.LogFormat != "json" {
		return Config{}, fmt.Errorf("invalid -log-format %q (want text or json)", c.LogFormat)
	}
	return c, nil
}

func splitList(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// Options hands every knob to the gateway (the production call site).
func (c Config) Options(version string, log *slog.Logger) gateway.Options {
	ro := media.DefaultReorderConfig()
	ro.Window = c.Window
	ro.MaxPackets = c.MaxPackets
	ro.NACKDelay = c.NACKDelay
	return gateway.Options{
		Relay: gateway.RelayOptions{
			URL:               c.RelayURL,
			PublishSecret:     c.RelayPublishSecret,
			PublishProxyToken: c.RelayProxyToken,
			Origin:            c.RelayOrigin,
			Insecure:          c.RelayInsecure,
			DialTimeout:       c.RelayDialTimeout,
		},
		PublicAddress:          c.PublicAddress,
		MediaBind:              c.MediaBind,
		MediaPort:              c.MediaPort,
		ICEDisconnectedTimeout: c.ICEDisconnected,
		ICEFailedTimeout:       c.ICEFailed,
		ICEKeepalive:           c.ICEKeepalive,
		AllowedOrigins:         c.AllowedOrigins,
		TrustedProxyCIDRs:      c.TrustedProxyCIDRs,
		POSTRate:               c.POSTRate,
		POSTBurst:              c.POSTBurst,
		StatusRate:             c.StatusRate,
		StatusBurst:            c.StatusBurst,
		MaxSessions:            c.MaxSessions,
		MaxSessionsHard:        c.MaxSessionsHard,
		FullRetryAfter:         c.FullRetryAfter,
		RelayBusyRetryAfter:    c.RelayBusyRetryAfter,
		Reorder:                ro,
		GOP:                    c.GOP,
		Backoff:                c.Backoff,
		RecordTTL:              c.RecordTTL,
		MaxRecords:             c.MaxRecords,
		Peers:                  c.Peers,
		InternalToken:          c.InternalToken,
		PeerTimeout:            c.PeerTimeout,
		TelemetryURL:           c.TelemetryURL,
		Version:                version,
		Log:                    log,
	}
}

// LogAttrs is the start-up summary: every knob, secrets as set/unset.
func (c Config) LogAttrs() []any {
	set := func(s string) bool { return s != "" }
	return []any{
		"listen", c.Listen, "internal_listen", c.InternalListen, "ops_listen", c.OpsListen, "tls", set(c.TLSCertFile),
		"relay_url", c.RelayURL, "relay_publish_secret_set", set(c.RelayPublishSecret),
		"relay_publish_proxy_token_set", set(c.RelayProxyToken), "relay_origin", c.RelayOrigin,
		"relay_insecure", c.RelayInsecure, "relay_dial_timeout", c.RelayDialTimeout,
		"public_address", c.PublicAddress, "media_bind", c.MediaBind, "media_port", c.MediaPort, "ordinal", c.Ordinal,
		"ice_disconnected_timeout", c.ICEDisconnected, "ice_failed_timeout", c.ICEFailed, "ice_keepalive", c.ICEKeepalive,
		"allowed_origins", c.AllowedOrigins, "trusted_proxy_cidrs", c.TrustedProxyCIDRs,
		"post_rate", c.POSTRate, "post_burst", c.POSTBurst, "status_rate", c.StatusRate, "status_burst", c.StatusBurst,
		"max_sessions", c.MaxSessions, "max_sessions_hard", c.MaxSessionsHard,
		"full_retry_after", c.FullRetryAfter, "relay_busy_retry_after", c.RelayBusyRetryAfter,
		"reorder_initial_rtt", c.Window.InitialRTT, "reorder_window_floor", c.Window.Floor,
		"reorder_window_cap", c.Window.Cap, "reorder_window_multiplier", c.Window.Multiplier,
		"reorder_window_offset", c.Window.Offset, "reorder_nack_delay", c.NACKDelay, "reorder_max_packets", c.MaxPackets,
		"gop_warn", c.GOP.WarnAbove, "gop_max", c.GOP.EndAbove, "gop_early_gap", c.GOP.EarlyGap, "gop_samples", c.GOP.Samples,
		"backoff_initial", c.Backoff.Initial, "backoff_max", c.Backoff.Max,
		"record_ttl", c.RecordTTL, "max_records", c.MaxRecords,
		"peers", len(c.Peers), "internal_token_set", set(c.InternalToken), "peer_timeout", c.PeerTimeout,
		"telemetry_url", c.TelemetryURL,
	}
}
