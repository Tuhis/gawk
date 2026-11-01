package main

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tuhis/gawk/gawk-telemetry/internal/repair"
)

// The 2026-11 history repair (docs/33 §4.17) is on by default — it has to
// run on the first boot of the released version with nobody at a shell —
// and -repair-history / GAWK_TELEMETRY_REPAIR_HISTORY turn it off.
func TestRepairHistoryFlag(t *testing.T) {
	c, err := parseFlags([]string{"-telemetry-key", key64}, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if !c.repairHistory {
		t.Error("repair-history off by default; production has no shell to run it from")
	}
	c, err = parseFlags(nil, envMap(map[string]string{
		"GAWK_TELEMETRY_KEY": key64, "GAWK_TELEMETRY_REPAIR_HISTORY": "false",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.repairHistory {
		t.Error("GAWK_TELEMETRY_REPAIR_HISTORY=false not applied")
	}
	c, err = parseFlags([]string{"-telemetry-key", key64, "-repair-history=false"}, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if c.repairHistory {
		t.Error("-repair-history=false not applied")
	}
}

// repairHistory is what run() calls before the store opens: it rewrites,
// marks, and logs one summary line; disabled, it touches nothing.
func TestRepairHistoryRunsOnceBeforeTheStore(t *testing.T) {
	poisoned := `{"sessionId":"aaaaaaaaaaaaaaaaaaaaaaaa","role":"viewer","series":{"capToRenderMs":{"n":1,"min":2880000,"p05":2880000,"median":2880000,"p95":2880000,"max":2880000}}}` + "\n"
	setup := func(t *testing.T) (string, string) {
		dir := t.TempDir()
		path := filepath.Join(dir, "rollups", "2026-09-20.ndjson")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(poisoned), 0o644); err != nil {
			t.Fatal(err)
		}
		return dir, path
	}

	t.Run("disabled", func(t *testing.T) {
		dir, path := setup(t)
		repairHistory(config{dataDir: dir}, slog.New(slog.DiscardHandler))
		if b, _ := os.ReadFile(path); string(b) != poisoned {
			t.Error("a disabled repair changed the data")
		}
		if _, err := os.Stat(repair.MarkerPath(dir)); !os.IsNotExist(err) {
			t.Error("a disabled repair wrote its marker")
		}
	})

	t.Run("enabled", func(t *testing.T) {
		dir, path := setup(t)
		var logs bytes.Buffer
		log := slog.New(slog.NewJSONHandler(&logs, nil))
		repairHistory(config{dataDir: dir, repairHistory: true}, log)
		if b, _ := os.ReadFile(path); strings.Contains(string(b), "capToRenderMs") {
			t.Errorf("poisoned series survived: %s", b)
		}
		if _, err := os.Stat(repair.MarkerPath(dir)); err != nil {
			t.Errorf("no marker: %v", err)
		}
		if !strings.Contains(logs.String(), `"latencySeriesRemoved":1`) {
			t.Errorf("no summary line: %s", logs.String())
		}
		logs.Reset()
		repairHistory(config{dataDir: dir, repairHistory: true}, log)
		if !strings.Contains(logs.String(), "already applied") {
			t.Errorf("second start did not report the marker: %s", logs.String())
		}
	})

	t.Run("previous attempt died", func(t *testing.T) {
		dir, path := setup(t)
		if err := os.MkdirAll(filepath.Dir(repair.StartedPath(dir)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(repair.StartedPath(dir), []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		var logs bytes.Buffer
		repairHistory(config{dataDir: dir, repairHistory: true}, slog.New(slog.NewJSONHandler(&logs, nil)))
		if b, _ := os.ReadFile(path); string(b) != poisoned {
			t.Error("a dead attempt was retried")
		}
		out := logs.String()
		if !strings.Contains(out, `"level":"WARN"`) || !strings.Contains(out, repair.StartedPath(dir)) {
			t.Errorf("want a WARN naming the marker to delete, got: %s", out)
		}
	})
}
