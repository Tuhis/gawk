package rollup

// Dip-episode detection (docs/33 D16 + §4.10).
//
// The rollup already summarizes every experiential series as percentiles, and
// percentiles are the right shape for "how did this session go". They are the
// WRONG shape for "did it fall apart now and then", which is the question a
// stuttering viewer actually asks — a session holding 30 fps that collapses to
// the 500 ms GOP cadence for six seconds a minute has a median of 30, and every
// funnel rule reading that median passes.
//
// So this measures something else, beside the percentiles rather than instead
// of them. Nothing here feeds a funnel ratio: mixing statistics across a ratio
// is how the read path once manufactured a confidently wrong `decoder-choking`
// verdict on a clean stream, and D16 exists specifically not to repeat it.
//
// The same detector runs over two windows — the session here, a rolling window
// in the live projection — because two disagreeing truths about one stream
// would be worse than one incomplete truth (§4.8.3).

import (
	"math"
	"sort"

	"github.com/Tuhis/gawk/gawk-telemetry/internal/schema"
)

const (
	// DipRatio is how far below its own baseline a rate must fall to count as
	// a dip. Self-relative on purpose: an absolute floor would accuse a
	// deliberate 5 fps stream forever and would miss a 60 fps stream
	// collapsing to 20.
	DipRatio = 0.5
	// MinBaselineFps is the baseline below which a session has no meaningful
	// steady state to dip FROM, and every wobble would read as an episode.
	MinBaselineFps = 5
	// DipShareBad / DipCountBad are where "degraded" becomes "broken from a
	// viewer's point of view". A tenth of a session spent collapsed, or four
	// separate collapses, is not a blip.
	DipShareBad = 0.1
	DipCountBad = 4
	// FallbackIntervalMs stands in for the first sample's duration, which has
	// no predecessor to measure against. It is the relay's default report
	// interval; a session whose real cadence differs corrects itself from the
	// second sample onward, and only the first sample's weight is affected.
	FallbackIntervalMs = 2000
)

// IntervalMinField is the nested object a client uses to report the MINIMUM it
// saw between two emitted samples (D16). Without it a dip shorter than one
// report interval is invisible: the client emits one tick every ~2 s and
// discards the three between.
const IntervalMinField = "intervalMin"

// Episodes summarizes the dips in one series over one window.
type Episodes struct {
	// Count is the number of distinct episodes — "one 6 s collapse" rather
	// than "three bad samples", which is what a human means by "every now and
	// then".
	Count int `json:"count"`
	// TotalMs / LongestMs are durations measured from real sample timestamps,
	// never assumed from a count.
	TotalMs   float64 `json:"totalMs,omitempty"`
	LongestMs float64 `json:"longestMs,omitempty"`
	// WorstValue is the lowest rate observed inside any episode. For the
	// keyframe-only failure mode this lands on the GOP cadence itself, which
	// is the tell.
	WorstValue float64 `json:"worstValue,omitempty"`
	// Baseline is what the dips are relative to, carried so a reader can see
	// the judgement rather than trust it.
	Baseline float64 `json:"baseline"`
	// Share is the fraction of the window spent in a dip.
	Share float64 `json:"share,omitempty"`
	// Deltas are how far the delivery counters advanced INSIDE the episodes.
	// This is the half that turns "your fps dipped" into "your fps dipped
	// because every GOP was broken" — playbook row 9's ratio measured in the
	// window where it is legible instead of across a session that dilutes it.
	Deltas map[string]float64 `json:"deltas,omitempty"`
}

// viewerDipCounters / broadcasterDipCounters are the counters whose advance
// inside a dip localizes its cause.
var viewerDipCounters = []string{
	"reorderGapResyncs", "keyframeStreamsReceived",
	"framesDroppedIncomplete", "framesDroppedLate", "framesDiscardedAwaitingKey",
}

var broadcasterDipCounters = []string{
	"droppedFrames", "fpsGateDropped", "keyframeStreamsFailed",
	"FramesDroppedAtSend", "KeyframeStreamsFailed",
}

// PrimarySeries names the rate a role is judged on: what that side actually
// delivered. One series per role — more would multiply verdicts without adding
// information, since the counter deltas are what localize the cause.
func PrimarySeries(role string) (field string, counters []string) {
	if role == "broadcaster" {
		return "sentFps", broadcasterDipCounters
	}
	return "receivedFps", viewerDipCounters
}

// TrimSample keeps only what the detector reads.
//
// It exists for the live projection, which is the one place a sample is HELD
// rather than streamed past: a rolling window of full ~80-field stats maps,
// times every viewer on the fleet, is hundreds of megabytes of retained
// telemetry to answer a question about six numbers. The stored-session path
// does not need it — those samples are read once from disk and released.
func TrimSample(s Sample, field string, counters []string) Sample {
	keep := make(map[string]any, len(counters)+2)
	if v, ok := s.Stats[field]; ok {
		keep[field] = v
	}
	for _, c := range counters {
		if v, ok := s.Stats[c]; ok {
			keep[c] = v
		}
	}
	// The interval minimum is the whole reason a sub-interval dip is visible;
	// dropping it here would silently undo the client-side half of D16.
	if m := schema.Nested(s.Stats, IntervalMinField); m != nil {
		if lo, ok := schema.Number(m, field); ok {
			keep[IntervalMinField] = map[string]any{field: lo}
		}
	}
	return Sample{TMs: s.TMs, Stats: keep}
}

// DetectEpisodes finds the dips in one series. Returns nil when the window
// cannot support the judgement at all — too few samples, or a baseline so low
// that "half of it" is noise. Absent is the honest answer there; a zeroed
// Episodes would claim a measurement that did not happen.
func DetectEpisodes(samples []Sample, field string, counters []string) *Episodes {
	if len(samples) < 2 {
		return nil
	}
	// schema.Number rejects non-finite values outright, so anything collected
	// here is a real reading — no NaN can reach the baseline or a comparison.
	values := make([]float64, 0, len(samples))
	for _, s := range samples {
		if v, ok := schema.Number(s.Stats, field); ok {
			values = append(values, v)
		}
	}
	if len(values) < 2 {
		return nil
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	baseline := quantile(sorted, 0.50)
	if baseline < MinBaselineFps {
		return nil
	}
	threshold := baseline * DipRatio

	ep := &Episodes{Baseline: baseline, WorstValue: math.Inf(1)}
	var windowMs float64
	// runStart is the index of the first sample of the episode in progress.
	// runWorst is tracked PER RUN rather than globally, so a run the guard
	// below discards cannot leave its low-water mark behind on the result.
	runStart := -1
	var runMs, runWorst float64

	closeRun := func(end int) {
		if runStart < 0 {
			return
		}
		// An episode is a FALL from a baseline, and a run that begins at the
		// first sample is not one — nothing was observed to fall. That is the
		// shape of every stream STARTING: the viewer reports zeros before the
		// first frame decodes (the e2e harness settles for exactly these
		// "pre-decode zeros"), then ramps to its rate.
		//
		// Without this a warmup would be reported as a collapse on every
		// healthy session's first seconds — the false-positive class the
		// telemetry E2E exists to catch, and the one this detector is most
		// likely to produce, because a starting stream looks locally identical
		// to a collapsing one. The difference is only visible in what came
		// before, and at index 0 nothing did.
		//
		// A stream that is genuinely bad from its first sample is not lost: it
		// drags the baseline down with it, where MinBaselineFps and the
		// steady-state rules take over.
		if runStart == 0 {
			runStart, runMs = -1, 0
			return
		}
		ep.Count++
		ep.TotalMs += runMs
		if runMs > ep.LongestMs {
			ep.LongestMs = runMs
		}
		if runWorst < ep.WorstValue {
			ep.WorstValue = runWorst
		}
		// The counters' advance across the episode, measured from the last
		// sample BEFORE it so the dip's onset is included. A counter that went
		// backwards is a restart, not negative traffic, and is skipped.
		base := runStart - 1
		if base < 0 {
			base = 0
		}
		for _, c := range counters {
			from, okFrom := schema.Number(samples[base].Stats, c)
			to, okTo := schema.Number(samples[end].Stats, c)
			if !okFrom || !okTo || to < from {
				continue
			}
			if ep.Deltas == nil {
				ep.Deltas = map[string]float64{}
			}
			ep.Deltas[c] += to - from
		}
		runStart, runMs = -1, 0
	}

	for i, s := range samples {
		// Each sample stands for the interval since its predecessor. The first
		// has none, so it borrows the report cadence.
		interval := float64(FallbackIntervalMs)
		if i > 0 {
			if d := samples[i].TMs - samples[i-1].TMs; d > 0 {
				interval = d
			}
		}
		windowMs += interval

		v, ok := effectiveValue(s.Stats, field)
		if !ok {
			// A sample that did not report the rate cannot extend or end an
			// episode: it is an absence of evidence, and treating it as
			// "healthy" would silently split one collapse into two.
			continue
		}
		if v <= threshold {
			if runStart < 0 {
				runStart, runWorst = i, math.Inf(1)
			}
			runMs += interval
			if v < runWorst {
				runWorst = v
			}
			continue
		}
		closeRun(i - 1)
	}
	closeRun(len(samples) - 1)

	if windowMs > 0 {
		ep.Share = ep.TotalMs / windowMs
	}
	if math.IsInf(ep.WorstValue, 1) {
		ep.WorstValue = 0
	}
	// A zero-count result is returned, not discarded: "we looked and the stream
	// was steady" is evidence, and "we could not look" is not. Only the guards
	// above return nil, and they are the cases where no judgement was possible.
	return ep
}

// Facts renders an Episodes into the fact names the rules read.
//
// It lives here rather than in either producer because BOTH must emit the same
// names from the same numbers — the live projection over its rolling window and
// the read path over the stored session. One derivation, two windows (§4.8.3).
//
// Every name is emitted whenever the detector ran, including as zero. That is
// what lets a steady session read `passed` on these rules instead of
// `unavailable`: a measured zero is evidence of health, an absent fact is not.
func (e *Episodes) Facts() map[string]float64 {
	if e == nil {
		return nil
	}
	return map[string]float64{
		"fpsDipEpisodes":  float64(e.Count),
		"fpsDipShare":     e.Share,
		"fpsDipWorstFps":  e.WorstValue,
		"fpsDipLongestMs": e.LongestMs,
		"fpsDipResyncs":   e.Deltas["reorderGapResyncs"],
		"fpsDipKeyframes": e.Deltas["keyframeStreamsReceived"],
	}
}

// EpisodeFactNames is exactly the set Facts emits. A producer that caches
// these needs it to know what to CLEAR when a window stops being judgeable —
// leaving the last judgement standing would keep a rule firing on a session
// nothing can currently say anything about.
var EpisodeFactNames = []string{
	"fpsDipEpisodes", "fpsDipShare", "fpsDipWorstFps",
	"fpsDipLongestMs", "fpsDipResyncs", "fpsDipKeyframes",
}

// effectiveValue reads the worst the client saw for this field in this sample:
// the nested `intervalMin` reading where the client reported one, otherwise the
// sample's own value.
//
// Only the DETECTOR reads it. Baselines and every funnel ratio keep reading the
// primary field, so a client that starts reporting minima cannot shift a median
// or make a ratio fire (D16).
func effectiveValue(stats map[string]any, field string) (float64, bool) {
	v, ok := schema.Number(stats, field)
	if m := schema.Nested(stats, IntervalMinField); m != nil {
		if lo, okLo := schema.Number(m, field); okLo && (!ok || lo < v) {
			return lo, true
		}
	}
	return v, ok
}
