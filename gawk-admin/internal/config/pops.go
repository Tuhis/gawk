package config

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"
)

// PopNamedAddress reports whether the named tier is configured (docs/76
// §6.11): -pops with a zone and an issuer. The wizard offers "named address"
// only then.
func (c Config) PopNamedAddress() bool {
	return c.Pops && c.PopDNSZone != "" && c.PopCertIssuer != ""
}

// parsePops resolves the R74 knobs. The durations and the zone/issuer pair
// are checked even with -pops off, so a typo in the chart fails the day it
// is written; the endpoint is required only with -pops.
func parsePops(cfg *Config, on, endpoint, pairingTTL, zone, issuer, dnsTTL string) error {
	pops, err := strconv.ParseBool(strings.TrimSpace(on))
	if err != nil {
		return fmt.Errorf("invalid -pops %q (want true|false)", on)
	}
	cfg.Pops = pops

	ttl, err := time.ParseDuration(strings.TrimSpace(pairingTTL))
	if err != nil || ttl <= 0 {
		return fmt.Errorf("invalid -pop-pairing-ttl %q (want a positive duration, e.g. 72h)", pairingTTL)
	}
	cfg.PopPairingTTL = ttl
	dns, err := time.ParseDuration(strings.TrimSpace(dnsTTL))
	if err != nil || dns <= 0 {
		return fmt.Errorf("invalid -pop-dns-ttl %q (want a positive duration, e.g. 60s)", dnsTTL)
	}
	cfg.PopDNSTTL = dns

	cfg.PopHomeEndpoint = strings.TrimSpace(endpoint)
	if cfg.PopHomeEndpoint != "" {
		if err := checkHostPort(cfg.PopHomeEndpoint); err != nil {
			return fmt.Errorf("invalid -pop-home-endpoint %q: %v", cfg.PopHomeEndpoint, err)
		}
	} else if pops {
		return fmt.Errorf("-pop-home-endpoint is required with -pops: a pairing string names the endpoint a PoP dials")
	}

	cfg.PopDNSZone = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(zone)), ".")
	cfg.PopCertIssuer = strings.TrimSpace(issuer)
	if (cfg.PopDNSZone == "") != (cfg.PopCertIssuer == "") {
		return fmt.Errorf("-pop-dns-zone and -pop-cert-issuer must be set together (both empty is pinned-only)")
	}
	if cfg.PopDNSZone != "" {
		if errs := validation.IsDNS1123Subdomain(cfg.PopDNSZone); len(errs) > 0 {
			return fmt.Errorf("invalid -pop-dns-zone %q: %s", cfg.PopDNSZone, strings.Join(errs, "; "))
		}
		if _, _, err := ParseIssuer(cfg.PopCertIssuer); err != nil {
			return err
		}
	}
	return nil
}

// ParseIssuer splits -pop-cert-issuer into the cert-manager issuerRef kind
// and name.
func ParseIssuer(v string) (kind, name string, err error) {
	kind, name, ok := strings.Cut(v, "/")
	if !ok || name == "" || (kind != "Issuer" && kind != "ClusterIssuer") {
		return "", "", fmt.Errorf("invalid -pop-cert-issuer %q (want Issuer/<name> or ClusterIssuer/<name>)", v)
	}
	if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
		return "", "", fmt.Errorf("invalid -pop-cert-issuer %q: %s", v, strings.Join(errs, "; "))
	}
	return kind, name, nil
}

// checkHostPort accepts host:port with a numeric port in range and no
// scheme or path: exactly what pops.FormatPairing puts before the token.
func checkHostPort(v string) error {
	if strings.ContainsAny(v, "/?#@ ") {
		return fmt.Errorf("want host:port with no scheme or path")
	}
	host, port, err := net.SplitHostPort(v)
	if err != nil {
		return fmt.Errorf("want host:port")
	}
	if host == "" {
		return fmt.Errorf("missing host")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("port %q out of range", port)
	}
	return nil
}
