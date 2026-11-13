package sqlengine

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// testWorkerCrashEnv makes a worker exit without answering, standing in for the
// OOM killer or a crash inside DuckDB.
const testWorkerCrashEnv = "GAWK_TELEMETRY_TEST_SQL_WORKER_CRASH"

// testWorkerSlowCatalogueEnv makes a worker asked for the whole catalogue hang
// past any budget, standing in for a store too large to sniff in one go — what
// happened in production on 2026-10-22. Every other request answers.
const testWorkerSlowCatalogueEnv = "GAWK_TELEMETRY_TEST_SQL_WORKER_SLOW_CATALOGUE"

// The isolated engine re-executes its own binary as the worker. Under `go test`
// that binary is this test binary, so it must dispatch the way the service's
// main does before any test runs.
func TestMain(m *testing.M) {
	// A worker starts with an allowlisted environment (workerEnviron); these
	// two are how a test tells its worker to misbehave.
	testWorkerEnvKeep = []string{testWorkerCrashEnv, testWorkerSlowCatalogueEnv}
	if IsWorker() {
		if os.Getenv(testWorkerCrashEnv) == "1" {
			os.Exit(3)
		}
		in, _ := io.ReadAll(os.Stdin)
		if os.Getenv(testWorkerSlowCatalogueEnv) == "1" {
			var req workerRequest
			if json.Unmarshal(in, &req) == nil && req.SQL == "" && req.Views == nil {
				time.Sleep(time.Hour)
			}
		}
		os.Exit(RunWorker(bytes.NewReader(in), os.Stdout))
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

// The worker runs an operator's SQL, so it must not carry the service's
// environment: the read password, the admin-roles client secret and the OIDC
// settings all arrive there, and none of them is the worker's to hold.
func TestTheWorkerEnvironmentCarriesNoServiceSecrets(t *testing.T) {
	got := workerEnviron([]string{
		"GAWK_TELEMETRY_READ_PASSWORD=hunter2",
		"GAWK_TELEMETRY_READ_USER=ops",
		"GAWK_TELEMETRY_ASK_API_KEY=sk-secret",
		"KUBERNETES_SERVICE_HOST=10.0.0.1",
		"AWS_SECRET_ACCESS_KEY=x",
		"TZ=Europe/Helsinki",
		"HOME=/home/nonroot",
		"GOMEMLIMIT=1GiB",
		WorkerEnv + "=0",
	})
	want := []string{"TZ=Europe/Helsinki", "HOME=/home/nonroot", "GOMEMLIMIT=1GiB", WorkerEnv + "=1"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("worker environment:\n got  %q\n want %q", got, want)
	}
}
