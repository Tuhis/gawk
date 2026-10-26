//go:build duckdb

package sqlengine

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Production, 2026-10-24: a rejoin storm doubled the sessions tree to ~4,600
// files, and from then on the sessions view never answered — not the probe's
// pruned count, not an operator's query. Every query ran in a fresh worker,
// and a view over `read_json_auto(…, union_by_name=1)` sniffs EVERY file under
// its glob each time it is bound: at CREATE VIEW, and again on every query
// that reads it. The cost grew with the whole store and nothing carried over
// from one worker to the next, so once it outgrew the 30 s budget no query on
// the view could ever finish again.
//
// Each attempt here is a fresh engine over the same root, as each worker is.
func TestAStoreTooBigToSniffInOneQueryBecomesQueryable(t *testing.T) {
	root := seedStore(t)
	seedSessions(t, root, 6, 10, 20, 20)
	opts := Options{Root: root, Timeout: time.Second}
	const q = "SELECT count(*) FROM sessions WHERE date = '2026-08-01'"

	var err error
	attempts := 0
	for attempts < 60 {
		attempts++
		var e Engine
		e, err = Open(opts)
		if err != nil {
			t.Fatal(err)
		}
		_, err = e.Query(q)
		e.Close()
		if err == nil {
			break
		}
	}
	if err != nil {
		t.Fatalf("sessions never answered in %d attempts: %v", attempts, err)
	}
	t.Logf("answered on attempt %d", attempts)

	// Learned once, it stays learned: a fresh engine answers first time.
	for i := 0; i < 3; i++ {
		e, err := Open(opts)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.Query(q); err != nil {
			t.Fatalf("a fresh engine over a learned store failed: %v", err)
		}
		e.Close()
	}
}

// writeRollup writes one rollups file (rollups is the view whose glob is a
// plain directory, so a test controls exactly which files it covers).
func writeRollup(t *testing.T, root, name string, lines ...string) string {
	t.Helper()
	dir := filepath.Join(root, "rollups")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// describe is view's columns as name → type, through the engine.
func describe(t *testing.T, e Engine, view string) map[string]string {
	t.Helper()
	res, err := e.Query("DESCRIBE " + view)
	if err != nil {
		t.Fatalf("DESCRIBE %s: %v", view, err)
	}
	out := map[string]string{}
	for _, r := range res.Rows {
		out[r[0].(string)] = r[1].(string)
	}
	return out
}

// duckdbUnion is what DuckDB itself makes of files under union_by_name: the
// answer the learned schema replaces, and must agree with.
func duckdbUnion(t *testing.T, files []string, hive int) map[string]string {
	t.Helper()
	db, err := sql.Open("duckdb", "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	quoted := make([]string, len(files))
	for i, f := range files {
		quoted[i] = sqlString(f)
	}
	rows, err := db.Query(fmt.Sprintf(`SELECT column_name, column_type FROM (DESCRIBE SELECT * FROM read_json_auto([%s], hive_partitioning=%d, union_by_name=1, ignore_errors=true))`,
		strings.Join(quoted, ", "), hive))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var n, ty string
		if err := rows.Scan(&n, &ty); err != nil {
			t.Fatal(err)
		}
		out[n] = ty
	}
	return out
}

// The merge rules are a mirror of what DuckDB's sniffer makes of the same
// files, because DuckDB does not expose its own (coltype.go). Learning each
// file in its own batch forces every pair through merge; the answer must be
// the one union_by_name gives over both at once. A DuckDB upgrade that
// changes the sniffer's mind fails here.
func TestTheLearnedTypesAreTheOnesDuckDBWouldSniff(t *testing.T) {
	defer func(n int) { learnBatch = n }(learnBatch)
	learnBatch = 1
	for _, c := range [][2]string{
		{`1`, `1.5`}, {`1`, `"s"`}, {`1.5`, `"s"`}, {`true`, `1`}, {`null`, `1`},
		{`"2026-10-24"`, `"hello"`}, {`"2026-10-24 15:17:42"`, `"hello"`}, {`"2026-10-24"`, `"2026-10-24 15:17:42"`},
		{`{"a":1}`, `{"b":"x"}`}, {`{"a":1,"b":1}`, `{"b":1.5,"c":"x"}`}, {`{"a":1}`, `1`}, {`{"a":1}`, `[1]`}, {`"s"`, `{"a":1}`},
		{`[1]`, `["x"]`}, {`[1]`, `1`}, {`[]`, `[1]`}, {`{}`, `{"a":1}`}, {`null`, `{"a":1}`},
		{`{"FramesDroppedAtSend":1}`, `{"framesDroppedAtSend":2.5}`},
		{`{"a":[{"x":1}]}`, `{"a":[{"y":"z"}]}`},
	} {
		for _, order := range [][2]string{{c[0], c[1]}, {c[1], c[0]}} {
			root := t.TempDir()
			a := writeRollup(t, root, "1.ndjson", `{"k":1,"v":`+order[0]+`}`)
			b := writeRollup(t, root, "2.ndjson", `{"k":2,"v":`+order[1]+`}`)
			e, err := Open(Options{Root: root})
			if err != nil {
				t.Fatal(err)
			}
			got := describe(t, e, "rollups")["v"]
			e.Close()
			if want := duckdbUnion(t, []string{a, b}, 0)["v"]; got != want {
				t.Errorf("v: %s then %s: learned %s, DuckDB sniffs %s", order[0], order[1], got, want)
			}
		}
	}
}

// Over a store shaped like production's — gzipped hive partitions, ~80-field
// stats, a field that appears later and one that widens — the learned view
// has exactly the columns and types the sniffing one had.
func TestTheLearnedViewDescribesLikeTheSniffedOne(t *testing.T) {
	defer func(n int) { learnBatch = n }(learnBatch)
	learnBatch = 3
	root := seedStore(t)
	seedSessions(t, root, 3, 4, 2, 5)
	dir := filepath.Join(root, "sessions", "date=2026-08-03", "broadcast=000000000009")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	late := `{"kind":"sample","sessionId":"x","role":"broadcaster","tMs":1.5,"stats":{"codec":"av01","field00":"n/a","encoderPressure":true},"issuer":"abc","receivedAtMs":1}`
	if err := os.WriteFile(filepath.Join(dir, "late.ndjson"), []byte(late+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e, err := Open(Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	got := describe(t, e, "sessions")
	files, _ := filepath.Glob(viewSources(root)["sessions"].glob)
	want := duckdbUnion(t, files, 1)
	if len(got) != len(want) {
		t.Errorf("learned %d columns, DuckDB sniffs %d", len(got), len(want))
	}
	for n, ty := range want {
		if got[n] != ty {
			t.Errorf("%s: learned %s, DuckDB sniffs %s", n, got[n], ty)
		}
	}
}

// The point of learning: a file is sniffed once. Not again by the next query,
// not by the next worker (a fresh engine over the same root), and again only
// when it changes.
func TestAFileIsSniffedOnceUntilItChanges(t *testing.T) {
	var sniffed []string
	testHookSniff = func(files []string) { sniffed = append(sniffed, files...) }
	defer func() { testHookSniff = nil }()

	root := seedStore(t)
	seedSessions(t, root, 2, 2, 2, 3)
	open := func() Engine {
		t.Helper()
		e, err := Open(Options{Root: root})
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	e := open()
	if len(sniffed) != 9 { // 8 session files and the rollup
		t.Fatalf("first open sniffed %d files, want 9", len(sniffed))
	}

	sniffed = nil
	if _, err := e.Query("SELECT count(*) FROM sessions"); err != nil {
		t.Fatal(err)
	}
	e.Close()
	e = open()
	if _, err := e.Query("SELECT count(*) FROM sessions"); err != nil {
		t.Fatal(err)
	}
	if len(sniffed) != 0 {
		t.Fatalf("an unchanged store was sniffed again: %v", sniffed)
	}

	rollup := filepath.Join(root, "rollups", "2026-07-29.ndjson")
	f, err := os.OpenFile(rollup, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"sessionId":"c","role":"viewer","stalls":1,"addedLater":"yes"}` + "\n")
	f.Close()
	res, err := e.Query("SELECT addedLater FROM rollups WHERE addedLater IS NOT NULL")
	if err != nil {
		t.Fatalf("a field appended to a learned file is not queryable: %v", err)
	}
	if res.RowCount != 1 {
		t.Fatalf("rowCount = %d, want 1", res.RowCount)
	}
	if len(sniffed) != 1 || sniffed[0] != rollup {
		t.Fatalf("sniffed %v, want only the file that grew", sniffed)
	}
	e.Close()
}

// Learning a cold store takes as many queries as it takes. One that runs out
// of its share says how far along it is — not "table does not exist" — keeps
// what it learned for the next, and never registers a list that misses files.
func TestAQueryStillLearningSaysHowFarAlongItIs(t *testing.T) {
	defer func(n int) { learnBatch = n }(learnBatch)
	learnBatch = 1
	testHookSniff = func([]string) { time.Sleep(150 * time.Millisecond) }
	defer func() { testHookSniff = nil }()

	root := seedStore(t)
	seedSessions(t, root, 2, 3, 2, 2)
	e, err := Open(Options{Root: root, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	_, err = e.Query("SELECT count(*) FROM sessions")
	var learning *errLearning
	if !errors.As(err, &learning) {
		t.Fatalf("err = %v, want errLearning", err)
	}
	if learning.done == 0 || learning.done >= learning.total || learning.total != 12 {
		t.Fatalf("progress %d of %d", learning.done, learning.total)
	}
	if !strings.Contains(err.Error(), "still learning") {
		t.Fatalf("message %q does not say what is happening", err)
	}
	for _, v := range e.Views() {
		if v.Name == "sessions" && v.Available {
			t.Fatal("sessions registered over a column list that misses files")
		}
	}
	b, rerr := os.ReadFile(filepath.Join(SchemaDir(root), "sessions.json"))
	if rerr != nil {
		t.Fatalf("progress was not kept: %v", rerr)
	}
	var kept learned
	if err := json.Unmarshal(b, &kept); err != nil || len(kept.Files) < learning.done {
		t.Fatalf("kept %d files, want at least %d (%v)", len(kept.Files), learning.done, err)
	}

	testHookSniff = nil
	if _, err := e.Query("SELECT count(*) FROM sessions"); err != nil {
		t.Fatalf("learning did not finish: %v", err)
	}
}

// The learned schema is a cache. One that is unreadable, from another DuckDB,
// or in another format is relearned, never trusted.
func TestALearnedSchemaThatCannotBeTrustedIsRelearned(t *testing.T) {
	for name, content := range map[string]string{
		"garbage":        "{not json",
		"other duckdb":   `{"version":1,"duckdb":"v0.0.1","columns":"STRUCT(\"stalls\" VARCHAR)","files":{}}`,
		"other format":   `{"version":99,"duckdb":"","columns":"STRUCT(\"stalls\" VARCHAR)","files":{}}`,
		"broken columns": `{"version":1,"duckdb":"","columns":"STRUCT(","files":{}}`,
	} {
		t.Run(name, func(t *testing.T) {
			root := seedStore(t)
			if err := os.MkdirAll(SchemaDir(root), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(SchemaDir(root), "rollups.json"), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			e, err := Open(Options{Root: root})
			if err != nil {
				t.Fatal(err)
			}
			defer e.Close()
			if got := describe(t, e, "rollups")["stalls"]; got != "BIGINT" {
				t.Fatalf("stalls is %s, want BIGINT", got)
			}
		})
	}
}

// Retention prunes partitions: the files leave the schema; the columns they
// brought stay (reading NULL), as they would have under union_by_name until
// the pod restarted — types only widen.
func TestAPrunedFileLeavesTheSchemaButNotItsColumns(t *testing.T) {
	root := seedStore(t)
	gone := writeRollup(t, root, "2026-07-01.ndjson", `{"sessionId":"old","retired":1}`)
	e, err := Open(Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	if _, ok := describe(t, e, "rollups")["retired"]; !ok {
		t.Fatal("a pruned file's column disappeared")
	}
	b, err := os.ReadFile(filepath.Join(SchemaDir(root), "rollups.json"))
	if err != nil {
		t.Fatal(err)
	}
	var kept learned
	if err := json.Unmarshal(b, &kept); err != nil {
		t.Fatal(err)
	}
	if _, still := kept.Files["rollups/2026-07-01.ndjson"]; still || len(kept.Files) != 1 {
		t.Fatalf("schema still covers %v", kept.Files)
	}
}
