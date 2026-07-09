// Package config holds the server configuration and its flag/env parsing.
//
// Precedence: command-line flag > environment variable > default.
// Every flag has a GAWK_*-prefixed environment fallback so the same binary
// is convenient both on the command line and in a k8s Deployment.
package config

import (
	"flag"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
)

// Config is the fully-resolved server configuration.
type Config struct {
	Addr           string     // UDP listen address, e.g. ":4433"
	CertFile       string     // path to PEM cert; empty in dev-cert mode
	KeyFile        string     // path to PEM key; empty in dev-cert mode
	DevCert        bool       // generate an in-memory ephemeral cert at startup
	DevCertHosts   string     // comma-separated SANs for the dev cert
	LogLevel       slog.Level
	LogFormat      string   // "text" or "json"
	MaxSubscribers int
	AllowedOrigins []string // empty = allow all (dev); checked on CONNECT
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
