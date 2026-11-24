package bridge

import (
	"runtime"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// Failure reasons (D18): the label values of gawk_bridge_mumble_failures_total
// and the keys of /statusz's mumbleFailures. A fixed set, so the metric's
// cardinality stays bounded.
const (
	ReasonUnreachable      = "murmur_unreachable"
	ReasonAutoban          = "autoban"
	ReasonServerFull       = "server_full"
	ReasonWrongPassword    = "wrong_server_password"
	ReasonInvalidUsername  = "invalid_username"
	ReasonNameUnavailable  = "username_unavailable"
	ReasonRejected         = "rejected"
	ReasonChannelNotFound  = "channel_not_found"
	ReasonPermissionDenied = "permission_denied"
	ReasonTempChannelCap   = "temp_channel_cap"
	ReasonConnectionLost   = "connection_lost"
	ReasonReplaced         = "replaced"
	ReasonTimeout          = "timeout"
	// Ice mode (docs/89 D11).
	ReasonIceUnavailable     = "ice_unavailable"
	ReasonIceSecret          = "ice_secret"
	ReasonIceProtocol        = "ice_protocol"
	ReasonIceServerAmbiguous = "ice_server_ambiguous"
)

// failureText is the operator- and browser-facing reason for each failure.
// The close reason a browser shows comes from here, never from an error
// string (which can carry murmur's address).
var failureText = map[string]string{
	ReasonUnreachable:        "the Mumble server is unreachable",
	ReasonAutoban:            "the Mumble server refused the connection (autoban?)",
	ReasonServerFull:         "the Mumble server is full",
	ReasonWrongPassword:      "the Mumble server password is wrong",
	ReasonInvalidUsername:    "the Mumble server refused the name",
	ReasonNameUnavailable:    "no free Mumble name",
	ReasonRejected:           "the Mumble server refused the login",
	ReasonChannelNotFound:    "the room's Mumble channel does not exist",
	ReasonPermissionDenied:   "the Mumble server denied entering the channel",
	ReasonTempChannelCap:     "the bridge's temporary channel cap is reached",
	ReasonConnectionLost:     "the Mumble connection was lost",
	ReasonReplaced:           "a newer Mumble login took this name",
	ReasonTimeout:            "the Mumble server did not answer in time",
	ReasonIceUnavailable:     "the Mumble server's Ice interface is unreachable",
	ReasonIceSecret:          "the Mumble server refused the bridge's Ice secret",
	ReasonIceProtocol:        "the Mumble server's Ice answer was not understood",
	ReasonIceServerAmbiguous: "the Mumble server runs several virtual servers; the bridge needs -mumble-ice-server-id",
}

type metrics struct {
	reg *prometheus.Registry

	voiceSessions prometheus.Gauge
	observers     prometheus.Gauge
	tempChannels  prometheus.Gauge
	draining      prometheus.Gauge
	failures      *prometheus.CounterVec
	refused       *prometheus.CounterVec
	closed        *prometheus.CounterVec
	removals      *prometheus.CounterVec
	forwarded     *prometheus.CounterVec
	dropped       *prometheus.CounterVec

	// Ice mode (docs/89 D12).
	iceEvents        *prometheus.CounterVec
	iceReconciles    *prometheus.CounterVec
	iceDrift         prometheus.Counter
	iceRegistrations *prometheus.CounterVec
	iceRegistered    prometheus.Gauge
}

func newMetrics(version string) *metrics {
	m := &metrics{reg: prometheus.NewRegistry()}
	f := func(c prometheus.Collector) { m.reg.MustRegister(c) }
	m.voiceSessions = prometheus.NewGauge(prometheus.GaugeOpts{Name: "gawk_bridge_voice_sessions", Help: "Browser voice sessions this pod holds."})
	m.observers = prometheus.NewGauge(prometheus.GaugeOpts{Name: "gawk_bridge_observers", Help: "Observer links (one per voice-live room) this pod holds."})
	m.tempChannels = prometheus.NewGauge(prometheus.GaugeOpts{Name: "gawk_bridge_temp_channels", Help: "Temporary channels this pod's observers hold (-max-temp-channels caps it)."})
	m.draining = prometheus.NewGauge(prometheus.GaugeOpts{Name: "gawk_bridge_draining", Help: "1 while the pod drains (D19)."})
	m.failures = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "gawk_bridge_mumble_failures_total", Help: "Mumble connections that failed, by reason (D18)."}, []string{"reason"})
	m.refused = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "gawk_bridge_sessions_refused_total", Help: "Voice sessions refused before Mumble, by reason."}, []string{"reason"})
	m.closed = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "gawk_bridge_sessions_closed_total", Help: "Voice sessions closed by the bridge, by close code."}, []string{"code"})
	m.removals = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "gawk_bridge_mumble_removals_total", Help: "Bridged users Mumble removed, by kind (kick, ban)."}, []string{"kind"})
	m.forwarded = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "gawk_bridge_frames_forwarded_total", Help: "Opus frames forwarded, by direction (to_mumble, to_browser)."}, []string{"direction"})
	m.dropped = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "gawk_bridge_frames_dropped_total", Help: "Opus frames dropped, by direction and reason."}, []string{"direction", "reason"})
	m.iceEvents = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "gawk_bridge_ice_events_total", Help: "Ice ServerCallback events received, by operation (docs/89 D2)."}, []string{"op"})
	m.iceReconciles = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "gawk_bridge_ice_reconciles_total", Help: "Periodic Ice re-reads, by result (ok, drift, error) (docs/89 D15)."}, []string{"result"})
	m.iceDrift = prometheus.NewCounter(prometheus.CounterOpts{Name: "gawk_bridge_ice_reconcile_drift_total", Help: "Ice re-reads that differed from the mirror: missed events (docs/89 D15)."})
	m.iceRegistrations = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "gawk_bridge_ice_registrations_total", Help: "Ice addCallback calls, by result (ok, secret, error) (docs/89 D15)."}, []string{"result"})
	m.iceRegistered = prometheus.NewGauge(prometheus.GaugeOpts{Name: "gawk_bridge_ice_registered", Help: "1 while the pod believes murmur holds its Ice callback registration (docs/89 D12)."})
	build := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "gawk_bridge_build_info", Help: "Build information; always 1."}, []string{"version", "goversion"})
	build.WithLabelValues(version, runtime.Version()).Set(1)
	for _, c := range []prometheus.Collector{m.voiceSessions, m.observers, m.tempChannels, m.draining, m.failures, m.refused,
		m.closed, m.removals, m.forwarded, m.dropped, build,
		m.iceEvents, m.iceReconciles, m.iceDrift, m.iceRegistrations, m.iceRegistered,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{})} {
		f(c)
	}
	for r := range failureText {
		m.failures.WithLabelValues(r)
	}
	for _, d := range []string{dirToMumble, dirToBrowser} {
		m.forwarded.WithLabelValues(d)
	}
	return m
}

const (
	dirToMumble  = "to_mumble"
	dirToBrowser = "to_browser"
)
