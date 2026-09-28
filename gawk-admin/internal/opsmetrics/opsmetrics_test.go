package opsmetrics

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func scrape(t *testing.T, m *Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body, _ := io.ReadAll(rec.Body)
	return string(body)
}

// The route label is the pattern that served the request, never the path:
// the path carries IDs (a ban, a room code) and would be an unbounded label.
// A nested mux is how the portal mounts /api/v1, so the innermost pattern
// must be the one that lands.
func TestMiddlewareLabelsByPatternNotPath(t *testing.T) {
	m := New("test")
	api := http.NewServeMux()
	api.HandleFunc("GET /api/v1/bans/{id}", func(w http.ResponseWriter, r *http.Request) {})
	api.HandleFunc("/api/v1/", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })
	outer := http.NewServeMux()
	outer.Handle("/api/v1/", api)
	outer.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {})
	h := m.Middleware(outer)

	for _, path := range []string{"/api/v1/bans/ZXQ7K2", "/api/v1/bans/K7XQ2M", "/api/v1/nope", "/healthz"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
	}
	// A POST to a GET-only path is answered by the mux itself (405) without
	// a matched pattern.
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/healthz", nil))

	want := map[[2]string]float64{
		{"GET /api/v1/bans/{id}", "2xx"}: 2,
		{"/api/v1/", "4xx"}:              1,
		{"GET /healthz", "2xx"}:          1,
		{"none", "4xx"}:                  1,
	}
	for k, v := range want {
		if got := testutil.ToFloat64(m.requests.WithLabelValues(k[0], k[1])); got != v {
			t.Errorf("requests{route=%q,code=%q} = %v, want %v", k[0], k[1], got, v)
		}
	}
	if body := scrape(t, m); strings.Contains(body, "ZXQ7K2") || strings.Contains(body, "K7XQ2M") {
		t.Error("a request path's ID reached /metrics")
	}
}

func TestHandlerServesBuildInfoAndOutcomes(t *testing.T) {
	m := New("1.2.3")
	m.Event("stored")
	m.Event("stored")
	m.Delivery("retry")
	body := scrape(t, m)
	for _, want := range []string{
		`gawk_admin_build_info{version="1.2.3"} 1`,
		`gawk_admin_events_ingested_total{outcome="stored"} 2`,
		`gawk_admin_webhook_deliveries_total{outcome="retry"} 1`,
		"go_goroutines",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics lacks %q", want)
		}
	}
}

// A pool that has never connected still reports its limits.
func TestPoolCollector(t *testing.T) {
	pool, err := pgxpool.New(context.Background(), "postgres://nobody@127.0.0.1:1/none?pool_max_conns=4")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	m := New("test")
	m.RegisterPool(pool)
	body := scrape(t, m)
	for _, want := range []string{"gawk_admin_db_pool_max_conns 4", "gawk_admin_db_pool_acquired_conns 0", "gawk_admin_db_pool_acquire_wait_seconds_total 0"} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics lacks %q", want)
		}
	}
}

// With the listener off the portal holds a nil *Metrics: every call is a
// no-op and the middleware adds nothing.
func TestNilMetricsIsInert(t *testing.T) {
	var m *Metrics
	m.Event("stored")
	m.Delivery("delivered")
	m.RegisterPool(nil)
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	if h := m.Middleware(next); h == nil {
		t.Fatal("nil Middleware returned nil")
	}
}
