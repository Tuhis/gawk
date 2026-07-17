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
		for _, mode := range CaptureModes {
			t.Run(c.Element+"/"+mode.String(), func(t *testing.T) {
				p := pipelineString(BuildPipeline(c, cfg, 42, mode))

				// VFR pass-through, drop-only. A CFR converter would hold the last
				// frame until the next arrives (unbounded on a static screen) then
				// burst stale duplicates that arrival-stamping would timestamp
				// "now" — silently wrecking the latency measurement. A framerate
				// pin on the encoder caps is fine — with drop-only=true it is
				// nominal-rate signalling, not CFR (no frame is ever synthesized) —
				// but a videorate without drop-only would be the real thing.
				if !strings.Contains(p, "videorate drop-only=true") {
					t.Errorf("no drop-only rate gate:\n%s", p)
				}
				if strings.Contains(p, "videorate ! ") {
					t.Errorf("bare videorate — looks like a CFR conversion:\n%s", p)
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
				// The node is selected with `path=<global id>`, NOT `target-object`.
				// The portal's Start response gives the node's global object id;
				// pipewiresrc's newer target-object property matches a node *name* or
				// *object.serial* instead, so target-object=<global id> fails at
				// runtime with "stream error: target not found". path takes the
				// global id directly.
				if !strings.Contains(p, "path=42") {
					t.Errorf("does not select the granted node by path=<global id>:\n%s", p)
				}
				if strings.Contains(p, "target-object=") {
					t.Errorf("uses target-object (matches name/serial, not the portal's global node id):\n%s", p)
				}
			})
		}
	}
}

// The capture-mode ladder exists because pipewiresrc's free negotiation can
// die during preroll with "stream error: unhandled format" (DMA-BUF modifier
// skew, 10-bit HDR desktops — seen live 2026-07-16 on AMD + vah264enc). The
// fallback rung pins the portal boundary to plain system memory, which forces
// modifier-less formats the compositor satisfies with CPU-visible buffers.
func TestSystemMemoryCapturePinsThePortalBoundary(t *testing.T) {
	cfg := engine.DefaultMediaConfig()
	for _, c := range Cascade {
		p := pipelineString(BuildPipeline(c, cfg, 42, CaptureSystemMemory))
		// The caps pin must sit directly on pipewiresrc, before anything else
		// gets a say in negotiation.
		if !strings.Contains(p, "do-timestamp=true ! video/x-raw ! videorate") {
			t.Errorf("%s: system-memory mode does not pin bare video/x-raw at the source:\n%s", c.Element, p)
		}
		// And a CPU converter after the rate gate (never before — dropped
		// frames must not be converted) covers formats the encoder's own
		// converter cannot import from system memory.
		if !strings.Contains(p, "videorate drop-only=true max-rate=60 ! videoconvert") {
			t.Errorf("%s: system-memory mode has no videoconvert after the rate gate:\n%s", c.Element, p)
		}
	}
}

// The default mode must stay byte-identical to the zero-copy pipeline: no caps
// pin at the source (that is what lets DMA-BUF through) and no CPU converter.
func TestAutoCaptureKeepsZeroCopyNegotiation(t *testing.T) {
	cfg := engine.DefaultMediaConfig()
	for _, c := range Cascade {
		p := pipelineString(BuildPipeline(c, cfg, 42, CaptureAuto))
		if strings.Contains(p, "do-timestamp=true ! video/x-raw !") {
			t.Errorf("%s: auto mode pins caps at the source, which forbids DMA-BUF:\n%s", c.Element, p)
		}
		if strings.Contains(p, "videoconvert") {
			t.Errorf("%s: auto mode inserts a CPU converter:\n%s", c.Element, p)
		}
	}
}

// The encoder caps must carry the nominal framerate. The portal's caps are
// framerate=0/1 (variable — damage-driven capture), and vah264enc budgets its
// rate control for an assumed 30 fps when it sees 0/1 (verified in
// gst-plugins-bad source, field finding 2026-07-17): at 60 fps that halves the
// effective bitrate and motion turns to mush. Pinning framerate=<fps>/1 after
// the drop-only gate tells the encoder the true budget without ever
// synthesizing a frame.
func TestEncoderCapsCarryTheNominalFramerate(t *testing.T) {
	cfg := engine.DefaultMediaConfig() // 60 fps
	for _, c := range Cascade {
		for _, mode := range CaptureModes {
			p := pipelineString(BuildPipeline(c, cfg, 1, mode))
			if !strings.Contains(p, "framerate=60/1") {
				t.Errorf("%s/%s: encoder caps carry no framerate — vah264enc will budget for 30 fps:\n%s",
					c.Element, mode, p)
			}
		}
	}
}

// In auto mode the candidate's converter sits directly on pipewiresrc: the
// DMA-BUF allocation query must flow between them with no element in between.
// The 2026-07-17 field failure died in pipewiresrc's finish/allocation step
// with videorate sitting in the middle — the caps mapped fine, the buffer
// pool negotiation did not. The rate gate moves after the converter (a GPU
// convert of a frame that is then dropped is the price of zero-copy import).
func TestAutoCapturePutsTheConverterOnThePortalBoundary(t *testing.T) {
	cfg := engine.DefaultMediaConfig()
	for _, c := range Cascade {
		p := pipelineString(BuildPipeline(c, cfg, 42, CaptureAuto))
		first := c.convert(cfg)[0]
		if !strings.Contains(p, "do-timestamp=true ! "+first) {
			t.Errorf("%s: auto mode does not put %s directly on pipewiresrc:\n%s", c.Element, first, p)
		}
	}
}

// In auto mode the converter→encoder handoff stays in the candidate's GPU
// memory. A bare video/x-raw capsfilter means system memory: the converter
// would download every frame for the encoder to re-upload — a round trip per
// frame that silently costs more than the conversion itself.
func TestAutoCaptureEncoderCapsStayOnTheGpu(t *testing.T) {
	cfg := engine.DefaultMediaConfig()
	for _, tc := range []struct{ element, feature string }{
		{"vulkanh264enc", "memory:VulkanImage"},
		{"nvh264enc", "memory:CUDAMemory"},
		{"vah264enc", "memory:VAMemory"},
	} {
		c, ok := FindCandidate(tc.element)
		if !ok {
			t.Fatalf("%s is not in the cascade", tc.element)
		}
		p := pipelineString(BuildPipeline(c, cfg, 1, CaptureAuto))
		if !strings.Contains(p, "video/x-raw("+tc.feature+")") {
			t.Errorf("%s: encoder caps do not pin %s — the handoff drops to system memory:\n%s",
				tc.element, tc.feature, p)
		}
		// System-memory capture keeps bare caps on purpose: it is the proven
		// fallback rung, and its negotiation semantics stay untouched.
		ps := pipelineString(BuildPipeline(c, cfg, 1, CaptureSystemMemory))
		if strings.Contains(ps, "video/x-raw(") {
			t.Errorf("%s: system-memory mode pins a GPU memory feature:\n%s", tc.element, ps)
		}
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
		if p := pipelineString(BuildPipeline(c, cfg, 1, CaptureAuto)); !strings.Contains(p, tc.want) {
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
		if p := pipelineString(BuildPipeline(c, cfg, 1, CaptureAuto)); !strings.Contains(p, tc.want) {
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
func TestEncoderCapsRoundToEvenDimensions(t *testing.T) {
	cfg := engine.MediaConfig{Width: 1921, Height: 1081, Fps: 60, GOPMs: 500}
	for _, mode := range CaptureModes {
		got := encoderCaps(Cascade[len(Cascade)-1], cfg, mode)
		if !strings.Contains(got, "width=1920") || !strings.Contains(got, "height=1080") {
			t.Errorf("encoderCaps (%s) = %q, want even dimensions", mode, got)
		}
	}
}

// gst-launch's syntax is positional: a stray or missing "!" changes the graph.
func TestPipelineLinksAreWellFormed(t *testing.T) {
	cfg := engine.DefaultMediaConfig()
	for _, c := range Cascade {
		args := BuildPipeline(c, cfg, 1, CaptureAuto)
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
	for _, tc := range []struct{ element, want string }{
		// vulkan: CBR at the configured rate (see the VBR test for why).
		{"vulkanh264enc", "bitrate=8000"},
		// nv: VBR — bitrate is the *target* (75%), max-bitrate the ceiling.
		{"nvh264enc", "bitrate=6000"},
		{"nvh264enc", "max-bitrate=8000"},
		// va: VBR — bitrate is the *ceiling*, target-percentage the target.
		{"vah264enc", "bitrate=8000"},
	} {
		c, _ := FindCandidate(tc.element)
		if p := pipelineString(BuildPipeline(c, cfg, 1, CaptureAuto)); !strings.Contains(p, tc.want) {
			t.Errorf("%s: want %q (kbps) for 8 Mbps:\n%s", tc.element, tc.want, p)
		}
	}
}

// The configured bitrate is a *ceiling*, not a constant spend: CBR poured the
// full rate into any sustained motion whether the scene needed it or not
// (field report 2026-07-17: "default bandwidth a bit too high"). VBR with a
// 75% target keeps typical motion around three quarters of the cap, lets
// complex scenes burst to it, and — with damage-driven capture — spends next
// to nothing on a static screen. The two elements spell it differently:
// vah264enc's `bitrate` is the max and `target-percentage` the target
// fraction; nvh264enc's `bitrate` is the target and `max-bitrate` the max.
// vulkanh264enc stays CBR on purpose — its rate-control property surface is
// unverified on hardware (the same reason it pins no GOP arg), and a wrong
// enum value would reject the *lead* candidate at launch.
func TestRateControlIsVBRWithABitrateCeiling(t *testing.T) {
	cfg := engine.DefaultMediaConfig() // 16 Mbps ceiling
	for _, tc := range []struct{ element, want string }{
		{"vah264enc", "rate-control=vbr"},
		{"vah264enc", "target-percentage=75"},
		{"nvh264enc", "rc-mode=vbr"},
		{"nvh264enc", "bitrate=12000"},
		{"nvh264enc", "max-bitrate=16000"},
		{"vulkanh264enc", "rate-control=cbr"},
	} {
		c, ok := FindCandidate(tc.element)
		if !ok {
			t.Fatalf("%s is not in the cascade", tc.element)
		}
		if p := pipelineString(BuildPipeline(c, cfg, 1, CaptureAuto)); !strings.Contains(p, tc.want) {
			t.Errorf("%s: want %q:\n%s", tc.element, tc.want, p)
		}
	}
}
