// Package ops serves the plain-TCP HTTP operations endpoint (R9 M1,
// docs/13): /metrics (Prometheus), /healthz, and /statusz. It exists because
// the WebTransport server is HTTP/3-over-UDP only — Prometheus and curl need
// a TCP listener. The listener must never be exposed publicly; in the Helm
// chart it is reachable only through a ClusterIP Service.
package ops

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/Tuhis/gawk/gawk-server/internal/hub"
)

// StatuszHandler serves the registry snapshot as JSON — the single definition
// shared by the H3 route (transport) and the TCP ops endpoint, so the two can
// never drift.
func StatuszHandler(r *hub.Registry, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(r.Stats()); err != nil {
			log.Warn("statusz encode failed", "err", err)
		}
	}
}

// Handler builds the ops mux. ready reports whether the relay is accepting
// new work (false once the SIGTERM drain has begun — R17 W1); nil means
// always ready. /readyz is the kubelet readiness target and matters for
// scale-down/HPA hygiene; rollout correctness comes from the active drain,
// not from readiness (docs/22 Decision 2).
//
// admin adds the R39 credential-gated /internal/admin/* routes (docs/42
// §4.5). Nil — or an admin with no credential configured — registers nothing,
// so those paths 404 exactly as they did before R39. Everything else on this
// mux is unauthenticated and unchanged: /statusz in particular stays
// HMAC-only and byte-identical.
func Handler(r *hub.Registry, g prometheus.Gatherer, log *slog.Logger, ready func() bool, admin *AdminOptions) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if ready != nil && !ready() {
			http.Error(w, "draining", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /statusz", StatuszHandler(r, log))
	mux.Handle("GET /metrics", promhttp.HandlerFor(g, promhttp.HandlerOpts{}))
	if registerAdmin(mux, admin) {
		log.Info("relay admin API enabled on the ops listener",
			"routes", "/internal/admin/broadcasts,/internal/admin/config",
			"static_token", admin.Auth.token != nil,
			"oidc_issuer", admin.Config.AdminOIDCIssuer)
	}
	return mux
}

// Run listens on addr and serves h until ctx is cancelled. An empty addr
// means the ops endpoint is disabled: Run returns nil immediately.
func Run(ctx context.Context, addr string, h http.Handler, log *slog.Logger) error {
	if addr == "" {
		log.Info("ops endpoint disabled")
		return nil
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("ops listen %s: %w", addr, err)
	}
	return Serve(ctx, l, h, log)
}

// Serve serves h on l until ctx is cancelled (split from Run so tests can
// pass an ephemeral-port listener). It returns nil after a graceful shutdown.
func Serve(ctx context.Context, l net.Listener, h http.Handler, log *slog.Logger) error {
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(l) }()

	log.Info("ops endpoint listening", "addr", l.Addr().String())
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		<-errCh // always http.ErrServerClosed after Shutdown
		return nil
	case err := <-errCh:
		return err
	}
}
