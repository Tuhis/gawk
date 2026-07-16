package gst

import (
	"strings"
	"testing"

	"github.com/Tuhis/gawk/gawk-broadcast/internal/engine"
)

func pipelineString(args []string) string { return strings.Join(args, " ") }

// Decision 13's invariants are acceptance criteria, not incidental pipeline
// args: the protocol and the latency story depend on them, and three hardware
// backends × driver versions is too much surface to leave to defaults. These
// assertions are how the invariants stay true when someone edits a pipeline.
func TestEveryCandidatePinsTheEncoderInvariants(t *testing.T) {
	cfg := engine.DefaultMediaConfig() // 1080p60, 500ms GOP
	for _, c := range Cascade {
		t.Run(c.Element, func(t *testing.T) {
			p := pipelineString(BuildPipeline(c, cfg, 42))

			// VFR pass-through, drop-only. A CFR converter would hold the last
			// frame until the next arrives (unbounded on a static screen) then
			// burst stale duplicates that arrival-stamping would timestamp
			// "now" — silently wrecking the latency measurement.
			if !strings.Contains(p, "videorate drop-only=true") {
				t.Errorf("no drop-only rate gate:\n%s", p)
			}
			if strings.Contains(p, "videorate ! ") || strings.Contains(p, "framerate=60/1 !") {
				t.Errorf("looks like a CFR conversion:\n%s", p)
			}

			// SPS/PPS before every IDR. Load-bearing: the DecoderConfig
			// extradata is empty on this path, so only a self-sufficient
			// keyframe can prime a late joiner.
			if !strings.Contains(p, "h264parse config-interval=-1") {
				t.Errorf("no in-band parameter sets (late joiners would never decode):\n%s", p)
			}

			// One PES = one AU.
			if !strings.Contains(p, "mpegtsmux") || !strings.Contains(p, "fdsink fd=1") {
				t.Errorf("not muxed to MPEG-TS on stdout:\n%s", p)
			}

			// The portal's fd, and the granted node.
			if !strings.Contains(p, "pipewiresrc fd=3") {
				t.Errorf("does not read the portal fd on 3:\n%s", p)
			}
			if !strings.Contains(p, "target-object=42") {
				t.Errorf("does not target the granted node:\n%s", p)
			}
		})
	}
}

// No B-frames: the whole viewer pipeline assumes decode order == presentation
// order (frameId ordering, the reorder buffer, live-edge and pacing math).
// vulkanh264enc has no B-frame support to disable, which is why this checks
// per candidate rather than blanket-asserting a property string.
func TestBFramesAreDisabledWhereTheEncoderHasThem(t *testing.T) {
	cfg := engine.DefaultMediaConfig()
	for _, tc := range []struct{ element, want string }{
		{"nvh264enc", "bframes=0"},
		{"vah264enc", "b-frames=0"},
	} {
		c, ok := FindCandidate(tc.element)
		if !ok {
			t.Fatalf("%s is not in the cascade", tc.element)
		}
		if p := pipelineString(BuildPipeline(c, cfg, 1)); !strings.Contains(p, tc.want) {
			t.Errorf("%s: no %q — B-frames would break decode-order == presentation-order:\n%s", tc.element, tc.want, p)
		}
	}
}

// The GOP reaches the encoder as frames derived from the time-based interval.
//
// vulkanh264enc is deliberately omitted: its keyframe-interval property surface
// varies across driver/GStreamer versions, so no arg is pinned for it here (see
// the candidate's comment in cascade.go). The 500 ms all-IDR GOP invariant on
// the Vulkan path is therefore a V3-fixture/on-hardware check, not an args
// assertion — the one Decision 13 invariant that is verified rather than
// asserted for the lead candidate.
func TestGOPReachesTheEncoder(t *testing.T) {
	cfg := engine.DefaultMediaConfig() // 60fps, 500ms → 30 frames
	for _, tc := range []struct{ element, want string }{
		{"nvh264enc", "gop-size=30"},
		{"vah264enc", "key-int-max=30"},
	} {
		c, _ := FindCandidate(tc.element)
		if p := pipelineString(BuildPipeline(c, cfg, 1)); !strings.Contains(p, tc.want) {
			t.Errorf("%s: no %q:\n%s", tc.element, tc.want, p)
		}
	}
}

// Trials encode videotestsrc, never the portal (Decision 4): probing must not
// pop share dialogs, which would defeat the restore token's entire purpose.
func TestTrialsNeverTouchThePortal(t *testing.T) {
	cfg := engine.DefaultMediaConfig()
	for _, c := range Cascade {
		p := pipelineString(BuildTrialPipeline(c, cfg))
		if strings.Contains(p, "pipewiresrc") {
			t.Errorf("%s trial reads the portal — probing must not ask for the screen:\n%s", c.Element, p)
		}
		if !strings.Contains(p, "videotestsrc") {
			t.Errorf("%s trial does not use videotestsrc:\n%s", c.Element, p)
		}
		// A trial must terminate on its own, or the cascade hangs.
		if !strings.Contains(p, "num-buffers=") {
			t.Errorf("%s trial is unbounded and would never exit:\n%s", c.Element, p)
		}
		// It must exercise the encoder — that is the entire point.
		if !strings.Contains(p, c.Element) {
			t.Errorf("%s trial does not run the encoder:\n%s", c.Element, p)
		}
	}
}

// H.264 wants even dimensions (docs/08 makes the same guarantee in the ladder).
func TestScaleCapsRoundsToEvenDimensions(t *testing.T) {
	cfg := engine.MediaConfig{Width: 1921, Height: 1081, Fps: 60, GOPMs: 500}
	if got := scaleCaps(cfg); !strings.Contains(got, "width=1920") || !strings.Contains(got, "height=1080") {
		t.Errorf("scaleCaps = %q, want even dimensions", got)
	}
}

// gst-launch's syntax is positional: a stray or missing "!" changes the graph.
func TestPipelineLinksAreWellFormed(t *testing.T) {
	cfg := engine.DefaultMediaConfig()
	for _, c := range Cascade {
		args := BuildPipeline(c, cfg, 1)
		if args[0] != "-q" {
			t.Errorf("%s: first arg = %q, want -q", c.Element, args[0])
		}
		for i, a := range args {
			if a != "!" {
				continue
			}
			if i == 0 || i == len(args)-1 {
				t.Errorf("%s: a link at position %d has nothing to link:\n%s", c.Element, i, pipelineString(args))
			}
			if i+1 < len(args) && args[i+1] == "!" {
				t.Errorf("%s: two consecutive links at %d:\n%s", c.Element, i, pipelineString(args))
			}
		}
	}
}

// The bitrate crosses a unit boundary: the engine speaks bps, every one of
// these elements speaks kbps. Getting it wrong by 1000x is the kind of bug
// that looks like a network problem.
func TestBitrateIsConvertedToKbps(t *testing.T) {
	cfg := engine.DefaultMediaConfig()
	cfg.BitrateBps = 8_000_000
	for _, c := range Cascade {
		p := pipelineString(BuildPipeline(c, cfg, 1))
		if !strings.Contains(p, "bitrate=8000") {
			t.Errorf("%s: want bitrate=8000 (kbps) for 8 Mbps:\n%s", c.Element, p)
		}
	}
}
