// Package relaystate is gawk-relay's state directory (R74 PP2, docs/76 §6.1):
// one place, owner-only, for the persisted QUIC/resume/stats keys, the
// rolling pinned certificate and, once paired, the PoP credential.
package relaystate

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
)

// The persisted key files, each 64 hex chars: the flag's own format.
const (
	StatelessResetKeyFile = "stateless-reset.key"
	ResumeTokenKeyFile    = "resume-token.key"
	StatsKeyFile          = "stats.key"
)

// appName is the directory name under the per-OS base.
const appName = "gawk-relay"

// DefaultDir is the per-OS state directory (docs/76 §6.1 item 2):
// %LOCALAPPDATA%\gawk-relay on Windows, ~/Library/Application
// Support/gawk-relay on macOS, $XDG_STATE_HOME/gawk-relay (falling back to
// ~/.local/state/gawk-relay) elsewhere. The container sets GAWK_STATE_DIR
// instead. goos and getenv are injected so every branch is testable on one
// host; the result is joined with the target OS's separator.
func DefaultDir(goos string, getenv func(string) string) (string, error) {
	switch goos {
	case "windows":
		base := getenv("LOCALAPPDATA")
		if base == "" {
			return "", errors.New("relaystate: LOCALAPPDATA is not set; pass -state-dir")
		}
		return strings.TrimRight(base, `\/`) + `\` + appName, nil
	case "darwin":
		home := getenv("HOME")
		if home == "" {
			return "", errors.New("relaystate: HOME is not set; pass -state-dir")
		}
		return strings.TrimRight(home, "/") + "/Library/Application Support/" + appName, nil
	default:
		// The XDG spec: a relative path in XDG_STATE_HOME is invalid and
		// must be ignored.
		if x := getenv("XDG_STATE_HOME"); strings.HasPrefix(x, "/") {
			return strings.TrimRight(x, "/") + "/" + appName, nil
		}
		home := getenv("HOME")
		if home == "" {
			return "", errors.New("relaystate: neither XDG_STATE_HOME nor HOME is set; pass -state-dir")
		}
		return strings.TrimRight(home, "/") + "/.local/state/" + appName, nil
	}
}

// Ensure creates dir (and its parents) owner-only.
func Ensure(dir string) error {
	if dir == "" {
		return errors.New("relaystate: empty state directory")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("relaystate: %w", err)
	}
	return nil
}

// LoadOrCreateKey returns the 32-byte key in dir/name, generating and
// persisting one when the file does not exist. A file that exists but does
// not parse is an error, never replaced: a new key would silently invalidate
// every resume token and stats key in the field.
func LoadOrCreateKey(dir, name string) ([]byte, error) {
	path := filepath.Join(dir, name)
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		key, perr := config.ParseHexKey32(name, string(b))
		if perr != nil {
			return nil, fmt.Errorf("relaystate: %s: %w (delete it to generate a new one)", path, perr)
		}
		if key == nil {
			return nil, fmt.Errorf("relaystate: %s is empty (delete it to generate a new one)", path)
		}
		return key, nil
	case !errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("relaystate: %w", err)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("relaystate: %w", err)
	}
	if err := writeAtomic(dir, name, []byte(hex.EncodeToString(key)+"\n")); err != nil {
		return nil, err
	}
	return key, nil
}

// ApplyPersistedKeys fills each of the stateless-reset, resume-token and
// stats keys that is still empty from the state directory, generating it on
// first start. An explicit flag or env value wins and is never persisted.
// It must run before serve.Run: the transport, the hub and the resume-token
// minter read the keys once, at construction.
func ApplyPersistedKeys(cfg *config.Config, dir string) error {
	for _, k := range []struct {
		dst  *[]byte
		file string
	}{
		{&cfg.StatelessResetKey, StatelessResetKeyFile},
		{&cfg.ResumeTokenKey, ResumeTokenKeyFile},
		{&cfg.StatsKey, StatsKeyFile},
	} {
		if len(*k.dst) != 0 {
			continue
		}
		key, err := LoadOrCreateKey(dir, k.file)
		if err != nil {
			return err
		}
		*k.dst = key
	}
	return nil
}

// SaveSecret stores b as dir/name, owner-only; on Windows it is also
// DPAPI-protected for the current user (secret_windows.go).
func SaveSecret(dir, name string, b []byte) error {
	sealed, err := protect(b)
	if err != nil {
		return fmt.Errorf("relaystate: protect %s: %w", name, err)
	}
	return writeAtomic(dir, name, sealed)
}

// LoadSecret reads a secret SaveSecret stored. A missing secret returns an
// error satisfying errors.Is(err, fs.ErrNotExist).
func LoadSecret(dir, name string) ([]byte, error) {
	sealed, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return nil, err
	}
	b, err := unprotect(sealed)
	if err != nil {
		return nil, fmt.Errorf("relaystate: unprotect %s: %w", name, err)
	}
	return b, nil
}

// RemoveSecret deletes a secret; a missing one is not an error.
func RemoveSecret(dir, name string) error {
	if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("relaystate: %w", err)
	}
	return nil
}

// writeAtomic replaces dir/name with b, owner-only, via a temp file and a
// rename, so a crash never leaves a half-written key.
func writeAtomic(dir, name string, b []byte) error {
	tmp, err := os.CreateTemp(dir, name+".tmp-*")
	if err != nil {
		return fmt.Errorf("relaystate: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("relaystate: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("relaystate: %w", err)
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return fmt.Errorf("relaystate: %w", err)
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, name)); err != nil {
		return fmt.Errorf("relaystate: %w", err)
	}
	return nil
}
