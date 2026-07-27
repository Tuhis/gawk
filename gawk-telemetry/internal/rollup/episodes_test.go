package rollup

import (
	"math"
	"testing"
)

// samplesAt builds a timeline at the default 2 s report cadence.
func samplesAt(values []float64, counters map[string][]float64) []Sample {
	out := make([]Sample, 0, len(values))
	for i, v := range values {
		st := map[string]any{"receivedFps": v}
		for name, series := range counters {
			st[name] = series[i]
		}
		out = append(out, Sample{TMs: float64(i) * 2000, Stats: st})
	}
	return out
}

func steady(n int, v float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = v
	}
	return out
}

func TestDetectEpisodesFindsTheDipAMedianHides(t *testing.T) {
	// 30 fps with two collapses to the GOP cadence. The median is 30, so every
	// percentile-based reading of this session looks fine.
	vals := steady(20, 30)
	vals[5], vals[6] = 2, 2
	vals[14] = 2

	ep := DetectEpisodes(samplesAt(vals, nil), "receivedFps", nil)
	if ep == nil {
		t.Fatal("no episodes detected on a session that collapses twice")
	}
	if ep.Count != 2 {
		t.Errorf("Count = %d, want 2 — contiguous samples are ONE episode", ep.Count)
	}
	if ep.Baseline != 30 {
		t.Errorf("Baseline = %v, want 30", ep.Baseline)
	}
	if ep.WorstValue != 2 {
		t.Errorf("WorstValue = %v, want 2", ep.WorstValue)
	}
	// Two samples plus one, each standing for its 2 s interval.
	if ep.TotalMs != 6000 {
		t.Errorf("TotalMs = %v, want 6000", ep.TotalMs)
	}
	if ep.LongestMs != 4000 {
		t.Errorf("LongestMs = %v, want 4000", ep.LongestMs)
	}
	if ep.Share <= 0 || ep.Share >= 1 {
		t.Errorf("Share = %v, want a fraction", ep.Share)
	}
}

// A detector that accuses everything is worth nothing.
func TestDetectEpisodesIsQuietOnASteadySession(t *testing.T) {
	ep := DetectEpisodes(samplesAt(steady(30, 60), nil), "receivedFps", nil)
	if ep == nil {
		t.Fatal("a steady session should still report a measured zero, not nothing")
	}
	if ep.Count != 0 {
		t.Errorf("Count = %d on a flat 60 fps session", ep.Count)
	}
	// The measured zero is what lets the rule read `passed` rather than
	// `unavailable` — evidence of health, not absence of evidence.
	if got := ep.Facts()["fpsDipEpisodes"]; got != 0 {
		t.Errorf("fpsDipEpisodes = %v, want a measured 0", got)
	}
}

// Self-relative by design: an absolute floor would accuse a deliberate low-rate
// stream forever.
func TestDetectEpisodesIgnoresSessionsWithNoBaselineToDipFrom(t *testing.T) {
	if ep := DetectEpisodes(samplesAt(steady(30, 3), nil), "receivedFps", nil); ep != nil {
		t.Errorf("a deliberate 3 fps stream produced episodes: %+v", ep)
	}
	// And a judgement cannot be made from a single sample at all.
	if ep := DetectEpisodes(samplesAt([]float64{30}, nil), "receivedFps", nil); ep != nil {
		t.Error("one sample was enough to claim a judgement")
	}
}

// The half that makes a dip actionable: what the counters did INSIDE it.
func TestDetectEpisodesAttributesCounterAdvanceToTheDip(t *testing.T) {
	vals := steady(12, 30)
	vals[5], vals[6] = 2, 2

	// Resyncs advance only across the dip; keyframes advance throughout.
	resyncs := make([]float64, 12)
	keyframes := make([]float64, 12)
	for i := range 12 {
		keyframes[i] = float64(i) * 4
		switch {
		case i < 5:
			resyncs[i] = 0
		case i <= 6:
			resyncs[i] = float64(i-4) * 4
		default:
			resyncs[i] = 8
		}
	}
	ep := DetectEpisodes(samplesAt(vals, map[string][]float64{
		"reorderGapResyncs": resyncs, "keyframeStreamsReceived": keyframes,
	}), "receivedFps", viewerDipCounters)
	if ep == nil {
		t.Fatal("no episode")
	}
	if got := ep.Deltas["reorderGapResyncs"]; got != 8 {
		t.Errorf("resync delta inside the dip = %v, want 8", got)
	}
	// Measured from the sample BEFORE the dip, so the onset is included:
	// index 4 → 6 is two intervals of 4 keyframes each.
	if got := ep.Deltas["keyframeStreamsReceived"]; got != 8 {
		t.Errorf("keyframe delta across the dip = %v, want 8", got)
	}
}

// A counter that goes backwards is a restart, never negative traffic.
func TestDetectEpisodesSkipsBackwardsCounters(t *testing.T) {
	vals := steady(10, 30)
	vals[5] = 2
	resyncs := []float64{50, 50, 50, 50, 50, 0, 0, 0, 0, 0}
	ep := DetectEpisodes(samplesAt(vals, map[string][]float64{"reorderGapResyncs": resyncs}),
		"receivedFps", viewerDipCounters)
	if ep == nil {
		t.Fatal("no episode")
	}
	if got, ok := ep.Deltas["reorderGapResyncs"]; ok && got < 0 {
		t.Errorf("a restart produced a negative delta: %v", got)
	}
}

// intervalMin is what makes a dip shorter than one report interval visible at
// all — the client emits one tick every ~2 s and discards the three between.
func TestDetectEpisodesReadsIntervalMinima(t *testing.T) {
	// Every emitted sample reads a healthy 30, but the client saw 2 between
	// two of them.
	samples := samplesAt(steady(10, 30), nil)
	samples[4].Stats[IntervalMinField] = map[string]any{"receivedFps": 2.0}

	ep := DetectEpisodes(samples, "receivedFps", nil)
	if ep == nil || ep.Count != 1 {
		t.Fatalf("sub-interval dip not detected: %+v", ep)
	}
	if ep.WorstValue != 2 {
		t.Errorf("WorstValue = %v, want 2", ep.WorstValue)
	}
	// And the baseline is untouched by it: minima inform the DETECTOR only,
	// never a median or a funnel ratio (D16).
	if ep.Baseline != 30 {
		t.Errorf("Baseline = %v, want 30 — intervalMin must not shift the baseline", ep.Baseline)
	}
}

// Durations come from real timestamps, never from a sample count.
func TestEpisodeDurationUsesRealTimestamps(t *testing.T) {
	samples := samplesAt(steady(6, 30), nil)
	// A client reporting every 5 s, not 2.
	for i := range samples {
		samples[i].TMs = float64(i) * 5000
	}
	samples[3].Stats["receivedFps"] = 2.0

	ep := DetectEpisodes(samples, "receivedFps", nil)
	if ep == nil {
		t.Fatal("no episode")
	}
	if ep.LongestMs != 5000 {
		t.Errorf("LongestMs = %v, want 5000 — the real cadence, not the default", ep.LongestMs)
	}
}

func TestFactsOnNilEpisodesIsEmpty(t *testing.T) {
	var ep *Episodes
	if got := ep.Facts(); got != nil {
		t.Errorf("nil episodes produced facts: %v", got)
	}
}

func TestDetectEpisodesToleratesMissingAndNonFiniteValues(t *testing.T) {
	samples := samplesAt(steady(10, 30), nil)
	delete(samples[4].Stats, "receivedFps")
	samples[7].Stats["receivedFps"] = math.NaN()
	// Must not panic, and must not invent an episode out of an absence.
	ep := DetectEpisodes(samples, "receivedFps", nil)
	if ep == nil {
		t.Fatal("no result")
	}
	if ep.Count != 0 {
		t.Errorf("Count = %d — a missing reading was treated as a dip", ep.Count)
	}
}

// EpisodeFactNames must stay exactly what Facts emits: a producer that caches
// these uses it to know what to CLEAR, so a name drifting out of the list
// would leave a stale verdict standing forever.
func TestEpisodeFactNamesMatchesFacts(t *testing.T) {
	ep := DetectEpisodes(samplesAt(steady(10, 30), nil), "receivedFps", nil)
	facts := ep.Facts()
	if len(facts) != len(EpisodeFactNames) {
		t.Fatalf("Facts emits %d names, EpisodeFactNames lists %d", len(facts), len(EpisodeFactNames))
	}
	for _, n := range EpisodeFactNames {
		if _, ok := facts[n]; !ok {
			t.Errorf("EpisodeFactNames lists %q, which Facts does not emit", n)
		}
	}
}

// The live projection holds a rolling window, so a sample kept there must
// carry only what the detector reads — and must NOT lose the interval minimum,
// which is the whole client-side half of D16.
func TestTrimSampleKeepsWhatTheDetectorNeeds(t *testing.T) {
	full := Sample{TMs: 4000, Stats: map[string]any{
		"receivedFps":             30.0,
		"reorderGapResyncs":       12.0,
		"keyframeStreamsReceived": 40.0,
		"capToRenderMs":           90.0, // not read by the detector
		"renderer":                "webgl",
		IntervalMinField:          map[string]any{"receivedFps": 2.0, "decoderFps": 5.0},
	}}
	got := TrimSample(full, "receivedFps", viewerDipCounters)

	if got.TMs != 4000 {
		t.Errorf("TMs = %v, want 4000", got.TMs)
	}
	for _, keep := range []string{"receivedFps", "reorderGapResyncs", "keyframeStreamsReceived"} {
		if _, ok := got.Stats[keep]; !ok {
			t.Errorf("trimmed sample lost %q", keep)
		}
	}
	for _, drop := range []string{"capToRenderMs", "renderer"} {
		if _, ok := got.Stats[drop]; ok {
			t.Errorf("trimmed sample kept %q, which the detector never reads", drop)
		}
	}
	// The minimum survives, and a trimmed sample still detects the dip.
	ep := DetectEpisodes([]Sample{
		{TMs: 0, Stats: map[string]any{"receivedFps": 30.0}},
		{TMs: 2000, Stats: map[string]any{"receivedFps": 30.0}},
		got,
		{TMs: 6000, Stats: map[string]any{"receivedFps": 30.0}},
	}, "receivedFps", nil)
	if ep == nil || ep.Count != 1 || ep.WorstValue != 2 {
		t.Errorf("trimming lost the interval minimum: %+v", ep)
	}
}

// --- D17: the configured target ---------------------------------------------

// A broadcaster row must carry what the stream was ASKED to be. Without it,
// "30 fps" reads identically whether 30 or 60 was requested.
func TestRollupCapturesTheBrowserBroadcasterTarget(t *testing.T) {
	samples := []Sample{
		{TMs: 0, Stats: map[string]any{
			"sentFps": 30.0, "targetWidth": 1920.0, "targetHeight": 1080.0,
			"targetFps": 60.0, "targetBitrateBps": 8_000_000.0,
			"codec": "avc1.640028", "acceleration": "prefer-hardware",
		}},
		{TMs: 2000, Stats: map[string]any{"sentFps": 30.0}},
	}
	r := Compute(Input{SessionID: "s", Role: "broadcaster", Samples: samples})

	for k, want := range map[string]string{
		"resolution":        "1920x1080",
		"targetFps":         "60",
		"targetBitrateKbps": "8000",
		"codec":             "avc1.640028",
		"acceleration":      "prefer-hardware",
	} {
		if got := r.Config[k]; got != want {
			t.Errorf("Config[%q] = %q, want %q", k, got, want)
		}
	}
}

// The native engine marshals engine.Stats with Go's capitalized names. One
// query must cover both producers, or half the fleet reports no configuration.
func TestRollupCapturesTheNativeBroadcasterTarget(t *testing.T) {
	samples := []Sample{
		{TMs: 0, Stats: map[string]any{
			"SentFps": 55.0, "Width": 2560.0, "Height": 1440.0,
			"Fps": 60.0, "BitrateBps": 12_000_000.0, "Encoder": "nvh264enc",
		}},
		{TMs: 2000, Stats: map[string]any{"SentFps": 55.0}},
	}
	r := Compute(Input{SessionID: "s", Role: "broadcaster", Samples: samples})

	if got := r.Config["resolution"]; got != "2560x1440" {
		t.Errorf("resolution = %q, want 2560x1440", got)
	}
	if got := r.Config["targetFps"]; got != "60" {
		t.Errorf("targetFps = %q, want 60", got)
	}
	if got := r.Config["targetBitrateKbps"]; got != "12000" {
		t.Errorf("targetBitrateKbps = %q, want 12000", got)
	}
}

// A viewer's resolution is what it DECODED, and must keep coming from the
// viewer fields — the two are different measurements of different things.
func TestViewerResolutionStillComesFromDecodedFrames(t *testing.T) {
	samples := []Sample{
		{TMs: 0, Stats: map[string]any{"receivedFps": 30.0, "frameWidth": 1280.0, "frameHeight": 720.0}},
		{TMs: 2000, Stats: map[string]any{"receivedFps": 30.0}},
	}
	r := Compute(Input{SessionID: "s", Role: "viewer", Samples: samples})
	if got := r.Config["resolution"]; got != "1280x720" {
		t.Errorf("resolution = %q, want 1280x720", got)
	}
}
