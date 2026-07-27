package main

import (
	"image"
	"testing"
	"time"

	"gioui.org/io/input"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/unit"

	gawkapp "github.com/Tuhis/gawk/gawk-broadcast/internal/app"
	"github.com/Tuhis/gawk/gawk-broadcast/internal/config"
)

// idleFrames lays the window out for n frames with nothing happening — no
// pointer, no keys, no broadcast — and reports how many of them asked to be
// redrawn immediately.
//
// A window that asks for an immediate redraw every frame free-runs at the
// compositor's rate for as long as it is open, which is a CPU burn nobody can
// see and nothing else here would catch (BUGS.md, 2026-07-27: a pre-filled
// gioui.org/x component.TextField did exactly this, from launch, for 20-30 %
// of a core). Frames, not CPU: a CPU threshold is machine-specific and would
// never fail in CI.
func idleFrames(t *testing.T, cfg *config.Config, n int) int {
	t.Helper()
	u := newUI(gawkapp.New(gawkapp.Options{Config: cfg}), cfg)
	var (
		r    input.Router
		ops  op.Ops
		now  = time.Unix(0, 0)
		imm  int
		size = image.Pt(460, 560)
	)
	for range n {
		ops.Reset()
		gtx := layout.Context{
			Ops:         &ops,
			Now:         now,
			Source:      r.Source(),
			Metric:      unit.Metric{PxPerDp: 1, PxPerSp: 1},
			Constraints: layout.Exact(size),
		}
		u.layout(gtx)
		r.Frame(gtx.Ops)
		// A wakeup already due is an "as soon as possible" invalidate. A
		// scheduled one (the caret blink of a focused field) is not, and is
		// deliberately not counted.
		if at, ok := r.WakeupTime(); ok && !at.After(now) {
			imm++
		}
		now = now.Add(16 * time.Millisecond)
	}
	return imm
}

func TestIdleWindowSchedulesNoRedraws(t *testing.T) {
	const frames = 20

	// The reported case: the settings fields are pre-filled from the saved
	// config in newUI, so this is what every real broadcaster's window does
	// from its very first paint.
	t.Run("settings pre-filled", func(t *testing.T) {
		cfg := &config.Config{
			RelayURL:      "https://relay.example.com",
			AppURL:        "https://gawk.example.com",
			PublishSecret: "hunter2",
			BitrateBps:    16_000_000,
		}
		if got := idleFrames(t, cfg, frames); got != 0 {
			t.Errorf("idle window asked for an immediate redraw on %d of %d frames, want 0", got, frames)
		}
	})

	// A first run has nothing to pre-fill. Kept because it is what made the
	// original bug survivable to diagnose — empty fields did not trigger it,
	// so a regression that only fires with text in the fields must fail the
	// case above while this one stays quiet.
	t.Run("first run, nothing configured", func(t *testing.T) {
		if got := idleFrames(t, &config.Config{}, frames); got != 0 {
			t.Errorf("idle window asked for an immediate redraw on %d of %d frames, want 0", got, frames)
		}
	})
}
