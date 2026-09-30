//go:build duckdb

package sqlengine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// residentMiB is this process's resident set, from /proc. The test that uses
// it is about memory the Go runtime never sees (DuckDB's allocator), so
// runtime.MemStats would be blind to exactly the thing being measured.
func residentMiB(t *testing.T) int {
	t.Helper()
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Skipf("no /proc/self/status on this platform: %v", err)
	}
	for _, l := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(l, "VmRSS:"); ok {
			var kb int
			if _, err := fmt.Sscanf(strings.TrimSpace(rest), "%d", &kb); err != nil {
				t.Fatalf("parsing %q: %v", l, err)
			}
			return kb / 1024
		}
	}
	t.Fatal("no VmRSS in /proc/self/status")
	return 0
}

// seedManySessions writes n small, uncompressed session files spread over a
// month of date partitions — the shape that makes every union_by_name bind
// open every file.
func seedManySessions(t *testing.T, root string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		dir := filepath.Join(root, "sessions",
			fmt.Sprintf("date=2026-08-%02d", i%30+1), fmt.Sprintf("broadcast=%012x", i%10))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		var sb strings.Builder
		for l := 0; l < 200; l++ {
			fmt.Fprintf(&sb, `{"kind":"sample","sessionId":"%024x","role":"viewer","tMs":%d,"stats":{"fps":60,"rttMs":%d,"field%d":1}}`+"\n", i, l*2000, l, i%7)
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%024x.ndjson", i)), []byte(sb.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// seedRelay writes a month of relay/date=…/<pod>.ndjson partitions, three pods
// a day: the scraper appends one line per pod every few seconds, so these are
// the store's few large files next to sessions' many small ones.
func seedRelay(t *testing.T, root string, linesPerPod int) {
	t.Helper()
	for d := 1; d <= 30; d++ {
		dir := filepath.Join(root, "relay", fmt.Sprintf("date=2026-08-%02d", d))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for p := 0; p < 3; p++ {
			var sb strings.Builder
			for l := 0; l < linesPerPod; l++ {
				fmt.Fprintf(&sb, `{"pod":"gawk-server-%d","tMs":%d,"subscribers":%d,"bytesOut":%d,"broadcasts":[{"key":"%012x","viewers":%d}]}`+"\n", p, l*5000, l%50, l*1000, l%10, l%7)
			}
			if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("gawk-server-%d.ndjson", p)), []byte(sb.String()), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// The service's memory grew by hundreds of MiB per view-probe round until the
// pod was killed, every 30–90 minutes (2026-09-30). DuckDB's JSON reader
// allocates on every bind and scan outside the buffer manager that memory_limit
// governs, and its allocator keeps that memory for the life of the process;
// the probe made the process query every five minutes. Repeated probing must
// not grow the SERVICE's resident set.
func TestRepeatedProbesDoNotGrowTheServiceMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("seeds a month of partitions and runs ten probe rounds")
	}
	root := seedStore(t)
	seedManySessions(t, root, 600)
	seedRelay(t, root, 20000)
	e, err := OpenIsolated(Options{Root: root, MemoryLimit: 256 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	// The first round is the baseline: anything a single round costs once is
	// not the defect; what every further round adds is.
	Probe(e, root, now)
	runtime.GC()
	base := residentMiB(t)
	for i := 0; i < 10; i++ {
		for _, h := range Probe(e, root, now) {
			if !h.OK {
				t.Fatalf("round %d: %s: %s", i, h.Name, h.Err)
			}
		}
	}
	runtime.GC()
	// In-process, ten rounds over this store added ~350 MiB; isolated, the
	// service's own growth is noise.
	grew := residentMiB(t) - base
	t.Logf("ten probe rounds: resident set %d MiB -> %d MiB", base, base+grew)
	if grew > 100 {
		t.Fatalf("ten probe rounds grew the service's resident set by %d MiB (budget 100)", grew)
	}
}

// What an operator sees must not change with where the query runs: the same
// rows and types, the catalogue, a refusal as ErrRefused, and a DuckDB error
// as readable text.
func TestTheIsolatedEngineAnswersLikeTheInProcessOne(t *testing.T) {
	e, err := OpenIsolated(Options{Root: seedStore(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	byName := map[string]bool{}
	for _, v := range e.Views() {
		byName[v.Name] = v.Available
	}
	if !byName["rollups"] || byName["relay"] {
		t.Errorf("catalogue after open = %v, want rollups available and relay not", byName)
	}

	res, err := e.Query("SELECT role, stalls, 9007199254740993::BIGINT AS big FROM rollups")
	if err != nil {
		t.Fatal(err)
	}
	if res.RowCount != 1 || len(res.Columns) != 3 || res.Columns[0] != "role" {
		t.Fatalf("result = %+v", res)
	}
	if len(res.Types) != 3 || len(res.Views) == 0 {
		t.Errorf("types %v / views %v missing from the result", res.Types, res.Views)
	}
	// Past 2^53: a float64 round-trip would print 9007199254740992.
	if got := fmt.Sprint(res.Rows[0][2]); got != "9007199254740993" {
		t.Errorf("a BIGINT crossed the process boundary as %s", got)
	}
	if fmt.Sprint(res.Rows[0][0]) != "viewer" || fmt.Sprint(res.Rows[0][1]) != "2" {
		t.Errorf("row = %v", res.Rows[0])
	}

	// A worker registers only the views its statement names; the catalogue
	// must not forget the others because one query did not look at them.
	res, err = e.Query("SELECT 42 AS answer")
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(res.Rows[0][0]) != "42" {
		t.Errorf("SELECT 42 = %v", res.Rows[0])
	}
	for _, views := range [][]ViewDoc{res.Views, e.Views()} {
		for _, v := range views {
			if v.Name == "rollups" && !v.Available {
				t.Errorf("a query that did not name rollups marked it unavailable: %v", views)
			}
		}
	}

	if _, err := e.Query("COPY rollups TO '/tmp/oops.csv'"); !errors.Is(err, ErrRefused) {
		t.Errorf("a write statement was not refused: %v", err)
	}
	_, err = e.Query("SELECT notacolumn FROM rollups")
	if err == nil || !strings.Contains(err.Error(), "notacolumn") {
		t.Errorf("a binder error came back as %v, want DuckDB's message", err)
	}
}

// A query that outlives its budget is ended — the worker is killed, so a
// runaway scan cannot keep a core and its memory after the caller gave up.
func TestAnIsolatedQueryIsKilledAtItsTimeout(t *testing.T) {
	e, err := OpenIsolated(Options{Root: seedStore(t), Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	started := time.Now()
	_, err = e.Query("SELECT count(*) FROM range(1000000000000) a, range(1000000000000) b")
	if err == nil {
		t.Fatal("a cartesian scan of 10^24 rows answered")
	}
	if took := time.Since(started); took > 10*time.Second {
		t.Errorf("the query took %s to give up, budget 2s", took)
	}
}

// A worker that dies without answering (the OOM killer, a crash in C++) must
// surface as an error that says so — never as an empty result.
func TestAWorkerThatDiesWithoutAnsweringIsAnError(t *testing.T) {
	e, err := OpenIsolated(Options{Root: seedStore(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	t.Setenv(testWorkerCrashEnv, "1")
	_, err = e.Query("SELECT 1")
	if err == nil {
		t.Fatal("a worker that exited without answering produced a result")
	}
	if !strings.Contains(err.Error(), "worker") {
		t.Errorf("error %q does not say the worker failed", err)
	}
}
