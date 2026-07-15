package engine

// Stats is what the engine can honestly see, mirroring the browser's
// BroadcastStats where the two overlap so a native diagnostics dump can be
// read next to a browser one (R9, docs/13).
//
// Decision 20 governs the gaps: report what we can see, mark what we cannot,
// fabricate nothing. The GStreamer child owns capture, so pre-encode capture
// fps is genuinely unavailable here — CaptureFpsAvailable is false and the
// shells print "n/a". Deriving it from the *requested* rate would be a
// plausible-looking number that answers the exact question ("is the source
// keeping up?") wrongly, which is worse than a gap.
type Stats struct {
	// Encoder is the cascade candidate actually in use ("vulkanh264enc",
	// "nvh264enc", "vah264enc"), or "" before one is chosen.
	Encoder string
	// Codec is the SPS-derived codec string sent to viewers ("avc1.42E02A").
	Codec string
	// Width/Height/Fps/BitrateBps are the session's fixed rung (Decision 9).
	Width, Height int
	Fps           int
	BitrateBps    int

	// CaptureFpsAvailable is always false: see Decision 20 above. The field
	// exists so the gap is explicit in the JSON rather than an absent key a
	// reader might mistake for zero.
	CaptureFpsAvailable bool

	// EncodedFrames counts access units demuxed from the child.
	EncodedFrames uint64
	Keyframes     uint64
	// SentFrames counts frames whose bytes reached the transport without
	// error — R9's funnel stage 4, "actually sent".
	SentFrames uint64
	// EncoderFps is the rate access units leave the child, and SentFps the
	// rate they actually leave the machine. Both are windowed over the stats
	// interval (the browser computes them the same way). A gap between them
	// localizes the bottleneck to the uplink rather than the encoder.
	EncoderFps float64
	SentFps    float64

	DatagramsSent uint64
	BytesSent     uint64
	ConfigsSent   uint64

	// Keyframe uni streams (R8 discipline, applied at the source by
	// Decision 12).
	KeyframeStreamsSent       uint64
	KeyframeStreamsFailed     uint64
	KeyframeStreamsSuperseded uint64
	KeyframeBytesSent         uint64

	// FramesDroppedAtSend counts frames abandoned mid-chunking after a
	// datagram send failure (Decision 12): the remainder is dead weight the
	// viewer's reassembler would discard anyway.
	FramesDroppedAtSend uint64

	// TimeSyncRttMs is the self-owned broadcaster↔relay RTT, and
	// TimeSyncOffsetUs the relay-clock offset published as ClockMapping.
	// Available is false until the first pong lands.
	TimeSyncAvailable bool
	TimeSyncRttMs     float64
	TimeSyncOffsetUs  int64
}
