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
		Addr:                ":4433",
		DevCertHosts:        "localhost,127.0.0.1",
		LogLevel:            slog.LevelInfo,
		LogFormat:           "text",
		MaxSubscribers:      15,
		MaxIdleTimeout:      30 * time.Second,
		KeepAlivePeriod:     10 * time.Second,
		BroadcastGrace:      5 * time.Minute,
		MaxBroadcasts:       5,
		MaxTotalSubscribers: 50,
		PublishSecret:       "",
		ConnRateLimit:       3.0,
		ConnBurstLimit:      10,
		MaxBandwidthBytes:   0,
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("got %+v, want %+v", cfg, want)
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
		}, noEnv)
		if err != nil {
			t.Fatalf("ParseFlags failed: %v", err)
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

	t.Run("invalid max-bandwidth", func(t *testing.T) {
		if _, err := ParseFlags([]string{"-max-bandwidth", "-5mbps"}, noEnv); err == nil {
			t.Error("expected error for negative bandwidth, got nil")
		}
		if _, err := ParseFlags([]string{"-max-bandwidth", "abc"}, noEnv); err == nil {
			t.Error("expected error for invalid bandwidth unit, got nil")
		}
	})
}
