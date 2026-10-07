package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/Tuhis/gawk/gawk-server/internal/clientinfo"
	"github.com/Tuhis/gawk/gawk-server/internal/hub"
)

// R59 usage series (docs/61 §4). The transport records sessions as they
// start and end; the hub reports an origin broadcast's lifetime when its hub
// is removed (hub.Options.OnOriginEnded).

// Broadcast start kinds (docs/61 D3) and viewer join kinds (D4).
const (
	BroadcastNew     = "new"
	BroadcastResumed = "resumed"
	JoinFirst        = "first"
	JoinRejoin       = "rejoin"
)

type usageMetrics struct {
	broadcastsStarted *prometheus.CounterVec
	viewerJoins       *prometheus.CounterVec
	viewerSession     *prometheus.HistogramVec
	broadcastDuration prometheus.Histogram
	broadcastPeak     prometheus.Histogram
	roomsMinted       prometheus.Counter
}

// durationBuckets span a one-minute test to an eight-hour session.
var durationBuckets = []float64{60, 300, 900, 1800, 3600, 7200, 14400, 28800}

func newUsageMetrics(reg prometheus.Registerer) usageMetrics {
	u := usageMetrics{
		broadcastsStarted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gawk_broadcasts_started_total",
			Help: "Accepted publish sessions: kind=new on the mint path, kind=resumed on the claim path (a reconnect, re-home or reclaim after a relay restart). Broadcasts started is kind=new.",
		}, []string{"kind", "app", "os", "browser"}),
		viewerJoins: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gawk_viewer_joins_total",
			Help: "Accepted viewer sessions, excluding R30 stripe legs and edge pulls; kind=rejoin is the web app's automatic reconnect.",
		}, []string{"kind", "delivery", "app", "os", "browser"}),
		viewerSession: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "gawk_viewer_session_seconds",
			Help:    "How long a counted viewer session lasted; a rejoin starts a new one.",
			Buckets: durationBuckets,
		}, []string{"delivery"}),
		broadcastDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "gawk_broadcast_duration_seconds",
			Help:    "An origin broadcast's life from hub creation to its publisher's last disconnect, observed when the hub is removed (grace excluded).",
			Buckets: durationBuckets,
		}),
		broadcastPeak: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "gawk_broadcast_peak_viewers",
			Help:    "The largest global viewer count an origin broadcast reached, observed when its hub is removed.",
			Buckets: []float64{0, 1, 2, 5, 10, 20, 50, 100, 200, 500, 1000},
		}),
		roomsMinted: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "gawk_rooms_minted_total",
			Help: "Dynamic rooms this pod minted (R42).",
		}),
	}
	reg.MustRegister(u.broadcastsStarted, u.viewerJoins, u.viewerSession,
		u.broadcastDuration, u.broadcastPeak, u.roomsMinted)
	return u
}

// BroadcastStarted records one accepted publish session.
func (m *ServerMetrics) BroadcastStarted(kind string, c clientinfo.Info) {
	if m == nil {
		return
	}
	m.usage.broadcastsStarted.WithLabelValues(kind, orUnknown(c.App), orUnknown(c.OS), orUnknown(c.Browser)).Inc()
}

// ViewerJoined records one counted viewer session.
func (m *ServerMetrics) ViewerJoined(kind, delivery string, c clientinfo.Info) {
	if m == nil {
		return
	}
	m.usage.viewerJoins.WithLabelValues(kind, delivery, orUnknown(c.App), orUnknown(c.OS), orUnknown(c.Browser)).Inc()
}

// ViewerLeft records how long a counted viewer session lasted.
func (m *ServerMetrics) ViewerLeft(delivery string, d time.Duration) {
	if m == nil {
		return
	}
	m.usage.viewerSession.WithLabelValues(delivery).Observe(d.Seconds())
}

// BroadcastEnded records an origin broadcast's lifetime and peak audience.
func (m *ServerMetrics) BroadcastEnded(e hub.BroadcastEnd) {
	if m == nil {
		return
	}
	m.usage.broadcastDuration.Observe(e.Duration.Seconds())
	m.usage.broadcastPeak.Observe(float64(e.PeakViewers))
}

// RoomMinted records one dynamic room mint.
func (m *ServerMetrics) RoomMinted() {
	if m == nil {
		return
	}
	m.usage.roomsMinted.Inc()
}

// orUnknown keeps a label value non-empty: a zero clientinfo.Info (a caller
// that never parsed one) reads as "unknown", never as a blank label.
func orUnknown(v string) string {
	if v == "" {
		return clientinfo.Unknown
	}
	return v
}

// Limits is the configured caps, exported as gawk_limit{name} (docs/61 D7).
// 0 means unlimited, as in the flags.
type Limits map[string]float64

// LimitsCollector exposes Limits as const gauges.
type LimitsCollector struct {
	desc   *prometheus.Desc
	limits Limits
}

// NewLimitsCollector builds the collector over a fixed set of caps.
func NewLimitsCollector(l Limits) *LimitsCollector {
	return &LimitsCollector{
		desc: prometheus.NewDesc("gawk_limit",
			"A configured cap (R59, docs/61 D7); 0 = unlimited. Per pod unless the name says otherwise.",
			[]string{"name"}, nil),
		limits: l,
	}
}

// Describe implements prometheus.Collector.
func (c *LimitsCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

// Collect implements prometheus.Collector.
func (c *LimitsCollector) Collect(ch chan<- prometheus.Metric) {
	for name, v := range c.limits {
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, v, name)
	}
}

// BroadcastsStartedCount reads back a started counter (test support).
func (m *ServerMetrics) BroadcastsStartedCount(kind, app, os, browser string) float64 {
	if m == nil {
		return 0
	}
	return counterValue(m.usage.broadcastsStarted.WithLabelValues(kind, app, os, browser))
}

// RoomsMintedCount reads back the rooms-minted counter (test support).
func (m *ServerMetrics) RoomsMintedCount() float64 {
	if m == nil {
		return 0
	}
	return counterValue(m.usage.roomsMinted)
}

// ViewerJoinsCount reads back a joins counter (test support).
func (m *ServerMetrics) ViewerJoinsCount(kind, delivery, app, os, browser string) float64 {
	if m == nil {
		return 0
	}
	return counterValue(m.usage.viewerJoins.WithLabelValues(kind, delivery, app, os, browser))
}

// ViewerSessionsObserved reads back how many viewer sessions of a delivery
// mode have been observed ending (test support).
func (m *ServerMetrics) ViewerSessionsObserved(delivery string) uint64 {
	if m == nil {
		return 0
	}
	var pb dto.Metric
	h, ok := m.usage.viewerSession.WithLabelValues(delivery).(prometheus.Histogram)
	if !ok || h.Write(&pb) != nil {
		return 0
	}
	return pb.GetHistogram().GetSampleCount()
}
