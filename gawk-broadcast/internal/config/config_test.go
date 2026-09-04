package config

import (
	"os"
	"path/filepath"
	"reflect"
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
		Room:                "TuhisRoom",
		RoomAttachSecret:    "attach-k",
		RoomLabel:           "Desk",
		Nickname:            "tuhis",
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
	// Load migrates the flat relay/secret pair into a profile (R37 SP8), so
	// the round trip is checked against the migrated shape — nothing lost,
	// just re-homed.
	if !reflect.DeepEqual(*got, Config{
		AppURL:          c.AppURL,
		LastBroadcastID: c.LastBroadcastID, LastResumeToken: c.LastResumeToken,
		LastGoodEncoder:     c.LastGoodEncoder,
		LastGoodAudioSource: c.LastGoodAudioSource,
		DisableAudio:        c.DisableAudio,
		AudioDevice:         c.AudioDevice,
		Room:                c.Room, RoomAttachSecret: c.RoomAttachSecret,
		RoomLabel: c.RoomLabel, Nickname: c.Nickname,
		Servers: []ServerProfile{{
			Name: "Migrated server", URL: "https://relay.example:4433", PublishSecret: "hunter2",
		}},
		SelectedServer: "Migrated server",
		Width:          c.Width, Height: c.Height,
		Fps: c.Fps, BitrateBps: c.BitrateBps, path: path,
	}) {
		t.Errorf("round trip lost fields:\n got %+v", *got)
	}
}

// A config already carrying profiles round-trips them untouched.
func TestSaveLoadRoundTripWithProfiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broadcast.json")
	c := &Config{
		Servers: []ServerProfile{
			{Name: DefaultServerName, URL: normalizeRelayURL(DefaultRelayURL), PublishSecret: "def-secret"},
			{Name: "Homelab", URL: "https://relay.home.example:4433", PublishSecret: "s3cret"},
		},
		SelectedServer: "Homelab",
	}
	c.SetPath(path)
	if err := c.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(got.Servers, c.Servers) || got.SelectedServer != c.SelectedServer {
		t.Errorf("round trip changed the server list:\n got %+v / %q\nwant %+v / %q",
			got.Servers, got.SelectedServer, c.Servers, c.SelectedServer)
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
	// Load migrates the flat URL into a profile; what matters here is that the
	// overwritten value, not the first one, is what a broadcast would dial.
	if got.Relay() != "https://b.example" {
		t.Errorf("Relay() = %q, want the overwritten value", got.Relay())
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
		// The pairing rule asks "is this the default fleet?", and that is a
		// question about an address, not about a string. A trailing slash, a
		// capitalized scheme or a capitalized host all name the very same relay
		// — the one whose key mints the tokens — so answering "no" to any of
		// them turns telemetry off for a user who did nothing but type the
		// address out. Silently: the only symptom is a session that never
		// appears, which is exactly the shape of the 2026-07-27 gap.
		{"a trailing slash is the same relay", DefaultRelayURL + "/", "", DefaultTelemetryURL},
		{"scheme case is the same relay", "HTTPS://api.gawk.ioio.fi:4433", "", DefaultTelemetryURL},
		{"host case is the same relay", "https://API.GAWK.IOIO.FI:4433", "", DefaultTelemetryURL},
		// Normalization must not go so far that a genuinely different relay
		// pairs with the reference collector.
		{"a different port is a different relay", "https://api.gawk.ioio.fi:5555", "", ""},
		{"a different host is a different relay", "https://relay.example:4433", "", ""},
		{"a path is a different relay", DefaultRelayURL + "/other", "", ""},
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

// --- R37 SP8: server profiles (docs/40 §4.1, §4.8) -------------------------

// The §4.1.2 migration, default-relay shape: a flat secret saved against the
// default fleet (URL blank or spelling the default out) becomes the default's
// credentials-only record, and the default stays selected.
func TestMigrationFromFlatDefaultRelay(t *testing.T) {
	for _, tc := range []struct{ name, relayJSON string }{
		{"blank URL", `{"publishSecret":"hunter2"}`},
		{"default URL spelled out", `{"relayUrl":"` + DefaultRelayURL + `/","publishSecret":"hunter2"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "broadcast.json")
			if err := os.WriteFile(path, []byte(tc.relayJSON), 0o600); err != nil {
				t.Fatal(err)
			}
			c, err := Load(path)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if c.RelayURL != "" || c.PublishSecret != "" {
				t.Errorf("flat fields survived migration: relay %q, secret %q", c.RelayURL, c.PublishSecret)
			}
			if c.SelectedServer != DefaultServerName {
				t.Errorf("SelectedServer = %q, want %q", c.SelectedServer, DefaultServerName)
			}
			relayURL, secret := c.Server()
			if relayURL != DefaultRelayURL || secret != "hunter2" {
				t.Errorf("Server() = (%q, %q), want the default relay with the migrated secret", relayURL, secret)
			}
			// The record is keyed to the URL it was saved against (F9's guard
			// needs that key to exist).
			if len(c.Servers) != 1 || c.Servers[0].Name != DefaultServerName || c.Servers[0].URL == "" {
				t.Errorf("Servers = %+v, want one URL-keyed default record", c.Servers)
			}
		})
	}
}

// The §4.1.2 migration, custom-relay shape: the user who pointed their install
// at their own relay keeps working without noticing.
func TestMigrationFromFlatCustomRelay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broadcast.json")
	legacy := `{"relayUrl":"https://relay.home.example:4433","publishSecret":"s3cret","appUrl":"https://gawk.example"}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	relayURL, secret := c.Server()
	if relayURL != "https://relay.home.example:4433" || secret != "s3cret" {
		t.Errorf("Server() = (%q, %q), want the legacy relay and secret", relayURL, secret)
	}
	if c.SelectedServer == "" || c.SelectedServer == DefaultServerName {
		t.Errorf("SelectedServer = %q, want the migrated profile selected", c.SelectedServer)
	}
	if c.AppURL != "https://gawk.example" {
		t.Errorf("AppURL = %q, migration must not touch unrelated fields", c.AppURL)
	}
	// The foreign-relay telemetry pairing rule survives the move: nothing is
	// reported by default on a non-default relay.
	if got := c.Telemetry(); got != "" {
		t.Errorf("Telemetry() = %q on a migrated custom relay, want none", got)
	}
}

// Migration is idempotent: Save → Load reproduces the same config, and the
// file stays 0600 through the rewrite.
func TestMigrationIsIdempotentAndKeeps0600(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broadcast.json")
	legacy := `{"relayUrl":"https://relay.home.example:4433","publishSecret":"s3cret"}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := first.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	second, err := Load(path)
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Errorf("migration is not idempotent:\nfirst  %+v\nsecond %+v", first, second)
	}
	if len(second.Servers) != 1 {
		t.Errorf("Servers after a second pass = %+v, want exactly the one migrated profile", second.Servers)
	}
	// Migrate() on an already-migrated config is a no-op too.
	second.Migrate()
	if !reflect.DeepEqual(first.Servers, second.Servers) || first.SelectedServer != second.SelectedServer {
		t.Errorf("a second Migrate() changed the config: %+v", second.Servers)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != FileMode {
		t.Errorf("mode after migration rewrite = %v, want %v — the secrets moved, the posture must not", fi.Mode().Perm(), FileMode)
	}
}

// F9: the default's credentials record is keyed to the URL it was saved
// against. A default that moved (binary upgrade) discards them rather than
// presenting the old relay's secret to the new host.
func TestStaleDefaultCredentialsAreDiscarded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broadcast.json")
	stale := `{"servers":[{"name":"default","url":"https://old-default.example:4433","publishSecret":"old"}],"selectedServer":"default"}`
	if err := os.WriteFile(path, []byte(stale), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.Servers) != 0 {
		t.Errorf("Servers = %+v, want the stale default record discarded", c.Servers)
	}
	if _, secret := c.Server(); secret != "" {
		t.Errorf("Server() secret = %q, want none — the old default's credential must not reach the new host", secret)
	}
}

// The selected profile is what Start dials; secrets are per-server (D4).
func TestServerResolution(t *testing.T) {
	base := func() *Config {
		return &Config{
			Servers: []ServerProfile{
				{Name: DefaultServerName, URL: normalizeRelayURL(DefaultRelayURL), PublishSecret: "def-secret"},
				{Name: "Homelab", URL: "https://relay.home.example:4433", PublishSecret: "home-secret"},
			},
		}
	}

	t.Run("default selected", func(t *testing.T) {
		c := base()
		c.SelectedServer = DefaultServerName
		if relayURL, secret := c.Server(); relayURL != DefaultRelayURL || secret != "def-secret" {
			t.Errorf("Server() = (%q, %q)", relayURL, secret)
		}
	})
	t.Run("custom selected", func(t *testing.T) {
		c := base()
		c.SelectedServer = "Homelab"
		if relayURL, secret := c.Server(); relayURL != "https://relay.home.example:4433" || secret != "home-secret" {
			t.Errorf("Server() = (%q, %q)", relayURL, secret)
		}
	})
	t.Run("unknown selection degrades to the default", func(t *testing.T) {
		c := base()
		c.SelectedServer = "Deleted"
		if relayURL, secret := c.Server(); relayURL != DefaultRelayURL || secret != "def-secret" {
			t.Errorf("Server() = (%q, %q), want the pinned default", relayURL, secret)
		}
	})
	t.Run("flat override to a stranger carries no stored secret", func(t *testing.T) {
		c := base()
		c.SelectedServer = "Homelab"
		c.RelayURL = "https://evil.example:4433" // what -url/GAWK_URL write
		if relayURL, secret := c.Server(); relayURL != "https://evil.example:4433" || secret != "" {
			t.Errorf("Server() = (%q, %q) — a stored secret must never follow an override to another host (D4)", relayURL, secret)
		}
	})
	t.Run("flat override matching a saved profile attaches its secret", func(t *testing.T) {
		c := base()
		c.SelectedServer = DefaultServerName
		// Same server as Homelab, spelled differently: normalized match.
		c.RelayURL = "HTTPS://relay.home.example:4433/"
		if _, secret := c.Server(); secret != "home-secret" {
			t.Errorf("Server() secret = %q, want the profile's own secret via normalized match", secret)
		}
	})
	t.Run("an explicit flat secret wins", func(t *testing.T) {
		c := base()
		c.SelectedServer = "Homelab"
		c.PublishSecret = "flag-secret" // what -secret/GAWK_SECRET write
		if _, secret := c.Server(); secret != "flag-secret" {
			t.Errorf("Server() secret = %q, want the explicit override", secret)
		}
	})
	t.Run("selected foreign profile reports no telemetry by default", func(t *testing.T) {
		c := base()
		c.SelectedServer = "Homelab"
		if got := c.Telemetry(); got != "" {
			t.Errorf("Telemetry() = %q, want none on a foreign relay (docs/40 §4.10 guard)", got)
		}
		c.SelectedServer = DefaultServerName
		if got := c.Telemetry(); got != DefaultTelemetryURL {
			t.Errorf("Telemetry() = %q on the default, want %q", got, DefaultTelemetryURL)
		}
	})
}

// The GUI's profile-management helpers keep the selection key (Name) unique
// and follow renames/removals with the selection.
func TestProfileManagementHelpers(t *testing.T) {
	c := &Config{}
	p := c.AddCustomServer()
	if p.Name == "" || p.Name == DefaultServerName {
		t.Fatalf("AddCustomServer name = %q", p.Name)
	}
	q := c.AddCustomServer()
	if q.Name == p.Name {
		t.Fatalf("two added profiles share the name %q", q.Name)
	}
	c.SelectedServer = p.Name
	c.UpdateCustomServer(0, ServerProfile{Name: "Homelab", URL: "https://relay.home.example:4433", PublishSecret: "s"})
	if c.SelectedServer != "Homelab" {
		t.Errorf("SelectedServer = %q, want the rename followed", c.SelectedServer)
	}
	// A rename into a collision (or the reserved name) keeps the old name.
	c.UpdateCustomServer(1, ServerProfile{Name: "Homelab"})
	if c.CustomServers()[1].Name == "Homelab" {
		t.Error("a name collision was allowed; Name is the selection key")
	}
	c.UpdateCustomServer(0, ServerProfile{Name: DefaultServerName, URL: "https://x.example"})
	if c.CustomServers()[0].Name != "Homelab" {
		t.Errorf("the reserved name %q was allowed onto a custom profile", DefaultServerName)
	}
	c.RemoveCustomServer(0)
	if c.SelectedServer != DefaultServerName {
		t.Errorf("SelectedServer = %q after removing the selected profile, want the default", c.SelectedServer)
	}
	if len(c.CustomServers()) != 1 {
		t.Errorf("CustomServers = %+v, want one left", c.CustomServers())
	}
	// The default's credential slot: set, rotate, clear (F4).
	c.SetDefaultSecret("first")
	if c.DefaultSecret() != "first" {
		t.Errorf("DefaultSecret = %q", c.DefaultSecret())
	}
	c.SetDefaultSecret("second")
	if c.DefaultSecret() != "second" {
		t.Errorf("DefaultSecret after rotation = %q", c.DefaultSecret())
	}
	c.SetDefaultSecret("")
	if c.DefaultSecret() != "" || len(c.Servers) != 1 {
		t.Errorf("clearing the default secret left %+v", c.Servers)
	}
}

func TestTelemetryOff(t *testing.T) {
	for raw, want := range map[string]bool{"off": true, " OFF ": true, "": false, "https://t.example": false} {
		if got := TelemetryOff(raw); got != want {
			t.Errorf("TelemetryOff(%q) = %v, want %v", raw, got, want)
		}
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
