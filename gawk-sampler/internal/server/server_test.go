package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func TestProbes(t *testing.T) {
	reg := prometheus.NewRegistry()
	g := prometheus.NewGauge(prometheus.GaugeOpts{Name: "gawk_sampler_test", Help: "t"})
	reg.MustRegister(g)
	ready := NewReady("model not loaded")
	srv := httptest.NewServer(Handler(reg, ready))
	defer srv.Close()

	get := func(path string) (int, string) {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var b strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := resp.Body.Read(buf)
			b.Write(buf[:n])
			if err != nil {
				break
			}
		}
		return resp.StatusCode, b.String()
	}
	if code, _ := get("/healthz"); code != 200 {
		t.Fatalf("/healthz %d", code)
	}
	if code, body := get("/readyz"); code != 503 || !strings.Contains(body, "model not loaded") {
		t.Fatalf("/readyz before ready: %d %q", code, body)
	}
	ready.Set()
	if code, _ := get("/readyz"); code != 200 {
		t.Fatalf("/readyz after ready: %d", code)
	}
	if code, body := get("/metrics"); code != 200 || !strings.Contains(body, "gawk_sampler_test") {
		t.Fatalf("/metrics: %d", code)
	}
	if code, _ := get("/debug/pprof/"); code != 404 {
		t.Fatalf("an unexpected route answered: %d", code)
	}
}
