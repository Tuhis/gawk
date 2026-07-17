package gst

import (
	"fmt"

	"github.com/Tuhis/gawk/gawk-broadcast/internal/engine"
)

// childPipeWireFD is the descriptor the portal fd lands on in the child.
// exec.Cmd.ExtraFiles[0] is always fd 3.
const childPipeWireFD = 3

// LaunchBinary is the GStreamer CLI we drive. A stock distro package
// (gstreamer1.0-tools); nothing here is built from source.
const LaunchBinary = "gst-launch-1.0"

// CaptureMode selects how frames may travel across the pipewiresrc boundary.
//
// CaptureAuto lets PipeWire negotiate freely, which on a healthy stack picks
// DMA-BUF and keeps the frame on the GPU end to end. That negotiation has a
// real-world failure mode, though: pipewiresrc dies during preroll with
// "stream error: unhandled format" when the format the compositor chose cannot
// be mapped back onto the downstream caps — DMA-BUF modifier / DRM-caps
// version skew between gst-plugin-pipewire and the encoder's converter, or a
// 10-bit format from an HDR desktop. Seen live 2026-07-16 (AMD, vah264enc;
// docs/19 gotchas).
//
// CaptureSystemMemory is the fallback rung: it pins the boundary to plain
// system-memory video/x-raw, so pipewiresrc only offers modifier-less formats
// and the compositor has to export CPU-visible buffers. One copy per frame,
// but immune to the modifier dance.
type CaptureMode int

const (
	CaptureAuto CaptureMode = iota
	CaptureSystemMemory
)

func (m CaptureMode) String() string {
	if m == CaptureSystemMemory {
		return "system-memory"
	}
	return "auto"
}

// CaptureModes is the in-order ladder the live start walks per encoder:
// zero-copy first, system memory when the free negotiation fails.
var CaptureModes = []CaptureMode{CaptureAuto, CaptureSystemMemory}

// CaptureFormatMessage is what a user sees when every live pipeline died
// inside pipewiresrc. Deliberately distinct from NoHardwareMessage: no frame
// ever reached an encoder, so "no working hardware encoder, use the browser"
// would send the user chasing GPU drivers over a compositor negotiation
// problem — the same misdiagnosis ErrNoLaunchBinary exists to prevent.
const CaptureFormatMessage = `Screen capture failed: your compositor's screencast stream and GStreamer's
pipewiresrc could not agree on a frame format. The GPU encoder is fine — the
pipeline died before any frame reached it.

This is a compositor / PipeWire / GStreamer combination problem (typically
DMA-BUF modifier negotiation, or a 10-bit HDR desktop format). gawk-broadcast
already retried with plain system-memory capture, and that failed too.

Things worth checking:
  - pipewire and gstreamer packages from the same era (a new compositor with
    an old gst-plugin-pipewire is the classic culprit)
  - whether another portal capture works on this machine (OBS, Kooha)

To capture the negotiation detail for a bug report, rerun with
GST_DEBUG=pipewire*:5 set in the environment and save the log output.`

// BuildPipeline returns the gst-launch-1.0 arguments for a live capture.
//
// The shape, and why each link is what it is:
//
//	pipewiresrc fd=3 path=N            the portal's granted stream (Decision 5)
//	auto:   ! <hw convert/scale>       directly on the source — see below
//	        ! videorate drop-only=true VFR pass-through — see below
//	sysmem: ! video/x-raw              CPU-visible formats only
//	        ! videorate drop-only=true
//	        ! videoconvert ! <hw convert/scale>
//	! caps WxH + framerate             scale target + nominal rate — see below
//	! <hw encoder>                     the GPU's encode block (Decision 4)
//	! h264parse config-interval=-1     SPS/PPS before every IDR (load-bearing)
//	! mpegtsmux ! fdsink fd=1          one PES = one AU (Decision 7)
//
// The parts that are easy to get subtly wrong:
//
// **videorate drop-only=true** and never a plain CFR conversion. Portal
// capture is damage-driven: nothing arrives while the screen is static. A CFR
// converter holds the last frame waiting for the *next* one — unbounded on a
// still screen — then emits a burst of stale duplicates, which arrival
// stamping would cheerfully timestamp "now". Dropping is the only rate control
// compatible with stamping frames as they arrive (Decision 13).
//
// **The encoder caps carry framerate=<fps>/1** even though the stream is VFR.
// The nominal rate is what the encoder budgets rate control against; without
// it the caps say 0/1 and vah264enc silently assumes 30 fps (verified in
// gst-plugins-bad source) — at 60 fps that halves the effective bitrate and
// motion turns to mush (field finding 2026-07-17). With drop-only upstream
// this is signalling, not CFR: no frame is ever synthesized, and when capture
// runs under the nominal rate the encoder simply underruns the bitrate.
//
// **Auto mode puts the converter directly on pipewiresrc.** The 2026-07-17
// zero-copy failure died in pipewiresrc's finish/allocation step with
// videorate sitting between it and vapostproc: the caps mapped, the DMA-BUF
// buffer-pool negotiation did not. With the converter adjacent, the
// allocation query flows to the one element that can actually provide a
// GPU-importing pool. The rate gate then runs on converted frames — a GPU
// convert of a frame that is later dropped is the price of zero-copy import,
// and it is the GPU's video block paying it, not the CPU. System-memory mode
// keeps the gate first: there the convert is CPU work, and dropped frames
// must never be converted.
//
// **h264parse config-interval=-1** puts SPS/PPS in front of every IDR. On this
// path the DecoderConfig extradata is empty (we emit raw Annex-B and never
// build an avcC record), so a late joiner primed with the relay's cached
// keyframe can only decode if the parameter sets are inside the keyframe AU
// itself. Without this, late joiners see nothing and everyone already watching
// is fine — the worst kind of bug.
func BuildPipeline(c Candidate, cfg engine.MediaConfig, nodeID uint32, mode CaptureMode) []string {
	args := []string{"-q"}
	args = append(args,
		"pipewiresrc",
		fmt.Sprintf("fd=%d", childPipeWireFD),
		// Select the granted node by its global object id via `path`, NOT
		// `target-object`. The ScreenCast portal's Start response gives the
		// node's global id (streams[].node_id); pipewiresrc's newer
		// target-object property matches a node *name* or *object.serial*
		// instead, so target-object=<global id> fails at runtime with
		// "stream error: target not found". `path` is deprecated in favour of
		// target-object but is the property that still takes the global id —
		// and it is what portal-screencast pipelines universally use.
		fmt.Sprintf("path=%d", nodeID),
		// Damage-driven capture with no clock slaving: the timestamps we care
		// about are stamped on arrival at our end anyway.
		"do-timestamp=true",
	)
	if mode == CaptureSystemMemory {
		// Pin the boundary before anything else sees caps: bare video/x-raw
		// (no memory feature) makes pipewiresrc offer only modifier-less
		// formats, which the compositor satisfies with CPU-visible buffers.
		args = append(args, "!", "video/x-raw")
		args = append(args, "!")
		args = append(args, rateGate(cfg)...)
		// videoconvert after the rate gate on purpose — dropped frames are
		// never CPU-converted. It covers a compositor whose system-memory
		// format the encoder's own converter cannot import (10-bit HDR
		// desktops); it is passthrough whenever downstream already accepts
		// the format.
		args = append(args, "!", "videoconvert")
		args = append(args, "!")
		args = append(args, c.convert(cfg)...)
	} else {
		args = append(args, "!")
		args = append(args, c.convert(cfg)...)
		args = append(args, "!")
		args = append(args, rateGate(cfg)...)
	}
	args = append(args, "!", encoderCaps(c, cfg, mode))
	args = append(args, "!")
	args = append(args, c.encArgs(cfg)...)
	args = append(args, "!", "h264parse", "config-interval=-1")
	args = append(args, "!", "mpegtsmux", "!", "fdsink", "fd=1")
	return withLinks(args)
}

// BuildTrialPipeline returns the arguments for a trial encode.
//
// It encodes videotestsrc — **never the portal** (Decision 4). Probing must not
// pop a share dialog: the whole point of the restore token is that the user
// picks their screen once, and a cascade that asked permission four times
// before every session would be worse than no cascade at all.
//
// A videotestsrc trial cannot prove the *real* path's dmabuf import, which is
// why the live start is the final probe and the cascade advances on live
// failure too.
func BuildTrialPipeline(c Candidate, cfg engine.MediaConfig) []string {
	args := []string{"-q",
		"videotestsrc", "num-buffers=10",
		"!", fmt.Sprintf("video/x-raw,width=%d,height=%d,framerate=%d/1", trialWidth, trialHeight, cfg.Fps),
	}
	args = append(args, "!")
	args = append(args, c.convert(cfg)...)
	args = append(args, "!")
	args = append(args, c.encArgs(cfg)...)
	args = append(args, "!", "fakesink")
	return withLinks(args)
}

// Trials run small: we are asking "does this element encode at all on this
// device", not "how fast". A 1080p trial would add seconds to startup for the
// same yes/no.
const (
	trialWidth  = 320
	trialHeight = 240
)

func rateGate(cfg engine.MediaConfig) []string {
	return []string{
		"videorate",
		"drop-only=true",
		fmt.Sprintf("max-rate=%d", cfg.Fps),
	}
}

// encoderCaps is the capsfilter in front of the encoder: the scale target,
// the nominal framerate (rate-control budget — see BuildPipeline), and, in
// auto mode, the candidate's GPU memory feature so the converter→encoder
// handoff never drops to system memory (a bare video/x-raw capsfilter means
// exactly system memory, which forces a download + re-upload round trip per
// frame). System-memory capture keeps bare caps: it is the proven fallback
// rung and its negotiation semantics stay untouched.
func encoderCaps(c Candidate, cfg engine.MediaConfig, mode CaptureMode) string {
	// H.264 wants even dimensions; the ladder math elsewhere in the project
	// makes the same guarantee (docs/08).
	w, h := cfg.Width&^1, cfg.Height&^1
	media := "video/x-raw"
	if mode == CaptureAuto && c.memory != "" {
		media = fmt.Sprintf("video/x-raw(%s)", c.memory)
	}
	return fmt.Sprintf("%s,width=%d,height=%d,framerate=%d/1", media, w, h, cfg.Fps)
}

// withLinks inserts the "!" separators gst-launch needs between elements while
// leaving each element's properties attached to it. The callers above already
// place "!" where an element boundary falls; this just filters out the empty
// segments a candidate with no converter would leave behind.
func withLinks(args []string) []string {
	out := make([]string, 0, len(args))
	for i, a := range args {
		if a != "!" {
			out = append(out, a)
			continue
		}
		// Drop a "!" that would start the pipeline or double up (a candidate
		// whose convert() returned nothing).
		if len(out) == 0 || out[len(out)-1] == "!" {
			continue
		}
		// Drop a trailing "!" with nothing after it.
		if i == len(args)-1 {
			continue
		}
		out = append(out, a)
	}
	return out
}
