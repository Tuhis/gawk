package ops

// The R39 relay admin API (docs/42 §4.5): two READ-ONLY routes on the ops
// listener, live only when a credential is configured.
//
// KILL AND BAN VERBS ARE DELIBERATELY ABSENT, and must stay absent. The Ban
// CR is the single write path into enforcement (docs/42 D2): relays act on
// what the CRD says, `kubectl apply` is the auth-free break-glass, and
// gawk-admin's Postgres is the system of record behind both. A second write
// path here would eventually disagree with that one, and the disagreement
// would be invisible until an operator needed it not to be.
//
// These are also the only responses in the relay that carry RAW broadcast IDs
// (docs/42 D8): this listener is ClusterIP-only AND credential-gated, which
// is strictly more protected than /statusz — whose HMAC-only shape is
// unchanged and asserted byte-identical in the tests.

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/netip"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/hub"
)

// Schema names. Versioned strings, not implied by the route, so a consumer
// pins the shape it parses (docs/42 §4.5) — the same discipline the telemetry
// ingest uses.
const (
	SchemaAdminBroadcasts = "gawk.admin.broadcasts.v1"
	SchemaAdminConfig     = "gawk.admin.config.v1"
)

// AdminOptions wires the admin routes. A nil *AdminOptions — or one whose
// Auth reports no configured credential — leaves the routes UNREGISTERED, so
// they 404 from the mux rather than 401ing: an unconfigured surface must be
// indistinguishable from a relay that predates R39.
type AdminOptions struct {
	// Registry supplies AdminStats.
	Registry *hub.Registry
	// Config is reported (redacted) by GET /internal/admin/config.
	Config config.Config
	// Pod names this pod in every response — the portal's per-pod fleet view.
	Pod string
	// Version is the ldflags-stamped build version.
	Version string
	// PublisherRemote maps a broadcast ID to its live publisher's source
	// address. Supplied by the transport (which owns session bookkeeping);
	// nil simply omits the field.
	PublisherRemote func(id string) (netip.Addr, bool)
	// Auth gates both routes.
	Auth *AdminAuth
	Log  *slog.Logger
}

type adminBroadcastsResponse struct {
	Schema     string               `json:"schema"`
	Pod        string               `json:"pod"`
	Broadcasts []hub.AdminBroadcast `json:"broadcasts"`
}

type adminConfigResponse struct {
	Schema  string                 `json:"schema"`
	Pod     string                 `json:"pod"`
	Version string                 `json:"version"`
	Config  config.SanitizedConfig `json:"config"`
}

// registerAdmin adds the admin routes to mux when a credential is configured.
// It reports whether it registered anything.
func registerAdmin(mux *http.ServeMux, opts *AdminOptions) bool {
	if opts == nil || !opts.Auth.Configured() || opts.Registry == nil {
		return false
	}
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	mux.Handle("GET /internal/admin/broadcasts",
		opts.guard(log, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			rows := opts.Registry.AdminStats()
			if opts.PublisherRemote != nil {
				for i := range rows {
					if addr, ok := opts.PublisherRemote(rows[i].ID); ok {
						rows[i].PublisherRemoteIP = addr.String()
					}
				}
			}
			writeAdminJSON(w, log, adminBroadcastsResponse{
				Schema: SchemaAdminBroadcasts, Pod: opts.Pod, Broadcasts: rows,
			})
		})))
	mux.Handle("GET /internal/admin/config",
		opts.guard(log, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeAdminJSON(w, log, adminConfigResponse{
				Schema: SchemaAdminConfig, Pod: opts.Pod, Version: opts.Version,
				Config: opts.Config.Sanitized(),
			})
		})))
	return true
}

// guard applies the bearer check. The response body says only "unauthorized"
// or "forbidden": which credential failed, and why, is Debug-level detail for
// the operator reading logs, not a hint for whoever is probing.
func (o *AdminOptions) guard(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status, reason := o.Auth.authorize(r)
		if status == http.StatusOK {
			next.ServeHTTP(w, r)
			return
		}
		log.Debug("admin api request rejected",
			"path", r.URL.Path, "remote", r.RemoteAddr, "status", status, "reason", reason)
		w.Header().Set("WWW-Authenticate", `Bearer realm="gawk-admin"`)
		http.Error(w, http.StatusText(status), status)
	})
}

func writeAdminJSON(w http.ResponseWriter, log *slog.Logger, v any) {
	w.Header().Set("Content-Type", "application/json")
	// The response carries raw broadcast IDs — joinable capabilities. No
	// intermediary may hold one.
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Warn("admin api encode failed", "err", err)
	}
}
