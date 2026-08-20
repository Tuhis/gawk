package config

import (
	"log/slog"
	"reflect"
	"testing"
	"time"
)

func noEnv(string) string { return "" }

func envMap(m map[string]string) func(string) string {
	return func(key string) string { return m[key] }
}

func TestDefaults(t *testing.T) {
	cfg, err := ParseFlags(nil, noEnv)
	if err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	want := Config{
		Addr:                 ":4433",
		DevCertHosts:         "localhost,127.0.0.1",
		LogLevel:             slog.LevelInfo,
		LogFormat:            "text",
		MaxSubscribers:       15,
		MaxIdleTimeout:       30 * time.Second,
		KeepAlivePeriod:      10 * time.Second,
		BroadcastGrace:       5 * time.Minute,
		MaxBroadcasts:        5,
		MaxTotalSubscribers:  50,
		PublishSecret:        "",
		ConnRateLimit:        3.0,
		ConnBurstLimit:       10,
		MaxBandwidthBytes:    0,
		MaxKeyframeBytes:     8388608,
		KeyframeWriteTimeout: time.Second,
		DVRWindow:            3 * time.Second,
		DVRMaxBytes:          24 << 20,
		DVRMaxCatchup:        4,
		DVRAudio:             true,
		// R29 (docs/34 §5.2): quality-first default, chart-overridable.
		ParityDefault: 2,
		// R30 (docs/35 §6): on by default — zero relay cost until a viewer
		// engages, and off is the byte-identical escape hatch.
		StripedDelivery: true,
		MetricsAddr:     ":2112",
		// R28: telemetry is off by default (no key), but the cadence it would
		// ask for still has a default so enabling it is one value, not two.
		TelemetryReportInterval: 2 * time.Second,
		// R39 (docs/42 §4.3): moderation is off unless an operator names a
		// source, so a relay predating R39 and a relay with the flag unset
		// behave identically.
		ModerationSource: "off",
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("got %+v, want %+v", cfg, want)
	}
}

func TestMetricsAddr(t *testing.T) {
	// Env fallback.
	cfg, err := ParseFlags(nil, envMap(map[string]string{"GAWK_METRICS_ADDR": ":9999"}))
	if err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if cfg.MetricsAddr != ":9999" {
		t.Errorf("MetricsAddr = %q, want :9999", cfg.MetricsAddr)
	}

	// Flag beats env.
	cfg, err = ParseFlags([]string{"-metrics-addr", ":7000"}, envMap(map[string]string{"GAWK_METRICS_ADDR": ":9999"}))
	if err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if cfg.MetricsAddr != ":7000" {
		t.Errorf("MetricsAddr = %q, want :7000", cfg.MetricsAddr)
	}

	// "off" disables (empty string can't travel through an env var — it
	// reads as unset and falls back to the default).
	for _, v := range []string{"off", "OFF", " off "} {
		cfg, err = ParseFlags([]string{"-metrics-addr", v}, noEnv)
		if err != nil {
			t.Fatalf("ParseFlags(%q): %v", v, err)
		}
		if cfg.MetricsAddr != "" {
			t.Errorf("MetricsAddr(%q) = %q, want empty (disabled)", v, cfg.MetricsAddr)
		}
	}
}

func TestEnvFallback(t *testing.T) {
	getenv := envMap(map[string]string{
		"GAWK_ADDR":             ":9999",
		"GAWK_LOG_LEVEL":        "debug",
		"GAWK_LOG_FORMAT":       "json",
		"GAWK_MAX_SUBSCRIBERS":  "3",
		"GAWK_DEV_CERT":         "true",
		"GAWK_ALLOWED_ORIGINS":  "https://a.example, https://b.example",
		"GAWK_MAX_IDLE_TIMEOUT": "45s",
		"GAWK_KEEPALIVE_PERIOD": "5s",
		"GAWK_QUIET_PROBE_LOGS": "true",
		"GAWK_BROADCAST_GRACE":  "2m",
	})
	cfg, err := ParseFlags(nil, getenv)
	if err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if cfg.Addr != ":9999" {
		t.Errorf("Addr = %q, want :9999", cfg.Addr)
	}
	if cfg.LogLevel != slog.LevelDebug {
		t.Errorf("LogLevel = %v, want debug", cfg.LogLevel)
	}
	if cfg.LogFormat != "json" {
		t.Errorf("LogFormat = %q, want json", cfg.LogFormat)
	}
	if cfg.MaxSubscribers != 3 {
		t.Errorf("MaxSubscribers = %d, want 3", cfg.MaxSubscribers)
	}
	if !cfg.DevCert {
		t.Error("DevCert = false, want true")
	}
	wantOrigins := []string{"https://a.example", "https://b.example"}
	if !reflect.DeepEqual(cfg.AllowedOrigins, wantOrigins) {
		t.Errorf("AllowedOrigins = %v, want %v", cfg.AllowedOrigins, wantOrigins)
	}
	if cfg.MaxIdleTimeout != 45*time.Second {
		t.Errorf("MaxIdleTimeout = %v, want 45s", cfg.MaxIdleTimeout)
	}
	if cfg.KeepAlivePeriod != 5*time.Second {
		t.Errorf("KeepAlivePeriod = %v, want 5s", cfg.KeepAlivePeriod)
	}
	if !cfg.QuietProbeLogs {
		t.Error("QuietProbeLogs = false, want true")
	}
	if cfg.BroadcastGrace != 2*time.Minute {
		t.Errorf("BroadcastGrace = %v, want 2m", cfg.BroadcastGrace)
	}
}

func TestFlagOverridesEnv(t *testing.T) {
	getenv := envMap(map[string]string{
		"GAWK_ADDR":             ":9999",
		"GAWK_LOG_LEVEL":        "error",
		"GAWK_QUIET_PROBE_LOGS": "true",
	})
	cfg, err := ParseFlags([]string{"-addr", ":1234", "-log-level", "warn", "-quiet-probe-logs=false", "-broadcast-grace", "10s"}, getenv)
	if err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if cfg.Addr != ":1234" {
		t.Errorf("Addr = %q, want flag value :1234", cfg.Addr)
	}
	if cfg.LogLevel != slog.LevelWarn {
		t.Errorf("LogLevel = %v, want warn from flag", cfg.LogLevel)
	}
	if cfg.QuietProbeLogs {
		t.Error("QuietProbeLogs = true, want false from flag override")
	}
	if cfg.BroadcastGrace != 10*time.Second {
		t.Errorf("BroadcastGrace = %v, want 10s from flag override", cfg.BroadcastGrace)
	}
}

func TestInvalidLogLevel(t *testing.T) {
	if _, err := ParseFlags([]string{"-log-level", "verbose"}, noEnv); err == nil {
		t.Error("expected error for invalid log level, got nil")
	}
}

func TestInvalidLogFormat(t *testing.T) {
	if _, err := ParseFlags([]string{"-log-format", "xml"}, noEnv); err == nil {
		t.Error("expected error for invalid log format, got nil")
	}
}

func TestInvalidMaxSubscribers(t *testing.T) {
	for _, v := range []string{"0", "-1", "abc"} {
		if _, err := ParseFlags([]string{"-max-subscribers", v}, noEnv); err == nil {
			t.Errorf("expected error for max-subscribers=%q, got nil", v)
		}
	}
}

func TestInvalidTimeouts(t *testing.T) {
	bad := [][]string{
		{"-max-idle-timeout", "soon"},
		{"-max-idle-timeout", "0s"},
		{"-max-idle-timeout", "-5s"},
		{"-keepalive-period", "often"},
		{"-keepalive-period", "-1s"},
		{"-keepalive-period", "30s"},                             // == default idle timeout
		{"-max-idle-timeout", "10s", "-keepalive-period", "15s"}, // keepalive > idle
		{"-broadcast-grace", "later"},
		{"-broadcast-grace", "0s"},
		{"-broadcast-grace", "-5s"},
	}
	for _, args := range bad {
		if _, err := ParseFlags(args, noEnv); err == nil {
			t.Errorf("expected error for %v, got nil", args)
		}
	}
	cfg, err := ParseFlags([]string{"-keepalive-period", "0"}, noEnv)
	if err != nil {
		t.Fatalf("keepalive-period 0 should disable keepalive, got error: %v", err)
	}
	if cfg.KeepAlivePeriod != 0 {
		t.Errorf("KeepAlivePeriod = %v, want 0 (disabled)", cfg.KeepAlivePeriod)
	}
}

func TestCertKeyMustBePaired(t *testing.T) {
	if _, err := ParseFlags([]string{"-cert-file", "/tls/tls.crt"}, noEnv); err == nil {
		t.Error("expected error for cert-file without key-file, got nil")
	}
	if _, err := ParseFlags([]string{"-key-file", "/tls/tls.key"}, noEnv); err == nil {
		t.Error("expected error for key-file without cert-file, got nil")
	}
	if _, err := ParseFlags([]string{"-cert-file", "/tls/tls.crt", "-key-file", "/tls/tls.key"}, noEnv); err != nil {
		t.Errorf("expected paired cert/key to parse, got %v", err)
	}
}

func TestHardeningConfig(t *testing.T) {
	t.Run("valid custom values", func(t *testing.T) {
		cfg, err := ParseFlags([]string{
			"-max-broadcasts", "10",
			"-max-total-subscribers", "100",
			"-publish-secret", "supersecret",
			"-conn-rate-limit", "5.5",
			"-conn-burst-limit", "20",
			"-max-bandwidth", "10mbps",
			"-max-keyframe-bytes", "2097152",
			"-keyframe-write-timeout", "2s",
		}, noEnv)
		if err != nil {
			t.Fatalf("ParseFlags failed: %v", err)
		}
		if cfg.MaxKeyframeBytes != 2097152 {
			t.Errorf("MaxKeyframeBytes = %d, want 2097152", cfg.MaxKeyframeBytes)
		}
		if cfg.KeyframeWriteTimeout != 2*time.Second {
			t.Errorf("KeyframeWriteTimeout = %v, want 2s", cfg.KeyframeWriteTimeout)
		}
		if cfg.MaxBroadcasts != 10 {
			t.Errorf("MaxBroadcasts = %d, want 10", cfg.MaxBroadcasts)
		}
		if cfg.MaxTotalSubscribers != 100 {
			t.Errorf("MaxTotalSubscribers = %d, want 100", cfg.MaxTotalSubscribers)
		}
		if cfg.PublishSecret != "supersecret" {
			t.Errorf("PublishSecret = %q, want supersecret", cfg.PublishSecret)
		}
		if cfg.ConnRateLimit != 5.5 {
			t.Errorf("ConnRateLimit = %f, want 5.5", cfg.ConnRateLimit)
		}
		if cfg.ConnBurstLimit != 20 {
			t.Errorf("ConnBurstLimit = %d, want 20", cfg.ConnBurstLimit)
		}
		// 10mbps = 10 * 1,000,000 / 8 = 1,250,000 bytes/sec
		if cfg.MaxBandwidthBytes != 1250000 {
			t.Errorf("MaxBandwidthBytes = %d, want 1250000", cfg.MaxBandwidthBytes)
		}
	})

	t.Run("invalid max-broadcasts", func(t *testing.T) {
		if _, err := ParseFlags([]string{"-max-broadcasts", "0"}, noEnv); err == nil {
			t.Error("expected error for max-broadcasts 0, got nil")
		}
		if _, err := ParseFlags([]string{"-max-broadcasts", "-1"}, noEnv); err == nil {
			t.Error("expected error for max-broadcasts -1, got nil")
		}
	})

	t.Run("invalid max-total-subscribers", func(t *testing.T) {
		if _, err := ParseFlags([]string{"-max-total-subscribers", "0"}, noEnv); err == nil {
			t.Error("expected error for max-total-subscribers 0, got nil")
		}
	})

	t.Run("invalid conn-rate-limit", func(t *testing.T) {
		if _, err := ParseFlags([]string{"-conn-rate-limit", "-0.1"}, noEnv); err == nil {
			t.Error("expected error for conn-rate-limit -0.1, got nil")
		}
	})

	t.Run("invalid conn-burst-limit", func(t *testing.T) {
		if _, err := ParseFlags([]string{"-conn-burst-limit", "0"}, noEnv); err == nil {
			t.Error("expected error for conn-burst-limit 0, got nil")
		}
	})

	t.Run("invalid max-keyframe-bytes", func(t *testing.T) {
		if _, err := ParseFlags([]string{"-max-keyframe-bytes", "0"}, noEnv); err == nil {
			t.Error("expected error for max-keyframe-bytes 0, got nil")
		}
		if _, err := ParseFlags([]string{"-max-keyframe-bytes", "abc"}, noEnv); err == nil {
			t.Error("expected error for non-integer max-keyframe-bytes, got nil")
		}
	})

	t.Run("invalid keyframe-write-timeout", func(t *testing.T) {
		if _, err := ParseFlags([]string{"-keyframe-write-timeout", "0"}, noEnv); err == nil {
			t.Error("expected error for keyframe-write-timeout 0, got nil")
		}
		if _, err := ParseFlags([]string{"-keyframe-write-timeout", "nope"}, noEnv); err == nil {
			t.Error("expected error for invalid keyframe-write-timeout, got nil")
		}
	})

	t.Run("invalid max-bandwidth", func(t *testing.T) {
		if _, err := ParseFlags([]string{"-max-bandwidth", "-5mbps"}, noEnv); err == nil {
			t.Error("expected error for negative bandwidth, got nil")
		}
		if _, err := ParseFlags([]string{"-max-bandwidth", "abc"}, noEnv); err == nil {
			t.Error("expected error for invalid bandwidth unit, got nil")
		}
	})
}

// R17 W1: the shared QUIC stateless reset key — empty disables, anything
// else must be exactly 32 bytes of hex (docs/22 Decision 3).
func TestStatelessResetKey(t *testing.T) {
	key64 := "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"

	cfg, err := ParseFlags([]string{"-stateless-reset-key", key64}, noEnv)
	if err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if len(cfg.StatelessResetKey) != 32 || cfg.StatelessResetKey[1] != 0x01 {
		t.Errorf("StatelessResetKey = %x, want decoded 32 bytes", cfg.StatelessResetKey)
	}

	cfg, err = ParseFlags(nil, envMap(map[string]string{"GAWK_STATELESS_RESET_KEY": key64}))
	if err != nil {
		t.Fatalf("ParseFlags env: %v", err)
	}
	if len(cfg.StatelessResetKey) != 32 {
		t.Errorf("env StatelessResetKey = %x, want 32 bytes", cfg.StatelessResetKey)
	}

	cfg, err = ParseFlags(nil, noEnv)
	if err != nil {
		t.Fatalf("ParseFlags default: %v", err)
	}
	if cfg.StatelessResetKey != nil {
		t.Errorf("default StatelessResetKey = %x, want nil (disabled)", cfg.StatelessResetKey)
	}

	for _, bad := range []string{"zz", "0102", key64 + "00"} {
		if _, err := ParseFlags([]string{"-stateless-reset-key", bad}, noEnv); err == nil {
			t.Errorf("ParseFlags accepted invalid stateless-reset-key %q", bad)
		}
	}
}

// R17 W3: -cluster-mode (default off ⇒ single-pod behavior byte-identical).
func TestClusterModeFlag(t *testing.T) {
	cfg, err := ParseFlags(nil, noEnv)
	if err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if cfg.ClusterMode {
		t.Error("ClusterMode default = true, want false")
	}
	clusterArgs := []string{"-internal-psk", "fleet-secret", "-internal-server-name", "relay.example.com"}
	cfg, err = ParseFlags(append([]string{"-cluster-mode"}, clusterArgs...), noEnv)
	if err != nil {
		t.Fatalf("ParseFlags flag: %v", err)
	}
	if !cfg.ClusterMode || cfg.InternalPSK != "fleet-secret" || cfg.InternalServerName != "relay.example.com" {
		t.Errorf("cluster flags not applied: %+v", cfg)
	}
	cfg, err = ParseFlags(clusterArgs, envMap(map[string]string{"GAWK_CLUSTER_MODE": "true"}))
	if err != nil {
		t.Fatalf("ParseFlags env: %v", err)
	}
	if !cfg.ClusterMode {
		t.Error("GAWK_CLUSTER_MODE=true not applied")
	}

	// Cluster mode without the internal PSK / server name is a startup
	// error, not a silently insecure fleet (W4, docs/22 Decision 9).
	if _, err := ParseFlags([]string{"-cluster-mode"}, noEnv); err == nil {
		t.Error("cluster-mode without internal-psk/server-name accepted")
	}
	if _, err := ParseFlags([]string{"-cluster-mode", "-internal-psk", "x"}, noEnv); err == nil {
		t.Error("cluster-mode without internal-server-name accepted")
	}
}

// R28 TM1: the fleet telemetry key. Its presence IS the feature switch
// (docs/33 D12) — absent, the relay mints no session tokens and sends no
// hello, so every client collects nothing.
func TestTelemetryKey(t *testing.T) {
	key64 := "5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a"

	cfg, err := ParseFlags([]string{"-telemetry-key", key64}, noEnv)
	if err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if len(cfg.TelemetryKey) != 32 || cfg.TelemetryKey[0] != 0x5a {
		t.Errorf("TelemetryKey = %x, want decoded 32 bytes", cfg.TelemetryKey)
	}

	cfg, err = ParseFlags(nil, envMap(map[string]string{"GAWK_TELEMETRY_KEY": key64}))
	if err != nil {
		t.Fatalf("ParseFlags env: %v", err)
	}
	if len(cfg.TelemetryKey) != 32 {
		t.Errorf("env TelemetryKey = %x, want 32 bytes", cfg.TelemetryKey)
	}

	cfg, err = ParseFlags(nil, noEnv)
	if err != nil {
		t.Fatalf("ParseFlags default: %v", err)
	}
	if cfg.TelemetryKey != nil {
		t.Errorf("default TelemetryKey = %x, want nil (telemetry disabled)", cfg.TelemetryKey)
	}

	// A short key is a misconfiguration, not a weaker mode.
	for _, bad := range []string{"zz", "0102", key64 + "00"} {
		if _, err := ParseFlags([]string{"-telemetry-key", bad}, noEnv); err == nil {
			t.Errorf("ParseFlags accepted invalid telemetry-key %q", bad)
		}
	}
}

// The reporting cadence the relay asks clients to use. Bounded because it
// rides a uint16 of milliseconds and a client cannot report faster than its
// own stats tick.
func TestTelemetryReportInterval(t *testing.T) {
	cfg, err := ParseFlags([]string{"-telemetry-report-interval", "5s"}, noEnv)
	if err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if cfg.TelemetryReportInterval != 5*time.Second {
		t.Errorf("TelemetryReportInterval = %v, want 5s", cfg.TelemetryReportInterval)
	}

	cfg, err = ParseFlags(nil, envMap(map[string]string{"GAWK_TELEMETRY_REPORT_INTERVAL": "10s"}))
	if err != nil {
		t.Fatalf("ParseFlags env: %v", err)
	}
	if cfg.TelemetryReportInterval != 10*time.Second {
		t.Errorf("env TelemetryReportInterval = %v, want 10s", cfg.TelemetryReportInterval)
	}

	for _, bad := range []string{"nonsense", "100ms", "5m", "0s", "-2s"} {
		if _, err := ParseFlags([]string{"-telemetry-report-interval", bad}, noEnv); err == nil {
			t.Errorf("ParseFlags accepted out-of-range telemetry-report-interval %q", bad)
		}
	}
}

// R39 AP2 (docs/42 §4.3, §9): -moderation-source parses all three forms,
// honours the env fallback and the flag-over-env precedence, and rejects
// anything else at startup rather than silently enforcing nothing.
func TestModerationSource(t *testing.T) {
	valid := []struct {
		args []string
		env  map[string]string
		want string
	}{
		{nil, nil, "off"},
		{[]string{"-moderation-source", "off"}, nil, "off"},
		{[]string{"-moderation-source", "k8s"}, nil, "k8s"},
		{[]string{"-moderation-source", "file:/etc/gawk/bans.json"}, nil, "file:/etc/gawk/bans.json"},
		// Env fallback.
		{nil, map[string]string{"GAWK_MODERATION_SOURCE": "k8s"}, "k8s"},
		{nil, map[string]string{"GAWK_MODERATION_SOURCE": "file:/tmp/bans.json"}, "file:/tmp/bans.json"},
		// Flag wins over env.
		{[]string{"-moderation-source", "off"}, map[string]string{"GAWK_MODERATION_SOURCE": "k8s"}, "off"},
		// Surrounding whitespace is trimmed, not rejected.
		{[]string{"-moderation-source", "  k8s  "}, nil, "k8s"},
	}
	for _, tt := range valid {
		getenv := noEnv
		if tt.env != nil {
			getenv = envMap(tt.env)
		}
		cfg, err := ParseFlags(tt.args, getenv)
		if err != nil {
			t.Errorf("ParseFlags(%v, %v): %v", tt.args, tt.env, err)
			continue
		}
		if cfg.ModerationSource != tt.want {
			t.Errorf("ParseFlags(%v, %v).ModerationSource = %q, want %q",
				tt.args, tt.env, cfg.ModerationSource, tt.want)
		}
	}

	invalid := []string{
		"postgres",    // not a known kind
		"file",        // missing the colon and the path
		"file:",       // missing the path
		"file:   ",    // whitespace-only path
		"k8s:default", // k8s takes no argument
		"off:",
	}
	for _, v := range invalid {
		if _, err := ParseFlags([]string{"-moderation-source", v}, noEnv); err == nil {
			t.Errorf("ParseFlags(-moderation-source %q) succeeded, want an error", v)
		}
	}
}
