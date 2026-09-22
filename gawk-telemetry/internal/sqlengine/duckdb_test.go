//go:build duckdb

package sqlengine

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The deployed image is built with this tag, so this is the configuration an
// operator actually queries.
const compiledExpectation = true

func seedStore(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "rollups"), 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"sessionId":"aaaaaaaaaaaaaaaaaaaaaaaa","broadcastKey":"1a2b3c4d5e6f","role":"viewer","stalls":2}`
	if err := os.WriteFile(filepath.Join(root, "rollups", "2026-07-29.ndjson"), []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestEngineAnswersOverTheStoredPartitions(t *testing.T) {
	e, err := Open(Options{Root: seedStore(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	res, err := e.Query("SELECT role, stalls FROM rollups")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if res.RowCount != 1 {
		t.Fatalf("rowCount = %d, want 1", res.RowCount)
	}
	if len(res.Columns) != 2 {
		t.Fatalf("columns = %v", res.Columns)
	}
	// Shaped so the UI can feed it to a chart rather than only to a table.
	if len(res.Rows[0]) != 2 {
		t.Fatalf("row = %v", res.Rows[0])
	}
}

// A view whose partition tree is empty must be REPORTED as unavailable, not
// silently absent — an operator writing a query against `relay` on a
// client-only fleet deserves to know why it fails.
func TestMissingPartitionsAreReportedNotHidden(t *testing.T) {
	e, err := Open(Options{Root: seedStore(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	byName := map[string]bool{}
	for _, v := range e.Views() {
		byName[v.Name] = v.Available
	}
	if !byName["rollups"] {
		t.Error("rollups is present on disk and reported unavailable")
	}
	if byName["relay"] {
		t.Error("relay has no partitions and is reported available")
	}
}

func TestEngineRefusesAWriteStatement(t *testing.T) {
	e, err := Open(Options{Root: seedStore(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if _, err := e.Query("COPY rollups TO '/tmp/oops.csv'"); !errors.Is(err, ErrRefused) {
		t.Fatalf("a write statement was not refused: %v", err)
	}
}

func TestAMalformedQueryFailsReadably(t *testing.T) {
	e, err := Open(Options{Root: seedStore(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	_, err = e.Query("SELECT notacolumn FROM rollups")
	if err == nil {
		t.Fatal("a query against a missing column succeeded")
	}
	if err.Error() == "" {
		t.Error("the failure carries no message")
	}
}

// The views are registered once, at startup, over globs that keep growing for
// the life of the pod — and DuckDB binds a view's column names and types at
// CREATE VIEW time. A partition written AFTER startup that adds a field (every
// milestone does, D15) or widens a type used to make every query on that view
// fail with "Contents of view were altered", until the pod restarted.
func TestAPartitionThatAddsAFieldAfterStartupStaysQueryable(t *testing.T) {
	root := seedStore(t)
	e, err := Open(Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if _, err := e.Query("SELECT count(*) FROM rollups"); err != nil {
		t.Fatalf("query before drift: %v", err)
	}

	line := `{"sessionId":"bbbbbbbbbbbbbbbbbbbbbbbb","broadcastKey":"1a2b3c4d5e6f","role":"viewer","stalls":"n/a","roomKey":"r0000001"}`
	if err := os.WriteFile(filepath.Join(root, "rollups", "2026-07-30.ndjson"), []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"SELECT count(*) FROM rollups",
		"SELECT * FROM rollups",
		"SELECT roomKey FROM rollups WHERE roomKey IS NOT NULL",
	} {
		res, err := e.Query(q)
		if err != nil {
			t.Fatalf("%s: a partition written after startup broke the view: %v", q, err)
		}
		if res.RowCount == 0 {
			t.Fatalf("%s: no rows", q)
		}
	}
}

// The spill directory lives on the data volume (the root filesystem is
// read-only), so a crashed process's leftovers must not accumulate there, the
// cap must actually be applied, and no view glob may pick the directory up.
func TestSpillDirIsClearedAtOpenAndCapped(t *testing.T) {
	root := seedStore(t)
	stale := filepath.Join(SpillDir(root), "duckdb_temp_storage-0.tmp")
	if err := os.MkdirAll(filepath.Dir(stale), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("left by a crashed process"), 0o600); err != nil {
		t.Fatal(err)
	}
	e, err := Open(Options{Root: root, SpillLimit: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("a previous process's spill file survived Open: %v", err)
	}
	if _, err := os.Stat(SpillDir(root)); err != nil {
		t.Errorf("spill dir not created: %v", err)
	}
	res, err := e.Query("SELECT current_setting('temp_directory'), current_setting('max_temp_directory_size'), current_setting('threads')")
	if err != nil {
		t.Fatal(err)
	}
	row := res.Rows[0]
	if row[0] != SpillDir(root) {
		t.Errorf("temp_directory = %v, want %s", row[0], SpillDir(root))
	}
	// DuckDB reports the cap in its own units; 64 MiB is "64.0 MiB".
	if s, _ := row[1].(string); !strings.HasPrefix(s, "64") {
		t.Errorf("max_temp_directory_size = %v, want 64 MiB", row[1])
	}
	if fmt.Sprint(row[2]) != fmt.Sprint(DefaultThreads) {
		t.Errorf("threads = %v, want %d", row[2], DefaultThreads)
	}
}

// The probe against the real engine: across a drift it must come back healthy
// (the heal runs through the same Query path), and its pruned questions must
// be valid SQL over real hive partitions.
func TestProbeOverTheRealEngineSurvivesDrift(t *testing.T) {
	root := seedStore(t)
	seedSessions(t, root, 1, 1, 1, 3)
	e, err := Open(Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	line := `{"sessionId":"bbbbbbbbbbbbbbbbbbbbbbbb","role":"viewer","stalls":"n/a","newField":true}`
	if err := os.WriteFile(filepath.Join(root, "rollups", "2026-07-30.ndjson"), []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, h := range Probe(e, root, time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)) {
		if !h.OK {
			t.Errorf("%s: probe failed: %s", h.Name, h.Err)
		}
		if want := h.Name == "sessions" || h.Name == "rollups"; h.HasData != want {
			t.Errorf("%s: hasData = %v, want %v", h.Name, h.HasData, want)
		}
	}
}

// PR #350 review: the engine has ONE connection, and Query held it (open rows)
// while taking the catalogue lock, whereas register took the catalogue lock and
// then waited for the connection. Two overlapping queries — the view probe and
// an operator, two console tabs, MCP — stalled each other until the first
// one's timeout, which lost its result and could fire a false view-down alert.
// The old engine re-registered the empty (so unavailable) relay view on every
// query — the normal state on a fleet without relay data — which is what put
// B in ExecContext holding the lock.
func TestOverlappingQueriesDoNotStallEachOther(t *testing.T) {
	root := seedStore(t)
	eng, err := Open(Options{Root: root, Timeout: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	holding := make(chan struct{})
	release := make(chan struct{})
	var once bool
	testHookRowsOpen = func() {
		if once {
			return
		}
		once = true
		close(holding)
		<-release
	}
	defer func() { testHookRowsOpen = nil }()

	start := time.Now()
	errA := make(chan error, 1)
	go func() {
		_, err := eng.Query("SELECT count(*) FROM rollups")
		errA <- err
	}()
	<-holding // A holds the connection
	errB := make(chan error, 1)
	go func() {
		_, err := eng.Query("SELECT role FROM rollups")
		errB <- err
	}()
	// Give B time to get as far as it can while A holds the connection. The
	// fixed engine passes with or without this; the broken one needs it to
	// reproduce the interleaving.
	time.Sleep(200 * time.Millisecond)
	close(release)

	for name, ch := range map[string]chan error{"A": errA, "B": errB} {
		select {
		case err := <-ch:
			if err != nil {
				t.Errorf("query %s failed: %v", name, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("query %s never returned", name)
		}
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("two small overlapping queries took %v; they stalled on each other", d)
	}
}

// PR #350 review: a registration cut short by the query's clock used to mark
// every view it touched unavailable and try to DROP them — so one slow query
// on the drift path took healthy views down for everyone, and the probe then
// reported them down.
func TestARegistrationThatRunsOutOfTimeLeavesTheCatalogueAlone(t *testing.T) {
	root := seedStore(t)
	eng, err := Open(Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	e := eng.(*engine)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e.register(ctx, true)
	for _, v := range e.Views() {
		if v.Name == "rollups" && !v.Available {
			t.Fatal("a registration that ran out of time marked rollups unavailable")
		}
	}
	if _, err := eng.Query("SELECT count(*) FROM rollups"); err != nil {
		t.Fatalf("rollups no longer answers: %v", err)
	}
}

// The same startup-only registration left a view whose tree was empty at boot —
// annotations before the first note, relay before the first scrape — reported
// unavailable (and unqueryable) until the pod restarted.
func TestAViewWhoseTreeAppearsAfterStartupBecomesQueryable(t *testing.T) {
	root := seedStore(t)
	e, err := Open(Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	dir := filepath.Join(root, "annotations")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"id":"n1","text":"deployed R50"}`
	if err := os.WriteFile(filepath.Join(dir, "annotations.ndjson"), []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := e.Query("SELECT text FROM annotations")
	if err != nil {
		t.Fatalf("annotations written after startup are unqueryable: %v", err)
	}
	if res.RowCount != 1 {
		t.Fatalf("rowCount = %d, want 1", res.RowCount)
	}
	for _, v := range e.Views() {
		if v.Name == "annotations" && !v.Available {
			t.Error("annotations exists on disk and is still reported unavailable")
		}
	}
}

// seedSessions writes a gzipped sessions tree shaped like the store's: one
// file per session under date=…/broadcast=…, stats objects of ~80 fields.
func seedSessions(t *testing.T, root string, dates, broadcasts, perBroadcast, lines int) {
	t.Helper()
	for d := 0; d < dates; d++ {
		for b := 0; b < broadcasts; b++ {
			dir := filepath.Join(root, "sessions", fmt.Sprintf("date=2026-08-%02d", d+1), fmt.Sprintf("broadcast=%012x", b))
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			for s := 0; s < perBroadcast; s++ {
				var buf bytes.Buffer
				gz := gzip.NewWriter(&buf)
				for l := 0; l < lines; l++ {
					stats := map[string]any{"codec": "avc1.42E02A"}
					for k := 0; k < 80; k++ {
						stats[fmt.Sprintf("field%02d", k)] = float64(l*k) / 7
					}
					line, _ := json.Marshal(map[string]any{
						"kind": "sample", "sessionId": fmt.Sprintf("%012x%012x", b, s),
						"broadcastKey": fmt.Sprintf("%012x", b), "role": "viewer",
						"tMs": l * 2000, "stats": stats, "receivedAtMs": 1756000000000 + l,
					})
					_, _ = gz.Write(append(line, '\n'))
				}
				gz.Close()
				name := filepath.Join(dir, fmt.Sprintf("%012x.ndjson.gz", s))
				if err := os.WriteFile(name, buf.Bytes(), 0o644); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
}

// BUGS.md (2026-08-20): every sessions query without a date/broadcast
// predicate died with "Out of Memory Error: failed to allocate data of size
// 32.0 MiB". The JSON reader holds a 32 MiB buffer PER THREAD, and DuckDB's
// default is one thread per core, so on a many-core node the scan alone needs
// more than the pod's whole budget before a byte is read. The engine must
// answer an unpruned scan inside the budget it is given.
func TestAnUnprunedSessionsScanFitsTheMemoryBudget(t *testing.T) {
	root := seedStore(t)
	seedSessions(t, root, 4, 5, 3, 50)
	e, err := Open(Options{Root: root, MemoryLimit: 160 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	for _, q := range []string{
		"SELECT count(*) FROM sessions",
		"SELECT sessionId, max(tMs) FROM sessions GROUP BY sessionId ORDER BY 2 DESC LIMIT 5",
	} {
		res, err := e.Query(q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if res.RowCount == 0 {
			t.Fatalf("%s: no rows", q)
		}
	}
}
