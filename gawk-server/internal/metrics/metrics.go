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

	broadcastsActive    *prometheus.Desc
	subscribersActive   *prometheus.Desc
	publisherActive     *prometheus.Desc
	graceRemaining      *prometheus.Desc
	subscribers         *prometheus.Desc
	reliableSubscribers *prometheus.Desc
	dvrSubscribers      *prometheus.Desc
	stripeLegs          *prometheus.Desc
	stripedPrimaries    *prometheus.Desc
	stripeSuppressed    desc
	stripeTransitions   desc
	stripeLegsReaped    desc
	dvrRingBytes        *prometheus.Desc
	edgeSessions        *prometheus.Desc
	viewersGlobal       *prometheus.Desc
	role                *prometheus.Desc
	cachedKeyframe      *prometheus.Desc

	framesRelayed    desc
	datagramsRelayed desc
	datagramsDropped desc
	badDatagrams     desc
	sendErrors       desc
	ingressLostF     desc
	ingressLostC     desc
	edgeIngressLostF desc
	edgeIngressLostC desc
	ingressBytes     desc
	egressBytes      desc
	bwDroppedBytes   desc
	kfIn             desc
	kfSent           desc
	kfDropped        desc
	kfOversize       desc
	carrierStreams   desc
	carrierRecords   desc
	dvrResyncs       desc
	carrierDropped   desc
	parityDatagrams  desc
	paritySuppressed desc
	egressParity     desc
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
			"Local viewers currently connected to the broadcast (edge sessions excluded).", []string{"broadcast"}, nil),
		reliableSubscribers: prometheus.NewDesc("gawk_broadcast_reliable_subscribers",
			"Local viewers in R19 reliable (resilient) delivery mode.", []string{"broadcast"}, nil),
		dvrSubscribers: prometheus.NewDesc("gawk_broadcast_dvr_subscribers",
			"Local viewers served from the R21 DVR ring at their own cursor.", []string{"broadcast"}, nil),
		stripeLegs: prometheus.NewDesc("gawk_broadcast_stripe_legs",
			"R30 stripe-leg sessions currently attached (docs/35). Legs also count in gawk_broadcast_subscribers — they are real external sessions — but never in viewers_global.", []string{"broadcast"}, nil),
		stripedPrimaries: prometheus.NewDesc("gawk_broadcast_striped_primaries",
			"Viewers whose primary session currently has delta datagrams suppressed because their stripe legs carry them (R30).", []string{"broadcast"}, nil),
		dvrRingBytes: prometheus.NewDesc("gawk_broadcast_dvr_ring_bytes",
			"Bytes this broadcast's DVR ring currently retains — the number to watch against -dvr-max-bytes.", []string{"broadcast"}, nil),
		edgeSessions: prometheus.NewDesc("gawk_broadcast_edge_sessions",
			"Downstream edge pods attached via the internal subscribe route (R17).", []string{"broadcast"}, nil),
		viewersGlobal: prometheus.NewDesc("gawk_broadcast_viewers_global",
			"Global viewer count the origin pushes to clients (R18: local viewers + edge downstream reports); origin hubs only. The Prometheus-side truth stays sum(gawk_broadcast_subscribers) by (broadcast) — this gauge exists to debug the pushed value.",
			[]string{"broadcast"}, nil),
		role: prometheus.NewDesc("gawk_broadcast_role",
			"This pod's role for the broadcast (R17): origin hosts the publisher, edge re-fans an upstream pull. Value is always 1; join on(broadcast) group_left(role).",
			[]string{"broadcast", "role"}, nil),
		cachedKeyframe: prometheus.NewDesc("gawk_broadcast_cached_keyframe_bytes",
			"Size of the cached keyframe used to prime late joiners.", []string{"broadcast"}, nil),

		framesRelayed: newDesc("frames_relayed_total",
			"Frames relayed; rate() approximates relay-side fps.", "kind"),
		datagramsRelayed: newDesc("datagrams_relayed_total",
			"Delta datagrams fanned out (before per-subscriber drops)."),
		datagramsDropped: newDesc("datagrams_dropped_total",
			"Per-subscriber datagram drops: queue_full = slow viewer, carrier_queue_full = R19 resilient viewer whose carrier drain fell behind (holes a reliable stream, viewer freezes to keyframe), bandwidth = configured egress cap.", "reason"),
		badDatagrams: newDesc("bad_datagrams_total",
			"Malformed publisher datagrams dropped."),
		sendErrors: newDesc("send_errors_total",
			"Datagram write failures to subscribers."),
		ingressLostF: newDesc("ingress_frames_lost_total",
			"Frames the publisher sent that never reached the relay (broadcaster-to-relay loss; origin hubs only)."),
		ingressLostC: newDesc("ingress_chunks_lost_total",
			"Missing chunks of frames that arrived incomplete (partial broadcaster-to-relay loss; origin hubs only)."),
		edgeIngressLostF: newDesc("edge_ingress_frames_lost_total",
			"Frames lost on the origin-to-edge leg (R17; edge hubs only — a separate family from the broadcaster-leg window, never mixed)."),
		edgeIngressLostC: newDesc("edge_ingress_chunks_lost_total",
			"Chunks lost on the origin-to-edge leg (R17; edge hubs only)."),
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
		carrierStreams: newDesc("carrier_streams_total",
			"R19 reliable carrier streams opened to resilient subscribers (~2/GOP each)."),
		carrierRecords: newDesc("carrier_records_total",
			"Delta datagrams delivered as reliable carrier records (R19)."),
		dvrResyncs: newDesc("dvr_resyncs_total",
			"R21 DVR subscribers resynced after their cursor fell off the ring's tail — the mode's only frame loss. Rising means stalls are outliving -dvr-window."),
		carrierDropped: newDesc("carrier_records_dropped_total",
			"Carrier records dropped: dead carrier after a stall/cancel, or stream-open failure (R19). Bandwidth-cap drops count as datagram bandwidth drops instead."),
		parityDatagrams: newDesc("parity_datagrams_total",
			"R29 forward-parity symbols forwarded to subscribers. The relay computes none of these — it forwards a per-subscriber prefix of what the producer emitted."),
		paritySuppressed: newDesc("parity_suppressed_total",
			"R29 parity symbols NOT forwarded because the subscriber's level was lower (or it is a carrier-mode subscriber, which recovers loss via QUIC retransmission). Rising against parity_datagrams_total is the per-subscriber filter working, not a fault."),
		egressParity: newDesc("egress_parity_bytes_total",
			"Bytes of R29 parity actually written to subscribers. A SLICE of egress_bytes_total{kind=\"delta\"}, not a sibling: parity rides the datagram path, so adding the two double-counts. This is what makes the fleet cost of -parity-default measurable rather than modelled."),
		stripeSuppressed: newDesc("stripe_suppressed_datagrams_total",
			"R30 delta datagrams withheld from striped primaries because their legs carry them (docs/35 §7). A leg's non-matching share is routing, not suppression, and is not counted."),
		stripeTransitions: newDesc("stripe_transitions_total",
			"R30 stripe suppression level flips observed (engage or release; the 1 Hz refresh does not count). Churning against a flat stripe_legs gauge is the flapping signature."),
		stripeLegsReaped: newDesc("stripe_legs_reaped_total",
			"R30 stripe-leg sessions the relay ended as orphaned (docs/35 \u00a714): owner-reaped when their primary session closed, or lease-reaped after inbound silence. Zero on a healthy fleet \u2014 rising means viewers are losing primaries without tearing their legs down."),
	}
}

// Describe implements prometheus.Collector.
func (c *RegistryCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.broadcastsActive
	ch <- c.subscribersActive
	ch <- c.publisherActive
	ch <- c.graceRemaining
	ch <- c.subscribers
	ch <- c.reliableSubscribers
	ch <- c.dvrSubscribers
	ch <- c.dvrRingBytes
	ch <- c.stripeLegs
	ch <- c.stripedPrimaries
	ch <- c.edgeSessions
	ch <- c.viewersGlobal
	ch <- c.role
	ch <- c.cachedKeyframe
	for _, d := range c.counterDescs() {
		ch <- d.broadcast
		ch <- d.relay
	}
}

func (c *RegistryCollector) counterDescs() []desc {
	return []desc{
		c.framesRelayed, c.datagramsRelayed, c.datagramsDropped, c.badDatagrams,
		c.sendErrors, c.ingressLostF, c.ingressLostC, c.edgeIngressLostF,
		c.edgeIngressLostC, c.ingressBytes,
		c.egressBytes, c.bwDroppedBytes, c.kfIn, c.kfSent, c.kfDropped, c.kfOversize,
		c.carrierStreams, c.carrierRecords, c.carrierDropped, c.dvrResyncs,
		c.parityDatagrams, c.paritySuppressed, c.egressParity,
		c.stripeSuppressed, c.stripeTransitions, c.stripeLegsReaped,
	}
}

// queueFull derives the generic slow-viewer drop bucket the way R9 fixed it:
// total per-subscriber drops minus every reason that has a bucket of its own
// (the egress cap, and since R19 a resilient subscriber's carrier-queue
// overflow). Floored at zero — the terms come from atomics read at slightly
// different instants during a snapshot, and a Prometheus counter must never
// wrap into a nonsense value because one raced ahead by a drop.
func queueFull(total, bandwidth, carrierOverflow uint64) uint64 {
	accounted := bandwidth + carrierOverflow
	if total < accounted {
		return 0
	}
	return total - accounted
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
		gauge(c.reliableSubscribers, float64(s.ReliableSubscribers), id)
		gauge(c.edgeSessions, float64(s.EdgeSessions), id)
		// Origin hubs only (docs/23 Decision 9): an edge doesn't compute G,
		// and a zero here would read as "no viewers" rather than "not mine".
		if s.Role == "origin" {
			gauge(c.viewersGlobal, float64(s.ViewersGlobal), id)
		}
		gauge(c.role, 1, id, s.Role)
		gauge(c.cachedKeyframe, float64(s.CachedKeyframeBytes), id)

		counter(c.framesRelayed.broadcast, s.FramesRelayed-s.KeyframeStreamsIn, id, "delta")
		counter(c.framesRelayed.broadcast, s.KeyframeStreamsIn, id, "keyframe")
		counter(c.datagramsRelayed.broadcast, s.DatagramsRelayed, id)
		counter(c.datagramsDropped.broadcast, queueFull(s.DatagramsDropped, s.BandwidthDroppedDatagrams, s.CarrierQueueOverflow), id, "queue_full")
		counter(c.datagramsDropped.broadcast, s.CarrierQueueOverflow, id, "carrier_queue_full")
		counter(c.datagramsDropped.broadcast, s.BandwidthDroppedDatagrams, id, "bandwidth")
		counter(c.badDatagrams.broadcast, s.BadDatagrams, id)
		counter(c.sendErrors.broadcast, s.SendErrors, id)
		// Loss attribution by leg (R17 W6): an edge hub's ingress window is
		// origin→edge loss; an origin hub's is broadcaster→relay loss.
		if s.Role == "edge" {
			counter(c.edgeIngressLostF.broadcast, s.IngressFramesLost, id)
			counter(c.edgeIngressLostC.broadcast, s.IngressChunksLost, id)
		} else {
			counter(c.ingressLostF.broadcast, s.IngressFramesLost, id)
			counter(c.ingressLostC.broadcast, s.IngressChunksLost, id)
		}
		counter(c.ingressBytes.broadcast, s.IngressDatagramBytes, id, "delta")
		counter(c.ingressBytes.broadcast, s.KeyframeBytesIn, id, "keyframe")
		counter(c.egressBytes.broadcast, s.EgressDatagramBytes, id, "delta")
		counter(c.egressBytes.broadcast, s.EgressKeyframeBytes, id, "keyframe")
		counter(c.egressBytes.broadcast, s.EgressCarrierBytes, id, "carrier")
		counter(c.bwDroppedBytes.broadcast, s.BandwidthDroppedBytes, id)
		counter(c.kfIn.broadcast, s.KeyframeStreamsIn, id)
		counter(c.kfSent.broadcast, s.KeyframeStreamsSent, id)
		counter(c.kfDropped.broadcast, s.KeyframeDrops.Superseded, id, "superseded")
		counter(c.kfDropped.broadcast, s.KeyframeDrops.Slow, id, "slow")
		counter(c.kfDropped.broadcast, s.KeyframeDrops.Bandwidth, id, "bandwidth")
		counter(c.kfDropped.broadcast, s.KeyframeDrops.OpenFailed, id, "open_failed")
		counter(c.kfOversize.broadcast, s.KeyframeStreamsOversize, id)
		counter(c.carrierStreams.broadcast, s.CarrierStreams, id)
		counter(c.carrierRecords.broadcast, s.CarrierRecords, id)
		counter(c.carrierDropped.broadcast, s.CarrierRecordsDropped, id)
		counter(c.parityDatagrams.broadcast, s.ParityDatagramsForwarded, id)
		counter(c.paritySuppressed.broadcast, s.ParitySuppressed, id)
		counter(c.egressParity.broadcast, s.EgressParityBytes, id)
		counter(c.stripeSuppressed.broadcast, s.StripeSuppressedDatagrams, id)
		counter(c.stripeTransitions.broadcast, s.StripeTransitions, id)
		counter(c.stripeLegsReaped.broadcast, s.StripeLegsReaped, id)
		gauge(c.stripeLegs, float64(s.StripeLegs), id)
		gauge(c.stripedPrimaries, float64(s.StripedPrimaries), id)
		counter(c.dvrResyncs.broadcast, s.DVRResyncs, id)
		// The ring's live cost, against which -dvr-max-bytes is set. A gauge,
		// not a counter: what matters is what it holds now.
		gauge(c.dvrSubscribers, float64(s.DVRSubscribers), id)
		gauge(c.dvrRingBytes, float64(s.DVRRingBytes), id)
	}

	t := snap.Totals
	counter(c.framesRelayed.relay, t.FramesRelayed-t.KeyframeStreamsIn, "delta")
	counter(c.framesRelayed.relay, t.KeyframeStreamsIn, "keyframe")
	counter(c.datagramsRelayed.relay, t.DatagramsRelayed)
	counter(c.datagramsDropped.relay, queueFull(t.DatagramsDropped, t.BandwidthDroppedDatagrams, t.CarrierQueueOverflow), "queue_full")
	counter(c.datagramsDropped.relay, t.CarrierQueueOverflow, "carrier_queue_full")
	counter(c.datagramsDropped.relay, t.BandwidthDroppedDatagrams, "bandwidth")
	counter(c.badDatagrams.relay, t.BadDatagrams)
	counter(c.sendErrors.relay, t.SendErrors)
	counter(c.ingressLostF.relay, t.IngressFramesLost)
	counter(c.ingressLostC.relay, t.IngressChunksLost)
	counter(c.edgeIngressLostF.relay, t.EdgeIngressFramesLost)
	counter(c.edgeIngressLostC.relay, t.EdgeIngressChunksLost)
	counter(c.ingressBytes.relay, t.IngressDatagramBytes, "delta")
	counter(c.ingressBytes.relay, t.KeyframeBytesIn, "keyframe")
	counter(c.egressBytes.relay, t.EgressDatagramBytes, "delta")
	counter(c.egressBytes.relay, t.EgressKeyframeBytes, "keyframe")
	counter(c.egressBytes.relay, t.EgressCarrierBytes, "carrier")
	counter(c.bwDroppedBytes.relay, t.BandwidthDroppedBytes)
	counter(c.kfIn.relay, t.KeyframeStreamsIn)
	counter(c.kfSent.relay, t.KeyframeStreamsSent)
	counter(c.kfDropped.relay, t.KeyframeDrops.Superseded, "superseded")
	counter(c.kfDropped.relay, t.KeyframeDrops.Slow, "slow")
	counter(c.kfDropped.relay, t.KeyframeDrops.Bandwidth, "bandwidth")
	counter(c.kfDropped.relay, t.KeyframeDrops.OpenFailed, "open_failed")
	counter(c.kfOversize.relay, t.KeyframeStreamsOversize)
	counter(c.carrierStreams.relay, t.CarrierStreams)
	counter(c.carrierRecords.relay, t.CarrierRecords)
	counter(c.carrierDropped.relay, t.CarrierRecordsDropped)
	counter(c.parityDatagrams.relay, t.ParityDatagramsForwarded)
	counter(c.paritySuppressed.relay, t.ParitySuppressed)
	counter(c.egressParity.relay, t.EgressParityBytes)
	counter(c.stripeSuppressed.relay, t.StripeSuppressedDatagrams)
	counter(c.stripeTransitions.relay, t.StripeTransitions)
	counter(c.stripeLegsReaped.relay, t.StripeLegsReaped)
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
	// OutcomeDraining marks CONNECTs rejected 503 during the SIGTERM drain
	// (R17 W1) — the pod is exiting and must not accept new sessions.
	OutcomeDraining = "draining"
	// OutcomeBanned marks publish attempts rejected 451 by an R39 moderation
	// ban (docs/42 §4.3). Distinct from "unauthorized" on purpose: a banned
	// broadcaster holds a perfectly valid secret and resume token, and
	// conflating the two would hide an enforcement event inside the
	// credential-failure rate every operator already watches.
	OutcomeBanned = "banned"
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
