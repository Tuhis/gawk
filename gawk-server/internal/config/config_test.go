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
		Addr:            ":4433",
		DevCertHosts:    "localhost,127.0.0.1",
		LogLevel:        slog.LevelInfo,
		LogFormat:       "text",
		MaxSubscribers:  15,
		MaxIdleTimeout:  30 * time.Second,
		KeepAlivePeriod: 10 * time.Second,
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
}

func TestFlagOverridesEnv(t *testing.T) {
	getenv := envMap(map[string]string{
		"GAWK_ADDR":             ":9999",
		"GAWK_LOG_LEVEL":        "error",
		"GAWK_QUIET_PROBE_LOGS": "true",
	})
	cfg, err := ParseFlags([]string{"-addr", ":1234", "-log-level", "warn", "-quiet-probe-logs=false"}, getenv)
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
