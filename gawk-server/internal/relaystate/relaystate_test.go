package relaystate

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
)

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestDefaultDirPerOS(t *testing.T) {
	cases := []struct {
		goos string
		env  map[string]string
		want string
	}{
		{"windows", map[string]string{"LOCALAPPDATA": `C:\Users\host\AppData\Local`}, `C:\Users\host\AppData\Local\gawk-relay`},
		{"darwin", map[string]string{"HOME": "/Users/host"}, "/Users/host/Library/Application Support/gawk-relay"},
		{"linux", map[string]string{"XDG_STATE_HOME": "/xdg/state", "HOME": "/home/host"}, "/xdg/state/gawk-relay"},
		{"linux", map[string]string{"HOME": "/home/host"}, "/home/host/.local/state/gawk-relay"},
		{"freebsd", map[string]string{"HOME": "/home/host"}, "/home/host/.local/state/gawk-relay"},
		// A relative XDG_STATE_HOME is invalid per the spec and ignored.
		{"linux", map[string]string{"XDG_STATE_HOME": "rel", "HOME": "/home/host"}, "/home/host/.local/state/gawk-relay"},
	}
	for _, c := range cases {
		got, err := DefaultDir(c.goos, envOf(c.env))
		if err != nil || got != c.want {
			t.Errorf("DefaultDir(%s, %v) = %q, %v; want %q", c.goos, c.env, got, err, c.want)
		}
	}
	for _, goos := range []string{"windows", "darwin", "linux"} {
		if _, err := DefaultDir(goos, envOf(nil)); err == nil {
			t.Errorf("DefaultDir(%s) with no env succeeded; want an error naming -state-dir", goos)
		}
	}
}

func TestEnsureCreatesAnOwnerOnlyDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "b")
	if err := Ensure(dir); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dir)
	if err != nil || !fi.IsDir() {
		t.Fatalf("stat: %v", err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o700 {
		t.Errorf("mode = %v, want 0700", fi.Mode().Perm())
	}
	if err := Ensure(dir); err != nil {
		t.Errorf("Ensure is not idempotent: %v", err)
	}
	if err := Ensure(""); err == nil {
		t.Error("Ensure accepted an empty directory")
	}
}

func TestLoadOrCreateKeyPersists(t *testing.T) {
	dir := t.TempDir()
	k1, err := LoadOrCreateKey(dir, "stats.key")
	if err != nil || len(k1) != 32 {
		t.Fatalf("create: %x, %v", k1, err)
	}
	k2, err := LoadOrCreateKey(dir, "stats.key")
	if err != nil || !bytes.Equal(k1, k2) {
		t.Fatalf("reload = %x, %v; want %x", k2, err, k1)
	}
	b, err := os.ReadFile(filepath.Join(dir, "stats.key"))
	if err != nil {
		t.Fatal(err)
	}
	if s := strings.TrimSpace(string(b)); len(s) != 64 || s != hex.EncodeToString(k1) {
		t.Errorf("file = %q, want the 64-hex flag format", b)
	}
	fi, _ := os.Stat(filepath.Join(dir, "stats.key"))
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
	// A corrupt key is an error, never silently replaced: replacing it would
	// invalidate every resume token in the field.
	if err := os.WriteFile(filepath.Join(dir, "bad.key"), []byte("nothex"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateKey(dir, "bad.key"); err == nil {
		t.Error("a corrupt key file was accepted")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "bad.key")); string(b) != "nothex" {
		t.Error("a corrupt key file was overwritten")
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "*.tmp*"))
	if len(matches) != 0 {
		t.Errorf("temp files left behind: %v", matches)
	}
}

// docs/76 §6.1 item 2: the three keys are generated once and persisted; an
// explicit flag or env value still wins and is never written to disk.
func TestApplyPersistedKeys(t *testing.T) {
	dir := t.TempDir()
	explicit := bytes.Repeat([]byte{0xAB}, 32)
	cfg := config.Config{ResumeTokenKey: explicit}
	if err := ApplyPersistedKeys(&cfg, dir); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(cfg.ResumeTokenKey, explicit) {
		t.Error("the explicit resume-token key lost to the persisted one")
	}
	if _, err := os.Stat(filepath.Join(dir, ResumeTokenKeyFile)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("an explicit key was persisted (or stat failed: %v)", err)
	}
	if len(cfg.StatelessResetKey) != 32 || len(cfg.StatsKey) != 32 {
		t.Fatalf("missing keys not filled: reset=%d stats=%d", len(cfg.StatelessResetKey), len(cfg.StatsKey))
	}
	if bytes.Equal(cfg.StatelessResetKey, cfg.StatsKey) {
		t.Error("two keys share one value")
	}
	// A restart reads the same keys back.
	var again config.Config
	if err := ApplyPersistedKeys(&again, dir); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again.StatelessResetKey, cfg.StatelessResetKey) || !bytes.Equal(again.StatsKey, cfg.StatsKey) {
		t.Error("persisted keys changed across a restart")
	}
	if len(again.ResumeTokenKey) != 32 || bytes.Equal(again.ResumeTokenKey, explicit) {
		t.Error("with no explicit key the resume key must come from the state directory")
	}
	// The startup log names the mode an operator checks.
	if again.ResumeTokenKeyMode() != "explicit-key" {
		t.Errorf("mode = %q", again.ResumeTokenKeyMode())
	}
}

func TestSecretRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadSecret(dir, "pop-credential"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("LoadSecret of a missing secret = %v, want a not-exist error", err)
	}
	want := []byte("credential-bytes\x00\xff")
	if err := SaveSecret(dir, "pop-credential", want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadSecret(dir, "pop-credential")
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("LoadSecret = %q, %v", got, err)
	}
	fi, _ := os.Stat(filepath.Join(dir, "pop-credential"))
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
	if err := SaveSecret(dir, "pop-credential", []byte("v2")); err != nil {
		t.Fatal(err)
	}
	if got, _ := LoadSecret(dir, "pop-credential"); string(got) != "v2" {
		t.Errorf("overwrite = %q", got)
	}
	if err := RemoveSecret(dir, "pop-credential"); err != nil {
		t.Fatal(err)
	}
	if err := RemoveSecret(dir, "pop-credential"); err != nil {
		t.Errorf("RemoveSecret of a missing secret: %v", err)
	}
}
