package live

import (
	"testing"

	"github.com/Tuhis/gawk/gawk-telemetry/internal/rollup"
)

// #376: an old viewer tab still open after the client fix keeps publishing
// the broadcaster tab's age as capToRenderMs/liveEdgeDriftMs. The live row
// shows that value in its cap→render column, so a 48-minute "latency" would
// read as the worst viewer on the fleet. Past rollup.MaxPlausibleLatencyMs
// the reading is absent, and a bogus sample after a good one clears the good
// one rather than leaving a stale value standing.
func TestLiveRowDropsImplausibleLatency(t *testing.T) {
	p, _ := newProj()
	const id = "9a9a9a9a9a9a9a9a9a9a9a9a"
	good := healthyViewer()
	good["capToRenderMs"] = 48.0
	good["liveEdgeDriftMs"] = 12.0
	p.ObserveClient(batch(id, "viewer", good), "Chrome 152", "Windows", "0.33.2")
	if v := findSession(p.Snapshot(), id); v == nil || v.Metrics["capToRenderMs"] != 48 {
		t.Fatalf("plausible capToRenderMs did not reach the live row: %+v", v)
	}

	bogus := healthyViewer()
	bogus["capToRenderMs"] = 2_880_000.0
	bogus["liveEdgeDriftMs"] = float64(rollup.MaxPlausibleLatencyMs + 1)
	p.ObserveClient(batch(id, "viewer", bogus), "Chrome 152", "Windows", "0.33.2")
	v := findSession(p.Snapshot(), id)
	if v == nil {
		t.Fatal("session vanished")
	}
	for _, k := range []string{"capToRenderMs", "liveEdgeDriftMs"} {
		if got, ok := v.Metrics[k]; ok {
			t.Errorf("live %s = %v, want absent (implausible, and the earlier reading is stale)", k, got)
		}
	}
	if v.Metrics["receivedFps"] != 60 {
		t.Errorf("receivedFps = %v; the guard must touch only the latency fields", v.Metrics["receivedFps"])
	}
}
