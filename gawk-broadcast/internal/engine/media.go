package engine

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
)

// MediaConfig is the session's fixed rung (Decision 9: no ladder, no
// auto-fallback, no mid-session changes in v1).
//
// The defaults are 1080p60 with a 500 ms GOP. 60 fps tracks R13's
// framerate-first rule rather than build-order item 11's 30 fps fan-out
// default: item 11 capped at 30 because a browser broadcaster might be
// software-encoding, and this engine is hardware-encode by construction
// (Decision 4) — so 1080p60 is coherent here rather than aspirational.
//
// R4's FallbackController is deliberately not ported. Its trigger is
// encodeQueueSize growth, and R4's own hardware finding (2026-07-13) was that
// hardware encoders drain frames without that signal ever firing — porting it
// would carry the complexity and faithfully reproduce the under-fire.
type MediaConfig struct {
	Width, Height int
	Fps           int
	BitrateBps    int
	// GOPMs is the keyframe interval in milliseconds (item 11's
	// keyframeIntervalMs: 500 ms so a lost frame self-heals in ≤0.5 s).
	GOPMs int
	// Encoder, when set, forces one cascade candidate and skips probing.
	Encoder string

	// DisableAudio turns the system-audio lane off entirely (R25, docs/28
	// Decision 11). The zero value means *on*, deliberately: a broadcaster who
	// wanted silence would not have installed a screen-sharing app, and
	// Decision 6 makes "on" safe on machines where audio cannot work. This
	// flag is for the broadcaster who genuinely wants silence, not a
	// workaround for breakage — breakage needs no user involvement.
	DisableAudio bool
	// AudioDevice pins one capture device by name (pulsesrc's device
	// property). Empty probes the cascade; set, it is the only candidate
	// tried — the same rule as Encoder, and for the same reason: silently
	// capturing something other than what the user named would be worse than
	// failing.
	AudioDevice string
}

// DefaultMediaConfig is the shipped rung.
//
// 16 Mbps: 1080p60 game content over H.264 with a 500 ms GOP spends a third
// of its budget on keyframes, and the field test (2026-07-17) put the
// default-8 picture at "garbage" while 24 was clean — 16 is the comfortable
// middle that still leaves 15 viewers at ~240 Mbps on the homelab's 1 Gbps
// uplink. Local-streaming peers land in the same range (Sunshine defaults to
// 20). The GUI and -bitrate override it per broadcast.
func DefaultMediaConfig() MediaConfig {
	return MediaConfig{
		Width:      1920,
		Height:     1080,
		Fps:        60,
		BitrateBps: 16_000_000,
		GOPMs:      500,
	}
}

// ParseResolution parses a user-facing "WIDTHxHEIGHT" string. Shared by the
// CLI flag and the GUI settings field so the two shells cannot drift on what
// "1920x1080" means.
func ParseResolution(s string) (int, int, error) {
	parts := strings.SplitN(strings.ToLower(s), "x", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("bad resolution %q: want WIDTHxHEIGHT, e.g. 1920x1080", s)
	}
	w, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil || w <= 0 {
		return 0, 0, fmt.Errorf("bad resolution width in %q", s)
	}
	h, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil || h <= 0 {
		return 0, 0, fmt.Errorf("bad resolution height in %q", s)
	}
	return w, h, nil
}

// AccessUnit is one encoded frame, already compressed, as it comes off the
// child's pipe.
//
// The engine never sees a pixel: by the time anything reaches this struct it
// is H.264, which is why the whole app can be a byte pump. Data is raw
// Annex-B, and stays that way — the viewer's isAnnexB start-code sniff routes
// it into the Annex-B branch, so the engine never builds an avcC record and
// the DecoderConfig extradata is empty. That designs out the nastiest interop
// risk in the whole idea (docs/19).
type AccessUnit struct {
	// Data is the access unit's Annex-B bytes, valid until the next receive.
	Data []byte
	// Keyframe is true when the AU contains an IDR slice (NAL type 5). Thanks
	// to h264parse config-interval=-1 such an AU also carries its SPS/PPS
	// in-band, which is what makes it self-sufficient for the relay's
	// cached-keyframe priming of late joiners.
	Keyframe bool
	// TimestampUs is the AU's presentation time on the engine's Clock — the
	// same clock TimeSync reads (Decision 6). Since 2026-07-17 it is the PES
	// PTS mapped onto that clock by ptsAnchor (internal/gst/pts.go), so stamps
	// carry the capture cadence instead of the pipe's delivery pattern; it
	// falls back to the pipe-arrival time only when the container carried no
	// PTS. See docs/19 deviation 11.
	TimestampUs uint64
	// PTSUs is the raw PES presentation timestamp, if the container carried
	// one. Carried for diagnostics only — the value actually shipped is the
	// clock-anchored TimestampUs above (ptsAnchor consumes the PTS directly);
	// nothing downstream reads this field.
	PTSUs  uint64
	HasPTS bool
}

// MediaSourceFactory builds a MediaSource. It exists so the engine can hand
// the source *its own* Clock rather than trusting the source to pick a
// compatible one: Decision 6's whole correctness argument is that frame stamps
// and TimeSync t0 read one clock, and a factory makes that structural instead
// of a convention someone can forget. (It mirrors the browser's
// BroadcastMediaSourceFactory seam, added for R11.)
type MediaSourceFactory func(cfg MediaConfig, clock Clock, log *slog.Logger) (MediaSource, error)

// MediaSource produces access units. It is the seam between "bytes on a pipe"
// and "frames on the wire": everything above it (the session, the send policy)
// is testable without a GPU, a portal or a child process.
type MediaSource interface {
	// Start begins capture and returns the AU channel. The channel closes
	// when capture ends for any reason; Err then says why.
	Start(ctx context.Context) (<-chan AccessUnit, error)
	// Stop ends capture and reaps the child.
	Stop() error
	// Encoder names the cascade candidate in use, or "" before Start.
	Encoder() string
	// CapturePath says how frames cross the capture boundary ("zero-copy",
	// "system-memory"), or "" before capture starts. Stats-only — nothing
	// branches on it; it exists so a broadcaster can see which rung of the
	// capture ladder actually won without reading child logs.
	CapturePath() string
	// Err returns the failure that ended capture, or nil for a clean stop.
	Err() error
}

// GeometrySource is implemented by media sources whose encode dimensions are
// not simply the configured ones (R35, docs/39 D2).
//
// Optional for the same reason AudioSource is: the fakes and internal/pubsim
// have nothing to say about geometry, and widening MediaSource would make them
// answer a question they cannot. A source that does not implement it reports
// the configured rung, which is what every source did before R35.
type GeometrySource interface {
	// EncodeSize is the width and height actually asked of the encoder — the
	// configured box fitted to the source's aspect. ok is false before the
	// source knows (i.e. before the portal has answered).
	EncodeSize() (width, height int, ok bool)
}

// ShareModeSource is implemented by media sources that know whether the
// broadcaster shared a screen or a single window (R35, docs/39 D1). Stats-only,
// like CapturePath: nothing in the engine branches on it.
type ShareModeSource interface {
	// ShareMode is "screen", "window", or "" before the picker has answered.
	ShareMode() string
}
