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
	"github.com/Tuhis/gawk/gawk-broadcast/internal/pwproto"
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
	return idleFramesWith(t, cfg, n, nil)
}

// idleFramesWith is idleFrames with a chance to put the app into a particular
// state first — R35's whose-audio card is a whole new set of widgets, and a
// window that free-runs only while the card is open would pass the plain case.
func idleFramesWith(t *testing.T, cfg *config.Config, n int, prepare func(*gawkapp.App)) int {
	t.Helper()
	a := gawkapp.New(gawkapp.Options{Config: cfg})
	if prepare != nil {
		prepare(a)
	}
	u := newUI(a, cfg)
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

// R37 SP8: the picker mirrors the config's profiles, a hand-built flat config
// is migrated on the way in, and a dropdown selection commits + persists
// through the same path the config package tests in depth.
func TestServerPickerDrivesTheConfig(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerProfile{
			{Name: "Homelab", URL: "https://relay.home.example:4433", PublishSecret: "s3"},
		},
		SelectedServer: "Homelab",
	}
	a := gawkapp.New(gawkapp.Options{Config: cfg})
	u := newUI(a, cfg)
	if u.serverSel != 1 {
		t.Fatalf("serverSel = %d, want 1 (the selected custom profile)", u.serverSel)
	}
	if got := u.relay.Text(); got != "https://relay.home.example:4433" {
		t.Errorf("URL field = %q, want the selected profile's", got)
	}

	// A dropdown choice made during layout is applied on the next
	// handleEvents pass: selection moves to the default and persists.
	u.serverPick.sel = 0
	var (
		r   input.Router
		ops op.Ops
	)
	gtx := layout.Context{
		Ops: &ops, Now: time.Unix(0, 0), Source: r.Source(),
		Metric: unit.Metric{PxPerDp: 1, PxPerSp: 1}, Constraints: layout.Exact(image.Pt(460, 560)),
	}
	u.handleEvents(gtx)
	if cfg.SelectedServer != config.DefaultServerName {
		t.Errorf("SelectedServer = %q after picking the default, want %q", cfg.SelectedServer, config.DefaultServerName)
	}
	if u.serverSel != 0 {
		t.Errorf("serverSel = %d, want 0", u.serverSel)
	}
	// The profile left behind kept its data.
	if got := cfg.CustomServers()[0]; got.URL != "https://relay.home.example:4433" || got.PublishSecret != "s3" {
		t.Errorf("profile after switching away = %+v", got)
	}
}

// A pre-R37 flat config handed straight to newUI (bypassing Load) is folded
// into the same profile shape.
func TestFlatConfigIsMigratedIntoThePicker(t *testing.T) {
	cfg := &config.Config{RelayURL: "https://relay.example.com", PublishSecret: "hunter2"}
	a := gawkapp.New(gawkapp.Options{Config: cfg})
	u := newUI(a, cfg)
	if u.serverSel != 1 {
		t.Fatalf("serverSel = %d, want the migrated profile selected", u.serverSel)
	}
	if got := u.relay.Text(); got != "https://relay.example.com" {
		t.Errorf("URL field = %q, want the migrated URL", got)
	}
	if relayURL, secret := cfg.Server(); relayURL != "https://relay.example.com" || secret != "hunter2" {
		t.Errorf("Server() = (%q, %q), want the flat pair preserved through migration", relayURL, secret)
	}
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

	// R35's whose-audio card: a list of buttons that appears mid-start, with
	// live-updating contents. Every one of those is a chance to reintroduce
	// exactly the bug this test exists for, and the card is on screen at the
	// worst possible moment — while the broadcaster is waiting to go live.
	t.Run("the whose-audio card is open", func(t *testing.T) {
		cfg := &config.Config{RelayURL: "https://relay.example.com", AudioApp: "supertuxkart"}
		got := idleFramesWith(t, cfg, frames, func(a *gawkapp.App) {
			go a.ChooseAudioTargetForTest(gawkapp.AppAudioOfferForTest([]pwproto.App{
				{Binary: "supertuxkart", Name: "SuperTuxKart", Streams: 1},
				{Binary: "firefox", Name: "Firefox", Streams: 2},
			}, nil))
			// The card opens on another goroutine; wait for it rather than
			// racing the first frame.
			deadline := time.Now().Add(2 * time.Second)
			for a.AudioPromptState() == nil && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
		})
		if got != 0 {
			t.Errorf("the window asked for an immediate redraw on %d of %d frames with the card open, want 0", got, frames)
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
