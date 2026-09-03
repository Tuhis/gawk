package wirecheck

import (
	"bytes"
	"encoding/hex"
	"errors"
	"reflect"
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
	// The length prefix of a record at the inclusive upper boundary: a full
	// delta chunk — which this module produces — is exactly MaxDatagramSize
	// (1200 = 0x04B0).
	goldenCarrierMaxRecordPrefixHex = "04b0"
	// R18 viewer count (docs/23 Decision 2): the relay→publisher push the
	// native broadcaster receives (engine readDatagrams → Stats.ViewerCount).
	goldenViewerCountHex = "010b00000003"
	// R15 system audio (docs/20 Decision 2). The native broadcaster does not
	// send audio yet — that is an explicit R15 non-goal and an R14 follow-up —
	// but the vectors are mirrored in all three wire implementations by rule,
	// exactly like the carrier framing above, so the format can't move under
	// this module unnoticed.
	goldenAudioFrameHex  = "01070000010203040000005d21dba5f0616263"
	goldenAudioConfigHex = "010800046f7075730000bb8002"
	// R28 telemetry hello (docs/33 §4.1). The native broadcaster DOES receive
	// this one — it is a publisher, and the relay hands it a telemetry
	// identity like any other client — so unlike the carrier/audio vectors
	// above this is coupling on a message this module actually parses.
	goldenTelemetryHelloHex = "010d0107d000012345000102030405060708090a0ba1a2a3a4a5a6a7a81a2b3c4d5e6f"
	// R29 forward parity (docs/34). This module DOES send parity chunks — it
	// is a producer — and it DOES parse RelayCapabilities, which is what gates
	// whether it sends them at all. So unlike the carrier/audio vectors above,
	// both of these are coupling on messages this module actually uses.
	goldenParityChunkHex       = "010e01020304010009000021c0deadbeef"
	goldenRelayCapabilitiesHex = "010f000102"
	// R30 striped delivery (docs/35). This module neither sends nor receives
	// StripeState — it is viewer↔relay — but the capabilities flags word is
	// the one field this producer parses that R30 extends, so the both-bits
	// vector pins that the R29-era strict 5-byte parse survives the new bit
	// (the "new bits, never new bytes" rule, docs/35 §5.3). The StripeState
	// vectors keep the shared wire package honest across all three mirrors.
	goldenStripeStateStripedHex   = "0110010300"
	goldenStripeStateUnstripedHex = "0110000000"
	goldenCapabilitiesBothBitsHex = "010f000302"
	// R37 (docs/40 SP4). RelayIdentity (0x11) is echo-route relay→client — the
	// probe's identity message, which this module's GUI picker parses via the
	// shared wire package. TelemetryEndpoint (0x12) this module's publisher
	// sessions actually receive: it is what repoints the R28 reporter on a
	// foreign relay (docs/40 §4.10), so like the hello above this is coupling
	// on a message this module really uses.
	goldenRelayIdentityHex       = "01110006312e34322e30096761776b20686f6d65"
	goldenRelayIdentityNoNameHex = "01110006312e34322e3000"
	goldenTelemetryEndpointHex   = "011200003068747470733a2f2f6761776b2e6578616d706c652e636f6d2f6170692f74656c656d657472792f76312f696e67657374"
	// R42 rooms (docs/44 §4.6, last bullet). The native broadcaster is a room
	// client at runtime: it SENDS RoomHello and the Attach/Detach commands and
	// PARSES RoomState and RoomEvent, so most of these are coupling on
	// messages this module really uses. The ones it never touches (the
	// viewer-side commands, the web-only events) ride along by the same rule
	// as the carrier/audio vectors above: every vector, every mirror.
	//
	// RoomHello: this module's first record on the room stream (protocol 1,
	// clientKind web-broadcaster in the vector; native sends kind 2).
	goldenRoomHelloHex = "0113010100057475686973"
	// The uint16 length prefix every room record wears; framed by this module
	// on send and stripped by it on receive.
	goldenRoomRecordHex = "000b" + goldenRoomHelloHex
	// RoomState: the snapshot this module parses on join and after Resync —
	// dynamic (creator token present, one live attachment) and static (display
	// name, no token) shapes.
	goldenRoomStateDynamicHex = "0114" + "03" + "00" + "00000007" + "0001" +
		"06355550345857" + "00" +
		"10000102030405060708090a0b0c0d0e0f" +
		"01" + "06414243444546" + "057475686973" + "01" + "00000003" +
		"0001" + "0001" + "01" + "02" + "057475686973" + "00"
	goldenRoomStateStaticHex = "0114" + "04" + "00" + "00000000" + "0002" +
		"095475686973526f6f6d" + "0b54756869732720726f6f6d" + "00" + "00" +
		"0001" + "0002" + "00" + "00" + "06766965776572" + "00"
	// RoomEvent: parsed by this module's room supervisor. ParticipantJoined/
	// Left drive the participant list; AttachmentUpdated/Removed are how it
	// learns its own attachment aged out; RoomEnding precedes close 4007;
	// CommandRejected is the answer to a refused Attach.
	goldenRoomEventJoinedHex            = "011500000008" + "01" + "0003" + "02" + "02" + "027063" + "00"
	goldenRoomEventLeftHex              = "011500000009" + "02" + "0003"
	goldenRoomEventAttachmentUpdatedHex = "01150000000d" + "12" + "06414243444546" + "057475686973" + "00" + "0000000c"
	goldenRoomEventAttachmentRemovedHex = "01150000000a" + "11" + "06414243444546" + "02"
	goldenRoomEventEndingHex            = "01150000000b" + "20" + "02"
	goldenRoomEventRejectedHex          = "01150000000c" + "30" + "01" + "01" + "09726f6f6d2066756c6c"
	// RoomCommand: Attach (with this module's own resume token) and Detach are
	// the two it sends; SetNickname/EndRoom/Resync are mirrored by rule.
	goldenRoomCommandAttachHex = "0116" + "01" + "06414243444546" +
		"10a0a1a2a3a4a5a6a7a8a9aaabacadaeaf" + "057475686973"
	goldenRoomCommandDetachHex = "0116" + "02" + "06414243444546"
	goldenRoomCommandNickHex   = "0116" + "03" + "057475686973"
	goldenRoomCommandEndHex    = "011604"
	goldenRoomCommandResyncHex = "011605"
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

// The inclusive upper boundary of the carrier record's uint16 length prefix.
// The carrier is relay→viewer, but the delta chunks it frames are the ones
// *this* module puts on the wire, and a full one is exactly MaxDatagramSize —
// so the boundary is mirrored here like every other carrier vector
// (docs/24 finding 15).
func TestCarrierRecordAtMaxDatagramSize(t *testing.T) {
	h := wire.VideoChunkHeader{FrameID: 43, ChunkIndex: 1, ChunkCount: 2, TimestampUs: 7654321}
	dgram, err := wire.AppendVideoChunk(nil, h, bytes.Repeat([]byte{0xAB}, wire.MaxChunkPayload))
	if err != nil {
		t.Fatalf("AppendVideoChunk: %v", err)
	}
	if len(dgram) != wire.MaxDatagramSize {
		t.Fatalf("full delta chunk is %d bytes, want %d", len(dgram), wire.MaxDatagramSize)
	}

	record, err := wire.AppendCarrierRecord(nil, dgram)
	if err != nil {
		t.Fatalf("AppendCarrierRecord(max size): %v", err)
	}
	if want := wire.CarrierRecordHeaderSize + wire.MaxDatagramSize; len(record) != want {
		t.Fatalf("record = %d bytes, want %d", len(record), want)
	}
	if got := hex.EncodeToString(record[:wire.CarrierRecordHeaderSize]); got != goldenCarrierMaxRecordPrefixHex {
		t.Errorf("max-size length prefix drifted from the golden vector\n got %s\nwant %s", got, goldenCarrierMaxRecordPrefixHex)
	}

	got, rest, err := wire.ParseCarrierRecord(record)
	if err != nil {
		t.Fatalf("ParseCarrierRecord(max size): %v", err)
	}
	if !bytes.Equal(got, dgram) {
		t.Errorf("max-size record = %d bytes, want the %d-byte datagram verbatim", len(got), len(dgram))
	}
	if len(rest) != 0 {
		t.Errorf("rest = %x, want empty", rest)
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
		// R21: the broadcaster never receives a DeliveryAck (it is relay→viewer
		// only), but the type byte is pinned here anyway — this table is the
		// allocation map, and a silent collision is exactly what it exists to
		// prevent.
		{"TypeDeliveryAck", wire.TypeDeliveryAck, 0x0C},
		{"DeliveryAckSize", wire.DeliveryAckSize, 5},
		{"TypeAudioFrame", wire.TypeAudioFrame, 0x07},
		{"TypeAudioConfig", wire.TypeAudioConfig, 0x08},
		{"TypeTelemetryHello", wire.TypeTelemetryHello, 0x0D},
		{"TypeParityChunk", wire.TypeParityChunk, 0x0E},
		{"TypeRelayCapabilities", wire.TypeRelayCapabilities, 0x0F},
		{"TypeStripeState", wire.TypeStripeState, 0x10},
		// R37 (docs/40 SP4): the probe's identity message and the telemetry
		// endpoint advertisement, with the bounds their parsers enforce.
		{"TypeRelayIdentity", wire.TypeRelayIdentity, 0x11},
		{"TypeTelemetryEndpoint", wire.TypeTelemetryEndpoint, 0x12},
		{"MaxRelayIdentityVersionLen", wire.MaxRelayIdentityVersionLen, 32},
		{"MaxRelayIdentityNameLen", wire.MaxRelayIdentityNameLen, 64},
		{"MaxTelemetryEndpointURLLen", wire.MaxTelemetryEndpointURLLen, 512},
		{"CarrierPrologueSize", wire.CarrierPrologueSize, 2},
		{"CarrierRecordHeaderSize", wire.CarrierRecordHeaderSize, 2},
		{"ViewerCountSize", wire.ViewerCountSize, 6},
		{"AudioFrameHeaderSize", wire.AudioFrameHeaderSize, 16},
		{"MaxAudioPayload", wire.MaxAudioPayload, 1184},
		// R42 rooms (docs/44 D15): the four room control types this module
		// speaks on the room stream, plus the bounds its own hello/commands
		// must respect and its parsers enforce.
		{"TypeRoomHello", wire.TypeRoomHello, 0x13},
		{"TypeRoomState", wire.TypeRoomState, 0x14},
		{"TypeRoomEvent", wire.TypeRoomEvent, 0x15},
		{"TypeRoomCommand", wire.TypeRoomCommand, 0x16},
		{"RoomProtocolVersion", wire.RoomProtocolVersion, 1},
		{"RoomRecordHeaderSize", wire.RoomRecordHeaderSize, 2},
		{"MaxRoomRecordSize", wire.MaxRoomRecordSize, 16384},
		{"MaxRoomNicknameLen", wire.MaxRoomNicknameLen, 32},
		{"MaxRoomCodeLen", wire.MaxRoomCodeLen, 32},
		{"MaxRoomDisplayNameLen", wire.MaxRoomDisplayNameLen, 64},
		{"MaxRoomLabelLen", wire.MaxRoomLabelLen, 32},
		{"MaxRoomIdentityLen", wire.MaxRoomIdentityLen, 64},
		{"MaxRoomRejectMessageLen", wire.MaxRoomRejectMessageLen, 128},
		{"RoomCreatorTokenSize", wire.RoomCreatorTokenSize, 16},
		{"ResumeTokenSize", wire.ResumeTokenSize, 16},
	} {
		if c.got != c.want {
			t.Errorf("wire.%s = %d, want %d — wire format changed; update the relay, both broadcasters and the viewer together", c.name, c.got, c.want)
		}
	}
}

// The WebTransport application close codes, restated from this side exactly
// like the golden vectors above.
//
// These are not decoration for this module: the engine's resume supervisor
// splits them into terminal and resumable (internal/engine/resume.go), so a
// silently renumbered code would not fail a build — it would make this
// broadcaster auto-resume into a broadcast the relay meant to end, or give up
// on one it meant to keep. Renumbering is never the right fix; allocate the
// next code in gawk-server/wire/wire.go and mirror it into all four
// implementations.
func TestCloseCodeParity(t *testing.T) {
	for _, c := range []struct {
		name string
		got  int
		want int
	}{
		{"CloseCodeBroadcastEnded", wire.CloseCodeBroadcastEnded, 4000},
		{"CloseCodeSubscriberUnresponsive", wire.CloseCodeSubscriberUnresponsive, 4001},
		{"CloseCodeServerDraining", wire.CloseCodeServerDraining, 4002},
		{"CloseCodeOriginMoved", wire.CloseCodeOriginMoved, 4003},
		{"CloseCodePublisherSuperseded", wire.CloseCodePublisherSuperseded, 4004},
		{"CloseCodeStripeLegOrphaned", wire.CloseCodeStripeLegOrphaned, 4005},
		// R39 (docs/42 §4.4): the operator terminated the broadcast. Terminal
		// for this publisher — see terminalForPublisher in the engine.
		{"CloseCodeTerminatedByOperator", wire.CloseCodeTerminatedByOperator, 4006},
		// R42 (docs/44 D15): the room ended. Sent on the room session, not
		// the publish session — this module's broadcast survives it.
		{"CloseCodeRoomEnded", wire.CloseCodeRoomEnded, 4007},
	} {
		if c.got != c.want {
			t.Errorf("wire.%s = %d, want %d — close codes are wire-visible; update the relay, both broadcasters and the viewer together", c.name, c.got, c.want)
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

// R15 audio (docs/20 Decision 2): not sent by this module today, pinned from
// this side anyway — see the constant's comment.
func TestGoldenAudioFrame(t *testing.T) {
	got, err := wire.AppendAudioFrame(nil, wire.AudioFrameHeader{
		Seq:         0x01020304,
		TimestampUs: 0x0000005d21dba5f0,
	}, []byte("abc"))
	if err != nil {
		t.Fatalf("AppendAudioFrame: %v", err)
	}
	if want := mustHex(t, goldenAudioFrameHex); !bytes.Equal(got, want) {
		t.Errorf("AudioFrame bytes drifted from the golden vector\n got %x\nwant %x", got, want)
	}
}

func TestGoldenAudioConfig(t *testing.T) {
	got, err := wire.AppendAudioConfig(nil, wire.AudioConfig{
		Codec:      "opus",
		SampleRate: 48000,
		Channels:   2,
	})
	if err != nil {
		t.Fatalf("AppendAudioConfig: %v", err)
	}
	if want := mustHex(t, goldenAudioConfigHex); !bytes.Equal(got, want) {
		t.Errorf("AudioConfig bytes drifted from the golden vector\n got %x\nwant %x", got, want)
	}
}

// R28 (docs/33 §4.1): the telemetry hello the relay sends this module's
// publisher sessions. Parsed, never sent — asserted from the consumer side.
func TestGoldenTelemetryHello(t *testing.T) {
	want := mustHex(t, goldenTelemetryHelloHex)
	got, err := wire.AppendTelemetryHello(nil, wire.TelemetryHello{
		Enabled:          true,
		ReportIntervalMs: 2000,
		Token: []byte{
			0x00, 0x01, 0x23, 0x45,
			0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b,
			0xa1, 0xa2, 0xa3, 0xa4, 0xa5, 0xa6, 0xa7, 0xa8,
		},
		BroadcastKey: []byte{0x1a, 0x2b, 0x3c, 0x4d, 0x5e, 0x6f},
	})
	if err != nil {
		t.Fatalf("AppendTelemetryHello: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("TelemetryHello bytes drifted from the golden vector\n got %x\nwant %x", got, want)
	}

	h, err := wire.ParseTelemetryHello(want)
	if err != nil {
		t.Fatalf("ParseTelemetryHello: %v", err)
	}
	if !h.Enabled || h.ReportIntervalMs != 2000 {
		t.Errorf("enabled/interval = %v/%d, want true/2000", h.Enabled, h.ReportIntervalMs)
	}
	if hex.EncodeToString(h.BroadcastKey) != "1a2b3c4d5e6f" {
		t.Errorf("broadcastKey = %x, want 1a2b3c4d5e6f", h.BroadcastKey)
	}
}

// --- R29 forward parity ----------------------------------------------------

func TestGoldenParityChunk(t *testing.T) {
	got, err := wire.AppendParityChunk(nil, wire.ParityChunkHeader{
		FrameID:     0x01020304,
		ParityIndex: 1,
		ChunkCount:  9,
		FrameBytes:  8640,
	}, []byte{0xde, 0xad, 0xbe, 0xef})
	if err != nil {
		t.Fatalf("AppendParityChunk: %v", err)
	}
	if want := mustHex(t, goldenParityChunkHex); !bytes.Equal(got, want) {
		t.Errorf("ParityChunk bytes drifted from the golden vector\n got %x\nwant %x", got, want)
	}
}

func TestGoldenRelayCapabilities(t *testing.T) {
	got, err := wire.AppendRelayCapabilities(nil, wire.RelayCapabilities{
		Flags:       wire.CapParityChunks,
		ParityLevel: 2,
	})
	if err != nil {
		t.Fatalf("AppendRelayCapabilities: %v", err)
	}
	if want := mustHex(t, goldenRelayCapabilitiesHex); !bytes.Equal(got, want) {
		t.Errorf("RelayCapabilities bytes drifted from the golden vector\n got %x\nwant %x", got, want)
	}
	// The producer reads this message; a parse regression would silently
	// disable parity fleet-wide rather than fail loudly.
	back, err := wire.ParseRelayCapabilities(got)
	if err != nil {
		t.Fatalf("ParseRelayCapabilities: %v", err)
	}
	if back.ParityLevel != 2 || back.Flags&wire.CapParityChunks == 0 {
		t.Errorf("round trip = %+v, want parity level 2 with CapParityChunks set", back)
	}
}

func TestGoldenStripeState(t *testing.T) {
	striped, err := wire.AppendStripeState(nil, wire.StripeState{Striped: true, StripeN: 3})
	if err != nil {
		t.Fatalf("AppendStripeState(striped): %v", err)
	}
	if want := mustHex(t, goldenStripeStateStripedHex); !bytes.Equal(striped, want) {
		t.Errorf("StripeState(striped) bytes drifted from the golden vector\n got %x\nwant %x", striped, want)
	}
	unstriped, err := wire.AppendStripeState(nil, wire.StripeState{})
	if err != nil {
		t.Fatalf("AppendStripeState(unstriped): %v", err)
	}
	if want := mustHex(t, goldenStripeStateUnstripedHex); !bytes.Equal(unstriped, want) {
		t.Errorf("StripeState(unstriped) bytes drifted from the golden vector\n got %x\nwant %x", unstriped, want)
	}
}

// TestCapabilitiesSurviveStripedBit is this producer's stake in R30: the one
// message it parses gains a flag bit and must stay 5 bytes, or the R29-era
// strict parse in every deployed native broadcaster breaks mid-skew.
func TestCapabilitiesSurviveStripedBit(t *testing.T) {
	got, err := wire.AppendRelayCapabilities(nil, wire.RelayCapabilities{
		Flags:       wire.CapParityChunks | wire.CapStripedDelivery,
		ParityLevel: 2,
	})
	if err != nil {
		t.Fatalf("AppendRelayCapabilities: %v", err)
	}
	if want := mustHex(t, goldenCapabilitiesBothBitsHex); !bytes.Equal(got, want) {
		t.Errorf("RelayCapabilities(both bits) drifted from the golden vector\n got %x\nwant %x", got, want)
	}
	back, err := wire.ParseRelayCapabilities(got)
	if err != nil {
		t.Fatalf("ParseRelayCapabilities with CapStripedDelivery set: %v", err)
	}
	if back.Flags&wire.CapParityChunks == 0 {
		t.Errorf("CapParityChunks lost when CapStripedDelivery is set: %+v", back)
	}
}

// --- R37 relay identity + telemetry endpoint -------------------------------

// RelayIdentity is relay-originated (echo route); this module parses it in the
// GUI's server probe. Both vectors — named and unset-name — are pinned, plus
// the one deliberate parser deviation both R37 messages share: trailing bytes
// are TOLERATED (the reserved extension space, docs/40 §4.9), unlike every
// strict exact-length parser before them.
func TestGoldenRelayIdentity(t *testing.T) {
	want := mustHex(t, goldenRelayIdentityHex)
	got, err := wire.AppendRelayIdentity(nil, wire.RelayIdentity{ServerVersion: "1.42.0", Name: "gawk home"})
	if err != nil {
		t.Fatalf("AppendRelayIdentity: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("RelayIdentity bytes drifted from the golden vector\n got %x\nwant %x", got, want)
	}
	id, err := wire.ParseRelayIdentity(want)
	if err != nil {
		t.Fatalf("ParseRelayIdentity: %v", err)
	}
	if id.ServerVersion != "1.42.0" || id.Name != "gawk home" {
		t.Errorf("parsed identity = %+v, want 1.42.0 / gawk home", id)
	}

	wantNoName := mustHex(t, goldenRelayIdentityNoNameHex)
	gotNoName, err := wire.AppendRelayIdentity(nil, wire.RelayIdentity{ServerVersion: "1.42.0"})
	if err != nil {
		t.Fatalf("AppendRelayIdentity(no name): %v", err)
	}
	if !bytes.Equal(gotNoName, wantNoName) {
		t.Errorf("RelayIdentity(no name) bytes drifted from the golden vector\n got %x\nwant %x", gotNoName, wantNoName)
	}
	if id, err := wire.ParseRelayIdentity(wantNoName); err != nil || id.Name != "" {
		t.Errorf("ParseRelayIdentity(no name) = %+v, %v, want an empty name", id, err)
	}

	// The forward-compat stance a strict parser here would break: bytes past
	// the name are the extension space a managed relay will someday use.
	trailing := append(append([]byte(nil), want...), 0xa1, 0xb2, 0xc3)
	if id, err := wire.ParseRelayIdentity(trailing); err != nil || id.ServerVersion != "1.42.0" || id.Name != "gawk home" {
		t.Errorf("trailing extension bytes broke the parse: %+v, %v", id, err)
	}
}

// TelemetryEndpoint is what makes a foreign relay's telemetry land somewhere
// its token can verify (docs/40 §4.10) — this producer receives it on its
// publish sessions and repoints the R28 reporter with it.
func TestGoldenTelemetryEndpoint(t *testing.T) {
	const url = "https://gawk.example.com/api/telemetry/v1/ingest"
	want := mustHex(t, goldenTelemetryEndpointHex)
	got, err := wire.AppendTelemetryEndpoint(nil, url)
	if err != nil {
		t.Fatalf("AppendTelemetryEndpoint: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("TelemetryEndpoint bytes drifted from the golden vector\n got %x\nwant %x", got, want)
	}
	parsed, err := wire.ParseTelemetryEndpoint(want)
	if err != nil {
		t.Fatalf("ParseTelemetryEndpoint: %v", err)
	}
	if parsed != url {
		t.Errorf("parsed URL = %q, want %q", parsed, url)
	}
	// Same trailing-bytes contract as RelayIdentity.
	if parsed, err := wire.ParseTelemetryEndpoint(append(append([]byte(nil), want...), 0x7f)); err != nil || parsed != url {
		t.Errorf("trailing extension bytes broke the parse: %q, %v", parsed, err)
	}
}

// TestParityFullPayloadFitsDatagram pins the sizing rule this module depends
// on: a full delta chunk is MaxChunkPayload, so its parity symbol is too, and
// the resulting datagram must still fit MaxDatagramSize. This is why the
// parity header is 13 bytes and not 20 (docs/34 §4.2).
func TestParityFullPayloadFitsDatagram(t *testing.T) {
	got, err := wire.AppendParityChunk(nil, wire.ParityChunkHeader{
		FrameID:    1,
		ChunkCount: 9,
		FrameBytes: 9 * wire.MaxChunkPayload,
	}, bytes.Repeat([]byte{0x5a}, wire.MaxChunkPayload))
	if err != nil {
		t.Fatalf("AppendParityChunk: %v", err)
	}
	if len(got) > wire.MaxDatagramSize {
		t.Errorf("full-payload parity datagram is %d bytes, exceeds MaxDatagramSize %d", len(got), wire.MaxDatagramSize)
	}
}

// --- R42 rooms -------------------------------------------------------------

// The expected structs, restated like the hex: the same values room_test.go
// holds, never imported from it.
var (
	goldenRoomHello = wire.RoomHello{Protocol: 1, ClientKind: wire.RoomClientWebBroadcaster, Nickname: "tuhis"}

	goldenRoomCreatorToken = []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	goldenRoomResumeToken  = []byte{0xa0, 0xa1, 0xa2, 0xa3, 0xa4, 0xa5, 0xa6, 0xa7, 0xa8, 0xa9, 0xaa, 0xab, 0xac, 0xad, 0xae, 0xaf}

	goldenRoomStateDynamic = wire.RoomState{
		Flags:        wire.RoomStateFlagDynamic | wire.RoomStateFlagCreator,
		Seq:          7,
		YourID:       1,
		Code:         "5UP4XW",
		CreatorToken: goldenRoomCreatorToken,
		Attachments:  []wire.RoomAttachment{{BroadcastID: "ABCDEF", Label: "tuhis", Live: true, ViewerCount: 3}},
		Participants: []wire.RoomParticipant{{ID: 1, Kind: wire.RoomClientWebBroadcaster, Flags: wire.RoomParticipantFlagStreaming, Nickname: "tuhis"}},
	}
	goldenRoomStateStatic = wire.RoomState{
		Flags:        wire.RoomStateFlagAttachOK,
		YourID:       2,
		Code:         "TuhisRoom",
		DisplayName:  "Tuhis' room",
		Participants: []wire.RoomParticipant{{ID: 2, Kind: wire.RoomClientWebViewer, Nickname: "viewer"}},
	}
)

// RoomHello is the first record this module writes on a room stream.
func TestGoldenRoomHello(t *testing.T) {
	want := mustHex(t, goldenRoomHelloHex)
	got, err := wire.AppendRoomHello(nil, goldenRoomHello)
	if err != nil {
		t.Fatalf("AppendRoomHello: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("RoomHello bytes drifted from the golden vector\n got %x\nwant %x", got, want)
	}
	h, err := wire.ParseRoomHello(want)
	if err != nil {
		t.Fatalf("ParseRoomHello: %v", err)
	}
	if h != goldenRoomHello {
		t.Errorf("ParseRoomHello = %+v, want %+v", h, goldenRoomHello)
	}
}

// The record framing this module applies on send and strips on receive.
func TestGoldenRoomRecord(t *testing.T) {
	want := mustHex(t, goldenRoomRecordHex)
	got, err := wire.AppendRoomRecord(nil, mustHex(t, goldenRoomHelloHex))
	if err != nil {
		t.Fatalf("AppendRoomRecord: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("room record drifted from the golden vector\n got %x\nwant %x", got, want)
	}
	n, err := wire.ParseRoomRecordLength(want)
	if err != nil {
		t.Fatalf("ParseRoomRecordLength: %v", err)
	}
	if n != 11 {
		t.Errorf("ParseRoomRecordLength = %d, want 11", n)
	}
}

// RoomState is parsed by this module on join and after every Resync.
func TestGoldenRoomState(t *testing.T) {
	for _, tc := range []struct {
		name string
		hex  string
		want wire.RoomState
	}{
		{"dynamic", goldenRoomStateDynamicHex, goldenRoomStateDynamic},
		{"static", goldenRoomStateStaticHex, goldenRoomStateStatic},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := mustHex(t, tc.hex)
			got, err := wire.AppendRoomState(nil, tc.want)
			if err != nil {
				t.Fatalf("AppendRoomState: %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("RoomState bytes drifted from the golden vector\n got %x\nwant %x", got, want)
			}
			s, err := wire.ParseRoomState(want)
			if err != nil {
				t.Fatalf("ParseRoomState: %v", err)
			}
			if !reflect.DeepEqual(s, tc.want) {
				t.Errorf("ParseRoomState = %+v, want %+v", s, tc.want)
			}
		})
	}
}

// RoomEvent is the relay→client stream this module's room supervisor reads.
func TestGoldenRoomEvents(t *testing.T) {
	for _, tc := range []struct {
		name string
		hex  string
		want wire.RoomEvent
	}{
		{"joined", goldenRoomEventJoinedHex, wire.RoomEvent{Seq: 8, Kind: wire.RoomEventParticipantJoined,
			Participant: wire.RoomParticipant{ID: 3, Kind: wire.RoomClientNative, Flags: wire.RoomParticipantFlagStreaming, Nickname: "pc"}}},
		{"left", goldenRoomEventLeftHex, wire.RoomEvent{Seq: 9, Kind: wire.RoomEventParticipantLeft, Participant: wire.RoomParticipant{ID: 3}}},
		{"attachment updated", goldenRoomEventAttachmentUpdatedHex, wire.RoomEvent{Seq: 13, Kind: wire.RoomEventAttachmentUpdated,
			Attachment: wire.RoomAttachment{BroadcastID: "ABCDEF", Label: "tuhis", ViewerCount: 12}}},
		{"attachment removed", goldenRoomEventAttachmentRemovedHex, wire.RoomEvent{Seq: 10, Kind: wire.RoomEventAttachmentRemoved,
			Attachment: wire.RoomAttachment{BroadcastID: "ABCDEF"}, Reason: wire.RoomDetachReasonExpired}},
		{"ending", goldenRoomEventEndingHex, wire.RoomEvent{Seq: 11, Kind: wire.RoomEventRoomEnding, Reason: wire.RoomEndReasonCreator}},
		{"rejected", goldenRoomEventRejectedHex, wire.RoomEvent{Seq: 12, Kind: wire.RoomEventCommandRejected,
			Command: wire.RoomCommandAttach, Reason: wire.RoomRejectLimit, Message: "room full"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := mustHex(t, tc.hex)
			got, err := wire.AppendRoomEvent(nil, tc.want)
			if err != nil {
				t.Fatalf("AppendRoomEvent: %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("RoomEvent bytes drifted from the golden vector\n got %x\nwant %x", got, want)
			}
			e, err := wire.ParseRoomEvent(want)
			if err != nil {
				t.Fatalf("ParseRoomEvent: %v", err)
			}
			if !reflect.DeepEqual(e, tc.want) {
				t.Errorf("ParseRoomEvent = %+v, want %+v", e, tc.want)
			}
		})
	}
}

// RoomCommand: Attach and Detach are the two this module sends.
func TestGoldenRoomCommands(t *testing.T) {
	for _, tc := range []struct {
		name string
		hex  string
		want wire.RoomCommand
	}{
		{"attach", goldenRoomCommandAttachHex, wire.RoomCommand{Kind: wire.RoomCommandAttach, BroadcastID: "ABCDEF", ResumeToken: goldenRoomResumeToken, Label: "tuhis"}},
		{"detach", goldenRoomCommandDetachHex, wire.RoomCommand{Kind: wire.RoomCommandDetach, BroadcastID: "ABCDEF"}},
		{"nick", goldenRoomCommandNickHex, wire.RoomCommand{Kind: wire.RoomCommandSetNickname, Nickname: "tuhis"}},
		{"end", goldenRoomCommandEndHex, wire.RoomCommand{Kind: wire.RoomCommandEndRoom}},
		{"resync", goldenRoomCommandResyncHex, wire.RoomCommand{Kind: wire.RoomCommandResync}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := mustHex(t, tc.hex)
			got, err := wire.AppendRoomCommand(nil, tc.want)
			if err != nil {
				t.Fatalf("AppendRoomCommand: %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("RoomCommand bytes drifted from the golden vector\n got %x\nwant %x", got, want)
			}
			c, err := wire.ParseRoomCommand(want)
			if err != nil {
				t.Fatalf("ParseRoomCommand: %v", err)
			}
			if !reflect.DeepEqual(c, tc.want) {
				t.Errorf("ParseRoomCommand = %+v, want %+v", c, tc.want)
			}
		})
	}
}

// The docs/44 §4.11 reserved kind ranges (events 0x40+, commands 0x50+):
// this module's room reader must see ErrUnknownRoomKind and skip the record
// rather than tear the session down, so the sentinel is pinned from here.
func TestRoomReservedKinds(t *testing.T) {
	if _, err := wire.ParseRoomEvent(mustHex(t, "0115000000014041")); !errors.Is(err, wire.ErrUnknownRoomKind) {
		t.Errorf("reserved event kind 0x40: err = %v, want ErrUnknownRoomKind", err)
	}
	if _, err := wire.ParseRoomCommand(mustHex(t, "01165000")); !errors.Is(err, wire.ErrUnknownRoomKind) {
		t.Errorf("reserved command kind 0x50: err = %v, want ErrUnknownRoomKind", err)
	}
}
