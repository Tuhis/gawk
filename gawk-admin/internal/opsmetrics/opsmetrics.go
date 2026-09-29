// Package opsmetrics is gawk-admin's own health in Prometheus format (R59,
// docs/61 D9), served on a listener of its own that no Ingress routes.
//
// Nothing here carries a broadcast ID, a room code, an IP or a user: route
// labels are the net/http patterns the portal registers (a fixed set), and
// status codes are folded into classes.
package opsmetrics

import (
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics owns the registry and the counters the portal's parts report into.
// Every method is nil-receiver-safe, so a part built without metrics (tests,
// a disabled listener) records nothing.
type Metrics struct {
	reg        *prometheus.Registry
	requests   *prometheus.CounterVec
	events     *prometheus.CounterVec
	deliveries *prometheus.CounterVec
}

// New builds the registry: Go and process collectors, the build info, and the
// portal's counters.
func New(version string) *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	build := prometheus.NewGauge(prometheus.GaugeOpts{
		Name:        "gawk_admin_build_info",
		Help:        "The running version; always 1.",
		ConstLabels: prometheus.Labels{"version": version},
	})
	build.Set(1)
	m := &Metrics{
		reg: reg,
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gawk_admin_http_requests_total",
			Help: "Portal requests by the net/http pattern that served them and the status class.",
		}, []string{"route", "code"}),
		events: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gawk_admin_events_ingested_total",
			Help: "Relay event-bus messages by outcome: stored, live (a delta for the live view), redeliver (ingest failed, left for redelivery), undecodable.",
		}, []string{"outcome"}),
		deliveries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gawk_admin_webhook_deliveries_total",
			Help: "Webhook delivery attempts by outcome: delivered, retry (scheduled on the ladder), failed (given up).",
		}, []string{"outcome"}),
	}
	reg.MustRegister(build, m.requests, m.events, m.deliveries)
	return m
}

// Handler serves the registry.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}

// RegisterPool exports a pgx pool's statistics.
func (m *Metrics) RegisterPool(p *pgxpool.Pool) {
	if m == nil || p == nil {
		return
	}
	m.reg.MustRegister(newPoolCollector(p.Stat))
}

// Event records one bus message's outcome.
func (m *Metrics) Event(outcome string) {
	if m == nil {
		return
	}
	m.events.WithLabelValues(outcome).Inc()
}

// Delivery records one webhook delivery attempt's outcome.
func (m *Metrics) Delivery(outcome string) {
	if m == nil {
		return
	}
	m.deliveries.WithLabelValues(outcome).Inc()
}

// Middleware counts every request by the pattern that served it. It must wrap
// the mux itself: ServeMux records the matched pattern on the request it was
// handed, and a nested mux (the /api/v1 routes) overwrites it with the more
// specific one, so after the handler returns r.Pattern is the innermost route.
// An unmatched request has no pattern and is labelled "none".
func (m *Metrics) Middleware(next http.Handler) http.Handler {
	if m == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		route := r.Pattern
		if route == "" {
			route = "none"
		}
		m.requests.WithLabelValues(route, strconv.Itoa(sw.status/100)+"xx").Inc()
	})
}

type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.status = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach the real writer.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// poolCollector snapshots pgxpool.Stat() per scrape.
type poolCollector struct {
	stat                                        func() *pgxpool.Stat
	acquired, idle, total, max                  *prometheus.Desc
	acquires, emptyAcquires, acquireWaitSeconds *prometheus.Desc
}

func newPoolCollector(stat func() *pgxpool.Stat) *poolCollector {
	d := func(name, help string) *prometheus.Desc {
		return prometheus.NewDesc("gawk_admin_db_pool_"+name, help, nil, nil)
	}
	return &poolCollector{
		stat:               stat,
		acquired:           d("acquired_conns", "Connections currently checked out."),
		idle:               d("idle_conns", "Idle connections in the pool."),
		total:              d("total_conns", "Connections the pool holds (acquired, idle and constructing)."),
		max:                d("max_conns", "The pool's size limit."),
		acquires:           d("acquires_total", "Successful connection acquires."),
		emptyAcquires:      d("empty_acquires_total", "Acquires that had to wait because no connection was idle."),
		acquireWaitSeconds: d("acquire_wait_seconds_total", "Total time spent waiting to acquire a connection."),
	}
}

func (c *poolCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{c.acquired, c.idle, c.total, c.max, c.acquires, c.emptyAcquires, c.acquireWaitSeconds} {
		ch <- d
	}
}

func (c *poolCollector) Collect(ch chan<- prometheus.Metric) {
	s := c.stat()
	g := func(d *prometheus.Desc, v float64) { ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v) }
	k := func(d *prometheus.Desc, v float64) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.CounterValue, v)
	}
	g(c.acquired, float64(s.AcquiredConns()))
	g(c.idle, float64(s.IdleConns()))
	g(c.total, float64(s.TotalConns()))
	g(c.max, float64(s.MaxConns()))
	k(c.acquires, float64(s.AcquireCount()))
	k(c.emptyAcquires, float64(s.EmptyAcquireCount()))
	k(c.acquireWaitSeconds, s.AcquireDuration().Seconds())
}
