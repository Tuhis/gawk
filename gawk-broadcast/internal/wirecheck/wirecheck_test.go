package wirecheck

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/Tuhis/gawk/gawk-server/wire"
)

// The golden vectors from gawk-server/wire/wire_test.go, restated here rather
// than imported: an exported fixture the two sides share could be edited once
// and stay green in both places, which would defeat the purpose. These are the
// bytes the relay and wire.ts already agree on (docs/02).
const (
	goldenVideoChunkHex        = "0101010001020304000500820000005d21dba5f0616263"
	goldenDecoderConfigAVCHex  = "0102000b617663312e3432453032410142e02affe1"
	goldenStreamFrameHeaderHex = "01040100010203040000005d21dba5f00000000600000003"
	goldenTimeSyncRequestHex   = "010500000000000f42400000000000000000"
	goldenClockMappingHex      = "0106000000000016e360"
	// R19 reliable-carrier framing (docs/24 Decision 3). The native
	// broadcaster never sends or receives carriers — they are relay→viewer —
	// but the vector is mirrored in all three wire implementations by rule.
	goldenCarrierPrologueHex = "010a"
	goldenCarrierRecordHex   = "0017" + goldenVideoChunkHex
	// R18 viewer count (docs/23 Decision 2): the relay→publisher push the
	// native broadcaster receives (engine readDatagrams → Stats.ViewerCount).
	goldenViewerCountHex = "010b00000003"
)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad golden hex %q: %v", s, err)
	}
	return b
}

// Every message the native broadcaster sends, encoded from this module and
// compared against the frozen bytes.

func TestGoldenVideoChunk(t *testing.T) {
	got, err := wire.AppendVideoChunk(nil, wire.VideoChunkHeader{
		Keyframe:    true,
		FrameID:     0x01020304,
		ChunkIndex:  5,
		ChunkCount:  130,
		TimestampUs: 0x0000005d21dba5f0,
	}, []byte("abc"))
	if err != nil {
		t.Fatalf("AppendVideoChunk: %v", err)
	}
	if want := mustHex(t, goldenVideoChunkHex); !bytes.Equal(got, want) {
		t.Errorf("VideoChunk bytes drifted from the golden vector\n got %x\nwant %x", got, want)
	}
}

func TestGoldenDecoderConfig(t *testing.T) {
	got, err := wire.AppendDecoderConfig(nil, wire.DecoderConfig{
		Codec:     "avc1.42E02A",
		Extradata: []byte{0x01, 0x42, 0xe0, 0x2a, 0xff, 0xe1},
	})
	if err != nil {
		t.Fatalf("AppendDecoderConfig: %v", err)
	}
	if want := mustHex(t, goldenDecoderConfigAVCHex); !bytes.Equal(got, want) {
		t.Errorf("DecoderConfig bytes drifted from the golden vector\n got %x\nwant %x", got, want)
	}
}

func TestGoldenStreamFrameHeader(t *testing.T) {
	got, err := wire.AppendStreamFrameHeader(nil, wire.StreamFrameHeader{
		Keyframe:    true,
		FrameID:     0x01020304,
		TimestampUs: 0x0000005d21dba5f0,
		ConfigLen:   6,
		PayloadLen:  3,
	})
	if err != nil {
		t.Fatalf("AppendStreamFrameHeader: %v", err)
	}
	if want := mustHex(t, goldenStreamFrameHeaderHex); !bytes.Equal(got, want) {
		t.Errorf("StreamFrame header bytes drifted from the golden vector\n got %x\nwant %x", got, want)
	}
}

func TestGoldenTimeSyncRequest(t *testing.T) {
	got := wire.AppendTimeSync(nil, 1_000_000, 0)
	if want := mustHex(t, goldenTimeSyncRequestHex); !bytes.Equal(got, want) {
		t.Errorf("TimeSync request bytes drifted from the golden vector\n got %x\nwant %x", got, want)
	}
}

func TestGoldenClockMapping(t *testing.T) {
	got := wire.AppendClockMapping(nil, 1_500_000)
	if want := mustHex(t, goldenClockMappingHex); !bytes.Equal(got, want) {
		t.Errorf("ClockMapping bytes drifted from the golden vector\n got %x\nwant %x", got, want)
	}
}

func TestGoldenCarrierPrologueAndRecord(t *testing.T) {
	prologue := wire.AppendCarrierPrologue(nil)
	if want := mustHex(t, goldenCarrierPrologueHex); !bytes.Equal(prologue, want) {
		t.Errorf("carrier prologue drifted from the golden vector\n got %x\nwant %x", prologue, want)
	}
	record, err := wire.AppendCarrierRecord(nil, mustHex(t, goldenVideoChunkHex))
	if err != nil {
		t.Fatalf("AppendCarrierRecord: %v", err)
	}
	if want := mustHex(t, goldenCarrierRecordHex); !bytes.Equal(record, want) {
		t.Errorf("carrier record drifted from the golden vector\n got %x\nwant %x", record, want)
	}
}

// ViewerCount is relay-originated; the native broadcaster only parses it,
// but the vector pins the bytes from this side like every other message.
func TestGoldenViewerCount(t *testing.T) {
	want := mustHex(t, goldenViewerCountHex)
	if got := wire.AppendViewerCount(nil, 3); !bytes.Equal(got, want) {
		t.Errorf("ViewerCount bytes drifted from the golden vector\n got %x\nwant %x", got, want)
	}
	count, err := wire.ParseViewerCount(want)
	if err != nil {
		t.Fatalf("ParseViewerCount: %v", err)
	}
	if count != 3 {
		t.Errorf("count = %d, want 3", count)
	}
}

// The announce is the one message the broadcaster receives.
func TestBroadcastAnnounceRoundTrip(t *testing.T) {
	msg, err := wire.AppendBroadcastAnnounce(nil, "K7M2QP")
	if err != nil {
		t.Fatalf("AppendBroadcastAnnounce: %v", err)
	}
	id, err := wire.ParseBroadcastAnnounce(msg)
	if err != nil {
		t.Fatalf("ParseBroadcastAnnounce: %v", err)
	}
	if id != "K7M2QP" {
		t.Errorf("id = %q, want %q", id, "K7M2QP")
	}
}

// The constants the engine's chunking, buffering and refusal limits are built
// on. A change to any of these is a protocol change, not a tuning knob.
func TestWireConstants(t *testing.T) {
	for _, c := range []struct {
		name string
		got  int
		want int
	}{
		{"MaxDatagramSize", wire.MaxDatagramSize, 1200},
		{"VideoChunkHeaderSize", wire.VideoChunkHeaderSize, 20},
		{"MaxChunkPayload", wire.MaxChunkPayload, 1180},
		{"MaxChunkCount", wire.MaxChunkCount, 3000},
		{"StreamFrameHeaderSize", wire.StreamFrameHeaderSize, 24},
		{"TimeSyncSize", wire.TimeSyncSize, 18},
		{"ClockMappingSize", wire.ClockMappingSize, 10},
		{"MaxKeyframeBytes", wire.MaxKeyframeBytes, 8 << 20},
		{"Version", wire.Version, 0x01},
		{"TypeVideoChunk", wire.TypeVideoChunk, 0x01},
		{"TypeDecoderConfig", wire.TypeDecoderConfig, 0x02},
		{"TypeBroadcastAnnounce", wire.TypeBroadcastAnnounce, 0x03},
		{"TypeStreamFrame", wire.TypeStreamFrame, 0x04},
		{"TypeTimeSync", wire.TypeTimeSync, 0x05},
		{"TypeClockMapping", wire.TypeClockMapping, 0x06},
		{"TypeResumeToken", wire.TypeResumeToken, 0x09},
		{"TypeReliableCarrier", wire.TypeReliableCarrier, 0x0A},
		{"TypeViewerCount", wire.TypeViewerCount, 0x0B},
		{"CarrierPrologueSize", wire.CarrierPrologueSize, 2},
		{"CarrierRecordHeaderSize", wire.CarrierRecordHeaderSize, 2},
		{"ViewerCountSize", wire.ViewerCountSize, 6},
	} {
		if c.got != c.want {
			t.Errorf("wire.%s = %d, want %d — wire format changed; update the relay, both broadcasters and the viewer together", c.name, c.got, c.want)
		}
	}
}

// The empty-extradata DecoderConfig is the native broadcaster's whole interop
// story (docs/19): it emits raw Annex-B, so it never builds an avcC record and
// the viewer's isAnnexB sniff routes around its extradata correction. If the
// relay ever rejects an empty extradata, that path breaks silently — in the
// viewer, not here — so pin it from this side.
func TestEmptyExtradataIsAccepted(t *testing.T) {
	dgram, err := wire.AppendDecoderConfig(nil, wire.DecoderConfig{Codec: "avc1.42E02A"})
	if err != nil {
		t.Fatalf("AppendDecoderConfig with empty extradata: %v", err)
	}
	cfg, err := wire.ParseDecoderConfig(dgram)
	if err != nil {
		t.Fatalf("ParseDecoderConfig: %v", err)
	}
	if cfg.Codec != "avc1.42E02A" {
		t.Errorf("codec = %q, want %q", cfg.Codec, "avc1.42E02A")
	}
	if len(cfg.Extradata) != 0 {
		t.Errorf("extradata = %x, want empty", cfg.Extradata)
	}
}
