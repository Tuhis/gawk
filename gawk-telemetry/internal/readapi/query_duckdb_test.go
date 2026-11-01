//go:build duckdb

package readapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-telemetry/internal/sqlengine"
)

// A NaN or ±Inf in a result must reach the console as a value, not as a
// failed response: encoding/json refuses both (docs/84 §12.4).
func TestTheQueryConsoleAnswersANonFiniteDouble(t *testing.T) {
	f := newFixture(t)
	eng, err := sqlengine.Open(sqlengine.Options{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	api, err := New(Options{Store: f.store, Now: func() time.Time { return f.now }, SQL: eng})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(api.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/query", "application/json",
		strings.NewReader(`{"sql":"SELECT 1/0 AS a, 'nan'::DOUBLE AS b, '-inf'::DOUBLE AS c"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /v1/query = %d, want 200", resp.StatusCode)
	}
	var res sqlengine.Result
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		t.Fatalf("the body is not a result: %v", err)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("rows = %v", res.Rows)
	}
	want := []any{"Infinity", "NaN", "-Infinity"}
	for i, w := range want {
		if res.Rows[0][i] != w {
			t.Errorf("column %d = %#v, want %#v", i, res.Rows[0][i], w)
		}
	}
}
