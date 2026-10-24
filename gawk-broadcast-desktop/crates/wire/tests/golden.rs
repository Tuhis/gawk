//! The golden vectors from gawk-server/wire/wire_test.go (plus parity_test.go
//! and stripe_test.go), restated here rather than imported or generated: an
//! exported fixture the mirrors shared could be edited once and stay green
//! everywhere, which would defeat the purpose. These are the bytes the relay
//! and wire.ts already agree on.
//!
//! Do not regenerate them from code; if they change, the wire format changed.

use gawk_wire::*;

const GOLDEN_VIDEO_CHUNK: &str = "0101010001020304000500820000005d21dba5f0616263";
const GOLDEN_DECODER_CONFIG_AVC: &str = "0102000b617663312e3432453032410142e02affe1";
const GOLDEN_DECODER_CONFIG_VP8: &str = "01020003767038";
const GOLDEN_STREAM_FRAME_HEADER: &str = "01040100010203040000005d21dba5f00000000600000003";
const GOLDEN_TIME_SYNC_REPLY: &str = "01050102030405060708090a0b0c0d0e0f10";
const GOLDEN_TIME_SYNC_REQUEST: &str = "010500000000000f42400000000000000000";
const GOLDEN_CLOCK_MAPPING: &str = "0106000000000016e360";
const GOLDEN_CLOCK_MAPPING_NEGATIVE: &str = "0106fffffffffff0bdc0";
const GOLDEN_BROADCAST_ANNOUNCE: &str = "0103064b375851324d"; // "K7XQ2M"
const GOLDEN_RESUME_TOKEN: &str = "010910000102030405060708090a0b0c0d0e0f";
// R57 (docs/59 CN1): SessionClosing, code 4004. Byte-identical to
// gawk-server/wire closing_test.go goldenSessionClosingHex.
const GOLDEN_SESSION_CLOSING: &str = "011700000fa4";
const GOLDEN_CARRIER_PROLOGUE: &str = "010a";
const GOLDEN_CARRIER_MAX_RECORD_PREFIX: &str = "04b0"; // 1200
const GOLDEN_VIEWER_COUNT: &str = "010b00000003";
const GOLDEN_VIEWER_COUNT_LARGE: &str = "010b01020304"; // endianness pin
const GOLDEN_DELIVERY_ACK_DVR: &str = "010c020bb8";
const GOLDEN_DELIVERY_ACK_DATAGRAMS: &str = "010c000000";
const GOLDEN_AUDIO_FRAME: &str = "01070000010203040000005d21dba5f0616263";
const GOLDEN_AUDIO_CONFIG: &str = "010800046f7075730000bb8002";
const GOLDEN_AUDIO_CONFIG_WITH_DESC: &str = "010800046f7075730000bb8002010203";
const GOLDEN_TELEMETRY_HELLO: &str =
    "010d0107d000012345000102030405060708090a0ba1a2a3a4a5a6a7a81a2b3c4d5e6f";
const GOLDEN_TELEMETRY_HELLO_DISABLED: &str =
    "010d000000000000000000000000000000000000000000000000000000000000000000";
const GOLDEN_PARITY_CHUNK: &str = "010e01020304010009000021c0deadbeef";
const GOLDEN_RELAY_CAPABILITIES: &str = "010f000102";
const GOLDEN_CAPABILITIES_BOTH_BITS: &str = "010f000302";
const GOLDEN_CAPABILITIES_UPLINK_CARRIERS: &str = "010f000702";
const GOLDEN_STRIPE_STATE_STRIPED: &str = "0110010300";
const GOLDEN_STRIPE_STATE_UNSTRIPED: &str = "0110000000";
// R37 (docs/40 SP4): ServerVersion "1.42.0", Name "gawk home".
const GOLDEN_RELAY_IDENTITY: &str = "01110006312e34322e30096761776b20686f6d65";
// An unset operator name is nameLen 0, not an absent field.
const GOLDEN_RELAY_IDENTITY_NO_NAME: &str = "01110006312e34322e3000";
// R40 (docs/71 §9.3): the same identity with the screening notice appended.
const GOLDEN_RELAY_IDENTITY_SCREENING: &str = "01110006312e34322e30096761776b20686f6d6501";
// URL "https://gawk.example.com/api/telemetry/v1/ingest" (48 bytes, u16 BE).
const GOLDEN_TELEMETRY_ENDPOINT: &str = "011200003068747470733a2f2f6761776b2e6578616d706c652e636f6d2f6170692f74656c656d657472792f76312f696e67657374";

// R42 room control protocol (docs/44 §4.6), from gawk-server/wire/room_test.go.
// Field-by-field breakdowns live in that file's comments; the pieces below are
// the same literal pieces, concatenated by the compiler — never computed.
//
// RoomHello: protocol 1, clientKind 1 (web-broadcaster), wantCaps 0, "tuhis".
const GOLDEN_ROOM_HELLO: &str = "0113010100057475686973";
// One room record framing the golden RoomHello (11 bytes).
const GOLDEN_ROOM_RECORD: &str = concat!("000b", "0113010100057475686973");
// RoomState, dynamic room right after /room/new: flags dynamic|creator, seq 7,
// yourID 1, code "5UP4XW", no display name, creator token 00..0f, one
// attachment (ABCDEF, "tuhis", live, 3 viewers), one participant (id 1,
// web-broadcaster, streaming, "tuhis", no identity).
const GOLDEN_ROOM_STATE_DYNAMIC: &str = concat!(
    "0114",
    "03",
    "00",
    "00000007",
    "0001",
    "06355550345857",
    "00",
    "10000102030405060708090a0b0c0d0e0f",
    "061a2b3c4d5e6f",
    "01",
    "06414243444546",
    "057475686973",
    "01",
    "00000003",
    "0001",
    "0001",
    "01",
    "02",
    "057475686973",
    "00"
);
// RoomState, static room, empty: flags attachOK, seq 0, yourID 2, code
// "TuhisRoom", display name "Tuhis' room", no token, no attachments, one
// participant (id 2, web-viewer, no flags, "viewer").
const GOLDEN_ROOM_STATE_STATIC: &str = concat!(
    "0114",
    "04",
    "00",
    "00000000",
    "0002",
    "095475686973526f6f6d",
    "0b54756869732720726f6f6d",
    "00",
    "00",
    "00",
    "0001",
    "0002",
    "00",
    "00",
    "06766965776572",
    "00"
);
// RoomEvent ParticipantJoined, seq 8: id 3, native, streaming, "pc".
const GOLDEN_ROOM_EVENT_JOINED: &str =
    concat!("011500000008", "01", "0003", "02", "02", "027063", "00");
// RoomEvent ParticipantLeft, seq 9, id 3.
const GOLDEN_ROOM_EVENT_LEFT: &str = concat!("011500000009", "02", "0003");
// RoomEvent AttachmentUpdated, seq 13: ABCDEF, "tuhis", AWAY, 12 viewers.
const GOLDEN_ROOM_EVENT_ATTACHMENT_UPDATED: &str = concat!(
    "01150000000d",
    "12",
    "06414243444546",
    "057475686973",
    "00",
    "0000000c"
);
// RoomEvent AttachmentRemoved, seq 10: ABCDEF, reason expired (2).
const GOLDEN_ROOM_EVENT_ATTACHMENT_REMOVED: &str =
    concat!("01150000000a", "11", "06414243444546", "02");
// RoomEvent RoomEnding, seq 11, reason creator (2).
const GOLDEN_ROOM_EVENT_ENDING: &str = concat!("01150000000b", "20", "02");
// RoomEvent CommandRejected, seq 12: command attach (1), reason limit (1),
// message "room full".
const GOLDEN_ROOM_EVENT_REJECTED: &str =
    concat!("01150000000c", "30", "01", "01", "09726f6f6d2066756c6c");
// RoomCommand Attach: ABCDEF, resume token a0..af, label "tuhis".
const GOLDEN_ROOM_COMMAND_ATTACH: &str = concat!(
    "0116",
    "01",
    "06414243444546",
    "10a0a1a2a3a4a5a6a7a8a9aaabacadaeaf",
    "057475686973"
);
// RoomCommand Detach ABCDEF.
const GOLDEN_ROOM_COMMAND_DETACH: &str = concat!("0116", "02", "06414243444546");
// RoomCommand SetNickname "tuhis".
const GOLDEN_ROOM_COMMAND_NICK: &str = concat!("0116", "03", "057475686973");
// RoomCommand EndRoom / Resync: no payload.
const GOLDEN_ROOM_COMMAND_END: &str = "011604";
const GOLDEN_ROOM_COMMAND_RESYNC: &str = "011605";

// Cross-implementation parity-symbol vectors, generated 2026-07-30 by running
// gawk-server/wire's ComputeParity (Go) over the inputs below — pinning that
// this GF(2^8) port and the Go original compute identical P and Q bytes:
//   chunks = [000102030405060708090a0b0c0d0e0f,
//             ffeeddccbbaa99887766554433221100,
//             deadbeefcafebabe], k = 2
const GOLDEN_PARITY_P: &str = "21426120755125317f6f5f4f3f2f1f0f";
const GOLDEN_PARITY_Q: &str = "bc4e671d6093fbc8e6c5a0836a492c0f";

fn from_hex(s: &str) -> Vec<u8> {
    assert!(s.len().is_multiple_of(2), "bad golden hex {s}");
    (0..s.len())
        .step_by(2)
        .map(|i| u8::from_str_radix(&s[i..i + 2], 16).expect("bad golden hex"))
        .collect()
}

fn to_hex(b: &[u8]) -> String {
    b.iter().map(|v| format!("{v:02x}")).collect()
}

#[test]
fn golden_video_chunk() {
    let mut got = Vec::new();
    append_video_chunk(
        &mut got,
        &VideoChunkHeader {
            keyframe: true,
            frame_id: 0x01020304,
            chunk_index: 5,
            chunk_count: 130,
            timestamp_us: 0x0000005d21dba5f0,
        },
        b"abc",
    )
    .unwrap();
    assert_eq!(to_hex(&got), GOLDEN_VIDEO_CHUNK);

    let (h, payload) = parse_video_chunk(&got).unwrap();
    assert!(h.keyframe);
    assert_eq!(
        (h.frame_id, h.chunk_index, h.chunk_count),
        (0x01020304, 5, 130)
    );
    assert_eq!(h.timestamp_us, 0x0000005d21dba5f0);
    assert_eq!(payload, b"abc");
}

#[test]
fn golden_decoder_config() {
    let mut got = Vec::new();
    append_decoder_config(
        &mut got,
        "avc1.42E02A",
        &[0x01, 0x42, 0xe0, 0x2a, 0xff, 0xe1],
    )
    .unwrap();
    assert_eq!(to_hex(&got), GOLDEN_DECODER_CONFIG_AVC);

    let mut vp8 = Vec::new();
    append_decoder_config(&mut vp8, "vp8", b"").unwrap();
    assert_eq!(to_hex(&vp8), GOLDEN_DECODER_CONFIG_VP8);

    let cfg = parse_decoder_config(&vp8).unwrap();
    assert_eq!(cfg.codec, "vp8");
    assert!(cfg.extradata.is_empty());
}

// The empty-extradata DecoderConfig is this broadcaster's whole interop story
// (docs/38 D10): it emits raw Annex-B, never an avcC record, and the viewer's
// isAnnexB sniff routes around its extradata correction. Pinned from this side.
#[test]
fn empty_extradata_is_accepted() {
    let mut d = Vec::new();
    append_decoder_config(&mut d, "avc1.42E02A", b"").unwrap();
    let cfg = parse_decoder_config(&d).unwrap();
    assert_eq!(cfg.codec, "avc1.42E02A");
    assert!(cfg.extradata.is_empty());
}

#[test]
fn golden_stream_frame_header() {
    let mut got = Vec::new();
    append_stream_frame_header(
        &mut got,
        &StreamFrameHeader {
            keyframe: true,
            frame_id: 0x01020304,
            timestamp_us: 0x0000005d21dba5f0,
            config_len: 6,
            payload_len: 3,
        },
    )
    .unwrap();
    assert_eq!(to_hex(&got), GOLDEN_STREAM_FRAME_HEADER);
    let h = parse_stream_frame_header(&got).unwrap();
    assert_eq!((h.config_len, h.payload_len), (6, 3));
}

#[test]
fn golden_time_sync() {
    let mut req = Vec::new();
    append_time_sync(&mut req, 1_000_000, 0);
    assert_eq!(to_hex(&req), GOLDEN_TIME_SYNC_REQUEST);

    let mut reply = Vec::new();
    append_time_sync(&mut reply, 0x0102030405060708, 0x090a0b0c0d0e0f10);
    assert_eq!(to_hex(&reply), GOLDEN_TIME_SYNC_REPLY);
    let (c, s) = parse_time_sync(&reply).unwrap();
    assert_eq!((c, s), (0x0102030405060708, 0x090a0b0c0d0e0f10));
}

#[test]
fn golden_clock_mapping_including_negative_offsets() {
    let mut pos = Vec::new();
    append_clock_mapping(&mut pos, 1_500_000);
    assert_eq!(to_hex(&pos), GOLDEN_CLOCK_MAPPING);

    let mut neg = Vec::new();
    append_clock_mapping(&mut neg, -1_000_000);
    assert_eq!(to_hex(&neg), GOLDEN_CLOCK_MAPPING_NEGATIVE);
    assert_eq!(parse_clock_mapping(&neg).unwrap(), -1_000_000);
}

#[test]
fn golden_broadcast_announce() {
    let mut got = Vec::new();
    append_broadcast_announce(&mut got, "K7XQ2M").unwrap();
    assert_eq!(to_hex(&got), GOLDEN_BROADCAST_ANNOUNCE);
    assert_eq!(parse_broadcast_announce(&got).unwrap(), "K7XQ2M");
}

#[test]
fn golden_resume_token() {
    let token: Vec<u8> = (0..16).collect();
    let mut got = Vec::new();
    append_resume_token(&mut got, &token).unwrap();
    assert_eq!(to_hex(&got), GOLDEN_RESUME_TOKEN);
    assert_eq!(parse_resume_token(&got).unwrap(), token.as_slice());
}

#[test]
fn golden_session_closing() {
    let mut got = Vec::new();
    append_session_closing(&mut got, CLOSE_CODE_PUBLISHER_SUPERSEDED).unwrap();
    assert_eq!(to_hex(&got), GOLDEN_SESSION_CLOSING);
    assert_eq!(
        parse_session_closing(&from_hex(GOLDEN_SESSION_CLOSING)).unwrap(),
        CLOSE_CODE_PUBLISHER_SUPERSEDED
    );
    assert!(parse_session_closing(&from_hex("011700000f")).is_err());
    assert!(parse_session_closing(&from_hex("011700000fa400")).is_err());
    assert!(parse_session_closing(&from_hex("011700000f9f")).is_err());
    assert!(append_session_closing(&mut Vec::new(), 5000).is_err());
}

#[test]
fn golden_carrier_prologue_and_record() {
    let mut prologue = Vec::new();
    append_carrier_prologue(&mut prologue);
    assert_eq!(to_hex(&prologue), GOLDEN_CARRIER_PROLOGUE);
    parse_carrier_prologue(&prologue).unwrap();

    let dgram = from_hex(GOLDEN_VIDEO_CHUNK);
    let mut record = Vec::new();
    append_carrier_record(&mut record, &dgram).unwrap();
    assert_eq!(to_hex(&record), format!("0017{GOLDEN_VIDEO_CHUNK}"));
}

// The inclusive upper boundary of the record length prefix: a full delta
// chunk — which this producer puts on the wire — is exactly MaxDatagramSize.
#[test]
fn carrier_record_at_max_datagram_size() {
    let h = VideoChunkHeader {
        keyframe: false,
        frame_id: 43,
        chunk_index: 1,
        chunk_count: 2,
        timestamp_us: 7654321,
    };
    let mut dgram = Vec::new();
    append_video_chunk(&mut dgram, &h, &vec![0xAB; MAX_CHUNK_PAYLOAD]).unwrap();
    assert_eq!(dgram.len(), MAX_DATAGRAM_SIZE);

    let mut record = Vec::new();
    append_carrier_record(&mut record, &dgram).unwrap();
    assert_eq!(record.len(), CARRIER_RECORD_HEADER_SIZE + MAX_DATAGRAM_SIZE);
    assert_eq!(
        to_hex(&record[..CARRIER_RECORD_HEADER_SIZE]),
        GOLDEN_CARRIER_MAX_RECORD_PREFIX
    );

    let (got, rest) = parse_carrier_record(&record).unwrap();
    assert_eq!(got, dgram.as_slice());
    assert!(rest.is_empty());
}

#[test]
fn golden_viewer_count() {
    let mut got = Vec::new();
    append_viewer_count(&mut got, 3);
    assert_eq!(to_hex(&got), GOLDEN_VIEWER_COUNT);
    assert_eq!(parse_viewer_count(&got).unwrap(), 3);

    // Every byte distinct — the endianness pin.
    let mut large = Vec::new();
    append_viewer_count(&mut large, 0x01020304);
    assert_eq!(to_hex(&large), GOLDEN_VIEWER_COUNT_LARGE);
}

#[test]
fn golden_delivery_ack() {
    let mut dvr = Vec::new();
    append_delivery_ack(&mut dvr, DeliveryMode::Dvr, 3000);
    assert_eq!(to_hex(&dvr), GOLDEN_DELIVERY_ACK_DVR);

    // What a viewer that asked for reliable and was refused must be told.
    let mut plain = Vec::new();
    append_delivery_ack(&mut plain, DeliveryMode::Datagrams, 0);
    assert_eq!(to_hex(&plain), GOLDEN_DELIVERY_ACK_DATAGRAMS);
}

#[test]
fn golden_audio_frame_and_config() {
    let mut frame = Vec::new();
    append_audio_frame(
        &mut frame,
        &AudioFrameHeader {
            seq: 0x01020304,
            timestamp_us: 0x0000005d21dba5f0,
        },
        b"abc",
    )
    .unwrap();
    assert_eq!(to_hex(&frame), GOLDEN_AUDIO_FRAME);

    let mut config = Vec::new();
    append_audio_config(
        &mut config,
        &AudioConfig {
            codec: "opus",
            sample_rate: 48000,
            channels: 2,
            description: b"",
        },
    )
    .unwrap();
    assert_eq!(to_hex(&config), GOLDEN_AUDIO_CONFIG);

    let mut with_desc = Vec::new();
    append_audio_config(
        &mut with_desc,
        &AudioConfig {
            codec: "opus",
            sample_rate: 48000,
            channels: 2,
            description: &[1, 2, 3],
        },
    )
    .unwrap();
    assert_eq!(to_hex(&with_desc), GOLDEN_AUDIO_CONFIG_WITH_DESC);
}

#[test]
fn golden_telemetry_hello() {
    let token = [
        0x00, 0x01, 0x23, 0x45, 0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a,
        0x0b, 0xa1, 0xa2, 0xa3, 0xa4, 0xa5, 0xa6, 0xa7, 0xa8,
    ];
    let key = [0x1a, 0x2b, 0x3c, 0x4d, 0x5e, 0x6f];
    let mut got = Vec::new();
    append_telemetry_hello(
        &mut got,
        &TelemetryHello {
            enabled: true,
            report_interval_ms: 2000,
            token: &token,
            broadcast_key: &key,
        },
    )
    .unwrap();
    assert_eq!(to_hex(&got), GOLDEN_TELEMETRY_HELLO);

    let h = parse_telemetry_hello(&got).unwrap();
    assert!(h.enabled);
    assert_eq!(h.report_interval_ms, 2000);
    assert_eq!(to_hex(h.broadcast_key), "1a2b3c4d5e6f");

    // Disabled: same 35 bytes, zero token/key — identical to a pre-R28 relay
    // sending nothing, and the client collects nothing.
    let disabled = from_hex(GOLDEN_TELEMETRY_HELLO_DISABLED);
    let h = parse_telemetry_hello(&disabled).unwrap();
    assert!(!h.enabled);
}

#[test]
fn golden_parity_chunk() {
    let mut got = Vec::new();
    append_parity_chunk(
        &mut got,
        &ParityChunkHeader {
            frame_id: 0x01020304,
            parity_index: 1,
            chunk_count: 9,
            frame_bytes: 8640,
        },
        &[0xde, 0xad, 0xbe, 0xef],
    )
    .unwrap();
    assert_eq!(to_hex(&got), GOLDEN_PARITY_CHUNK);
}

// Pins the sizing rule this producer depends on: a full delta chunk's parity
// symbol is MaxChunkPayload bytes and the resulting datagram must still fit
// MaxDatagramSize — the reason the parity header is 13 bytes, not 20.
#[test]
fn parity_full_payload_fits_datagram() {
    let mut got = Vec::new();
    append_parity_chunk(
        &mut got,
        &ParityChunkHeader {
            frame_id: 1,
            parity_index: 0,
            chunk_count: 9,
            frame_bytes: (9 * MAX_CHUNK_PAYLOAD) as u32,
        },
        &vec![0x5a; MAX_CHUNK_PAYLOAD],
    )
    .unwrap();
    assert!(got.len() <= MAX_DATAGRAM_SIZE);
    assert_eq!(got.len(), PARITY_CHUNK_HEADER_SIZE + MAX_CHUNK_PAYLOAD);
}

#[test]
fn golden_parity_symbols_match_the_go_implementation() {
    let c0 = from_hex("000102030405060708090a0b0c0d0e0f");
    let c1 = from_hex("ffeeddccbbaa99887766554433221100");
    let c2 = from_hex("deadbeefcafebabe");
    let symbols = compute_parity(&[c0.as_slice(), c1.as_slice(), c2.as_slice()], 2).unwrap();
    assert_eq!(symbols.len(), 2);
    assert_eq!(to_hex(&symbols[0]), GOLDEN_PARITY_P);
    assert_eq!(to_hex(&symbols[1]), GOLDEN_PARITY_Q);

    // min(k, n): a single-chunk frame yields one symbol (P duplicates the
    // chunk; a second symbol would duplicate it again — wire waste).
    let single = compute_parity(&[c0.as_slice()], 2).unwrap();
    assert_eq!(single.len(), 1);
    assert_eq!(single[0], c0);
}

#[test]
fn golden_relay_capabilities() {
    let mut got = Vec::new();
    append_relay_capabilities(
        &mut got,
        &RelayCapabilities {
            flags: CAP_PARITY_CHUNKS,
            parity_level: 2,
        },
    )
    .unwrap();
    assert_eq!(to_hex(&got), GOLDEN_RELAY_CAPABILITIES);
    let back = parse_relay_capabilities(&got).unwrap();
    assert_eq!(back.parity_level, 2);
    assert_ne!(back.flags & CAP_PARITY_CHUNKS, 0);
}

// This producer's stake in R30: the one message it parses gains a flag bit
// and must stay 5 bytes ("new bits, never new bytes"), or the strict parse in
// every deployed producer breaks mid-skew.
#[test]
fn capabilities_survive_the_striped_bit() {
    let mut got = Vec::new();
    append_relay_capabilities(
        &mut got,
        &RelayCapabilities {
            flags: CAP_PARITY_CHUNKS | CAP_STRIPED_DELIVERY,
            parity_level: 2,
        },
    )
    .unwrap();
    assert_eq!(to_hex(&got), GOLDEN_CAPABILITIES_BOTH_BITS);
    let back = parse_relay_capabilities(&got).unwrap();
    assert_ne!(back.flags & CAP_PARITY_CHUNKS, 0);
}

// R73 (docs/75 G7): the bit this producer will gate its uplink carriers on,
// in the same 5 bytes.
#[test]
fn capabilities_carry_the_uplink_carriers_bit() {
    let mut got = Vec::new();
    append_relay_capabilities(
        &mut got,
        &RelayCapabilities {
            flags: CAP_PARITY_CHUNKS | CAP_STRIPED_DELIVERY | CAP_UPLINK_CARRIERS,
            parity_level: 2,
        },
    )
    .unwrap();
    assert_eq!(to_hex(&got), GOLDEN_CAPABILITIES_UPLINK_CARRIERS);
    let back = parse_relay_capabilities(&got).unwrap();
    assert_ne!(back.flags & CAP_UPLINK_CARRIERS, 0);
}

#[test]
fn golden_stripe_state() {
    let mut striped = Vec::new();
    append_stripe_state(
        &mut striped,
        &StripeState {
            striped: true,
            stripe_n: 3,
        },
    )
    .unwrap();
    assert_eq!(to_hex(&striped), GOLDEN_STRIPE_STATE_STRIPED);

    let mut unstriped = Vec::new();
    append_stripe_state(&mut unstriped, &StripeState::default()).unwrap();
    assert_eq!(to_hex(&unstriped), GOLDEN_STRIPE_STATE_UNSTRIPED);
}

#[test]
fn golden_relay_identity() {
    let mut got = Vec::new();
    append_relay_identity(
        &mut got,
        &RelayIdentity {
            server_version: "1.42.0",
            name: "gawk home",
            notices: 0,
        },
    )
    .unwrap();
    assert_eq!(to_hex(&got), GOLDEN_RELAY_IDENTITY);
    let id = parse_relay_identity(&got).unwrap();
    assert_eq!(id.server_version, "1.42.0");
    assert_eq!(id.name, "gawk home");
    assert_eq!(id.notices, 0);
    assert!(!id.screens());

    let mut no_name = Vec::new();
    append_relay_identity(
        &mut no_name,
        &RelayIdentity {
            server_version: "1.42.0",
            name: "",
            notices: 0,
        },
    )
    .unwrap();
    assert_eq!(to_hex(&no_name), GOLDEN_RELAY_IDENTITY_NO_NAME);
    let id = parse_relay_identity(&no_name).unwrap();
    assert_eq!(id.server_version, "1.42.0");
    assert!(id.name.is_empty());
    assert_eq!(id.notices, 0);
}

// R40 (docs/71 §9.3): the notices byte is appended only when non-zero, so
// the vector above is also the notice-off shape; this one carries the bit.
#[test]
fn golden_relay_identity_screening() {
    let mut got = Vec::new();
    append_relay_identity(
        &mut got,
        &RelayIdentity {
            server_version: "1.42.0",
            name: "gawk home",
            notices: RELAY_NOTICE_SCREENING,
        },
    )
    .unwrap();
    assert_eq!(to_hex(&got), GOLDEN_RELAY_IDENTITY_SCREENING);
    let id = parse_relay_identity(&got).unwrap();
    assert_eq!(id.server_version, "1.42.0");
    assert_eq!(id.name, "gawk home");
    assert_eq!(id.notices, RELAY_NOTICE_SCREENING);
    assert!(id.screens());
}

// The deliberate deviation from house strictness (docs/40 §4.4): trailing
// bytes are this message's extension space — appended fields arrive through
// it without a wire break. Since R40 the first is the notices byte (any bits,
// unknown ones ignored); everything after it is still ignored.
#[test]
fn relay_identity_tolerates_trailing_extension_bytes() {
    let mut msg = from_hex(GOLDEN_RELAY_IDENTITY);
    msg.extend_from_slice(&[0xa1, 0xb2, 0xc3]);
    let id = parse_relay_identity(&msg).unwrap();
    assert_eq!(id.server_version, "1.42.0");
    assert_eq!(id.name, "gawk home");
    assert_eq!(id.notices, 0xa1);
    assert!(id.screens());

    let mut other_bits = from_hex(GOLDEN_RELAY_IDENTITY);
    other_bits.push(0xa2);
    let id = parse_relay_identity(&other_bits).unwrap();
    assert_eq!(id.notices, 0xa2);
    assert!(!id.screens(), "only bit 0 is the screening notice");
}

#[test]
fn golden_telemetry_endpoint() {
    const URL: &str = "https://gawk.example.com/api/telemetry/v1/ingest";
    let mut got = Vec::new();
    append_telemetry_endpoint(&mut got, URL).unwrap();
    assert_eq!(to_hex(&got), GOLDEN_TELEMETRY_ENDPOINT);
    assert_eq!(parse_telemetry_endpoint(&got).unwrap(), URL);

    // Trailing extension bytes tolerated, same contract as RelayIdentity.
    let mut trailing = from_hex(GOLDEN_TELEMETRY_ENDPOINT);
    trailing.push(0x7f);
    assert_eq!(parse_telemetry_endpoint(&trailing).unwrap(), URL);
}

// --- R42 room control protocol ----------------------------------------------

const GOLDEN_CREATOR_TOKEN: [u8; 16] = [0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15];
const GOLDEN_RESUME_TOKEN_BYTES: [u8; 16] = [
    0xa0, 0xa1, 0xa2, 0xa3, 0xa4, 0xa5, 0xa6, 0xa7, 0xa8, 0xa9, 0xaa, 0xab, 0xac, 0xad, 0xae, 0xaf,
];

fn golden_room_hello_value() -> RoomHello<'static> {
    RoomHello {
        protocol: 1,
        client_kind: ROOM_CLIENT_WEB_BROADCASTER,
        want_caps: 0,
        nickname: "tuhis",
    }
}

fn golden_room_state_dynamic_value() -> RoomState<'static> {
    RoomState {
        flags: ROOM_STATE_FLAG_DYNAMIC | ROOM_STATE_FLAG_CREATOR,
        caps: 0,
        seq: 7,
        your_id: 1,
        code: "5UP4XW",
        display_name: "",
        creator_token: &GOLDEN_CREATOR_TOKEN,
        key: &[0x1a, 0x2b, 0x3c, 0x4d, 0x5e, 0x6f],
        attachments: vec![RoomAttachment {
            broadcast_id: "ABCDEF".to_string(),
            label: "tuhis",
            live: true,
            viewer_count: 3,
        }],
        participants: vec![RoomParticipant {
            id: 1,
            kind: ROOM_CLIENT_WEB_BROADCASTER,
            flags: ROOM_PARTICIPANT_FLAG_STREAMING,
            nickname: "tuhis",
            identity: "",
        }],
    }
}

fn golden_room_state_static_value() -> RoomState<'static> {
    RoomState {
        flags: ROOM_STATE_FLAG_ATTACH_OK,
        your_id: 2,
        code: "TuhisRoom",
        display_name: "Tuhis' room",
        participants: vec![RoomParticipant {
            id: 2,
            kind: ROOM_CLIENT_WEB_VIEWER,
            nickname: "viewer",
            ..RoomParticipant::default()
        }],
        ..RoomState::default()
    }
}

#[test]
fn golden_room_hello() {
    let mut got = Vec::new();
    append_room_hello(&mut got, &golden_room_hello_value()).unwrap();
    assert_eq!(to_hex(&got), GOLDEN_ROOM_HELLO);
    let bytes = from_hex(GOLDEN_ROOM_HELLO);
    let h = parse_room_hello(&bytes).unwrap();
    assert_eq!(h, golden_room_hello_value());
}

#[test]
fn golden_room_record() {
    let hello = from_hex(GOLDEN_ROOM_HELLO);
    let mut got = Vec::new();
    append_room_record(&mut got, &hello).unwrap();
    assert_eq!(to_hex(&got), GOLDEN_ROOM_RECORD);
    assert_eq!(parse_room_record_length(&got).unwrap(), 11);

    assert_eq!(
        parse_room_record_length(&[0, 0]),
        Err(WireError::BadRoomRecord)
    );
    assert_eq!(
        parse_room_record_length(&[0xff, 0xff]),
        Err(WireError::BadRoomRecord)
    );
    assert!(matches!(
        parse_room_record_length(&[0x00]),
        Err(WireError::ShortDatagram { len: 1, need: 2 })
    ));
    assert_eq!(
        append_room_record(&mut Vec::new(), &vec![0; MAX_ROOM_RECORD_SIZE + 1]),
        Err(WireError::BadRoomRecord)
    );
}

#[test]
fn golden_room_state() {
    for (hex, want) in [
        (GOLDEN_ROOM_STATE_DYNAMIC, golden_room_state_dynamic_value()),
        (GOLDEN_ROOM_STATE_STATIC, golden_room_state_static_value()),
    ] {
        let mut got = Vec::new();
        append_room_state(&mut got, &want).unwrap();
        assert_eq!(to_hex(&got), hex);
        let bytes = from_hex(hex);
        let s = parse_room_state(&bytes).unwrap();
        assert_eq!(s, want);
    }
}

#[test]
fn golden_room_events() {
    let cases: [(&str, RoomEvent<'_>); 6] = [
        (
            GOLDEN_ROOM_EVENT_JOINED,
            RoomEvent {
                seq: 8,
                kind: ROOM_EVENT_PARTICIPANT_JOINED,
                participant: RoomParticipant {
                    id: 3,
                    kind: ROOM_CLIENT_NATIVE,
                    flags: ROOM_PARTICIPANT_FLAG_STREAMING,
                    nickname: "pc",
                    identity: "",
                },
                ..RoomEvent::default()
            },
        ),
        (
            GOLDEN_ROOM_EVENT_LEFT,
            RoomEvent {
                seq: 9,
                kind: ROOM_EVENT_PARTICIPANT_LEFT,
                participant: RoomParticipant {
                    id: 3,
                    ..RoomParticipant::default()
                },
                ..RoomEvent::default()
            },
        ),
        (
            GOLDEN_ROOM_EVENT_ATTACHMENT_UPDATED,
            RoomEvent {
                seq: 13,
                kind: ROOM_EVENT_ATTACHMENT_UPDATED,
                attachment: RoomAttachment {
                    broadcast_id: "ABCDEF".to_string(),
                    label: "tuhis",
                    live: false,
                    viewer_count: 12,
                },
                ..RoomEvent::default()
            },
        ),
        (
            GOLDEN_ROOM_EVENT_ATTACHMENT_REMOVED,
            RoomEvent {
                seq: 10,
                kind: ROOM_EVENT_ATTACHMENT_REMOVED,
                attachment: RoomAttachment {
                    broadcast_id: "ABCDEF".to_string(),
                    ..RoomAttachment::default()
                },
                reason: ROOM_DETACH_REASON_EXPIRED,
                ..RoomEvent::default()
            },
        ),
        (
            GOLDEN_ROOM_EVENT_ENDING,
            RoomEvent {
                seq: 11,
                kind: ROOM_EVENT_ROOM_ENDING,
                reason: ROOM_END_REASON_CREATOR,
                ..RoomEvent::default()
            },
        ),
        (
            GOLDEN_ROOM_EVENT_REJECTED,
            RoomEvent {
                seq: 12,
                kind: ROOM_EVENT_COMMAND_REJECTED,
                command: ROOM_COMMAND_ATTACH,
                reason: ROOM_REJECT_LIMIT,
                message: "room full",
                ..RoomEvent::default()
            },
        ),
    ];
    for (hex, want) in cases {
        let mut got = Vec::new();
        append_room_event(&mut got, &want).unwrap();
        assert_eq!(to_hex(&got), hex);
        let bytes = from_hex(hex);
        let e = parse_room_event(&bytes).unwrap();
        assert_eq!(e, want);
    }
}

#[test]
fn golden_room_commands() {
    let cases: [(&str, RoomCommand<'_>); 5] = [
        (
            GOLDEN_ROOM_COMMAND_ATTACH,
            RoomCommand {
                kind: ROOM_COMMAND_ATTACH,
                broadcast_id: "ABCDEF".to_string(),
                resume_token: &GOLDEN_RESUME_TOKEN_BYTES,
                label: "tuhis",
                nickname: "",
            },
        ),
        (
            GOLDEN_ROOM_COMMAND_DETACH,
            RoomCommand {
                kind: ROOM_COMMAND_DETACH,
                broadcast_id: "ABCDEF".to_string(),
                ..RoomCommand::default()
            },
        ),
        (
            GOLDEN_ROOM_COMMAND_NICK,
            RoomCommand {
                kind: ROOM_COMMAND_SET_NICKNAME,
                nickname: "tuhis",
                ..RoomCommand::default()
            },
        ),
        (
            GOLDEN_ROOM_COMMAND_END,
            RoomCommand {
                kind: ROOM_COMMAND_END_ROOM,
                ..RoomCommand::default()
            },
        ),
        (
            GOLDEN_ROOM_COMMAND_RESYNC,
            RoomCommand {
                kind: ROOM_COMMAND_RESYNC,
                ..RoomCommand::default()
            },
        ),
    ];
    for (hex, want) in cases {
        let mut got = Vec::new();
        append_room_command(&mut got, &want).unwrap();
        assert_eq!(to_hex(&got), hex);
        let bytes = from_hex(hex);
        let c = parse_room_command(&bytes).unwrap();
        assert_eq!(c, want);
    }
}

// The docs/44 §4.11 reserved ranges: an unknown kind is reported as such
// with the header fields filled in, so a reader can skip the record and a
// relay can answer ROOM_REJECT_UNSUPPORTED.
#[test]
fn room_reserved_kinds_are_distinguishable() {
    assert_eq!(
        parse_room_event(&from_hex("0115000000014041")),
        Err(WireError::UnknownRoomKind { seq: 1, kind: 0x40 })
    );
    assert_eq!(
        parse_room_command(&from_hex("01165000")),
        Err(WireError::UnknownRoomKind { seq: 0, kind: 0x50 })
    );
    assert_eq!(
        append_room_event(
            &mut Vec::new(),
            &RoomEvent {
                kind: 0x4f,
                ..RoomEvent::default()
            }
        ),
        Err(WireError::UnknownRoomKind { seq: 0, kind: 0x4f })
    );
    assert_eq!(
        append_room_command(
            &mut Vec::new(),
            &RoomCommand {
                kind: 0x5f,
                ..RoomCommand::default()
            }
        ),
        Err(WireError::UnknownRoomKind { seq: 0, kind: 0x5f })
    );
}

#[test]
fn room_parsers_reject() {
    let hello = from_hex(GOLDEN_ROOM_HELLO);
    // Trailing bytes are a framing error on strict messages.
    let mut trailing = hello.clone();
    trailing.push(0x00);
    assert_eq!(parse_room_hello(&trailing), Err(WireError::BadRoomHello));
    for (idx, val) in [
        (2, 2u8),  // protocol
        (3, 3),    // client kind
        (4, 0x04), // reserved cap bit
        (5, 0x20), // nick overrun
        (6, 0xff), // invalid UTF-8
    ] {
        let mut bad = hello.clone();
        bad[idx] = val;
        assert_eq!(
            parse_room_hello(&bad),
            Err(WireError::BadRoomHello),
            "hello byte {idx} = {val:#04x}"
        );
    }
    let long_nick = "a".repeat(MAX_ROOM_NICKNAME_LEN + 1);
    assert_eq!(
        append_room_hello(
            &mut Vec::new(),
            &RoomHello {
                protocol: 1,
                nickname: &long_nick,
                ..RoomHello::default()
            }
        ),
        Err(WireError::BadRoomHello)
    );

    let state = from_hex(GOLDEN_ROOM_STATE_DYNAMIC);
    for (idx, val) in [
        (2, 0x08u8), // reserved state flag
        (3, 0x04),   // reserved cap
        (17, 0x05),  // mis-frames the token length (neither 0 nor 16)
    ] {
        let mut bad = state.clone();
        bad[idx] = val;
        assert_eq!(
            parse_room_state(&bad),
            Err(WireError::BadRoomState),
            "state byte {idx} = {val:#04x}"
        );
    }
    assert_eq!(
        parse_room_state(&state[..state.len() - 1]),
        Err(WireError::BadRoomState)
    );
    assert_eq!(
        append_room_state(
            &mut Vec::new(),
            &RoomState {
                code: "x",
                creator_token: &[1],
                ..RoomState::default()
            }
        ),
        Err(WireError::BadRoomState)
    );
    assert_eq!(
        append_room_state(&mut Vec::new(), &RoomState::default()),
        Err(WireError::BadRoomState)
    );
    assert_eq!(
        append_room_state(
            &mut Vec::new(),
            &RoomState {
                code: "x",
                attachments: vec![RoomAttachment {
                    broadcast_id: "0OIL11".to_string(),
                    ..RoomAttachment::default()
                }],
                ..RoomState::default()
            }
        ),
        Err(WireError::BadRoomState)
    );
    assert_eq!(
        append_room_state(
            &mut Vec::new(),
            &RoomState {
                code: "x",
                participants: vec![RoomParticipant {
                    flags: 0x80,
                    ..RoomParticipant::default()
                }],
                ..RoomState::default()
            }
        ),
        Err(WireError::BadRoomState)
    );

    let ev = from_hex(GOLDEN_ROOM_EVENT_JOINED);
    let mut trailing = ev.clone();
    trailing.push(0);
    assert_eq!(parse_room_event(&trailing), Err(WireError::BadRoomEvent));
    assert!(matches!(
        parse_room_event(&ev[..6]),
        Err(WireError::ShortDatagram { len: 6, need: 7 })
    ));

    let cmd = from_hex(GOLDEN_ROOM_COMMAND_ATTACH);
    let mut bad = cmd.clone();
    bad[10] = 0x0f; // token length 15
    assert_eq!(parse_room_command(&bad), Err(WireError::BadRoomCommand));
    assert_eq!(
        append_room_command(
            &mut Vec::new(),
            &RoomCommand {
                kind: ROOM_COMMAND_ATTACH,
                broadcast_id: "ABCDEF".to_string(),
                resume_token: &[1],
                ..RoomCommand::default()
            }
        ),
        Err(WireError::BadRoomCommand)
    );
    // Broadcast IDs normalize on parse: a lower-case ID on the wire is
    // accepted and returned upper-case, so the relay never compares raw.
    let lower_bytes = from_hex(concat!("0116", "02", "06616263646566"));
    let lower = parse_room_command(&lower_bytes).unwrap();
    assert_eq!(lower.broadcast_id, "ABCDEF");
    assert_eq!(
        parse_room_command(&from_hex(concat!("0116", "02", "06304f494c3131"))),
        Err(WireError::BadRoomCommand)
    );
    assert_eq!(
        parse_room_command(&from_hex("011604ff")),
        Err(WireError::BadRoomCommand)
    );
}

// The constants the engine's chunking, buffering and refusal limits are built
// on. A change to any of these is a protocol change, not a tuning knob —
// update the relay, both native broadcasters and the viewer together.
#[test]
fn wire_constants_are_pinned() {
    assert_eq!(VERSION, 0x01);
    assert_eq!(TYPE_VIDEO_CHUNK, 0x01);
    assert_eq!(TYPE_DECODER_CONFIG, 0x02);
    assert_eq!(TYPE_BROADCAST_ANNOUNCE, 0x03);
    assert_eq!(TYPE_STREAM_FRAME, 0x04);
    assert_eq!(TYPE_TIME_SYNC, 0x05);
    assert_eq!(TYPE_CLOCK_MAPPING, 0x06);
    assert_eq!(TYPE_AUDIO_FRAME, 0x07);
    assert_eq!(TYPE_AUDIO_CONFIG, 0x08);
    assert_eq!(TYPE_RESUME_TOKEN, 0x09);
    assert_eq!(TYPE_RELIABLE_CARRIER, 0x0A);
    assert_eq!(TYPE_VIEWER_COUNT, 0x0B);
    assert_eq!(TYPE_DELIVERY_ACK, 0x0C);
    assert_eq!(TYPE_TELEMETRY_HELLO, 0x0D);
    assert_eq!(TYPE_PARITY_CHUNK, 0x0E);
    assert_eq!(TYPE_RELAY_CAPABILITIES, 0x0F);
    assert_eq!(TYPE_STRIPE_STATE, 0x10);
    assert_eq!(TYPE_RELAY_IDENTITY, 0x11);
    assert_eq!(TYPE_TELEMETRY_ENDPOINT, 0x12);
    assert_eq!(TYPE_ROOM_HELLO, 0x13);
    assert_eq!(TYPE_ROOM_STATE, 0x14);
    assert_eq!(TYPE_ROOM_EVENT, 0x15);
    assert_eq!(TYPE_ROOM_COMMAND, 0x16);
    assert_eq!(TYPE_SESSION_CLOSING, 0x17);
    assert_eq!(SESSION_CLOSING_SIZE, 6);

    assert_eq!(MAX_DATAGRAM_SIZE, 1200);
    assert_eq!(VIDEO_CHUNK_HEADER_SIZE, 20);
    assert_eq!(MAX_CHUNK_PAYLOAD, 1180);
    assert_eq!(MAX_CHUNK_COUNT, 3000);
    assert_eq!(STREAM_FRAME_HEADER_SIZE, 24);
    assert_eq!(MAX_KEYFRAME_BYTES, 8 << 20);
    assert_eq!(TIME_SYNC_SIZE, 18);
    assert_eq!(CLOCK_MAPPING_SIZE, 10);
    assert_eq!(VIEWER_COUNT_SIZE, 6);
    assert_eq!(CARRIER_PROLOGUE_SIZE, 2);
    assert_eq!(CARRIER_RECORD_HEADER_SIZE, 2);
    assert_eq!(AUDIO_FRAME_HEADER_SIZE, 16);
    assert_eq!(MAX_AUDIO_PAYLOAD, 1184);
    assert_eq!(TELEMETRY_HELLO_SIZE, 35);
    assert_eq!(TELEMETRY_SESSION_TOKEN_SIZE, 24);
    assert_eq!(TELEMETRY_BROADCAST_KEY_SIZE, 6);
    assert_eq!(DELIVERY_ACK_SIZE, 5);
    assert_eq!(PARITY_CHUNK_HEADER_SIZE, 13);
    assert_eq!(MAX_PARITY_SYMBOLS, 2);
    assert_eq!(MAX_PARITY_DATA_CHUNKS, 255);
    assert_eq!(RELAY_CAPABILITIES_SIZE, 5);
    assert_eq!(STRIPE_STATE_SIZE, 5);
    assert_eq!(MAX_RELAY_IDENTITY_VERSION_LEN, 32);
    assert_eq!(MAX_RELAY_IDENTITY_NAME_LEN, 64);
    assert_eq!(MAX_TELEMETRY_ENDPOINT_URL_LEN, 512);
    assert_eq!(MAX_STRIPE_LEGS, 4);
    assert_eq!(CAP_PARITY_CHUNKS, 1 << 0);
    assert_eq!(CAP_STRIPED_DELIVERY, 1 << 1);
    assert_eq!(CAP_UPLINK_CARRIERS, 1 << 2);
    assert_eq!(ROOM_PROTOCOL_VERSION, 1);
    assert_eq!(ROOM_RECORD_HEADER_SIZE, 2);
    assert_eq!(MAX_ROOM_RECORD_SIZE, 16384);
    assert_eq!(MAX_ROOM_NICKNAME_LEN, 32);
    assert_eq!(MAX_ROOM_CODE_LEN, 32);
    assert_eq!(MAX_ROOM_DISPLAY_NAME_LEN, 64);
    assert_eq!(MAX_ROOM_LABEL_LEN, 32);
    assert_eq!(MAX_ROOM_IDENTITY_LEN, 64);
    assert_eq!(MAX_ROOM_REJECT_MESSAGE_LEN, 128);
    assert_eq!(ROOM_CREATOR_TOKEN_SIZE, 16);
    assert_eq!(ROOM_KEY_SIZE, 6);
    assert_eq!(RESUME_TOKEN_SIZE, 16);
    assert_eq!(BROADCAST_ID_LENGTH, 6);

    assert_eq!(CLOSE_CODE_BROADCAST_ENDED, 4000);
    assert_eq!(CLOSE_CODE_SUBSCRIBER_UNRESPONSIVE, 4001);
    assert_eq!(CLOSE_CODE_SERVER_DRAINING, 4002);
    assert_eq!(CLOSE_CODE_ORIGIN_MOVED, 4003);
    assert_eq!(CLOSE_CODE_PUBLISHER_SUPERSEDED, 4004);
    assert_eq!(CLOSE_CODE_STRIPE_LEG_ORPHANED, 4005);
    assert_eq!(CLOSE_CODE_TERMINATED_BY_OPERATOR, 4006);
    assert_eq!(CLOSE_CODE_ROOM_ENDED, 4007);

    assert_eq!(BROADCAST_ID_ALPHABET, "23456789ABCDEFGHJKMNPQRSTUVWXYZ");
    assert_eq!(BROADCAST_ID_ALPHABET.len(), 31);
}

// --- R74 PoP relays (docs/76 §9) ---------------------------------------------
// LocalRelayInfo (0x18) and LocalRelayOffer (0x19), byte-identical to
// gawk-server/wire/localrelay_test.go — restated, never imported.

// A: a linked PoP whose broadcaster is on the party wall.
const GOLDEN_LOCAL_RELAY_INFO_LINKED: &str = concat!(
    "01180003",
    "0d417373656d626c792032303236",
    "1468747470733a2f2f6761776b2e696f696f2e6669",
    "0d617373656d626c792d32303236"
);
// B: a standalone PoP (state 0, no origin, no wall).
const GOLDEN_LOCAL_RELAY_INFO_STANDALONE: &str = "011800000d417373656d626c7920323032360000";
// The offer: handle 6f1c2d3e-…, "Assembly 2026", 3600 s, two candidates.
const GOLDEN_LOCAL_RELAY_OFFER: &str = concat!(
    "011900",
    "2436663163326433652d346135622d346336642d386537662d303132333435363738396162",
    "0d417373656d626c792032303236",
    "00000e10",
    "02",
    "002668747470733a2f2f702d376b326d39782e706f702e6761776b2e696f696f2e66693a34343333",
    "00",
    "001968747470733a2f2f3139322e3136382e312e32303a34343333",
    "02",
    "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f",
    "202122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f"
);
// R74: SessionClosing for 4012 PopUplinkFull (closing_test.go
// goldenSessionClosingPopUplinkFullHex).
const GOLDEN_SESSION_CLOSING_POP_UPLINK_FULL: &str = "011700000fac";
// R76 (docs/77 §5): SessionClosing for 4014 BroadcasterStopped
// (closing_test.go goldenSessionClosingBroadcasterStoppedHex).
const GOLDEN_SESSION_CLOSING_BROADCASTER_STOPPED: &str = "011700000fae";

const LOCAL_RELAY_H1: [u8; 32] = {
    let mut h = [0u8; 32];
    let mut i = 0;
    while i < 32 {
        h[i] = i as u8;
        i += 1;
    }
    h
};
const LOCAL_RELAY_H2: [u8; 32] = {
    let mut h = [0u8; 32];
    let mut i = 0;
    while i < 32 {
        h[i] = 0x20 + i as u8;
        i += 1;
    }
    h
};

fn golden_local_relay_info_linked() -> LocalRelayInfo<'static> {
    LocalRelayInfo {
        linked: true,
        on_wall: true,
        name: "Assembly 2026",
        home_origin: "https://gawk.ioio.fi",
        wall_code: "assembly-2026",
    }
}

fn golden_offer() -> LocalRelayOffer<'static> {
    LocalRelayOffer {
        handle: "6f1c2d3e-4a5b-4c6d-8e7f-0123456789ab",
        name: "Assembly 2026",
        validity: 3600,
        candidates: vec![
            LocalRelayCandidate {
                url: "https://p-7k2m9x.pop.gawk.ioio.fi:4433",
                hashes: vec![],
            },
            LocalRelayCandidate {
                url: "https://192.168.1.20:4433",
                hashes: vec![&LOCAL_RELAY_H1, &LOCAL_RELAY_H2],
            },
        ],
    }
}

#[test]
fn golden_local_relay_info() {
    let standalone = LocalRelayInfo {
        name: "Assembly 2026",
        ..Default::default()
    };
    for (hex, msg) in [
        (
            GOLDEN_LOCAL_RELAY_INFO_LINKED,
            golden_local_relay_info_linked(),
        ),
        (GOLDEN_LOCAL_RELAY_INFO_STANDALONE, standalone),
    ] {
        let mut got = Vec::new();
        msg.encode(&mut got).unwrap();
        assert_eq!(to_hex(&got), hex);
        let bytes = from_hex(hex);
        assert_eq!(LocalRelayInfo::parse(&bytes).unwrap(), msg);
        // The free-function spelling is the same code.
        assert_eq!(parse_local_relay_info(&bytes).unwrap(), msg);
    }
}

#[test]
fn local_relay_info_tolerates_trailing_bytes_and_unknown_state() {
    let bytes = from_hex(&format!("{GOLDEN_LOCAL_RELAY_INFO_LINKED}a1b2c3"));
    assert_eq!(
        LocalRelayInfo::parse(&bytes).unwrap(),
        golden_local_relay_info_linked()
    );
    let mut b = from_hex(GOLDEN_LOCAL_RELAY_INFO_STANDALONE);
    b[3] = 0xfd;
    let info = LocalRelayInfo::parse(&b).unwrap();
    assert!(info.linked && !info.on_wall);
}

#[test]
fn local_relay_info_origin_rule() {
    let ok = [
        "",
        "https://gawk.ioio.fi",
        "https://gawk.ioio.fi:8443",
        "https://192.168.1.20:4433",
        "https://[fd00::1]:4433",
        "https://a",
        "https://a:65535",
        "http://localhost",
        "http://localhost:5173",
        "http://127.0.0.1",
        "http://127.0.0.1:4480",
    ];
    for o in ok {
        let msg = LocalRelayInfo {
            linked: !o.is_empty(),
            name: "x",
            home_origin: o,
            ..Default::default()
        };
        let mut b = Vec::new();
        msg.encode(&mut b).unwrap_or_else(|e| panic!("{o:?}: {e}"));
        assert_eq!(LocalRelayInfo::parse(&b).unwrap().home_origin, o);
    }
    let long = format!("https://{}", "a".repeat(MAX_LOCAL_RELAY_ORIGIN_LEN));
    let bad = [
        "https://gawk.ioio.fi/",
        "https://gawk.ioio.fi/app",
        "https://gawk.ioio.fi?x=1",
        "https://gawk.ioio.fi#f",
        "https://user@gawk.ioio.fi",
        "https://",
        "http://gawk.ioio.fi",
        "http://localhost.evil.example",
        "http://192.168.1.20",
        "ws://gawk.ioio.fi",
        "gawk.ioio.fi",
        "https://gawk.ioio.fi:notaport",
        "https://gawk ioio.fi",
        "https://gawk.ioio.fi\x7f",
        "https://a:0",
        "https://a:65536",
        "https://a:",
        "https://a:123456",
        "https://a::1",
        "https://[]",
        "https://[::1",
        "https://[::1]x",
        "https://[[::1]",
        "https://a]",
        "HTTPS://gawk.ioio.fi",
        long.as_str(),
    ];
    for o in bad {
        let msg = LocalRelayInfo {
            linked: true,
            name: "x",
            home_origin: o,
            ..Default::default()
        };
        assert_eq!(
            msg.encode(&mut Vec::new()),
            Err(WireError::BadLocalRelayInfo),
            "{o:?}"
        );
        if o.len() > 255 {
            continue;
        }
        let mut b = vec![0x01, 0x18, 0, 0, 0, o.len() as u8];
        b.extend_from_slice(o.as_bytes());
        b.push(0);
        assert_eq!(
            LocalRelayInfo::parse(&b),
            Err(WireError::BadLocalRelayInfo),
            "{o:?}"
        );
    }
}

#[test]
fn local_relay_info_rejects() {
    let long_name = "a".repeat(MAX_RELAY_IDENTITY_NAME_LEN + 1);
    let long_wall = "a".repeat(MAX_ROOM_CODE_LEN + 1);
    for msg in [
        LocalRelayInfo {
            name: &long_name,
            ..Default::default()
        },
        LocalRelayInfo {
            wall_code: &long_wall,
            ..Default::default()
        },
    ] {
        assert_eq!(
            msg.encode(&mut Vec::new()),
            Err(WireError::BadLocalRelayInfo)
        );
    }

    let golden = from_hex(GOLDEN_LOCAL_RELAY_INFO_STANDALONE);
    let n = golden.len();
    let with = |at: usize, v: u8| {
        let mut b = golden.clone();
        b[at] = v;
        b
    };
    let cat = |a: &[u8], b: &[u8]| [a, b].concat();
    let mut name65 = vec![1, 0x18, 0, 0, 65];
    name65.extend(std::iter::repeat_n(b'a', 65));
    name65.extend([0, 0]);
    let mut wall33 = vec![33u8];
    wall33.extend(std::iter::repeat_n(b'a', 33));
    let cases: Vec<(Vec<u8>, WireError)> = vec![
        (vec![], WireError::ShortDatagram { len: 0, need: 7 }),
        (
            golden[..6].to_vec(),
            WireError::ShortDatagram { len: 6, need: 7 },
        ),
        (with(0, 2), WireError::BadVersion(2)),
        (
            with(1, TYPE_RELAY_IDENTITY),
            WireError::BadType {
                got: TYPE_RELAY_IDENTITY,
                want: TYPE_LOCAL_RELAY_INFO,
            },
        ),
        (with(2, 0x01), WireError::BadLocalRelayInfo),
        (with(4, 0x30), WireError::BadLocalRelayInfo),
        (name65, WireError::BadLocalRelayInfo),
        (
            vec![1, 0x18, 0, 0, 1, 0xff, 0, 0],
            WireError::BadLocalRelayInfo,
        ),
        (golden[..n - 2].to_vec(), WireError::BadLocalRelayInfo),
        (golden[..n - 1].to_vec(), WireError::BadLocalRelayInfo),
        (with(n - 1, 5), WireError::BadLocalRelayInfo),
        (cat(&golden[..n - 1], &wall33), WireError::BadLocalRelayInfo),
        (
            cat(&golden[..n - 1], &[1, 0xc3]),
            WireError::BadLocalRelayInfo,
        ),
    ];
    for (b, want) in cases {
        assert_eq!(LocalRelayInfo::parse(&b), Err(want), "{}", to_hex(&b));
    }
}

#[test]
fn golden_local_relay_offer() {
    let mut got = Vec::new();
    golden_offer().encode(&mut got).unwrap();
    assert_eq!(to_hex(&got), GOLDEN_LOCAL_RELAY_OFFER);
    let bytes = from_hex(GOLDEN_LOCAL_RELAY_OFFER);
    assert_eq!(LocalRelayOffer::parse(&bytes).unwrap(), golden_offer());
    assert_eq!(parse_local_relay_offer(&bytes).unwrap(), golden_offer());
    let trailing = from_hex(&format!("{GOLDEN_LOCAL_RELAY_OFFER}a1b2c3"));
    assert_eq!(LocalRelayOffer::parse(&trailing).unwrap(), golden_offer());
}

#[test]
fn local_relay_url_rule() {
    let ok = [
        "https://a",
        "https://p-7k2m9x.pop.gawk.ioio.fi:4433",
        "https://192.168.1.20:4433",
        "https://[fd00::1]:4433",
        "https://a.example/some/path",
        "https://a.example/",
    ];
    let long = format!("https://a.example/{}", "a".repeat(MAX_LOCAL_RELAY_URL_LEN));
    let bad = [
        "",
        "https://",
        "https:///path",
        "http://a.example",
        "HTTPS://a.example",
        "//a.example",
        "https://a.example?q",
        "https://a.example/p?q=1",
        "https://a.example/#f",
        "https://u@a.example",
        "https://a.example:0",
        "https://a.example:65536",
        "https://a.example:/p",
        "https://a.example:44x3",
        "https://[fd00::1",
        "https://[fd00::1]:",
        "https://a b",
        "https://a\x00",
        "https://\u{e9}.example",
        long.as_str(),
    ];
    let offer_with = |url| LocalRelayOffer {
        candidates: vec![LocalRelayCandidate {
            url,
            hashes: vec![],
        }],
        ..golden_offer()
    };
    for u in ok {
        let mut b = Vec::new();
        offer_with(u)
            .encode(&mut b)
            .unwrap_or_else(|e| panic!("{u:?}: {e}"));
        assert_eq!(LocalRelayOffer::parse(&b).unwrap().candidates[0].url, u);
    }
    for u in bad {
        assert_eq!(
            offer_with(u).encode(&mut Vec::new()),
            Err(WireError::BadLocalRelayOffer),
            "{u:?}"
        );
    }
}

#[test]
fn local_relay_offer_rejects_on_encode() {
    let g = golden_offer;
    let h = [0u8; 32];
    let long_handle = "a".repeat(MAX_LOCAL_RELAY_HANDLE_LEN + 1);
    let long_name = "a".repeat(MAX_RELAY_IDENTITY_NAME_LEN + 1);
    let bad = [
        LocalRelayOffer { handle: "", ..g() },
        LocalRelayOffer {
            handle: &long_handle,
            ..g()
        },
        LocalRelayOffer {
            handle: "a\x01b",
            ..g()
        },
        LocalRelayOffer {
            name: &long_name,
            ..g()
        },
        LocalRelayOffer { validity: 0, ..g() },
        LocalRelayOffer {
            candidates: vec![],
            ..g()
        },
        LocalRelayOffer {
            candidates: (0..5)
                .map(|_| LocalRelayCandidate {
                    url: "https://a.example",
                    hashes: vec![],
                })
                .collect(),
            ..g()
        },
        LocalRelayOffer {
            candidates: vec![LocalRelayCandidate {
                url: "https://a.example",
                hashes: vec![&h, &h, &h],
            }],
            ..g()
        },
    ];
    for o in bad {
        assert_eq!(
            o.encode(&mut Vec::new()),
            Err(WireError::BadLocalRelayOffer),
            "{o:?}"
        );
    }
}

#[test]
fn local_relay_offer_rejects_on_parse() {
    let golden = from_hex(GOLDEN_LOCAL_RELAY_OFFER);
    let handle_len_at = 3;
    let name_len_at = handle_len_at + 1 + 36;
    let validity_at = name_len_at + 1 + 13;
    let cand_count_at = validity_at + 4;
    let url1_len_at = cand_count_at + 1;
    let hash1_at = url1_len_at + 2 + 38;
    let url2_len_at = hash1_at + 1;
    let hash2_at = url2_len_at + 2 + 25;
    let set = |at: usize, v: &[u8]| {
        let mut b = golden.clone();
        b[at..at + v.len()].copy_from_slice(v);
        b
    };
    let replace_url1 = |u: &str| {
        let mut b = golden[..url1_len_at].to_vec();
        b.extend((u.len() as u16).to_be_bytes());
        b.extend_from_slice(u.as_bytes());
        b.extend_from_slice(&golden[hash1_at..]);
        b
    };
    let bad = WireError::BadLocalRelayOffer;
    let mut handle65 = vec![1, 0x19, 0, 65];
    handle65.extend(std::iter::repeat_n(b'a', 65));
    handle65.extend_from_slice(&golden[name_len_at..]);
    let mut hash3 = set(hash2_at, &[3]);
    hash3.extend([0u8; 32]);
    let cases: Vec<(Vec<u8>, WireError)> = vec![
        (vec![], WireError::ShortDatagram { len: 0, need: 11 }),
        (
            golden[..3].to_vec(),
            WireError::ShortDatagram { len: 3, need: 11 },
        ),
        (set(0, &[2]), WireError::BadVersion(2)),
        (
            set(1, &[TYPE_LOCAL_RELAY_INFO]),
            WireError::BadType {
                got: TYPE_LOCAL_RELAY_INFO,
                want: TYPE_LOCAL_RELAY_OFFER,
            },
        ),
        (set(2, &[0x80]), bad.clone()),
        (
            [&[1, 0x19, 0, 0][..], &golden[name_len_at..]].concat(),
            bad.clone(),
        ),
        (handle65, bad.clone()),
        (set(handle_len_at + 1, &[0x01]), bad.clone()),
        (set(name_len_at + 1, &[0xff]), bad.clone()),
        (set(validity_at, &[0, 0, 0, 0]), bad.clone()),
        (
            set(cand_count_at, &[0])[..cand_count_at + 1].to_vec(),
            bad.clone(),
        ),
        (set(cand_count_at, &[5]), bad.clone()),
        (set(cand_count_at, &[3]), bad.clone()),
        (set(url1_len_at, &[0, 0]), bad.clone()),
        (set(url1_len_at, &[1, 1]), bad.clone()),
        (golden[..url1_len_at + 10].to_vec(), bad.clone()),
        (
            replace_url1("http://p-7k2m9x.pop.gawk.ioio.fi:4433"),
            bad.clone(),
        ),
        (
            replace_url1("https://p-7k2m9x.pop.gawk.ioio.fi:4433?q"),
            bad.clone(),
        ),
        (
            replace_url1("https://u@p-7k2m9x.pop.gawk.ioio.fi:4433"),
            bad.clone(),
        ),
        (hash3, bad.clone()),
        (golden[..golden.len() - 1].to_vec(), bad.clone()),
        (golden[..hash2_at].to_vec(), bad.clone()),
        (golden[..validity_at + 2].to_vec(), bad.clone()),
    ];
    for (b, want) in cases {
        assert_eq!(LocalRelayOffer::parse(&b), Err(want), "{}", to_hex(&b));
    }
}

#[test]
fn local_relay_offer_maximal_round_trips() {
    let h = [7u8; 32];
    let handle = "h".repeat(MAX_LOCAL_RELAY_HANDLE_LEN);
    let name = "n".repeat(MAX_RELAY_IDENTITY_NAME_LEN);
    let prefix = "https://a.example/";
    let url = format!(
        "{prefix}{}",
        "p".repeat(MAX_LOCAL_RELAY_URL_LEN - prefix.len())
    );
    let o = LocalRelayOffer {
        handle: &handle,
        name: &name,
        validity: u32::MAX,
        candidates: (0..MAX_LOCAL_RELAY_CANDIDATES)
            .map(|_| LocalRelayCandidate {
                url: &url,
                hashes: vec![&h, &h],
            })
            .collect(),
    };
    let mut b = Vec::new();
    o.encode(&mut b).unwrap();
    assert_eq!(LocalRelayOffer::parse(&b).unwrap(), o);
}

#[test]
fn local_relay_parsers_never_panic() {
    for seed in [
        GOLDEN_LOCAL_RELAY_INFO_LINKED,
        GOLDEN_LOCAL_RELAY_INFO_STANDALONE,
        GOLDEN_LOCAL_RELAY_OFFER,
    ] {
        let seed = from_hex(seed);
        for n in 0..=seed.len() {
            let _ = LocalRelayInfo::parse(&seed[..n]);
            let _ = LocalRelayOffer::parse(&seed[..n]);
        }
        for i in 0..seed.len() {
            for v in [0x00, 0x01, 0x7f, 0x80, 0xff] {
                let mut b = seed.clone();
                b[i] = v;
                let _ = LocalRelayInfo::parse(&b);
                let _ = LocalRelayOffer::parse(&b);
            }
        }
    }
}

#[test]
fn golden_session_closing_pop_uplink_full() {
    let mut got = Vec::new();
    append_session_closing(&mut got, CLOSE_CODE_POP_UPLINK_FULL).unwrap();
    assert_eq!(to_hex(&got), GOLDEN_SESSION_CLOSING_POP_UPLINK_FULL);
    assert_eq!(
        parse_session_closing(&from_hex(GOLDEN_SESSION_CLOSING_POP_UPLINK_FULL)).unwrap(),
        4012
    );
}

// R76 (docs/77 D5): the relay tells a viewer the broadcaster stopped in-band
// before the 4014 close, which Chrome rarely surfaces (docs/59).
#[test]
fn golden_session_closing_broadcaster_stopped() {
    let mut got = Vec::new();
    append_session_closing(&mut got, CLOSE_CODE_BROADCASTER_STOPPED).unwrap();
    assert_eq!(to_hex(&got), GOLDEN_SESSION_CLOSING_BROADCASTER_STOPPED);
    assert_eq!(
        parse_session_closing(&from_hex(GOLDEN_SESSION_CLOSING_BROADCASTER_STOPPED)).unwrap(),
        4014
    );
}

// R76 (docs/77 §5): 4014 is pinned by value, and it is NOT terminal for a
// viewer: it shows the ended state and keeps dialling the code (D7).
#[test]
fn r76_broadcaster_stopped_is_pinned_and_not_terminal() {
    assert_eq!(CLOSE_CODE_BROADCASTER_STOPPED, 4014);
    assert!(!terminal_for_viewer(CLOSE_CODE_BROADCASTER_STOPPED));
}

#[test]
fn r74_constants_are_pinned() {
    assert_eq!(TYPE_LOCAL_RELAY_INFO, 0x18);
    assert_eq!(TYPE_LOCAL_RELAY_OFFER, 0x19);
    assert_eq!(LOCAL_RELAY_LINKED, 0x01);
    assert_eq!(LOCAL_RELAY_ON_WALL, 0x02);
    assert_eq!(LOCAL_RELAY_INFO_MIN_SIZE, 7);
    assert_eq!(MAX_LOCAL_RELAY_ORIGIN_LEN, 128);
    assert_eq!(MAX_LOCAL_RELAY_HANDLE_LEN, 64);
    assert_eq!(MAX_LOCAL_RELAY_URL_LEN, 256);
    assert_eq!(MAX_LOCAL_RELAY_CANDIDATES, 4);
    assert_eq!(MAX_LOCAL_RELAY_HASHES, 2);
    assert_eq!(CLOSE_CODE_POP_UPLINK_FULL, 4012);
    assert_eq!(CLOSE_CODE_POP_NO_DEMAND, 4013);
    // 4012 is NOT terminal for a viewer: it retries every 20 s.
    assert!(!terminal_for_viewer(CLOSE_CODE_POP_UPLINK_FULL));
    assert!(!terminal_for_viewer(CLOSE_CODE_POP_NO_DEMAND));
}
