package rollup

import "testing"

// A hidden tab is not a fair witness to its own rendering: the browser stops
// firing rAF, so `renderedFps` falls to 0 while decode carries on perfectly.
// Folding those samples into the permanent rollup row (D4) records a median
// that describes tab state rather than rendering — and unlike the raw samples,
// that row is never pruned.
//
// So presentation series exclude hidden samples. Everything the WORKER
// measured is untouched: decode did not stop, and pretending it did would be
// the same error in the other direction.
func TestViewerPresentationSeriesIgnoreHiddenSamples(t *testing.T) {
	samples := []Sample{
		{TMs: 0, Stats: map[string]any{
			"receivedFps": 30.0, "decoderFps": 30.0, "renderedFps": 30.0,
			"documentHidden": false, "documentHiddenMs": 0.0,
		}},
		{TMs: 2000, Stats: map[string]any{
			"receivedFps": 30.0, "decoderFps": 30.0, "renderedFps": 30.0,
			"documentHidden": false, "documentHiddenMs": 0.0,
		}},
		// Backgrounded: rendering stops, delivery does not.
		{TMs: 4000, Stats: map[string]any{
			"receivedFps": 30.0, "decoderFps": 30.0, "renderedFps": 0.0,
			"documentHidden": true, "documentHiddenMs": 2000.0,
		}},
		{TMs: 6000, Stats: map[string]any{
			"receivedFps": 30.0, "decoderFps": 30.0, "renderedFps": 0.0,
			"documentHidden": true, "documentHiddenMs": 4000.0,
		}},
	}

	r := Compute(Input{SessionID: "s", Role: "viewer", Samples: samples})

	rendered := r.Series["renderedFps"]
	if rendered == nil {
		t.Fatal("no renderedFps series")
	}
	if rendered.Median != 30 {
		t.Errorf("renderedFps p50 = %v, want 30 — the hidden samples must not drag it down", rendered.Median)
	}
	if rendered.Min != 30 {
		t.Errorf("renderedFps min = %v, want 30; a hidden window is not a rendering low", rendered.Min)
	}
	// Delivery kept running and its series must be untouched by any of this.
	if got := r.Series["receivedFps"]; got == nil || got.Median != 30 {
		t.Errorf("receivedFps was altered by the visibility filter: %+v", got)
	}
	if got := r.Series["decoderFps"]; got == nil || got.Median != 30 {
		t.Errorf("decoderFps was altered by the visibility filter: %+v", got)
	}
}

// A session spent entirely in the background has NO presentation evidence. The
// row must omit the series rather than record a confident zero — "not measured"
// and "measured, and it was zero" are different claims, and only the second is
// evidence.
func TestAFullyHiddenViewerHasNoPresentationSeries(t *testing.T) {
	samples := []Sample{
		{TMs: 0, Stats: map[string]any{"receivedFps": 30.0, "renderedFps": 0.0, "documentHidden": true}},
		{TMs: 2000, Stats: map[string]any{"receivedFps": 30.0, "renderedFps": 0.0, "documentHidden": true}},
	}
	r := Compute(Input{SessionID: "s", Role: "viewer", Samples: samples})
	if got := r.Series["renderedFps"]; got != nil {
		t.Errorf("renderedFps = %+v, want absent for a session that was never visible", got)
	}
	if got := r.Series["receivedFps"]; got == nil {
		t.Error("receivedFps went missing; only PRESENTATION series depend on visibility")
	}
}

// A BROADCASTER is the opposite case, and getting this backwards would hide a
// real fault. A hidden broadcaster tab is throttled by the browser, so it
// genuinely sends fewer frames — every viewer sees that degradation. It must be
// EXPLAINED by the row, never filtered out of it.
func TestBroadcasterRatesAreNeverFilteredByVisibility(t *testing.T) {
	samples := []Sample{
		{TMs: 0, Stats: map[string]any{"captureFps": 30.0, "sentFps": 30.0, "documentHidden": false}},
		{TMs: 2000, Stats: map[string]any{"captureFps": 2.0, "sentFps": 2.0, "documentHidden": true}},
		{TMs: 4000, Stats: map[string]any{"captureFps": 2.0, "sentFps": 2.0, "documentHidden": true}},
	}
	r := Compute(Input{SessionID: "s", Role: "broadcaster", Samples: samples})
	sent := r.Series["sentFps"]
	if sent == nil {
		t.Fatal("no sentFps series")
	}
	if sent.Min != 2 {
		t.Errorf("sentFps min = %v, want 2 — a throttled broadcaster's collapse is real and must be recorded", sent.Min)
	}
}

// The fact itself rides the row for both roles, so "why was this session's
// rendering flat?" is answerable from the permanent artifact alone, years after
// the raw samples were pruned.
func TestHiddenTimeIsRecordedOnTheRow(t *testing.T) {
	for _, role := range []string{"viewer", "broadcaster"} {
		t.Run(role, func(t *testing.T) {
			samples := []Sample{
				{TMs: 0, Stats: map[string]any{"receivedFps": 30.0, "sentFps": 30.0, "documentHiddenMs": 0.0}},
				{TMs: 2000, Stats: map[string]any{"receivedFps": 30.0, "sentFps": 30.0, "documentHiddenMs": 7000.0}},
			}
			r := Compute(Input{SessionID: "s", Role: role, Samples: samples})
			if got, ok := r.Counters["documentHiddenMs"]; !ok || got != 7000 {
				t.Errorf("documentHiddenMs = %v (present=%v), want 7000", got, ok)
			}
		})
	}
}
