package sqlengine

import (
	"os"
	"strings"
	"testing"
)

// testWorkerCrashEnv makes a worker exit without answering, standing in for the
// OOM killer or a crash inside DuckDB.
const testWorkerCrashEnv = "GAWK_TELEMETRY_TEST_SQL_WORKER_CRASH"

// The isolated engine re-executes its own binary as the worker. Under `go test`
// that binary is this test binary, so it must dispatch the way the service's
// main does before any test runs.
func TestMain(m *testing.M) {
	if IsWorker() {
		if os.Getenv(testWorkerCrashEnv) == "1" {
			os.Exit(3)
		}
		os.Exit(RunWorker(os.Stdin, os.Stdout))
	}
	os.Exit(m.Run())
}

func TestReferencedViewsNamesWhatTheStatementReads(t *testing.T) {
	for stmt, want := range map[string]string{
		"SELECT 42":                    "",
		"SELECT * FROM sessions":       "sessions",
		`select count(*) from "Relay"`: "relay",
		"SELECT r.* FROM rollups r JOIN annotations a ON true": "rollups annotations",
		"SELECT sessionId FROM rollups":                        "rollups",
		"SELECT * FROM my_sessions_archive":                    "",
	} {
		if got := strings.Join(referencedViews(stmt), " "); got != want {
			t.Errorf("referencedViews(%q) = %q, want %q", stmt, got, want)
		}
	}
}
