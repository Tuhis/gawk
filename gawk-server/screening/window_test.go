package screening

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

var rule846 = WindowRule{Threshold: 0.8, Size: 6, Hits: 4}

func TestWindowRuleValidate(t *testing.T) {
	if err := rule846.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []WindowRule{
		{Threshold: 0, Size: 6, Hits: 4},
		{Threshold: 1.01, Size: 6, Hits: 4},
		{Threshold: 0.8, Size: 0, Hits: 0},
		{Threshold: 0.8, Size: 6, Hits: 0},
		{Threshold: 0.8, Size: 6, Hits: 7},
		{Threshold: 0.8, Size: MaxWindow + 1, Hits: 4},
	} {
		if err := bad.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v: Validate() = %v, want ErrInvalid", bad, err)
		}
	}
}

// Trip, hold, clear: the table D7 describes.
func TestWindowHysteresis(t *testing.T) {
	H, L := 0.9, 0.1
	steps := []struct {
		score float64
		want  Transition
	}{
		{H, Unchanged}, {L, Unchanged}, {H, Unchanged}, {H, Unchanged},
		{H, Tripped},                   // 4th hit inside the window: trips before the window is full
		{L, Unchanged}, {L, Unchanged}, // holds while hits remain
		{H, Unchanged},
		{L, Unchanged}, {L, Unchanged}, {L, Unchanged}, {L, Unchanged}, {L, Unchanged},
		{L, Cleared}, // only when all six are below
		{L, Unchanged},
		{H, Unchanged}, {H, Unchanged}, {H, Unchanged},
		{H, Tripped}, // a second episode trips again
	}
	w := NewWindow(rule846)
	for i, s := range steps {
		if got := w.Observe(s.score); got != s.want {
			t.Fatalf("step %d (score %v): transition %v, want %v (scores %v)", i, s.score, got, s.want, w.Scores())
		}
	}
	if !w.Tripped() || w.Len() != 6 {
		t.Fatalf("tripped=%v len=%d", w.Tripped(), w.Len())
	}
}

// A score exactly at the threshold is a hit.
func TestWindowThresholdInclusive(t *testing.T) {
	w := NewWindow(WindowRule{Threshold: 0.8, Size: 2, Hits: 1})
	if got := w.Observe(0.8); got != Tripped {
		t.Fatalf("0.8 at threshold 0.8: %v, want Tripped", got)
	}
}

// A flickering score never clears a tripped window while any hit remains.
func TestWindowFlickerHolds(t *testing.T) {
	w := NewWindow(rule846)
	for i := 0; i < 4; i++ {
		w.Observe(0.95)
	}
	for i := 0; i < 50; i++ {
		score := 0.1
		if i%5 == 0 {
			score = 0.95
		}
		if got := w.Observe(score); got == Cleared {
			t.Fatalf("cleared at step %d with a hit in the last six", i)
		}
	}
}

func TestWindowScoresOldestFirst(t *testing.T) {
	w := NewWindow(WindowRule{Threshold: 0.5, Size: 3, Hits: 3})
	for _, s := range []float64{0.1, 0.2, 0.3, 0.4} {
		w.Observe(s)
	}
	if got := w.Scores(); !reflect.DeepEqual(got, []float64{0.2, 0.3, 0.4}) {
		t.Fatalf("Scores() = %v", got)
	}
}

func TestSampleScore(t *testing.T) {
	scores := map[string]float64{"A": 0.2, "B": 0.7, "C": 0.9}
	if got := SampleScore(scores, []string{"A", "B"}); got != 0.7 {
		t.Fatalf("SampleScore = %v, want 0.7 (C is not a flagged label)", got)
	}
	if got := SampleScore(scores, []string{"Z"}); got != 0 {
		t.Fatalf("absent label scored %v", got)
	}
}

// Replay over a session with gaps: the gaps are below-floor samples, and the
// hand-computed answer for three settings holds.
func TestReplaySessionWithGaps(t *testing.T) {
	t0 := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	at := func(seq int64) time.Time { return t0.Add(time.Duration(seq) * 5 * time.Second) }
	// 20 samples taken; stored (≥ floor 0.3) are:
	//   3:0.85 4:0.9 5:0.82 7:0.95   (one episode under 0.8/6/4 at seq 7)
	//   15:0.5 16:0.6                 (only a trip under a low threshold)
	stored := []StoredSample{
		{3, at(3), 0.85}, {4, at(4), 0.9}, {5, at(5), 0.82}, {7, at(7), 0.95},
		{15, at(15), 0.5}, {16, at(16), 0.6},
	}
	cases := []struct {
		name string
		rule WindowRule
		want []Episode
	}{
		{"0.8/6/4", rule846, []Episode{{At: at(7), PeakScore: 0.95}}},
		{"0.8/6/3", WindowRule{Threshold: 0.8, Size: 6, Hits: 3}, []Episode{{At: at(5), PeakScore: 0.95}}},
		{"0.45/3/2", WindowRule{Threshold: 0.45, Size: 3, Hits: 2}, []Episode{
			{At: at(4), PeakScore: 0.95}, // 3,4 hit; holds through 5 and 7 (6 is a gap inside the window)
			{At: at(16), PeakScore: 0.6}, // after clearing at 10, 15+16 trip again
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ReplaySession(c.rule, 20, stored)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("ReplaySession = %+v, want %+v", got, c.want)
			}
		})
	}
	// Samples numbered past `taken` (a batch racing a count) are ignored.
	if got := ReplaySession(rule846, 2, stored); len(got) != 0 {
		t.Fatalf("replay past taken found %+v", got)
	}
}
