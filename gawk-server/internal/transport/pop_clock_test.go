package transport

import (
	"testing"

	"github.com/Tuhis/gawk/gawk-server/wire"
)

// TestRewriteClockMappingBackComposition pins the uplink leg's clock rule
// (docs/76 item 26): the PoP's ClockMapping says popUs = tsUs + X, the
// leg's estimator homeUs ≈ popUs + off, so home is sent X + off — the
// mirror of the edge's forward rewrite (X − off).
func TestRewriteClockMappingBackComposition(t *testing.T) {
	est := &timeSyncEstimator{}
	mapping := wire.AppendClockMapping(nil, 7_000_000) // X: broadcaster→PoP

	if _, ok := rewriteClockMappingBack(mapping, est); ok {
		t.Fatal("mapping served before the estimator had a sample")
	}

	// home ≈ pop + 2_000_000 (a symmetric exchange: the offset is exact).
	p0 := uint64(10_000_000)
	est.record(p0, 12_000_500, p0+1000)
	rewritten, ok := rewriteClockMappingBack(mapping, est)
	if !ok {
		t.Fatal("rewrite failed with a sample present")
	}
	got, err := wire.ParseClockMapping(rewritten)
	if err != nil {
		t.Fatalf("rewritten mapping unparseable: %v", err)
	}
	if got != 7_000_000+2_000_000 {
		t.Fatalf("rewritten offset = %d, want %d", got, 9_000_000)
	}

	// Composed with the edge's forward rewrite on the far side of home, the
	// two hops cancel: an edge whose clock equals the PoP's sees X again.
	fwd := &timeSyncEstimator{}
	fwd.record(p0, 12_000_500, p0+1000) // that edge ≈ home − 2_000_000
	back, ok := rewriteClockMapping(rewritten, fwd)
	if !ok {
		t.Fatal("forward rewrite failed")
	}
	if x, _ := wire.ParseClockMapping(back); x != 7_000_000 {
		t.Fatalf("round trip = %d, want 7000000", x)
	}

	if _, ok := rewriteClockMappingBack([]byte{0x01, 0x06, 0x00}, est); ok {
		t.Fatal("malformed mapping rewritten")
	}
}
