package screening

import (
	"math"
	"time"
)

// WindowRule is D7's hysteresis: over the last Size samples, a window TRIPS
// when at least Hits score at or above Threshold, and a tripped window CLEARS
// only when all Size are below it. It is one definition for the live sampler
// and for gawk-admin's replay (D21), so "what would this setting have done?"
// is answered by the code that would have done it.
type WindowRule struct {
	Threshold float64
	Size      int
	Hits      int
}

// Validate checks 0 < Threshold ≤ 1, 1 ≤ Size ≤ MaxWindow, 1 ≤ Hits ≤ Size.
func (r WindowRule) Validate() error {
	if math.IsNaN(r.Threshold) || r.Threshold <= 0 || r.Threshold > 1 {
		return invalid("threshold %v, want (0, 1]", r.Threshold)
	}
	if r.Size < 1 || r.Size > MaxWindow {
		return invalid("window %d, want 1-%d", r.Size, MaxWindow)
	}
	if r.Hits < 1 || r.Hits > r.Size {
		return invalid("windowHits %d, want 1-%d", r.Hits, r.Size)
	}
	return nil
}

// Transition is what one observed sample did to a window.
type Transition int

const (
	// Unchanged: the window stayed tripped or stayed clear.
	Unchanged Transition = iota
	// Tripped: the window went from clear to tripped on this sample.
	Tripped
	// Cleared: the window went from tripped to clear on this sample.
	Cleared
)

// Window is one publisher session's sliding window. The zero value is not
// usable; build one with NewWindow. Not safe for concurrent use.
type Window struct {
	rule    WindowRule
	scores  []float64 // ring, len == rule.Size
	next    int       // ring write position
	n       int       // samples held, ≤ rule.Size
	tripped bool
}

// NewWindow returns an empty window for rule, which the caller has
// validated.
func NewWindow(rule WindowRule) *Window {
	return &Window{rule: rule, scores: make([]float64, rule.Size)}
}

// Observe adds one sample's score and reports the transition it caused.
func (w *Window) Observe(score float64) Transition {
	w.scores[w.next] = score
	w.next = (w.next + 1) % len(w.scores)
	if w.n < len(w.scores) {
		w.n++
	}
	hits := w.Hits()
	switch {
	case !w.tripped && hits >= w.rule.Hits:
		w.tripped = true
		return Tripped
	case w.tripped && w.n == len(w.scores) && hits == 0:
		w.tripped = false
		return Cleared
	}
	return Unchanged
}

// Tripped reports whether the window is tripped.
func (w *Window) Tripped() bool { return w.tripped }

// Len is how many samples the window holds.
func (w *Window) Len() int { return w.n }

// Hits is how many held samples score at or above the threshold.
func (w *Window) Hits() int {
	hits := 0
	for _, s := range w.Scores() {
		if s >= w.rule.Threshold {
			hits++
		}
	}
	return hits
}

// Scores returns the held samples, oldest first, in a fresh slice.
func (w *Window) Scores() []float64 {
	out := make([]float64, 0, w.n)
	start := (w.next - w.n + len(w.scores)) % len(w.scores)
	for i := 0; i < w.n; i++ {
		out = append(out, w.scores[(start+i)%len(w.scores)])
	}
	return out
}

// SampleScore is D7's per-sample score: the maximum confidence among labels.
// A label absent from scores counts as 0.
func SampleScore(scores map[string]float64, labels []string) float64 {
	max := 0.0
	for _, l := range labels {
		if v := scores[l]; v > max {
			max = v
		}
	}
	return max
}

// StoredSample is one row of gawk-admin's screening_samples as replay reads
// it: the sample's per-session sequence number (1-based), its time, and its
// score under the replay's label set.
type StoredSample struct {
	Seq   int64
	At    time.Time
	Score float64
}

// Episode is one trip found by ReplaySession: when the window tripped and the
// highest score seen while it stayed tripped.
type Episode struct {
	At        time.Time
	PeakScore float64
}

// ReplaySession runs rule over one publisher session as gawk-admin stored it
// (D20, D21): taken samples numbered 1…taken, of which only those in stored
// (ascending Seq) were kept. A missing number is a sample that scored below
// the floor, so it counts as 0 — below every threshold the caller allows
// (a threshold under the floor is refused before this runs), which keeps the
// replay exact. Stored samples numbered past taken are ignored.
func ReplaySession(rule WindowRule, taken int64, stored []StoredSample) []Episode {
	var out []Episode
	w := NewWindow(rule)
	i := 0
	var cur *Episode
	for seq := int64(1); seq <= taken; seq++ {
		score := 0.0
		var at time.Time
		for i < len(stored) && stored[i].Seq < seq {
			i++
		}
		if i < len(stored) && stored[i].Seq == seq {
			score, at = stored[i].Score, stored[i].At
		}
		switch w.Observe(score) {
		case Tripped:
			// A trip needs this sample at or above the threshold, and every
			// such sample is stored, so `at` is always set here.
			out = append(out, Episode{At: at, PeakScore: score})
			cur = &out[len(out)-1]
		case Cleared:
			cur = nil
		default:
			if cur != nil && score > cur.PeakScore {
				cur.PeakScore = score
			}
		}
	}
	return out
}
