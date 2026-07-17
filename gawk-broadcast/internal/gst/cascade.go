// Package gst runs capture and encode in a GStreamer child process and turns
// its MPEG-TS output into access units (R14 Decisions 3, 4 and 7, docs/19).
//
// A subprocess, not in-process bindings, for two honest reasons: **crash
// isolation** — this is flaky NVIDIA-on-Wayland driver territory, and a dying
// child is a notification whereas an in-process crash takes the GUI down with
// it — and **version tolerance**: any distro GStreamer ≥1.24 works, and the
// elements resolve at runtime. ("No cgo" is not one of the reasons; Gio
// already requires cgo.)
//
// The picture never touches our address space: dmabufs go from the portal to
// the GPU's encode block, and what comes back down the pipe is already H.264.
// That is why this whole app can be a byte pump.
package gst

import (
	"context"
	"fmt"
	"strings"

	"github.com/Tuhis/gawk/gawk-broadcast/internal/engine"
)

// Candidate is one hardware encoder in the cascade.
//
// There is no software rung, by decision (Decision 4): hardware encode is the
// entire reason this app exists, and the browser broadcaster already covers
// software encode on Linux perfectly well. A machine where nothing passes is
// told to use the browser, not quietly degraded to x264 — which would burn the
// game's CPU budget to produce a stream the browser could have produced.
type Candidate struct {
	// Element is the GStreamer encoder element name.
	Element string
	// Name is what we report in stats and the UI.
	Name string
	// Package names the distro package that provides the element, so a
	// missing one produces a sentence rather than a stack trace.
	Package string
	// convert returns the elements between the source and the encoder. They
	// keep the frame on the GPU: the encoder wants its own memory type, and
	// the conversion is a GPU operation, not a readback.
	convert func(cfg engine.MediaConfig) []string
	// encArgs returns the encoder element plus its properties.
	encArgs func(cfg engine.MediaConfig) []string
	// memory is the GstCapsFeatures name of the candidate's GPU memory type,
	// pinned on the encoder caps in auto capture so the converter→encoder
	// handoff never falls to system memory (see encoderCaps).
	memory string
}

// EncoderAPI names the encode API behind a cascade element, for humans: this
// is what the GUI's indicator shows, because "VA-API" answers "which driver
// stack am I debugging?" where "vah264enc" answers only "which element".
func EncoderAPI(element string) string {
	switch element {
	case "vulkanh264enc":
		return "Vulkan Video"
	case "nvh264enc":
		return "NVENC"
	case "vah264enc":
		return "VA-API"
	default:
		return element
	}
}

// Cascade is the ordered preference list.
//
// Vulkan leads by user decision (2026-07-15), faithful to Decision 21: Vulkan
// Video is the target encode API — the only one spanning RADV, ANV and NVIDIA
// with no asterisk — so it gets exercised on every capable machine from day
// one rather than waiting for V8.
//
// Direct VAAPI is deliberately *not* the answer here and vah264enc sits last:
// Chromium's Linux backend is VA-API only, and that is exactly why it cannot
// encode on NVIDIA. VAAPI is vendor-neutral for Intel and AMD and structurally
// excludes the third vendor.
//
// All three are permanent infrastructure, not scaffolding toward V8: R1 made
// gawk multi-broadcaster, and friends publish from GPUs we cannot survey.
var Cascade = []Candidate{
	{
		Element: "vulkanh264enc",
		Name:    "vulkanh264enc",
		Package: "gstreamer1.0-plugins-bad",
		convert: func(cfg engine.MediaConfig) []string {
			return []string{"vulkanupload", "vulkancolorconvert"}
		},
		// Two Decision 13 invariants are deliberately not pinned as args here,
		// and this is the one candidate where that is a live question rather
		// than a settled one:
		//
		//   - No b-frames property: vulkanh264enc's H.264 Vulkan Video encode is
		//     I+P only in the GStreamer versions we target, so there is no
		//     B-frame knob to set to 0 (TestBFramesAreDisabled… checks nv/va
		//     only, for exactly this reason).
		//   - No GOP / key-int property: vulkanh264enc's keyframe-interval
		//     property surface varies across driver and GStreamer versions, so
		//     pinning a name we cannot verify here would fail the pipeline
		//     launch and reject Vulkan — the *lead* candidate — outright.
		//
		// So the 500 ms all-IDR GOP invariant on the Vulkan path is verified on
		// real hardware (V2 trial + V3 fixture: keyframe cadence ≈ 500 ms, every
		// keyframe IDR), not asserted in args — the standing "advisory probe,
		// runtime/fixture wins" rule. It is the first thing to check when
		// validating Vulkan on the gaming PC: if the default GOP is wrong, this
		// candidate needs a version-specific arg, or drops below nv/va.
		memory: "memory:VulkanImage",
		encArgs: func(cfg engine.MediaConfig) []string {
			return []string{
				"vulkanh264enc",
				"rate-control=cbr",
				fmt.Sprintf("bitrate=%d", cfg.BitrateBps/1000), // kbps
			}
		},
	},
	{
		Element: "nvh264enc",
		Name:    "nvh264enc",
		Package: "gstreamer1.0-plugins-bad (nvcodec)",
		convert: func(cfg engine.MediaConfig) []string {
			// cudaupload keeps the frame on the GPU where the dmabuf import
			// works; this is the step most likely to fail on the proprietary
			// driver, and the risk V2 exists to settle on real hardware.
			return []string{"cudaupload", "cudaconvertscale"}
		},
		memory: "memory:CUDAMemory",
		encArgs: func(cfg engine.MediaConfig) []string {
			return []string{
				"nvh264enc",
				"rc-mode=cbr",
				"zerolatency=true",
				fmt.Sprintf("bitrate=%d", cfg.BitrateBps/1000), // kbps
				fmt.Sprintf("gop-size=%d", gopFrames(cfg)),
				"bframes=0", // Decision 13: decode order == presentation order
			}
		},
	},
	{
		Element: "vah264enc",
		Name:    "vah264enc",
		Package: "gstreamer1.0-plugins-bad (va)",
		convert: func(cfg engine.MediaConfig) []string {
			return []string{"vapostproc"}
		},
		memory: "memory:VAMemory",
		encArgs: func(cfg engine.MediaConfig) []string {
			return []string{
				"vah264enc",
				"rate-control=cbr",
				fmt.Sprintf("bitrate=%d", cfg.BitrateBps/1000), // kbps
				fmt.Sprintf("key-int-max=%d", gopFrames(cfg)),
				"b-frames=0", // Decision 13
			}
		},
	},
}

// gopFrames converts the GOP interval in milliseconds to frames.
//
// The cadence is time-based on purpose (item 11): a frame-count GOP is 24 s at
// 5 fps. The encoders only take frames, so the conversion happens here, where
// the session's fps is known and fixed.
func gopFrames(cfg engine.MediaConfig) int {
	if cfg.Fps <= 0 || cfg.GOPMs <= 0 {
		return 30
	}
	n := cfg.Fps * cfg.GOPMs / 1000
	if n < 1 {
		n = 1
	}
	return n
}

// FindCandidate returns the cascade entry with the given element name.
func FindCandidate(element string) (Candidate, bool) {
	for _, c := range Cascade {
		if c.Element == element || c.Name == element {
			return c, true
		}
	}
	return Candidate{}, false
}

// CandidateNames lists the cascade, for error messages and help text.
func CandidateNames() []string {
	names := make([]string, 0, len(Cascade))
	for _, c := range Cascade {
		names = append(names, c.Element)
	}
	return names
}

// TrialFunc runs a trial encode of one candidate and reports whether it
// actually encodes on this machine.
//
// Injectable so the cascade's decision logic is a unit test rather than a GPU
// requirement.
type TrialFunc func(ctx context.Context, c Candidate, cfg engine.MediaConfig) error

// SelectEncoder picks the encoder to use.
//
// The rules (Decision 4), in order:
//
//   - An explicit override forces that candidate and skips the cascade
//     entirely. The user asked; the live start will tell them if they were
//     wrong.
//   - Otherwise the cached last-good encoder is re-verified first. Four serial
//     probes before every session is a slow start for a machine whose answer
//     has not changed since yesterday — but it is still runtime truth, because
//     we re-verify rather than trust the cache.
//   - Otherwise the full cascade, in order, first one that *actually encodes*.
//
// Availability is never the test. gst-inspect-1.0 happily lists elements that
// fail at runtime on the actual device — a VA element on a machine whose
// driver cannot encode, an NVIDIA element with no usable NVENC. This is R13's
// probe-matrix instinct one layer down, and it inherits R13's caveat: the
// probe's answer is advisory, and runtime wins. Which is the project's
// standing getSettings() rule in another costume — trust the thing actually in
// hand, not the metadata describing it.
func SelectEncoder(ctx context.Context, cfg engine.MediaConfig, lastGood string, trial TrialFunc) (Candidate, error) {
	if cfg.Encoder != "" {
		c, ok := FindCandidate(cfg.Encoder)
		if !ok {
			return Candidate{}, fmt.Errorf("unknown encoder %q: choose one of %s",
				cfg.Encoder, strings.Join(CandidateNames(), ", "))
		}
		return c, nil
	}

	order := Cascade
	if lastGood != "" {
		if c, ok := FindCandidate(lastGood); ok {
			if err := trial(ctx, c, cfg); err == nil {
				return c, nil
			}
			// The cached answer went stale (driver update, GPU swap, someone
			// else holding the encode engine). Fall through to the full
			// cascade rather than refusing.
		}
	}

	var failures []string
	for _, c := range order {
		if err := trial(ctx, c, cfg); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", c.Element, err))
			continue
		}
		return c, nil
	}
	return Candidate{}, fmt.Errorf("%w\n  tried: %s", engine.ErrNoHardwareEncoder, strings.Join(failures, "\n         "))
}

// NoHardwareMessage is what a user sees when nothing in the cascade encodes.
// It points at the browser rather than degrading, per Decision 4 — and says
// why, because "not supported" invites a flag hunt that cannot succeed.
const NoHardwareMessage = `No working hardware H.264 encoder was found on this machine.

gawk-broadcast exists to do hardware encode, which the browser cannot do on
Linux, so there is deliberately no software fallback here: software encode is
the browser's job, and it does it well.

Use the browser broadcaster instead — open the gawk web app and start a
broadcast there. It will encode in software, which costs some CPU but works.

If you believe this machine does have a usable encoder, run with
-encoder <element> to force one (` + `vulkanh264enc, nvh264enc, vah264enc` + `),
and check that the GStreamer plugin packages are installed.`
