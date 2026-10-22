package popsrc

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/pops"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

const (
	uidA = "6f1c2d3e-4a5b-4c6d-8e7f-0123456789ab"
	uidB = "0a1b2c3d-4e5f-4a6b-8c7d-9e8f7a6b5c4d"
)

var credHash = pops.CredentialSHA256("the-credential")

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

func entry(name, uid, cred string) string {
	return "- name: " + name + "\n  uid: " + uid + "\n  credentialSHA256: " + cred + "\n"
}

func TestLoadFileParsesYAMLAndJSON(t *testing.T) {
	dir := t.TempDir()
	y := filepath.Join(dir, "pops.yaml")
	writeFile(t, y, `
- name: assembly-2026
  uid: `+uidA+`
  displayName: Assembly 2026
  credentialSHA256: `+strings.ToUpper(credHash)+`
  notAfter: 2026-11-09T00:00:00Z
  uplink: {maxMbps: 30}
  guests: {autoSwitch: false}
  wall: {roomCode: assembly-2026}
`)
	got, err := LoadFile(y)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d pops", len(got))
	}
	p := got[0]
	if p.Name != "assembly-2026" || string(p.UID) != uidA || p.Spec.DisplayName != "Assembly 2026" || p.Spec.Uplink.MaxMbps != 30 || p.AutoSwitch() {
		t.Errorf("pop = %+v", p)
	}
	if p.Spec.NotAfter == nil || p.Spec.Wall == nil || p.Spec.Wall.RoomCode != "assembly-2026" {
		t.Errorf("notAfter/wall = %v %v", p.Spec.NotAfter, p.Spec.Wall)
	}
	// The credential works with no pairing at all (docs/76 item 9), and the
	// hash is matched case-insensitively.
	if err := p.CheckCredential("the-credential", time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Errorf("file credential refused: %v", err)
	}
	if err := p.CheckCredential("other", time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)); !errors.Is(err, pops.ErrCredentialNotCurrent) {
		t.Errorf("wrong credential: %v", err)
	}

	j := filepath.Join(dir, "pops.json")
	writeFile(t, j, `[{"name":"lan","uid":"`+uidB+`","credentialSHA256":"`+credHash+`"}]`)
	got, err = LoadFile(j)
	if err != nil || len(got) != 1 || got[0].Name != "lan" {
		t.Fatalf("JSON LoadFile = %+v, %v", got, err)
	}

	empty := filepath.Join(dir, "empty.yaml")
	writeFile(t, empty, "")
	if got, err := LoadFile(empty); err != nil || len(got) != 0 {
		t.Errorf("empty file = %+v, %v", got, err)
	}
}

func TestLoadFileRejects(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct{ name, body, want string }{
		{"missing uid", "- name: lan\n  credentialSHA256: " + credHash + "\n", "uid"},
		{"uid not a uuid", entry("lan", "not-a-uuid", credHash), "uid"},
		{"duplicate name", entry("lan", uidA, credHash) + entry("lan", uidB, credHash), "duplicate"},
		{"duplicate uid", entry("lan", uidA, credHash) + entry("lan-2", uidA, credHash), "duplicate"},
		{"bad name", entry("Lan_Party", uidA, credHash), "name"},
		{"long name", entry(strings.Repeat("a", 64), uidA, credHash), "name"},
		{"missing credential", "- name: lan\n  uid: " + uidA + "\n", "credentialSHA256"},
		{"short credential", entry("lan", uidA, "abcd"), "credentialSHA256"},
		{"non-hex credential", entry("lan", uidA, strings.Repeat("z", 64)), "credentialSHA256"},
		{"bad wall", entry("lan", uidA, credHash) + "  wall: {roomCode: 5UP4XW}\n", "wall"},
		{"unknown field", entry("lan", uidA, credHash) + "  credential: plaintext\n", "unknown"},
		{"not a list", "name: lan\n", ""},
	} {
		path := filepath.Join(dir, strings.ReplaceAll(tc.name, " ", "-")+".yaml")
		writeFile(t, path, tc.body)
		_, err := LoadFile(path)
		if err == nil {
			t.Errorf("%s: LoadFile accepted it", tc.name)
			continue
		}
		if tc.want != "" && !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %q does not mention %q", tc.name, err, tc.want)
		}
		if strings.Contains(err.Error(), credHash) {
			t.Errorf("%s: error leaks the credential hash: %v", tc.name, err)
		}
	}
	if _, err := LoadFile(filepath.Join(dir, "absent.yaml")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("absent file: %v, want ErrNotExist", err)
	}
}

type events struct {
	mu    sync.Mutex
	names []string
}

func (e *events) add(name string) { e.mu.Lock(); e.names = append(e.names, name); e.mu.Unlock() }
func (e *events) seen(name string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, x := range e.names {
		if x == name {
			n++
		}
	}
	return n
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestStartFileReloadsAndKeepsTheLastGoodSet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pops.yaml")
	writeFile(t, path, entry("lan", uidA, credHash))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	src, err := startFile(ctx, path, discard, 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if !src.Synced() {
		t.Error("a file source is synced after its first load")
	}
	var ev events
	stop := src.Subscribe(ev.add)
	defer stop()

	p, ok := src.Get("lan")
	if !ok || string(p.UID) != uidA {
		t.Fatalf("Get(lan) = %+v, %v", p, ok)
	}
	// Get hands out a copy.
	p.Spec.DisplayName = "mutated"
	if again, _ := src.Get("lan"); again.Spec.DisplayName != "" {
		t.Error("Get aliases the source's copy")
	}

	// Add one, change one.
	writeFile(t, path, entry("lan", uidA, credHash)+"  displayName: LAN\n"+entry("party", uidB, credHash))
	waitUntil(t, "reload", func() bool { _, ok := src.Get("party"); return ok })
	waitUntil(t, "events", func() bool { return ev.seen("party") == 1 && ev.seen("lan") == 1 })
	if l := src.List(); len(l) != 2 || l[0].Name != "lan" || l[1].Name != "party" || l[0].Spec.DisplayName != "LAN" {
		t.Errorf("List = %+v", l)
	}

	// A malformed rewrite keeps the previous set.
	writeFile(t, path, entry("lan", "nope", credHash))
	time.Sleep(100 * time.Millisecond)
	if _, ok := src.Get("party"); !ok {
		t.Error("a malformed file dropped the previous set")
	}

	// Removal fires an event and ends the entry.
	writeFile(t, path, entry("lan", uidA, credHash)+"  displayName: LAN\n")
	waitUntil(t, "removal", func() bool { _, ok := src.Get("party"); return !ok })
	waitUntil(t, "removal event", func() bool { return ev.seen("party") == 2 })
	if ev.seen("lan") != 1 {
		t.Errorf("an unchanged entry fired %d events", ev.seen("lan"))
	}

	// Cancelled subscriptions see nothing more.
	stop()
	writeFile(t, path, "")
	waitUntil(t, "empty", func() bool { return len(src.List()) == 0 })
	if ev.seen("lan") != 1 {
		t.Error("a cancelled subscription still fired")
	}
}

func TestStartFileMissingAndMalformedAtStartup(t *testing.T) {
	dir := t.TempDir()
	ctx := t.Context()
	src, err := startFile(ctx, filepath.Join(dir, "absent.yaml"), discard, time.Hour)
	if err != nil {
		t.Fatalf("a missing file is no PoPs, not fatal: %v", err)
	}
	if len(src.List()) != 0 || !src.Synced() {
		t.Errorf("missing file: %+v synced=%v", src.List(), src.Synced())
	}
	bad := filepath.Join(dir, "bad.yaml")
	writeFile(t, bad, entry("lan", "nope", credHash))
	if _, err := StartFile(ctx, bad, discard); err == nil {
		t.Error("a malformed file at startup must fail the start")
	}
}
