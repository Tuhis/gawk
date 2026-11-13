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
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/internal/roomsrv"
)

// RoomStatsSource is the /statusz rooms section (R42, docs/44 §4.10): rows
// keyed by the per-process HMAC of the code, never the code. An interface so
// ops keeps its one-way dependency shape; roomsrv.Registry implements it.
type RoomStatsSource interface {
	Stats() map[string]roomsrv.RoomStats
}

// statusz is the /statusz document: the hub snapshot, flattened as before,
// plus the rooms section — omitted entirely with -rooms off, so the
// pre-R42 document is byte-identical (docs/44 D17).
type statusz struct {
	hub.RegistryStats
	Rooms map[string]roomsrv.RoomStats `json:"rooms,omitempty"`
}

// StatuszHandler serves the registry snapshot as JSON, built fresh for each
// request. It is the ops listener's handler; the public H3 route serves the
// same document through CachedStatuszHandler. rooms may be nil (no rooms
// section).
func StatuszHandler(r *hub.Registry, rooms RoomStatsSource, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		doc := statusz{RegistryStats: r.Stats()}
		if rooms != nil {
			doc.Rooms = rooms.Stats()
		}
		if err := json.NewEncoder(w).Encode(doc); err != nil {
			log.Warn("statusz encode failed", "err", err)
		}
	}
}

// PublicStatuszMaxAge is how long the public H3 route serves one snapshot.
// That route is unauthenticated, and a snapshot walks every broadcast and
// subscriber under Registry.mu, the lock every relayed datagram takes, so
// without a bound anyone could stall fan-out by polling it. One second keeps
// the document useful for a human watching counters and makes a flood cost
// one snapshot per second however many requests arrive.
const PublicStatuszMaxAge = time.Second

// CachedStatuszHandler is StatuszHandler for a listener strangers can reach:
// it builds at most one snapshot per maxAge and serves the stored bytes to
// everyone else. Requests that arrive while a snapshot is being built wait
// for it instead of building their own. The document is the same one
// StatuszHandler writes, up to maxAge old.
func CachedStatuszHandler(r *hub.Registry, rooms RoomStatsSource, log *slog.Logger, maxAge time.Duration) http.HandlerFunc {
	return cachedStatuszHandler(r, rooms, log, maxAge, time.Now)
}

func cachedStatuszHandler(r *hub.Registry, rooms RoomStatsSource, log *slog.Logger, maxAge time.Duration, now func() time.Time) http.HandlerFunc {
	var (
		mu    sync.Mutex // held across a build, so concurrent requests share it
		body  []byte
		built time.Time
	)
	snapshot := func() ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		if t := now(); body == nil || t.Sub(built) >= maxAge {
			doc := statusz{RegistryStats: r.Stats()}
			if rooms != nil {
				doc.Rooms = rooms.Stats()
			}
			b, err := json.Marshal(doc)
			if err != nil {
				return nil, err
			}
			// json.Encoder, which StatuszHandler uses, ends with a newline.
			body, built = append(b, '\n'), t
		}
		return body, nil
	}
	return func(w http.ResponseWriter, _ *http.Request) {
		b, err := snapshot()
		if err != nil {
			log.Warn("statusz encode failed", "err", err)
			http.Error(w, "statusz unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(b)
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
//
// screening adds R40's keyframe route under its own token (docs/71 D1).
// Nil — or an empty token — registers nothing: the path 404s.
// extra adds routes only the ops listener serves, unauthenticated like
// /statusz there: R77's /internal/telemetry/pops (docs/78 D7).
func Handler(r *hub.Registry, rooms RoomStatsSource, g prometheus.Gatherer, log *slog.Logger, ready func() bool, admin *AdminOptions, screening *ScreeningOptions, extra ...Route) http.Handler {
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
	mux.HandleFunc("GET /statusz", StatuszHandler(r, rooms, log))
	mux.Handle("GET /metrics", promhttp.HandlerFor(g, promhttp.HandlerOpts{}))
	if registerAdmin(mux, admin) {
		log.Info("relay admin API enabled on the ops listener",
			"routes", "/internal/admin/broadcasts,/internal/admin/config,/internal/admin/rooms,/internal/admin/pops",
			"static_token", admin.Auth.token != nil,
			"oidc_issuer", admin.Config.AdminOIDCIssuer)
	}
	if registerScreening(mux, screening) {
		log.Info("relay screening keyframe route enabled on the ops listener",
			"route", "/internal/screening/broadcasts/{id}/keyframe")
	}
	for _, rt := range extra {
		mux.Handle(rt.Pattern, rt.Handler)
	}
	return mux
}

// Route is one extra ops-only route (Handler's extra).
type Route struct {
	Pattern string
	Handler http.Handler
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
