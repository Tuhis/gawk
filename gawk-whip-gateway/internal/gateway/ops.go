package gateway

import (
	"io"
	"net/http"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// OpsHandler serves /healthz, /readyz and /metrics on the ops listener,
// which is never routed publicly.
func (s *Server) OpsHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok\n") })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		closed := s.closed
		s.mu.Unlock()
		if closed {
			http.Error(w, "shutting down", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.Handle("GET /metrics", promhttp.HandlerFor(s.m.Registry, promhttp.HandlerOpts{}))
	return mux
}
