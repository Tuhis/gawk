package engine

import (
	"context"
	"log/slog"
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
}

// DefaultMediaConfig is the shipped rung.
func DefaultMediaConfig() MediaConfig {
	return MediaConfig{
		Width:      1920,
		Height:     1080,
		Fps:        60,
		BitrateBps: 8_000_000,
		GOPMs:      500,
	}
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
	// TimestampUs is when the AU was fully read from the pipe, on the
	// engine's Clock — the same clock TimeSync reads (Decision 6).
	TimestampUs uint64
	// PTSUs is the PES presentation timestamp, if the container carried one.
	// Unused today: it is kept because Decision 6's upgrade path (if V4's
	// 15 ms bias gate fails) is clock-anchored PTS, and this is the half of
	// it that comes free with the framing.
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
	// Err returns the failure that ended capture, or nil for a clean stop.
	Err() error
}
