package engine

// System audio for the native broadcaster (R25, docs/28).
//
// R15 built this feature and only the *browser* got a producer: wire types
// 0x07/0x08 live in the shared gawk-server/wire package, and the relay's
// dispatch, config cache and join-prime — plus the entire viewer path — are
// engine-agnostic and already shipped. So everything here is a producer, and
// the deliberate absence of anything else is the point: no wire change, no
// relay change, no viewer change.

// The audio format is a set of constants, not settings (docs/28 Decision 3,
// following docs/20's "bitrate is a named constant, not a setting, in v1").
// They mirror gawk-app/src/media/audio-lane.ts so both broadcasters describe
// the same stream to the same viewer.
const (
	// AudioCodec is the WebCodecs codec string the viewer configures from.
	AudioCodec = "opus"
	// AudioSampleRate and AudioChannels are forced in the capture caps rather
	// than accepted from the source: a 5.1 monitor's 6-channel Opus would be
	// channel-mapping family 1 (multistream), which WebCodecs cannot decode
	// without an OpusHead description — and docs/20 sends an empty description
	// by design.
	AudioSampleRate = 48000
	AudioChannels   = 2
	// AudioBitrateBps matches the browser lane's AUDIO_BITRATE_BPS.
	AudioBitrateBps = 128_000
	// AudioFrameMs is the Opus frame duration. 20 ms ≈ 320 B at 128 kbps,
	// which is what makes "one Opus packet per datagram, never chunked" true.
	AudioFrameMs = 20
	// AudioConfigResendMs mirrors the browser's AUDIO_CONFIG_RESEND_MS: audio
	// has no keyframe to anchor config re-emits to, so the config rides the
	// packet flow at 1 Hz (docs/20 Decision 5). 50 packets/s is already a
	// scheduler; there is no separate timer.
	AudioConfigResendMs = 1000
)

// AudioState mirrors the browser's BroadcastStats.audioState vocabulary
// (gawk-app/src/transport/broadcaster.ts) so a native diagnostics dump and a
// browser one mean the same thing side by side. The native lane can reach four
// of its six values; 'no-track' and 'unsupported' are browser-picker concepts
// with no analogue here (docs/28 Decision 1: the share picker stays
// video-only).
type AudioState string

const (
	// AudioOff: the broadcaster asked for silence (-audio=false).
	AudioOff AudioState = "off"
	// AudioUnavailable: audio was wanted and no source could produce it.
	// Never an error — the broadcast publishes video and says so (Decision 6).
	AudioUnavailable AudioState = "unavailable"
	// AudioActive: packets are flowing.
	AudioActive AudioState = "active"
	// AudioError: the lane produced something we refuse to ship — today only
	// a bitstream that disagrees with the advertised config (Decision 10).
	AudioError AudioState = "error"
)

// AudioPacket is one encoded Opus packet, already compressed, as it comes off
// the child's pipe — the audio twin of AccessUnit, and just as opaque.
type AudioPacket struct {
	// Data is one Opus packet, valid until the next receive.
	Data []byte
	// TimestampUs is the packet's presentation time on the engine's Clock —
	// the same clock video access units are stamped on, mapped by the *same*
	// ptsAnchor instance (Decision 5). That shared anchor is the whole A/V
	// sync design: one PTS timeline through one affine function makes the
	// relative skew zero by construction. Giving audio its own anchor would
	// look tidier and would silently reintroduce a constant lip-sync bias the
	// viewer can neither detect nor remove.
	TimestampUs uint64
}

// AudioFormat describes the stream the lane is producing. It is what the
// AudioConfig datagram is built from, so it must describe the bitstream rather
// than the request — see the TOC check in sender.sendAudio (Decision 10).
type AudioFormat struct {
	// Codec is the WebCodecs codec string ("opus").
	Codec string
	// SampleRate in Hz (48000) and Channels (2), as forced in the caps.
	SampleRate int
	Channels   int
	// BitrateBps is the encoder's target. Reported for stats only; it is not
	// on the wire (AudioConfig carries codec, rate, channels, description).
	BitrateBps int
	// Source names the winning cascade candidate, for stats and the GUI.
	Source string
}

// DefaultAudioFormat is the format Decision 3's caps filter forces.
func DefaultAudioFormat(source string) AudioFormat {
	return AudioFormat{
		Codec:      AudioCodec,
		SampleRate: AudioSampleRate,
		Channels:   AudioChannels,
		BitrateBps: AudioBitrateBps,
		Source:     source,
	}
}

// AudioSource is implemented by media sources that carry audio (Decision 8).
//
// It is deliberately a *separate, optional* interface rather than three more
// methods on MediaSource: widening MediaSource would force audio semantics
// onto every implementation — the test fakes and internal/pubsim — that has
// nothing to say about it, and would erode video-only as a first-class shape.
// That shape is not merely tidy: R20 tier-1 continuously asserts the no-audio
// path, and that assertion is only meaningful while a video-only source is a
// real thing rather than an audio source returning nil.
//
// The engine type-asserts; a source that does not implement this is video-only.
type AudioSource interface {
	// Audio returns the packet channel. It closes when capture ends, like the
	// access-unit channel. Nil when this source has no audio to give.
	Audio() <-chan AudioPacket
	// AudioFormat describes what Audio() will carry. ok is false when no
	// source passed the pre-flight trial, which is the "publish video and say
	// so" path, not an error.
	AudioFormat() (AudioFormat, bool)
}
