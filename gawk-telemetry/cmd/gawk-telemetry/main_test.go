package main

import (
	"strings"
	"testing"
	"time"
)

func noEnv(string) string { return "" }

func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

const key64 = "5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a"

// Without the fleet key the service could only reject everything or accept
// anything. Refusing to start is the only honest third option.
func TestRefusesToStartWithoutTheFleetKey(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"-telemetry-key", ""},
		{"-telemetry-key", "tooshort"},
		{"-telemetry-key", "zz" + key64[2:]},
	} {
		if _, err := parseFlags(args, noEnv); err == nil {
			t.Errorf("parseFlags(%v) succeeded without a usable key", args)
		}
	}
}

func TestDefaults(t *testing.T) {
	c, err := parseFlags([]string{"-telemetry-key", key64}, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	// The two listeners are separate by default: only ingest is ever routed
	// publicly (D1/D14).
	if c.ingestAddr == c.readAddr {
		t.Error("ingest and read share a listener; the read side must be separately exposable")
	}
	if c.retentionDays != 14 {
		t.Errorf("retentionDays = %d, want 14", c.retentionDays)
	}
	if c.scrapeInterval != 5*time.Second {
		t.Errorf("scrapeInterval = %v, want 5s", c.scrapeInterval)
	}
	// D11: the SQL passthrough is a deliberate act, never a default.
	if c.enableSQL {
		t.Error("query_sql enabled by default")
	}
	// No auth by default — cluster-internal is R9 D1's posture.
	if c.basicAuthUser != "" {
		t.Error("read listener has auth by default")
	}
	if !c.mcpEnabled {
		t.Error("MCP off by default; it is the item's primary read surface")
	}
}

func TestEnvFallbackAndFlagPrecedence(t *testing.T) {
	env := envMap(map[string]string{
		"GAWK_TELEMETRY_KEY":            key64,
		"GAWK_TELEMETRY_INGEST_ADDR":    ":9000",
		"GAWK_TELEMETRY_RETENTION_DAYS": "30",
		"GAWK_TELEMETRY_QUERY_SQL":      "true",
		"GAWK_TELEMETRY_RELAY_HEADLESS": "gawk-server-metrics-headless.gawk.svc",
	})
	c, err := parseFlags(nil, env)
	if err != nil {
		t.Fatal(err)
	}
	if c.ingestAddr != ":9000" || c.retentionDays != 30 || !c.enableSQL {
		t.Errorf("env not applied: %+v", c)
	}
	if !strings.Contains(c.relayTargetDescription(), "headless") {
		t.Errorf("relay target = %q, want the headless Service", c.relayTargetDescription())
	}

	c, err = parseFlags([]string{"-ingest-addr", ":7000"}, env)
	if err != nil {
		t.Fatal(err)
	}
	if c.ingestAddr != ":7000" {
		t.Errorf("ingestAddr = %q, want the flag to win over env", c.ingestAddr)
	}
}

func TestRejectsInvalidDurationsAndRetention(t *testing.T) {
	for _, args := range [][]string{
		{"-telemetry-key", key64, "-scrape-interval", "nonsense"},
		{"-telemetry-key", key64, "-scrape-interval", "0s"},
		{"-telemetry-key", key64, "-session-idle", "-1m"},
		{"-telemetry-key", key64, "-retention-days", "0"},
	} {
		if _, err := parseFlags(args, noEnv); err == nil {
			t.Errorf("parseFlags(%v) accepted an invalid value", args)
		}
	}
}

// With no relay configured the service still runs — client-only telemetry —
// and says so rather than pretending it has a relay view.
func TestNoRelayConfiguredIsAValidMode(t *testing.T) {
	c, err := parseFlags([]string{"-telemetry-key", key64}, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(c.relayTargetDescription(), "client-only") {
		t.Errorf("relay target = %q, want it to name the client-only mode", c.relayTargetDescription())
	}
	addrs, err := c.resolver()(nil)
	if err != nil || len(addrs) != 0 {
		t.Errorf("resolver = %v / %v, want an empty target list", addrs, err)
	}
}

func TestStaticRelayAddrsOverrideTheHeadlessService(t *testing.T) {
	c, err := parseFlags([]string{
		"-telemetry-key", key64,
		"-relay-headless-service", "headless.svc",
		"-relay-addrs", "10.0.0.1:2112, 10.0.0.2:2112",
	}, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	addrs, err := c.resolver()(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(addrs) != 2 || addrs[0] != "10.0.0.1:2112" {
		t.Errorf("addrs = %v", addrs)
	}
}
