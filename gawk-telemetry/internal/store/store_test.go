package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestStore(t *testing.T, now func() time.Time) *Store {
	t.Helper()
	s, err := New(Options{Root: t.TempDir(), Now: now})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func ref() SessionRef {
	return SessionRef{Date: "2026-07-26", BroadcastKey: "1a2b3c4d5e6f", SessionID: "000102030405060708090a0b"}
}

func TestAppendAndReadSession(t *testing.T) {
	s := newTestStore(t, nil)
	r := ref()
	if err := s.AppendSession(r, [][]byte{[]byte(`{"a":1}`), []byte(`{"a":2}`)}); err != nil {
		t.Fatalf("AppendSession: %v", err)
	}
	if err := s.AppendSession(r, [][]byte{[]byte(`{"a":3}`)}); err != nil {
		t.Fatalf("AppendSession: %v", err)
	}
	lines, err := s.ReadSession(r)
	if err != nil {
		t.Fatalf("ReadSession: %v", err)
	}
	if len(lines) != 3 {
		t.Fatalf("lines = %d, want 3", len(lines))
	}
	if string(lines[2]) != `{"a":3}` {
		t.Errorf("last line = %s", lines[2])
	}

	// Hive partitioning: the path is what both plain Go and DuckDB prune on.
	want := filepath.Join(s.Root(), "sessions", "date=2026-07-26", "broadcast=1a2b3c4d5e6f", "000102030405060708090a0b.ndjson")
	if _, err := os.Stat(want); err != nil {
		t.Errorf("expected file at %s: %v", want, err)
	}
}

// Every identifier that reaches a path is fixed-width hex by construction, so
// an exact-match pattern is airtight — no separator, dot or traversal
// component can pass. This is the only thing between a request and the disk.
func TestPathIdentifiersAreValidated(t *testing.T) {
	s := newTestStore(t, nil)
	bad := []SessionRef{
		{Date: "../../etc", BroadcastKey: "1a2b3c4d5e6f", SessionID: "000102030405060708090a0b"},
		{Date: "2026-07-26", BroadcastKey: "../escape", SessionID: "000102030405060708090a0b"},
		{Date: "2026-07-26", BroadcastKey: "1a2b3c4d5e6f", SessionID: "../../../etc/passwd"},
		{Date: "2026-07-26", BroadcastKey: "1a2b3c4d5e6f", SessionID: "0001"},
		{Date: "2026-07-26", BroadcastKey: "ZZZZZZZZZZZZ", SessionID: "000102030405060708090a0b"},
		{Date: "2026-7-6", BroadcastKey: "1a2b3c4d5e6f", SessionID: "000102030405060708090a0b"},
	}
	for _, r := range bad {
		if err := s.AppendSession(r, [][]byte{[]byte(`{}`)}); !errors.Is(err, ErrBadIdentifier) {
			t.Errorf("AppendSession(%+v) = %v, want ErrBadIdentifier", r, err)
		}
		if _, err := s.ReadSession(r); !errors.Is(err, ErrBadIdentifier) {
			t.Errorf("ReadSession(%+v) = %v, want ErrBadIdentifier", r, err)
		}
	}
	for _, pod := range []string{"../escape", "pod/../..", "POD", ""} {
		if err := s.AppendRelay("2026-07-26", pod, [][]byte{[]byte(`{}`)}); !errors.Is(err, ErrBadIdentifier) {
			t.Errorf("AppendRelay(pod=%q) = %v, want ErrBadIdentifier", pod, err)
		}
	}
	if _, err := s.FindSession("../x"); !errors.Is(err, ErrBadIdentifier) {
		t.Errorf("FindSession traversal = %v, want ErrBadIdentifier", err)
	}
}

func TestFinalizeGzipsAndReadStaysTransparent(t *testing.T) {
	s := newTestStore(t, nil)
	r := ref()
	if err := s.AppendSession(r, [][]byte{[]byte(`{"a":1}`), []byte(`{"a":2}`)}); err != nil {
		t.Fatal(err)
	}
	if err := s.FinalizeSession(r); err != nil {
		t.Fatalf("FinalizeSession: %v", err)
	}
	plain := filepath.Join(s.Root(), "sessions", "date=2026-07-26", "broadcast=1a2b3c4d5e6f", "000102030405060708090a0b.ndjson")
	if _, err := os.Stat(plain); !os.IsNotExist(err) {
		t.Error("the plain file survived finalize")
	}
	if _, err := os.Stat(plain + ".gz"); err != nil {
		t.Fatalf("no gzipped file: %v", err)
	}
	lines, err := s.ReadSession(r)
	if err != nil {
		t.Fatalf("ReadSession after finalize: %v", err)
	}
	if len(lines) != 2 || string(lines[0]) != `{"a":1}` {
		t.Errorf("lines after finalize = %q", lines)
	}
	// Idempotent: finalizing again is a no-op, not an error.
	if err := s.FinalizeSession(r); err != nil {
		t.Errorf("second FinalizeSession: %v", err)
	}
}

// The crash-recovery half of D3: a process that died mid-session leaves a
// plain .ndjson behind, and a directory scan that gzips them is obviously
// correct in a way appending to a gzip stream would not be.
func TestSweepOrphansFinalizesAbandonedFiles(t *testing.T) {
	now := time.Now()
	s := newTestStore(t, func() time.Time { return now })
	r := ref()
	if err := s.AppendSession(r, [][]byte{[]byte(`{"a":1}`)}); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash: the handles are gone but the plain file remains.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := New(Options{Root: s.Root(), Now: func() time.Time { return now.Add(time.Hour) }})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()

	n, err := reopened.SweepOrphans(time.Minute)
	if err != nil {
		t.Fatalf("SweepOrphans: %v", err)
	}
	if n != 1 {
		t.Fatalf("finalized %d orphans, want 1", n)
	}
	lines, err := reopened.ReadSession(r)
	if err != nil || len(lines) != 1 {
		t.Errorf("orphan not readable after sweep: %v / %d lines", err, len(lines))
	}
}

// A file this process is actively writing is not an orphan, even if quiet.
func TestSweepOrphansSkipsOpenFiles(t *testing.T) {
	now := time.Now()
	s := newTestStore(t, func() time.Time { return now })
	if err := s.AppendSession(ref(), [][]byte{[]byte(`{"a":1}`)}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	n, err := s.SweepOrphans(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("swept %d open files, want 0", n)
	}
}

// Retention is a directory delete, not a query — and rollups survive it. That
// split is the whole point of D4: raw is disposable, the summary is permanent.
func TestPruneDeletesOldRawPartitionsAndSparesRollups(t *testing.T) {
	s := newTestStore(t, nil)
	dates := []string{"2026-07-01", "2026-07-11", "2026-07-12", "2026-07-26"}
	for _, d := range dates {
		r := SessionRef{Date: d, BroadcastKey: "1a2b3c4d5e6f", SessionID: "000102030405060708090a0b"}
		if err := s.AppendSession(r, [][]byte{[]byte(`{"a":1}`)}); err != nil {
			t.Fatal(err)
		}
		if err := s.AppendRelay(d, "pod-a", [][]byte{[]byte(`{"r":1}`)}); err != nil {
			t.Fatal(err)
		}
		if err := s.AppendRollup(d, []byte(`{"sessionId":"x"}`)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(Options{Root: s.Root()})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()

	// Boundary property: the cutoff DAY itself survives; only strictly older
	// partitions go.
	cutoff := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)
	removed, err := reopened.Prune(cutoff)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	// 07-01 and 07-11 in both sessions/ and relay/.
	if removed != 4 {
		t.Errorf("removed %d partitions, want 4", removed)
	}
	for _, d := range []string{"2026-07-01", "2026-07-11"} {
		if _, err := os.Stat(filepath.Join(reopened.Root(), "sessions", "date="+d)); !os.IsNotExist(err) {
			t.Errorf("sessions/date=%s survived the prune", d)
		}
	}
	for _, d := range []string{"2026-07-12", "2026-07-26"} {
		if _, err := os.Stat(filepath.Join(reopened.Root(), "sessions", "date="+d)); err != nil {
			t.Errorf("sessions/date=%s was pruned but is on/after the cutoff", d)
		}
	}
	// Rollups are NEVER pruned.
	rollups, err := reopened.ReadRollups(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rollups) != len(dates) {
		t.Errorf("rollups after prune = %d, want %d (rollups are permanent)", len(rollups), len(dates))
	}
}

func TestReadRollupsFiltersBySince(t *testing.T) {
	s := newTestStore(t, nil)
	for _, d := range []string{"2026-07-01", "2026-07-20", "2026-07-26"} {
		if err := s.AppendRollup(d, []byte(`{"date":"`+d+`"}`)); err != nil {
			t.Fatal(err)
		}
	}
	lines, err := s.ReadRollups(time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 {
		t.Fatalf("lines = %d, want 2", len(lines))
	}
	if !strings.Contains(string(lines[0]), "2026-07-20") {
		t.Errorf("first line = %s, want the oldest included partition first", lines[0])
	}
}

func TestFindSessionLocatesAcrossPartitions(t *testing.T) {
	s := newTestStore(t, nil)
	r := SessionRef{Date: "2026-07-20", BroadcastKey: "aabbccddeeff", SessionID: "0f0e0d0c0b0a09080706050a"}
	if err := s.AppendSession(r, [][]byte{[]byte(`{"a":1}`)}); err != nil {
		t.Fatal(err)
	}
	got, err := s.FindSession(r.SessionID)
	if err != nil {
		t.Fatalf("FindSession: %v", err)
	}
	if got != r {
		t.Errorf("FindSession = %+v, want %+v", got, r)
	}
	if _, err := s.FindSession("ffffffffffffffffffffffff"); !errors.Is(err, ErrNotFound) {
		t.Errorf("FindSession(missing) = %v, want ErrNotFound", err)
	}
}

// A file being appended to (or truncated by a kill) must still read up to its
// last complete line rather than failing the whole read — the store exists to
// let you look at sessions that ended badly.
func TestReadSessionToleratesATruncatedTail(t *testing.T) {
	s := newTestStore(t, nil)
	r := ref()
	if err := s.AppendSession(r, [][]byte{[]byte(`{"a":1}`), []byte(`{"a":2}`)}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.Root(), "sessions", "date=2026-07-26", "broadcast=1a2b3c4d5e6f", "000102030405060708090a0b.ndjson")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"a":3` /* no closing brace, no newline */); err != nil {
		t.Fatal(err)
	}
	f.Close()

	reopened, err := New(Options{Root: s.Root()})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	lines, err := reopened.ReadSession(r)
	if err != nil {
		t.Fatalf("ReadSession: %v", err)
	}
	// The partial line comes back as-is; what matters is that the two complete
	// ones did, and that the read did not fail.
	if len(lines) < 2 || string(lines[0]) != `{"a":1}` || string(lines[1]) != `{"a":2}` {
		t.Errorf("lines = %q, want the complete ones intact", lines)
	}
}

func TestCloseIdleReleasesHandles(t *testing.T) {
	now := time.Now()
	s := newTestStore(t, func() time.Time { return now })
	if err := s.AppendSession(ref(), [][]byte{[]byte(`{"a":1}`)}); err != nil {
		t.Fatal(err)
	}
	if n := s.CloseIdle(time.Hour); n != 0 {
		t.Errorf("closed %d fresh handles, want 0", n)
	}
	now = now.Add(2 * time.Hour)
	if n := s.CloseIdle(time.Hour); n != 1 {
		t.Errorf("closed %d idle handles, want 1", n)
	}
	// Re-appending after a close reopens transparently and appends, never
	// truncates.
	if err := s.AppendSession(ref(), [][]byte{[]byte(`{"a":2}`)}); err != nil {
		t.Fatal(err)
	}
	lines, err := s.ReadSession(ref())
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 {
		t.Errorf("lines after reopen = %d, want 2 (append, not truncate)", len(lines))
	}
}

func TestAppendRelayAndRead(t *testing.T) {
	s := newTestStore(t, nil)
	if err := s.AppendRelay("2026-07-26", "gawk-server-0", [][]byte{[]byte(`{"pod":"a"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendRelay("2026-07-26", "gawk-server-1", [][]byte{[]byte(`{"pod":"b"}`)}); err != nil {
		t.Fatal(err)
	}
	lines, err := s.ReadRelay("2026-07-26")
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 {
		t.Errorf("relay lines = %d, want 2 (one per pod)", len(lines))
	}
	empty, err := s.ReadRelay("2026-07-25")
	if err != nil || len(empty) != 0 {
		t.Errorf("missing partition = %v / %d lines, want no error and none", err, len(empty))
	}
}
