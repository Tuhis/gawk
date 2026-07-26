package wire

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"
)

// Golden vectors, computed by hand from the wire format spec.
//
// NOTE: these hex strings will be copy-pasted into the TypeScript mirror's
// tests (gawk-app/src/transport/) — they are the cross-language portability
// guarantee. Do not regenerate them from code; if they change, the wire
// format changed.
const (
	// VideoChunk: Keyframe=true, FrameID=0x01020304, ChunkIndex=5,
	// ChunkCount=130, TimestampUs=0x0000005D21DBA5F0, payload "abc".
	//
	//   01                       version
	//   01                       type = VideoChunk
	//   01                       flags (bit0 keyframe = 1)
	//   00                       reserved
	//   01 02 03 04              frameID = 0x01020304
	//   00 05                    chunkIndex = 5
	//   00 82                    chunkCount = 130
	//   00 00 00 5d 21 db a5 f0  timestampUs = 0x0000005D21DBA5F0
	//   61 62 63                 payload "abc"
	goldenVideoChunkHex = "0101010001020304000500820000005d21dba5f0616263"

	// DecoderConfig: Codec="avc1.42E02A", extradata 01 42 E0 2A FF E1.
	//
	//   01                                 version
	//   02                                 type = DecoderConfig
	//   00                                 reserved
	//   0b                                 codecLen = 11
	//   61 76 63 31 2e 34 32 45 30 32 41   "avc1.42E02A"
	//   01 42 e0 2a ff e1                  extradata
	goldenDecoderConfigAVCHex = "0102000b617663312e3432453032410142e02affe1"

	// DecoderConfig: Codec="vp8", empty extradata.
	//
	//   01         version
	//   02         type = DecoderConfig
	//   00         reserved
	//   03         codecLen = 3
	//   76 70 38   "vp8"
	goldenDecoderConfigVP8Hex = "01020003767038"

	// StreamFrame header: Keyframe=true, FrameID=0x01020304,
	// TimestampUs=0x0000005D21DBA5F0, ConfigLen=6, PayloadLen=3.
	//
	//   01                       version
	//   04                       type = StreamFrame
	//   01                       flags (bit0 keyframe = 1)
	//   00                       reserved
	//   01 02 03 04              frameID = 0x01020304
	//   00 00 00 5d 21 db a5 f0  timestampUs = 0x0000005D21DBA5F0
	//   00 00 00 06              configLen = 6
	//   00 00 00 03              payloadLen = 3
	goldenStreamFrameHeaderHex = "01040100010203040000005d21dba5f00000000600000003"

	// TimeSync reply: ClientTimeUs=0x0102030405060708,
	// ServerTimeUs=0x090A0B0C0D0E0F10 (R5 Q2).
	//
	//   01                       version
	//   05                       type = TimeSync
	//   01 02 03 04 05 06 07 08  clientTimeUs
	//   09 0a 0b 0c 0d 0e 0f 10  serverTimeUs
	goldenTimeSyncHex = "01050102030405060708090a0b0c0d0e0f10"

	// TimeSync request: ClientTimeUs=1_000_000 (0x0F4240), ServerTimeUs=0.
	goldenTimeSyncRequestHex = "010500000000000f42400000000000000000"

	// ClockMapping: OffsetUs=+1_500_000 (0x16E360).
	//
	//   01                       version
	//   06                       type = ClockMapping
	//   00 00 00 00 00 16 e3 60  offsetUs (int64, big-endian)
	goldenClockMappingHex = "0106000000000016e360"

	// ClockMapping: OffsetUs=-1_000_000 (two's complement).
	goldenClockMappingNegativeHex = "0106fffffffffff0bdc0"

	// Reliable-carrier stream prologue (R19, docs/24 Decision 3).
	//
	//   01   version
	//   0a   type = ReliableCarrier
	goldenCarrierPrologueHex = "010a"

	// ViewerCount: count=3 (R18, docs/23 Decision 2).
	//
	//   01            version
	//   0b            type = ViewerCount
	//   00 00 00 03   count (uint32, big-endian)
	goldenViewerCountHex = "010b00000003"

	// ViewerCount: count=0x01020304 (every byte distinct — endianness pin).
	goldenViewerCountLargeHex = "010b01020304"

	// DeliveryAck: mode=DeliveryDVR, bufferMs=3000 (R21, docs/26 Decision 7a).
	//   01          version
	//   0c          type = DeliveryAck
	//   02          mode = DVR
	//   0bb8        accepted buffer = 3000 ms
	goldenDeliveryAckHex = "010c020bb8"
	// DeliveryAck: mode=DeliveryDatagrams, bufferMs=0 — what a viewer that
	// asked for reliable and was refused must be told.
	goldenDeliveryAckDatagramsHex = "010c000000"

	// One carrier record framing the golden VideoChunk datagram (23 bytes).
	//
	//   00 17   record length = 23
	//   01 01 01 00 ... 61 62 63   the golden VideoChunk datagram, verbatim
	goldenCarrierRecordHex = "0017" + goldenVideoChunkHex

	// The length prefix of a record at the inclusive upper boundary: a full
	// delta chunk is exactly MaxDatagramSize (1200 = 0x04B0), which is the
	// record size the carrier carries most often. The record body is a
	// 1200-byte VideoChunk, so only the prefix is worth pinning as hex.
	goldenCarrierMaxRecordPrefixHex = "04b0"

	// AudioFrame: Seq=0x01020304, TimestampUs=0x0000005D21DBA5F0,
	// payload "abc" (R15, docs/20 Decision 2).
	//
	//   01                       version
	//   07                       type = AudioFrame
	//   00                       flags (reserved)
	//   00                       reserved
	//   01 02 03 04              seq = 0x01020304
	//   00 00 00 5d 21 db a5 f0  timestampUs = 0x0000005D21DBA5F0
	//   61 62 63                 payload "abc"
	goldenAudioFrameHex = "01070000010203040000005d21dba5f0616263"

	// AudioConfig: Codec="opus", SampleRate=48000, Channels=2, empty
	// description (R15 — the production configuration).
	//
	//   01            version
	//   08            type = AudioConfig
	//   00            reserved
	//   04            codecLen = 4
	//   6f 70 75 73   "opus"
	//   00 00 bb 80   sampleRate = 48000
	//   02            channels = 2
	goldenAudioConfigHex = "010800046f7075730000bb8002"

	// AudioConfig: as above but with a 3-byte description 01 02 03.
	goldenAudioConfigDescHex = "010800046f7075730000bb8002010203"

	// TelemetryHello: Enabled=true, ReportIntervalMs=2000, a synthetic token
	// and broadcast key (R28, docs/33 §4.1). The token bytes here are
	// deliberately NOT a real HMAC — this vector pins the message framing, and
	// the token's own construction is pinned separately by
	// TestGoldenTelemetrySessionToken, so a change to either is visible on its
	// own.
	//
	//   01                          version
	//   0d                          type = TelemetryHello
	//   01                          flags (bit0 enabled = 1)
	//   07 d0                       reportIntervalMs = 2000
	//   00 01 23 45                 token: expHour
	//   00 01 02 03 04 05 06 07 08 09 0a 0b   token: nonce
	//   a1 a2 a3 a4 a5 a6 a7 a8     token: tag
	//   1a 2b 3c 4d 5e 6f           obfuscated broadcast key
	goldenTelemetryHelloHex = "010d0107d000012345000102030405060708090a0ba1a2a3a4a5a6a7a81a2b3c4d5e6f"

	// TelemetryHello with collection off: the fleet has no telemetry key, so
	// the client collects nothing and ignores every other field. Same shape as
	// the enabled one — zero-value token and key rather than a shorter message,
	// because a fixed-width message is what makes the strict parse trivial.
	goldenTelemetryHelloDisabledHex = "010d000000" + "000000000000000000000000000000000000000000000000000000000000"

	// The token minted for key=0x42*32, nonce=00..0b, broadcast key
	// 1a2b3c4d5e6f, role "viewer", at 2026-07-26T00:00:00Z. Pins expHour
	// arithmetic, field order and HMAC truncation together.
	goldenTelemetrySessionTokenHex = "000791b8000102030405060708090a0b9d4e7750cdf69a2b"
)

// Fixtures for the TelemetryHello + token vectors above.
var (
	goldenTelemetryToken = []byte{
		0x00, 0x01, 0x23, 0x45,
		0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b,
		0xa1, 0xa2, 0xa3, 0xa4, 0xa5, 0xa6, 0xa7, 0xa8,
	}
	goldenTelemetryBroadcastKey = []byte{0x1a, 0x2b, 0x3c, 0x4d, 0x5e, 0x6f}
)

var (
	goldenVideoChunkHeader = VideoChunkHeader{
		Keyframe:    true,
		FrameID:     0x01020304,
		ChunkIndex:  5,
		ChunkCount:  130,
		TimestampUs: 0x0000005D21DBA5F0,
	}
	goldenVideoChunkPayload = []byte("abc")

	goldenDecoderConfigAVC = DecoderConfig{
		Codec:     "avc1.42E02A",
		Extradata: []byte{0x01, 0x42, 0xE0, 0x2A, 0xFF, 0xE1},
	}
	goldenDecoderConfigVP8 = DecoderConfig{Codec: "vp8"}

	goldenStreamFrameHeader = StreamFrameHeader{
		Keyframe:    true,
		FrameID:     0x01020304,
		TimestampUs: 0x0000005D21DBA5F0,
		ConfigLen:   6,
		PayloadLen:  3,
	}

	goldenAudioFrameHeader = AudioFrameHeader{
		Seq:         0x01020304,
		TimestampUs: 0x0000005D21DBA5F0,
	}
	goldenAudioFramePayload = []byte("abc")

	goldenAudioConfig = AudioConfig{
		Codec:      "opus",
		SampleRate: 48000,
		Channels:   2,
	}
	goldenAudioConfigDesc = AudioConfig{
		Codec:       "opus",
		SampleRate:  48000,
		Channels:    2,
		Description: []byte{0x01, 0x02, 0x03},
	}
)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad hex constant %q: %v", s, err)
	}
	return b
}

func TestConstants(t *testing.T) {
	if MaxChunkPayload != MaxDatagramSize-VideoChunkHeaderSize {
		t.Fatalf("MaxChunkPayload = %d, want %d", MaxChunkPayload, MaxDatagramSize-VideoChunkHeaderSize)
	}
}

func TestVideoChunkRoundTrip(t *testing.T) {
	cases := []struct {
		name    string
		h       VideoChunkHeader
		payload []byte
	}{
		{"keyframe", VideoChunkHeader{Keyframe: true, FrameID: 42, ChunkIndex: 0, ChunkCount: 3, TimestampUs: 1234567}, []byte("hello")},
		{"delta frame", VideoChunkHeader{Keyframe: false, FrameID: 43, ChunkIndex: 2, ChunkCount: 3, TimestampUs: 7654321}, []byte{0xde, 0xad, 0xbe, 0xef}},
		{"empty payload", VideoChunkHeader{Keyframe: false, FrameID: 0, ChunkIndex: 0, ChunkCount: 1, TimestampUs: 0}, nil},
		{"max payload", VideoChunkHeader{Keyframe: true, FrameID: 0xFFFFFFFF, ChunkIndex: MaxChunkCount - 1, ChunkCount: MaxChunkCount, TimestampUs: 0xFFFFFFFFFFFFFFFF}, bytes.Repeat([]byte{0xAB}, MaxChunkPayload)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dgram, err := AppendVideoChunk(nil, tc.h, tc.payload)
			if err != nil {
				t.Fatalf("AppendVideoChunk: %v", err)
			}
			if len(dgram) > MaxDatagramSize {
				t.Fatalf("datagram %d bytes exceeds MaxDatagramSize", len(dgram))
			}
			ver, typ, err := PeekType(dgram)
			if err != nil || ver != Version || typ != TypeVideoChunk {
				t.Fatalf("PeekType = (%d, %d, %v), want (%d, %d, nil)", ver, typ, err, Version, TypeVideoChunk)
			}
			h, payload, err := ParseVideoChunk(dgram)
			if err != nil {
				t.Fatalf("ParseVideoChunk: %v", err)
			}
			if h != tc.h {
				t.Errorf("header = %+v, want %+v", h, tc.h)
			}
			if !bytes.Equal(payload, tc.payload) {
				t.Errorf("payload mismatch: got %d bytes, want %d bytes", len(payload), len(tc.payload))
			}
		})
	}

	t.Run("exceeds max chunk count", func(t *testing.T) {
		h := VideoChunkHeader{ChunkIndex: 0, ChunkCount: MaxChunkCount + 1}
		// AppendVideoChunk itself checks h.ChunkIndex >= h.ChunkCount, but doesn't check MaxChunkCount because Append is pure formatting.
		// So we construct a manual/raw datagram or call Append and Parse to verify.
		dgram, err := AppendVideoChunk(nil, h, []byte("x"))
		if err != nil {
			t.Fatalf("AppendVideoChunk: %v", err)
		}
		_, _, err = ParseVideoChunk(dgram)
		if !errors.Is(err, ErrBadChunkCount) {
			t.Fatalf("ParseVideoChunk error = %v, want %v", err, ErrBadChunkCount)
		}
	})
}

func TestVideoChunkAppendToExisting(t *testing.T) {
	prefix := []byte("existing")
	dgram, err := AppendVideoChunk(prefix, VideoChunkHeader{ChunkCount: 1}, []byte("x"))
	if err != nil {
		t.Fatalf("AppendVideoChunk: %v", err)
	}
	if !bytes.HasPrefix(dgram, prefix) {
		t.Fatalf("append did not preserve dst prefix")
	}
	if len(dgram) != len(prefix)+VideoChunkHeaderSize+1 {
		t.Fatalf("dgram length = %d, want %d", len(dgram), len(prefix)+VideoChunkHeaderSize+1)
	}
}

func TestVideoChunkPayloadAliasesInput(t *testing.T) {
	dgram, err := AppendVideoChunk(nil, VideoChunkHeader{ChunkCount: 1}, []byte("abc"))
	if err != nil {
		t.Fatalf("AppendVideoChunk: %v", err)
	}
	_, payload, err := ParseVideoChunk(dgram)
	if err != nil {
		t.Fatalf("ParseVideoChunk: %v", err)
	}
	dgram[VideoChunkHeaderSize] = 'Z'
	if payload[0] != 'Z' {
		t.Fatalf("payload does not alias input datagram")
	}
}

func TestDecoderConfigRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		c    DecoderConfig
	}{
		{"h264 with extradata", DecoderConfig{Codec: "avc1.640028", Extradata: []byte{0x01, 0x64, 0x00, 0x28}}},
		{"vp9 empty extradata", DecoderConfig{Codec: "vp09.00.40.08"}},
		{"single char codec", DecoderConfig{Codec: "x", Extradata: []byte{0x00}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dgram, err := AppendDecoderConfig(nil, tc.c)
			if err != nil {
				t.Fatalf("AppendDecoderConfig: %v", err)
			}
			ver, typ, err := PeekType(dgram)
			if err != nil || ver != Version || typ != TypeDecoderConfig {
				t.Fatalf("PeekType = (%d, %d, %v), want (%d, %d, nil)", ver, typ, err, Version, TypeDecoderConfig)
			}
			got, err := ParseDecoderConfig(dgram)
			if err != nil {
				t.Fatalf("ParseDecoderConfig: %v", err)
			}
			if got.Codec != tc.c.Codec {
				t.Errorf("codec = %q, want %q", got.Codec, tc.c.Codec)
			}
			if !bytes.Equal(got.Extradata, tc.c.Extradata) {
				t.Errorf("extradata = %x, want %x", got.Extradata, tc.c.Extradata)
			}
			if len(tc.c.Extradata) == 0 && len(got.Extradata) != 0 {
				t.Errorf("expected empty extradata, got %d bytes", len(got.Extradata))
			}
		})
	}
}

func TestGoldenVideoChunk(t *testing.T) {
	want := mustHex(t, goldenVideoChunkHex)

	dgram, err := AppendVideoChunk(nil, goldenVideoChunkHeader, goldenVideoChunkPayload)
	if err != nil {
		t.Fatalf("AppendVideoChunk: %v", err)
	}
	if !bytes.Equal(dgram, want) {
		t.Errorf("append produced %x, want %x", dgram, want)
	}

	h, payload, err := ParseVideoChunk(want)
	if err != nil {
		t.Fatalf("ParseVideoChunk: %v", err)
	}
	if h != goldenVideoChunkHeader {
		t.Errorf("header = %+v, want %+v", h, goldenVideoChunkHeader)
	}
	if !bytes.Equal(payload, goldenVideoChunkPayload) {
		t.Errorf("payload = %x, want %x", payload, goldenVideoChunkPayload)
	}
}

func TestGoldenDecoderConfig(t *testing.T) {
	cases := []struct {
		name    string
		hexData string
		want    DecoderConfig
	}{
		{"avc1 with extradata", goldenDecoderConfigAVCHex, goldenDecoderConfigAVC},
		{"vp8 empty extradata", goldenDecoderConfigVP8Hex, goldenDecoderConfigVP8},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantBytes := mustHex(t, tc.hexData)

			dgram, err := AppendDecoderConfig(nil, tc.want)
			if err != nil {
				t.Fatalf("AppendDecoderConfig: %v", err)
			}
			if !bytes.Equal(dgram, wantBytes) {
				t.Errorf("append produced %x, want %x", dgram, wantBytes)
			}

			got, err := ParseDecoderConfig(wantBytes)
			if err != nil {
				t.Fatalf("ParseDecoderConfig: %v", err)
			}
			if got.Codec != tc.want.Codec {
				t.Errorf("codec = %q, want %q", got.Codec, tc.want.Codec)
			}
			if !bytes.Equal(got.Extradata, tc.want.Extradata) {
				t.Errorf("extradata = %x, want %x", got.Extradata, tc.want.Extradata)
			}
		})
	}
}

func TestGoldenStreamFrameHeader(t *testing.T) {
	want := mustHex(t, goldenStreamFrameHeaderHex)

	buf, err := AppendStreamFrameHeader(nil, goldenStreamFrameHeader)
	if err != nil {
		t.Fatalf("AppendStreamFrameHeader: %v", err)
	}
	if !bytes.Equal(buf, want) {
		t.Errorf("append produced %x, want %x", buf, want)
	}
	if len(buf) != StreamFrameHeaderSize {
		t.Fatalf("header length = %d, want %d", len(buf), StreamFrameHeaderSize)
	}

	h, err := ParseStreamFrameHeader(want)
	if err != nil {
		t.Fatalf("ParseStreamFrameHeader: %v", err)
	}
	if h != goldenStreamFrameHeader {
		t.Errorf("header = %+v, want %+v", h, goldenStreamFrameHeader)
	}
}

func TestStreamFrameHeaderRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		h    StreamFrameHeader
	}{
		{"keyframe with config", StreamFrameHeader{Keyframe: true, FrameID: 42, TimestampUs: 1234567, ConfigLen: 20, PayloadLen: 500000}},
		{"no config", StreamFrameHeader{Keyframe: true, FrameID: 0, TimestampUs: 0, ConfigLen: 0, PayloadLen: 1}},
		{"max sizes", StreamFrameHeader{Keyframe: true, FrameID: 0xFFFFFFFF, TimestampUs: 0xFFFFFFFFFFFFFFFF, ConfigLen: 0, PayloadLen: MaxKeyframeBytes - StreamFrameHeaderSize}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf, err := AppendStreamFrameHeader(nil, tc.h)
			if err != nil {
				t.Fatalf("AppendStreamFrameHeader: %v", err)
			}
			ver, typ, err := PeekType(buf)
			if err != nil || ver != Version || typ != TypeStreamFrame {
				t.Fatalf("PeekType = (%d, %d, %v), want (%d, %d, nil)", ver, typ, err, Version, TypeStreamFrame)
			}
			got, err := ParseStreamFrameHeader(buf)
			if err != nil {
				t.Fatalf("ParseStreamFrameHeader: %v", err)
			}
			if got != tc.h {
				t.Errorf("header = %+v, want %+v", got, tc.h)
			}
		})
	}
}

func TestStreamFrameHeaderErrors(t *testing.T) {
	valid := mustHex(t, goldenStreamFrameHeaderHex)
	corrupt := func(offset int, b byte) []byte {
		d := bytes.Clone(valid)
		d[offset] = b
		return d
	}

	t.Run("parse errors", func(t *testing.T) {
		cases := []struct {
			name string
			buf  []byte
			want error
		}{
			{"empty", nil, ErrShortDatagram},
			{"23 bytes", valid[:23], ErrShortDatagram},
			{"bad version", corrupt(0, 0x02), ErrBadVersion},
			{"bad type", corrupt(1, 0x02), ErrBadType},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if _, err := ParseStreamFrameHeader(tc.buf); !errors.Is(err, tc.want) {
					t.Errorf("error = %v, want %v", err, tc.want)
				}
			})
		}
	})

	t.Run("oversize declared payload rejected on parse", func(t *testing.T) {
		// PayloadLen so large that header+config+payload overflows MaxKeyframeBytes.
		h := StreamFrameHeader{Keyframe: true, ConfigLen: 0, PayloadLen: MaxKeyframeBytes}
		buf := make([]byte, StreamFrameHeaderSize)
		buf[0], buf[1], buf[2] = Version, TypeStreamFrame, flagKeyframe
		binary.BigEndian.PutUint32(buf[20:24], h.PayloadLen)
		if _, err := ParseStreamFrameHeader(buf); !errors.Is(err, ErrKeyframeTooLarge) {
			t.Errorf("error = %v, want %v", err, ErrKeyframeTooLarge)
		}
	})

	t.Run("oversize rejected on append", func(t *testing.T) {
		h := StreamFrameHeader{Keyframe: true, PayloadLen: MaxKeyframeBytes}
		if _, err := AppendStreamFrameHeader(nil, h); !errors.Is(err, ErrKeyframeTooLarge) {
			t.Errorf("error = %v, want %v", err, ErrKeyframeTooLarge)
		}
	})
}

func TestPeekTypeErrors(t *testing.T) {
	for _, dgram := range [][]byte{nil, {}, {0x01}} {
		if _, _, err := PeekType(dgram); !errors.Is(err, ErrShortDatagram) {
			t.Errorf("PeekType(%x) error = %v, want ErrShortDatagram", dgram, err)
		}
	}
	// PeekType does not validate version or type.
	ver, typ, err := PeekType([]byte{0xFF, 0xEE})
	if err != nil || ver != 0xFF || typ != 0xEE {
		t.Errorf("PeekType = (%#x, %#x, %v), want (0xff, 0xee, nil)", ver, typ, err)
	}
}

func TestParseVideoChunkErrors(t *testing.T) {
	valid := mustHex(t, goldenVideoChunkHex)

	corrupt := func(offset int, b byte) []byte {
		d := bytes.Clone(valid)
		d[offset] = b
		return d
	}
	setU16 := func(offset int, v uint16) []byte {
		d := bytes.Clone(valid)
		d[offset] = byte(v >> 8)
		d[offset+1] = byte(v)
		return d
	}

	cases := []struct {
		name  string
		dgram []byte
		want  error
	}{
		{"empty", nil, ErrShortDatagram},
		{"1 byte", valid[:1], ErrShortDatagram},
		{"19 bytes", valid[:19], ErrShortDatagram},
		{"bad version", corrupt(0, 0x02), ErrBadVersion},
		{"bad type", corrupt(1, 0x02), ErrBadType},
		{"chunkCount zero", setU16(10, 0), ErrBadChunkCount},
		{"chunkIndex == chunkCount", setU16(8, 130), ErrBadChunkCount},
		{"chunkIndex > chunkCount", setU16(8, 200), ErrBadChunkCount},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := ParseVideoChunk(tc.dgram); !errors.Is(err, tc.want) {
				t.Errorf("error = %v, want %v", err, tc.want)
			}
		})
	}

	// Exactly header-sized datagram with empty payload is valid.
	if _, payload, err := ParseVideoChunk(valid[:VideoChunkHeaderSize]); err != nil || len(payload) != 0 {
		t.Errorf("20-byte datagram: payload = %x, err = %v; want empty, nil", payload, err)
	}
}

func TestAppendVideoChunkErrors(t *testing.T) {
	cases := []struct {
		name    string
		h       VideoChunkHeader
		payload []byte
		want    error
	}{
		{"oversized payload", VideoChunkHeader{ChunkCount: 1}, make([]byte, MaxChunkPayload+1), ErrPayloadTooLarge},
		{"chunkCount zero", VideoChunkHeader{ChunkCount: 0}, nil, ErrBadChunkCount},
		{"chunkIndex == chunkCount", VideoChunkHeader{ChunkIndex: 3, ChunkCount: 3}, nil, ErrBadChunkCount},
		{"chunkIndex > chunkCount", VideoChunkHeader{ChunkIndex: 4, ChunkCount: 3}, nil, ErrBadChunkCount},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := AppendVideoChunk(nil, tc.h, tc.payload); !errors.Is(err, tc.want) {
				t.Errorf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestParseDecoderConfigErrors(t *testing.T) {
	cases := []struct {
		name  string
		dgram []byte
		want  error
	}{
		{"empty", nil, ErrShortDatagram},
		{"1 byte", []byte{0x01}, ErrShortDatagram},
		{"3 bytes", []byte{0x01, 0x02, 0x00}, ErrShortDatagram},
		{"bad version", []byte{0x02, 0x02, 0x00, 0x01, 'x'}, ErrBadVersion},
		{"bad type", []byte{0x01, 0x01, 0x00, 0x01, 'x'}, ErrBadType},
		{"codecLen overruns datagram", []byte{0x01, 0x02, 0x00, 0x05, 'v', 'p', '8'}, ErrBadCodec},
		{"codecLen overruns by one", []byte{0x01, 0x02, 0x00, 0x04, 'v', 'p', '8'}, ErrBadCodec},
		{"codecLen zero", []byte{0x01, 0x02, 0x00, 0x00}, ErrBadCodec},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseDecoderConfig(tc.dgram); !errors.Is(err, tc.want) {
				t.Errorf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestAppendDecoderConfigErrors(t *testing.T) {
	cases := []struct {
		name string
		c    DecoderConfig
		want error
	}{
		{"empty codec", DecoderConfig{Codec: ""}, ErrBadCodec},
		{"codec over 255 bytes", DecoderConfig{Codec: strings.Repeat("a", 256)}, ErrBadCodec},
		{"total exceeds MaxDatagramSize", DecoderConfig{Codec: "vp8", Extradata: make([]byte, MaxDatagramSize)}, ErrDatagramTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := AppendDecoderConfig(nil, tc.c); !errors.Is(err, tc.want) {
				t.Errorf("error = %v, want %v", err, tc.want)
			}
		})
	}

	// Exactly at the size limit is fine: 4 + 3 + 1193 = 1200.
	c := DecoderConfig{Codec: "vp8", Extradata: make([]byte, MaxDatagramSize-4-3)}
	dgram, err := AppendDecoderConfig(nil, c)
	if err != nil {
		t.Fatalf("AppendDecoderConfig at limit: %v", err)
	}
	if len(dgram) != MaxDatagramSize {
		t.Fatalf("dgram length = %d, want %d", len(dgram), MaxDatagramSize)
	}
}

// FuzzParse feeds arbitrary bytes to PeekType and both parsers, asserting
// only that they never panic and, on success, uphold their invariants.
func FuzzParse(f *testing.F) {
	seed := func(hexData string) []byte {
		b, err := hex.DecodeString(hexData)
		if err != nil {
			f.Fatalf("bad seed hex %q: %v", hexData, err)
		}
		return b
	}
	f.Add(seed(goldenVideoChunkHex))
	f.Add(seed(goldenDecoderConfigAVCHex))
	f.Add(seed(goldenDecoderConfigVP8Hex))
	f.Add(seed(goldenStreamFrameHeaderHex))
	f.Add(seed("0103064b375851324d"))
	f.Add([]byte{})
	f.Add([]byte{0x01})
	f.Add([]byte{0x01, 0x01})
	f.Add([]byte{0x01, 0x02, 0x00, 0xFF})

	f.Fuzz(func(t *testing.T, data []byte) {
		_, _, _ = PeekType(data)

		if h, payload, err := ParseVideoChunk(data); err == nil {
			if h.ChunkCount == 0 || h.ChunkIndex >= h.ChunkCount {
				t.Errorf("ParseVideoChunk accepted invalid index/count: %+v", h)
			}
			if len(payload) > MaxChunkPayload && len(data) <= MaxDatagramSize {
				t.Errorf("payload %d bytes from %d-byte datagram", len(payload), len(data))
			}
		}

		if c, err := ParseDecoderConfig(data); err == nil {
			if len(c.Codec) == 0 || len(c.Codec) > 255 {
				t.Errorf("ParseDecoderConfig accepted codec of length %d", len(c.Codec))
			}
		}

		if _, err := ParseBroadcastAnnounce(data); err == nil {
			// just asserting no panic
		}

		if h, err := ParseStreamFrameHeader(data); err == nil {
			if StreamFrameHeaderSize+uint64(h.ConfigLen)+uint64(h.PayloadLen) > MaxKeyframeBytes {
				t.Errorf("ParseStreamFrameHeader accepted oversize declaration: %+v", h)
			}
		}
	})
}

func TestBroadcastAnnounceRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		id   string
	}{
		{"normal ID", "ABCDEF"},
		{"numbers in ID", "234567"},
		{"mixed standard ID", "K7XQ2M"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dgram, err := AppendBroadcastAnnounce(nil, tc.id)
			if err != nil {
				t.Fatalf("AppendBroadcastAnnounce: %v", err)
			}
			ver, typ, err := PeekType(dgram)
			if err != nil || ver != Version || typ != TypeBroadcastAnnounce {
				t.Fatalf("PeekType = (%d, %d, %v), want (%d, %d, nil)", ver, typ, err, Version, TypeBroadcastAnnounce)
			}
			got, err := ParseBroadcastAnnounce(dgram)
			if err != nil {
				t.Fatalf("ParseBroadcastAnnounce: %v", err)
			}
			if got != tc.id {
				t.Errorf("got %q, want %q", got, tc.id)
			}
		})
	}
}

func TestGoldenBroadcastAnnounce(t *testing.T) {
	const goldenHex = "0103064b375851324d"
	const expectedID = "K7XQ2M"
	wantBytes := mustHex(t, goldenHex)

	dgram, err := AppendBroadcastAnnounce(nil, expectedID)
	if err != nil {
		t.Fatalf("AppendBroadcastAnnounce: %v", err)
	}
	if !bytes.Equal(dgram, wantBytes) {
		t.Errorf("AppendBroadcastAnnounce produced %x, want %x", dgram, wantBytes)
	}

	got, err := ParseBroadcastAnnounce(wantBytes)
	if err != nil {
		t.Fatalf("ParseBroadcastAnnounce: %v", err)
	}
	if got != expectedID {
		t.Errorf("ParseBroadcastAnnounce got %q, want %q", got, expectedID)
	}
}

func TestParseBroadcastAnnounceErrors(t *testing.T) {
	cases := []struct {
		name  string
		dgram []byte
		want  error
	}{
		{"empty", nil, ErrShortDatagram},
		{"1 byte", []byte{0x01}, ErrShortDatagram},
		{"2 bytes", []byte{0x01, 0x03}, ErrShortDatagram},
		{"bad version", []byte{0x02, 0x03, 0x01, 'K'}, ErrBadVersion},
		{"bad type", []byte{0x01, 0x02, 0x01, 'K'}, ErrBadType},
		{"idLen overrun", []byte{0x01, 0x03, 0x05, 'K'}, ErrBadBroadcastID},
		{"idLen underrun", []byte{0x01, 0x03, 0x01, 'K', '7'}, ErrBadBroadcastID},
		{"chars outside alphabet (lowercase)", []byte{0x01, 0x03, 0x01, 'k'}, ErrBadBroadcastID},
		{"chars outside alphabet (invalid symbols)", []byte{0x01, 0x03, 0x01, '0'}, ErrBadBroadcastID},
		{"chars outside alphabet (invalid letter O)", []byte{0x01, 0x03, 0x01, 'O'}, ErrBadBroadcastID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseBroadcastAnnounce(tc.dgram); !errors.Is(err, tc.want) {
				t.Errorf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestAppendBroadcastAnnounceErrors(t *testing.T) {
	cases := []struct {
		name string
		id   string
		want error
	}{
		{"empty ID", "", ErrBadBroadcastID},
		{"too long ID", strings.Repeat("K", 256), ErrBadBroadcastID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := AppendBroadcastAnnounce(nil, tc.id); !errors.Is(err, tc.want) {
				t.Errorf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestTimeSyncGolden(t *testing.T) {
	reply := mustHex(t, goldenTimeSyncHex)
	if got := AppendTimeSync(nil, 0x0102030405060708, 0x090A0B0C0D0E0F10); !bytes.Equal(got, reply) {
		t.Errorf("AppendTimeSync = %x, want %x", got, reply)
	}
	request := mustHex(t, goldenTimeSyncRequestHex)
	if got := AppendTimeSync(nil, 1_000_000, 0); !bytes.Equal(got, request) {
		t.Errorf("AppendTimeSync request = %x, want %x", got, request)
	}

	clientUs, serverUs, err := ParseTimeSync(reply)
	if err != nil {
		t.Fatalf("ParseTimeSync: %v", err)
	}
	if clientUs != 0x0102030405060708 || serverUs != 0x090A0B0C0D0E0F10 {
		t.Errorf("ParseTimeSync = (%#x, %#x)", clientUs, serverUs)
	}
	clientUs, serverUs, err = ParseTimeSync(request)
	if err != nil {
		t.Fatalf("ParseTimeSync request: %v", err)
	}
	if clientUs != 1_000_000 || serverUs != 0 {
		t.Errorf("ParseTimeSync request = (%d, %d), want (1000000, 0)", clientUs, serverUs)
	}
}

func TestClockMappingGolden(t *testing.T) {
	positive := mustHex(t, goldenClockMappingHex)
	if got := AppendClockMapping(nil, 1_500_000); !bytes.Equal(got, positive) {
		t.Errorf("AppendClockMapping = %x, want %x", got, positive)
	}
	negative := mustHex(t, goldenClockMappingNegativeHex)
	if got := AppendClockMapping(nil, -1_000_000); !bytes.Equal(got, negative) {
		t.Errorf("AppendClockMapping negative = %x, want %x", got, negative)
	}

	offsetUs, err := ParseClockMapping(positive)
	if err != nil {
		t.Fatalf("ParseClockMapping: %v", err)
	}
	if offsetUs != 1_500_000 {
		t.Errorf("offsetUs = %d, want 1500000", offsetUs)
	}
	offsetUs, err = ParseClockMapping(negative)
	if err != nil {
		t.Fatalf("ParseClockMapping negative: %v", err)
	}
	if offsetUs != -1_000_000 {
		t.Errorf("offsetUs = %d, want -1000000", offsetUs)
	}
}

func TestTimeSyncParseErrors(t *testing.T) {
	good := mustHex(t, goldenTimeSyncHex)
	cases := []struct {
		name  string
		dgram []byte
		want  error
	}{
		{"truncated", good[:TimeSyncSize-1], ErrBadLength},
		{"oversize", append(append([]byte{}, good...), 0x00), ErrBadLength},
		{"bad version", append([]byte{0x02}, good[1:]...), ErrBadVersion},
		{"bad type", mustHex(t, goldenClockMappingHex+"0000000000000000"), ErrBadType},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := ParseTimeSync(tc.dgram); !errors.Is(err, tc.want) {
				t.Errorf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestClockMappingParseErrors(t *testing.T) {
	good := mustHex(t, goldenClockMappingHex)
	cases := []struct {
		name  string
		dgram []byte
		want  error
	}{
		{"truncated", good[:ClockMappingSize-1], ErrBadLength},
		{"oversize", append(append([]byte{}, good...), 0x00), ErrBadLength},
		{"bad version", append([]byte{0x02}, good[1:]...), ErrBadVersion},
		{"bad type (time sync sized down)", mustHex(t, goldenTimeSyncHex)[:ClockMappingSize], ErrBadType},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseClockMapping(tc.dgram); !errors.Is(err, tc.want) {
				t.Errorf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestViewerCountGolden(t *testing.T) {
	small := mustHex(t, goldenViewerCountHex)
	if got := AppendViewerCount(nil, 3); !bytes.Equal(got, small) {
		t.Errorf("AppendViewerCount = %x, want %x", got, small)
	}
	large := mustHex(t, goldenViewerCountLargeHex)
	if got := AppendViewerCount(nil, 0x01020304); !bytes.Equal(got, large) {
		t.Errorf("AppendViewerCount large = %x, want %x", got, large)
	}

	count, err := ParseViewerCount(small)
	if err != nil {
		t.Fatalf("ParseViewerCount: %v", err)
	}
	if count != 3 {
		t.Errorf("count = %d, want 3", count)
	}
	count, err = ParseViewerCount(large)
	if err != nil {
		t.Fatalf("ParseViewerCount large: %v", err)
	}
	if count != 0x01020304 {
		t.Errorf("count = %#x, want 0x01020304", count)
	}
}

func TestDeliveryAckGolden(t *testing.T) {
	dvr := mustHex(t, goldenDeliveryAckHex)
	if got := AppendDeliveryAck(nil, DeliveryDVR, 3000); !bytes.Equal(got, dvr) {
		t.Errorf("AppendDeliveryAck = %x, want %x", got, dvr)
	}
	plain := mustHex(t, goldenDeliveryAckDatagramsHex)
	if got := AppendDeliveryAck(nil, DeliveryDatagrams, 0); !bytes.Equal(got, plain) {
		t.Errorf("AppendDeliveryAck datagrams = %x, want %x", got, plain)
	}

	mode, buf, err := ParseDeliveryAck(dvr)
	if err != nil {
		t.Fatalf("ParseDeliveryAck: %v", err)
	}
	if mode != DeliveryDVR || buf != 3000 {
		t.Errorf("mode/buffer = %d/%d, want %d/3000", mode, buf, DeliveryDVR)
	}
}

func TestDeliveryAckRoundTrip(t *testing.T) {
	for _, mode := range []DeliveryMode{DeliveryDatagrams, DeliveryReliable, DeliveryDVR} {
		for _, ms := range []uint16{0, 1, 1000, 3000, 0xFFFF} {
			dgram := AppendDeliveryAck(nil, mode, ms)
			if len(dgram) != DeliveryAckSize {
				t.Fatalf("DeliveryAck is %d bytes, want %d", len(dgram), DeliveryAckSize)
			}
			gotMode, gotMs, err := ParseDeliveryAck(dgram)
			if err != nil {
				t.Fatalf("ParseDeliveryAck(%d,%d): %v", mode, ms, err)
			}
			if gotMode != mode || gotMs != ms {
				t.Errorf("round trip = %d/%d, want %d/%d", gotMode, gotMs, mode, ms)
			}
		}
	}
}

func TestDeliveryAckRejectsMalformed(t *testing.T) {
	good := AppendDeliveryAck(nil, DeliveryDVR, 3000)
	cases := map[string][]byte{
		"short":       good[:len(good)-1],
		"long":        append(append([]byte{}, good...), 0),
		"bad version": {0x02, TypeDeliveryAck, 2, 0x0b, 0xb8},
		"bad type":    {Version, TypeViewerCount, 2, 0x0b, 0xb8},
		// An unknown mode must be rejected rather than silently reported as
		// datagrams: a viewer that cannot name what it got is the exact
		// diagnostic gap this message exists to close (docs/26 Decision 7a).
		"unknown mode": {Version, TypeDeliveryAck, 9, 0x0b, 0xb8},
	}
	for name, dgram := range cases {
		if _, _, err := ParseDeliveryAck(dgram); err == nil {
			t.Errorf("%s: ParseDeliveryAck accepted %x", name, dgram)
		}
	}
}

func TestViewerCountRoundTrip(t *testing.T) {
	// Zero is legitimate (everyone left) and must round-trip like any other.
	for _, count := range []uint32{0, 1, 15, 500, 0xFFFFFFFF} {
		dgram := AppendViewerCount(nil, count)
		if len(dgram) != ViewerCountSize {
			t.Fatalf("len = %d, want %d", len(dgram), ViewerCountSize)
		}
		got, err := ParseViewerCount(dgram)
		if err != nil {
			t.Fatalf("ParseViewerCount(%d): %v", count, err)
		}
		if got != count {
			t.Errorf("round trip = %d, want %d", got, count)
		}
	}
}

func TestViewerCountParseErrors(t *testing.T) {
	good := mustHex(t, goldenViewerCountHex)
	cases := []struct {
		name  string
		dgram []byte
		want  error
	}{
		{"truncated", good[:ViewerCountSize-1], ErrBadLength},
		{"oversize", append(append([]byte{}, good...), 0x00), ErrBadLength},
		{"bad version", append([]byte{0x02}, good[1:]...), ErrBadVersion},
		{"bad type (clock mapping sized down)", mustHex(t, goldenClockMappingHex)[:ViewerCountSize], ErrBadType},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseViewerCount(tc.dgram); !errors.Is(err, tc.want) {
				t.Errorf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

// ResumeToken (R17 W2, docs/22 Decision 7).

func TestResumeTokenRoundTrip(t *testing.T) {
	tokens := [][]byte{
		{0x42},
		mustHex(t, "000102030405060708090a0b0c0d0e0f"),
		bytes.Repeat([]byte{0xAB}, 255),
	}
	for _, token := range tokens {
		msg, err := AppendResumeToken(nil, token)
		if err != nil {
			t.Fatalf("AppendResumeToken(%d bytes): %v", len(token), err)
		}
		ver, typ, err := PeekType(msg)
		if err != nil || ver != Version || typ != TypeResumeToken {
			t.Fatalf("PeekType = (%d, %d, %v), want (%d, %d, nil)", ver, typ, err, Version, TypeResumeToken)
		}
		got, err := ParseResumeToken(msg)
		if err != nil {
			t.Fatalf("ParseResumeToken: %v", err)
		}
		if !bytes.Equal(got, token) {
			t.Errorf("round trip got %x, want %x", got, token)
		}
	}
}

// NOTE: copy-pasted into the TypeScript mirror's golden test (wire.test.ts).
const goldenResumeTokenHex = "010910000102030405060708090a0b0c0d0e0f"

func TestGoldenResumeToken(t *testing.T) {
	token := mustHex(t, "000102030405060708090a0b0c0d0e0f")
	wantBytes := mustHex(t, goldenResumeTokenHex)

	msg, err := AppendResumeToken(nil, token)
	if err != nil {
		t.Fatalf("AppendResumeToken: %v", err)
	}
	if !bytes.Equal(msg, wantBytes) {
		t.Errorf("AppendResumeToken produced %x, want %x", msg, wantBytes)
	}

	got, err := ParseResumeToken(wantBytes)
	if err != nil {
		t.Fatalf("ParseResumeToken: %v", err)
	}
	if !bytes.Equal(got, token) {
		t.Errorf("ParseResumeToken got %x, want %x", got, token)
	}
}

func TestParseResumeTokenErrors(t *testing.T) {
	cases := []struct {
		name string
		msg  []byte
		want error
	}{
		{"empty", nil, ErrShortDatagram},
		{"2 bytes", []byte{0x01, 0x09}, ErrShortDatagram},
		{"bad version", []byte{0x02, 0x09, 0x01, 0x42}, ErrBadVersion},
		{"bad type", []byte{0x01, 0x03, 0x01, 0x42}, ErrBadType},
		{"zero-length token", []byte{0x01, 0x09, 0x00}, ErrBadResumeToken},
		{"declared overrun", []byte{0x01, 0x09, 0x05, 0x42}, ErrBadResumeToken},
		{"declared underrun", []byte{0x01, 0x09, 0x01, 0x42, 0x43}, ErrBadResumeToken},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseResumeToken(tc.msg); !errors.Is(err, tc.want) {
				t.Errorf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestAppendResumeTokenErrors(t *testing.T) {
	if _, err := AppendResumeToken(nil, nil); !errors.Is(err, ErrBadResumeToken) {
		t.Errorf("empty token error = %v, want ErrBadResumeToken", err)
	}
	if _, err := AppendResumeToken(nil, bytes.Repeat([]byte{1}, 256)); !errors.Is(err, ErrBadResumeToken) {
		t.Errorf("256-byte token error = %v, want ErrBadResumeToken", err)
	}
}

func TestGoldenCarrierPrologue(t *testing.T) {
	got := AppendCarrierPrologue(nil)
	if want := mustHex(t, goldenCarrierPrologueHex); !bytes.Equal(got, want) {
		t.Errorf("carrier prologue = %x, want %x", got, want)
	}
	if err := ParseCarrierPrologue(got); err != nil {
		t.Errorf("ParseCarrierPrologue(golden) = %v, want nil", err)
	}
}

func TestGoldenCarrierRecord(t *testing.T) {
	dgram := mustHex(t, goldenVideoChunkHex)
	got, err := AppendCarrierRecord(nil, dgram)
	if err != nil {
		t.Fatalf("AppendCarrierRecord: %v", err)
	}
	if want := mustHex(t, goldenCarrierRecordHex); !bytes.Equal(got, want) {
		t.Errorf("carrier record = %x, want %x", got, want)
	}

	record, rest, err := ParseCarrierRecord(got)
	if err != nil {
		t.Fatalf("ParseCarrierRecord: %v", err)
	}
	if !bytes.Equal(record, dgram) {
		t.Errorf("record = %x, want %x", record, dgram)
	}
	if len(rest) != 0 {
		t.Errorf("rest = %x, want empty", rest)
	}
}

func TestCarrierRecordSequence(t *testing.T) {
	// Two records back to back, as they would land on one carrier stream.
	first := mustHex(t, goldenVideoChunkHex)
	second := mustHex(t, goldenDecoderConfigAVCHex)
	buf := AppendCarrierPrologue(nil)
	var err error
	if buf, err = AppendCarrierRecord(buf, first); err != nil {
		t.Fatalf("AppendCarrierRecord(first): %v", err)
	}
	if buf, err = AppendCarrierRecord(buf, second); err != nil {
		t.Fatalf("AppendCarrierRecord(second): %v", err)
	}

	if err := ParseCarrierPrologue(buf); err != nil {
		t.Fatalf("ParseCarrierPrologue: %v", err)
	}
	rest := buf[CarrierPrologueSize:]
	record, rest, err := ParseCarrierRecord(rest)
	if err != nil {
		t.Fatalf("ParseCarrierRecord(first): %v", err)
	}
	if !bytes.Equal(record, first) {
		t.Errorf("first record = %x, want %x", record, first)
	}
	record, rest, err = ParseCarrierRecord(rest)
	if err != nil {
		t.Fatalf("ParseCarrierRecord(second): %v", err)
	}
	if !bytes.Equal(record, second) {
		t.Errorf("second record = %x, want %x", record, second)
	}
	if len(rest) != 0 {
		t.Errorf("rest = %x, want empty", rest)
	}
}

// The inclusive upper boundary of the uint16 length prefix. A full delta
// chunk is exactly MaxDatagramSize bytes, so this is the *common* record on
// a carrier — not an exotic edge — and the framing has to carry it whole and
// still frame whatever follows it on the stream. DecoderConfig and AudioFrame
// both pin their exact-1200 boundary; this is the symmetric carrier vector
// (docs/24 finding 15).
func TestCarrierRecordMaxSizeDatagram(t *testing.T) {
	h := VideoChunkHeader{FrameID: 43, ChunkIndex: 1, ChunkCount: 2, TimestampUs: 7654321}
	dgram, err := AppendVideoChunk(nil, h, bytes.Repeat([]byte{0xAB}, MaxChunkPayload))
	if err != nil {
		t.Fatalf("AppendVideoChunk: %v", err)
	}
	if len(dgram) != MaxDatagramSize {
		t.Fatalf("full delta chunk is %d bytes, want %d", len(dgram), MaxDatagramSize)
	}

	trailer := mustHex(t, goldenVideoChunkHex)
	buf, err := AppendCarrierRecord(nil, dgram)
	if err != nil {
		t.Fatalf("AppendCarrierRecord(max size): %v", err)
	}
	if buf, err = AppendCarrierRecord(buf, trailer); err != nil {
		t.Fatalf("AppendCarrierRecord(trailer): %v", err)
	}
	if want := 2*CarrierRecordHeaderSize + MaxDatagramSize + len(trailer); len(buf) != want {
		t.Fatalf("carrier bytes = %d, want %d", len(buf), want)
	}
	if got := hex.EncodeToString(buf[:CarrierRecordHeaderSize]); got != goldenCarrierMaxRecordPrefixHex {
		t.Errorf("max-size length prefix = %s, want %s", got, goldenCarrierMaxRecordPrefixHex)
	}

	record, rest, err := ParseCarrierRecord(buf)
	if err != nil {
		t.Fatalf("ParseCarrierRecord(max size): %v", err)
	}
	if !bytes.Equal(record, dgram) {
		t.Errorf("max-size record = %d bytes, want the %d-byte datagram verbatim", len(record), len(dgram))
	}
	record, rest, err = ParseCarrierRecord(rest)
	if err != nil {
		t.Fatalf("ParseCarrierRecord(trailer): %v", err)
	}
	if !bytes.Equal(record, trailer) {
		t.Errorf("trailing record = %x, want %x", record, trailer)
	}
	if len(rest) != 0 {
		t.Errorf("rest = %x, want empty", rest)
	}
}

func TestCarrierPrologueErrors(t *testing.T) {
	cases := []struct {
		name string
		buf  []byte
		want error
	}{
		{"empty", nil, ErrShortDatagram},
		{"1 byte", []byte{0x01}, ErrShortDatagram},
		{"bad version", []byte{0x02, 0x0A}, ErrBadVersion},
		{"bad type", []byte{0x01, 0x04}, ErrBadType},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := ParseCarrierPrologue(tc.buf); !errors.Is(err, tc.want) {
				t.Errorf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestCarrierRecordErrors(t *testing.T) {
	if _, err := AppendCarrierRecord(nil, nil); !errors.Is(err, ErrBadCarrierRecord) {
		t.Errorf("empty datagram error = %v, want ErrBadCarrierRecord", err)
	}
	if _, err := AppendCarrierRecord(nil, bytes.Repeat([]byte{1}, MaxDatagramSize+1)); !errors.Is(err, ErrBadCarrierRecord) {
		t.Errorf("oversize datagram error = %v, want ErrBadCarrierRecord", err)
	}

	cases := []struct {
		name string
		buf  []byte
		want error
	}{
		{"empty", nil, ErrShortDatagram},
		{"1 byte", []byte{0x00}, ErrShortDatagram},
		{"zero length", []byte{0x00, 0x00, 0x42}, ErrBadCarrierRecord},
		{"oversize length", binary.BigEndian.AppendUint16(nil, MaxDatagramSize+1), ErrBadCarrierRecord},
		{"incomplete record", []byte{0x00, 0x03, 0x61, 0x62}, ErrShortDatagram},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := ParseCarrierRecord(tc.buf); !errors.Is(err, tc.want) {
				t.Errorf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestAudioConstants(t *testing.T) {
	if MaxAudioPayload != MaxDatagramSize-AudioFrameHeaderSize {
		t.Fatalf("MaxAudioPayload = %d, want %d", MaxAudioPayload, MaxDatagramSize-AudioFrameHeaderSize)
	}
}

func TestGoldenAudioFrame(t *testing.T) {
	want := mustHex(t, goldenAudioFrameHex)

	dgram, err := AppendAudioFrame(nil, goldenAudioFrameHeader, goldenAudioFramePayload)
	if err != nil {
		t.Fatalf("AppendAudioFrame: %v", err)
	}
	if !bytes.Equal(dgram, want) {
		t.Errorf("append produced %x, want %x", dgram, want)
	}

	h, payload, err := ParseAudioFrame(want)
	if err != nil {
		t.Fatalf("ParseAudioFrame: %v", err)
	}
	if h != goldenAudioFrameHeader {
		t.Errorf("header = %+v, want %+v", h, goldenAudioFrameHeader)
	}
	if !bytes.Equal(payload, goldenAudioFramePayload) {
		t.Errorf("payload = %x, want %x", payload, goldenAudioFramePayload)
	}
}

func TestGoldenAudioConfig(t *testing.T) {
	cases := []struct {
		name    string
		hexData string
		want    AudioConfig
	}{
		{"opus empty description", goldenAudioConfigHex, goldenAudioConfig},
		{"opus with description", goldenAudioConfigDescHex, goldenAudioConfigDesc},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantBytes := mustHex(t, tc.hexData)

			dgram, err := AppendAudioConfig(nil, tc.want)
			if err != nil {
				t.Fatalf("AppendAudioConfig: %v", err)
			}
			if !bytes.Equal(dgram, wantBytes) {
				t.Errorf("append produced %x, want %x", dgram, wantBytes)
			}

			got, err := ParseAudioConfig(wantBytes)
			if err != nil {
				t.Fatalf("ParseAudioConfig: %v", err)
			}
			if got.Codec != tc.want.Codec || got.SampleRate != tc.want.SampleRate || got.Channels != tc.want.Channels {
				t.Errorf("config = %+v, want %+v", got, tc.want)
			}
			if !bytes.Equal(got.Description, tc.want.Description) {
				t.Errorf("description = %x, want %x", got.Description, tc.want.Description)
			}
		})
	}
}

func TestAudioFrameRoundTrip(t *testing.T) {
	h := AudioFrameHeader{Seq: 0xFFFFFFFF, TimestampUs: 1}
	payload := bytes.Repeat([]byte{0x5A}, MaxAudioPayload)

	dgram, err := AppendAudioFrame([]byte("prefix"), h, payload)
	if err != nil {
		t.Fatalf("AppendAudioFrame: %v", err)
	}
	dgram = dgram[len("prefix"):]
	if len(dgram) != MaxDatagramSize {
		t.Fatalf("max-payload audio frame is %d bytes, want %d", len(dgram), MaxDatagramSize)
	}

	got, gotPayload, err := ParseAudioFrame(dgram)
	if err != nil {
		t.Fatalf("ParseAudioFrame: %v", err)
	}
	if got != h {
		t.Errorf("header = %+v, want %+v", got, h)
	}
	if !bytes.Equal(gotPayload, payload) {
		t.Errorf("payload mismatch")
	}
	// Alias check, consistent with ParseVideoChunk.
	dgram[AudioFrameHeaderSize] = 0x00
	if gotPayload[0] != 0x00 {
		t.Errorf("payload does not alias the input datagram")
	}
}

func TestAppendAudioFrameErrors(t *testing.T) {
	if _, err := AppendAudioFrame(nil, AudioFrameHeader{}, nil); !errors.Is(err, ErrBadAudioPayload) {
		t.Errorf("empty payload error = %v, want ErrBadAudioPayload", err)
	}
	oversize := bytes.Repeat([]byte{1}, MaxAudioPayload+1)
	if _, err := AppendAudioFrame(nil, AudioFrameHeader{}, oversize); !errors.Is(err, ErrBadAudioPayload) {
		t.Errorf("oversize payload error = %v, want ErrBadAudioPayload", err)
	}
}

func TestParseAudioFrameErrors(t *testing.T) {
	valid := mustHex(t, goldenAudioFrameHex)

	short := valid[:AudioFrameHeaderSize-1]
	badVersion := append([]byte(nil), valid...)
	badVersion[0] = 0x02
	badType := append([]byte(nil), valid...)
	badType[1] = TypeVideoChunk
	empty := valid[:AudioFrameHeaderSize]

	cases := []struct {
		name  string
		dgram []byte
		want  error
	}{
		{"short", short, ErrShortDatagram},
		{"bad version", badVersion, ErrBadVersion},
		{"bad type", badType, ErrBadType},
		{"empty payload", empty, ErrBadAudioPayload},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := ParseAudioFrame(tc.dgram); !errors.Is(err, tc.want) {
				t.Errorf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestAppendAudioConfigErrors(t *testing.T) {
	if _, err := AppendAudioConfig(nil, AudioConfig{SampleRate: 48000, Channels: 2}); !errors.Is(err, ErrBadCodec) {
		t.Errorf("empty codec error = %v, want ErrBadCodec", err)
	}
	long := AudioConfig{Codec: strings.Repeat("x", 256), SampleRate: 48000, Channels: 2}
	if _, err := AppendAudioConfig(nil, long); !errors.Is(err, ErrBadCodec) {
		t.Errorf("long codec error = %v, want ErrBadCodec", err)
	}
	if _, err := AppendAudioConfig(nil, AudioConfig{Codec: "opus", Channels: 2}); !errors.Is(err, ErrBadAudioConfig) {
		t.Errorf("zero sample rate error = %v, want ErrBadAudioConfig", err)
	}
	if _, err := AppendAudioConfig(nil, AudioConfig{Codec: "opus", SampleRate: 48000}); !errors.Is(err, ErrBadAudioConfig) {
		t.Errorf("zero channels error = %v, want ErrBadAudioConfig", err)
	}
	big := AudioConfig{
		Codec:       "opus",
		SampleRate:  48000,
		Channels:    2,
		Description: bytes.Repeat([]byte{1}, MaxDatagramSize),
	}
	if _, err := AppendAudioConfig(nil, big); !errors.Is(err, ErrDatagramTooLarge) {
		t.Errorf("oversize error = %v, want ErrDatagramTooLarge", err)
	}
}

func TestParseAudioConfigErrors(t *testing.T) {
	valid := mustHex(t, goldenAudioConfigHex)

	badVersion := append([]byte(nil), valid...)
	badVersion[0] = 0x02
	badType := append([]byte(nil), valid...)
	badType[1] = TypeDecoderConfig
	emptyCodec := append([]byte(nil), valid...)
	emptyCodec[3] = 0
	// codecLen that leaves no room for sampleRate+channels.
	overrun := append([]byte(nil), valid...)
	overrun[3] = uint8(len(valid) - 4)
	// Truncated: header + codec but missing the fixed tail.
	truncated := valid[:len(valid)-1]
	zeroRate := mustHex(t, "010800046f70757300000000"+"02")
	zeroChannels := mustHex(t, "010800046f7075730000bb80"+"00")

	cases := []struct {
		name  string
		dgram []byte
		want  error
	}{
		{"empty", nil, ErrShortDatagram},
		{"short", valid[:3], ErrShortDatagram},
		{"bad version", badVersion, ErrBadVersion},
		{"bad type", badType, ErrBadType},
		{"empty codec", emptyCodec, ErrBadCodec},
		{"codec overrun", overrun, ErrBadCodec},
		{"truncated tail", truncated, ErrBadCodec},
		{"zero sample rate", zeroRate, ErrBadAudioConfig},
		{"zero channels", zeroChannels, ErrBadAudioConfig},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseAudioConfig(tc.dgram); !errors.Is(err, tc.want) {
				t.Errorf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

// --- R28 TelemetryHello (0x0D) + session tokens (docs/33 TM1) ---

func TestGoldenTelemetryHello(t *testing.T) {
	want := mustHex(t, goldenTelemetryHelloHex)
	got, err := AppendTelemetryHello(nil, TelemetryHello{
		Enabled:          true,
		ReportIntervalMs: 2000,
		Token:            goldenTelemetryToken,
		BroadcastKey:     goldenTelemetryBroadcastKey,
	})
	if err != nil {
		t.Fatalf("AppendTelemetryHello: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("AppendTelemetryHello = %x, want %x", got, want)
	}
	if len(got) != TelemetryHelloSize {
		t.Errorf("hello is %d bytes, want %d", len(got), TelemetryHelloSize)
	}

	h, err := ParseTelemetryHello(want)
	if err != nil {
		t.Fatalf("ParseTelemetryHello: %v", err)
	}
	if !h.Enabled || h.ReportIntervalMs != 2000 {
		t.Errorf("enabled/interval = %v/%d, want true/2000", h.Enabled, h.ReportIntervalMs)
	}
	if !bytes.Equal(h.Token, goldenTelemetryToken) {
		t.Errorf("token = %x, want %x", h.Token, goldenTelemetryToken)
	}
	if !bytes.Equal(h.BroadcastKey, goldenTelemetryBroadcastKey) {
		t.Errorf("broadcastKey = %x, want %x", h.BroadcastKey, goldenTelemetryBroadcastKey)
	}
}

// A fleet with telemetry off still produces a well-formed message; the client
// reads enabled=0 and collects nothing. Same bytes as a relay predating R28
// producing nothing at all, in observable client behaviour.
func TestGoldenTelemetryHelloDisabled(t *testing.T) {
	want := mustHex(t, goldenTelemetryHelloDisabledHex)
	got, err := AppendTelemetryHello(nil, TelemetryHello{
		Token:        make([]byte, TelemetrySessionTokenSize),
		BroadcastKey: make([]byte, TelemetryBroadcastKeySize),
	})
	if err != nil {
		t.Fatalf("AppendTelemetryHello: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("AppendTelemetryHello disabled = %x, want %x", got, want)
	}
	h, err := ParseTelemetryHello(want)
	if err != nil {
		t.Fatalf("ParseTelemetryHello: %v", err)
	}
	if h.Enabled {
		t.Error("Enabled = true, want false")
	}
}

func TestAppendTelemetryHelloRejectsWrongFieldSizes(t *testing.T) {
	good := TelemetryHello{
		Token:        make([]byte, TelemetrySessionTokenSize),
		BroadcastKey: make([]byte, TelemetryBroadcastKeySize),
	}
	shortToken := good
	shortToken.Token = make([]byte, TelemetrySessionTokenSize-1)
	longKey := good
	longKey.BroadcastKey = make([]byte, TelemetryBroadcastKeySize+1)
	for name, h := range map[string]TelemetryHello{"short token": shortToken, "long key": longKey} {
		t.Run(name, func(t *testing.T) {
			if _, err := AppendTelemetryHello(nil, h); !errors.Is(err, ErrBadTelemetryHello) {
				t.Errorf("error = %v, want ErrBadTelemetryHello", err)
			}
		})
	}
}

func TestParseTelemetryHelloStrict(t *testing.T) {
	valid := mustHex(t, goldenTelemetryHelloHex)

	badVersion := append([]byte(nil), valid...)
	badVersion[0] = 0x02
	badType := append([]byte(nil), valid...)
	badType[1] = TypeDeliveryAck
	// A set reserved bit means a field this build would misread; strict parse
	// rejects rather than masking it away.
	reservedFlag := append([]byte(nil), valid...)
	reservedFlag[2] = 0x02

	cases := []struct {
		name string
		msg  []byte
		want error
	}{
		{"empty", nil, ErrBadLength},
		{"short", valid[:TelemetryHelloSize-1], ErrBadLength},
		{"long", append(append([]byte(nil), valid...), 0x00), ErrBadLength},
		{"bad version", badVersion, ErrBadVersion},
		{"bad type", badType, ErrBadType},
		{"reserved flag bit", reservedFlag, ErrBadTelemetryHello},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseTelemetryHello(tc.msg); !errors.Is(err, tc.want) {
				t.Errorf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

// The token's construction, pinned independently of the message framing.
// Deterministic inputs: a fixed key, a fixed nonce, a fixed mint time.
func TestGoldenTelemetrySessionToken(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, TelemetryKeySize)
	nonce := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}
	// 2026-07-26T00:00:00Z; +24h expiry lands on unix hour 0x000856F8.
	mintedAt := time.Unix(1785715200, 0).UTC()

	token, err := mintTelemetrySessionToken(key, goldenTelemetryBroadcastKey, TelemetryRoleViewer, mintedAt, nonce)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if got, want := hex.EncodeToString(token), goldenTelemetrySessionTokenHex; got != want {
		t.Errorf("token = %s, want %s", got, want)
	}
	if len(token) != TelemetrySessionTokenSize {
		t.Fatalf("token is %d bytes, want %d", len(token), TelemetrySessionTokenSize)
	}
	// The sessionId is the nonce, and it is the ONLY part that is ever stored.
	sid, err := TelemetrySessionID(token)
	if err != nil {
		t.Fatalf("TelemetrySessionID: %v", err)
	}
	if sid != hex.EncodeToString(nonce) || len(sid) != TelemetrySessionIDLen {
		t.Errorf("sessionId = %q (%d chars), want %q (%d)", sid, len(sid), hex.EncodeToString(nonce), TelemetrySessionIDLen)
	}
}

func TestTelemetryTokenMintVerifyRoundTrip(t *testing.T) {
	key := bytes.Repeat([]byte{0x11}, TelemetryKeySize)
	now := time.Unix(1785715200, 0).UTC()
	for _, role := range []TelemetryRole{TelemetryRoleViewer, TelemetryRoleBroadcaster} {
		token, err := MintTelemetrySessionToken(key, goldenTelemetryBroadcastKey, role, now)
		if err != nil {
			t.Fatalf("mint %s: %v", role, err)
		}
		sid, err := VerifyTelemetrySessionToken(key, token, goldenTelemetryBroadcastKey, role, now)
		if err != nil {
			t.Fatalf("verify %s: %v", role, err)
		}
		unverified, err := TelemetrySessionID(token)
		if err != nil || sid != unverified {
			t.Errorf("verified sessionId %q != %q (err %v)", sid, unverified, err)
		}
	}
}

// Two mints of the same session are two different sessions: the nonce is
// random, so sessionIds never collide across a fleet and can't be guessed.
func TestTelemetryTokenNonceIsFresh(t *testing.T) {
	key := bytes.Repeat([]byte{0x11}, TelemetryKeySize)
	now := time.Unix(1785715200, 0).UTC()
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		token, err := MintTelemetrySessionToken(key, goldenTelemetryBroadcastKey, TelemetryRoleViewer, now)
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		sid, _ := TelemetrySessionID(token)
		if seen[sid] {
			t.Fatalf("duplicate sessionId %q", sid)
		}
		seen[sid] = true
	}
}

func TestTelemetryTokenVerifyRejects(t *testing.T) {
	key := bytes.Repeat([]byte{0x11}, TelemetryKeySize)
	otherKey := bytes.Repeat([]byte{0x22}, TelemetryKeySize)
	otherBroadcast := []byte{0xde, 0xad, 0xbe, 0xef, 0x00, 0x01}
	now := time.Unix(1785715200, 0).UTC()
	token, err := MintTelemetrySessionToken(key, goldenTelemetryBroadcastKey, TelemetryRoleViewer, now)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	tampered := append([]byte(nil), token...)
	tampered[TelemetrySessionTokenSize-1] ^= 0xFF

	cases := []struct {
		name         string
		key          []byte
		token        []byte
		broadcastKey []byte
		role         TelemetryRole
		now          time.Time
		want         error
	}{
		// Cross-fleet rejection is what makes the public write surface
		// tolerable: only clients that connected to THIS fleet can ingest.
		{"other fleet key", otherKey, token, goldenTelemetryBroadcastKey, TelemetryRoleViewer, now, ErrTelemetryTokenInvalid},
		{"tampered broadcast key", key, token, otherBroadcast, TelemetryRoleViewer, now, ErrTelemetryTokenInvalid},
		// A viewer's token must not submit broadcaster-shaped records.
		{"tampered role", key, token, goldenTelemetryBroadcastKey, TelemetryRoleBroadcaster, now, ErrTelemetryTokenInvalid},
		{"unknown role", key, token, goldenTelemetryBroadcastKey, TelemetryRole("edge"), now, ErrTelemetryTokenInvalid},
		{"tampered tag", key, tampered, goldenTelemetryBroadcastKey, TelemetryRoleViewer, now, ErrTelemetryTokenInvalid},
		{"short token", key, token[:len(token)-1], goldenTelemetryBroadcastKey, TelemetryRoleViewer, now, ErrTelemetryTokenInvalid},
		{"short key", key[:16], token, goldenTelemetryBroadcastKey, TelemetryRoleViewer, now, ErrTelemetryKey},
		// Expiry is actionable (the client needs a fresh hello), so it is its
		// own error rather than folded into "invalid".
		{"expired", key, token, goldenTelemetryBroadcastKey, TelemetryRoleViewer, now.Add(TelemetryTokenTTL + time.Hour), ErrTelemetryTokenExpired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := VerifyTelemetrySessionToken(tc.key, tc.token, tc.broadcastKey, tc.role, tc.now); !errors.Is(err, tc.want) {
				t.Errorf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

// Inside the TTL a token stays valid right up to its final hour — a long
// broadcast must not lose its telemetry identity mid-session.
func TestTelemetryTokenValidUntilExpiry(t *testing.T) {
	key := bytes.Repeat([]byte{0x11}, TelemetryKeySize)
	now := time.Unix(1785715200, 0).UTC()
	token, err := MintTelemetrySessionToken(key, goldenTelemetryBroadcastKey, TelemetryRoleViewer, now)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	for _, d := range []time.Duration{0, time.Hour, TelemetryTokenTTL - time.Minute, TelemetryTokenTTL} {
		if _, err := VerifyTelemetrySessionToken(key, token, goldenTelemetryBroadcastKey, TelemetryRoleViewer, now.Add(d)); err != nil {
			t.Errorf("verify at +%v: %v", d, err)
		}
	}
}
