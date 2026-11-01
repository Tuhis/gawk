package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The ingest listener is bound before the history repair runs, so the
// liveness probe (httpGet /healthz on the ingest port, no startupProbe) sees
// a live process for however long the repair takes. Until the real handler
// is installed, everything except /healthz answers 503 with Retry-After: no
// batch can reach a store that is not open yet.
func TestStartGateServesHealthzUntilOpened(t *testing.T) {
	g := newStartGate()
	get := func(method, path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		g.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
		return rec
	}

	if rec := get(http.MethodGet, "/healthz"); rec.Code != http.StatusOK {
		t.Errorf("/healthz during the repair = %d, want 200 (or liveness kills the pod)", rec.Code)
	}
	for _, p := range []string{"/api/telemetry/v1/ingest", "/v1/ingest", "/anything"} {
		rec := get(http.MethodPost, p)
		if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
			t.Errorf("POST %s during the repair = %d (Retry-After %q), want 503 with Retry-After",
				p, rec.Code, rec.Header().Get("Retry-After"))
		}
	}

	real := http.NewServeMux()
	real.HandleFunc("/v1/ingest", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusAccepted) })
	real.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) })
	g.open(real)
	if rec := get(http.MethodPost, "/v1/ingest"); rec.Code != http.StatusAccepted {
		t.Errorf("after open, /v1/ingest = %d, want the real handler's 202", rec.Code)
	}
	if rec := get(http.MethodGet, "/healthz"); rec.Code != http.StatusTeapot {
		t.Errorf("after open, /healthz = %d, want the real handler", rec.Code)
	}
}
