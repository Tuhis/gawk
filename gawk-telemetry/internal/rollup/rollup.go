// Package rollup computes R28's permanent per-session summary (docs/33 D4 +
// §4.5).
//
// Raw samples are disposable — 14 days and gone, and their shape may drift
// with every milestone. **The rollup row is permanent**, so it is the only
// place where getting the schema wrong is expensive. Three rules follow:
//
//  1. **Additive forever.** Fields are appended, never renamed or repurposed.
//     A reader must tolerate rows from older releases missing newer fields,
//     which sparse JSON gives for free and which the query layer must not
//     undermine by assuming presence.
//  2. **Percentiles, not means, for anything experiential.** A mean fps over a
//     session with one 4-second freeze looks fine. Median + p95 (+ p05 where
//     the bad tail is low, e.g. fps) is the shape.
//  3. **Typed by construction.** The row is computed from already-validated
//     inputs, so a value that could not be computed is **absent** — never a
//     coerced guess, a null, or a zero standing in for "unknown". Every
//     numeric field is a pointer for exactly that reason.
package rollup

import (
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/Tuhis/gawk/gawk-telemetry/internal/schema"
)

// Stat is the percentile summary of one measured series. Absent (a nil *Stat)
// when the series had no usable values at all — which, after schema
// sanitizing, is the only way it can fail to be numbers.
type Stat struct {
	N      int     `json:"n"`
	Min    float64 `json:"min"`
	P05    float64 `json:"p05"`
	Median float64 `json:"median"`
	P95    float64 `json:"p95"`
	Max    float64 `json:"max"`
}

// Row is one permanent rollup line. Every optional numeric is a pointer:
// absent means "could not be computed", which a zero would silently misreport
// as a real measurement of zero (D15's typed-by-construction rule).
type Row struct {
	// --- Identity ---
	SessionID    string `json:"sessionId"`
	BroadcastKey string `json:"broadcastKey"`
	Role         string `json:"role"`
	AppVersion   string `json:"appVersion,omitempty"`
	Browser      string `json:"browser,omitempty"`
	OS           string `json:"os,omitempty"`
	StartedAt    int64  `json:"startedAt,omitempty"`
	EndedAt      int64  `json:"endedAt,omitempty"`
	DurationMs   int64  `json:"durationMs,omitempty"`
	// EndedCleanly distinguishes a session that said goodbye from one that
	// simply stopped being heard from. Both are normal; only the second can
	// also mean "the client died", which changes how much its last samples
	// are worth.
	EndedCleanly bool `json:"endedCleanly"`
	// RelayPod/RelayRole/RelayVersion and RelayCoverage are filled by the TM4
	// join. RelayCoverage is "none" | "partial" | "full" and is what stops a
	// verdict silently resting on a relay view that does not exist (D5).
	RelayPod      string `json:"relayPod,omitempty"`
	RelayRole     string `json:"relayRole,omitempty"`
	RelayCoverage string `json:"relayCoverage"`

	// --- Data quality (D15) ---
	// A verdict computed over a session with a high anomaly count is a verdict
	// to distrust, and this is what makes that visible.
	SchemaAnomalies schema.Anomalies `json:"schemaAnomalies"`
	Samples         int              `json:"samples"`
	Events          int              `json:"events"`
	SeqGaps         int              `json:"seqGaps"`
	Truncated       bool             `json:"truncated,omitempty"`

	// --- Configuration: what this session WAS ---
	Config map[string]string `json:"config,omitempty"`

	// --- Funnel, health, latency, audio ---
	// Keyed by field name so the row grows additively without a schema
	// migration: a new measured series appears as a new key, and an old reader
	// simply does not look for it.
	Series map[string]*Stat `json:"series,omitempty"`
	// Counters are end-of-session totals of cumulative fields (the last value
	// observed), which is what "how many stalls did this session have" means.
	Counters map[string]float64 `json:"counters,omitempty"`
	// Stalls is derived rather than reported: the client has no "stall count"
	// field, but timeSinceLastFrameMs crossing a threshold is one.
	Stalls         int      `json:"stalls"`
	LongestStallMs float64  `json:"longestStallMs"`
	TotalStallMs   float64  `json:"totalStallMs"`
	CloseCodes     []string `json:"closeCodes,omitempty"`
	Reconnects     int      `json:"reconnects"`

	// --- Relay side (joined, TM4) ---
	Relay map[string]float64 `json:"relay,omitempty"`

	// --- Verdict (TM6) ---
	// diagnose()'s ranked output at session end. Storing it is what makes
	// "has this got better since the R15 fix?" a single query over rollups
	// instead of a re-analysis of raw windows that no longer exist.
	Verdict json.RawMessage `json:"verdict,omitempty"`
}

// StallThresholdMs is when a gap in frames becomes a stall. Two GOPs at the
// 500 ms cadence: one missed keyframe is recovery, two is a freeze a viewer
// notices.
const StallThresholdMs = 1000

// The series a rollup summarizes, per role. Additive: appending here makes a
// new series appear on future rows, and old rows simply lack the key.
var viewerSeries = []string{
	"receivedFps", "decoderFps", "renderedFps",
	"capToRenderMs", "liveEdgeDriftMs", "timeSyncRttMs",
	"playoutOffsetMs", "arrivalJitterMs", "renderCadenceP95Ms", "decodeJitterMs",
	"decoderQueueDepth", "avSkewMs",
}

var broadcasterSeries = []string{
	"captureFps", "encoderFps", "sentFps", "encoderQueueDepth",
	"lastEncodeLatencyMs", "timeSyncRttMs", "viewerCount",
	// The native broadcaster's capitalized spellings (R14 marshals
	// engine.Stats with Go's default names).
	"EncoderFps", "SentFps", "TimeSyncRttMs", "ViewerCount", "KeyframeIntervalMs",
}

var viewerCounters = []string{
	"framesAssembled", "framesDroppedIncomplete", "framesDroppedLate",
	"reorderGapResyncs", "reorderKeyframeWaitDrops", "framesDiscardedAwaitingKey",
	"keyframeStreamsReceived", "configsApplied", "decodedFrames",
	"videoBytesReceived", "carrierStreams", "carrierStreamsAborted",
	"audioPacketsReceived", "audioPacketsDecoded",
}

var broadcasterCounters = []string{
	"encodedFrames", "keyframes", "droppedFrames", "fpsGateDropped",
	"datagramsSent", "bytesSent", "keyframeStreamsSent", "keyframeStreamsFailed",
	"autoStepDowns", "autoStepUps", "audioPacketsSent",
	"EncodedFrames", "Keyframes", "SentFrames", "DatagramsSent", "BytesSent",
	"KeyframeStreamsSent", "KeyframeStreamsFailed", "FramesDroppedAtSend",
}

// Config fields describe what a session WAS, as opposed to how it went.
var viewerConfig = []string{
	"deliveryMode", "playoutMode", "interpolation", "presentation",
	"renderer", "pipelineContext", "transport", "audioState", "avMaster",
}

var broadcasterConfig = []string{
	"autoRung", "autoCeiling", "pipelineContext", "audioState", "audioCodec",
	"Encoder", "Codec", "CapturePath",
}

// Input is one session's stored timeline, as the finalizer hands it over.
type Input struct {
	SessionID    string
	BroadcastKey string
	Role         string
	AppVersion   string
	Browser      string
	OS           string
	StartedAtMs  int64
	EndedAtMs    int64
	EndedCleanly bool
	Anomalies    schema.Anomalies
	SeqGaps      int
	Truncated    bool
	Samples      []Sample
	Events       []Event
}

// Sample is one observation from the stored timeline.
type Sample struct {
	TMs   float64
	Stats map[string]any
}

// Event is one narrative point.
type Event struct {
	TMs    float64
	Kind   string
	Detail string
}

// Compute builds the permanent row. It never fails: a session with no usable
// samples still produces a row (identity + data quality), because "this
// session reported nothing usable" is itself the answer to a question someone
// will ask.
func Compute(in Input) Row {
	r := Row{
		SessionID:       in.SessionID,
		BroadcastKey:    in.BroadcastKey,
		Role:            in.Role,
		AppVersion:      in.AppVersion,
		Browser:         in.Browser,
		OS:              in.OS,
		StartedAt:       in.StartedAtMs,
		EndedAt:         in.EndedAtMs,
		EndedCleanly:    in.EndedCleanly,
		SchemaAnomalies: in.Anomalies,
		Samples:         len(in.Samples),
		Events:          len(in.Events),
		SeqGaps:         in.SeqGaps,
		Truncated:       in.Truncated,
		RelayCoverage:   "none",
	}
	if in.EndedAtMs > in.StartedAtMs {
		r.DurationMs = in.EndedAtMs - in.StartedAtMs
	}

	seriesNames, counterNames, configNames := viewerSeries, viewerCounters, viewerConfig
	if in.Role == "broadcaster" {
		seriesNames, counterNames, configNames = broadcasterSeries, broadcasterCounters, broadcasterConfig
	}

	r.Series = map[string]*Stat{}
	for _, name := range seriesNames {
		values := collect(in.Samples, name)
		if st := summarize(values); st != nil {
			r.Series[name] = st
		}
	}
	if len(r.Series) == 0 {
		r.Series = nil
	}

	r.Counters = map[string]float64{}
	for _, name := range counterNames {
		// The LAST observed value: these are cumulative counters, so the final
		// reading is the session total. Absent when never reported — never a
		// zero standing in for "unknown".
		if v, ok := lastValue(in.Samples, name); ok {
			r.Counters[name] = v
		}
	}
	if len(r.Counters) == 0 {
		r.Counters = nil
	}

	r.Config = map[string]string{}
	for _, name := range configNames {
		if v, ok := lastString(in.Samples, name); ok {
			r.Config[name] = v
		}
	}
	// Resolution is worth having as one field rather than two, and it is the
	// first thing anyone asks about a stuttering stream.
	if w, okW := lastValue(in.Samples, "frameWidth"); okW {
		if h, okH := lastValue(in.Samples, "frameHeight"); okH {
			r.Config["resolution"] = formatDims(w, h)
		}
	}
	if len(r.Config) == 0 {
		r.Config = nil
	}

	r.Stalls, r.TotalStallMs, r.LongestStallMs = stalls(in.Samples)

	seenCode := map[string]bool{}
	for _, e := range in.Events {
		switch e.Kind {
		case "reconnect":
			r.Reconnects++
		}
		if code := closeCodeOf(e.Detail); code != "" && !seenCode[code] {
			seenCode[code] = true
			r.CloseCodes = append(r.CloseCodes, code)
		}
	}
	sort.Strings(r.CloseCodes)

	return r
}

// collect pulls one field's finite values across the timeline. A sample
// missing the field contributes nothing rather than a zero — the difference
// between "30, 30, 30" and "30, 0, 30" is the whole point.
func collect(samples []Sample, field string) []float64 {
	out := make([]float64, 0, len(samples))
	for _, s := range samples {
		if v, ok := schema.Number(s.Stats, field); ok {
			out = append(out, v)
		}
	}
	return out
}

func lastValue(samples []Sample, field string) (float64, bool) {
	for i := len(samples) - 1; i >= 0; i-- {
		if v, ok := schema.Number(samples[i].Stats, field); ok {
			return v, true
		}
	}
	return 0, false
}

func lastString(samples []Sample, field string) (string, bool) {
	for i := len(samples) - 1; i >= 0; i-- {
		if v, ok := schema.String(samples[i].Stats, field); ok && v != "" {
			return v, true
		}
	}
	return "", false
}

// summarize computes the percentile shape. Returns nil for an empty series so
// the field is ABSENT on the row rather than a row of zeros claiming a
// measurement that never happened.
func summarize(values []float64) *Stat {
	if len(values) == 0 {
		return nil
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	return &Stat{
		N:      len(sorted),
		Min:    sorted[0],
		P05:    quantile(sorted, 0.05),
		Median: quantile(sorted, 0.50),
		P95:    quantile(sorted, 0.95),
		Max:    sorted[len(sorted)-1],
	}
}

// quantile is nearest-rank on a sorted slice. Deliberately not interpolated:
// every value here is a real measurement, and an interpolated p95 is a number
// nothing ever observed — which is a bad thing to put in a verdict's evidence.
func quantile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil(q*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// stalls derives freeze episodes from timeSinceLastFrameMs. The clients have
// no "stall count" field — R28 adds no measurement (docs/33 §1.2) — but a
// frame gap crossing the threshold IS a stall, and counting the episodes
// rather than the samples is what makes "one 4 s freeze" read as one stall of
// 4 s instead of eight samples of nothing much.
func stalls(samples []Sample) (count int, total, longest float64) {
	inStall := false
	var peak float64
	for _, s := range samples {
		v, ok := schema.Number(s.Stats, "timeSinceLastFrameMs")
		if !ok {
			continue
		}
		if v >= StallThresholdMs {
			if !inStall {
				inStall = true
				count++
				peak = 0
			}
			if v > peak {
				peak = v
			}
			continue
		}
		if inStall {
			inStall = false
			total += peak
			if peak > longest {
				longest = peak
			}
		}
	}
	if inStall {
		// A session that ended mid-stall still had one, and it is probably why
		// it ended.
		total += peak
		if peak > longest {
			longest = peak
		}
	}
	return count, total, longest
}

// closeCodeOf extracts the numeric close code a client narrated in an event
// detail ("attempt 1 close 4002: ..."). The codes are the single most useful
// thing an event carries — 4000/4002/4004 each mean something completely
// different about why a session ended.
func closeCodeOf(detail string) string {
	i := strings.Index(detail, "close ")
	if i < 0 {
		return ""
	}
	rest := detail[i+len("close "):]
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	if end == 0 {
		return ""
	}
	return rest[:end]
}

func formatDims(w, h float64) string {
	return strconv.Itoa(int(w)) + "x" + strconv.Itoa(int(h))
}
