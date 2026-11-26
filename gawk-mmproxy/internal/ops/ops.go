// Package ops serves gawk-mmproxy's ops listener: /healthz, /readyz and
// /metrics (docs/92 D7, §5.1). It is never routed publicly, and none of it
// is a Kubernetes probe of murmur's pod (docs/92 D15): /readyz is for
// operators and alerts.
package ops

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/Tuhis/gawk/gawk-mmproxy/internal/listeners"
	"github.com/Tuhis/gawk/gawk-mmproxy/internal/metrics"
	"github.com/Tuhis/gawk/gawk-mmproxy/internal/routes"
)

// Options configure the ops listener.
type Options struct {
	Listen        string
	MurmurPort    uint16
	RequireRoutes bool

	// Routes is the proxy's last routes check.
	Routes func() (routes.State, error)
	// Reachable replaces listeners.Reachable (tests).
	Reachable func(port uint16) (listeners.Reach, error)
}

// Ready applies D7: the routes the proxy needs are in place, and murmur has
// a listener D3's loopback dial reaches, per family. The reason says which
// check failed.
func Ready(o Options) (bool, string) {
	st, err := o.Routes()
	if o.RequireRoutes {
		if err != nil {
			return false, fmt.Sprintf("can't read the loopback routes: %v", err)
		}
		if !st.V4 {
			return false, "the loopback policy routes are missing (did the routes init container run with NET_ADMIN?)"
		}
	}
	reach := o.Reachable
	if reach == nil {
		reach = listeners.Reachable
	}
	r, err := reach(o.MurmurPort)
	if errors.Is(err, listeners.ErrUnavailable) {
		return false, fmt.Sprintf("the kernel refused the inet_diag dump, so murmur's listener can't be checked: %v", err)
	}
	if err != nil {
		return false, err.Error()
	}
	if !r.V4 {
		return false, fmt.Sprintf("murmur has no listener on port %d that 127.0.0.1 reaches (0.0.0.0, 127.0.0.1 or a dual-stack ::)", o.MurmurPort)
	}
	if st.V6 && !r.V6 {
		return false, fmt.Sprintf("the IPv6 routes are in place but murmur has no listener on port %d that ::1 reaches (:: or ::1)", o.MurmurPort)
	}
	return true, "ready"
}

// Handler serves the three routes.
func Handler(o Options, m *metrics.Metrics) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		ok, why := Ready(o)
		if !ok {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		fmt.Fprintln(w, why)
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_ = m.Write(w)
	})
	return mux
}

// Serve runs the ops listener until ctx ends.
func Serve(ctx context.Context, ln net.Listener, h http.Handler) error {
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
