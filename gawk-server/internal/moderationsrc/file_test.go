package moderationsrc

import (
	"context"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/moderation"
)

var discardLog = slog.New(slog.NewTextHandler(io.Discard, nil))

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func waitFor(t *testing.T, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", desc)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestFileSourceLoadsAtStartup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bans.json")
	write(t, path, `[
	  {"target":{"type":"broadcastId","value":"abc23z"},"reason":"fraud"},
	  {"target":{"type":"ip","value":"203.0.113.0/24"},"reason":"abuse"}
	]`)

	set := moderation.NewSet()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := Start(ctx, Options{Source: "file:" + path, Set: set, Log: discardLog, PollInterval: 5 * time.Millisecond}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Synchronous: a relay must be enforcing before it accepts its first
	// publish, not one poll interval later.
	if _, ok := set.BannedID("ABC23Z", time.Now()); !ok {
		t.Error("the ID ban was not loaded at startup")
	}
	if _, ok := set.BannedIP(netip.MustParseAddr("203.0.113.7"), time.Now()); !ok {
		t.Error("the CIDR ban was not loaded at startup")
	}
}

// Acceptance criterion (docs/42 §9 AP2): reload on file change.
func TestFileSourceReloadsOnChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bans.json")
	write(t, path, `[{"target":{"type":"broadcastId","value":"ABC23Z"}}]`)

	set := moderation.NewSet()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := Start(ctx, Options{Source: "file:" + path, Set: set, Log: discardLog, PollInterval: 2 * time.Millisecond}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, ok := set.BannedID("ABC23Z", time.Now()); !ok {
		t.Fatal("initial ban missing")
	}

	// Swap the list: the old ban must go, the new one must arrive.
	write(t, path, `[{"target":{"type":"ip","value":"198.51.100.0/24"},"reason":"new"}]`)
	waitFor(t, "the new CIDR ban", func() bool {
		_, ok := set.BannedIP(netip.MustParseAddr("198.51.100.9"), time.Now())
		return ok
	})
	if _, ok := set.BannedID("ABC23Z", time.Now()); ok {
		t.Error("the removed ID ban is still enforced after a reload")
	}
}

// Acceptance criterion (docs/42 §9 AP2): reload on SIGHUP.
//
// The signal is sent to this very test process, which is the point: it proves
// the production handler is installed by Start, not merely that some channel
// can be written to.
func TestFileSourceReloadsOnSIGHUP(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bans.json")
	write(t, path, `[]`)

	set := moderation.NewSet()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// A poll interval far longer than the test can wait, so only SIGHUP can
	// be what causes the reload.
	if err := Start(ctx, Options{Source: "file:" + path, Set: set, Log: discardLog, PollInterval: time.Hour}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, ok := set.BannedID("ABC23Z", time.Now()); ok {
		t.Fatal("the empty list banned something")
	}

	write(t, path, `[{"target":{"type":"broadcastId","value":"ABC23Z"},"reason":"kill"}]`)
	if _, ok := set.BannedID("ABC23Z", time.Now()); ok {
		t.Fatal("the file was picked up without a signal; the poll interval was supposed to be an hour")
	}
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatalf("SIGHUP: %v", err)
	}
	waitFor(t, "the SIGHUP reload", func() bool {
		_, ok := set.BannedID("ABC23Z", time.Now())
		return ok
	})
}

// A missing file is "no bans yet", not a startup failure — and it is adopted
// the moment it appears.
func TestFileSourceToleratesAMissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bans.json")

	set := moderation.NewSet()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := Start(ctx, Options{Source: "file:" + path, Set: set, Log: discardLog, PollInterval: 2 * time.Millisecond}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	write(t, path, `[{"target":{"type":"broadcastId","value":"ABC23Z"}}]`)
	waitFor(t, "the ban file to be adopted once created", func() bool {
		_, ok := set.BannedID("ABC23Z", time.Now())
		return ok
	})

	// ...and a file that disappears clears the set rather than freezing it.
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}
	waitFor(t, "the ban set to clear when the file vanishes", func() bool {
		_, ok := set.BannedID("ABC23Z", time.Now())
		return !ok
	})
}

// A fat-fingered edit must not silently un-ban everyone.
func TestFileSourceKeepsPreviousSetOnMalformedJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bans.json")
	write(t, path, `[{"target":{"type":"broadcastId","value":"ABC23Z"}}]`)

	set := moderation.NewSet()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := Start(ctx, Options{Source: "file:" + path, Set: set, Log: discardLog, PollInterval: 2 * time.Millisecond}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	write(t, path, `{ this is not a list `)
	// Give the poller several intervals to have seen (and rejected) it.
	time.Sleep(50 * time.Millisecond)
	if _, ok := set.BannedID("ABC23Z", time.Now()); !ok {
		t.Error("a malformed ban file cleared the ban set")
	}
}

// One bad entry is skipped; the rest of the list still enforces.
func TestFileSourceSkipsUnparseableEntries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bans.json")
	write(t, path, `[
	  {"target":{"type":"broadcastId","value":"NOT-AN-ID"}},
	  {"target":{"type":"ip","value":"not-a-cidr"}},
	  {"target":{"type":"broadcastId","value":"ABC23Z"}}
	]`)

	set := moderation.NewSet()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := Start(ctx, Options{Source: "file:" + path, Set: set, Log: discardLog, PollInterval: time.Hour}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, ok := set.BannedID("ABC23Z", time.Now()); !ok {
		t.Error("a valid entry was dropped along with the invalid ones")
	}
	if got := set.ActiveCounts(time.Now()); got["broadcastId"] != 1 || got["ip"] != 0 {
		t.Errorf("ActiveCounts = %v, want exactly the one valid entry", got)
	}
}

// The file format carries expiresAt as RFC 3339, and it is honoured lazily.
func TestFileSourceHonoursExpiresAt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bans.json")
	exp := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	write(t, path, `[{"target":{"type":"broadcastId","value":"ABC23Z"},"expiresAt":"`+exp+`"}]`)

	set := moderation.NewSet()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := Start(ctx, Options{Source: "file:" + path, Set: set, Log: discardLog, PollInterval: time.Hour}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, ok := set.BannedID("ABC23Z", time.Now()); !ok {
		t.Error("the ban is not in force before its expiry")
	}
	if _, ok := set.BannedID("ABC23Z", time.Now().Add(2*time.Hour)); ok {
		t.Error("the ban is still in force after its expiry")
	}
}
