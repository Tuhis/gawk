package config

import (
	"strings"
	"testing"
)

// R79 (docs/80 §11.4): the voice knobs — flags and GAWK_* envs, the
// all-or-nothing rule, the server-name default, and every refusal.

var voiceFlags = []string{
	"-rooms",
	"-voice-bridge-addr", "bridge.gawk.svc:4444",
	"-voice-bridge-public-url", "https://voice.example/voice",
	"-voice-bridge-psk", "link-psk",
	"-voice-ticket-secret", "0123456789abcdef",
}

func TestVoiceFlagsParse(t *testing.T) {
	cfg, err := ParseFlags(append(append([]string{}, voiceFlags...),
		"-voice-bridge-ca-file", "/etc/gawk/bridge-ca.pem", "-voice-max-rooms", "3", "-voice-max-users-per-room", "7"), envMap(nil))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.VoiceConfigured() || cfg.VoiceBridgeAddr != "bridge.gawk.svc:4444" || cfg.VoiceBridgePublicURL != "https://voice.example/voice" ||
		cfg.VoiceBridgePSK != "link-psk" || cfg.VoiceTicketSecret != "0123456789abcdef" || cfg.VoiceBridgeCAFile != "/etc/gawk/bridge-ca.pem" ||
		cfg.VoiceMaxRooms != 3 || cfg.VoiceMaxUsersPerRoom != 7 {
		t.Fatalf("cfg = %+v", cfg)
	}
	// The server name defaults to the address's host.
	if cfg.VoiceBridgeServerName != "bridge.gawk.svc" {
		t.Fatalf("server name = %q", cfg.VoiceBridgeServerName)
	}
}

func TestVoiceEnvsParse(t *testing.T) {
	cfg, err := ParseFlags(nil, envMap(map[string]string{
		"GAWK_ROOMS":                    "true",
		"GAWK_VOICE_BRIDGE_ADDR":        "10.0.0.5:4444",
		"GAWK_VOICE_BRIDGE_PUBLIC_URL":  "https://voice.example/voice",
		"GAWK_VOICE_BRIDGE_PSK":         "env-psk",
		"GAWK_VOICE_TICKET_SECRET":      "env-secret-0123456789",
		"GAWK_VOICE_BRIDGE_SERVER_NAME": "bridge.internal",
		"GAWK_VOICE_BRIDGE_CA_FILE":     "/ca.pem",
		"GAWK_VOICE_MAX_ROOMS":          "4",
		"GAWK_VOICE_MAX_USERS_PER_ROOM": "9",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.VoiceConfigured() || cfg.VoiceBridgeAddr != "10.0.0.5:4444" || cfg.VoiceBridgePSK != "env-psk" ||
		cfg.VoiceTicketSecret != "env-secret-0123456789" || cfg.VoiceBridgeServerName != "bridge.internal" ||
		cfg.VoiceBridgeCAFile != "/ca.pem" || cfg.VoiceMaxRooms != 4 || cfg.VoiceMaxUsersPerRoom != 9 {
		t.Fatalf("cfg = %+v", cfg)
	}
}

func TestVoiceUnconfiguredIsOff(t *testing.T) {
	cfg, err := ParseFlags([]string{"-rooms"}, envMap(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.VoiceConfigured() {
		t.Fatal("voice configured with no bridge")
	}
	// A hand-built config with rooms off is never voice.
	c := cfg
	c.Rooms = false
	c.VoiceBridgeAddr, c.VoiceBridgePublicURL, c.VoiceBridgePSK, c.VoiceTicketSecret = "a:1", "https://x", "p", "s"
	if c.VoiceConfigured() {
		t.Fatal("VoiceConfigured without rooms")
	}
}

func TestVoiceFlagRefusals(t *testing.T) {
	with := func(replace map[string]string, drop ...string) []string {
		var out []string
		for i := 0; i < len(voiceFlags); i++ {
			f := voiceFlags[i]
			if f == "-rooms" {
				if !contains(drop, f) {
					out = append(out, f)
				}
				continue
			}
			v := voiceFlags[i+1]
			i++
			if contains(drop, f) {
				continue
			}
			if r, ok := replace[f]; ok {
				v = r
			}
			out = append(out, f, v)
		}
		return out
	}
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"partial set", with(nil, "-voice-bridge-psk"), "3 of 4"},
		{"only the address", []string{"-rooms", "-voice-bridge-addr", "a:1"}, "1 of 4"},
		{"no rooms", with(nil, "-rooms"), "requires -rooms"},
		{"short secret", with(map[string]string{"-voice-ticket-secret": "short"}), "at least 16"},
		{"http url", with(map[string]string{"-voice-bridge-public-url": "http://voice.example"}), "https"},
		{"relative url", with(map[string]string{"-voice-bridge-public-url": "/voice"}), "https"},
		{"long url", with(map[string]string{"-voice-bridge-public-url": "https://x/" + strings.Repeat("a", 600)}), "https"},
		{"addr without port", with(map[string]string{"-voice-bridge-addr": "bridge"}), "host:port"},
		{"bad max rooms", append(with(nil), "-voice-max-rooms", "0"), "voice-max-rooms"},
		{"bad max users", append(with(nil), "-voice-max-users-per-room", "x"), "voice-max-users-per-room"},
		{"server name without voice", []string{"-voice-bridge-server-name", "x"}, "need voice configured"},
		{"ca file without voice", []string{"-voice-bridge-ca-file", "/ca"}, "need voice configured"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseFlags(tc.args, envMap(nil))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
