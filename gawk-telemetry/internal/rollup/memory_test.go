package rollup

import (
	"fmt"
	"testing"
)

const mib = 1 << 20

// memEvents is a native session's minute `memory` events (docs/77 D9):
// footprint(t) for each minute in [0, minutes].
func memEvents(minutes int, footprint func(min int) float64) []Event {
	var out []Event
	for m := 0; m <= minutes; m++ {
		fp := footprint(m)
		out = append(out, Event{
			TMs:    float64(m * 60_000),
			Kind:   "memory",
			Detail: fmt.Sprintf("footprint=%.0f,peak=%.0f,heap=1000,gpu=2000", fp, fp+5*mib),
		})
	}
	return out
}

func memInput(events []Event, samples []Sample) Input {
	return Input{SessionID: "s", Role: "broadcaster", Events: events, Samples: samples}
}

// The report that started R76: about 250 MB for hours, then about 750 MB
// after a mid-stream change, with the samples cut off early by the byte
// budget (90 minutes, as at the old 4 MiB), so only the minute events can
// see it.
func TestMemoryGrowthIsReadFromTheMinuteEventsPastTheSampleBudget(t *testing.T) {
	events := memEvents(240, func(m int) float64 {
		if m < 180 {
			return 250 * mib
		}
		return 750 * mib
	})
	var samples []Sample
	for s := 0; s < 90*30; s++ { // 90 minutes at 2 s
		samples = append(samples, Sample{TMs: float64(s * 2000), Stats: map[string]any{"memFootprintBytes": 250.0 * mib}})
	}
	r := Compute(memInput(events, samples))
	m := r.Memory
	if m == nil {
		t.Fatal("no memory summary")
	}
	if m.Source != "events" || m.Points != 241 {
		t.Fatalf("source %q, %d points", m.Source, m.Points)
	}
	if *m.EarlyBytes != 250*mib || *m.LateBytes != 750*mib || *m.GrowthBytes != 500*mib {
		t.Fatalf("early %v late %v growth %v", *m.EarlyBytes, *m.LateBytes, *m.GrowthBytes)
	}
	if m.PeakBytes != 755*mib {
		t.Fatalf("peak %v: the reported peak, not only the footprint", m.PeakBytes)
	}
	if m.MedianBytes != 250*mib {
		t.Fatalf("median %v", m.MedianBytes)
	}
	if m.SpanMs != 240*60_000 {
		t.Fatalf("span %v", m.SpanMs)
	}
}

// Warm-up is not growth: the pipeline allocating in its first minutes is
// what starting looks like.
func TestMemoryWarmUpIsNotGrowth(t *testing.T) {
	r := Compute(memInput(memEvents(60, func(m int) float64 {
		if m < 2 {
			return 50 * mib
		}
		return 120 * mib
	}), nil))
	if g := *r.Memory.GrowthBytes; g != 0 {
		t.Fatalf("growth %v", g)
	}
}

// Too short to tell growth from noise: no growth figure at all, rather than
// a confident zero.
func TestMemoryGrowthNeedsTwentyMinutes(t *testing.T) {
	r := Compute(memInput(memEvents(19, func(int) float64 { return 100 * mib }), nil))
	if r.Memory == nil || r.Memory.GrowthBytes != nil || r.Memory.EarlyBytes != nil {
		t.Fatalf("%+v", r.Memory)
	}
	if r.Memory.MedianBytes != 100*mib {
		t.Fatalf("the median stands without the growth: %+v", r.Memory)
	}
}

// A client that predates the events still gets a summary from its samples.
func TestMemoryFallsBackToSamples(t *testing.T) {
	var samples []Sample
	for s := 0; s <= 30*30; s++ {
		fp := 80.0 * mib
		if s > 25*30 {
			fp = 200 * mib
		}
		samples = append(samples, Sample{TMs: float64(s * 2000), Stats: map[string]any{
			"memFootprintBytes": fp, "memFootprintPeakBytes": 210.0 * mib,
		}})
	}
	m := Compute(memInput(nil, samples)).Memory
	if m == nil || m.Source != "samples" {
		t.Fatalf("%+v", m)
	}
	if *m.GrowthBytes != 120*mib || m.PeakBytes != 210*mib {
		t.Fatalf("growth %v peak %v", *m.GrowthBytes, m.PeakBytes)
	}
}

// Malformed details and other roles produce nothing, never a zero.
func TestMemoryIgnoresWhatItCannotRead(t *testing.T) {
	bad := []Event{
		{TMs: 0, Kind: "memory", Detail: "footprint=abc"},
		{TMs: 1, Kind: "memory", Detail: "peak=5"},
		{TMs: 2, Kind: "memory", Detail: "footprint=-1"},
	}
	if m := Compute(memInput(bad, nil)).Memory; m != nil {
		t.Fatalf("%+v", m)
	}
	viewer := memInput(memEvents(30, func(int) float64 { return mib }), nil)
	viewer.Role = "viewer"
	if m := Compute(viewer).Memory; m != nil {
		t.Fatalf("viewer: %+v", m)
	}
}
