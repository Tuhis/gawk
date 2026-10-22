package config

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// R74 PP2 (docs/76 §6.1 item 2): the gawk-relay profile changes defaults,
// never mechanisms. Everything not named in the §6.1 table must equal the
// gawk-server default, so the server profile's own defaults are the base.
func TestRelayDefaults(t *testing.T) {
	cfg, rc, err := ParseRelayFlags(nil, noEnv)
	if err != nil {
		t.Fatalf("ParseRelayFlags: %v", err)
	}
	want, err := ParseFlags(nil, noEnv)
	if err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	want.MaxBroadcasts = 50
	want.MaxSubscribers = 100
	want.MaxTotalSubscribers = 500
	want.MetricsAddr = "127.0.0.1:2112"
	want.Rooms = true
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("relay config =\n%+v\nwant\n%+v", cfg, want)
	}
	// The R73 knobs keep gawk-server's defaults (docs/76 §6.1 item 2).
	if !cfg.UplinkCarriers || cfg.UplinkMaxBitrateBytes != 50_000_000/8 {
		t.Errorf("uplink knobs = %v / %d, want gawk-server's defaults", cfg.UplinkCarriers, cfg.UplinkMaxBitrateBytes)
	}
	// The three keys stay empty here: an empty key on the relay means "load
	// or generate in the state directory" (relaystate.ApplyPersistedKeys).
	if cfg.StatelessResetKey != nil || cfg.ResumeTokenKey != nil || cfg.StatsKey != nil {
		t.Error("ParseRelayFlags must not invent keys; the state directory supplies them")
	}

	wantRC := RelayConfig{
		StateDir:       "", // resolved by relaystate.DefaultDir in cmd/gawk-relay
		ConsoleAddr:    "127.0.0.1:4480",
		UIAddr:         ":4433",
		LANAddress:     "auto",
		PopLegDelivery: "carrier",
	}
	if rc != wantRC {
		t.Errorf("relay knobs = %+v, want %+v", rc, wantRC)
	}
}

// The server profile is byte-identical to before the profile split: the
// FlagSet name and the defaults TestDefaults pins.
func TestServerProfileIsTheOldDefaults(t *testing.T) {
	want := Profile{
		Name: "gawk-server", Addr: ":4433", MaxSubscribers: "15", MaxBroadcasts: "5",
		MaxTotalSubscribers: "50", MetricsAddr: ":2112", Rooms: false,
	}
	if ServerProfile != want {
		t.Errorf("ServerProfile = %+v, want %+v", ServerProfile, want)
	}
	wantRelay := Profile{
		Name: "gawk-relay", Addr: ":4433", MaxSubscribers: "100", MaxBroadcasts: "50",
		MaxTotalSubscribers: "500", MetricsAddr: "127.0.0.1:2112", Rooms: true,
	}
	if RelayProfile != wantRelay {
		t.Errorf("RelayProfile = %+v, want %+v", RelayProfile, wantRelay)
	}
	// gawk-server never learns a relay-only flag.
	if _, err := ParseFlags([]string{"-state-dir", "/tmp/x"}, noEnv); err == nil {
		t.Error("gawk-server accepted -state-dir; relay knobs must be relay-only")
	}
}

// Precedence is the usual flag > env > profile default, for a profile knob,
// a shared knob and a relay-only knob alike, under the same GAWK_* names.
func TestParseRelayFlagsPrecedence(t *testing.T) {
	env := envMap(map[string]string{
		"GAWK_MAX_BROADCASTS":   "7",
		"GAWK_ROOMS":            "false",
		"GAWK_STATE_DIR":        "/env/state",
		"GAWK_CONSOLE_ADDR":     "127.0.0.1:5000",
		"GAWK_LAN_ADDRESS":      "192.168.1.20",
		"GAWK_PAIR":             "gawk-pair:api.example:4433/ABCDE",
		"GAWK_POP_LEG_DELIVERY": "datagram",
		"GAWK_UPLINK_BUDGET":    "25/3",
		"GAWK_DOWNLINK_BUDGET":  "100",
		"GAWK_UI_ADDR":          ":8443",
		"GAWK_SERVER_NAME":      "Assembly 2026",
	})
	cfg, rc, err := ParseRelayFlags(nil, env)
	if err != nil {
		t.Fatalf("env only: %v", err)
	}
	if cfg.MaxBroadcasts != 7 || cfg.Rooms || cfg.ServerName != "Assembly 2026" {
		t.Errorf("env did not beat the profile: max-broadcasts=%d rooms=%v name=%q", cfg.MaxBroadcasts, cfg.Rooms, cfg.ServerName)
	}
	wantRC := RelayConfig{
		StateDir: "/env/state", ConsoleAddr: "127.0.0.1:5000", UIAddr: ":8443", LANAddress: "192.168.1.20",
		Pair: "gawk-pair:api.example:4433/ABCDE", PopLegDelivery: "datagram",
		UplinkBudget: Budget{Mbps: 25, Streams: 3}, DownlinkBudget: Budget{Mbps: 100},
	}
	if rc != wantRC {
		t.Errorf("env relay knobs = %+v, want %+v", rc, wantRC)
	}

	cfg, rc, err = ParseRelayFlags([]string{
		"-max-broadcasts", "9", "-rooms=true", "-state-dir", "/flag/state",
		"-pop-leg-delivery", "carrier", "-uplink-budget", "10", "-lan-address", "auto",
	}, env)
	if err != nil {
		t.Fatalf("flags: %v", err)
	}
	if cfg.MaxBroadcasts != 9 || !cfg.Rooms {
		t.Errorf("flag did not beat env: max-broadcasts=%d rooms=%v", cfg.MaxBroadcasts, cfg.Rooms)
	}
	if rc.StateDir != "/flag/state" || rc.PopLegDelivery != "carrier" || rc.UplinkBudget != (Budget{Mbps: 10}) || rc.LANAddress != "auto" {
		t.Errorf("flag relay knobs = %+v", rc)
	}
	// Untouched by the flags: still the env's.
	if rc.ConsoleAddr != "127.0.0.1:5000" || rc.DownlinkBudget != (Budget{Mbps: 100}) {
		t.Errorf("env value lost under unrelated flags: %+v", rc)
	}
}

func TestParseRelayFlagsRejects(t *testing.T) {
	for _, args := range [][]string{
		{"-pop-leg-delivery", "stream"},
		{"-uplink-budget", "-1"},
		{"-uplink-budget", "25/x"},
		{"-uplink-budget", "25/3/1"},
		{"-downlink-budget", "fast"},
		{"-lan-address", "not-an-ip"},
		{"-console-addr", "no-port"},
		{"-ui-addr", "no-port"},
		// The shared validation still runs under the relay profile.
		{"-max-broadcasts", "0"},
	} {
		if _, _, err := ParseRelayFlags(args, noEnv); err == nil {
			t.Errorf("ParseRelayFlags(%q) accepted", args)
		}
	}
}

// -dev-cert is ignored by the relay (it always has a certificate); DevMode
// records that it was asked for, so main can say so instead of silently
// dropping it.
func TestRelayDevCertIsRecorded(t *testing.T) {
	_, rc, err := ParseRelayFlags([]string{"-dev-cert"}, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if !rc.DevMode {
		t.Error("DevMode not set with -dev-cert")
	}
}

func TestParseBudget(t *testing.T) {
	for in, want := range map[string]Budget{
		"":      {},
		"0":     {},
		"25":    {Mbps: 25},
		" 25/3": {Mbps: 25, Streams: 3},
		"0/2":   {Streams: 2},
	} {
		got, err := ParseBudget(in)
		if err != nil || got != want {
			t.Errorf("ParseBudget(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	if s := (Budget{Mbps: 25, Streams: 3}).String(); s != "25/3" {
		t.Errorf("String = %q", s)
	}
	if s := (Budget{Mbps: 25}).String(); s != "25" {
		t.Errorf("String = %q", s)
	}
	if s := (Budget{}).String(); s != "ceiling" {
		t.Errorf("String = %q", s)
	}
}

// The pairing string is a one-time credential: it never reaches a log.
func TestRelaySanitizedRedactsThePair(t *testing.T) {
	rc := RelayConfig{StateDir: "/s", Pair: "gawk-pair:host:4433/SECRET-TOKEN", UplinkBudget: Budget{Mbps: 25, Streams: 3}}
	out, err := json.Marshal(rc.Sanitized())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "SECRET") {
		t.Errorf("sanitized relay config leaks the pairing string: %s", out)
	}
	m := rc.Sanitized()
	if m["pair"] != secretSet || m["stateDir"] != "/s" || m["uplinkBudget"] != "25/3" {
		t.Errorf("sanitized = %v", m)
	}
	if (RelayConfig{}).Sanitized()["pair"] != secretUnset {
		t.Error("an unset pair must read <unset>")
	}
	// Every RelayConfig field is enumerated, the Config.Sanitized rule.
	if got, want := len(m), reflect.TypeOf(RelayConfig{}).NumField(); got != want {
		t.Errorf("Sanitized has %d keys, RelayConfig has %d fields", got, want)
	}
}

func TestParseHexKey32Exported(t *testing.T) {
	k, err := ParseHexKey32("x", strings.Repeat("ab", 32))
	if err != nil || len(k) != 32 {
		t.Fatalf("ParseHexKey32 = %v, %v", k, err)
	}
	if _, err := ParseHexKey32("x", "abcd"); err == nil {
		t.Error("short key accepted")
	}
}
