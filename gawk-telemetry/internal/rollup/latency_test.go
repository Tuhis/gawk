package rollup

import "testing"

// #376: viewers before the client fix published capToRenderMs and
// liveEdgeDriftMs equal to the broadcaster tab's AGE — seconds to 48
// minutes — and those samples landed in the permanent row's Stat as if they
// were latencies. A value past MaxPlausibleLatencyMs is a clock fault, not a
// measurement, so it is left out of the derived Stat (the raw line keeps it:
// tolerant reader, D15).
func TestImplausibleLatencySamplesAreExcluded(t *testing.T) {
	samples := []Sample{
		{TMs: 0, Stats: map[string]any{"capToRenderMs": 48.0, "liveEdgeDriftMs": 10.0}},
		{TMs: 2000, Stats: map[string]any{"capToRenderMs": 2_880_000.0, "liveEdgeDriftMs": 2_879_000.0}},
		{TMs: 4000, Stats: map[string]any{"capToRenderMs": 52.0, "liveEdgeDriftMs": 12.0}},
		// Exactly at the bound is still a (terrible) measurement.
		{TMs: 6000, Stats: map[string]any{"capToRenderMs": float64(MaxPlausibleLatencyMs), "liveEdgeDriftMs": 14.0}},
	}
	row := Compute(Input{SessionID: "s", Role: "viewer", Samples: samples})

	cap := row.Series["capToRenderMs"]
	if cap == nil || cap.N != 3 || cap.Max != MaxPlausibleLatencyMs || cap.Min != 48 {
		t.Errorf("capToRenderMs = %+v, want n=3 over the plausible samples, max at the bound", cap)
	}
	drift := row.Series["liveEdgeDriftMs"]
	if drift == nil || drift.N != 3 || drift.Max != 14 {
		t.Errorf("liveEdgeDriftMs = %+v, want n=3, max 14", drift)
	}
}

// A session whose every latency sample was bogus has no latency Stat at all:
// absent means "could not be computed" (D15), never a Stat of clock faults.
func TestAllImplausibleLatencyLeavesSeriesAbsent(t *testing.T) {
	samples := []Sample{
		{TMs: 0, Stats: map[string]any{"capToRenderMs": 90_000.0, "receivedFps": 30.0}},
		{TMs: 2000, Stats: map[string]any{"capToRenderMs": 92_000.0, "receivedFps": 30.0}},
	}
	row := Compute(Input{SessionID: "s", Role: "viewer", Samples: samples})
	if st, ok := row.Series["capToRenderMs"]; ok {
		t.Errorf("capToRenderMs = %+v, want absent", st)
	}
	if row.Series["receivedFps"] == nil {
		t.Error("receivedFps went missing; the guard must touch only the latency series")
	}
}
