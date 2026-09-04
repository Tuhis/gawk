package config

// R39 AP3 (docs/42 §4.5, §9): "a unit test feeds a config full of sentinel
// secret values and asserts none appear in the output — that test is the
// acceptance gate".

import (
	"encoding/json"
	"log/slog"
	"net"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// secretFields are the Config fields whose VALUE must never reach the
// sanitized view. publicFields are the ones whose value is deliberately
// reported. Every field of Config must appear in exactly one list — see
// TestSanitizedCoversEveryConfigField, which is what stops a secret added in
// some later milestone from being published by omission.
var (
	secretFields = []string{
		"PublishSecret",
		"InternalPSK",
		"StatsKey",
		"ResumeTokenKey",
		"TelemetryKey",
		"StatelessResetKey",
		"AdminAPIToken",
		"RoomCreateSecret",
	}
	publicFields = []string{
		"Addr", "CertFile", "KeyFile", "DevCert", "DevCertHosts",
		"LogLevel", "LogFormat", "MaxSubscribers", "AllowedOrigins",
		"MaxBroadcasts", "MaxTotalSubscribers", "ConnRateLimit",
		"ConnBurstLimit", "MaxBandwidthBytes", "MaxKeyframeBytes",
		"KeyframeWriteTimeout", "MetricsAddr", "ClusterMode",
		"InternalServerName", "TrustedCIDRs", "TelemetryReportInterval",
		"TelemetryAdvertiseURL", "ServerName", "ReleaseVersion",
		"QuietProbeLogs", "DVRWindow", "DVRMaxBytes", "DVRMaxCatchup",
		"DVRAudio", "LiveEdgeAudioOnReliableStream", "ParityDefault",
		"StripedDelivery", "ModerationSource", "AdminOIDCIssuer",
		"AdminOIDCAudience", "AdminOIDCRolesClaim", "AdminOIDCRole",
		"MaxIdleTimeout", "KeepAlivePeriod", "BroadcastGrace",
		"Rooms", "RoomEmptyGrace", "MaxRooms", "MaxRoomBroadcasts",
		"MaxRoomParticipants", "RoomsFile",
	}
)

// THE ACCEPTANCE GATE. Every secret-bearing field is set to a value that
// exists nowhere else in the process, and none of them may appear anywhere in
// the serialized response — not in a field of its own, not embedded in
// another string, not hex-encoded.
func TestSanitizedLeaksNoSecretValue(t *testing.T) {
	const (
		sentinelSecret = "SENTINEL-publish-secret-xyzzy"
		sentinelPSK    = "SENTINEL-internal-psk-xyzzy"
		sentinelToken  = "SENTINEL-admin-token-xyzzy"
	)
	// Distinct 32-byte keys, so a swapped assignment shows up as a leak of
	// the wrong one rather than passing by coincidence.
	statsKey := bytes32(0xA1)
	resumeKey := bytes32(0xB2)
	telemetryKey := bytes32(0xC3)
	resetKey := bytes32(0xD4)

	cfg := Config{
		PublishSecret:     sentinelSecret,
		InternalPSK:       sentinelPSK,
		AdminAPIToken:     sentinelToken,
		StatsKey:          statsKey,
		ResumeTokenKey:    resumeKey,
		TelemetryKey:      telemetryKey,
		StatelessResetKey: resetKey,
		// Non-secret fields too: the redaction must not be achieved by
		// dropping half the struct.
		Addr:             ":4433",
		ClusterMode:      true,
		MaxBroadcasts:    200,
		ModerationSource: "k8s",
	}

	body, err := json.Marshal(cfg.Sanitized())
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	out := string(body)

	for _, secret := range []string{sentinelSecret, sentinelPSK, sentinelToken, "SENTINEL", "xyzzy"} {
		if strings.Contains(out, secret) {
			t.Errorf("sanitized config leaked %q:\n%s", secret, out)
		}
	}
	for name, key := range map[string][]byte{
		"stats-key": statsKey, "resume-token-key": resumeKey,
		"telemetry-key": telemetryKey, "stateless-reset-key": resetKey,
	} {
		for _, enc := range encodings(key) {
			if strings.Contains(out, enc) {
				t.Errorf("sanitized config leaked the %s as %q:\n%s", name, enc, out)
			}
		}
	}

	// Redacted, not absent: an operator must be able to tell "set" from
	// "unset", which is the whole point of the view.
	san := cfg.Sanitized()
	for name, got := range map[string]string{
		"publishSecret":     san.PublishSecret,
		"internalPsk":       san.InternalPSK,
		"statsKey":          san.StatsKey,
		"statelessResetKey": san.StatelessResetKey,
		"telemetryKey":      san.TelemetryKey,
		"adminApiToken":     san.AdminAPIToken,
	} {
		if got != secretSet {
			t.Errorf("%s = %q, want %q", name, got, secretSet)
		}
	}
	if san.ResumeTokenKey != "<set:explicit-key>" {
		t.Errorf("resumeTokenKey = %q, want <set:explicit-key>", san.ResumeTokenKey)
	}
	// ...and the non-secret fields really are reported.
	if san.Addr != ":4433" || !san.ClusterMode || san.MaxBroadcasts != 200 || san.ModerationSource != "k8s" {
		t.Errorf("sanitized config dropped non-secret values: %+v", san)
	}
}

// An empty config says "unset" for every secret, so "no publish secret" is
// distinguishable from "a publish secret I am not showing you".
func TestSanitizedReportsUnsetSecrets(t *testing.T) {
	san := Config{}.Sanitized()
	for name, got := range map[string]string{
		"publishSecret":     san.PublishSecret,
		"internalPsk":       san.InternalPSK,
		"statsKey":          san.StatsKey,
		"statelessResetKey": san.StatelessResetKey,
		"telemetryKey":      san.TelemetryKey,
		"adminApiToken":     san.AdminAPIToken,
	} {
		if got != secretUnset {
			t.Errorf("%s = %q, want %q", name, got, secretUnset)
		}
	}
	if san.ResumeTokenKey != "<unset:per-process-random>" {
		t.Errorf("resumeTokenKey = %q, want <unset:per-process-random>", san.ResumeTokenKey)
	}
}

// The resume key reports its MODE, echoing the startup log verbatim — the
// three-way answer an operator actually needs (docs/42 §4.5).
func TestSanitizedResumeTokenKeyNamesItsMode(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
		want string
	}{
		{"explicit key wins", Config{ResumeTokenKey: bytes32(1), PublishSecret: "s"}, "<set:explicit-key>"},
		{"derived from the publish secret", Config{PublishSecret: "s"}, "<set:derived-from-publish-secret>"},
		{"per-process random", Config{}, "<unset:per-process-random>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.Sanitized().ResumeTokenKey; got != tc.want {
				t.Errorf("resumeTokenKey = %q, want %q", got, tc.want)
			}
			// The bare mode is what logStartup prints; the two must stay one
			// string, not two that drift.
			if !strings.Contains(tc.want, tc.cfg.ResumeTokenKeyMode()) {
				t.Errorf("redaction %q does not carry the logged mode %q",
					tc.want, tc.cfg.ResumeTokenKeyMode())
			}
		})
	}
}

// Completeness. Sanitized enumerates fields by hand on purpose (no reflection
// over names — see sanitized.go), so the guard against a future field being
// forgotten has to live here: every Config field must be classified, and
// every classified name must still exist.
func TestSanitizedCoversEveryConfigField(t *testing.T) {
	rt := reflect.TypeOf(Config{})
	var missing []string
	for i := range rt.NumField() {
		name := rt.Field(i).Name
		inSecret := slices.Contains(secretFields, name)
		inPublic := slices.Contains(publicFields, name)
		switch {
		case inSecret && inPublic:
			t.Errorf("Config.%s is classified both secret and public", name)
		case !inSecret && !inPublic:
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Errorf(`Config gained field(s) %v that sanitized_test.go does not classify.

Decide whether each is secret-bearing, add it to secretFields or publicFields,
and make sure config.Sanitized() handles it — a field nobody classified is a
field that could reach GET /internal/admin/config unredacted.`, missing)
	}
	for _, name := range slices.Concat(secretFields, publicFields) {
		if _, ok := rt.FieldByName(name); !ok {
			t.Errorf("sanitized_test.go classifies %q, which Config no longer has", name)
		}
	}
}

// Sanity on the sanitized shape itself: every field carries a json tag (an
// untagged field would ship as a Go name in a versioned schema), and the
// durations render as strings an operator recognizes from the flags.
func TestSanitizedShape(t *testing.T) {
	rt := reflect.TypeOf(SanitizedConfig{})
	for i := range rt.NumField() {
		f := rt.Field(i)
		if f.Tag.Get("json") == "" {
			t.Errorf("SanitizedConfig.%s has no json tag", f.Name)
		}
	}
	san := Config{
		BroadcastGrace:  5 * time.Minute,
		KeepAlivePeriod: 0,
		LogLevel:        slog.LevelWarn,
		TrustedCIDRs:    []*net.IPNet{mustCIDR(t, "10.0.0.0/8")},
	}.Sanitized()
	if san.BroadcastGrace != "5m0s" {
		t.Errorf("broadcastGrace = %q, want 5m0s", san.BroadcastGrace)
	}
	// "keepalive off" is a real configuration and must not read as unset.
	if san.KeepAlivePeriod != "0s" {
		t.Errorf("keepAlivePeriod = %q, want 0s", san.KeepAlivePeriod)
	}
	if san.LogLevel != "WARN" {
		t.Errorf("logLevel = %q, want WARN", san.LogLevel)
	}
	if len(san.TrustedCIDRs) != 1 || san.TrustedCIDRs[0] != "10.0.0.0/8" {
		t.Errorf("trustedCidrs = %v, want [10.0.0.0/8]", san.TrustedCIDRs)
	}
}

func bytes32(fill byte) []byte {
	b := make([]byte, 32)
	for i := range b {
		b[i] = fill
	}
	return b
}

// encodings lists the renderings a key could plausibly leak as.
func encodings(key []byte) []string {
	var hexUpper, hexLower strings.Builder
	const digits = "0123456789abcdef"
	for _, b := range key {
		hexLower.WriteByte(digits[b>>4])
		hexLower.WriteByte(digits[b&0xf])
	}
	hexUpper.WriteString(strings.ToUpper(hexLower.String()))
	return []string{hexLower.String(), hexUpper.String(), string(key)}
}

func mustCIDR(t *testing.T, s string) *net.IPNet {
	t.Helper()
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatalf("ParseCIDR(%q): %v", s, err)
	}
	return n
}
