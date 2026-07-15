// Package config is the broadcaster's persisted settings (R14 Decision 19,
// docs/19).
//
// The file is 0600 and it matters: it holds the publish secret *and* the
// portal restore token, and the token grants screen capture with no picker —
// it is a capability, not a preference.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// FileMode is the config file's permission. A pre-shared secret in a local
// file is consistent with how it already travels (a query param, per R2 — the
// WebTransport JS API cannot set headers), but the restore token raises the
// stakes: anything that can read this file can silently capture the screen.
const FileMode fs.FileMode = 0o600

// Config is the persisted settings. Everything is optional; the zero value is
// a usable first run.
type Config struct {
	// RelayURL is the relay's https:// origin.
	RelayURL string `json:"relayUrl,omitempty"`
	// AppURL is the *frontend* origin, used to build "join:" links and Copy
	// link. It is deliberately separate: it is not derivable from the relay
	// URL — they are different hosts (the relay is a UDP LoadBalancer, the app
	// is behind an nginx Ingress).
	AppURL string `json:"appUrl,omitempty"`
	// PublishSecret is R2's pre-shared publish secret.
	PublishSecret string `json:"publishSecret,omitempty"`
	// LastBroadcastID backs the GUI's Resume.
	LastBroadcastID string `json:"lastBroadcastId,omitempty"`
	// LastGoodEncoder is the cached cascade winner (Decision 4). Re-verified
	// on use, never trusted.
	LastGoodEncoder string `json:"lastGoodEncoder,omitempty"`
	// RestoreToken is the portal grant (Decision 5) — the reason for 0600.
	RestoreToken string `json:"restoreToken,omitempty"`

	// Rung.
	Width      int `json:"width,omitempty"`
	Height     int `json:"height,omitempty"`
	Fps        int `json:"fps,omitempty"`
	BitrateBps int `json:"bitrateBps,omitempty"`
	// Encoder pins one cascade candidate.
	Encoder string `json:"encoder,omitempty"`

	// path is where this was loaded from; Save writes back to it.
	path string
}

// DefaultPath is ~/.config/gawk/broadcast.json, honouring XDG_CONFIG_HOME.
func DefaultPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "gawk", "broadcast.json"), nil
}

// Load reads the config. A missing file is not an error — it is a first run.
func Load(path string) (*Config, error) {
	c := &Config{path: path}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, c); err != nil {
		// A corrupt config must not brick the app: say so, keep the defaults.
		return c, fmt.Errorf("config %s is not valid JSON (using defaults): %w", path, err)
	}
	c.path = path
	return c, nil
}

// Save writes the config atomically, 0600.
//
// Atomic because a crash mid-write would otherwise lose the restore token, and
// losing the token means the share picker comes back — the one thing the whole
// portal handshake exists to avoid.
func (c *Config) Save() error {
	if c.path == "" {
		return errors.New("config: no path to save to")
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')

	tmp, err := os.CreateTemp(filepath.Dir(c.path), ".broadcast-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds

	// Chmod before writing: the secret and the token must never exist on disk
	// world-readable, not even briefly.
	if err := tmp.Chmod(FileMode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, c.path)
}

// Path returns where this config lives.
func (c *Config) Path() string { return c.path }

// SetPath points the config at a file (for a config built from flags).
func (c *Config) SetPath(p string) { c.path = p }
