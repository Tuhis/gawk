package metrics_test

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Tuhis/gawk/gawk-server/internal/metrics"
)

// R50's headline promise is that a deployment without a bus is byte-identical
// to one predating it, and EB1's acceptance criteria name /metrics alongside
// /statusz — so the bus counters exist only when a bus does. main decides
// that (see TestEventBusMetricsAreOnlyBuiltWhenTheBusIs); this is the other
// half: a registry nothing registered them into carries no series, and the
// ones registered carry the documented names.
func TestEventBusCountersAppearOnlyWhenRegistered(t *testing.T) {
	off := prometheus.NewRegistry()
	if got := testutil.CollectAndCount(off); got != 0 {
		t.Fatalf("an unregistered bus contributed %d series", got)
	}

	on := prometheus.NewRegistry()
	m := metrics.NewEventBusMetrics(on)
	m.Published()
	m.Dropped("queue_full")

	const want = `# HELP gawk_eventbus_dropped_total Events this pod dropped rather than delaying the media path, by reason (R50).
# TYPE gawk_eventbus_dropped_total counter
gawk_eventbus_dropped_total{reason="queue_full"} 1
# HELP gawk_eventbus_published_total Events this pod handed to JetStream (R50).
# TYPE gawk_eventbus_published_total counter
gawk_eventbus_published_total 1
`
	if err := testutil.GatherAndCompare(on, strings.NewReader(want),
		"gawk_eventbus_published_total", "gawk_eventbus_dropped_total"); err != nil {
		t.Error(err)
	}
}

// A nil *EventBusMetrics is the shape the publisher holds when nothing is
// counting, and every method has to tolerate it: the bus is telemetry about
// the deployment, and telemetry must not be able to panic the relay.
func TestEventBusMetricsAreNilSafe(t *testing.T) {
	var m *metrics.EventBusMetrics
	m.Published()
	m.Dropped("publish")
}
