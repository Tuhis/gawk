package pubsim

import (
	"sort"
	"testing"

	"github.com/Tuhis/gawk/gawk-broadcast/internal/fixture"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// The R30 large-frame fixture exists for exactly one property: its delta
// frames must exceed the ~8-chunk burst threshold (docs/34 finding 4), or
// the striped e2e pass engages nothing and asserts the designed no-op
// (docs/35 §5.4's "nothing to split" hold). This test pins that property so
// a regenerated clip that compresses under the threshold fails loudly here
// instead of silently hollowing out the pass.
func TestLargeFixtureDeltasExceedBurstThreshold(t *testing.T) {
	aus, err := Demux(fixture.TSLarge)
	if err != nil {
		t.Fatalf("Demux(TSLarge): %v", err)
	}
	if len(aus) < 55 {
		t.Fatalf("large fixture has %d access units, want ~60 (2 s @ 30 fps)", len(aus))
	}

	var deltaChunks []int
	keyframes := 0
	for _, au := range aus {
		if au.Keyframe {
			keyframes++
			continue
		}
		deltaChunks = append(deltaChunks, (len(au.Data)+wire.MaxChunkPayload-1)/wire.MaxChunkPayload)
	}
	// GOP 15 over ~60 frames: 4 keyframes (same cadence contract as TS).
	if keyframes < 3 || keyframes > 6 {
		t.Fatalf("keyframes = %d, want ~4 (GOP 15 over 60 frames)", keyframes)
	}
	sort.Ints(deltaChunks)
	median := deltaChunks[len(deltaChunks)/2]
	p10 := deltaChunks[len(deltaChunks)/10]
	max := deltaChunks[len(deltaChunks)-1]
	t.Logf("delta chunks: p10=%d median=%d max=%d over %d deltas", p10, median, max, len(deltaChunks))

	// The load-bearing bound: at the stripe target of 6 chunks/leg, a median
	// past 12 sizes the controller to N >= 2 even before parity, and past the
	// threshold of 8 every delta is in the lossy regime the pass exercises.
	if median <= 12 {
		t.Fatalf("median delta = %d chunks, want > 12 — the fixture no longer exceeds the burst threshold with margin", median)
	}
	if p10 <= 8 {
		t.Fatalf("p10 delta = %d chunks, want > 8 — a tail of small deltas dilutes the striped pass", p10)
	}
	// Sanity the other way: parity's MDS guard and the reorder buffer both
	// assume deltas nowhere near the 255-chunk wall.
	if max > 64 {
		t.Fatalf("max delta = %d chunks — implausibly large for a 5 Mbps 30 fps clip; the encode drifted", max)
	}
}
