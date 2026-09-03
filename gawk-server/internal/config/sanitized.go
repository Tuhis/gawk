package config

// The redacted effective-config view served by GET /internal/admin/config
// (R39 AP3, docs/42 §4.5, D10). Read-only by construction: there is no write
// path of any kind — GitOps stays the only mutation channel.
//
// EVERY FIELD IS ENUMERATED BY HAND, deliberately. A reflective "redact any
// field whose name contains 'secret' or 'key'" walk is one rename away from
// publishing a secret, and it silently mis-classifies both directions
// (KeyFile is a path, StatsKey is a key). The cost of the explicit list is
// that a new Config field must be added here too — which is exactly what the
// completeness test in sanitized_test.go enforces.

import (
	"log/slog"
	"net"
	"time"
)

// Redaction placeholders. The resume key additionally names its MODE, because
// "set" is not the interesting question for it — "which of the three key
// sources is this pod actually using" is, and a fleet with per-process keys
// silently breaks cross-pod resume (docs/22 Decision 7).
const (
	secretSetPrefix   = "set"
	secretUnsetPrefix = "unset"
	secretSet         = "<" + secretSetPrefix + ">"
	secretUnset       = "<" + secretUnsetPrefix + ">"
)

// SanitizedConfig is the gawk.admin.config.v1 schema's "config" object.
// Field order here is the order an operator reads it in.
type SanitizedConfig struct {
	// Listeners and identity.
	Addr           string   `json:"addr"`
	MetricsAddr    string   `json:"metricsAddr"`
	ServerName     string   `json:"serverName"`
	ReleaseVersion string   `json:"releaseVersion"`
	AllowedOrigins []string `json:"allowedOrigins"`

	// TLS. Paths, not material — the certificate itself is never read here.
	CertFile     string `json:"certFile"`
	KeyFile      string `json:"keyFile"`
	DevCert      bool   `json:"devCert"`
	DevCertHosts string `json:"devCertHosts"`

	// Logging.
	LogLevel       string `json:"logLevel"`
	LogFormat      string `json:"logFormat"`
	QuietProbeLogs bool   `json:"quietProbeLogs"`

	// Limits.
	MaxSubscribers       int     `json:"maxSubscribers"`
	MaxBroadcasts        int     `json:"maxBroadcasts"`
	MaxTotalSubscribers  int     `json:"maxTotalSubscribers"`
	ConnRateLimit        float64 `json:"connRateLimit"`
	ConnBurstLimit       int     `json:"connBurstLimit"`
	MaxBandwidthBytes    int64   `json:"maxBandwidthBytes"`
	MaxKeyframeBytes     int     `json:"maxKeyframeBytes"`
	KeyframeWriteTimeout string  `json:"keyframeWriteTimeout"`
	MaxIdleTimeout       string  `json:"maxIdleTimeout"`
	KeepAlivePeriod      string  `json:"keepAlivePeriod"`
	BroadcastGrace       string  `json:"broadcastGrace"`

	// Delivery modes.
	DVRWindow                     string  `json:"dvrWindow"`
	DVRMaxBytes                   int     `json:"dvrMaxBytes"`
	DVRMaxCatchup                 float64 `json:"dvrMaxCatchup"`
	DVRAudio                      bool    `json:"dvrAudio"`
	LiveEdgeAudioOnReliableStream bool    `json:"liveEdgeAudioOnReliableStream"`
	ParityDefault                 int     `json:"parityDefault"`
	StripedDelivery               bool    `json:"stripedDelivery"`

	// Federation (R17).
	ClusterMode        bool     `json:"clusterMode"`
	InternalServerName string   `json:"internalServerName"`
	TrustedCIDRs       []string `json:"trustedCidrs"`

	// Telemetry (R28/R37). The key's PRESENCE is the feature switch, so the
	// redacted form still answers "is this fleet collecting at all?".
	TelemetryReportInterval string `json:"telemetryReportInterval"`
	TelemetryAdvertiseURL   string `json:"telemetryAdvertiseUrl"`

	// Rooms (R42, docs/44 §4.10).
	Rooms               bool   `json:"rooms"`
	RoomEmptyGrace      string `json:"roomEmptyGrace"`
	MaxRooms            int    `json:"maxRooms"`
	MaxRoomBroadcasts   int    `json:"maxRoomBroadcasts"`
	MaxRoomParticipants int    `json:"maxRoomParticipants"`
	RoomsFile           string `json:"roomsFile"`

	// Moderation (R39).
	ModerationSource    string `json:"moderationSource"`
	AdminOIDCIssuer     string `json:"adminOidcIssuer"`
	AdminOIDCAudience   string `json:"adminOidcAudience"`
	AdminOIDCRolesClaim string `json:"adminOidcRolesClaim"`
	AdminOIDCRole       string `json:"adminOidcRole"`

	// Secret-bearing fields. Every one of these renders as a placeholder and
	// NEVER as its value — the acceptance gate for this whole type
	// (docs/42 §4.5) is the sentinel test that proves it.
	PublishSecret     string `json:"publishSecret"`
	InternalPSK       string `json:"internalPsk"`
	StatsKey          string `json:"statsKey"`
	StatelessResetKey string `json:"statelessResetKey"`
	TelemetryKey      string `json:"telemetryKey"`
	AdminAPIToken     string `json:"adminApiToken"`
	RoomCreateSecret  string `json:"roomCreateSecret"`
	// ResumeTokenKey names the mode as well as the presence — see the
	// placeholder comment above and ResumeTokenKeyMode.
	ResumeTokenKey string `json:"resumeTokenKey"`
}

// Sanitized returns the redacted view of this configuration.
func (c Config) Sanitized() SanitizedConfig {
	return SanitizedConfig{
		Addr:           c.Addr,
		MetricsAddr:    c.MetricsAddr,
		ServerName:     c.ServerName,
		ReleaseVersion: c.ReleaseVersion,
		AllowedOrigins: append([]string(nil), c.AllowedOrigins...),

		CertFile:     c.CertFile,
		KeyFile:      c.KeyFile,
		DevCert:      c.DevCert,
		DevCertHosts: c.DevCertHosts,

		LogLevel:       c.LogLevel.String(),
		LogFormat:      c.LogFormat,
		QuietProbeLogs: c.QuietProbeLogs,

		MaxSubscribers:       c.MaxSubscribers,
		MaxBroadcasts:        c.MaxBroadcasts,
		MaxTotalSubscribers:  c.MaxTotalSubscribers,
		ConnRateLimit:        c.ConnRateLimit,
		ConnBurstLimit:       c.ConnBurstLimit,
		MaxBandwidthBytes:    c.MaxBandwidthBytes,
		MaxKeyframeBytes:     c.MaxKeyframeBytes,
		KeyframeWriteTimeout: dur(c.KeyframeWriteTimeout),
		MaxIdleTimeout:       dur(c.MaxIdleTimeout),
		KeepAlivePeriod:      dur(c.KeepAlivePeriod),
		BroadcastGrace:       dur(c.BroadcastGrace),

		DVRWindow:                     dur(c.DVRWindow),
		DVRMaxBytes:                   c.DVRMaxBytes,
		DVRMaxCatchup:                 c.DVRMaxCatchup,
		DVRAudio:                      c.DVRAudio,
		LiveEdgeAudioOnReliableStream: c.LiveEdgeAudioOnReliableStream,
		ParityDefault:                 c.ParityDefault,
		StripedDelivery:               c.StripedDelivery,

		ClusterMode:        c.ClusterMode,
		InternalServerName: c.InternalServerName,
		TrustedCIDRs:       cidrStrings(c.TrustedCIDRs),

		TelemetryReportInterval: dur(c.TelemetryReportInterval),
		TelemetryAdvertiseURL:   c.TelemetryAdvertiseURL,

		Rooms:               c.Rooms,
		RoomEmptyGrace:      dur(c.RoomEmptyGrace),
		MaxRooms:            c.MaxRooms,
		MaxRoomBroadcasts:   c.MaxRoomBroadcasts,
		MaxRoomParticipants: c.MaxRoomParticipants,
		RoomsFile:           c.RoomsFile,

		ModerationSource:    c.ModerationSource,
		AdminOIDCIssuer:     c.AdminOIDCIssuer,
		AdminOIDCAudience:   c.AdminOIDCAudience,
		AdminOIDCRolesClaim: c.AdminOIDCRolesClaim,
		AdminOIDCRole:       c.AdminOIDCRole,

		PublishSecret:     setness(c.PublishSecret != ""),
		InternalPSK:       setness(c.InternalPSK != ""),
		StatsKey:          setness(len(c.StatsKey) > 0),
		StatelessResetKey: setness(len(c.StatelessResetKey) > 0),
		TelemetryKey:      setness(len(c.TelemetryKey) > 0),
		AdminAPIToken:     setness(c.AdminAPIToken != ""),
		RoomCreateSecret:  setness(c.RoomCreateSecret != ""),
		ResumeTokenKey:    resumeTokenKeyRedaction(c),
	}
}

// resumeTokenKeyRedaction renders presence AND mode: "<set:explicit-key>",
// "<set:derived-from-publish-secret>" or "<unset:per-process-random>". The
// mode string is byte-identical to the one logStartup prints, which is the
// point — an operator comparing the two is comparing the same words.
func resumeTokenKeyRedaction(c Config) string {
	mode := c.ResumeTokenKeyMode()
	if mode == "per-process-random" {
		return "<" + secretUnsetPrefix + ":" + mode + ">"
	}
	return "<" + secretSetPrefix + ":" + mode + ">"
}

// ResumeTokenKeyMode names where the resume-token key comes from (R17 W2) —
// logged at startup and reported by Sanitized, so a fleet misconfiguration
// (per-process keys on multiple pods, which silently breaks cross-pod resume)
// is visible in both places from ONE definition.
//
// Order mirrors newResumeTokens: the explicit key wins over the
// publish-secret derivation (PR #47 security review — a secret-derived key is
// computable by every broadcaster holding the secret; "explicit-key" is the
// mode that actually closes the graced-ID hijack between broadcasters).
func (c Config) ResumeTokenKeyMode() string {
	switch {
	case len(c.ResumeTokenKey) > 0:
		return "explicit-key"
	case c.PublishSecret != "":
		return "derived-from-publish-secret"
	default:
		return "per-process-random"
	}
}

func setness(set bool) string {
	if set {
		return secretSet
	}
	return secretUnset
}

// dur renders a duration the way an operator wrote it in a flag. The zero
// value stays "0s" rather than becoming empty: "keepalive off" is a real,
// deliberate configuration and must not read as "unset".
func dur(d time.Duration) string { return d.String() }

func cidrStrings(nets []*net.IPNet) []string {
	out := make([]string, 0, len(nets))
	for _, n := range nets {
		if n != nil {
			out = append(out, n.String())
		}
	}
	return out
}

// Compile-time proof that LogLevel is an slog.Level (its String method is
// what Sanitized renders); a type change here would otherwise print a number.
var _ slog.Level = Config{}.LogLevel
