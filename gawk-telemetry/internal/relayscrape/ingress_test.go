package relayscrape

import (
	"encoding/json"
	"testing"
)

// docs/75 RU0: the relay's frame-unit counters decode, and the two ratios
// are computed over a window, one unit each.
func TestIngressRatiosOverAWindow(t *testing.T) {
	var prev, cur Broadcast
	if err := json.Unmarshal([]byte(`{"ingressFramesLost":10,"ingressFramesDamaged":20,
		"ingressFramesExpected":100000,"ingressChunksLost":30,"ingressChunksExpected":900000}`), &prev); err != nil {
		t.Fatal(err)
	}
	// A late episode: 1 lost + 99 damaged of 1000 frames, 210 of 10000 chunks.
	cur = prev
	cur.IngressFramesLost += 1
	cur.IngressFramesDamaged += 99
	cur.IngressFramesExpected += 1000
	cur.IngressChunksLost += 210
	cur.IngressChunksExpected += 10000

	frame, frameOK, chunk, chunkOK := cur.IngressLoss().RatiosSince(prev.IngressLoss())
	if !frameOK || frame != 0.1 {
		t.Errorf("frame ratio = %v (ok=%v), want 0.1 — the window's, not the lifetime's", frame, frameOK)
	}
	if !chunkOK || chunk != 0.021 {
		t.Errorf("chunk ratio = %v (ok=%v), want 0.021", chunk, chunkOK)
	}

	// An idle window is measured, and clean.
	frame, frameOK, chunk, chunkOK = cur.IngressLoss().RatiosSince(cur.IngressLoss())
	if !frameOK || !chunkOK || frame != 0 || chunk != 0 {
		t.Errorf("idle window = %v/%v (ok %v/%v), want 0/0 measured", frame, chunk, frameOK, chunkOK)
	}
	// A relay that predates the unit reports nothing, not zero.
	old := Broadcast{IngressFramesLost: 4, IngressChunksLost: 6000}
	if _, frameOK, _, chunkOK = old.IngressLoss().RatiosSince(IngressLoss{}); frameOK || chunkOK {
		t.Error("a relay without expected counters produced a ratio")
	}
	// Counters that went backwards (relay restart) give no window.
	if _, frameOK, _, chunkOK = prev.IngressLoss().RatiosSince(cur.IngressLoss()); frameOK || chunkOK {
		t.Error("a backwards window produced a ratio")
	}
}
