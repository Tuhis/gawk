// Package wire implements the frozen datagram wire format shared by the
// relay server, the broadcasters, and the viewers.
//
// This package is deliberately public (R14 Decision 1, docs/19): the native
// Linux broadcaster lives in its own top-level module (gawk-broadcast/) and
// imports it, because a second hand-written implementation of the format is
// exactly what the golden vectors below exist to prevent. Go's internal/ rule
// forbids that import from another module, so wire sits outside internal/.
// It may still import internal/broadcastid — the rule restricts importers by
// path prefix, and gawk-server/wire qualifies.
//
// Every datagram starts with a common 2-byte prefix: byte 0 is the protocol
// version (Version) and byte 1 is the message type (TypeVideoChunk or
// TypeDecoderConfig). All multi-byte integers are big-endian, which maps
// directly onto a TypeScript DataView in the frontend mirror of this package.
//
// Message layouts:
//
//	0x01 VideoChunk (VideoChunkHeaderSize = 20 bytes of header, then payload):
//	  byte 2       uint8   flags        bit0 = keyframe, bits 1-7 reserved (0)
//	  byte 3       uint8   reserved (0)
//	  bytes 4-7    uint32  frameID
//	  bytes 8-9    uint16  chunkIndex   0-based
//	  bytes 10-11  uint16  chunkCount   total chunks in the frame (>= 1)
//	  bytes 12-19  uint64  timestampUs
//	  bytes 20+    payload, at most MaxChunkPayload bytes
//
//	0x02 DecoderConfig:
//	  byte 2       uint8   reserved (0)
//	  byte 3       uint8   codecLen
//	  bytes 4..    codecLen bytes of ASCII codec string
//	  rest         extradata (may be empty)
//
// Parse functions never copy: returned payload/extradata slices alias the
// input datagram. Callers that retain parsed data past the lifetime of the
// datagram buffer must copy it themselves.
package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	"github.com/Tuhis/gawk/gawk-server/internal/broadcastid"
)

// Version is the only wire protocol version currently defined. It occupies
// byte 0 of every datagram.
const Version = 0x01

// Message types, occupying byte 1 of every datagram (or, for TypeStreamFrame,
// byte 1 of a unidirectional-stream payload).
const (
	// TypeVideoChunk identifies a VideoChunk datagram.
	TypeVideoChunk = 0x01
	// TypeDecoderConfig identifies a DecoderConfig datagram.
	TypeDecoderConfig = 0x02
	// TypeBroadcastAnnounce identifies a BroadcastAnnounce message.
	TypeBroadcastAnnounce = 0x03
	// TypeStreamFrame identifies a StreamFrame message. Unlike the others it
	// never travels as a datagram — it is the payload of a reliable
	// unidirectional stream carrying exactly one keyframe (R8, docs/12).
	TypeStreamFrame = 0x04
	// TypeTimeSync identifies a TimeSync datagram (R5 Q2, docs/15): a
	// client↔relay clock-sync ping/pong. The client sends it with
	// serverTimeUs = 0; the relay echoes clientTimeUs and fills serverTimeUs
	// from its monotonic clock, giving the client an NTP-style offset + RTT
	// sample against the relay clock.
	TypeTimeSync = 0x05
	// TypeClockMapping identifies a ClockMapping datagram (R5 Q2, docs/15):
	// broadcaster→viewers, relayed and cached by the hub like the keyframe
	// prime. It maps frame timestamps onto the relay clock
	// (relayClockUs = timestampUs + offsetUs) so a viewer holding its own
	// TimeSync offset can compute absolute capture→render latency.
	TypeClockMapping = 0x06

	// TypeAudioFrame identifies an AudioFrame datagram (R15, docs/20
	// Decision 2): broadcaster→viewers, one complete Opus packet per
	// datagram — audio has no keyframes, no chunking, and no reassembly.
	// seq is audio's own uint32 sequence space (independent of video
	// frameIDs, same serial arithmetic); timestampUs is on the broadcaster's
	// performance.now() µs clock, the same clock video capture stamps, so
	// A/V skew is a subtraction.
	TypeAudioFrame = 0x07
	// TypeAudioConfig identifies an AudioConfig datagram (R15, docs/20):
	// broadcaster→viewers, relayed and cached by the hub like ClockMapping
	// (join-primed, invalidated on a new publisher session and on edge
	// upstream loss). Audio has no keyframe to anchor config re-emits to, so
	// the broadcaster re-sends it at 1 Hz; both paths are idempotent on the
	// viewer.
	TypeAudioConfig = 0x08

	// TypeResumeToken identifies a ResumeToken message (R17 W2, docs/22
	// Decision 7): server→publisher on its own unidirectional stream right
	// after the session upgrade (the browser WebTransport API exposes no
	// response headers, so delivery must be in-band). The token is what a
	// publisher presents (as the `resume` query param) to claim /publish/{id}
	// — reclaim of a graced hub AND claim of an ID unknown to the receiving
	// pod, which then creates the hub. Stateless HMAC: any pod can mint and
	// verify. Never sent by clients as a wire message.
	TypeResumeToken = 0x09

	// TypeReliableCarrier identifies a reliable carrier stream (R19, docs/24
	// Decision 3): the stream-kind discriminator byte of a relay→subscriber
	// unidirectional stream that conveys delta datagrams reliably to an
	// opted-in resilient subscriber. Never a datagram. The stream opens with
	// the two-byte prologue Version‖TypeReliableCarrier (a keyframe stream's
	// first two bytes are Version‖TypeStreamFrame, so a bare length prefix
	// would be ambiguous), followed by records of
	// uint16 length (BE) ‖ verbatim datagram bytes — each record a complete,
	// already-golden-vectored datagram the relay would otherwise have sent
	// via SendDatagram. The relay stays a byte forwarder: it never
	// reassembles frames for this path.
	TypeReliableCarrier = 0x0A

	// TypeViewerCount identifies a ViewerCount datagram (R18, docs/23
	// Decision 2): only ever produced by a relay, reused in both directions
	// and disambiguated by which read loop receives it, exactly like
	// TimeSync's ping/pong. Downstream (relay→viewers and relay→publisher)
	// count is the broadcast's global viewer total G; upstream (edge→origin
	// over the internal subscribe session) count is that edge's local
	// external-subscriber count. Clients parse it and never send it — a
	// ViewerCount arriving where a client is the sender is dropped.
	TypeViewerCount = 0x0B

	// TypeDeliveryAck identifies a DeliveryAck datagram (R21, docs/26
	// Decision 7a): relay→viewer, sent once at join, telling the subscriber
	// what it was ACTUALLY served. Delivery is negotiated by query param, and
	// R21's replayed GOPs are byte-identical on the wire to live ones — so
	// without this a viewer cannot tell an honoured request from a downgraded
	// one, or from a relay too old to know the parameter. That gap is the one
	// the 2026-07-22 investigation was lost in (BUGS.md); the R19 truthful
	// "reliable requested / datagrams served" row exists for the same reason
	// and extends naturally from this. Clients parse it and never send it.
	TypeDeliveryAck = 0x0C

	// TypeTelemetryHello identifies a TelemetryHello message (R28, docs/33
	// D2): relay→client, sent once per session on its own reliable
	// unidirectional stream right after the upgrade — the ResumeToken (0x09)
	// precedent, chosen over DeliveryAck's datagram because a lost hello is a
	// session that silently never reports, and DeliveryAck's own re-announce
	// loop exists precisely because one-shot join-time datagrams get lost.
	//
	// It carries the three things a client cannot otherwise know: its
	// telemetry session token (the ingest credential AND the join key that
	// makes the relay's view of a viewer and the viewer's view of itself one
	// dataset), the OBFUSCATED broadcast key (so a client never reports a
	// joinable raw ID — R9 D3), and whether the fleet collects telemetry at
	// all and at what cadence. Never sent on /internal/subscribe: an edge is
	// plumbing, not a client. Clients parse it and never send it — a
	// TelemetryHello arriving where a client is the sender is dropped,
	// matching TypeViewerCount's rule.
	TypeTelemetryHello = 0x0D

	// TypeParityChunk identifies a ParityChunk datagram (R29, docs/34):
	// broadcaster→relay→viewers, carrying one RAID-6 P/Q parity symbol over
	// a delta frame's data chunks so a live-edge viewer can repair chunk
	// loss without the latency of R19's reliable carriers.
	//
	// Deltas only. Keyframes already ride reliable uni streams (R8), so they
	// are not exposed to datagram loss and carry no parity.
	//
	// The relay computes nothing: it forwards a per-subscriber PREFIX of the
	// symbols the producer emitted (parityIndex < subscriber's k), which is
	// what keeps it a byte forwarder and what makes the origin/edge cascade
	// work unchanged (docs/34 §5).
	TypeParityChunk = 0x0E

	// TypeRelayCapabilities identifies a RelayCapabilities message (R29,
	// docs/34 §4.4): relay→client, sent once per session at session start on
	// both routes, telling a producer which optional features this fleet
	// supports and at what level.
	//
	// It exists as its own message rather than as extra fields on
	// BroadcastAnnounce because the parsers are strict (appending bytes to an
	// existing message breaks old readers) and because the browser
	// WebTransport API exposes no HTTP response headers, so a capability
	// cannot ride the connect response. A producer that never sees it emits
	// no parity, which is what makes a new broadcaster against an old relay
	// byte-identical to pre-R29. Clients parse it and never send it.
	TypeRelayCapabilities = 0x0F

	// TypeStripeState identifies a StripeState datagram (R30, docs/35 §5.3):
	// client→relay, the ONE message a striping viewer sends on its primary
	// subscribe session to suppress (or restore) delta datagrams there while
	// stripe legs carry them. Level state re-sent at 1 Hz while striped; the
	// relay expires a stale suppression (StripeStateTTL) so a lost message
	// converges to duplicates, never holes. Accepted only on an external
	// datagram-delivery subscribe session that is not itself a leg —
	// anywhere else it is silently discarded like any unknown datagram.
	TypeStripeState = 0x10
)

// DeliveryMode names what a subscriber is actually being served, as carried
// by DeliveryAck. Values are wire-visible: append, never renumber.
type DeliveryMode uint8

const (
	// DeliveryDatagrams is the default live-edge path: unreliable datagrams.
	// Also what a viewer is told when it asked for more and was refused.
	DeliveryDatagrams DeliveryMode = 0
	// DeliveryReliable is R19 carrier delivery without a DVR ring.
	DeliveryReliable DeliveryMode = 1
	// DeliveryDVR is R21: carrier delivery served from the broadcast's ring
	// at this subscriber's own cursor.
	DeliveryDVR DeliveryMode = 2
)

// DeliveryAckSize is the exact size of a DeliveryAck datagram: version, type,
// mode, then the accepted buffer in ms (big-endian uint16).
const DeliveryAckSize = 5

// CloseCodeBroadcastEnded is the WebTransport application close code sent
// to subscribers when their broadcast is garbage-collected.
const CloseCodeBroadcastEnded = 4000

// CloseCodeSubscriberUnresponsive is sent when the relay evicts a subscriber
// whose keyframe stream opens fail persistently (R10, docs/14 — typically a
// session whose client stopped reading uni streams and exhausted its stream
// credit). Unlike CloseCodeBroadcastEnded it is NOT terminal for a live
// client: the viewer's reconnect policy retries, and a fresh session
// restores the stream credit.
const CloseCodeSubscriberUnresponsive = 4001

// CloseCodeServerDraining is sent to every open session when the relay
// drains on SIGTERM (R17 W1, docs/22 Decision 2): the pod is shutting down
// for a planned rollout while still Ready, so its conntrack entries still
// point at it and the close frame actually reaches the peer. Non-terminal
// and explicitly fast: clients reconnect immediately (0 ms first retry) —
// a ready replacement pod is behind the same Service by construction.
const CloseCodeServerDraining = 4002

// CloseCodeOriginMoved is sent on INTERNAL edge sessions only (R17 W5,
// docs/22 Decision 11): the origin lost its Lease (force-take — the
// broadcaster re-homed) and is demoting itself. The Go edge client
// re-resolves the lease on any close; 4003 exists for log clarity, never
// for browsers.
const CloseCodeOriginMoved = 4003

// CloseCodePublisherSuperseded is sent to a publisher session when a newer
// session claims its broadcast ID with a verified resume token (docs/06
// revision 2026-07-18). The relay cannot tell a silently-dead publisher from
// a live one inside the QUIC idle window, so a token-bearing claim that
// completes its upgrade deposes the incumbent — newest publisher wins, the
// same-pod counterpart of R17 W3's force-take of the origin Lease. In
// practice this lands on the broadcaster's own zombie session; a live client
// receiving it has been replaced and must NOT auto-resume back (terminal for
// resume, like CloseCodeBroadcastEnded is for viewers).
const CloseCodePublisherSuperseded = 4004

// Size constants for the wire format.
const (
	// MaxDatagramSize is the largest datagram we ever produce. It is chosen
	// conservatively below typical QUIC datagram MTU limits.
	MaxDatagramSize = 1200
	// VideoChunkHeaderSize is the fixed size of a VideoChunk header,
	// including the common 2-byte version/type prefix.
	VideoChunkHeaderSize = 20
	// MaxChunkPayload is the largest payload a single VideoChunk may carry.
	MaxChunkPayload = MaxDatagramSize - VideoChunkHeaderSize
	// MaxChunkCount is the maximum number of chunks permitted in a keyframe
	// to prevent memory inflation attacks.
	MaxChunkCount = 3000
	// StreamFrameHeaderSize is the fixed size of a StreamFrame header (R8).
	StreamFrameHeaderSize = 24
	// MaxKeyframeBytes is the absolute ceiling on a single StreamFrame message
	// (header + config + payload). It is the stream analogue of MaxChunkCount:
	// a reader must never allocate a keyframe buffer larger than this from an
	// untrusted length field. The hub's configurable cap defaults to it.
	MaxKeyframeBytes = 8 << 20 // 8 MiB
	// TimeSyncSize is the exact size of a TimeSync datagram (R5 Q2).
	TimeSyncSize = 18
	// ClockMappingSize is the exact size of a ClockMapping datagram (R5 Q2).
	ClockMappingSize = 10
	// ViewerCountSize is the exact size of a ViewerCount datagram (R18).
	ViewerCountSize = 6
	// CarrierPrologueSize is the size of the reliable-carrier stream prologue
	// (Version + TypeReliableCarrier), written once when the stream opens.
	CarrierPrologueSize = 2
	// CarrierRecordHeaderSize is the size of the uint16 length prefix in front
	// of every carrier record.
	CarrierRecordHeaderSize = 2
	// AudioFrameHeaderSize is the fixed size of an AudioFrame header (R15),
	// including the common 2-byte version/type prefix.
	AudioFrameHeaderSize = 16
	// TelemetryHelloSize is the exact size of a TelemetryHello message (R28).
	TelemetryHelloSize = 35
	// TelemetryBroadcastKeySize is the byte length of the obfuscated broadcast
	// key a TelemetryHello carries — the raw digest behind Registry.ObfuscateID,
	// which the client hex-encodes for the ingest envelope.
	TelemetryBroadcastKeySize = 6
	// MaxAudioPayload is the largest payload a single AudioFrame may carry —
	// one Opus packet. 20 ms at 128 kbps is ~320 bytes; anything up to
	// ~470 kbps fits, so a conforming encoder never comes near this.
	MaxAudioPayload = MaxDatagramSize - AudioFrameHeaderSize
)

// flagKeyframe is bit 0 of the VideoChunk flags byte.
const flagKeyframe = 0x01

// Sentinel errors returned (possibly wrapped) by the parse and append
// functions. Check with errors.Is.
var (
	// ErrShortDatagram indicates the datagram is too small to contain the
	// expected header.
	ErrShortDatagram = errors.New("wire: datagram too short")
	// ErrBadVersion indicates byte 0 is not Version.
	ErrBadVersion = errors.New("wire: unsupported version")
	// ErrBadType indicates byte 1 is not the expected message type.
	ErrBadType = errors.New("wire: unexpected message type")
	// ErrBadChunkCount indicates chunkCount == 0 or chunkIndex >= chunkCount.
	ErrBadChunkCount = errors.New("wire: invalid chunk index/count")
	// ErrPayloadTooLarge indicates a VideoChunk payload exceeds MaxChunkPayload.
	ErrPayloadTooLarge = errors.New("wire: payload exceeds MaxChunkPayload")
	// ErrBadCodec indicates a DecoderConfig codec string that is empty,
	// longer than 255 bytes, or (on parse) overruns the datagram.
	ErrBadCodec = errors.New("wire: invalid codec string")
	// ErrDatagramTooLarge indicates an encoded datagram would exceed
	// MaxDatagramSize.
	ErrDatagramTooLarge = errors.New("wire: datagram exceeds MaxDatagramSize")
	// ErrBadBroadcastID indicates a BroadcastAnnounce message with an invalid ID.
	ErrBadBroadcastID = errors.New("wire: invalid broadcast ID")
	// ErrKeyframeTooLarge indicates a StreamFrame whose declared or actual
	// size exceeds MaxKeyframeBytes.
	ErrKeyframeTooLarge = errors.New("wire: keyframe exceeds MaxKeyframeBytes")
	// ErrBadLength indicates a fixed-size message (TimeSync, ClockMapping)
	// whose length is not exactly the expected size.
	ErrBadLength = errors.New("wire: unexpected message length")
	// ErrBadResumeToken indicates a ResumeToken message with an invalid
	// (empty, oversized, or length-mismatched) token.
	ErrBadResumeToken = errors.New("wire: invalid resume token")
	// ErrBadCarrierRecord indicates a carrier record whose declared length is
	// zero or exceeds MaxDatagramSize (R19) — records carry verbatim
	// datagrams, so any other length is a framing corruption.
	ErrBadCarrierRecord = errors.New("wire: invalid carrier record")
	// ErrBadAudioPayload indicates an AudioFrame payload that is empty or
	// exceeds MaxAudioPayload (R15) — a datagram carries exactly one Opus
	// packet, and there is no such thing as a zero-byte one (DTX is off).
	ErrBadAudioPayload = errors.New("wire: invalid audio payload")
	// ErrBadAudioConfig indicates an AudioConfig with a zero sample rate or
	// zero channels (R15).
	ErrBadAudioConfig = errors.New("wire: invalid audio config")
	// ErrBadTelemetryHello indicates a TelemetryHello whose token or broadcast
	// key is the wrong length, or which sets a reserved flag bit (R28).
	ErrBadTelemetryHello = errors.New("wire: invalid telemetry hello")
)

// VideoChunkHeader is the parsed header of a VideoChunk datagram.
type VideoChunkHeader struct {
	// Keyframe is true if this chunk belongs to a keyframe.
	Keyframe bool
	// FrameID identifies the encoded frame this chunk belongs to. It is
	// monotonic per publisher session, starting at 0.
	FrameID uint32
	// ChunkIndex is the 0-based index of this chunk within the frame.
	ChunkIndex uint16
	// ChunkCount is the total number of chunks in the frame. Always >= 1
	// and > ChunkIndex.
	ChunkCount uint16
	// TimestampUs is the frame timestamp in microseconds
	// (EncodedVideoChunk.timestamp).
	TimestampUs uint64
}

// DecoderConfig is the parsed contents of a DecoderConfig datagram.
type DecoderConfig struct {
	// Codec is the WebCodecs codec string, e.g. "avc1.42E02A". Always 1-255
	// bytes on the wire.
	Codec string
	// Extradata is codec-specific configuration (e.g. AVCC bytes for H.264).
	// May be empty (VP8/VP9). When produced by ParseDecoderConfig it aliases
	// the input datagram.
	Extradata []byte
}

// PeekType reports the version and message type bytes of a datagram without
// validating them; parsers perform validation. It returns an error only if
// the datagram is shorter than the 2-byte common prefix.
func PeekType(dgram []byte) (version, msgType uint8, err error) {
	if len(dgram) < 2 {
		return 0, 0, fmt.Errorf("%w: %d bytes, need at least 2", ErrShortDatagram, len(dgram))
	}
	return dgram[0], dgram[1], nil
}

// AppendVideoChunk appends a VideoChunk datagram encoding h and payload to
// dst and returns the extended slice. It returns an error if payload exceeds
// MaxChunkPayload, h.ChunkCount is 0, or h.ChunkIndex >= h.ChunkCount.
// Reserved header bits are written as 0.
func AppendVideoChunk(dst []byte, h VideoChunkHeader, payload []byte) ([]byte, error) {
	if len(payload) > MaxChunkPayload {
		return nil, fmt.Errorf("%w: %d bytes, max %d", ErrPayloadTooLarge, len(payload), MaxChunkPayload)
	}
	if h.ChunkCount == 0 || h.ChunkIndex >= h.ChunkCount {
		return nil, fmt.Errorf("%w: index %d, count %d", ErrBadChunkCount, h.ChunkIndex, h.ChunkCount)
	}
	var flags uint8
	if h.Keyframe {
		flags |= flagKeyframe
	}
	dst = append(dst, Version, TypeVideoChunk, flags, 0)
	dst = binary.BigEndian.AppendUint32(dst, h.FrameID)
	dst = binary.BigEndian.AppendUint16(dst, h.ChunkIndex)
	dst = binary.BigEndian.AppendUint16(dst, h.ChunkCount)
	dst = binary.BigEndian.AppendUint64(dst, h.TimestampUs)
	dst = append(dst, payload...)
	return dst, nil
}

// ParseVideoChunk parses a VideoChunk datagram. The returned payload aliases
// dgram (no copy); it may be empty. It returns an error if the datagram is
// shorter than VideoChunkHeaderSize, has the wrong version or type, has
// chunkCount == 0, or has chunkIndex >= chunkCount.
func ParseVideoChunk(dgram []byte) (h VideoChunkHeader, payload []byte, err error) {
	if len(dgram) < VideoChunkHeaderSize {
		return VideoChunkHeader{}, nil, fmt.Errorf("%w: %d bytes, need at least %d for video chunk",
			ErrShortDatagram, len(dgram), VideoChunkHeaderSize)
	}
	if dgram[0] != Version {
		return VideoChunkHeader{}, nil, fmt.Errorf("%w: 0x%02x", ErrBadVersion, dgram[0])
	}
	if dgram[1] != TypeVideoChunk {
		return VideoChunkHeader{}, nil, fmt.Errorf("%w: got 0x%02x, want video chunk 0x%02x",
			ErrBadType, dgram[1], TypeVideoChunk)
	}
	h = VideoChunkHeader{
		Keyframe:    dgram[2]&flagKeyframe != 0,
		FrameID:     binary.BigEndian.Uint32(dgram[4:8]),
		ChunkIndex:  binary.BigEndian.Uint16(dgram[8:10]),
		ChunkCount:  binary.BigEndian.Uint16(dgram[10:12]),
		TimestampUs: binary.BigEndian.Uint64(dgram[12:20]),
	}
	if h.ChunkCount == 0 || h.ChunkIndex >= h.ChunkCount || h.ChunkCount > MaxChunkCount {
		return VideoChunkHeader{}, nil, fmt.Errorf("%w: index %d, count %d (max %d)", ErrBadChunkCount, h.ChunkIndex, h.ChunkCount, MaxChunkCount)
	}
	return h, dgram[VideoChunkHeaderSize:], nil
}

// AppendDecoderConfig appends a DecoderConfig datagram encoding c to dst and
// returns the extended slice. It returns an error if the codec string is
// empty or longer than 255 bytes, or if the encoded datagram would exceed
// MaxDatagramSize.
func AppendDecoderConfig(dst []byte, c DecoderConfig) ([]byte, error) {
	if len(c.Codec) == 0 {
		return nil, fmt.Errorf("%w: empty", ErrBadCodec)
	}
	if len(c.Codec) > 255 {
		return nil, fmt.Errorf("%w: %d bytes, max 255", ErrBadCodec, len(c.Codec))
	}
	total := 4 + len(c.Codec) + len(c.Extradata)
	if total > MaxDatagramSize {
		return nil, fmt.Errorf("%w: %d bytes, max %d", ErrDatagramTooLarge, total, MaxDatagramSize)
	}
	dst = append(dst, Version, TypeDecoderConfig, 0, uint8(len(c.Codec)))
	dst = append(dst, c.Codec...)
	dst = append(dst, c.Extradata...)
	return dst, nil
}

// ParseDecoderConfig parses a DecoderConfig datagram. The returned Extradata
// aliases dgram (no copy), consistent with ParseVideoChunk. It returns an
// error if the datagram is shorter than 4 bytes, has the wrong version or
// type, or if codecLen overruns the datagram or is zero.
func ParseDecoderConfig(dgram []byte) (DecoderConfig, error) {
	if len(dgram) < 4 {
		return DecoderConfig{}, fmt.Errorf("%w: %d bytes, need at least 4 for decoder config",
			ErrShortDatagram, len(dgram))
	}
	if dgram[0] != Version {
		return DecoderConfig{}, fmt.Errorf("%w: 0x%02x", ErrBadVersion, dgram[0])
	}
	if dgram[1] != TypeDecoderConfig {
		return DecoderConfig{}, fmt.Errorf("%w: got 0x%02x, want decoder config 0x%02x",
			ErrBadType, dgram[1], TypeDecoderConfig)
	}
	codecLen := int(dgram[3])
	if codecLen == 0 {
		return DecoderConfig{}, fmt.Errorf("%w: empty", ErrBadCodec)
	}
	if 4+codecLen > len(dgram) {
		return DecoderConfig{}, fmt.Errorf("%w: codecLen %d overruns %d-byte datagram",
			ErrBadCodec, codecLen, len(dgram))
	}
	return DecoderConfig{
		Codec:     string(dgram[4 : 4+codecLen]),
		Extradata: dgram[4+codecLen:],
	}, nil
}

// AppendBroadcastAnnounce appends a BroadcastAnnounce message encoding ID to dst
// and returns the extended slice.
func AppendBroadcastAnnounce(dst []byte, id string) ([]byte, error) {
	if len(id) == 0 || len(id) > 255 {
		return nil, fmt.Errorf("%w: invalid length %d", ErrBadBroadcastID, len(id))
	}
	dst = append(dst, Version, TypeBroadcastAnnounce, uint8(len(id)))
	dst = append(dst, id...)
	return dst, nil
}

// ParseBroadcastAnnounce parses a BroadcastAnnounce message.
// It returns an error if the message is shorter than 3 bytes, has the wrong version
// or type, or if the ID length is invalid or characters are outside the alphabet.
func ParseBroadcastAnnounce(dgram []byte) (string, error) {
	if len(dgram) < 3 {
		return "", fmt.Errorf("%w: %d bytes, need at least 3 for broadcast announce",
			ErrShortDatagram, len(dgram))
	}
	if dgram[0] != Version {
		return "", fmt.Errorf("%w: 0x%02x", ErrBadVersion, dgram[0])
	}
	if dgram[1] != TypeBroadcastAnnounce {
		return "", fmt.Errorf("%w: got 0x%02x, want broadcast announce 0x%02x",
			ErrBadType, dgram[1], TypeBroadcastAnnounce)
	}
	idLen := int(dgram[2])
	if 3+idLen != len(dgram) {
		return "", fmt.Errorf("%w: expected %d bytes for ID length %d, got %d",
			ErrBadBroadcastID, 3+idLen, idLen, len(dgram))
	}
	id := string(dgram[3:])
	for i := 0; i < len(id); i++ {
		if strings.IndexByte(broadcastid.Alphabet, id[i]) == -1 {
			return "", fmt.Errorf("%w: invalid character %q at index %d", ErrBadBroadcastID, id[i], i)
		}
	}
	return id, nil
}

// AppendTimeSync appends a TimeSync datagram to dst and returns the extended
// slice (R5 Q2). clientTimeUs is the requester's clock at send (echoed back by
// the relay); serverTimeUs is the relay's monotonic clock in replies and 0 in
// requests.
func AppendTimeSync(dst []byte, clientTimeUs, serverTimeUs uint64) []byte {
	dst = append(dst, Version, TypeTimeSync)
	dst = binary.BigEndian.AppendUint64(dst, clientTimeUs)
	dst = binary.BigEndian.AppendUint64(dst, serverTimeUs)
	return dst
}

// ParseTimeSync parses a TimeSync datagram. Strict: the datagram must be
// exactly TimeSyncSize bytes with the right version and type.
func ParseTimeSync(dgram []byte) (clientTimeUs, serverTimeUs uint64, err error) {
	if len(dgram) != TimeSyncSize {
		return 0, 0, fmt.Errorf("%w: %d bytes, want exactly %d for time sync",
			ErrBadLength, len(dgram), TimeSyncSize)
	}
	if dgram[0] != Version {
		return 0, 0, fmt.Errorf("%w: 0x%02x", ErrBadVersion, dgram[0])
	}
	if dgram[1] != TypeTimeSync {
		return 0, 0, fmt.Errorf("%w: got 0x%02x, want time sync 0x%02x",
			ErrBadType, dgram[1], TypeTimeSync)
	}
	return binary.BigEndian.Uint64(dgram[2:10]), binary.BigEndian.Uint64(dgram[10:18]), nil
}

// AppendClockMapping appends a ClockMapping datagram to dst and returns the
// extended slice (R5 Q2). offsetUs is signed:
// relayClockUs = frame timestampUs + offsetUs (two's complement wraparound
// intended on both sides).
func AppendClockMapping(dst []byte, offsetUs int64) []byte {
	dst = append(dst, Version, TypeClockMapping)
	dst = binary.BigEndian.AppendUint64(dst, uint64(offsetUs))
	return dst
}

// ParseClockMapping parses a ClockMapping datagram. Strict: the datagram must
// be exactly ClockMappingSize bytes with the right version and type.
func ParseClockMapping(dgram []byte) (offsetUs int64, err error) {
	if len(dgram) != ClockMappingSize {
		return 0, fmt.Errorf("%w: %d bytes, want exactly %d for clock mapping",
			ErrBadLength, len(dgram), ClockMappingSize)
	}
	if dgram[0] != Version {
		return 0, fmt.Errorf("%w: 0x%02x", ErrBadVersion, dgram[0])
	}
	if dgram[1] != TypeClockMapping {
		return 0, fmt.Errorf("%w: got 0x%02x, want clock mapping 0x%02x",
			ErrBadType, dgram[1], TypeClockMapping)
	}
	return int64(binary.BigEndian.Uint64(dgram[2:10])), nil
}

// AppendViewerCount appends a ViewerCount datagram (R18, docs/23) to dst and
// returns the extended slice. count is uint32 by convention with the
// codebase's frameID/counter integers — deliberate overkill for the audience
// sizes involved, zero doubt about overflow.
func AppendViewerCount(dst []byte, count uint32) []byte {
	dst = append(dst, Version, TypeViewerCount)
	dst = binary.BigEndian.AppendUint32(dst, count)
	return dst
}

// ParseViewerCount parses a ViewerCount datagram. Strict: the datagram must
// be exactly ViewerCountSize bytes with the right version and type.
func ParseViewerCount(dgram []byte) (count uint32, err error) {
	if len(dgram) != ViewerCountSize {
		return 0, fmt.Errorf("%w: %d bytes, want exactly %d for viewer count",
			ErrBadLength, len(dgram), ViewerCountSize)
	}
	if dgram[0] != Version {
		return 0, fmt.Errorf("%w: 0x%02x", ErrBadVersion, dgram[0])
	}
	if dgram[1] != TypeViewerCount {
		return 0, fmt.Errorf("%w: got 0x%02x, want viewer count 0x%02x",
			ErrBadType, dgram[1], TypeViewerCount)
	}
	return binary.BigEndian.Uint32(dgram[2:6]), nil
}

// AppendDeliveryAck appends a DeliveryAck datagram (R21) to dst. bufferMs is
// the buffer the relay accepted after clamping — 0 whenever mode is not
// DeliveryDVR, since no other mode has one.
func AppendDeliveryAck(dst []byte, mode DeliveryMode, bufferMs uint16) []byte {
	dst = append(dst, Version, TypeDeliveryAck, byte(mode))
	dst = binary.BigEndian.AppendUint16(dst, bufferMs)
	return dst
}

// ParseDeliveryAck parses a DeliveryAck datagram. Strict, like every other
// parser here: exact length, right version and type, and a mode this build
// knows. An unknown mode is an error rather than a silent fallback — a viewer
// that cannot name what it was served is the gap this message closes.
func ParseDeliveryAck(dgram []byte) (mode DeliveryMode, bufferMs uint16, err error) {
	if len(dgram) != DeliveryAckSize {
		return 0, 0, fmt.Errorf("%w: %d bytes, want exactly %d for delivery ack",
			ErrBadLength, len(dgram), DeliveryAckSize)
	}
	if dgram[0] != Version {
		return 0, 0, fmt.Errorf("%w: 0x%02x", ErrBadVersion, dgram[0])
	}
	if dgram[1] != TypeDeliveryAck {
		return 0, 0, fmt.Errorf("%w: got 0x%02x, want delivery ack 0x%02x",
			ErrBadType, dgram[1], TypeDeliveryAck)
	}
	mode = DeliveryMode(dgram[2])
	switch mode {
	case DeliveryDatagrams, DeliveryReliable, DeliveryDVR:
	default:
		return 0, 0, fmt.Errorf("%w: unknown delivery mode %d", ErrBadType, dgram[2])
	}
	return mode, binary.BigEndian.Uint16(dgram[3:5]), nil
}

// TelemetryHello is the parsed contents of a TelemetryHello message (R28,
// docs/33 §4.1). Layout, big-endian, exactly TelemetryHelloSize bytes:
//
//	byte 0      Version
//	byte 1      TypeTelemetryHello
//	byte 2      flags — bit 0 Enabled; bits 1-7 reserved, must be 0
//	bytes 3-4   uint16 ReportIntervalMs
//	bytes 5-28  session token (TelemetrySessionTokenSize)
//	bytes 29-34 obfuscated broadcast key (TelemetryBroadcastKeySize)
type TelemetryHello struct {
	// Enabled reports whether this fleet collects telemetry at all. False
	// means the client collects nothing and ignores the remaining fields —
	// which is also, deliberately, exactly what a client does when a relay
	// predating R28 sends no hello at all.
	Enabled bool
	// ReportIntervalMs is the sampling cadence the relay asks clients to use.
	// The relay is the authority so a fleet can turn the volume down without
	// shipping a new frontend.
	ReportIntervalMs uint16
	// Token is the stateless-verifiable session credential. Never logged,
	// never stored: the service verifies it and keeps only the sessionId
	// derived from its nonce.
	Token []byte
	// BroadcastKey is the raw obfuscated-ID digest (Registry.ObfuscateID's
	// pre-hex bytes). Aliases the input message when produced by Parse.
	BroadcastKey []byte
}

// flagTelemetryEnabled is bit 0 of the TelemetryHello flags byte.
const flagTelemetryEnabled = 0x01

// AppendTelemetryHello appends a TelemetryHello message encoding h to dst and
// returns the extended slice. It returns ErrBadTelemetryHello if the token or
// broadcast key is not exactly its fixed size — both are fixed-width fields,
// so a wrong length is a bug on the minting side, never a truncation to paper
// over.
func AppendTelemetryHello(dst []byte, h TelemetryHello) ([]byte, error) {
	if len(h.Token) != TelemetrySessionTokenSize {
		return nil, fmt.Errorf("%w: token %d bytes, want %d", ErrBadTelemetryHello, len(h.Token), TelemetrySessionTokenSize)
	}
	if len(h.BroadcastKey) != TelemetryBroadcastKeySize {
		return nil, fmt.Errorf("%w: broadcast key %d bytes, want %d", ErrBadTelemetryHello, len(h.BroadcastKey), TelemetryBroadcastKeySize)
	}
	var flags uint8
	if h.Enabled {
		flags |= flagTelemetryEnabled
	}
	dst = append(dst, Version, TypeTelemetryHello, flags)
	dst = binary.BigEndian.AppendUint16(dst, h.ReportIntervalMs)
	dst = append(dst, h.Token...)
	dst = append(dst, h.BroadcastKey...)
	return dst, nil
}

// ParseTelemetryHello parses a TelemetryHello message. The returned Token and
// BroadcastKey alias msg (no copy), consistent with every other parser here.
// Strict: exact length, right version and type, and reserved flag bits clear —
// a set reserved bit means a future field this build would misread, so it is
// an error rather than a mask-and-hope.
func ParseTelemetryHello(msg []byte) (TelemetryHello, error) {
	if len(msg) != TelemetryHelloSize {
		return TelemetryHello{}, fmt.Errorf("%w: %d bytes, want exactly %d for telemetry hello",
			ErrBadLength, len(msg), TelemetryHelloSize)
	}
	if msg[0] != Version {
		return TelemetryHello{}, fmt.Errorf("%w: 0x%02x", ErrBadVersion, msg[0])
	}
	if msg[1] != TypeTelemetryHello {
		return TelemetryHello{}, fmt.Errorf("%w: got 0x%02x, want telemetry hello 0x%02x",
			ErrBadType, msg[1], TypeTelemetryHello)
	}
	if msg[2]&^flagTelemetryEnabled != 0 {
		return TelemetryHello{}, fmt.Errorf("%w: reserved flag bits set (0x%02x)", ErrBadTelemetryHello, msg[2])
	}
	return TelemetryHello{
		Enabled:          msg[2]&flagTelemetryEnabled != 0,
		ReportIntervalMs: binary.BigEndian.Uint16(msg[3:5]),
		Token:            msg[5 : 5+TelemetrySessionTokenSize],
		BroadcastKey:     msg[5+TelemetrySessionTokenSize:],
	}, nil
}

// AudioFrameHeader is the parsed header of an AudioFrame datagram (R15).
type AudioFrameHeader struct {
	// Seq is audio's own uint32 sequence space, monotonic per publisher
	// session and independent of video frameIDs. Consumers compare with the
	// same serial arithmetic as frameIDs (wrap-aware).
	Seq uint32
	// TimestampUs is the packet timestamp in microseconds on the
	// broadcaster's performance.now() clock — the same clock video capture
	// stamps, which is what makes A/V skew a subtraction.
	TimestampUs uint64
}

// AudioConfig is the parsed contents of an AudioConfig datagram (R15).
type AudioConfig struct {
	// Codec is the WebCodecs codec string, e.g. "opus". Always 1-255 bytes
	// on the wire.
	Codec string
	// SampleRate is the capture sample rate in Hz (48000 for opus).
	SampleRate uint32
	// Channels is the channel count (2 for the stereo default).
	Channels uint8
	// Description is codec-specific configuration. Empty for opus. When
	// produced by ParseAudioConfig it aliases the input datagram.
	Description []byte
}

// AppendAudioFrame appends an AudioFrame datagram (R15) to dst and returns
// the extended slice. Layout: version, type 0x07, flags (0), reserved (0),
// uint32 seq, uint64 timestampUs, then the payload — exactly one Opus packet.
// It returns ErrBadAudioPayload if the payload is empty or exceeds
// MaxAudioPayload.
func AppendAudioFrame(dst []byte, h AudioFrameHeader, payload []byte) ([]byte, error) {
	if len(payload) == 0 || len(payload) > MaxAudioPayload {
		return nil, fmt.Errorf("%w: %d bytes, want 1-%d", ErrBadAudioPayload, len(payload), MaxAudioPayload)
	}
	dst = append(dst, Version, TypeAudioFrame, 0, 0)
	dst = binary.BigEndian.AppendUint32(dst, h.Seq)
	dst = binary.BigEndian.AppendUint64(dst, h.TimestampUs)
	dst = append(dst, payload...)
	return dst, nil
}

// ParseAudioFrame parses an AudioFrame datagram. The returned payload aliases
// dgram (no copy). Strict: it returns an error if the datagram is shorter
// than AudioFrameHeaderSize, has the wrong version or type, or carries an
// empty payload (there is no zero-byte Opus packet).
func ParseAudioFrame(dgram []byte) (h AudioFrameHeader, payload []byte, err error) {
	if len(dgram) < AudioFrameHeaderSize {
		return AudioFrameHeader{}, nil, fmt.Errorf("%w: %d bytes, need at least %d for audio frame",
			ErrShortDatagram, len(dgram), AudioFrameHeaderSize)
	}
	if dgram[0] != Version {
		return AudioFrameHeader{}, nil, fmt.Errorf("%w: 0x%02x", ErrBadVersion, dgram[0])
	}
	if dgram[1] != TypeAudioFrame {
		return AudioFrameHeader{}, nil, fmt.Errorf("%w: got 0x%02x, want audio frame 0x%02x",
			ErrBadType, dgram[1], TypeAudioFrame)
	}
	if len(dgram) == AudioFrameHeaderSize {
		return AudioFrameHeader{}, nil, fmt.Errorf("%w: empty", ErrBadAudioPayload)
	}
	h = AudioFrameHeader{
		Seq:         binary.BigEndian.Uint32(dgram[4:8]),
		TimestampUs: binary.BigEndian.Uint64(dgram[8:16]),
	}
	return h, dgram[AudioFrameHeaderSize:], nil
}

// AppendAudioConfig appends an AudioConfig datagram (R15) to dst and returns
// the extended slice. Layout: version, type 0x08, reserved (0), uint8
// codecLen, codec bytes, uint32 sampleRate, uint8 channels, then the
// description (may be empty). It returns ErrBadCodec for an empty or oversize
// codec string, ErrBadAudioConfig for a zero sample rate or zero channels,
// and ErrDatagramTooLarge if the encoded datagram would exceed
// MaxDatagramSize.
func AppendAudioConfig(dst []byte, c AudioConfig) ([]byte, error) {
	if len(c.Codec) == 0 {
		return nil, fmt.Errorf("%w: empty", ErrBadCodec)
	}
	if len(c.Codec) > 255 {
		return nil, fmt.Errorf("%w: %d bytes, max 255", ErrBadCodec, len(c.Codec))
	}
	if c.SampleRate == 0 || c.Channels == 0 {
		return nil, fmt.Errorf("%w: sampleRate %d, channels %d", ErrBadAudioConfig, c.SampleRate, c.Channels)
	}
	total := 4 + len(c.Codec) + 5 + len(c.Description)
	if total > MaxDatagramSize {
		return nil, fmt.Errorf("%w: %d bytes, max %d", ErrDatagramTooLarge, total, MaxDatagramSize)
	}
	dst = append(dst, Version, TypeAudioConfig, 0, uint8(len(c.Codec)))
	dst = append(dst, c.Codec...)
	dst = binary.BigEndian.AppendUint32(dst, c.SampleRate)
	dst = append(dst, c.Channels)
	dst = append(dst, c.Description...)
	return dst, nil
}

// ParseAudioConfig parses an AudioConfig datagram. The returned Description
// aliases dgram (no copy). Strict: it returns an error if the datagram is
// shorter than its declared layout, has the wrong version or type, has an
// empty codec string, or declares a zero sample rate or zero channels.
func ParseAudioConfig(dgram []byte) (AudioConfig, error) {
	if len(dgram) < 4 {
		return AudioConfig{}, fmt.Errorf("%w: %d bytes, need at least 4 for audio config",
			ErrShortDatagram, len(dgram))
	}
	if dgram[0] != Version {
		return AudioConfig{}, fmt.Errorf("%w: 0x%02x", ErrBadVersion, dgram[0])
	}
	if dgram[1] != TypeAudioConfig {
		return AudioConfig{}, fmt.Errorf("%w: got 0x%02x, want audio config 0x%02x",
			ErrBadType, dgram[1], TypeAudioConfig)
	}
	codecLen := int(dgram[3])
	if codecLen == 0 {
		return AudioConfig{}, fmt.Errorf("%w: empty", ErrBadCodec)
	}
	if 4+codecLen+5 > len(dgram) {
		return AudioConfig{}, fmt.Errorf("%w: codecLen %d overruns %d-byte datagram",
			ErrBadCodec, codecLen, len(dgram))
	}
	c := AudioConfig{
		Codec:       string(dgram[4 : 4+codecLen]),
		SampleRate:  binary.BigEndian.Uint32(dgram[4+codecLen : 4+codecLen+4]),
		Channels:    dgram[4+codecLen+4],
		Description: dgram[4+codecLen+5:],
	}
	if c.SampleRate == 0 || c.Channels == 0 {
		return AudioConfig{}, fmt.Errorf("%w: sampleRate %d, channels %d", ErrBadAudioConfig, c.SampleRate, c.Channels)
	}
	return c, nil
}

// AppendResumeToken appends a ResumeToken message (R17 W2) to dst and returns
// the extended slice. Layout: version, type 0x09, uint8 tokenLen, then the
// token bytes. The token is opaque on the wire (the server mints truncated
// HMACs, but the format doesn't care).
func AppendResumeToken(dst []byte, token []byte) ([]byte, error) {
	if len(token) == 0 || len(token) > 255 {
		return nil, fmt.Errorf("%w: %d bytes, want 1-255", ErrBadResumeToken, len(token))
	}
	dst = append(dst, Version, TypeResumeToken, uint8(len(token)))
	dst = append(dst, token...)
	return dst, nil
}

// ParseResumeToken parses a ResumeToken message. Strict: the declared token
// length must account for the entire message. The returned slice aliases msg.
func ParseResumeToken(msg []byte) ([]byte, error) {
	if len(msg) < 3 {
		return nil, fmt.Errorf("%w: %d bytes, need at least 3 for resume token", ErrShortDatagram, len(msg))
	}
	if msg[0] != Version {
		return nil, fmt.Errorf("%w: 0x%02x", ErrBadVersion, msg[0])
	}
	if msg[1] != TypeResumeToken {
		return nil, fmt.Errorf("%w: got 0x%02x, want resume token 0x%02x", ErrBadType, msg[1], TypeResumeToken)
	}
	tokenLen := int(msg[2])
	if tokenLen == 0 || 3+tokenLen != len(msg) {
		return nil, fmt.Errorf("%w: declared %d bytes in %d-byte message", ErrBadResumeToken, tokenLen, len(msg))
	}
	return msg[3:], nil
}

// AppendCarrierPrologue appends the two-byte reliable-carrier stream prologue
// (R19): Version, then TypeReliableCarrier. Written exactly once per carrier
// stream, before the first record.
func AppendCarrierPrologue(dst []byte) []byte {
	return append(dst, Version, TypeReliableCarrier)
}

// ParseCarrierPrologue validates the two-byte carrier prologue at the start of
// buf. Like ParseStreamFrameHeader it does not require buf to hold anything
// beyond the prologue — records follow on the stream.
func ParseCarrierPrologue(buf []byte) error {
	if len(buf) < CarrierPrologueSize {
		return fmt.Errorf("%w: %d bytes, need at least %d for carrier prologue",
			ErrShortDatagram, len(buf), CarrierPrologueSize)
	}
	if buf[0] != Version {
		return fmt.Errorf("%w: 0x%02x", ErrBadVersion, buf[0])
	}
	if buf[1] != TypeReliableCarrier {
		return fmt.Errorf("%w: got 0x%02x, want reliable carrier 0x%02x",
			ErrBadType, buf[1], TypeReliableCarrier)
	}
	return nil
}

// AppendCarrierRecord appends one carrier record (uint16 big-endian length,
// then the datagram verbatim) to dst and returns the extended slice. It
// returns ErrBadCarrierRecord for an empty datagram or one exceeding
// MaxDatagramSize — the record framing carries exactly what SendDatagram
// would have.
func AppendCarrierRecord(dst []byte, dgram []byte) ([]byte, error) {
	if len(dgram) == 0 || len(dgram) > MaxDatagramSize {
		return nil, fmt.Errorf("%w: %d bytes, want 1-%d", ErrBadCarrierRecord, len(dgram), MaxDatagramSize)
	}
	dst = binary.BigEndian.AppendUint16(dst, uint16(len(dgram)))
	dst = append(dst, dgram...)
	return dst, nil
}

// ParseCarrierRecord parses one carrier record at the start of buf, returning
// the record's datagram bytes (aliasing buf) and the remainder after it. An
// incomplete record returns ErrShortDatagram (the caller reads more from the
// stream); a zero or oversize declared length returns ErrBadCarrierRecord
// (framing corruption — the caller must abandon the stream).
func ParseCarrierRecord(buf []byte) (record, rest []byte, err error) {
	if len(buf) < CarrierRecordHeaderSize {
		return nil, nil, fmt.Errorf("%w: %d bytes, need at least %d for carrier record header",
			ErrShortDatagram, len(buf), CarrierRecordHeaderSize)
	}
	n := int(binary.BigEndian.Uint16(buf))
	if n == 0 || n > MaxDatagramSize {
		return nil, nil, fmt.Errorf("%w: declared %d bytes, want 1-%d", ErrBadCarrierRecord, n, MaxDatagramSize)
	}
	if len(buf) < CarrierRecordHeaderSize+n {
		return nil, nil, fmt.Errorf("%w: %d bytes, need %d for declared record",
			ErrShortDatagram, len(buf), CarrierRecordHeaderSize+n)
	}
	return buf[CarrierRecordHeaderSize : CarrierRecordHeaderSize+n], buf[CarrierRecordHeaderSize+n:], nil
}

// StreamFrameHeader is the parsed 24-byte header of a StreamFrame message
// (R8, docs/12). A StreamFrame is the entire payload of one unidirectional
// stream: this header, then ConfigLen bytes of an embedded DecoderConfig
// datagram (its 0x01/0x02 prefix included, or empty when ConfigLen == 0),
// then PayloadLen bytes of the raw encoded keyframe.
type StreamFrameHeader struct {
	// Keyframe is true if this stream frame is a keyframe (always true for
	// now; the flag is reserved so future non-keyframe stream frames stay
	// distinguishable).
	Keyframe bool
	// FrameID identifies the encoded frame, monotonic per publisher session,
	// shared with the datagram VideoChunk numbering so the viewer can order
	// keyframes (streams) against deltas (datagrams).
	FrameID uint32
	// TimestampUs is the frame timestamp in microseconds.
	TimestampUs uint64
	// ConfigLen is the byte length of the embedded DecoderConfig datagram
	// that follows the header (0 if none).
	ConfigLen uint32
	// PayloadLen is the byte length of the encoded keyframe that follows the
	// config block.
	PayloadLen uint32
}

// AppendStreamFrameHeader appends the 24-byte StreamFrame header encoding h to
// dst and returns the extended slice. Reserved bits are written as 0. It
// returns ErrKeyframeTooLarge if the declared total (header + config +
// payload) exceeds MaxKeyframeBytes.
func AppendStreamFrameHeader(dst []byte, h StreamFrameHeader) ([]byte, error) {
	if StreamFrameHeaderSize+uint64(h.ConfigLen)+uint64(h.PayloadLen) > MaxKeyframeBytes {
		return nil, fmt.Errorf("%w: header+%d+%d bytes", ErrKeyframeTooLarge, h.ConfigLen, h.PayloadLen)
	}
	var flags uint8
	if h.Keyframe {
		flags |= flagKeyframe
	}
	dst = append(dst, Version, TypeStreamFrame, flags, 0)
	dst = binary.BigEndian.AppendUint32(dst, h.FrameID)
	dst = binary.BigEndian.AppendUint64(dst, h.TimestampUs)
	dst = binary.BigEndian.AppendUint32(dst, h.ConfigLen)
	dst = binary.BigEndian.AppendUint32(dst, h.PayloadLen)
	return dst, nil
}

// ParseStreamFrameHeader parses the 24-byte StreamFrame header at the start of
// buf. It validates version and type and rejects a declared total that exceeds
// MaxKeyframeBytes, but does not require buf to contain the whole message —
// the caller reads ConfigLen + PayloadLen further bytes from the stream. It
// returns an error if buf is shorter than StreamFrameHeaderSize, has the wrong
// version or type, or declares more than MaxKeyframeBytes.
func ParseStreamFrameHeader(buf []byte) (StreamFrameHeader, error) {
	if len(buf) < StreamFrameHeaderSize {
		return StreamFrameHeader{}, fmt.Errorf("%w: %d bytes, need at least %d for stream frame header",
			ErrShortDatagram, len(buf), StreamFrameHeaderSize)
	}
	if buf[0] != Version {
		return StreamFrameHeader{}, fmt.Errorf("%w: 0x%02x", ErrBadVersion, buf[0])
	}
	if buf[1] != TypeStreamFrame {
		return StreamFrameHeader{}, fmt.Errorf("%w: got 0x%02x, want stream frame 0x%02x",
			ErrBadType, buf[1], TypeStreamFrame)
	}
	h := StreamFrameHeader{
		Keyframe:    buf[2]&flagKeyframe != 0,
		FrameID:     binary.BigEndian.Uint32(buf[4:8]),
		TimestampUs: binary.BigEndian.Uint64(buf[8:16]),
		ConfigLen:   binary.BigEndian.Uint32(buf[16:20]),
		PayloadLen:  binary.BigEndian.Uint32(buf[20:24]),
	}
	if StreamFrameHeaderSize+uint64(h.ConfigLen)+uint64(h.PayloadLen) > MaxKeyframeBytes {
		return StreamFrameHeader{}, fmt.Errorf("%w: header+%d+%d bytes", ErrKeyframeTooLarge, h.ConfigLen, h.PayloadLen)
	}
	return h, nil
}
