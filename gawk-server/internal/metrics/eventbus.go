package metrics

import "github.com/prometheus/client_golang/prometheus"

// EventBusMetrics is the relay's honest signal about the R50 event bus
// (docs/51 D1, §6). It satisfies eventbus.Metrics; the drop REASONS are
// declared there, beside the code that decides them, because internal/eventbus
// must not import this package (metrics imports roomsrv, and roomsrv hands the
// bus its events).
//
// The bus is telemetry about the deployment, so it is
// governed by the relay's first rule — drop rather than stall — and every drop
// is counted with a reason instead of retried. "Drops climbing" is a NATS
// problem; nothing on the media path waits for it.
type EventBusMetrics struct {
	published prometheus.Counter
	dropped   *prometheus.CounterVec
}

// NewEventBusMetrics builds and registers the bus counters. They are
// registered even when the bus is off, so an operator can tell "configured and
// silent" from "not configured".
func NewEventBusMetrics(reg prometheus.Registerer) *EventBusMetrics {
	m := &EventBusMetrics{
		published: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "gawk_eventbus_published_total",
			Help: "Events this pod handed to JetStream (R50).",
		}),
		dropped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gawk_eventbus_dropped_total",
			Help: "Events this pod dropped rather than delaying the media path, by reason (R50).",
		}, []string{"reason"}),
	}
	reg.MustRegister(m.published, m.dropped)
	return m
}

// Published records one event handed to JetStream.
func (m *EventBusMetrics) Published() {
	if m == nil {
		return
	}
	m.published.Inc()
}

// Dropped records one event that never reached the bus.
func (m *EventBusMetrics) Dropped(reason string) {
	if m == nil {
		return
	}
	m.dropped.WithLabelValues(reason).Inc()
}
