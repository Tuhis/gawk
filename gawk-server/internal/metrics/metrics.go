// Package metrics owns everything Prometheus (R9, docs/13): the base
// registry (runtime collectors + build info), the hub registry collector, and
// the transport-layer connection counters. The hub itself stays free of
// Prometheus imports — its existing counters are the single source of truth,
// snapshotted here into const metrics so /metrics and /statusz can never
// disagree.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	dto "github.com/prometheus/client_model/go"

	"github.com/Tuhis/gawk/gawk-server/internal/hub"
)

// NewBaseRegistry builds the process-level Prometheus registry: Go runtime +
// process collectors and gawk_build_info carrying the ldflags-stamped version.
func NewBaseRegistry(version string) *prometheus.Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector())
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	build := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "gawk_build_info",
		Help: "Build information; the value is always 1.",
	}, []string{"version"})
	build.WithLabelValues(version).Set(1)
	reg.MustRegister(build)
	return reg
}

// Metric name vocabulary (docs/13): per-broadcast series are
// gawk_broadcast_*{broadcast=<obfuscated id>} and vanish when a broadcast is
// GC'd; registry-lifetime totals (which include folded/expired broadcasts and
// only ever grow) are gawk_relay_* — those are the ones for long-range rate
// queries. The broadcast label is the same per-process HMAC obfuscation as
// /statusz keys; raw IDs are joinable and must never appear here.

// desc pairs a per-broadcast and a totals Desc for one logical counter.
type desc struct {
	broadcast *prometheus.Desc
	relay     *prometheus.Desc
}

func newDesc(name, help string, extraLabels ...string) desc {
	return desc{
		broadcast: prometheus.NewDesc("gawk_broadcast_"+name, help+" (per broadcast)",
			append([]string{"broadcast"}, extraLabels...), nil),
		relay: prometheus.NewDesc("gawk_relay_"+name, help+" (registry lifetime, incl. expired broadcasts)",
			extraLabels, nil),
	}
}

// RegistryCollector exposes hub.Registry stats as Prometheus metrics via a
// point-in-time snapshot per scrape (prometheus.Collector).
type RegistryCollector struct {
	registry *hub.Registry

	broadcastsActive  *prometheus.Desc
	subscribersActive *prometheus.Desc
	publisherActive   *prometheus.Desc
	graceRemaining    *prometheus.Desc
	subscribers       *prometheus.Desc
	cachedKeyframe    *prometheus.Desc

	framesRelayed    desc
	datagramsRelayed desc
	datagramsDropped desc
	badDatagrams     desc
	sendErrors       desc
	ingressLostF     desc
	ingressLostC     desc
	ingressBytes     desc
	egressBytes      desc
	bwDroppedBytes   desc
	kfIn             desc
	kfSent           desc
	kfDropped        desc
	kfOversize       desc
}

// NewRegistryCollector builds the collector over a hub registry.
func NewRegistryCollector(r *hub.Registry) *RegistryCollector {
	return &RegistryCollector{
		registry: r,

		broadcastsActive: prometheus.NewDesc("gawk_broadcasts_active",
			"Broadcasts currently registered (incl. publisher-away grace).", nil, nil),
		subscribersActive: prometheus.NewDesc("gawk_subscribers_active",
			"Subscribers currently connected across all broadcasts.", nil, nil),
		publisherActive: prometheus.NewDesc("gawk_broadcast_publisher_active",
			"1 while the broadcast's publisher session is connected, 0 during grace.",
			[]string{"broadcast"}, nil),
		graceRemaining: prometheus.NewDesc("gawk_broadcast_grace_remaining_seconds",
			"Seconds until an abandoned broadcast is garbage-collected; 0 while the publisher is active.",
			[]string{"broadcast"}, nil),
		subscribers: prometheus.NewDesc("gawk_broadcast_subscribers",
			"Subscribers currently connected to the broadcast.", []string{"broadcast"}, nil),
		cachedKeyframe: prometheus.NewDesc("gawk_broadcast_cached_keyframe_bytes",
			"Size of the cached keyframe used to prime late joiners.", []string{"broadcast"}, nil),

		framesRelayed: newDesc("frames_relayed_total",
			"Frames relayed; rate() approximates relay-side fps.", "kind"),
		datagramsRelayed: newDesc("datagrams_relayed_total",
			"Delta datagrams fanned out (before per-subscriber drops)."),
		datagramsDropped: newDesc("datagrams_dropped_total",
			"Per-subscriber datagram drops: queue_full = slow viewer, bandwidth = configured egress cap.", "reason"),
		badDatagrams: newDesc("bad_datagrams_total",
			"Malformed publisher datagrams dropped."),
		sendErrors: newDesc("send_errors_total",
			"Datagram write failures to subscribers."),
		ingressLostF: newDesc("ingress_frames_lost_total",
			"Frames the publisher sent that never reached the relay (broadcaster-to-relay loss)."),
		ingressLostC: newDesc("ingress_chunks_lost_total",
			"Missing chunks of frames that arrived incomplete (partial broadcaster-to-relay loss)."),
		ingressBytes: newDesc("ingress_bytes_total",
			"Bytes received from the publisher.", "kind"),
		egressBytes: newDesc("egress_bytes_total",
			"Bytes actually delivered to subscribers.", "kind"),
		bwDroppedBytes: newDesc("bandwidth_dropped_bytes_total",
			"Bytes dropped by the configured egress bandwidth cap (datagrams and keyframes)."),
		kfIn: newDesc("keyframe_streams_in_total",
			"Keyframe streams ingested from the publisher; rate() is the GOP cadence."),
		kfSent: newDesc("keyframe_streams_sent_total",
			"Keyframe streams fully delivered to subscribers."),
		kfDropped: newDesc("keyframe_streams_dropped_total",
			"Keyframe streams dropped per subscriber; superseded is benign, slow is a stalling viewer.", "reason"),
		kfOversize: newDesc("keyframe_streams_oversize_total",
			"Publisher keyframe streams rejected over max-keyframe-bytes."),
	}
}

// Describe implements prometheus.Collector.
func (c *RegistryCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.broadcastsActive
	ch <- c.subscribersActive
	ch <- c.publisherActive
	ch <- c.graceRemaining
	ch <- c.subscribers
	ch <- c.cachedKeyframe
	for _, d := range c.counterDescs() {
		ch <- d.broadcast
		ch <- d.relay
	}
}

func (c *RegistryCollector) counterDescs() []desc {
	return []desc{
		c.framesRelayed, c.datagramsRelayed, c.datagramsDropped, c.badDatagrams,
		c.sendErrors, c.ingressLostF, c.ingressLostC, c.ingressBytes,
		c.egressBytes, c.bwDroppedBytes, c.kfIn, c.kfSent, c.kfDropped, c.kfOversize,
	}
}

// Collect implements prometheus.Collector: one Registry.Stats() snapshot per
// scrape (the same cost as a /statusz poll).
func (c *RegistryCollector) Collect(ch chan<- prometheus.Metric) {
	snap := c.registry.Stats()

	gauge := func(d *prometheus.Desc, v float64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v, labels...)
	}
	counter := func(d *prometheus.Desc, v uint64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.CounterValue, float64(v), labels...)
	}

	gauge(c.broadcastsActive, float64(snap.Totals.Broadcasts))
	gauge(c.subscribersActive, float64(snap.Totals.Subscribers))

	for id, s := range snap.Broadcasts {
		active := 0.0
		if s.PublisherActive {
			active = 1
		}
		gauge(c.publisherActive, active, id)
		gauge(c.graceRemaining, float64(s.GraceRemainingSeconds), id)
		gauge(c.subscribers, float64(s.Subscribers), id)
		gauge(c.cachedKeyframe, float64(s.CachedKeyframeBytes), id)

		counter(c.framesRelayed.broadcast, s.FramesRelayed-s.KeyframeStreamsIn, id, "delta")
		counter(c.framesRelayed.broadcast, s.KeyframeStreamsIn, id, "keyframe")
		counter(c.datagramsRelayed.broadcast, s.DatagramsRelayed, id)
		counter(c.datagramsDropped.broadcast, s.DatagramsDropped-s.BandwidthDroppedDatagrams, id, "queue_full")
		counter(c.datagramsDropped.broadcast, s.BandwidthDroppedDatagrams, id, "bandwidth")
		counter(c.badDatagrams.broadcast, s.BadDatagrams, id)
		counter(c.sendErrors.broadcast, s.SendErrors, id)
		counter(c.ingressLostF.broadcast, s.IngressFramesLost, id)
		counter(c.ingressLostC.broadcast, s.IngressChunksLost, id)
		counter(c.ingressBytes.broadcast, s.IngressDatagramBytes, id, "delta")
		counter(c.ingressBytes.broadcast, s.KeyframeBytesIn, id, "keyframe")
		counter(c.egressBytes.broadcast, s.EgressDatagramBytes, id, "delta")
		counter(c.egressBytes.broadcast, s.EgressKeyframeBytes, id, "keyframe")
		counter(c.bwDroppedBytes.broadcast, s.BandwidthDroppedBytes, id)
		counter(c.kfIn.broadcast, s.KeyframeStreamsIn, id)
		counter(c.kfSent.broadcast, s.KeyframeStreamsSent, id)
		counter(c.kfDropped.broadcast, s.KeyframeDrops.Superseded, id, "superseded")
		counter(c.kfDropped.broadcast, s.KeyframeDrops.Slow, id, "slow")
		counter(c.kfDropped.broadcast, s.KeyframeDrops.Bandwidth, id, "bandwidth")
		counter(c.kfDropped.broadcast, s.KeyframeDrops.OpenFailed, id, "open_failed")
		counter(c.kfOversize.broadcast, s.KeyframeStreamsOversize, id)
	}

	t := snap.Totals
	counter(c.framesRelayed.relay, t.FramesRelayed-t.KeyframeStreamsIn, "delta")
	counter(c.framesRelayed.relay, t.KeyframeStreamsIn, "keyframe")
	counter(c.datagramsRelayed.relay, t.DatagramsRelayed)
	counter(c.datagramsDropped.relay, t.DatagramsDropped-t.BandwidthDroppedDatagrams, "queue_full")
	counter(c.datagramsDropped.relay, t.BandwidthDroppedDatagrams, "bandwidth")
	counter(c.badDatagrams.relay, t.BadDatagrams)
	counter(c.sendErrors.relay, t.SendErrors)
	counter(c.ingressLostF.relay, t.IngressFramesLost)
	counter(c.ingressLostC.relay, t.IngressChunksLost)
	counter(c.ingressBytes.relay, t.IngressDatagramBytes, "delta")
	counter(c.ingressBytes.relay, t.KeyframeBytesIn, "keyframe")
	counter(c.egressBytes.relay, t.EgressDatagramBytes, "delta")
	counter(c.egressBytes.relay, t.EgressKeyframeBytes, "keyframe")
	counter(c.bwDroppedBytes.relay, t.BandwidthDroppedBytes)
	counter(c.kfIn.relay, t.KeyframeStreamsIn)
	counter(c.kfSent.relay, t.KeyframeStreamsSent)
	counter(c.kfDropped.relay, t.KeyframeDrops.Superseded, "superseded")
	counter(c.kfDropped.relay, t.KeyframeDrops.Slow, "slow")
	counter(c.kfDropped.relay, t.KeyframeDrops.Bandwidth, "bandwidth")
	counter(c.kfDropped.relay, t.KeyframeDrops.OpenFailed, "open_failed")
	counter(c.kfOversize.relay, t.KeyframeStreamsOversize)
}

// ServerMetrics are the transport-layer connection counters (R9 M4). All
// methods are nil-receiver-safe so the transport works unwired in tests.
type ServerMetrics struct {
	connections    *prometheus.CounterVec
	rateLimited    prometheus.Counter
	originRejected prometheus.Counter
}

// Connection outcomes (closed enum; see docs/13 D3).
const (
	OutcomeAccepted      = "accepted"
	OutcomeUnauthorized  = "unauthorized"
	OutcomeNotFound      = "not_found"
	OutcomeConflict      = "conflict"
	OutcomeLimitRejected = "limit_rejected"
	OutcomeUpgradeFailed = "upgrade_failed"
	OutcomeError         = "error"
)

// NewServerMetrics builds and registers the transport counters.
func NewServerMetrics(reg prometheus.Registerer) *ServerMetrics {
	m := &ServerMetrics{
		connections: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gawk_connections_total",
			Help: "Session attempts per route and outcome (rate-limited attempts count only in gawk_rate_limited_total).",
		}, []string{"route", "outcome"}),
		rateLimited: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "gawk_rate_limited_total",
			Help: "Connection attempts rejected by the per-IP rate limiter.",
		}),
		originRejected: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "gawk_origin_rejected_total",
			Help: "Sessions rejected by the Origin allowlist.",
		}),
	}
	reg.MustRegister(m.connections, m.rateLimited, m.originRejected)
	return m
}

// Connection records one session attempt outcome for a route.
func (m *ServerMetrics) Connection(route, outcome string) {
	if m == nil {
		return
	}
	m.connections.WithLabelValues(route, outcome).Inc()
}

// RateLimited records one rate-limiter rejection.
func (m *ServerMetrics) RateLimited() {
	if m == nil {
		return
	}
	m.rateLimited.Inc()
}

// OriginRejected records one Origin-allowlist rejection.
func (m *ServerMetrics) OriginRejected() {
	if m == nil {
		return
	}
	m.originRejected.Inc()
}

// ConnectionCount reads back a labeled connection counter (test support).
func (m *ServerMetrics) ConnectionCount(route, outcome string) float64 {
	if m == nil {
		return 0
	}
	return counterValue(m.connections.WithLabelValues(route, outcome))
}

// RateLimitedCount reads back the rate-limited counter (test support).
func (m *ServerMetrics) RateLimitedCount() float64 {
	if m == nil {
		return 0
	}
	return counterValue(m.rateLimited)
}

// OriginRejectedCount reads back the origin-rejected counter (test support).
func (m *ServerMetrics) OriginRejectedCount() float64 {
	if m == nil {
		return 0
	}
	return counterValue(m.originRejected)
}

func counterValue(c prometheus.Counter) float64 {
	var pb dto.Metric
	if err := c.Write(&pb); err != nil {
		return 0
	}
	return pb.GetCounter().GetValue()
}
