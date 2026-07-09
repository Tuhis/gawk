package config

import (
	"log/slog"
	"reflect"
	"testing"
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
		Addr:           ":4433",
		DevCertHosts:   "localhost,127.0.0.1",
		LogLevel:       slog.LevelInfo,
		LogFormat:      "text",
		MaxSubscribers: 15,
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("got %+v, want %+v", cfg, want)
	}
}

func TestEnvFallback(t *testing.T) {
	getenv := envMap(map[string]string{
		"GAWK_ADDR":            ":9999",
		"GAWK_LOG_LEVEL":       "debug",
		"GAWK_LOG_FORMAT":      "json",
		"GAWK_MAX_SUBSCRIBERS": "3",
		"GAWK_DEV_CERT":        "true",
		"GAWK_ALLOWED_ORIGINS": "https://a.example, https://b.example",
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
}

func TestFlagOverridesEnv(t *testing.T) {
	getenv := envMap(map[string]string{
		"GAWK_ADDR":      ":9999",
		"GAWK_LOG_LEVEL": "error",
	})
	cfg, err := ParseFlags([]string{"-addr", ":1234", "-log-level", "warn"}, getenv)
	if err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if cfg.Addr != ":1234" {
		t.Errorf("Addr = %q, want flag value :1234", cfg.Addr)
	}
	if cfg.LogLevel != slog.LevelWarn {
		t.Errorf("LogLevel = %v, want warn from flag", cfg.LogLevel)
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
