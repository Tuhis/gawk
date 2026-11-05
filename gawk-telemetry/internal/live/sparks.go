package live

// R36 TU7: the two-minute fps tail behind each live session row (UD14's
// sparkline).
//
// It is NOT part of the snapshot. `/live` and its stream are bounded (UD1,
// UD22), and a tail per session per tick would multiply that payload by the
// tail's length on exactly the broadcasts where it is largest. The page asks
// for one broadcast's tails, while that broadcast's card is open
// (`GET /live/sparks`), and this answers from a small ring per session that
// ingest already feeds — no file is read, and nothing new is measured (UD3):
// the values are the client's own decoderFps / receivedFps / encoderFps.

import (
	"math"
	"sort"
	"time"

	"github.com/Tuhis/gawk/gawk-telemetry/internal/ingest"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/schema"
)

// SparkWindow is how much of each session's recent past a tail covers.
const SparkWindow = 2 * time.Minute

// sparkRingCap bounds one session's ring. Points older than the window are
// dropped as they arrive; the cap only matters for a client reporting faster
// than the default 2 s (the relay may ask for 500 ms), and costs at most
// ~6 KB per session.
const sparkRingCap = 256

// sparkOffsetBatches is how many recent batches the clock offset is the
// minimum over (~2 minutes at a 10 s flush).
const sparkOffsetBatches = 12

// sparkSteps are the bucket widths a tail may use, each dividing SparkWindow
// exactly. A tail's step is the narrowest of them at least sparkStepFactor
// times the broadcast's typical (median) sample gap. A bucket narrower than
// the gaps would be empty with nothing missing — a break that is not one
// (UD9): a web client emits a sample at the first 500 ms stats tick at least
// 2 s after the last, so its gaps alternate between 2.0 and 2.5 s, and a 2 s
// grid would be empty about one bucket in five. Every gap under the step
// leaves no bucket empty, while a lost batch (a hole of 10 s or more) still
// does. The median, not the mean, so the hole cannot widen the grid it
// should show on.
var sparkSteps = []int64{4000, 5000, 6000, 8000, 10000, 12000, 15000, 20000, 24000, 30000}

// sparkStepFactor is how far past the median gap a sample may land without
// leaving an empty bucket: the 2.5 s of a 2 s cadence, plus timer jitter.
const sparkStepFactor = 1.6

// Sparks is one broadcast's tails.
type Sparks struct {
	// AtMs is the end of the newest bucket, on this service's clock.
	AtMs int64 `json:"atMs"`
	// StepMs is the bucket width, chosen from the broadcast's own sample
	// cadence (sparkSteps) so that a null means a sample really did not
	// arrive; 4000 at the default 2 s report interval.
	StepMs   int64 `json:"stepMs"`
	WindowMs int64 `json:"windowMs"`
	// Sessions maps a sessionId to its tail. A session with no sample of its
	// field in the window is absent, not a row of nulls.
	Sessions map[string]SparkSeries `json:"sessions"`
}

// SparkSeries is one session's tail.
type SparkSeries struct {
	// Field is what Values are: decoderFps (or receivedFps, for a viewer
	// that does not report it) or encoderFps — the second figure of the live
	// table's fps column.
	Field string `json:"field"`
	// Values are oldest → newest, one per StepMs bucket ending at AtMs: the
	// LOWEST sample in the bucket, so a dip cannot be averaged away, or null
	// where no sample landed. Never interpolated (UD9). Rounded to 0.1.
	Values []*float64 `json:"values"`
	// ThroughMs is the newest sample's time. Buckets after it are empty
	// because the client's next batch has not arrived yet (it flushes every
	// ~10 s), not because samples went missing: the page can draw the two
	// differently.
	ThroughMs int64 `json:"throughMs,omitempty"`
}

// sparkPoint is one sample, at the client's own clock: its session start plus
// the sample's monotonic offset.
type sparkPoint struct {
	clientMs          int64
	primary, fallback float64 // NaN when the sample did not report it
}

// sparkRing is one session's tail and the clock offset that places it.
type sparkRing struct {
	startedAtMs int64
	pts         []sparkPoint
	// offsets are recent batches' (arrival − client time of their newest
	// sample). The client's clock is not this service's (#376 met a tab 48
	// minutes off), so the offset is measured, like the timeline's
	// (broadcast.go): the minimum over recent batches is the skew plus the
	// shortest sample-to-arrival delay seen, which places a sample at its
	// true time to within that delay and never after its own arrival.
	offsets []int64
}

// sparkFields names the fields a role's tail is drawn from.
func sparkFields(role string) (primary, fallback string) {
	if role == "broadcaster" {
		return "encoderFps", ""
	}
	return "decoderFps", "receivedFps"
}

// fold adds one accepted batch to the ring. now is the arrival.
func (r *sparkRing) fold(a ingest.Accepted, role string, now time.Time) {
	if a.StartedAtMs <= 0 || len(a.Samples) == 0 {
		// Without the session's start there is no absolute time to place a
		// sample at; guessing one would draw it in the wrong place.
		return
	}
	if a.StartedAtMs != r.startedAtMs {
		// A new session start is a new clock origin: the old points and the
		// old offset no longer line up with it.
		*r = sparkRing{startedAtMs: a.StartedAtMs}
	}
	primary, fallback := sparkFields(role)
	for _, smp := range a.Samples {
		p := sparkPoint{clientMs: a.StartedAtMs + int64(smp.TMs), primary: math.NaN(), fallback: math.NaN()}
		if v, ok := schema.Number(smp.Stats, primary); ok {
			p.primary = v
		}
		if fallback != "" {
			if v, ok := schema.Number(smp.Stats, fallback); ok {
				p.fallback = v
			}
		}
		r.pts = append(r.pts, p)
	}
	newest := a.StartedAtMs + int64(a.Samples[len(a.Samples)-1].TMs)
	r.offsets = append(r.offsets, now.UnixMilli()-newest)
	if n := len(r.offsets); n > sparkOffsetBatches {
		r.offsets = append([]int64(nil), r.offsets[n-sparkOffsetBatches:]...)
	}

	// Batches arrive in order but a retried one may not: keep time order,
	// then keep only what a tail can still show.
	sort.SliceStable(r.pts, func(i, j int) bool { return r.pts[i].clientMs < r.pts[j].clientMs })
	keepFrom := r.pts[len(r.pts)-1].clientMs - (SparkWindow + 30*time.Second).Milliseconds()
	cut := sort.Search(len(r.pts), func(i int) bool { return r.pts[i].clientMs >= keepFrom })
	if len(r.pts)-cut > sparkRingCap {
		cut = len(r.pts) - sparkRingCap
	}
	if cut > 0 {
		// Copy, not reslice: a reslice keeps the old backing array alive.
		r.pts = append([]sparkPoint(nil), r.pts[cut:]...)
	}
}

func (r *sparkRing) offset() int64 {
	if len(r.offsets) == 0 {
		return 0
	}
	m := r.offsets[0]
	for _, o := range r.offsets[1:] {
		m = min(m, o)
	}
	return m
}

// medianGapMs is the session's typical sample spacing inside the window, or
// 0 when it has too few samples to say.
func (r *sparkRing) medianGapMs(fromServerMs int64) int64 {
	off := r.offset()
	var gaps []int64
	prev := int64(-1)
	for _, p := range r.pts {
		at := p.clientMs + off
		if at < fromServerMs {
			continue
		}
		if prev >= 0 && at > prev {
			gaps = append(gaps, at-prev)
		}
		prev = at
	}
	if len(gaps) == 0 {
		return 0
	}
	sort.Slice(gaps, func(i, j int) bool { return gaps[i] < gaps[j] })
	return gaps[len(gaps)/2]
}

// series renders the ring onto the grid, or ok=false when nothing in the
// window reported the field.
func (r *sparkRing) series(role string, atMs, step int64) (SparkSeries, bool) {
	fromMs := atMs - SparkWindow.Milliseconds()
	n := int(SparkWindow.Milliseconds() / step)
	off := r.offset()
	primary, fallback := sparkFields(role)

	field, pick := primary, func(p sparkPoint) float64 { return p.primary }
	if !r.has(fromMs, atMs, off, pick) {
		if fallback == "" {
			return SparkSeries{}, false
		}
		field, pick = fallback, func(p sparkPoint) float64 { return p.fallback }
		if !r.has(fromMs, atMs, off, pick) {
			return SparkSeries{}, false
		}
	}

	out := SparkSeries{Field: field, Values: make([]*float64, n)}
	for _, p := range r.pts {
		at := p.clientMs + off
		v := pick(p)
		if at < fromMs || at > atMs || math.IsNaN(v) {
			continue
		}
		j := min(int((at-fromMs)/step), n-1)
		if out.Values[j] == nil || v < *out.Values[j] {
			out.Values[j] = &v
		}
		out.ThroughMs = max(out.ThroughMs, at)
	}
	for _, v := range out.Values {
		if v != nil {
			*v = math.Round(*v*10) / 10
		}
	}
	return out, true
}

func (r *sparkRing) has(fromMs, atMs, off int64, pick func(sparkPoint) float64) bool {
	for _, p := range r.pts {
		if at := p.clientMs + off; at >= fromMs && at <= atMs && !math.IsNaN(pick(p)) {
			return true
		}
	}
	return false
}

// Sparks renders one live broadcast's tails; ok is false when the projection
// holds no such broadcast.
func (p *Projection) Sparks(broadcastKey string) (Sparks, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	p.sweepLocked(now)
	b, ok := p.bcasts[broadcastKey]
	if !ok {
		return Sparks{}, false
	}
	atMs := now.UnixMilli()
	out := Sparks{AtMs: atMs, WindowMs: SparkWindow.Milliseconds(), Sessions: map[string]SparkSeries{}}

	// One step for the whole broadcast, so its rows share one grid, from the
	// median session's median gap: one slow reporter does not coarsen
	// everyone's tail.
	var gaps []int64
	for _, s := range b.sessions {
		if g := s.spark.medianGapMs(atMs - SparkWindow.Milliseconds()); g > 0 {
			gaps = append(gaps, g)
		}
	}
	out.StepMs = sparkSteps[0]
	if len(gaps) > 0 {
		sort.Slice(gaps, func(i, j int) bool { return gaps[i] < gaps[j] })
		want := sparkStepFactor * float64(gaps[len(gaps)/2])
		out.StepMs = sparkSteps[len(sparkSteps)-1]
		for _, st := range sparkSteps {
			if float64(st) >= want {
				out.StepMs = st
				break
			}
		}
	}

	// Anchor the window to multiples of the step, ending at the boundary at or
	// after now: bucket edges then stay put from one poll to the next, so a
	// sample does not hop between buckets (and the line wobble) when nothing
	// new arrived. The last bucket may still be filling.
	out.AtMs = (atMs + out.StepMs - 1) / out.StepMs * out.StepMs
	atMs = out.AtMs

	for id, s := range b.sessions {
		if series, ok := s.spark.series(s.view.Role, atMs, out.StepMs); ok {
			out.Sessions[id] = series
		}
	}
	return out, true
}
