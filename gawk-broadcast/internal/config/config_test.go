package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadMissingFileIsAFirstRunNotAnError(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("Load of a missing config = %v, want a usable zero config", err)
	}
	if c.RelayURL != "" {
		t.Errorf("RelayURL = %q, want empty", c.RelayURL)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "broadcast.json")
	c := &Config{
		RelayURL:            "https://relay.example:4433",
		AppURL:              "https://gawk.example",
		PublishSecret:       "hunter2",
		LastBroadcastID:     "K7M2QP",
		LastResumeToken:     "abab1212abab1212abab1212abab1212",
		LastGoodEncoder:     "nvh264enc",
		LastGoodAudioSource: "pipewire-monitor",
		DisableAudio:        true,
		AudioDevice:         "my-sink.monitor",
		Width:               1920,
		Height:              1080,
		Fps:                 60,
		BitrateBps:          8_000_000,
	}
	c.SetPath(path)
	if err := c.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if *got != (Config{
		RelayURL: c.RelayURL, AppURL: c.AppURL, PublishSecret: c.PublishSecret,
		LastBroadcastID: c.LastBroadcastID, LastResumeToken: c.LastResumeToken,
		LastGoodEncoder:     c.LastGoodEncoder,
		LastGoodAudioSource: c.LastGoodAudioSource,
		DisableAudio:        c.DisableAudio,
		AudioDevice:         c.AudioDevice,
		Width:               c.Width, Height: c.Height,
		Fps: c.Fps, BitrateBps: c.BitrateBps, path: path,
	}) {
		t.Errorf("round trip lost fields:\n got %+v\nwant %+v", *got, *c)
	}
}

// The file holds the publish secret, a credential. Anything that can read this
// file can publish to the relay under this broadcaster's identity.
func TestSaveIs0600(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broadcast.json")
	c := &Config{PublishSecret: "hunter2"}
	c.SetPath(path)
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != FileMode {
		t.Errorf("mode = %v, want %v — the publish secret is a credential", fi.Mode().Perm(), FileMode)
	}
}

// Overwriting must not widen the mode or leave a stale file behind.
func TestSaveOverwritesAtomicallyAndKeepsMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broadcast.json")
	c := &Config{RelayURL: "https://a.example"}
	c.SetPath(path)
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	c.RelayURL = "https://b.example"
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.RelayURL != "https://b.example" {
		t.Errorf("RelayURL = %q, want the overwritten value", got.RelayURL)
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != FileMode {
		t.Errorf("mode after overwrite = %v, want %v", fi.Mode().Perm(), FileMode)
	}
	// No temp files left over.
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range entries {
		if e.Name() != "broadcast.json" {
			t.Errorf("stray file left behind: %s", e.Name())
		}
	}
}

// A corrupt config must not brick the app — losing settings is recoverable,
// refusing to start is not.
func TestCorruptConfigDegradesToDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broadcast.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err == nil {
		t.Error("Load of a corrupt config returned no error; the user should be told")
	}
	if c == nil {
		t.Fatal("Load of a corrupt config returned no config; the app would not start")
	}
	if c.RelayURL != "" {
		t.Errorf("RelayURL = %q, want it blank so Relay() falls back to the default", c.RelayURL)
	}
}

// A first run must reach the reference fleet without being told where it is —
// "download it and press Start" is the whole point of the default.
func TestZeroConfigPointsAtTheDefaultFleet(t *testing.T) {
	var c Config
	if got := c.Relay(); got != DefaultRelayURL {
		t.Errorf("Relay() on a zero config = %q, want %q", got, DefaultRelayURL)
	}
	if got := c.Telemetry(); got != DefaultTelemetryURL {
		t.Errorf("Telemetry() on a zero config = %q, want %q", got, DefaultTelemetryURL)
	}
}

func TestResolveRelayURL(t *testing.T) {
	for _, tc := range []struct{ name, raw, want string }{
		{"blank uses the default", "", DefaultRelayURL},
		{"whitespace is blank", "   ", DefaultRelayURL},
		{"an explicit URL wins", "https://relay.example:4433", "https://relay.example:4433"},
		{"surrounding space is trimmed", "  https://relay.example  ", "https://relay.example"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveRelayURL(tc.raw); got != tc.want {
				t.Errorf("ResolveRelayURL(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestResolveTelemetryURL(t *testing.T) {
	for _, tc := range []struct{ name, relay, telemetry, want string }{
		{"default relay reports by default", "", "", DefaultTelemetryURL},
		{"the default relay spelled out still reports", DefaultRelayURL, "", DefaultTelemetryURL},
		// The pairing rule: a private relay's tokens are minted with a
		// different key, so the reference collector would reject the batch —
		// and pointing someone's own deployment at it by default is the wrong
		// default even when nothing lands.
		{"another relay reports nowhere by default", "https://relay.example:4433", "", ""},
		{"an explicit endpoint wins on any relay", "https://relay.example:4433", "https://t.example/ingest", "https://t.example/ingest"},
		{"off disables on the default relay", "", Off, ""},
		{"off is case-insensitive", "", "OFF", ""},
		{"off wins over an explicit relay too", "https://relay.example:4433", "off", ""},
		{"whitespace is blank, not an endpoint", "", "  ", DefaultTelemetryURL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveTelemetryURL(tc.relay, tc.telemetry); got != tc.want {
				t.Errorf("ResolveTelemetryURL(%q, %q) = %q, want %q", tc.relay, tc.telemetry, got, tc.want)
			}
		})
	}
}

// Resolution happens at use, never at save: a config file that records the
// default would pin the user to today's fleet address forever, and blanking a
// field is how you get back to "follow the default".
func TestSaveDoesNotBakeInTheDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broadcast.json")
	c := &Config{}
	c.SetPath(path)
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), DefaultRelayURL) || strings.Contains(string(b), DefaultTelemetryURL) {
		t.Errorf("Save wrote a default into the config file:\n%s", b)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.RelayURL != "" || got.TelemetryURL != "" {
		t.Errorf("round trip = {relay %q, telemetry %q}, want both still blank", got.RelayURL, got.TelemetryURL)
	}
}

func TestDefaultPathHonoursXDG(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xdg-test")
	got, err := DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	if want := "/tmp/xdg-test/gawk/broadcast.json"; got != want {
		t.Errorf("DefaultPath = %q, want %q", got, want)
	}
}

// The audio flag is spelled as a *disable* on purpose: a config written before
// R25 has no audio key at all, and the zero value has to mean audio on
// (docs/28 Decision 11). A `enableAudio` field would have silenced every
// existing installation on upgrade.
func TestAPreR25ConfigMeansAudioOn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broadcast.json")
	if err := os.WriteFile(path, []byte(`{"relayUrl":"https://relay.example:4433"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.DisableAudio {
		t.Error("a config predating R25 reads as audio-disabled")
	}
	if c.AudioDevice != "" {
		t.Errorf("AudioDevice = %q, want empty (probe the cascade)", c.AudioDevice)
	}
}
