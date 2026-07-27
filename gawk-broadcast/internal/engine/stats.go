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
// Every field carries an explicit JSON tag, and the tags are the browser's
// lowerCamelCase spelling rather than Go's exported names. That is load-bearing
// rather than cosmetic: this struct is serialized directly into an R28
// telemetry batch, and everything downstream — schema.BroadcasterFields, the
// rollup's curated series, diagnose()'s rules, the dip detector, the dashboard
// — matches on the browser's names. Untagged, the native broadcaster reported
// `EncoderFps` where every reader looked for `encoderFps`: stored faithfully,
// readable by nothing. See stats_test.go, which pins the key set.
//
// Fields the two producers genuinely share take the browser's name outright
// (the funnel, the D17 target trio, the audio lane). Fields only this engine
// has keep their own name in the same case (`encoder`, `capturePath`,
// `audioSource`) rather than being forced into a browser field whose values
// mean something else — one column, one vocabulary.
type Stats struct {
	// Encoder is the cascade candidate actually in use ("vulkanh264enc",
	// "nvh264enc", "vah264enc"), or "" before one is chosen.
	Encoder string `json:"encoder,omitempty"`
	// Codec is the SPS-derived codec string sent to viewers ("avc1.42E02A").
	Codec string `json:"codec,omitempty"`
	// Width/Height/Fps/BitrateBps are the session's fixed rung (Decision 9),
	// and they are what R28's D17 target rules read — hence the `target*`
	// names, which are the browser's for the same quantities.
	Width      int `json:"targetWidth,omitempty"`
	Height     int `json:"targetHeight,omitempty"`
	Fps        int `json:"targetFps,omitempty"`
	BitrateBps int `json:"targetBitrateBps,omitempty"`

	// CaptureFpsAvailable is always false: see Decision 20 above. The field
	// exists so the gap is explicit in the JSON rather than an absent key a
	// reader might mistake for zero.
	CaptureFpsAvailable bool `json:"captureFpsAvailable"`

	// CapturePath says how frames cross the pipewiresrc boundary:
	// "zero-copy" (DMA-BUF stays on the GPU) or "system-memory" (one CPU
	// copy per frame); "" before capture starts.
	CapturePath string `json:"capturePath,omitempty"`

	// EncodedFrames counts access units demuxed from the child.
	EncodedFrames uint64 `json:"encodedFrames"`
	Keyframes     uint64 `json:"keyframes"`

	// KeyframeIntervalMs is an EMA of the wall-clock spacing between
	// keyframes leaving the encoder, measured on AU arrival stamps.
	// GOPMs is the *target*; the encoders take a frame count derived from
	// the nominal fps, so damage-driven capture running under that rate
	// stretches the real cadence proportionally — this stat is what makes
	// that visible. Available is false until two keyframes have arrived.
	KeyframeIntervalAvailable bool    `json:"keyframeIntervalAvailable"`
	KeyframeIntervalMs        float64 `json:"keyframeIntervalMs"`

	// ParityLevel is how many R29 forward-parity symbols this producer emits
	// per delta frame (docs/34). It mirrors the browser's
	// BroadcastStats.parityLevel: the level the RELAY advertised, not a local
	// preference — a producer cannot choose to protect a stream the fleet has
	// not asked it to, because the relay is what filters symbols per
	// subscriber. 0 against a relay predating R29 or configured off.
	ParityLevel      int    `json:"parityLevel"`
	ParityChunksSent uint64 `json:"parityChunksSent"`
	ParityBytesSent  uint64 `json:"parityBytesSent"`
	// SentFrames counts frames whose bytes reached the transport without
	// error — R9's funnel stage 4, "actually sent".
	SentFrames uint64 `json:"sentFrames"`
	// EncoderFps is the rate access units leave the child, and SentFps the
	// rate they actually leave the machine. Both are windowed over the stats
	// interval (the browser computes them the same way). A gap between them
	// localizes the bottleneck to the uplink rather than the encoder.
	EncoderFps float64 `json:"encoderFps"`
	SentFps    float64 `json:"sentFps"`

	DatagramsSent uint64 `json:"datagramsSent"`
	BytesSent     uint64 `json:"bytesSent"`
	ConfigsSent   uint64 `json:"configsSent"`

	// Keyframe uni streams (R8 discipline, applied at the source by
	// Decision 12).
	KeyframeStreamsSent       uint64 `json:"keyframeStreamsSent"`
	KeyframeStreamsFailed     uint64 `json:"keyframeStreamsFailed"`
	KeyframeStreamsSuperseded uint64 `json:"keyframeStreamsSuperseded"`
	KeyframeBytesSent         uint64 `json:"keyframeBytesSent"`

	// FramesDroppedAtSend counts frames abandoned mid-chunking after a
	// datagram send failure (Decision 12): the remainder is dead weight the
	// viewer's reassembler would discard anyway.
	FramesDroppedAtSend uint64 `json:"framesDroppedAtSend"`

	// TimeSyncRttMs is the self-owned broadcaster↔relay RTT, and
	// TimeSyncOffsetUs the relay-clock offset published as ClockMapping.
	// Available is false until the first pong lands.
	TimeSyncAvailable bool    `json:"timeSyncAvailable"`
	TimeSyncRttMs     float64 `json:"timeSyncRttMs"`
	TimeSyncOffsetUs  int64   `json:"timeSyncOffsetUs"`

	// ViewerCount is the live "N watching" number the relay pushes (R18,
	// docs/23 — fleet-global in cluster mode). Available is false until the
	// first push lands (an old relay never sends one).
	ViewerCountAvailable bool   `json:"viewerCountAvailable"`
	ViewerCount          uint32 `json:"viewerCount"`

	// Resumes counts how many times auto-resume has reclaimed this broadcast
	// on a fresh relay session, and Resuming says whether a reclaim is in
	// flight right now (capture and encode still running, nothing reaching
	// viewers). A broadcast that quietly resumes every few minutes is a
	// working broadcast on a failing path, and only this counter says so.
	Resumes  uint64 `json:"resumes"`
	Resuming bool   `json:"resuming"`

	// System audio (R25, docs/28), mirroring the browser lane's audio stats
	// so the two dumps read alike.
	//
	// AudioState is the honest summary: "off" when the broadcaster asked for
	// silence, "unavailable" when audio was wanted and no source could give
	// it, "active" when packets are flowing, "error" when the bitstream
	// disagreed with the config we advertise (Decision 10). A machine with no
	// usable audio source is *not* an error, and this vocabulary says so.
	AudioState AudioState `json:"audioState,omitempty"`
	// AudioSource names the winning cascade candidate ("pipewire-monitor"),
	// which is the first thing to know when audio is present but wrong.
	AudioSource string `json:"audioSource,omitempty"`
	// The format actually advertised to viewers. Empty until a lane starts.
	AudioCodec      string `json:"audioCodec,omitempty"`
	AudioSampleRate int    `json:"audioSampleRate,omitempty"`
	AudioChannels   int    `json:"audioChannels,omitempty"`
	AudioBitrateBps int    `json:"audioBitrateBps,omitempty"`

	AudioPacketsSent uint64 `json:"audioPacketsSent"`
	AudioBytesSent   uint64 `json:"audioBytesSent"`
	AudioConfigsSent uint64 `json:"audioConfigsSent"`
	// AudioPacketsDropped counts packets lost at the send path — oversize, or
	// a datagram the transport refused. Deliberately its own counter: audio
	// never touches FramesDroppedAtSend, so a glance at the two separates a
	// saturated uplink from an audio-only problem.
	AudioPacketsDropped uint64 `json:"audioPacketsDropped"`
}
