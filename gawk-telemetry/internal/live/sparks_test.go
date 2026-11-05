package live

import (
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-telemetry/internal/ingest"
)

const sparkKey = "1a2b3c4d5e6f"

// feedSparks delivers one session's samples the way a client does: a batch
// per `per` samples, each arriving `delay` after its newest sample, from a
// client whose wall clock is `skew` off this service's. gapsMs are the
// spacings between consecutive samples (cycled); stats builds each sample.
// Returns the server time the session started.
func feedSparks(p *Projection, c *clock, sid, role string, skew time.Duration, n, per int,
	gapsMs []int64, skip func(i int) bool, stats func(i int) map[string]any) time.Time {
	start := c.t
	startedAtMs := start.Add(skew).UnixMilli()
	var tMs int64
	var batch []ingest.Sample
	for i := range n {
		if i > 0 {
			tMs += gapsMs[(i-1)%len(gapsMs)]
		}
		if skip == nil || !skip(i) {
			batch = append(batch, ingest.Sample{TMs: float64(tMs), Stats: stats(i)})
		}
		if (i+1)%per == 0 || i == n-1 {
			c.t = start.Add(time.Duration(tMs)*time.Millisecond + 500*time.Millisecond)
			if len(batch) > 0 {
				p.ObserveClient(ingest.Accepted{
					SessionID: sid, BroadcastKey: sparkKey, Role: role,
					StartedAtMs: startedAtMs, Samples: batch,
				}, "", "", "")
			}
			batch = nil
		}
	}
	return start
}

func fps(field string, v float64) map[string]any { return map[string]any{field: v} }

// A minute of a viewer at 60 fps with one 12 fps sample, from a client whose
// clock is an hour fast: the tail lands on THIS service's clock, keeps the
// dip, and is null — not zero, not interpolated — where nothing was sampled.
func TestASparkTailIsPlacedOnTheServiceClock(t *testing.T) {
	p, c := newProj()
	const sid = "9a9a9a9a9a9a9a9a9a9a9a9a"
	start := feedSparks(p, c, sid, "viewer", time.Hour, 30, 5, []int64{2000}, nil, func(i int) map[string]any {
		if i == 10 {
			return fps("decoderFps", 12)
		}
		return map[string]any{"decoderFps": 60.0, "receivedFps": 61.0}
	})
	c.t = start.Add(time.Minute)

	sp, ok := p.Sparks(sparkKey)
	if !ok {
		t.Fatal("no tails for a live broadcast")
	}
	if sp.AtMs != c.t.UnixMilli() || sp.WindowMs != 120000 || sp.StepMs != 4000 {
		t.Fatalf("grid = at %d step %d window %d, want now, 4000 (twice the 2 s gap), 120000", sp.AtMs, sp.StepMs, sp.WindowMs)
	}
	s, ok := sp.Sessions[sid]
	if !ok || s.Field != "decoderFps" || len(s.Values) != 30 {
		t.Fatalf("tail = %+v, want 30 decoderFps buckets", s)
	}
	// Samples sit at start+tMs+0.5 s on the service's clock: buckets 15–29.
	for j, v := range s.Values {
		switch {
		case j < 15 && v != nil:
			t.Errorf("bucket %d = %v before the session existed, want null", j, *v)
		case j >= 15 && v == nil:
			t.Errorf("bucket %d is null inside the sampled minute", j)
		case j == 20 && *v != 12:
			t.Errorf("bucket 20 = %v, want the dip's 12 (the lowest sample, never an average)", *v)
		case j >= 15 && j != 20 && *v != 60:
			t.Errorf("bucket %d = %v, want 60", j, *v)
		}
	}
	if want := start.Add(58500 * time.Millisecond).UnixMilli(); s.ThroughMs != want {
		t.Errorf("throughMs = %d, want the newest sample's %d", s.ThroughMs, want)
	}
}

// The grid is anchored to multiples of the step, not to "now": two polls a
// moment apart, with no new data between them, return the same buckets. A
// window ending at the poll's instant slid every bucket edge on each 2 s
// poll, so samples near an edge changed buckets and the line wobbled with
// nothing new to show (R36 round-1 review).
func TestTheSparkGridDoesNotSlideBetweenPolls(t *testing.T) {
	p, c := newProj()
	const sid = "9e9e9e9e9e9e9e9e9e9e9e9e"
	start := feedSparks(p, c, sid, "viewer", 0, 50, 5, []int64{2000, 2500}, nil, func(i int) map[string]any {
		// Every sample different, so a sample changing bucket changes a min.
		return fps("decoderFps", float64(60-i%17))
	})
	c.t = start.Add(110*time.Second + 300*time.Millisecond)
	first, _ := p.Sparks(sparkKey)
	c.t = c.t.Add(1300 * time.Millisecond)
	second, _ := p.Sparks(sparkKey)

	if first.AtMs%first.StepMs != 0 || second.AtMs%second.StepMs != 0 {
		t.Fatalf("window ends at %d and %d, want multiples of the %d ms step", first.AtMs, second.AtMs, first.StepMs)
	}
	a, b := first.Sessions[sid].Values, second.Sessions[sid].Values
	if len(a) != len(b) {
		t.Fatalf("bucket counts %d and %d", len(a), len(b))
	}
	for j := range a {
		if (a[j] == nil) != (b[j] == nil) || (a[j] != nil && *a[j] != *b[j]) {
			t.Fatalf("bucket %d moved between polls with no new data: %v then %v", j, deref(a[j]), deref(b[j]))
		}
	}
}

func deref(v *float64) any {
	if v == nil {
		return nil
	}
	return *v
}

// A batch that never arrived is a break, never a line drawn across it (UD9).
func TestALostBatchIsABreakInTheTail(t *testing.T) {
	p, c := newProj()
	const sid = "9b9b9b9b9b9b9b9b9b9b9b9b"
	start := feedSparks(p, c, sid, "viewer", 0, 30, 5, []int64{2000},
		func(i int) bool { return i >= 15 && i < 20 }, // the batch tMs 30–38 s
		func(int) map[string]any { return fps("decoderFps", 30) })
	c.t = start.Add(time.Minute)
	s := mustSpark(t, p, sid)
	for j, v := range s.Values {
		gap := j == 23 || j == 24 // [32 s, 40 s) holds only lost samples
		if gap && v != nil {
			t.Errorf("bucket %d = %v inside the lost batch, want null", j, *v)
		}
		if !gap && j >= 15 && v == nil {
			t.Errorf("bucket %d is null outside the lost batch", j)
		}
	}
}

// A web client's samples come 2.0 or 2.5 s apart (the first 500 ms stats
// tick at least 2 s after the last). A 2 s grid would leave empty buckets
// with nothing missing; the tail's step is wide enough that it never does,
// whichever of the two gaps is the median, and widens with a slower cadence
// the relay asked for.
func TestTheSparkStepFollowsTheCadence(t *testing.T) {
	for _, tc := range []struct {
		gaps []int64
		step int64
	}{
		{[]int64{2000, 2500}, 4000},
		{[]int64{2500, 2500, 2000}, 4000},
		{[]int64{2000}, 4000},
		{[]int64{5000}, 8000},
		{[]int64{500}, 4000},
	} {
		p, c := newProj()
		const sid = "9c9c9c9c9c9c9c9c9c9c9c9c"
		// More than the window, so no bucket predates the session.
		n := int(130000 / min(tc.gaps[0], tc.gaps[len(tc.gaps)-1]))
		feedSparks(p, c, sid, "viewer", 0, n, 5, tc.gaps, nil,
			func(int) map[string]any { return fps("decoderFps", 30) })
		sp, _ := p.Sparks(sparkKey)
		if sp.StepMs != tc.step {
			t.Errorf("gaps %v: step = %d, want %d", tc.gaps, sp.StepMs, tc.step)
		}
		s := sp.Sessions[sid]
		through := (s.ThroughMs - (sp.AtMs - sp.WindowMs)) / sp.StepMs
		for j, v := range s.Values {
			// From the first full bucket to the newest sample's, every
			// bucket holds one.
			if j > 0 && int64(j) <= through && v == nil {
				t.Errorf("gaps %v: bucket %d of %d is null with no sample missing", tc.gaps, j, len(s.Values))
			}
		}
	}
}

// The field is the live table's second fps figure: decoderFps, else
// receivedFps for a viewer that does not report it; a broadcaster's
// encoderFps. A session with none of them has no tail rather than nulls.
func TestTheSparkFieldFollowsTheRole(t *testing.T) {
	p, c := newProj()
	feedSparks(p, c, "9d9d9d9d9d9d9d9d9d9d9d9d", "viewer", 0, 10, 5, []int64{2000}, nil,
		func(int) map[string]any { return fps("receivedFps", 24) })
	feedSparks(p, c, "9e9e9e9e9e9e9e9e9e9e9e9e", "broadcaster", 0, 10, 5, []int64{2000}, nil,
		func(int) map[string]any { return map[string]any{"encoderFps": 50.04, "sentFps": 49.0} })
	feedSparks(p, c, "9f9f9f9f9f9f9f9f9f9f9f9f", "viewer", 0, 10, 5, []int64{2000}, nil,
		func(int) map[string]any { return fps("renderedFps", 60) })
	sp, _ := p.Sparks(sparkKey)
	if s := sp.Sessions["9d9d9d9d9d9d9d9d9d9d9d9d"]; s.Field != "receivedFps" {
		t.Errorf("viewer without decoderFps: field %q, want receivedFps", s.Field)
	}
	b := sp.Sessions["9e9e9e9e9e9e9e9e9e9e9e9e"]
	if b.Field != "encoderFps" {
		t.Errorf("broadcaster: field %q, want encoderFps", b.Field)
	}
	for _, v := range b.Values {
		if v != nil && *v != 50 {
			t.Errorf("broadcaster value %v, want 50.04 rounded to 50", *v)
		}
	}
	if _, ok := sp.Sessions["9f9f9f9f9f9f9f9f9f9f9f9f"]; ok {
		t.Error("a session reporting none of the fields got a tail")
	}
}

// Ten minutes at the fastest cadence the relay may ask for: the ring holds
// the window and no more.
func TestTheSparkRingIsBounded(t *testing.T) {
	p, c := newProj()
	const sid = "a0a0a0a0a0a0a0a0a0a0a0a0"
	feedSparks(p, c, sid, "viewer", 0, 1200, 20, []int64{500}, nil,
		func(int) map[string]any { return fps("decoderFps", 30) })
	p.mu.Lock()
	n := len(p.bcasts[sparkKey].sessions[sid].spark.pts)
	p.mu.Unlock()
	if n > sparkRingCap {
		t.Errorf("ring holds %d points, want at most %d", n, sparkRingCap)
	}
	if _, ok := p.Sparks("ffffffffffff"); ok {
		t.Error("tails for a broadcast the projection does not hold")
	}
}

func mustSpark(t *testing.T, p *Projection, sid string) SparkSeries {
	t.Helper()
	sp, ok := p.Sparks(sparkKey)
	if !ok {
		t.Fatal("no tails")
	}
	s, ok := sp.Sessions[sid]
	if !ok {
		t.Fatalf("no tail for %s", sid)
	}
	return s
}
