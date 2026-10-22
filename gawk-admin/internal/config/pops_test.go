package config

import (
	"strings"
	"testing"
	"time"
)

// R74 (docs/76 §9): the portal's PoP knobs. Default off, with the documented
// defaults for the rest even while off, so a typo fails the day it is written.
func TestPopKnobDefaults(t *testing.T) {
	cfg, err := ParseFlags(nil, envFrom(minimal()))
	if err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if cfg.Pops {
		t.Error("Pops = true by default, want off")
	}
	if cfg.PopPairingTTL != 72*time.Hour {
		t.Errorf("PopPairingTTL = %v, want 72h", cfg.PopPairingTTL)
	}
	if cfg.PopDNSTTL != 60*time.Second {
		t.Errorf("PopDNSTTL = %v, want 60s", cfg.PopDNSTTL)
	}
	if cfg.PopDNSZone != "" || cfg.PopCertIssuer != "" || cfg.PopHomeEndpoint != "" {
		t.Errorf("zone/issuer/endpoint = %q/%q/%q, want empty", cfg.PopDNSZone, cfg.PopCertIssuer, cfg.PopHomeEndpoint)
	}
	if cfg.PopNamedAddress() {
		t.Error("PopNamedAddress() with no zone and issuer")
	}
}

// Every knob reads its GAWK_ADMIN_* env — the names the chart renders
// (W1-pops' deployment.yaml) — and the flag beats the env.
func TestPopKnobsFromEnvAndFlags(t *testing.T) {
	env := minimal()
	env["GAWK_ADMIN_POPS"] = "true"
	env["GAWK_ADMIN_POP_HOME_ENDPOINT"] = "api.gawk.ioio.fi:4433"
	env["GAWK_ADMIN_POP_PAIRING_TTL"] = "24h"
	env["GAWK_ADMIN_POP_DNS_ZONE"] = "pop.gawk.ioio.fi"
	env["GAWK_ADMIN_POP_CERT_ISSUER"] = "ClusterIssuer/letsencrypt-shortlived"
	env["GAWK_ADMIN_POP_DNS_TTL"] = "30s"
	cfg, err := ParseFlags(nil, envFrom(env))
	if err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if !cfg.Pops || cfg.PopHomeEndpoint != "api.gawk.ioio.fi:4433" || cfg.PopPairingTTL != 24*time.Hour ||
		cfg.PopDNSZone != "pop.gawk.ioio.fi" || cfg.PopCertIssuer != "ClusterIssuer/letsencrypt-shortlived" ||
		cfg.PopDNSTTL != 30*time.Second {
		t.Fatalf("env not applied: %+v", cfg)
	}
	if !cfg.PopNamedAddress() {
		t.Error("PopNamedAddress() false with a zone and an issuer")
	}
	attrs := strings.Join(asStrings(cfg.LogAttrs()), " ")
	for _, want := range []string{"pops", "popHomeEndpoint", "popPairingTtl", "popDnsZone", "popCertIssuer", "popDnsTtl"} {
		if !strings.Contains(attrs, want) {
			t.Errorf("startup log is missing %q: %s", want, attrs)
		}
	}

	cfg, err = ParseFlags([]string{"-pops", "false", "-pop-pairing-ttl", "1h", "-pop-dns-ttl", "2m",
		"-pop-home-endpoint", "relay.example:443"}, envFrom(env))
	if err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if cfg.Pops || cfg.PopPairingTTL != time.Hour || cfg.PopDNSTTL != 2*time.Minute || cfg.PopHomeEndpoint != "relay.example:443" {
		t.Fatalf("flags did not beat the env: %+v", cfg)
	}
}

func TestPopKnobsRefuseWhatCouldNeverWork(t *testing.T) {
	on := func(extra map[string]string) map[string]string {
		env := minimal()
		env["GAWK_ADMIN_POPS"] = "true"
		env["GAWK_ADMIN_POP_HOME_ENDPOINT"] = "api.gawk.ioio.fi:4433"
		for k, v := range extra {
			env[k] = v
		}
		return env
	}
	if _, err := ParseFlags(nil, envFrom(on(nil))); err != nil {
		t.Fatalf("a valid -pops config: %v", err)
	}
	for name, env := range map[string]map[string]string{
		// A pairing string names the endpoint a PoP dials; without one it
		// pairs nothing.
		"pops without an endpoint":  on(map[string]string{"GAWK_ADMIN_POP_HOME_ENDPOINT": ""}),
		"endpoint without a port":   on(map[string]string{"GAWK_ADMIN_POP_HOME_ENDPOINT": "api.gawk.ioio.fi"}),
		"endpoint with a scheme":    on(map[string]string{"GAWK_ADMIN_POP_HOME_ENDPOINT": "https://api.gawk.ioio.fi:4433"}),
		"endpoint with a bad port":  on(map[string]string{"GAWK_ADMIN_POP_HOME_ENDPOINT": "api.gawk.ioio.fi:99999"}),
		"zero pairing ttl":          on(map[string]string{"GAWK_ADMIN_POP_PAIRING_TTL": "0s"}),
		"unparsable pairing ttl":    on(map[string]string{"GAWK_ADMIN_POP_PAIRING_TTL": "soon"}),
		"zero dns ttl":              on(map[string]string{"GAWK_ADMIN_POP_DNS_TTL": "0s"}),
		"unparsable pops":           on(map[string]string{"GAWK_ADMIN_POPS": "maybe"}),
		"zone without an issuer":    on(map[string]string{"GAWK_ADMIN_POP_DNS_ZONE": "pop.example"}),
		"issuer without a zone":     on(map[string]string{"GAWK_ADMIN_POP_CERT_ISSUER": "ClusterIssuer/le"}),
		"issuer without a kind":     on(map[string]string{"GAWK_ADMIN_POP_DNS_ZONE": "pop.example", "GAWK_ADMIN_POP_CERT_ISSUER": "letsencrypt"}),
		"issuer of an unknown kind": on(map[string]string{"GAWK_ADMIN_POP_DNS_ZONE": "pop.example", "GAWK_ADMIN_POP_CERT_ISSUER": "Secret/le"}),
		"zone that is not a name":   on(map[string]string{"GAWK_ADMIN_POP_DNS_ZONE": "pop example", "GAWK_ADMIN_POP_CERT_ISSUER": "Issuer/le"}),
	} {
		if _, err := ParseFlags(nil, envFrom(env)); err == nil {
			t.Errorf("%s: ParseFlags succeeded, want an error", name)
		}
	}
	// With -pops off nothing about the endpoint is required, but a malformed
	// duration is still refused.
	env := minimal()
	if _, err := ParseFlags(nil, envFrom(env)); err != nil {
		t.Fatalf("pops off: %v", err)
	}
	env["GAWK_ADMIN_POP_PAIRING_TTL"] = "soon"
	if _, err := ParseFlags(nil, envFrom(env)); err == nil {
		t.Error("a malformed -pop-pairing-ttl with -pops off was accepted")
	}
}
