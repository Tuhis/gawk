// Package metrics holds gawk-mmproxy's counters and gauges and writes them
// in the Prometheus text format (docs/92 §5.1).
//
// No label ever takes an address (docs/92 D10): the only labels are the
// fixed Result and Pool sets below, typed so nothing else can be passed.
package metrics

import (
	"fmt"
	"io"
	"sync/atomic"
)

// Result is a connection's single outcome (docs/92 D6: each accepted
// connection is counted exactly once, under its first outcome).
type Result int

const (
	Proxied Result = iota
	BadHeader
	HeaderTimeout
	RefusedSource
	MurmurUnreachable
	NoRoutes
	OverCap
	Undrained
	numResults
)

var resultNames = [numResults]string{
	Proxied: "proxied", BadHeader: "bad_header", HeaderTimeout: "header_timeout",
	RefusedSource: "refused_source", MurmurUnreachable: "murmur_unreachable",
	NoRoutes: "no_routes", OverCap: "over_cap", Undrained: "undrained",
}

func (r Result) String() string { return resultNames[r] }

// Pool is where a refused connection drains.
type Pool int

const (
	Slots Pool = iota
	OverCapPool
	numPools
)

var poolNames = [numPools]string{Slots: "slots", OverCapPool: "over_cap"}

// Metrics is safe for concurrent use.
type Metrics struct {
	conns    [numResults]atomic.Uint64
	resets   atomic.Uint64
	active   atomic.Int64
	draining [numPools]atomic.Int64
	routesOK atomic.Bool
}

// New returns zeroed metrics.
func New() *Metrics { return &Metrics{} }

// Count records one connection's outcome.
func (m *Metrics) Count(r Result) { m.conns[r].Add(1) }

// Reset records a murmur reset passed through to the bridge.
func (m *Metrics) Reset() { m.resets.Add(1) }

// Active moves the active gauge (slot holders not draining).
func (m *Metrics) Active(delta int64) { m.active.Add(delta) }

// Draining moves a pool's draining gauge.
func (m *Metrics) Draining(p Pool, delta int64) { m.draining[p].Add(delta) }

// SetRoutesOK sets gawk_mmproxy_routes_ok.
func (m *Metrics) SetRoutesOK(ok bool) { m.routesOK.Store(ok) }

// Snapshot is a consistent-enough read for tests and /readyz.
type Snapshot struct {
	Conns    map[string]uint64
	Resets   uint64
	Active   int64
	Draining map[string]int64
	RoutesOK bool
}

// Snapshot reads every value.
func (m *Metrics) Snapshot() Snapshot {
	s := Snapshot{Conns: map[string]uint64{}, Draining: map[string]int64{}}
	for r := Result(0); r < numResults; r++ {
		s.Conns[r.String()] = m.conns[r].Load()
	}
	for p := Pool(0); p < numPools; p++ {
		s.Draining[poolNames[p]] = m.draining[p].Load()
	}
	s.Resets, s.Active, s.RoutesOK = m.resets.Load(), m.active.Load(), m.routesOK.Load()
	return s
}

// Write emits the Prometheus text exposition.
func (m *Metrics) Write(w io.Writer) error {
	b2i := func(b bool) int {
		if b {
			return 1
		}
		return 0
	}
	p := func(format string, a ...any) { fmt.Fprintf(w, format, a...) }
	p("# HELP gawk_mmproxy_connections_total Accepted connections by their one outcome (docs/92 D6).\n")
	p("# TYPE gawk_mmproxy_connections_total counter\n")
	for r := Result(0); r < numResults; r++ {
		p("gawk_mmproxy_connections_total{result=%q} %d\n", r.String(), m.conns[r].Load())
	}
	p("# HELP gawk_mmproxy_resets_total Murmur resets passed through to the bridge as resets.\n")
	p("# TYPE gawk_mmproxy_resets_total counter\n")
	p("gawk_mmproxy_resets_total %d\n", m.resets.Load())
	p("# HELP gawk_mmproxy_active Slot holders not draining: waiting for a header, dialling murmur or proxied.\n")
	p("# TYPE gawk_mmproxy_active gauge\n")
	p("gawk_mmproxy_active %d\n", m.active.Load())
	p("# HELP gawk_mmproxy_draining Connections draining after a refusal, by pool.\n")
	p("# TYPE gawk_mmproxy_draining gauge\n")
	for q := Pool(0); q < numPools; q++ {
		p("gawk_mmproxy_draining{pool=%q} %d\n", poolNames[q], m.draining[q].Load())
	}
	p("# HELP gawk_mmproxy_routes_ok Whether the IPv4 loopback policy routes are in place (docs/92 D5).\n")
	p("# TYPE gawk_mmproxy_routes_ok gauge\n")
	_, err := fmt.Fprintf(w, "gawk_mmproxy_routes_ok %d\n", b2i(m.routesOK.Load()))
	return err
}
