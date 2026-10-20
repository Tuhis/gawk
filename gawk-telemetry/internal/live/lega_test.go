package live

import (
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-telemetry/internal/relayscrape"
)

// incidentObs is the BUGS.md 2026-09-27 broadcast at one scrape: a long
// clean history, then gen windows of a fifth of all chunks lost with
// almost no whole frame lost. Cumulative, as the relay reports them.
func incidentObs(gen uint64) []relayscrape.Observation {
	const history = 10_000_000 // frames relayed before the onset
	bc := relayscrape.Broadcast{
		PublisherActive: true, Role: "origin",
		FramesRelayed:         history + 300*gen,
		IngressFramesExpected: history + 300*gen,
		IngressChunksExpected: 10*history + 3000*gen,
		IngressFramesLost:     gen / 2,
		IngressFramesDamaged:  290 * gen,
		IngressChunksLost:     630 * gen,
	}
	return []relayscrape.Observation{{
		Kind: "broadcast", Pod: "pod-a", Role: "origin", BroadcastKey: "1a2b3c4d5e6f", Broadcast: &bc,
	}}
}

func findingIDs(bv BroadcastView) map[string]bool {
	out := map[string]bool{}
	for _, f := range bv.Findings {
		out[f.ID] = true
	}
	return out
}

func TestLiveBroadcastSeesLegAChunkLoss(t *testing.T) {
	p, c := newProj()
	p.ObserveRelay(round(incidentObs(1)))
	c.add(5 * time.Second)
	p.ObserveRelay(round(incidentObs(2)))
	bv := p.Snapshot().Live[0]
	if got := bv.Metrics["ingressChunkLossRatio"]; got != 0.21 {
		t.Errorf("ingressChunkLossRatio = %v, want this window's 0.21", got)
	}
	if !findingIDs(bv)["leg-a-broadcaster-uplink"] {
		t.Errorf("leg-a did not fire on a fifth of chunks lost; findings %+v", bv.Findings)
	}
}

func TestLiveBroadcastNamesTheRelayIntakeWhenTheUplinkLostNothing(t *testing.T) {
	p, c := newProj()
	cast := func(sent float64) map[string]any {
		return map[string]any{"uplinkPacketsAvailable": true, "uplinkPacketsSent": sent, "uplinkPacketsLost": 0.0}
	}
	p.ObserveClient(batch("f0f0f0f0f0f0f0f0f0f0f0f0", "broadcaster", cast(1000)), "", "Windows", "2.4.0")
	p.ObserveRelay(round(incidentObs(1)))
	c.add(5 * time.Second)
	p.ObserveClient(batch("f0f0f0f0f0f0f0f0f0f0f0f0", "broadcaster", cast(4000)), "", "Windows", "2.4.0")
	p.ObserveRelay(round(incidentObs(2)))
	ids := findingIDs(p.Snapshot().Live[0])
	if !ids["relay-intake-drops"] {
		t.Errorf("relay-intake-drops did not fire with a loss-free uplink; findings %v", ids)
	}
	if ids["leg-a-broadcaster-uplink"] {
		t.Error("leg-a blamed an uplink whose QUIC lost nothing")
	}
}
