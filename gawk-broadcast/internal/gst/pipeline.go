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

// BuildPipeline returns the gst-launch-1.0 arguments for a live capture.
//
// The shape, and why each link is what it is:
//
//	pipewiresrc fd=3 path=N            the portal's granted stream (Decision 5)
//	! videorate drop-only=true         VFR pass-through — see below
//	! <hw convert/scale>               stays on the GPU
//	! <hw encoder>                     the GPU's encode block (Decision 4)
//	! h264parse config-interval=-1     SPS/PPS before every IDR (load-bearing)
//	! mpegtsmux ! fdsink fd=1          one PES = one AU (Decision 7)
//
// Two of those are easy to get subtly wrong:
//
// **videorate drop-only=true** and never a plain CFR conversion. Portal
// capture is damage-driven: nothing arrives while the screen is static. A CFR
// converter holds the last frame waiting for the *next* one — unbounded on a
// still screen — then emits a burst of stale duplicates, which arrival
// stamping would cheerfully timestamp "now". Dropping is the only rate control
// compatible with stamping frames as they arrive (Decision 13).
//
// **h264parse config-interval=-1** puts SPS/PPS in front of every IDR. On this
// path the DecoderConfig extradata is empty (we emit raw Annex-B and never
// build an avcC record), so a late joiner primed with the relay's cached
// keyframe can only decode if the parameter sets are inside the keyframe AU
// itself. Without this, late joiners see nothing and everyone already watching
// is fine — the worst kind of bug.
func BuildPipeline(c Candidate, cfg engine.MediaConfig, nodeID uint32) []string {
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
	args = append(args, "!")
	args = append(args, rateGate(cfg)...)
	args = append(args, "!")
	args = append(args, c.convert(cfg)...)
	args = append(args, "!", scaleCaps(cfg))
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

func scaleCaps(cfg engine.MediaConfig) string {
	// H.264 wants even dimensions; the ladder math elsewhere in the project
	// makes the same guarantee (docs/08).
	w, h := cfg.Width&^1, cfg.Height&^1
	return fmt.Sprintf("video/x-raw,width=%d,height=%d", w, h)
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
