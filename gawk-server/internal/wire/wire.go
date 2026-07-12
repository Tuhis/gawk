// Package wire implements the frozen datagram wire format shared by the
// relay server, the broadcaster, and the viewers.
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

// Message types, occupying byte 1 of every datagram.
const (
	// TypeVideoChunk identifies a VideoChunk datagram.
	TypeVideoChunk = 0x01
	// TypeDecoderConfig identifies a DecoderConfig datagram.
	TypeDecoderConfig = 0x02
	// TypeBroadcastAnnounce identifies a BroadcastAnnounce message.
	TypeBroadcastAnnounce = 0x03
)

// CloseCodeBroadcastEnded is the WebTransport application close code sent
// to subscribers when their broadcast is garbage-collected.
const CloseCodeBroadcastEnded = 4000

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
	MaxChunkCount = 1000
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
