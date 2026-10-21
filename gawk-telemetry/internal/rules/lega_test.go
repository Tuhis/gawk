package rules

import (
	"strings"
	"testing"
)

// BUGS.md "Telemetry blames leg B for broadcaster-uplink loss" (2026-09-27),
// fixed in docs/75 RU0. The incident's relay facts: 4 whole frames lost
// against ~29k relayed, while a fifth of all chunks never reached the relay.
func incidentRelayFacts(f *Facts) *Facts {
	f.SetRelay("ingressLossRatio", 4.0/29000)
	f.SetRelay("ingressChunkLossRatio", 0.21)
	return f
}

func TestHeavyChunkLossWithCleanWholeFramesIsLegA(t *testing.T) {
	rep := Evaluate(incidentRelayFacts(bcastFacts()), Playbook())
	got := findingByID(rep, "leg-a-broadcaster-uplink")
	if got == nil {
		t.Fatalf("leg-a-broadcaster-uplink did not fire on 21%% chunk loss; healthy=%v passed=%v",
			rep.Healthy, rep.Passed)
	}
	if got.Severity != SeverityBad {
		t.Errorf("severity = %s, want bad at 21%% chunk loss", got.Severity)
	}
	if rep.Healthy {
		t.Error("a broadcast losing a fifth of its chunks on leg A reported healthy")
	}
}

func TestDamagedFramesAloneFireLegA(t *testing.T) {
	// A relay that reports the frame unit (RU0): no whole frame lost, but a
	// third of frames arrived with a chunk missing.
	f := bcastFacts()
	f.SetRelay("ingressLossRatio", 0)
	f.SetRelay("ingressFrameLossRatio", 0.33)
	if findingByID(Evaluate(f, Playbook()), "leg-a-broadcaster-uplink") == nil {
		t.Fatal("leg-a-broadcaster-uplink did not fire on a 33% damaged-frame ratio")
	}
}

func TestBaselineChunkLossDoesNotFireLegA(t *testing.T) {
	// The incident's pre-onset baseline, 0.1–0.4 % of chunks, which parity
	// repairs: a frame ratio of a few percent before parity is not a fault.
	f := bcastFacts()
	f.SetRelay("ingressLossRatio", 0)
	f.SetRelay("ingressChunkLossRatio", 0.004)
	f.SetRelay("ingressFrameLossRatio", 0.03)
	if got := findingByID(Evaluate(f, Playbook()), "leg-a-broadcaster-uplink"); got != nil {
		t.Errorf("leg-a fired on baseline loss: %+v", got)
	}
}

func TestKeyframeOnlyDeliveryDoesNotBlameLegBOnLegALoss(t *testing.T) {
	f := incidentRelayFacts(viewerFacts())
	f.SetClient("fpsDipResyncs", 40)
	f.SetClient("fpsDipKeyframes", 40)
	f.SetRelay("keyframesDropped", 3)
	got := findingByID(Evaluate(f, Playbook()), "keyframe-only-delivery")
	if got == nil {
		t.Fatal("keyframe-only-delivery did not fire")
	}
	if strings.Contains(got.Action, "leg B") {
		t.Errorf("action blames leg B while the relay's ingress is losing a fifth of its chunks: %q", got.Action)
	}
	if !strings.Contains(got.Action, "leg A") {
		t.Errorf("action does not name leg A: %q", got.Action)
	}
}

func TestKeyframeOnlyDeliveryKeepsLegBOnCleanIngress(t *testing.T) {
	f := viewerFacts()
	f.SetRelay("ingressLossRatio", 0)
	f.SetRelay("ingressChunkLossRatio", 0)
	f.SetClient("fpsDipResyncs", 40)
	f.SetClient("fpsDipKeyframes", 40)
	got := findingByID(Evaluate(f, Playbook()), "keyframe-only-delivery")
	if got == nil || !strings.Contains(got.Action, "leg B") {
		t.Fatalf("clean ingress should keep the leg-B action, got %+v", got)
	}
}

func TestLegBSingleViewerNeedsChunkCleanIngress(t *testing.T) {
	f := incidentRelayFacts(viewerFacts())
	f.SetRelay("subscriberDropped", 900)
	f.SetRelay("peerMedianDropped", 10)
	if got := findingByID(Evaluate(f, Playbook()), "leg-b-single-viewer"); got != nil {
		t.Errorf("leg-b-single-viewer fired through 21%% leg-A chunk loss: %+v", got)
	}
}

// docs/75 F7/D8: the relay's ingress window cannot tell a chunk the network
// lost from one quic-go dropped before the relay read it. The broadcaster's
// QUIC packet counters can: chunk loss at the relay with none on the uplink
// is the relay's own intake.
func f7Facts(uplinkLoss float64) *Facts {
	f := bcastFacts()
	f.SetRelay("ingressLossRatio", 0)
	f.SetRelay("ingressChunkLossRatio", 0.011)
	f.SetRelay("ingressFrameLossRatio", 0.43)
	f.SetClient("uplinkPacketLossRatio", uplinkLoss)
	return f
}

func TestRelayIntakeDropsNamesTheRelayNotTheUplink(t *testing.T) {
	rep := Evaluate(f7Facts(0), Playbook())
	got := findingByID(rep, "relay-intake-drops")
	if got == nil {
		t.Fatalf("relay-intake-drops did not fire; findings=%+v unavailable=%+v", rep.Findings, rep.Unavailable)
	}
	if leg := findingByID(rep, "leg-a-broadcaster-uplink"); leg != nil {
		t.Errorf("leg-a blamed the uplink although the broadcaster's QUIC lost nothing: %+v", leg)
	}
}

func TestUplinkPacketLossKeepsTheBlameOnTheUplink(t *testing.T) {
	rep := Evaluate(f7Facts(0.012), Playbook())
	if got := findingByID(rep, "relay-intake-drops"); got != nil {
		t.Errorf("relay-intake-drops fired although the uplink lost packets: %+v", got)
	}
	if findingByID(rep, "leg-a-broadcaster-uplink") == nil {
		t.Error("leg-a-broadcaster-uplink did not fire on uplink packet loss")
	}
}
