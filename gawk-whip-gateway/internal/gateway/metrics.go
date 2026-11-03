package gateway

import (
	"runtime"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// Frame-drop reasons (gawk_whip_frames_dropped_total{reason}).
const (
	dropReorderWindow = "reorder_window" // D6: incomplete when its window closed
	dropAwaitingIDR   = "awaiting_idr"   // a delta after a loss, before the next IDR
	dropBackpressure  = "engine_backpressure"
)

// X-Forwarded-For anomalies (gawk_whip_xff_misconfigured_total{kind}), D4.
const (
	xffUntrustedPeer   = "untrusted_peer"    // XFF from a peer outside a non-empty trusted list
	xffClientIPUnknown = "client_ip_unknown" // trusted peer, header missing or unparsable: 400
)

// Metrics are the gateway's Prometheus series (docs/83 §6: sessions, POST
// outcomes by status, frames dropped by the reorder window, measured GOP,
// NACKs sent; plus the D4 misconfiguration counter). Labels are fixed sets:
// no key, ID or address ever becomes a label.
type Metrics struct {
	Registry *prometheus.Registry

	sessions      prometheus.Gauge
	posts         *prometheus.CounterVec
	ended         *prometheus.CounterVec
	framesDropped *prometheus.CounterVec
	frames        prometheus.Counter
	gop           prometheus.Histogram
	nacks         prometheus.Counter
	plis          *prometheus.CounterVec
	xff           *prometheus.CounterVec
	peer          *prometheus.CounterVec
	rtt           prometheus.Histogram
}

// NewMetrics registers the series on a fresh registry.
func NewMetrics(version string) *Metrics {
	m := &Metrics{Registry: prometheus.NewRegistry()}
	m.sessions = prometheus.NewGauge(prometheus.GaugeOpts{Name: "gawk_whip_sessions",
		Help: "WHIP sessions this pod holds (live or being torn down); maxSessions caps it."})
	m.posts = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "gawk_whip_posts_total",
		Help: "POST /whip outcomes, by HTTP status."}, []string{"status"})
	m.ended = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "gawk_whip_sessions_ended_total",
		Help: "Sessions ended, by reason (deleted, superseded, shutdown, or a lastError reason)."}, []string{"reason"})
	m.framesDropped = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "gawk_whip_frames_dropped_total",
		Help: "Video frames not republished, by reason (reorder_window: D6's window closed on it; awaiting_idr: a delta after a loss; engine_backpressure)."},
		[]string{"reason"})
	m.frames = prometheus.NewCounter(prometheus.CounterOpts{Name: "gawk_whip_frames_total",
		Help: "Video access units handed to the publisher engine."})
	m.gop = prometheus.NewHistogram(prometheus.HistogramOpts{Name: "gawk_whip_gop_seconds",
		Help:    "Measured keyframe interval (D7's median), sampled once a second per live session.",
		Buckets: []float64{0.25, 0.5, 0.75, 1, 1.1, 1.5, 2, 2.2, 3, 4, 5, 8}})
	m.nacks = prometheus.NewCounter(prometheus.CounterOpts{Name: "gawk_whip_nacks_sent_total",
		Help: "Video sequence numbers NACKed to the sender (D6)."})
	m.plis = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "gawk_whip_plis_sent_total",
		Help: "PLIs sent, by trigger (start, resume). Never on a drop, never periodic (D7)."}, []string{"trigger"})
	m.xff = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "gawk_whip_xff_misconfigured_total",
		Help: "X-Forwarded-For anomalies (D4): untrusted_peer (header from a peer outside a non-empty trustedProxyCidrs), client_ip_unknown (trusted peer, header missing or unparsable: 400)."},
		[]string{"kind"})
	m.peer = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "gawk_whip_peer_requests_total",
		Help: "D10 peer requests this pod made, by operation and result."}, []string{"op", "result"})
	m.rtt = prometheus.NewHistogram(prometheus.HistogramOpts{Name: "gawk_whip_rtt_seconds",
		Help:    "OBS → gateway RTT estimate from NACK → retransmit (D6), sampled once a second per live session that has one.",
		Buckets: []float64{0.002, 0.005, 0.01, 0.02, 0.04, 0.06, 0.08, 0.1, 0.15, 0.2, 0.3, 0.5}})
	build := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "gawk_whip_build_info", Help: "Build information; always 1."},
		[]string{"version", "goversion"})
	build.WithLabelValues(version, runtime.Version()).Set(1)
	m.Registry.MustRegister(m.sessions, m.posts, m.ended, m.framesDropped, m.frames, m.gop, m.nacks, m.plis,
		m.xff, m.peer, m.rtt, build,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return m
}

func (m *Metrics) post(status int) { m.posts.WithLabelValues(strconv.Itoa(status)).Inc() }
