//! The golden vectors from gawk-server/wire/wire_test.go (plus parity_test.go
//! and stripe_test.go), restated here rather than imported or generated: an
//! exported fixture the mirrors shared could be edited once and stay green
//! everywhere, which would defeat the purpose. These are the bytes the relay,
//! wire.ts, and gawk-broadcast/internal/wirecheck already agree on.
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
const GOLDEN_STRIPE_STATE_STRIPED: &str = "0110010300";
const GOLDEN_STRIPE_STATE_UNSTRIPED: &str = "0110000000";

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
    assert_eq!(MAX_STRIPE_LEGS, 4);
    assert_eq!(CAP_PARITY_CHUNKS, 1 << 0);
    assert_eq!(CAP_STRIPED_DELIVERY, 1 << 1);

    assert_eq!(CLOSE_CODE_BROADCAST_ENDED, 4000);
    assert_eq!(CLOSE_CODE_SUBSCRIBER_UNRESPONSIVE, 4001);
    assert_eq!(CLOSE_CODE_SERVER_DRAINING, 4002);
    assert_eq!(CLOSE_CODE_ORIGIN_MOVED, 4003);
    assert_eq!(CLOSE_CODE_PUBLISHER_SUPERSEDED, 4004);
    assert_eq!(CLOSE_CODE_STRIPE_LEG_ORPHANED, 4005);

    assert_eq!(BROADCAST_ID_ALPHABET, "23456789ABCDEFGHJKMNPQRSTUVWXYZ");
    assert_eq!(BROADCAST_ID_ALPHABET.len(), 31);
}
