package wire

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
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
