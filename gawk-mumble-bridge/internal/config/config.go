// Package config holds gawk-mumble-bridge's configuration (docs/80 §11.4).
//
// Precedence: command-line flag > GAWK_BRIDGE_* environment variable >
// default, the contract every gawk binary keeps. Every knob is validated at
// start-up, and Options() hands each one to the object that consumes it; the
// carry-all test walks every flag to that object (CODE-REVIEW.md, the R2
// lesson). Secrets are logged only as whether they are set.
package config

import (
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Tuhis/gawk/gawk-mumble-bridge/internal/bridge"
	"github.com/Tuhis/gawk/gawk-mumble-bridge/internal/mumble"
	"github.com/Tuhis/gawk/gawk-server/voice"
)

// EnvPrefix prefixes every environment twin: -relay-url is
// GAWK_BRIDGE_RELAY_URL.
const EnvPrefix = "GAWK_BRIDGE_"

// Defaults (docs/80 §11.4).
const (
	DefaultListen              = ":4443"
	DefaultInternalListen      = ":4444"
	DefaultOpsListen           = ":2113"
	DefaultNameTemplate        = "{nick} [web]"
	DefaultObserverName        = "gawk"
	DefaultClientVersion       = "1.4.0"
	DefaultMaxVoiceSessions    = 200
	DefaultMaxTempChannels     = 50
	DefaultDrainTimeout        = 10 * time.Second
	DefaultTempChannelTemplate = voice.DefaultTempChannelTemplate
)

// Config is the fully resolved configuration.
type Config struct {
	Listen         string
	InternalListen string
	OpsListen      string
	CertFile       string
	KeyFile        string
	DevCert        bool
	AllowedOrigins []string

	PSK          string
	TicketSecret string

	RelayURL        string
	RelayServerName string
	RelayCAFile     string

	MumbleAddr               string
	MumblePassword           string
	MumbleAccessToken        string
	MumbleCAFile             string
	MumbleCertSHA256         string
	MumbleInsecureSkipVerify bool
	MumbleNameTemplate       string
	MumbleObserverName       string
	MumbleTempParent         string
	MumbleTempTemplate       string
	MumbleClientVersion      mumble.Version

	MaxVoiceSessions int
	MaxTempChannels  int
	DrainTimeout     time.Duration
	// StatsKey keys /statusz and the log's room keys. When -stats-key is
	// unset it is random per process (StatsKeyRandom).
	StatsKey       []byte
	StatsKeyRandom bool

	LogLevel  slog.Level
	LogFormat string
}

// envName is a flag's environment twin.
func envName(flagName string) string {
	return EnvPrefix + strings.ToUpper(strings.ReplaceAll(flagName, "-", "_"))
}

// Parse resolves the configuration from args (without the program name) and
// getenv.
func Parse(args []string, getenv func(string) string) (Config, error) {
	fs := flag.NewFlagSet("gawk-mumble-bridge", flag.ContinueOnError)
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
	listen := str("listen", DefaultListen, "public QUIC listener for browsers' voice sessions (CONNECT /voice)")
	internalListen := str("internal-listen", DefaultInternalListen, "internal QUIC listener for the relay's observer links (CONNECT /internal/observe); never route it publicly")
	opsListen := str("ops-listen", DefaultOpsListen, "ops TCP listener: /healthz, /readyz, /metrics, /statusz; never route it publicly")
	certFile := str("cert-file", "", "TLS certificate (PEM) for both QUIC listeners; reloaded on change")
	keyFile := str("key-file", "", "TLS key (PEM) for -cert-file")
	devCert := boolean("dev-cert", "serve an in-memory self-signed certificate instead of -cert-file (development only)")
	origins := str("allowed-origins", "", "comma-separated Origins allowed to open a voice session (the relay's allowedOrigins); empty allows any (development only)")
	psk := str("psk", "", "pre-shared key gating both internal links (the relay's -voice-bridge-psk); prefer the env")
	secret := str("ticket-secret", "", fmt.Sprintf("HMAC key for voice tickets and the temporary-channel key (the relay's -voice-ticket-secret), at least %d bytes; prefer the env", voice.MinSecretLen))
	relayURL := str("relay-url", "", "the relay's https URL the session links dial, e.g. https://gawk-server:4433")
	relayName := str("relay-server-name", "", "TLS name verified on the session-link dial; default the host of -relay-url")
	relayCA := str("relay-ca-file", "", "extra roots (PEM) for the session-link dial; default the system roots")
	mumbleAddr := str("mumble-addr", "", "murmur's host:port")
	mumblePW := str("mumble-password", "", "murmur's server password, if it has one; prefer the env")
	mumbleToken := str("mumble-access-token", "", "access token every bridged connection sends (ACL group #<token>, D10); prefer the env")
	mumbleCA := str("mumble-ca-file", "", "roots (PEM) to verify murmur's certificate; default the system roots")
	mumblePin := str("mumble-cert-sha256", "", "pin murmur's certificate by the SHA-256 of its DER (hex); replaces chain verification")
	mumbleInsecure := boolean("mumble-insecure-skip-verify", "accept any murmur certificate (lab only)")
	nameTmpl := str("mumble-name-template", DefaultNameTemplate, "a bridged user's Mumble name; {nick} is the gawk nickname (D9)")
	observerName := str("mumble-observer-name", DefaultObserverName, "the observer's Mumble name (OD33)")
	tempParent := str("mumble-temp-parent", "", "path of the channel temporary channels are created under, e.g. Gawk/Rooms; empty is the root")
	tempTmpl := str("mumble-temp-channel-template", DefaultTempChannelTemplate, "a dynamic room's temporary channel name; {key} the keyed channel key, {code} the room code (OD32)")
	clientVersion := str("mumble-client-version", DefaultClientVersion, "the Mumble version the bridge reports; below 1.5.0 so murmur speaks the legacy voice format")
	maxSessions := str("max-voice-sessions", strconv.Itoa(DefaultMaxVoiceSessions), "voice sessions per pod (D23); more are refused with 4016")
	maxTemp := str("max-temp-channels", strconv.Itoa(DefaultMaxTempChannels), "temporary channels this pod's observers hold at once (D23)")
	drain := str("drain-timeout", DefaultDrainTimeout.String(), "how long the SIGTERM drain may take (D19)")
	statsKey := str("stats-key", "", "key for the /statusz and log room hashes; random per process when unset")
	logLevel := str("log-level", "info", "log level: debug|info|warn|error")
	logFormat := str("log-format", "text", "log format: text|json")
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
		CertFile:           strings.TrimSpace(*certFile),
		KeyFile:            strings.TrimSpace(*keyFile),
		PSK:                *psk,
		TicketSecret:       *secret,
		RelayURL:           strings.TrimRight(strings.TrimSpace(*relayURL), "/"),
		RelayServerName:    strings.TrimSpace(*relayName),
		RelayCAFile:        strings.TrimSpace(*relayCA),
		MumbleAddr:         strings.TrimSpace(*mumbleAddr),
		MumblePassword:     *mumblePW,
		MumbleAccessToken:  *mumbleToken,
		MumbleCAFile:       strings.TrimSpace(*mumbleCA),
		MumbleCertSHA256:   strings.TrimSpace(*mumblePin),
		MumbleNameTemplate: *nameTmpl,
		MumbleObserverName: strings.TrimSpace(*observerName),
		MumbleTempParent:   strings.TrimSpace(*tempParent),
		MumbleTempTemplate: *tempTmpl,
		LogFormat:          strings.TrimSpace(*logFormat),
	}
	var err error
	for _, o := range strings.Split(*origins, ",") {
		if o = strings.TrimSpace(o); o != "" {
			c.AllowedOrigins = append(c.AllowedOrigins, o)
		}
	}
	c.DevCert, c.MumbleInsecureSkipVerify = *devCert, *mumbleInsecure
	for _, l := range []struct{ name, v string }{{"-listen", c.Listen}, {"-internal-listen", c.InternalListen}, {"-ops-listen", c.OpsListen}} {
		if _, _, err := net.SplitHostPort(l.v); err != nil {
			return Config{}, fmt.Errorf("invalid %s %q: %v", l.name, l.v, err)
		}
	}
	switch {
	case c.DevCert && (c.CertFile != "" || c.KeyFile != ""):
		return Config{}, errors.New("-dev-cert and -cert-file/-key-file are exclusive")
	case !c.DevCert && (c.CertFile == "" || c.KeyFile == ""):
		return Config{}, errors.New("-cert-file and -key-file are required (or -dev-cert for development)")
	}
	for _, o := range c.AllowedOrigins {
		if o == voice.LinkOrigin {
			return Config{}, fmt.Errorf("-allowed-origins must not list %s: it is the internal links' origin", voice.LinkOrigin)
		}
	}
	if c.PSK == "" {
		return Config{}, errors.New("-psk (GAWK_BRIDGE_PSK) is required")
	}
	if len(c.TicketSecret) < voice.MinSecretLen {
		return Config{}, fmt.Errorf("-ticket-secret (GAWK_BRIDGE_TICKET_SECRET) must be at least %d bytes", voice.MinSecretLen)
	}
	u, err := url.Parse(c.RelayURL)
	if c.RelayURL == "" || err != nil || u.Scheme != "https" || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.User != nil {
		return Config{}, fmt.Errorf("invalid -relay-url %q (want https://host:port)", c.RelayURL)
	}
	if c.RelayServerName == "" {
		c.RelayServerName = u.Hostname()
	}
	if c.MumbleAddr == "" {
		return Config{}, errors.New("-mumble-addr is required")
	}
	if host, port, err := net.SplitHostPort(c.MumbleAddr); err != nil || host == "" || port == "" {
		return Config{}, fmt.Errorf("invalid -mumble-addr %q (want host:port)", c.MumbleAddr)
	}
	trust := 0
	for _, set := range []bool{c.MumbleCAFile != "", c.MumbleCertSHA256 != "", c.MumbleInsecureSkipVerify} {
		if set {
			trust++
		}
	}
	if trust > 1 {
		return Config{}, errors.New("-mumble-ca-file, -mumble-cert-sha256 and -mumble-insecure-skip-verify are exclusive")
	}
	if c.MumbleCertSHA256 != "" {
		if _, err := mumble.ParseCertPin(c.MumbleCertSHA256); err != nil {
			return Config{}, fmt.Errorf("invalid -mumble-cert-sha256: %v", err)
		}
	}
	if !strings.Contains(c.MumbleNameTemplate, "{nick}") {
		return Config{}, fmt.Errorf("invalid -mumble-name-template %q: it must contain {nick}", c.MumbleNameTemplate)
	}
	if c.MumbleObserverName == "" {
		return Config{}, errors.New("-mumble-observer-name must not be empty")
	}
	if strings.TrimSpace(c.MumbleTempTemplate) == "" || !(strings.Contains(c.MumbleTempTemplate, "{key}") || strings.Contains(c.MumbleTempTemplate, "{code}")) {
		return Config{}, fmt.Errorf("invalid -mumble-temp-channel-template %q: it must contain {key} or {code}, or every room would share one channel", c.MumbleTempTemplate)
	}
	if c.MumbleClientVersion, err = mumble.ParseVersion(strings.TrimSpace(*clientVersion)); err != nil {
		return Config{}, fmt.Errorf("invalid -mumble-client-version: %v", err)
	}
	if c.MumbleClientVersion.Less(mumble.MinOpusVersion) || !c.MumbleClientVersion.Less(mumble.LegacyAudioBefore) {
		return Config{}, fmt.Errorf("invalid -mumble-client-version %s: want %s up to below %s (Opus, and the legacy voice format the bridge speaks)",
			c.MumbleClientVersion, mumble.MinOpusVersion, mumble.LegacyAudioBefore)
	}
	if c.MaxVoiceSessions, err = positive("-max-voice-sessions", *maxSessions); err != nil {
		return Config{}, err
	}
	if c.MaxTempChannels, err = nonNegative("-max-temp-channels", *maxTemp); err != nil {
		return Config{}, err
	}
	if c.DrainTimeout, err = time.ParseDuration(strings.TrimSpace(*drain)); err != nil || c.DrainTimeout <= 0 {
		return Config{}, fmt.Errorf("invalid -drain-timeout %q", *drain)
	}
	if *statsKey != "" {
		c.StatsKey = []byte(*statsKey)
	} else {
		c.StatsKey = make([]byte, 32)
		if _, err := rand.Read(c.StatsKey); err != nil {
			return Config{}, err
		}
		c.StatsKeyRandom = true
	}
	if err := c.LogLevel.UnmarshalText([]byte(strings.TrimSpace(*logLevel))); err != nil {
		return Config{}, fmt.Errorf("invalid -log-level %q", *logLevel)
	}
	if c.LogFormat != "text" && c.LogFormat != "json" {
		return Config{}, fmt.Errorf("invalid -log-format %q (want text or json)", c.LogFormat)
	}
	return c, nil
}

func positive(name, v string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 1 {
		return 0, fmt.Errorf("invalid %s %q (want a positive integer)", name, v)
	}
	return n, nil
}

func nonNegative(name, v string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid %s %q (want a non-negative integer)", name, v)
	}
	return n, nil
}

// Options hands every knob to the bridge (the production call site).
func (c Config) Options(version string, log *slog.Logger) bridge.Options {
	return bridge.Options{
		Listen:         c.Listen,
		InternalListen: c.InternalListen,
		OpsListen:      c.OpsListen,
		CertFile:       c.CertFile,
		KeyFile:        c.KeyFile,
		DevCert:        c.DevCert,
		AllowedOrigins: c.AllowedOrigins,
		PSK:            c.PSK,
		TicketSecret:   []byte(c.TicketSecret),
		Relay: bridge.RelayOptions{
			URL:        c.RelayURL,
			ServerName: c.RelayServerName,
			CAFile:     c.RelayCAFile,
		},
		Mumble: bridge.MumbleOptions{
			Addr:        c.MumbleAddr,
			Password:    c.MumblePassword,
			AccessToken: c.MumbleAccessToken,
			TLS: mumble.TLSOptions{
				CAFile:             c.MumbleCAFile,
				CertSHA256:         c.MumbleCertSHA256,
				InsecureSkipVerify: c.MumbleInsecureSkipVerify,
			},
			ClientVersion:       c.MumbleClientVersion,
			NameTemplate:        c.MumbleNameTemplate,
			ObserverName:        c.MumbleObserverName,
			TempParent:          c.MumbleTempParent,
			TempChannelTemplate: c.MumbleTempTemplate,
		},
		MaxVoiceSessions: c.MaxVoiceSessions,
		MaxTempChannels:  c.MaxTempChannels,
		DrainTimeout:     c.DrainTimeout,
		StatsKey:         c.StatsKey,
		Version:          version,
		Logger:           log,
	}
}

// LogAttrs is the start-up summary: every knob, secrets as set/unset.
func (c Config) LogAttrs() []any {
	set := func(s string) bool { return s != "" }
	return []any{
		"listen", c.Listen, "internal_listen", c.InternalListen, "ops_listen", c.OpsListen,
		"cert_file", c.CertFile, "dev_cert", c.DevCert, "allowed_origins", c.AllowedOrigins,
		"psk_set", set(c.PSK), "ticket_secret_set", set(c.TicketSecret),
		"relay_url", c.RelayURL, "relay_server_name", c.RelayServerName, "relay_ca_file", c.RelayCAFile,
		"mumble_addr", c.MumbleAddr, "mumble_password_set", set(c.MumblePassword),
		"mumble_access_token_set", set(c.MumbleAccessToken), "mumble_ca_file", c.MumbleCAFile,
		"mumble_cert_pinned", set(c.MumbleCertSHA256), "mumble_insecure_skip_verify", c.MumbleInsecureSkipVerify,
		"mumble_name_template", c.MumbleNameTemplate, "mumble_observer_name", c.MumbleObserverName,
		"mumble_temp_parent", c.MumbleTempParent, "mumble_temp_channel_template", c.MumbleTempTemplate,
		"mumble_client_version", c.MumbleClientVersion.String(),
		"max_voice_sessions", c.MaxVoiceSessions, "max_temp_channels", c.MaxTempChannels,
		"drain_timeout", c.DrainTimeout, "stats_key_random", c.StatsKeyRandom,
	}
}
