package opsmetrics

import (
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-telemetry/internal/sqlengine"
)

func scrape(t *testing.T, r *Registry) string {
	t.Helper()
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("content type %q", ct)
	}
	b, _ := io.ReadAll(rec.Body)
	return string(b)
}

func TestExpositionCarriesTheProbeVerdicts(t *testing.T) {
	r := New("1.9.0", true)
	r.SetSQLEngine(true)
	r.SetViews([]sqlengine.ViewHealth{
		{Name: "rollups", HasData: true, OK: false, Err: "Binder Error"},
		{Name: "relay", HasData: false, OK: true},
	}, time.Unix(1790000000, 0))
	out := scrape(t, r)
	for _, want := range []string{
		`gawk_telemetry_build_info{version="1.9.0"} 1`,
		"gawk_telemetry_sql_engine_up 1",
		`gawk_telemetry_sql_view_up{view="rollups"} 0`,
		`gawk_telemetry_sql_view_up{view="relay"} 1`,
		`gawk_telemetry_sql_view_has_data{view="rollups"} 1`,
		`gawk_telemetry_sql_view_has_data{view="relay"} 0`,
		"gawk_telemetry_sql_probe_last_run_timestamp_seconds 1790000000",
		"# TYPE gawk_telemetry_sql_view_up gauge",
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// The error text is for the log, not a label: it would be unbounded
	// cardinality, and it is not needed to decide whether to page.
	if strings.Contains(out, "Binder") {
		t.Error("an error message leaked into the exposition")
	}
}

// The chart's alert rules name these metrics in PromQL strings that nothing
// compiles. A renamed gauge would leave an alert that can never fire — the
// exact silent-rot shape the alerts exist to end — so every name the rules use
// must be one this package exports.
func TestTheChartsAlertRulesUseExportedMetrics(t *testing.T) {
	rules, err := os.ReadFile(filepath.Join("..", "..", "deploy", "charts", "gawk-telemetry", "templates", "prometheusrule.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	r := New("x", true)
	r.SetSQLEngine(true)
	r.SetViews([]sqlengine.ViewHealth{{Name: "rollups", HasData: true, OK: true}}, time.Unix(1, 0))
	out := scrape(t, r)
	names := regexp.MustCompile(`gawk_telemetry_[a-z_]+`).FindAllString(string(rules), -1)
	if len(names) == 0 {
		t.Fatal("no metric names found in the rules; the pattern or the file moved")
	}
	for _, n := range names {
		if !strings.Contains(out, "# TYPE "+n+" ") {
			t.Errorf("the chart's alert rules use %s, which is not exported", n)
		}
	}
}

// A deployment that turned the console off must not export an "engine down"
// that an alert would fire on.
func TestNoSQLGaugesWhenTheConsoleIsOff(t *testing.T) {
	out := scrape(t, New("1.9.0", false))
	if strings.Contains(out, "gawk_telemetry_sql_") {
		t.Errorf("SQL gauges exported with -query-sql off:\n%s", out)
	}
}

// Before the first probe round there is no verdict, and a zero would read as
// "broken" (view_up) or "stale since 1970" (the timestamp).
func TestNoViewGaugesBeforeTheFirstProbe(t *testing.T) {
	r := New("1.9.0", true)
	r.SetSQLEngine(false)
	out := scrape(t, r)
	if !strings.Contains(out, "gawk_telemetry_sql_engine_up 0\n") {
		t.Errorf("engine_up missing:\n%s", out)
	}
	if strings.Contains(out, "view_up") || strings.Contains(out, "last_run") {
		t.Errorf("view gauges before any probe:\n%s", out)
	}
}
