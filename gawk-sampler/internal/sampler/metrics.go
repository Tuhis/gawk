package sampler

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Metrics are the sampler's operational metrics — latency, effective interval,
// failures, POST outcomes (docs/71 §5). None carries a broadcast ID or key:
// per-broadcast detail is gawk-admin's to hold, and a label per broadcast
// would grow without bound. Every method is nil-receiver-safe.
type Metrics struct {
	samples         *prometheus.CounterVec
	failures        *prometheus.CounterVec
	stageSeconds    *prometheus.HistogramVec
	effective       prometheus.Gauge
	target          prometheus.Gauge
	paused          *prometheus.GaugeVec
	targets         *prometheus.GaugeVec
	tripped         prometheus.Gauge
	flagPosts       *prometheus.CounterVec
	postErrors      *prometheus.CounterVec
	samplesSent     prometheus.Counter
	samplesDropped  prometheus.Counter
	targetsRefresh  *prometheus.CounterVec
	workers         prometheus.Gauge
	workersBusy     prometheus.Gauge
	lastTargetsPoll prometheus.Gauge
}

// NewMetrics registers the sampler's metrics on reg.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		samples: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gawk_sampler_samples_total",
			Help: "Samples scored (D20: a sample is a scored frame), by result: scored (a fetched frame) or unchanged (a 304, repeating the previous score).",
		}, []string{"result"}),
		failures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gawk_sampler_sample_failures_total",
			Help: "Fetches that ended in no sample, by stage: not_found (relay 404), fetch, convert, decode, decode_timeout, inference, stale (a 304 with no score to repeat).",
		}, []string{"stage"}),
		stageSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "gawk_sampler_stage_seconds",
			Help:    "Per-sample time by stage: fetch, decode, inference, and total.",
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2, 5},
		}, []string{"stage"}),
		effective: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "gawk_sampler_interval_effective_seconds",
			Help: "The longest time between two samples of any screened broadcast in the current rotation (D6). Above the target, the worker pool cannot keep up and every broadcast's interval has stretched evenly.",
		}),
		target: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "gawk_sampler_interval_target_seconds",
			Help: "-interval: the per-broadcast sample interval the sampler aims for (D6).",
		}),
		paused: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gawk_sampler_paused",
			Help: "1 while the sampler samples nothing, by reason: no_flag_route (flag mode with no webhook to page, D12).",
		}, []string{"reason"}),
		targets: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gawk_sampler_targets",
			Help: "Broadcasts in this shard's last target list, by state: screened, exempt (an exempt room, never sampled, D11), muted (dismissed, still sampled, D10).",
		}, []string{"state"}),
		tripped: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "gawk_sampler_windows_tripped",
			Help: "Broadcasts whose window is tripped (D7).",
		}),
		flagPosts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gawk_sampler_flag_posts_total",
			Help: "Content-flag POSTs gawk-admin answered, by the state posted and its answer (raised, updated, resolved, muted, ended, ignored).",
		}, []string{"state", "status"}),
		postErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gawk_sampler_post_errors_total",
			Help: "Calls to gawk-admin that failed, by endpoint: content_flags, samples, targets.",
		}, []string{"endpoint"}),
		samplesSent: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "gawk_sampler_samples_sent_total",
			Help: "Samples at or above -sample-floor delivered to gawk-admin in batches (D20).",
		}),
		samplesDropped: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "gawk_sampler_samples_dropped_total",
			Help: "Samples taken but never delivered because gawk-admin stayed unreachable past the batch size limit.",
		}),
		targetsRefresh: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gawk_sampler_targets_refresh_total",
			Help: "Target-list polls by result: ok or error.",
		}, []string{"result"}),
		workers: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "gawk_sampler_workers",
			Help: "-workers: concurrent fetch+decode+infer slots.",
		}),
		workersBusy: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "gawk_sampler_workers_busy",
			Help: "Slots currently sampling.",
		}),
		lastTargetsPoll: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "gawk_sampler_targets_last_success_timestamp_seconds",
			Help: "When the target list was last read successfully.",
		}),
	}
	reg.MustRegister(m.samples, m.failures, m.stageSeconds, m.effective, m.target, m.paused, m.targets, m.tripped,
		m.flagPosts, m.postErrors, m.samplesSent, m.samplesDropped, m.targetsRefresh, m.workers, m.workersBusy, m.lastTargetsPoll)
	// Pre-create the label sets an alert or a dashboard reads, so they exist
	// at zero rather than appearing on the first event.
	m.paused.WithLabelValues(pausedNoFlagRoute)
	for _, s := range []string{"screened", "exempt", "muted"} {
		m.targets.WithLabelValues(s)
	}
	return m
}

const pausedNoFlagRoute = "no_flag_route"

func (m *Metrics) sample(result string) {
	if m != nil {
		m.samples.WithLabelValues(result).Inc()
	}
}

func (m *Metrics) failure(stage string) {
	if m != nil {
		m.failures.WithLabelValues(stage).Inc()
	}
}

func (m *Metrics) stage(stage string, seconds float64) {
	if m != nil {
		m.stageSeconds.WithLabelValues(stage).Observe(seconds)
	}
}

func (m *Metrics) setEffective(seconds float64) {
	if m != nil {
		m.effective.Set(seconds)
	}
}

func (m *Metrics) setPaused(on bool) {
	if m != nil {
		v := 0.0
		if on {
			v = 1
		}
		m.paused.WithLabelValues(pausedNoFlagRoute).Set(v)
	}
}

func (m *Metrics) setTargets(screened, exempt, muted int) {
	if m != nil {
		m.targets.WithLabelValues("screened").Set(float64(screened))
		m.targets.WithLabelValues("exempt").Set(float64(exempt))
		m.targets.WithLabelValues("muted").Set(float64(muted))
	}
}

func (m *Metrics) setTripped(n int) {
	if m != nil {
		m.tripped.Set(float64(n))
	}
}

func (m *Metrics) flagPost(state, status string) {
	if m != nil {
		m.flagPosts.WithLabelValues(state, status).Inc()
	}
}

func (m *Metrics) postError(endpoint string) {
	if m != nil {
		m.postErrors.WithLabelValues(endpoint).Inc()
	}
}

func (m *Metrics) sent(n int) {
	if m != nil {
		m.samplesSent.Add(float64(n))
	}
}

func (m *Metrics) dropped(n int) {
	if m != nil {
		m.samplesDropped.Add(float64(n))
	}
}

func (m *Metrics) refreshed(ok bool, unix float64) {
	if m == nil {
		return
	}
	if ok {
		m.targetsRefresh.WithLabelValues("ok").Inc()
		m.lastTargetsPoll.Set(unix)
		return
	}
	m.targetsRefresh.WithLabelValues("error").Inc()
}

func (m *Metrics) setWorkers(total int, target float64) {
	if m != nil {
		m.workers.Set(float64(total))
		m.target.Set(target)
	}
}

func (m *Metrics) busy(delta float64) {
	if m != nil {
		m.workersBusy.Add(delta)
	}
}
