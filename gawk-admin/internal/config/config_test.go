package config

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// minimal is the smallest environment that boots a serving process. Tests
// mutate a copy rather than restating the whole set, so a new required knob
// shows up as one edit here instead of twenty.
func minimal() map[string]string {
	return map[string]string{
		"GAWK_ADMIN_EXTERNAL_URL":      "https://admin.example.com",
		"GAWK_ADMIN_OIDC_ISSUER":       "https://idp.example.com/realms/gawk",
		"GAWK_ADMIN_OIDC_CLIENT_ID":    "gawk-admin",
		"GAWK_ADMIN_OIDC_AUDIENCE":     "gawk-admin",
		"GAWK_ADMIN_PG_DSN":            "postgres://u:p@db/gawkadmin",
		"GAWK_ADMIN_RELAY_SCAN_TARGET": "gawk-server-metrics-headless",
		"GAWK_ADMIN_RELAY_ADMIN_TOKEN": "s3cret",
		"POD_NAMESPACE":                "production",
	}
}

func envFrom(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestDefaults(t *testing.T) {
	cfg, err := ParseFlags(nil, envFrom(minimal()))
	if err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if cfg.Mode != ModeServe {
		t.Errorf("Mode = %q, want %q", cfg.Mode, ModeServe)
	}
	if cfg.Addr != ":8090" {
		t.Errorf("Addr = %q, want :8090", cfg.Addr)
	}
	if cfg.RelayOpsPort != 2112 {
		t.Errorf("RelayOpsPort = %d, want 2112", cfg.RelayOpsPort)
	}
	if cfg.KillCooldown != 10*time.Minute {
		t.Errorf("KillCooldown = %v, want 10m", cfg.KillCooldown)
	}
	if cfg.OperatorRole != "operator" || cfg.FlaggerRole != "flagger" {
		t.Errorf("roles = %q/%q, want operator/flagger", cfg.OperatorRole, cfg.FlaggerRole)
	}
	if cfg.Namespace != "production" {
		t.Errorf("Namespace = %q, want production (from POD_NAMESPACE)", cfg.Namespace)
	}
}

// The default roles claim is the Keycloak client-roles path with the client ID
// substituted — the substitution is what makes the default work at all, so it
// is asserted rather than assumed (docs/42 §4.8).
func TestRolesClaimSubstitution(t *testing.T) {
	cfg, err := ParseFlags(nil, envFrom(minimal()))
	if err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if got, want := cfg.RolesClaimPath(), "resource_access.gawk-admin.roles"; got != want {
		t.Errorf("RolesClaimPath() = %q, want %q", got, want)
	}

	cfg, err = ParseFlags([]string{"-oidc-roles-claim", "groups"}, envFrom(minimal()))
	if err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if got := cfg.RolesClaimPath(); got != "groups" {
		t.Errorf("overridden RolesClaimPath() = %q, want groups", got)
	}
}

func TestFlagBeatsEnv(t *testing.T) {
	env := minimal()
	env["GAWK_ADMIN_ADDR"] = ":9999"
	cfg, err := ParseFlags([]string{"-addr", ":1234"}, envFrom(env))
	if err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if cfg.Addr != ":1234" {
		t.Errorf("Addr = %q, want the flag value :1234", cfg.Addr)
	}
}

func TestRequiredKnobs(t *testing.T) {
	for _, key := range []string{
		"GAWK_ADMIN_EXTERNAL_URL",
		"GAWK_ADMIN_OIDC_ISSUER",
		"GAWK_ADMIN_OIDC_CLIENT_ID",
		"GAWK_ADMIN_OIDC_AUDIENCE",
		"GAWK_ADMIN_PG_DSN",
		"GAWK_ADMIN_RELAY_SCAN_TARGET",
		"GAWK_ADMIN_RELAY_ADMIN_TOKEN",
		"POD_NAMESPACE",
	} {
		env := minimal()
		delete(env, key)
		if _, err := ParseFlags(nil, envFrom(env)); err == nil {
			t.Errorf("missing %s: ParseFlags succeeded, want an error", key)
		}
	}
}

// Blanking either authorization knob would authorize every valid token. The
// process must refuse to start instead (docs/42 §9 AP5).
func TestBlankAuthorizationKnobsRefuseToStart(t *testing.T) {
	for _, args := range [][]string{
		{"-oidc-roles-claim", ""},
		{"-operator-role", "  "},
	} {
		if _, err := ParseFlags(args, envFrom(minimal())); err == nil {
			t.Errorf("ParseFlags(%v) succeeded, want a refusal", args)
		}
	}
}

// The migrate subcommand shares the binary but needs only the database: the
// Helm hook Job must not have to carry OIDC configuration it never uses
// (docs/42 §4.15).
func TestMigrateModeNeedsOnlyTheDSN(t *testing.T) {
	cfg, err := ParseFlags([]string{"migrate"}, envFrom(map[string]string{
		"GAWK_ADMIN_PG_DSN": "postgres://u:p@db/gawkadmin",
	}))
	if err != nil {
		t.Fatalf("ParseFlags(migrate): %v", err)
	}
	if cfg.Mode != ModeMigrate {
		t.Errorf("Mode = %q, want %q", cfg.Mode, ModeMigrate)
	}

	if _, err := ParseFlags([]string{"migrate"}, envFrom(map[string]string{})); err == nil {
		t.Error("migrate without -pg-dsn succeeded, want an error")
	}
	if _, err := ParseFlags([]string{"sideways"}, envFrom(minimal())); err == nil {
		t.Error("unknown subcommand succeeded, want an error")
	}
}

func TestStaticWebhooks(t *testing.T) {
	env := minimal()
	env["GAWK_ADMIN_STATIC_WEBHOOKS"] = `[{"name":"ntfy","url":"https://ntfy.example.com/gawk","secretEnv":"NTFY_SECRET"},
	                                      {"name":"parked","url":"https://x.example.com/h","secretEnv":"X_SECRET","enabled":false}]`
	env["NTFY_SECRET"] = "hunter2"
	env["X_SECRET"] = "hunter3"

	cfg, err := ParseFlags(nil, envFrom(env))
	if err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if len(cfg.StaticWebhooks) != 2 {
		t.Fatalf("got %d static webhooks, want 2", len(cfg.StaticWebhooks))
	}
	if cfg.StaticWebhooks[0].Secret != "hunter2" {
		t.Errorf("secret not resolved from its env var: %q", cfg.StaticWebhooks[0].Secret)
	}
	if !cfg.StaticWebhooks[0].IsEnabled() {
		t.Error("webhook with no explicit enabled flag should be enabled")
	}
	if cfg.StaticWebhooks[1].IsEnabled() {
		t.Error(`webhook with "enabled":false should be parked`)
	}

	// The signing key must never be reachable through the value that gets
	// logged or rendered; the config carries only the env var's NAME.
	if strings.Contains(strings.Join(asStrings(cfg.LogAttrs()), " "), "hunter2") {
		t.Error("a webhook signing secret reached the startup log")
	}
}

func TestStaticWebhookRejections(t *testing.T) {
	cases := map[string]string{
		"not JSON":          `nope`,
		"missing name":      `[{"url":"https://x/y","secretEnv":"S"}]`,
		"missing secretEnv": `[{"name":"a","url":"https://x/y"}]`,
		"unset secret env":  `[{"name":"a","url":"https://x/y","secretEnv":"NOT_SET_ANYWHERE"}]`,
		"bad url":           `[{"name":"a","url":"not-a-url","secretEnv":"S"}]`,
		"duplicate names":   `[{"name":"a","url":"https://x/y","secretEnv":"S"},{"name":"a","url":"https://x/z","secretEnv":"S"}]`,
	}
	for name, raw := range cases {
		env := minimal()
		env["GAWK_ADMIN_STATIC_WEBHOOKS"] = raw
		env["S"] = "sekrit"
		if _, err := ParseFlags(nil, envFrom(env)); err == nil {
			t.Errorf("%s: ParseFlags succeeded, want an error", name)
		}
	}
}

// The startup log is the operator's confirmation surface, so it must be
// complete — and must never carry a secret VALUE (CLAUDE.md, docs/42 §4.5).
func TestLogAttrsRedactSecrets(t *testing.T) {
	env := minimal()
	env["GAWK_ADMIN_PG_DSN"] = "postgres://u:SUPERSECRETPW@db/gawkadmin"
	env["GAWK_ADMIN_RELAY_ADMIN_TOKEN"] = "TOKENSENTINEL"
	cfg, err := ParseFlags(nil, envFrom(env))
	if err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	joined := strings.Join(asStrings(cfg.LogAttrs()), " ")
	for _, sentinel := range []string{"SUPERSECRETPW", "TOKENSENTINEL"} {
		if strings.Contains(joined, sentinel) {
			t.Errorf("startup log leaked %q: %s", sentinel, joined)
		}
	}
	for _, want := range []string{"externalUrl", "oidcIssuer", "operatorRole", "killCooldown", "relayScanTarget"} {
		if !strings.Contains(joined, want) {
			t.Errorf("startup log is missing %q: %s", want, joined)
		}
	}
}

func TestInvalidScalars(t *testing.T) {
	for _, args := range [][]string{
		{"-relay-ops-port", "0"},
		{"-relay-ops-port", "banana"},
		{"-kill-cooldown", "-1m"},
		{"-kill-cooldown", "soon"},
		{"-log-level", "loud"},
		{"-log-format", "yaml"},
		{"-external-url", "admin.example.com"},
	} {
		if _, err := ParseFlags(args, envFrom(minimal())); err == nil {
			t.Errorf("ParseFlags(%v) succeeded, want an error", args)
		}
	}
}

func asStrings(attrs []any) []string {
	out := make([]string, 0, len(attrs))
	for _, a := range attrs {
		out = append(out, fmt.Sprint(a))
	}
	return out
}
