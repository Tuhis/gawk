package config

import (
	"os"
	"path/filepath"
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
		RelayURL:        "https://relay.example:4433",
		AppURL:          "https://gawk.example",
		PublishSecret:   "hunter2",
		LastBroadcastID: "K7M2QP",
		LastResumeToken: "abab1212abab1212abab1212abab1212",
		LastGoodEncoder: "nvh264enc",
		Width:           1920,
		Height:          1080,
		Fps:             60,
		BitrateBps:      8_000_000,
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
		LastGoodEncoder: c.LastGoodEncoder,
		Width:           c.Width, Height: c.Height,
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
		t.Errorf("RelayURL = %q, want the default", c.RelayURL)
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
