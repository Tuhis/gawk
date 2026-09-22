// Package opsmetrics is gawk-telemetry's Prometheus endpoint: the few facts
// about the service's own health that an alert can watch.
//
// It exists because the SQL console broke in production twice and was found
// weeks later each time, by an operator who happened to reach for it
// (BUGS.md). The read listener is behind basic auth and routed to operators,
// so it is the wrong place for a scrape target; this is served on its own
// ClusterIP-only listener, like the relay's ops port, and carries no
// broadcast, session or room identifiers — nothing here is worth hiding.
//
// The exposition format is written by hand. It is five gauges, and the module
// otherwise carries no metrics dependency; client_golang would be most of its
// dependency graph for that.
package opsmetrics

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Tuhis/gawk/gawk-telemetry/internal/sqlengine"
)

// Registry holds the current values.
type Registry struct {
	version string
	// sqlExpected is whether -query-sql is on. Only then is the engine's
	// absence a fault worth a gauge: a deployment that turned the console off
	// must not export an "engine down" an alert would fire on.
	sqlExpected bool

	mu       sync.Mutex
	sqlUp    bool
	views    []sqlengine.ViewHealth
	probedAt time.Time
}

// New builds a registry.
func New(version string, sqlExpected bool) *Registry {
	return &Registry{version: version, sqlExpected: sqlExpected}
}

// SetSQLEngine records whether an engine is wired.
func (r *Registry) SetSQLEngine(up bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sqlUp = up
}

// SetViews records one probe round.
func (r *Registry) SetViews(views []sqlengine.ViewHealth, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.views = append([]sqlengine.ViewHealth(nil), views...)
	r.probedAt = at
}

// Handler serves the exposition.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(r.render()))
	})
}

func (r *Registry) render() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var b strings.Builder
	gauge := func(name, help string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name)
	}
	gauge("gawk_telemetry_build_info", "The running version; always 1.")
	fmt.Fprintf(&b, "gawk_telemetry_build_info{version=%q} 1\n", r.version)

	if !r.sqlExpected {
		return b.String()
	}
	gauge("gawk_telemetry_sql_engine_up", "1 when the SQL console has an engine; 0 when -query-sql is on but none is wired.")
	fmt.Fprintf(&b, "gawk_telemetry_sql_engine_up %d\n", boolInt(r.sqlUp))
	if r.probedAt.IsZero() {
		return b.String()
	}

	views := append([]sqlengine.ViewHealth(nil), r.views...)
	sort.Slice(views, func(i, j int) bool { return views[i].Name < views[j].Name })
	gauge("gawk_telemetry_sql_view_up", "0 when a SQL view has data on disk and the probe query on it failed; 1 otherwise.")
	for _, v := range views {
		fmt.Fprintf(&b, "gawk_telemetry_sql_view_up{view=%q} %d\n", v.Name, boolInt(v.OK))
	}
	gauge("gawk_telemetry_sql_view_has_data", "1 when a SQL view's partition tree holds at least one file.")
	for _, v := range views {
		fmt.Fprintf(&b, "gawk_telemetry_sql_view_has_data{view=%q} %d\n", v.Name, boolInt(v.HasData))
	}
	gauge("gawk_telemetry_sql_probe_last_run_timestamp_seconds", "When the SQL view probe last completed.")
	fmt.Fprintf(&b, "gawk_telemetry_sql_probe_last_run_timestamp_seconds %d\n", r.probedAt.Unix())
	return b.String()
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
