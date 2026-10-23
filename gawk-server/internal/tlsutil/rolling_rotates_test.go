package tlsutil

import (
	"testing"
	"time"
)

// RotatesAt is when the serving certificate hands over to the next one:
// the console's "rotates in n days" (docs/76 §7.1 Health).
func TestRollingRotatesAt(t *testing.T) {
	start := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	clk := &fakeClock{now: start}
	r := newTestRolling(t, t.TempDir(), clk)
	if got, want := r.RotatesAt(), start.Add(DefaultRollingServe); !got.Equal(want) {
		t.Fatalf("fresh: RotatesAt = %v, want %v", got, want)
	}
	clk.Advance(DefaultRollingServe + time.Hour)
	r.Tick()
	if got, want := r.RotatesAt(), start.Add(2*DefaultRollingServe); !got.Equal(want) {
		t.Fatalf("after one rotation: RotatesAt = %v, want %v", got, want)
	}
}
