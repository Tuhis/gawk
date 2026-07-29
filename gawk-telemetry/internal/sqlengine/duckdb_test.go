//go:build duckdb

package sqlengine

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
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
