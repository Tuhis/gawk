// Package server is the sampler's one listener, -metrics-addr: Prometheus
// /metrics (operational metrics only, docs/71 §5), /healthz and /readyz. It is
// never routed; the chart reaches it from inside the cluster only. Nothing it
// serves carries a frame, a broadcast ID or a key (D8).
package server

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Ready is the readiness gate: nil until the model is loaded and ffmpeg is
// found, then the reason it is not ready, or ok.
type Ready struct {
	reason atomic.Pointer[string]
}

// NewReady starts not ready, for reason.
func NewReady(reason string) *Ready {
	r := &Ready{}
	r.reason.Store(&reason)
	return r
}

// Set marks the process ready.
func (r *Ready) Set() { r.reason.Store(nil) }

// NotReady reports why the process is not ready, or "" when it is.
func (r *Ready) NotReady() string {
	if p := r.reason.Load(); p != nil {
		return *p
	}
	return ""
}

// Handler serves /metrics from g and the two probes.
func Handler(g prometheus.Gatherer, ready *Ready) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if why := ready.NotReady(); why != "" {
			http.Error(w, why, http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok"))
	})
	mux.Handle("GET /metrics", promhttp.HandlerFor(g, promhttp.HandlerOpts{}))
	return mux
}

// Serve serves h on l until ctx ends.
func Serve(ctx context.Context, l net.Listener, h http.Handler, log *slog.Logger) error {
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(l) }()
	log.Info("metrics listener up", "addr", l.Addr().String())
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		<-errCh
		return nil
	case err := <-errCh:
		return fmt.Errorf("metrics listener: %w", err)
	}
}
