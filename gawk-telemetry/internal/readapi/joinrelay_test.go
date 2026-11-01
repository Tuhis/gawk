package readapi

import (
	"fmt"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-telemetry/internal/rollup"
)

// JoinRelay built the permanent row's subscriber facts (Relay dropped, …,
// RelayPod, RelayRole) from the LAST subscriber observation carrying the
// session's id, and a striped viewer's legs carry it too (docs/35 §5.8). A
// leg that sorted after the primary — a later pod file, or simply the later
// line — overwrote the viewer's own figures with a leg's, on the one
// artifact that is never pruned. Edge sessions (internal) are judged by the
// same predicate, relayscrape.IsAudience, as every other audience loop.
func TestJoinRelayIgnoresStripeLegsAndEdges(t *testing.T) {
	const sid = "6a6a6a6a6a6a6a6a6a6a6a6a"
	sub := func(pod, role, extra string, dropped int) []byte {
		return []byte(fmt.Sprintf(`{"kind":"subscriber","atMs":1785715230000,"pod":%q,"role":%q,`+
			`"broadcastKey":%q,"sessionId":%q,"subscriber":{"sessionId":%q,"dropped":%d,`+
			`"keyframesDropped":%d,"carrierQueueOverflow":%d,"carrierRecordsDropped":%d,"dvrResyncs":%d%s}}`,
			pod, role, bkey, sid, sid, dropped, dropped, dropped, dropped, dropped, extra))
	}
	// The primary first, the leg (and an edge) after it: the order in which
	// "last observation wins" picks the wrong one.
	lines := [][]byte{
		sub("pod-a", "origin", "", 7),
		sub("pod-b", "edge", `,"stripeLeg":true`, 99),
		sub("pod-c", "edge", `,"internal":true`, 55),
	}
	newRow := func() rollup.Row {
		return rollup.Row{
			SessionID: sid, BroadcastKey: bkey, Role: "viewer",
			StartedAt: 1785715200000, EndedAt: 1785715260000, RelayCoverage: "none",
		}
	}

	row := newRow()
	JoinRelay(&row, lines, 5*time.Second)
	if row.RelayPod != "pod-a" || row.RelayRole != "origin" {
		t.Errorf("relayPod/relayRole = %q/%q, want the primary's pod-a/origin", row.RelayPod, row.RelayRole)
	}
	for _, k := range []string{"dropped", "keyframesDropped", "carrierQueueOverflow", "carrierRecordsDropped", "dvrResyncs"} {
		if row.Relay[k] != 7 {
			t.Errorf("relay[%s] = %v, want the primary's 7 (a leg or edge leaked in)", k, row.Relay[k])
		}
	}
	if row.RelayCoverage != "full" {
		t.Errorf("relayCoverage = %q, want full", row.RelayCoverage)
	}

	// Legs alone are not the viewer: a row whose only observations are legs
	// has no relay-side view of its own.
	legsOnly := newRow()
	JoinRelay(&legsOnly, lines[1:], 5*time.Second)
	if legsOnly.RelayCoverage != "none" || legsOnly.RelayPod != "" || len(legsOnly.Relay) != 0 {
		t.Errorf("legs-only join = coverage %q pod %q relay %v, want none/empty",
			legsOnly.RelayCoverage, legsOnly.RelayPod, legsOnly.Relay)
	}
}

// The same "last line carrying the sessionId wins" read lives in the session
// diagnose (factsFor) and in the dip movers (relayMovers). Both must skip
// legs and edges, or a repaired (or pre-#657) relay file still feeds a leg's
// drops into a verdict.
func TestSessionRelayReadersIgnoreStripeLegs(t *testing.T) {
	const sid = "6b6b6b6b6b6b6b6b6b6b6b6b"
	sub := func(atMs int64, extra string, dropped int) []byte {
		return []byte(fmt.Sprintf(`{"kind":"subscriber","atMs":%d,"pod":"pod-a","role":"origin",`+
			`"broadcastKey":%q,"sessionId":%q,"subscriber":{"sessionId":%q,"dropped":%d%s}}`,
			atMs, bkey, sid, sid, dropped, extra))
	}
	lines := [][]byte{
		sub(1785715210000, "", 2),
		sub(1785715210000, `,"stripeLeg":true`, 40),
		sub(1785715250000, "", 2),
		sub(1785715250000, `,"stripeLeg":true`, 90),
	}

	t.Run("factsFor", func(t *testing.T) {
		f := newFixture(t)
		in := rollup.Input{SessionID: sid, BroadcastKey: bkey, Role: "viewer",
			StartedAtMs: 1785715200000, EndedAtMs: 1785715260000}
		facts := f.api.factsFor(rollup.Compute(in), in, lines)
		if got, ok := facts.Relay("subscriberDropped"); !ok || got != 2 {
			t.Errorf("relay.subscriberDropped = %v (ok=%v), want the primary's 2", got, ok)
		}
	})

	t.Run("relayMovers", func(t *testing.T) {
		for _, m := range relayMovers(lines, sid, 1785715200000, 1785715260000) {
			if m.Signal == "subscriberDropped" {
				t.Errorf("subscriberDropped moved %v → %v; the primary never moved, a leg did", m.From, m.To)
			}
		}
	})
}
