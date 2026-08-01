package gst

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Tuhis/gawk/gawk-broadcast/internal/engine"
)

// The audio source cascade (R25, docs/28 Decision 2).
//
// Audio does not come from the portal grant: the XDG ScreenCast interface
// carries no audio and has no flag that would give it any (Decision 1). It
// comes from PipeWire directly, capturing the **monitor of the default sink**
// — what is coming out of the speakers. PipeWire is guaranteed present,
// because the portal handshake this app already performs requires it.
//
// Same instinct as the encoder cascade: a list of candidates, each accepted
// only by a real trial, last-good cached and re-verified rather than trusted.

// ErrNoAudioSource is returned when no candidate produces audio on this
// machine.
//
// It is emphatically *not* a start failure. Audio is subordinate (Decision 6):
// the caller logs it, reports AudioState "unavailable", and publishes video.
// A machine with no usable audio source broadcasts exactly as it does today.
var ErrNoAudioSource = errors.New("no working system-audio source")

// AudioCandidate is one way of capturing system audio.
type AudioCandidate struct {
	// Name is the stable id: persisted as LastGoodAudioSource, reported in
	// stats and the GUI, matched by FindAudioCandidate. It is deliberately not
	// the element name — two candidates drive the same element.
	Name string
	// Element is the GStreamer element this candidate drives. Used for the
	// stderr attribution bucket and for the "install this package" message.
	Element string
	// Package names the distro package that provides the element, so a
	// missing one produces a sentence rather than a silent video-only start.
	Package string
	// Description is the one-line explanation a log line or the GUI shows.
	Description string
	// src returns the source element plus its properties, given the
	// configured device (used only by the explicit-device candidate).
	src func(device string) []string
}

// audioCascade is the probed order.
//
// pipewiresrc leads for one reason, and it is the single most valuable
// property in the list: WirePlumber routes `stream.capture.sink=true` to the
// default sink's monitor and *follows* that default, so a headphone/speaker
// switch re-routes the stream instead of erroring it. NA1 run 2 measured
// exactly that — a 28 s capture stayed audible across a mid-recording device
// switch (docs/28 finding 9), which is what narrows Decision 6's mid-session
// hole.
var audioCascade = []AudioCandidate{
	{
		Name:        "pipewire-monitor",
		Element:     "pipewiresrc",
		Package:     "gstreamer1.0-pipewire",
		Description: "PipeWire capture of the default output's monitor (follows a device switch)",
		src: func(string) []string {
			// The caps filter is not decoration: pipewiresrc negotiates video
			// as happily as audio, and this is what pins which. NA1 verified
			// this exact spelling, and that the property survives into the
			// stream properties WirePlumber routes on (docs/28 finding 7).
			return []string{
				"pipewiresrc", "stream-properties=props,stream.capture.sink=true",
				"!", "audio/x-raw",
			}
		},
	},
	{
		Name:        "pulse-default-monitor",
		Element:     "pulsesrc",
		Package:     "gstreamer1.0-pulseaudio",
		Description: "PulseAudio/pipewire-pulse capture of the default monitor (bound at start)",
		src: func(string) []string {
			// The pipewire-pulse compatibility path, for stacks where
			// candidate 1 does not resolve. It binds to the monitor at start
			// and does not follow a later default change.
			return []string{"pulsesrc", "device=@DEFAULT_MONITOR@"}
		},
	},
}

// audioDeviceCandidate is the escape hatch for a machine whose "system audio"
// is a specific device the broadcaster names.
var audioDeviceCandidate = AudioCandidate{
	Name:        "pulse-device",
	Element:     "pulsesrc",
	Package:     "gstreamer1.0-pulseaudio",
	Description: "PulseAudio capture of an explicitly named device",
	src: func(device string) []string {
		return []string{"pulsesrc", fmt.Sprintf("device=%s", device)}
	},
}

// AppAudioSourceName is the candidate name single-application capture reports
// (stats, the GUI's audio line, the R28 report). It is deliberately not in the
// cascade: this source is created per broadcast around a sink serial that only
// exists for the life of that broadcast, so it can never be a cached last-good
// answer the way the system candidates are.
const AppAudioSourceName = "app-sink-monitor"

// AppAudioCandidate captures the monitor of the helper-owned virtual sink
// (R35, docs/39 D3).
//
// Everything downstream of the source element is the R25 branch, unchanged and
// byte-for-byte: the whole point of the virtual-sink shape is that GStreamer
// never learns any of this happened. The pipeline captures **one static node
// forever** and all the dynamism — which application, streams dying and
// reappearing, re-links — lives outside it in PipeWire link management.
//
// The node is addressed by `object.serial`, not by name: serials are unique for
// the daemon's lifetime, so nothing can race us to the name and win.
func AppAudioCandidate(sinkSerial uint32) AudioCandidate {
	return AudioCandidate{
		Name:        AppAudioSourceName,
		Element:     "pipewiresrc",
		Package:     "gstreamer1.0-pipewire",
		Description: "PipeWire capture of one application's audio, through a private sink",
		src: func(string) []string {
			return []string{
				"pipewiresrc",
				fmt.Sprintf("target-object=%d", sinkSerial),
				// The same caps pin and the same capture-sink property as the
				// system candidate: this is a sink monitor too, and pipewiresrc
				// negotiates video as happily as audio without the filter.
				"stream-properties=props,stream.capture.sink=true",
				"!", "audio/x-raw",
			}
		},
	}
}

// AudioCandidates returns the cascade to probe.
//
// An explicit device pins exactly one candidate — the same rule as the encoder
// override, for the same reason: capturing something other than what the user
// named would be worse than capturing nothing. Unlike the encoder override it
// is still trialled, because audio is subordinate: a device name that does not
// resolve must degrade to video-only, never fail the broadcast.
func AudioCandidates(device string) []AudioCandidate {
	if device != "" {
		return []AudioCandidate{audioDeviceCandidate}
	}
	return audioCascade
}

// FindAudioCandidate returns the candidate with the given name.
func FindAudioCandidate(name string) (AudioCandidate, bool) {
	if name == audioDeviceCandidate.Name {
		return audioDeviceCandidate, true
	}
	for _, c := range audioCascade {
		if c.Name == name {
			return c, true
		}
	}
	return AudioCandidate{}, false
}

// AudioTrialFunc runs a trial capture+encode of one candidate and reports
// whether it actually produces Opus on this machine.
//
// Injectable for the same reason TrialFunc is: the cascade's decision logic
// should be a unit test, not a sound card.
type AudioTrialFunc func(ctx context.Context, c AudioCandidate, device string) error

// SelectAudioSource picks the audio source to use, or ErrNoAudioSource.
//
// The rules mirror SelectEncoder, with one difference that matters: an
// explicit device does not skip the trial (see AudioCandidates).
func SelectAudioSource(ctx context.Context, device, lastGood string, trial AudioTrialFunc) (AudioCandidate, error) {
	order := AudioCandidates(device)
	if lastGood != "" && device == "" {
		if c, ok := FindAudioCandidate(lastGood); ok && c.Name != audioDeviceCandidate.Name {
			if err := trial(ctx, c, device); err == nil {
				return c, nil
			}
			// The cached answer went stale (a sound server restart, a stack
			// change). Fall through to the full cascade rather than refusing.
		}
	}

	var failures []string
	for _, c := range order {
		if err := trial(ctx, c, device); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", c.Name, err))
			continue
		}
		return c, nil
	}
	if len(failures) == 0 {
		return AudioCandidate{}, ErrNoAudioSource
	}
	return AudioCandidate{}, fmt.Errorf("%w\n  tried: %s", ErrNoAudioSource, strings.Join(failures, "\n         "))
}

// audioBranch returns the elements between the source and the muxer.
//
// Every property is load-bearing; none is decoration (Decision 3):
//
//   - **rate/channels forced.** A 5.1 output sink has a 6-channel monitor, and
//     opusenc would answer that with channel-mapping family 1 (multistream
//     Opus). WebCodecs cannot decode that without an OpusHead description, and
//     docs/20 sends an empty description by design. The downmix in
//     audioconvert is what keeps the viewer's decoder configuration honest;
//     audioresample covers a 44.1 kHz monitor.
//   - **dtx=false.** docs/20 Decision 1 makes a constant packet rate
//     load-bearing: the viewer's gap detection and buffer clock assume 50
//     packets per second, and DTX would make silence look like loss.
//   - **frame-size=20.** 20 ms ≈ 320 B at 128 kbps — the "one Opus packet per
//     datagram, no chunking, no reassembly" property the R15 wire design rests
//     on.
//   - **inband-fec=false.** The viewer has no FEC hook (docs/20 Decision 8:
//     WebCodecs exposes none), so redundancy would spend bitrate nothing can
//     read.
//   - **audio-type=restricted-lowdelay.** Drops libopus's ~6.5 ms encoder
//     lookahead. The one property here that is a judgement rather than a
//     constraint, flagged in NA8's verification so it gets listened to.
//   - **queue on both sides.** Decouples the audio source's thread from the
//     muxer's, so audio scheduling jitter does not reach the video path. Small
//     and non-leaky: the drop policy lives in Go, where it can be counted.
func audioBranch() []string {
	return []string{
		"queue",
		"!", "audioconvert",
		"!", "audioresample",
		"!", audioCaps(),
		"!", "opusenc",
		fmt.Sprintf("bitrate=%d", engine.AudioBitrateBps),
		fmt.Sprintf("frame-size=%d", engine.AudioFrameMs),
		"dtx=false",
		"inband-fec=false",
		"audio-type=restricted-lowdelay",
		"!", "queue",
	}
}

// audioCaps is the format the viewer is promised. The engine's constants are
// the single definition (CODE-REVIEW.md); this only spells them for gst.
func audioCaps() string {
	return fmt.Sprintf("audio/x-raw,rate=%d,channels=%d", engine.AudioSampleRate, engine.AudioChannels)
}

// BuildAudioTrialPipeline returns the arguments for a trial audio capture.
//
// An audio trial is cheap and unobtrusive in a way the video trial is not: it
// opens no picker, needs no permission and touches no GPU. That is why it runs
// *before* the portal handshake, alongside EnsureBinary — the same ordering
// rule that keeps a machine without GStreamer from being asked to share its
// screen first. A broadcaster learns "no audio on this machine" before they
// pick a window, not after.
//
// 25 buffers ≈ 500 ms: enough to prove the source produces samples and that
// opusenc accepts them, short enough not to be felt at startup. The caller
// bounds it with a context timeout as well, because a source that opens and
// then never produces is exactly as broken as one that fails to open, and must
// not hang startup.
func BuildAudioTrialPipeline(c AudioCandidate, device string) []string {
	args := []string{"-q"}
	args = append(args, c.src(device)...)
	args = append(args, "!", "audioconvert", "!", "audioresample")
	args = append(args, "!", audioCaps())
	args = append(args, "!", "opusenc", "!", "fakesink", fmt.Sprintf("num-buffers=%d", audioTrialBuffers))
	return withLinks(args)
}

const audioTrialBuffers = 25

// audioElementNames returns the element names that appear **only** in the
// audio branch, for the stderr attribution bucket (Decision 7).
//
// pipewiresrc is deliberately excluded even when it is the audio candidate:
// the *video* capture is a pipewiresrc too, so matching it would attribute a
// compositor negotiation failure to the sound card — the precise misdiagnosis
// ErrCaptureFormat exists to prevent. The cost of that exclusion is bounded:
// an audio pipewiresrc dying live falls through to the one clean audio-off
// re-run (Decision 6), which costs a cascade pass and still ends up
// broadcasting video.
func audioElementNames(c AudioCandidate) []string {
	names := []string{"opusenc", "audioconvert", "audioresample"}
	if c.Element != "" && c.Element != "pipewiresrc" {
		names = append(names, c.Element)
	}
	return names
}

// failureNamesAudioElement reports whether a child's dying words implicate the
// audio branch. Matched on the element names the pipeline builder itself chose
// — never on free-text guessing.
func failureNamesAudioElement(failure string, c AudioCandidate) bool {
	lower := strings.ToLower(failure)
	for _, name := range audioElementNames(c) {
		if strings.Contains(lower, name) {
			return true
		}
	}
	return false
}
