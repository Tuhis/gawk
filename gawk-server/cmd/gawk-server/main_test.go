package main

import (
	"bytes"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/hub"
)

// R2 review finding F1: the hardening limits parsed by config.ParseFlags must
// actually reach hub.Options in production. The original R2 change wired them
// only into the transport test helper, so -max-bandwidth was a no-op and
// -max-broadcasts / -max-total-subscribers overrides were silently ignored.
func TestRegistryOptionsCarryAllLimits(t *testing.T) {
	// Every value here is deliberately non-zero. A bool left at its zero value
	// proves nothing about plumbing — it matches whether the assignment exists
	// or not — which is the same blind spot F1 was, one type down.
	cfg := config.Config{
		MaxSubscribers:                7,
		BroadcastGrace:                42 * time.Second,
		MaxBroadcasts:                 9,
		MaxTotalSubscribers:           33,
		MaxBandwidthBytes:             1250000,
		DVRAudio:                      true,
		LiveEdgeAudioOnReliableStream: true,
	}
	want := hub.Options{
		MaxSubscribers:                7,
		BroadcastGrace:                42 * time.Second,
		MaxBroadcasts:                 9,
		MaxTotalSubscribers:           33,
		MaxBandwidthBytes:             1250000,
		DVRAudio:                      true,
		LiveEdgeAudioOnReliableStream: true,
	}
	// DeepEqual, not ==: Options grew func-typed cluster hooks in R17 W3
	// (nil here on both sides — registryOptions never sets them; main wires
	// them separately when -cluster-mode is on).
	if got := registryOptions(cfg); !reflect.DeepEqual(got, want) {
		t.Errorf("registryOptions(cfg) = %+v, want %+v", got, want)
	}
}

// R39 AP2 (docs/42 §9): the startup log must state which ban source this pod
// is enforcing from — the operator's only confirmation surface for a knob
// that otherwise shows up nowhere.
func TestStartupLogStatesTheModerationSource(t *testing.T) {
	for _, source := range []string{"off", "k8s", "file:/etc/gawk/bans.json"} {
		var buf bytes.Buffer
		log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
		logStartup(log, config.Config{ModerationSource: source}, "test")
		if got := buf.String(); !strings.Contains(got, "moderation_source="+source) {
			t.Errorf("startup log for source %q does not state it:\n%s", source, got)
		}
	}
}

// R39 AP3 (docs/42 §4.3 table, §9): none of the admin-API knobs is a
// hub.Options field, so registryOptions never sees them — which makes the
// startup log the ONLY place an operator can confirm what this pod will do
// with /internal/admin/*. The R2 lesson, one knob-shape over: a knob nobody
// can see is a knob nobody notices is inert.
func TestStartupLogStatesTheAdminAPIConfiguration(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	logStartup(log, config.Config{
		AdminAPIToken:       "s3cr3t-token",
		AdminOIDCIssuer:     "https://idp.example/realms/gawk",
		AdminOIDCAudience:   "gawk-admin",
		AdminOIDCRolesClaim: config.DefaultAdminOIDCRolesClaim,
		AdminOIDCRole:       config.DefaultAdminOIDCRole,
	}, "test")
	out := buf.String()

	for _, want := range []string{
		"admin_api_token_set=true",
		"admin_oidc_issuer=https://idp.example/realms/gawk",
		"admin_oidc_audience=gawk-admin",
		"admin_oidc_role=operator",
		"admin_api_enabled=true",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("startup log is missing %q:\n%s", want, out)
		}
	}
	// THE TOKEN ITSELF IS NEVER LOGGED — only whether one is set, which is
	// what decides 404 vs. 401.
	if strings.Contains(out, "s3cr3t-token") {
		t.Fatalf("the admin API token leaked into the startup log:\n%s", out)
	}

	// With no credential the log says so, so "why is /internal/admin 404ing?"
	// is answerable from the pod's own first line.
	buf.Reset()
	logStartup(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})),
		config.Config{}, "test")
	if got := buf.String(); !strings.Contains(got, "admin_api_enabled=false") {
		t.Errorf("startup log does not state that the admin API is disabled:\n%s", got)
	}
}

// The redacted config view and the startup log must agree about the
// resume-token key mode — docs/42 §4.5 says the sanitized value "echoes the
// startup log", and one definition is what makes that true rather than
// aspirational.
func TestResumeTokenKeyModeMatchesTheSanitizedConfig(t *testing.T) {
	for _, cfg := range []config.Config{
		{ResumeTokenKey: make([]byte, 32)},
		{PublishSecret: "hunter2"},
		{},
	} {
		var buf bytes.Buffer
		log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
		logStartup(log, cfg, "test")
		mode := resumeTokenKeyMode(cfg)
		if !strings.Contains(buf.String(), "resume_token_key_mode="+mode) {
			t.Errorf("startup log does not carry mode %q:\n%s", mode, buf.String())
		}
		if got := cfg.Sanitized().ResumeTokenKey; !strings.Contains(got, mode) {
			t.Errorf("sanitized resumeTokenKey %q does not carry the logged mode %q", got, mode)
		}
	}
}
