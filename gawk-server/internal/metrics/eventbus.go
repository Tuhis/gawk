package metrics

import "github.com/prometheus/client_golang/prometheus"

// EventBusMetrics is the relay's honest signal about the R50 event bus
// (docs/51 D1, §6). The bus is telemetry about the deployment, so it is
// governed by the relay's first rule — drop rather than stall — and every drop
// is counted with a reason instead of retried. "Drops climbing" is a NATS
// problem; nothing on the media path waits for it.
type EventBusMetrics struct {
	published prometheus.Counter
	dropped   *prometheus.CounterVec
}

// Drop reasons. A new one is cheap; conflating two is not.
const (
	// DropQueueFull: the bounded channel was full when a hook fired. The
	// publisher is slower than the transitions, or NATS is unreachable and
	// the drain goroutine is blocked on backpressure.
	DropQueueFull = "queue_full"
	// DropPublish: the async publish itself failed, including "no response
	// from stream" — which is what a relay publishing before gawk-admin has
	// created GAWK_EVENTS looks like (docs/51 §6: order does not matter).
	DropPublish = "publish"
	// DropEncode: the event could not be marshalled. A bug, not an operational
	// condition; counted rather than panicking, because a malformed event must
	// not take the relay down.
	DropEncode = "encode"
)

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
