package readapi

import (
	"encoding/json"
	"testing"

	"github.com/Tuhis/gawk/gawk-telemetry/internal/rules"
)

// BUGS.md 2026-09-27: the stored broadcast verdict divided LIFETIME counters,
// so an episode starting late in a long broadcast was diluted to nothing —
// and it read whole frames only. The incident, replayed: hours of clean
// history, then a fifth of all chunks lost with almost no whole frame lost.
func TestStoredBroadcastVerdictSeesALateLegAEpisode(t *testing.T) {
	line := func(atMs int64, gen uint64) []byte {
		const history = 10_000_000
		b, err := json.Marshal(map[string]any{
			"kind": "broadcast", "atMs": atMs, "pod": "pod-a", "role": "origin", "broadcastKey": bkey,
			"broadcast": map[string]any{
				"publisherActive": true,
				"framesRelayed":   history + 300*gen, "ingressFramesLost": gen / 2,
				"ingressFramesExpected": history + 300*gen, "ingressFramesDamaged": 290 * gen,
				"ingressChunksExpected": 10*history + 3000*gen, "ingressChunksLost": 630 * gen,
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	lines := [][]byte{
		line(1_000_000, 0),         // the onset, within the window
		line(1_030_000, 10),        // 30 s of the episode
		line(1_000_000-600_000, 0), // ten minutes earlier: outside the window
	}
	f := rules.NewFacts(bkey, "broadcast", "")
	setBroadcastRelayFacts(f, lines, bkey)
	if got, _ := f.Relay("ingressChunkLossRatio"); got != 0.21 {
		t.Errorf("ingressChunkLossRatio = %v, want the window's 0.21", got)
	}
	rep := rules.Evaluate(f, rules.Playbook())
	if rep.Healthy {
		t.Fatalf("a broadcast losing a fifth of its chunks on leg A reported healthy; passed %v", rep.Passed)
	}
	fired := false
	for _, fd := range rep.Findings {
		fired = fired || fd.ID == "leg-a-broadcaster-uplink"
	}
	if !fired {
		t.Errorf("leg-a-broadcaster-uplink did not fire: %+v", rep.Findings)
	}
}

// A broadcaster session's stored facts carry its QUIC packet loss over the
// session (docs/75 D8), so the broadcast verdict can split uplink from intake.
func TestStoredBroadcasterCarriesUplinkPacketLoss(t *testing.T) {
	f := rules.NewFacts(bkey, "broadcast", "")
	setUplinkLoss(f, map[string]float64{"uplinkPacketsSent": 20000, "uplinkPacketsLost": 0})
	if v, ok := f.Client("uplinkPacketLossRatio"); !ok || v != 0 {
		t.Errorf("uplinkPacketLossRatio = %v (ok=%v), want a measured 0", v, ok)
	}
	f = rules.NewFacts(bkey, "broadcast", "")
	setUplinkLoss(f, map[string]float64{"uplinkPacketsSent": 0})
	if _, ok := f.Client("uplinkPacketLossRatio"); ok {
		t.Error("a broadcaster that sent nothing (or a browser one) produced an uplink ratio")
	}
}
