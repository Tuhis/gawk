//go:build duckdb

package sqlengine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// shadowStore is a store whose values cannot come from the schema: anything
// a check answers that contains one of them came from the data.
func shadowStore(t *testing.T) (root string, markers []string) {
	t.Helper()
	root = t.TempDir()
	writeRollup(t, root, "2026-11-01.ndjson",
		`{"sessionId":"feedfacefeedfacefeedface","role":"viewer","os":"ZZMARKERLINUX","browser":"ZZMARKER 1","durationMs":987654321,"series":{"capToRenderMs":{"n":3,"median":41.5}},"verdict":{"healthy":true,"findings":[{"id":"zzfinding"}]}}`,
		`{"sessionId":"beefbeefbeefbeefbeefbeef","role":"broadcaster","os":"ZZMARKERWIN","browser":"gawk-broadcast-windows","durationMs":123456789}`)
	dir := filepath.Join(root, "sessions", "date=2026-11-01", "broadcast=abcdef012345")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "feedfacefeedfacefeedface.ndjson"),
		[]byte(`{"kind":"sample","sessionId":"feedfacefeedfacefeedface","role":"viewer","tMs":2000,"stats":{"receivedFps":59}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedRelay(t, root, 2)
	return root, []string{"ZZMARKER", "feedface", "987654321", "abcdef012345", "zzfinding"}
}

// openLearned is an isolated engine over root with every view's columns
// learned, as a console query leaves them.
func openLearned(t *testing.T, root string) *isolated {
	t.Helper()
	e, err := OpenIsolated(Options{Root: root, Timeout: 60 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range ShadowViews {
		if _, err := e.Query("SELECT count(*) FROM " + v); err != nil {
			t.Fatalf("learning %s: %v", v, err)
		}
	}
	return e.(*isolated)
}

func explain(t *testing.T, e *isolated, sql string) string {
	t.Helper()
	v, err := e.Explain(context.Background(), sql)
	if err != nil {
		t.Fatalf("Explain(%q): %v", sql, err)
	}
	return v
}

// The shadow catalogue binds what the views bind: its columns and types are
// the learned schema's, which is what DESCRIBE of each real view reports.
func TestShadowCatalogueHasTheViewsColumnsAndTypes(t *testing.T) {
	root, _ := shadowStore(t)
	e := openLearned(t, root)
	views, err := e.Schema()
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != len(ShadowViews) {
		t.Fatalf("shadow views = %d, want %d", len(views), len(ShadowViews))
	}
	for _, v := range views {
		res, err := e.Query("DESCRIBE " + v.Name)
		if err != nil {
			t.Fatal(err)
		}
		var real, shadow []string
		for _, r := range res.Rows {
			// DESCRIBE quotes only the field names that need it; the
			// shadow DDL quotes all of them. Same type either way.
			typ, err := parseType(fmt.Sprint(r[1]))
			if err != nil {
				t.Fatal(err)
			}
			real = append(real, fmt.Sprintf("%v %s", r[0], typ.emit()))
		}
		for _, c := range v.Columns {
			shadow = append(shadow, c.Name+" "+c.Type)
		}
		if strings.Join(real, "\n") != strings.Join(shadow, "\n") {
			t.Errorf("%s: shadow columns differ from the view's\nview:\n%s\nshadow:\n%s", v.Name,
				strings.Join(real, "\n"), strings.Join(shadow, "\n"))
		}
		// And DuckDB reads the shadow table back as the same columns.
		if got := explain(t, e, "SELECT * FROM "+v.Name); got != ExplainOK {
			t.Errorf("SELECT * FROM %s: %s", v.Name, got)
		}
	}
}

// docs/84 §12.6's probes: none of them may reach a value, and the two PIVOT
// drafts answer the same for a value that exists as for one that does not.
func TestExplainNeverReadsTheStore(t *testing.T) {
	root, markers := shadowStore(t)
	e := openLearned(t, root)
	probes := []string{
		"SELECT nosuchcol FROM (PIVOT rollups ON os USING count(*))",
		`SELECT "ZZMARKERLINUX" FROM (PIVOT rollups ON os USING count(*))`,
		"SELECT * FROM rollups WHERE sessionId = 'feedfacefeedfacefeedface'",
		"SELECT * FROM read_json_auto('" + filepath.Join(root, "rollups", "*.ndjson") + "')",
		"SELECT * FROM read_csv('" + filepath.Join(root, "rollups", "2026-11-01.ndjson") + "')",
		"SELECT * FROM glob('" + root + "/**')",
		"SELECT count(*) FROM sessions WHERE date = DATE '2026-11-01'",
		"SELECT max(durationMs) / 0 FROM rollups",
		"SELECT CAST(os AS INTEGER) FROM rollups",
		"SUMMARIZE rollups",
	}
	for _, p := range probes {
		got := explain(t, e, p)
		for _, m := range markers {
			// A verdict may quote the draft; only a value the draft did
			// not name itself would be a leak.
			if strings.Contains(strings.ToLower(p), strings.ToLower(m)) {
				continue
			}
			if strings.Contains(strings.ToLower(got), strings.ToLower(m)) {
				t.Errorf("%s\n  answered with a stored value (%q): %s", p, m, got)
			}
		}
	}
	// A value that exists and one that does not get the same answer, so the
	// verdict says nothing about which values exist. The two names differ in
	// one letter so that DuckDB's "candidate bindings", ranked by how alike
	// a column's name is, rank the same.
	exists := explain(t, e, `SELECT "ZZMARKERLINUX" FROM (PIVOT rollups ON os USING count(*))`)
	absent := explain(t, e, `SELECT "ZZMARKERLINUY" FROM (PIVOT rollups ON os USING count(*))`)
	if strings.ReplaceAll(exists, "ZZMARKERLINUX", "X") != strings.ReplaceAll(absent, "ZZMARKERLINUY", "X") {
		t.Errorf("a PIVOT answers differently for a stored value:\n  exists: %s\n  absent: %s", exists, absent)
	}
	if got := explain(t, e, "SELECT * FROM read_json_auto('/etc/hostname')"); !strings.Contains(got, "ERROR") {
		t.Errorf("a file function bound in the shadow catalogue: %s", got)
	}
}

func TestExplainAnswersOKOrTheBinderError(t *testing.T) {
	root, _ := shadowStore(t)
	e := openLearned(t, root)
	if got := explain(t, e, "SELECT role, count(*) FROM rollups GROUP BY role"); got != ExplainOK {
		t.Errorf("a valid draft: %s", got)
	}
	got := explain(t, e, "SELECT nosuch FROM rollups")
	if !strings.HasPrefix(got, "ERROR: ") || !strings.Contains(got, "nosuch") {
		t.Errorf("a binder error: %s", got)
	}
	if len(got) > MaxExplainBytes {
		t.Errorf("verdict is %d bytes, over %d", len(got), MaxExplainBytes)
	}
}

// Explain refuses what Check refuses, without starting a worker.
func TestExplainRefusesWhatCheckRefuses(t *testing.T) {
	e := &isolated{opts: Options{Root: t.TempDir()}, exe: "/nonexistent", checkTurn: make(chan struct{}, 1)}
	for _, s := range []string{
		"COPY rollups TO '/tmp/x.csv'",
		"SET enable_external_access = true",
		"SELECT 1; SELECT 2",
		"ATTACH '/tmp/x.db'",
		"",
	} {
		got, err := e.Explain(context.Background(), s)
		if err != nil {
			t.Errorf("%q: started a worker: %v", s, err)
			continue
		}
		if !strings.HasPrefix(got, "ERROR: refused") {
			t.Errorf("%q: %s", s, got)
		}
	}
}

// The catalogue cannot be reopened from inside: external access stays off.
func TestShadowConfigurationIsLocked(t *testing.T) {
	db, err := openShadow([]string{"CREATE TABLE rollups (os VARCHAR)"})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range []string{"SET enable_external_access = true", "SET autoload_known_extensions = true", "RESET enable_external_access"} {
		if _, err := db.Exec(s); err == nil {
			t.Errorf("%s: accepted; the configuration is not locked", s)
		}
	}
	var v string
	if err := db.QueryRow("SELECT current_setting('enable_external_access')::VARCHAR").Scan(&v); err != nil || v != "false" {
		t.Errorf("enable_external_access = %q (%v), want false", v, err)
	}
}

// A column the store gains is in the next check: the catalogue is read from
// the learned schema each time.
func TestShadowCatalogueFollowsTheLearnedSchema(t *testing.T) {
	root, _ := shadowStore(t)
	e := openLearned(t, root)
	if got := explain(t, e, "SELECT newField FROM rollups"); got == ExplainOK {
		t.Fatal("a column nothing has written binds")
	}
	writeRollup(t, root, "2026-11-02.ndjson", `{"sessionId":"cafecafecafecafecafecafe","newField":7}`)
	if _, err := e.Query("SELECT count(*) FROM rollups"); err != nil {
		t.Fatal(err)
	}
	if got := explain(t, e, "SELECT newField FROM rollups"); got != ExplainOK {
		t.Errorf("after the store learned newField: %s", got)
	}
}

// A check never waits behind a console query.
func TestExplainDoesNotQueueBehindAQuery(t *testing.T) {
	root, _ := shadowStore(t)
	e := openLearned(t, root)
	e.turn <- struct{}{} // a console query holds the engine
	defer func() { <-e.turn }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if got, err := e.Explain(ctx, "SELECT 1"); err != nil || got != ExplainOK {
		t.Errorf("Explain while a query runs: %q, %v", got, err)
	}
}
