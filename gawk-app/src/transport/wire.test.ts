import { describe, expect, it } from 'vitest';

import {
  MAX_CHUNK_COUNT,
  MAX_CHUNK_PAYLOAD,
  MAX_DATAGRAM_SIZE,
  MAX_KEYFRAME_BYTES,
  STREAM_FRAME_HEADER_SIZE,
  TYPE_DECODER_CONFIG,
  TYPE_STREAM_FRAME,
  TYPE_VIDEO_CHUNK,
  TYPE_BROADCAST_ANNOUNCE,
  TYPE_TIME_SYNC,
  TYPE_CLOCK_MAPPING,
  TYPE_RESUME_TOKEN,
  TYPE_RELIABLE_CARRIER,
  TYPE_DELIVERY_ACK,
  DELIVERY_ACK_SIZE,
  encodeDeliveryAck,
  parseDeliveryAck,
  encodeTelemetryHello,
  parseTelemetryHello,
  telemetrySessionId,
  TELEMETRY_HELLO_SIZE,
  encodeRelayIdentity,
  parseRelayIdentity,
  encodeTelemetryEndpoint,
  parseTelemetryEndpoint,
  TELEMETRY_SESSION_ID_LEN,
  TYPE_STRIPE_STATE,
  STRIPE_STATE_SIZE,
  MAX_STRIPE_LEGS,
  encodeStripeState,
  parseStripeState,
  stripeOrdinal,
  TYPE_VIEWER_COUNT,
  TIME_SYNC_SIZE,
  CLOCK_MAPPING_SIZE,
  VIEWER_COUNT_SIZE,
  CARRIER_PROLOGUE_SIZE,
  CARRIER_RECORD_HEADER_SIZE,
  CLOSE_CODE_BROADCAST_ENDED,
  CLOSE_CODE_SERVER_DRAINING,
  CLOSE_CODE_ORIGIN_MOVED,
  CLOSE_CODE_PUBLISHER_SUPERSEDED,
  CLOSE_CODE_STRIPE_LEG_ORPHANED,
  CLOSE_CODE_TERMINATED_BY_OPERATOR,
  CLOSE_CODE_ROOM_ENDED,
  TYPE_ROOM_HELLO,
  TYPE_ROOM_STATE,
  TYPE_ROOM_EVENT,
  TYPE_ROOM_COMMAND,
  ROOM_PROTOCOL_VERSION,
  ROOM_RECORD_HEADER_SIZE,
  MAX_ROOM_RECORD_SIZE,
  MAX_ROOM_NICKNAME_LEN,
  MAX_ROOM_CODE_LEN,
  MAX_ROOM_DISPLAY_NAME_LEN,
  MAX_ROOM_LABEL_LEN,
  MAX_ROOM_IDENTITY_LEN,
  MAX_ROOM_REJECT_MESSAGE_LEN,
  ROOM_CREATOR_TOKEN_SIZE,
  ROOM_KEY_SIZE,
  RESUME_TOKEN_SIZE,
  ROOM_CLIENT_WEB_VIEWER,
  ROOM_CLIENT_WEB_BROADCASTER,
  ROOM_CLIENT_NATIVE,
  ROOM_STATE_FLAG_DYNAMIC,
  ROOM_STATE_FLAG_CREATOR,
  ROOM_STATE_FLAG_ATTACH_OK,
  ROOM_PARTICIPANT_FLAG_STREAMING,
  ROOM_EVENT_PARTICIPANT_JOINED,
  ROOM_EVENT_PARTICIPANT_LEFT,
  ROOM_EVENT_ATTACHMENT_UPDATED,
  ROOM_EVENT_ATTACHMENT_REMOVED,
  ROOM_EVENT_ROOM_ENDING,
  ROOM_EVENT_COMMAND_REJECTED,
  ROOM_COMMAND_ATTACH,
  ROOM_COMMAND_DETACH,
  ROOM_COMMAND_SET_NICKNAME,
  ROOM_COMMAND_END_ROOM,
  ROOM_COMMAND_RESYNC,
  ROOM_END_REASON_CREATOR,
  ROOM_DETACH_REASON_EXPIRED,
  ROOM_REJECT_LIMIT,
  RoomUnknownKindError,
  RoomRecordReader,
  encodeRoomRecord,
  parseRoomRecordLength,
  encodeRoomHello,
  parseRoomHello,
  encodeRoomState,
  parseRoomState,
  encodeRoomEvent,
  parseRoomEvent,
  encodeRoomCommand,
  parseRoomCommand,
  normalizeBroadcastId,
  type RoomHello,
  type RoomState,
  type RoomEvent,
  type RoomCommand,
  VIDEO_CHUNK_HEADER_SIZE,
  WIRE_VERSION,
  WireError,
  CarrierRecordParser,
  encodeCarrierPrologue,
  encodeCarrierRecord,
  parseCarrierPrologue,
  encodeClockMapping,
  encodeResumeToken,
  parseResumeToken,
  encodeDecoderConfig,
  encodeStreamFrame,
  encodeStreamFrameHeader,
  encodeTimeSync,
  encodeVideoChunk,
  parseClockMapping,
  encodeViewerCount,
  parseViewerCount,
  parseDecoderConfig,
  parseStreamFrameHeader,
  parseTimeSync,
  parseVideoChunk,
  peekType,
  parseBroadcastAnnounce,
  TYPE_AUDIO_FRAME,
  TYPE_AUDIO_CONFIG,
  AUDIO_FRAME_HEADER_SIZE,
  MAX_AUDIO_PAYLOAD,
  encodeAudioFrame,
  parseAudioFrame,
  encodeAudioConfig,
  parseAudioConfig,
  type AudioConfigMessage,
  type AudioFrameHeader,
  type DecoderConfigMessage,
  type StreamFrameHeader,
  type TimeSyncMessage,
  type VideoChunkHeader,
} from './wire';

// Golden vectors copied verbatim from gawk-server/internal/wire/wire_test.go
// — the cross-language portability guarantee. Do not regenerate them from
// code; if they change, the wire format changed.
const GOLDEN_VIDEO_CHUNK_HEX = '0101010001020304000500820000005d21dba5f0616263';
const GOLDEN_DECODER_CONFIG_AVC_HEX = '0102000b617663312e3432453032410142e02affe1';
const GOLDEN_DECODER_CONFIG_VP8_HEX = '01020003767038';
const GOLDEN_BROADCAST_ANNOUNCE_HEX = '0103064b375851324d';
const GOLDEN_STREAM_FRAME_HEADER_HEX = '01040100010203040000005d21dba5f00000000600000003';
const GOLDEN_TIME_SYNC_HEX = '01050102030405060708090a0b0c0d0e0f10';
const GOLDEN_TIME_SYNC_REQUEST_HEX = '010500000000000f42400000000000000000';
const GOLDEN_CLOCK_MAPPING_HEX = '0106000000000016e360';
const GOLDEN_CLOCK_MAPPING_NEGATIVE_HEX = '0106fffffffffff0bdc0';
// R19 reliable-carrier framing (docs/24 Decision 3).
const GOLDEN_CARRIER_PROLOGUE_HEX = '010a';
const GOLDEN_CARRIER_RECORD_HEX = '0017' + GOLDEN_VIDEO_CHUNK_HEX;
// The length prefix of a record at the inclusive upper boundary: a full delta
// chunk is exactly MAX_DATAGRAM_SIZE (1200 = 0x04b0).
const GOLDEN_CARRIER_MAX_RECORD_PREFIX_HEX = '04b0';
// R18 viewer count (docs/23 Decision 2).
const GOLDEN_VIEWER_COUNT_HEX = '010b00000003';
const GOLDEN_VIEWER_COUNT_LARGE_HEX = '010b01020304';

const GOLDEN_AUDIO_FRAME_HEX = '01070000010203040000005d21dba5f0616263';
const GOLDEN_AUDIO_CONFIG_HEX = '010800046f7075730000bb8002';
const GOLDEN_AUDIO_CONFIG_DESC_HEX = '010800046f7075730000bb8002010203';

const goldenStreamFrameHeader: StreamFrameHeader = {
  keyframe: true,
  frameId: 0x01020304,
  timestampUs: 0x0000005d21dba5f0n,
  configLen: 6,
  payloadLen: 3,
};

const goldenVideoChunkHeader: VideoChunkHeader = {
  keyframe: true,
  frameId: 0x01020304,
  chunkIndex: 5,
  chunkCount: 130,
  timestampUs: 0x0000005d21dba5f0n,
};
const goldenVideoChunkPayload = new TextEncoder().encode('abc');

const goldenDecoderConfigAvc: DecoderConfigMessage = {
  codec: 'avc1.42E02A',
  extradata: new Uint8Array([0x01, 0x42, 0xe0, 0x2a, 0xff, 0xe1]),
};
const goldenDecoderConfigVp8: DecoderConfigMessage = {
  codec: 'vp8',
  extradata: new Uint8Array(0),
};

function fromHex(hex: string): Uint8Array {
  const out = new Uint8Array(hex.length / 2);
  for (let i = 0; i < out.length; i++) {
    out[i] = parseInt(hex.slice(i * 2, i * 2 + 2), 16);
  }
  return out;
}

function toHex(bytes: Uint8Array): string {
  return Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join('');
}

const goldenTimeSync: TimeSyncMessage = {
  clientTimeUs: 0x0102030405060708n,
  serverTimeUs: 0x090a0b0c0d0e0f10n,
};
const goldenTimeSyncRequest: TimeSyncMessage = {
  clientTimeUs: 1_000_000n,
  serverTimeUs: 0n,
};
const GOLDEN_CLOCK_MAPPING_OFFSET_US = 1_500_000n;
const GOLDEN_CLOCK_MAPPING_NEGATIVE_OFFSET_US = -1_000_000n;

const goldenAudioFrameHeader: AudioFrameHeader = {
  seq: 0x01020304,
  timestampUs: 0x0000005d21dba5f0n,
};
const goldenAudioFramePayload = new TextEncoder().encode('abc');

const goldenAudioConfig: AudioConfigMessage = {
  codec: 'opus',
  sampleRate: 48000,
  channels: 2,
  description: new Uint8Array(0),
};
const goldenAudioConfigDesc: AudioConfigMessage = {
  codec: 'opus',
  sampleRate: 48000,
  channels: 2,
  description: new Uint8Array([0x01, 0x02, 0x03]),
};

describe('constants', () => {
  it('match the Go package', () => {
    expect(MAX_DATAGRAM_SIZE).toBe(1200);
    expect(VIDEO_CHUNK_HEADER_SIZE).toBe(20);
    expect(MAX_CHUNK_PAYLOAD).toBe(1180);
    expect(MAX_CHUNK_COUNT).toBe(3000);
    expect(TYPE_BROADCAST_ANNOUNCE).toBe(0x03);
    expect(TYPE_STREAM_FRAME).toBe(0x04);
    expect(STREAM_FRAME_HEADER_SIZE).toBe(24);
    expect(MAX_KEYFRAME_BYTES).toBe(8 * 1024 * 1024);
    expect(CLOSE_CODE_BROADCAST_ENDED).toBe(4000);
    expect(CLOSE_CODE_PUBLISHER_SUPERSEDED).toBe(4004);
    expect(TYPE_TIME_SYNC).toBe(0x05);
    expect(TYPE_CLOCK_MAPPING).toBe(0x06);
    expect(TYPE_RESUME_TOKEN).toBe(0x09);
    expect(TIME_SYNC_SIZE).toBe(18);
    expect(CLOCK_MAPPING_SIZE).toBe(10);
    expect(CLOSE_CODE_SERVER_DRAINING).toBe(4002);
    expect(CLOSE_CODE_ORIGIN_MOVED).toBe(4003);
    expect(CLOSE_CODE_STRIPE_LEG_ORPHANED).toBe(4005);
    expect(CLOSE_CODE_TERMINATED_BY_OPERATOR).toBe(4006);
    expect(CLOSE_CODE_ROOM_ENDED).toBe(4007);
    expect(TYPE_ROOM_HELLO).toBe(0x13);
    expect(TYPE_ROOM_STATE).toBe(0x14);
    expect(TYPE_ROOM_EVENT).toBe(0x15);
    expect(TYPE_ROOM_COMMAND).toBe(0x16);
    expect(TYPE_RELIABLE_CARRIER).toBe(0x0a);
    expect(TYPE_DELIVERY_ACK).toBe(0x0c);
    expect(DELIVERY_ACK_SIZE).toBe(5);
    expect(CARRIER_PROLOGUE_SIZE).toBe(2);
    expect(CARRIER_RECORD_HEADER_SIZE).toBe(2);
    expect(TYPE_VIEWER_COUNT).toBe(0x0b);
    expect(VIEWER_COUNT_SIZE).toBe(6);
    expect(TYPE_AUDIO_FRAME).toBe(0x07);
    expect(TYPE_AUDIO_CONFIG).toBe(0x08);
    expect(AUDIO_FRAME_HEADER_SIZE).toBe(16);
    expect(MAX_AUDIO_PAYLOAD).toBe(1184);
  });
});

describe('golden vectors', () => {
  it('encodes the golden video chunk byte-for-byte', () => {
    const dgram = encodeVideoChunk(goldenVideoChunkHeader, goldenVideoChunkPayload);
    expect(toHex(dgram)).toBe(GOLDEN_VIDEO_CHUNK_HEX);
  });

  it('parses the golden video chunk', () => {
    const { header, payload } = parseVideoChunk(fromHex(GOLDEN_VIDEO_CHUNK_HEX));
    expect(header).toEqual(goldenVideoChunkHeader);
    expect(new TextDecoder().decode(payload)).toBe('abc');
  });

  it('encodes the golden decoder configs byte-for-byte', () => {
    expect(toHex(encodeDecoderConfig(goldenDecoderConfigAvc))).toBe(GOLDEN_DECODER_CONFIG_AVC_HEX);
    expect(toHex(encodeDecoderConfig(goldenDecoderConfigVp8))).toBe(GOLDEN_DECODER_CONFIG_VP8_HEX);
  });

  it('parses the golden decoder configs', () => {
    const avc = parseDecoderConfig(fromHex(GOLDEN_DECODER_CONFIG_AVC_HEX));
    expect(avc.codec).toBe('avc1.42E02A');
    expect(toHex(avc.extradata)).toBe('0142e02affe1');

    const vp8 = parseDecoderConfig(fromHex(GOLDEN_DECODER_CONFIG_VP8_HEX));
    expect(vp8.codec).toBe('vp8');
    expect(vp8.extradata.length).toBe(0);
  });

  it('encodes and parses the golden time syncs byte-for-byte (R5 Q2)', () => {
    expect(toHex(encodeTimeSync(goldenTimeSync))).toBe(GOLDEN_TIME_SYNC_HEX);
    expect(toHex(encodeTimeSync(goldenTimeSyncRequest))).toBe(GOLDEN_TIME_SYNC_REQUEST_HEX);
    expect(parseTimeSync(fromHex(GOLDEN_TIME_SYNC_HEX))).toEqual(goldenTimeSync);
    expect(parseTimeSync(fromHex(GOLDEN_TIME_SYNC_REQUEST_HEX))).toEqual(goldenTimeSyncRequest);
  });

  it('encodes and parses the golden clock mappings byte-for-byte (R5 Q2)', () => {
    expect(toHex(encodeClockMapping(GOLDEN_CLOCK_MAPPING_OFFSET_US))).toBe(GOLDEN_CLOCK_MAPPING_HEX);
    expect(toHex(encodeClockMapping(GOLDEN_CLOCK_MAPPING_NEGATIVE_OFFSET_US))).toBe(
      GOLDEN_CLOCK_MAPPING_NEGATIVE_HEX,
    );
    expect(parseClockMapping(fromHex(GOLDEN_CLOCK_MAPPING_HEX))).toBe(GOLDEN_CLOCK_MAPPING_OFFSET_US);
    expect(parseClockMapping(fromHex(GOLDEN_CLOCK_MAPPING_NEGATIVE_HEX))).toBe(
      GOLDEN_CLOCK_MAPPING_NEGATIVE_OFFSET_US,
    );
  });

  it('rejects malformed time syncs and clock mappings (strict length)', () => {
    // Truncated, oversize, wrong version, wrong type — all drop, never trust.
    expect(() => parseTimeSync(fromHex(GOLDEN_TIME_SYNC_HEX.slice(0, -2)))).toThrow(WireError);
    expect(() => parseTimeSync(fromHex(GOLDEN_TIME_SYNC_HEX + '00'))).toThrow(WireError);
    expect(() => parseTimeSync(fromHex('02' + GOLDEN_TIME_SYNC_HEX.slice(2)))).toThrow(WireError);
    expect(() => parseTimeSync(fromHex(GOLDEN_CLOCK_MAPPING_HEX))).toThrow(WireError);
    expect(() => parseClockMapping(fromHex(GOLDEN_CLOCK_MAPPING_HEX.slice(0, -2)))).toThrow(WireError);
    expect(() => parseClockMapping(fromHex(GOLDEN_CLOCK_MAPPING_HEX + '00'))).toThrow(WireError);
    expect(() => parseClockMapping(fromHex('02' + GOLDEN_CLOCK_MAPPING_HEX.slice(2)))).toThrow(WireError);
    expect(() => parseClockMapping(fromHex(GOLDEN_TIME_SYNC_HEX))).toThrow(WireError);
  });

  it('encodes and parses the golden viewer counts byte-for-byte (R18)', () => {
    expect(toHex(encodeViewerCount(3))).toBe(GOLDEN_VIEWER_COUNT_HEX);
    expect(toHex(encodeViewerCount(0x01020304))).toBe(GOLDEN_VIEWER_COUNT_LARGE_HEX);
    expect(parseViewerCount(fromHex(GOLDEN_VIEWER_COUNT_HEX))).toBe(3);
    expect(parseViewerCount(fromHex(GOLDEN_VIEWER_COUNT_LARGE_HEX))).toBe(0x01020304);
  });

  it('round-trips viewer counts including zero', () => {
    for (const count of [0, 1, 15, 500, 0xffffffff]) {
      expect(parseViewerCount(encodeViewerCount(count))).toBe(count);
    }
  });

  it('rejects malformed viewer counts strictly', () => {
    expect(() => parseViewerCount(fromHex(GOLDEN_VIEWER_COUNT_HEX.slice(0, -2)))).toThrow(WireError);
    expect(() => parseViewerCount(fromHex(GOLDEN_VIEWER_COUNT_HEX + '00'))).toThrow(WireError);
    expect(() => parseViewerCount(fromHex('02' + GOLDEN_VIEWER_COUNT_HEX.slice(2)))).toThrow(WireError);
    // Right length, wrong type (a clock mapping truncated to 6 bytes).
    expect(() => parseViewerCount(fromHex(GOLDEN_CLOCK_MAPPING_HEX.slice(0, VIEWER_COUNT_SIZE * 2)))).toThrow(
      WireError,
    );
  });

  it('encodes the golden audio frame byte-for-byte (R15)', () => {
    const dgram = encodeAudioFrame(goldenAudioFrameHeader, goldenAudioFramePayload);
    expect(toHex(dgram)).toBe(GOLDEN_AUDIO_FRAME_HEX);
  });

  it('parses the golden audio frame', () => {
    const { header, payload } = parseAudioFrame(fromHex(GOLDEN_AUDIO_FRAME_HEX));
    expect(header).toEqual(goldenAudioFrameHeader);
    expect(new TextDecoder().decode(payload)).toBe('abc');
  });

  it('encodes the golden audio configs byte-for-byte (R15)', () => {
    expect(toHex(encodeAudioConfig(goldenAudioConfig))).toBe(GOLDEN_AUDIO_CONFIG_HEX);
    expect(toHex(encodeAudioConfig(goldenAudioConfigDesc))).toBe(GOLDEN_AUDIO_CONFIG_DESC_HEX);
  });

  it('parses the golden audio configs', () => {
    const cfg = parseAudioConfig(fromHex(GOLDEN_AUDIO_CONFIG_HEX));
    expect(cfg.codec).toBe('opus');
    expect(cfg.sampleRate).toBe(48000);
    expect(cfg.channels).toBe(2);
    expect(cfg.description.length).toBe(0);

    const withDesc = parseAudioConfig(fromHex(GOLDEN_AUDIO_CONFIG_DESC_HEX));
    expect(toHex(withDesc.description)).toBe('010203');
  });

  it('rejects malformed audio frames strictly', () => {
    // Truncated header, empty payload, wrong version, wrong type.
    expect(() => parseAudioFrame(fromHex(GOLDEN_AUDIO_FRAME_HEX.slice(0, (AUDIO_FRAME_HEADER_SIZE - 1) * 2)))).toThrow(
      WireError,
    );
    expect(() => parseAudioFrame(fromHex(GOLDEN_AUDIO_FRAME_HEX.slice(0, AUDIO_FRAME_HEADER_SIZE * 2)))).toThrow(
      WireError,
    );
    expect(() => parseAudioFrame(fromHex('02' + GOLDEN_AUDIO_FRAME_HEX.slice(2)))).toThrow(WireError);
    expect(() => parseAudioFrame(fromHex(GOLDEN_VIDEO_CHUNK_HEX))).toThrow(WireError);
    // Encoder refuses empty and oversize payloads.
    expect(() => encodeAudioFrame(goldenAudioFrameHeader, new Uint8Array(0))).toThrow(WireError);
    expect(() => encodeAudioFrame(goldenAudioFrameHeader, new Uint8Array(MAX_AUDIO_PAYLOAD + 1))).toThrow(
      WireError,
    );
  });

  it('rejects malformed audio configs strictly', () => {
    // Truncated tail, wrong version, wrong type, empty codec, overrunning
    // codecLen, zero sampleRate, zero channels.
    expect(() => parseAudioConfig(fromHex(GOLDEN_AUDIO_CONFIG_HEX.slice(0, -2)))).toThrow(WireError);
    expect(() => parseAudioConfig(fromHex('02' + GOLDEN_AUDIO_CONFIG_HEX.slice(2)))).toThrow(WireError);
    expect(() => parseAudioConfig(fromHex(GOLDEN_DECODER_CONFIG_VP8_HEX))).toThrow(WireError);
    expect(() => parseAudioConfig(fromHex('010800' + '00' + GOLDEN_AUDIO_CONFIG_HEX.slice(8)))).toThrow(
      WireError,
    );
    expect(() => parseAudioConfig(fromHex('0108' + '00ff' + GOLDEN_AUDIO_CONFIG_HEX.slice(8)))).toThrow(
      WireError,
    );
    expect(() => parseAudioConfig(fromHex('010800046f707573' + '00000000' + '02'))).toThrow(WireError);
    expect(() => parseAudioConfig(fromHex('010800046f707573' + '0000bb80' + '00'))).toThrow(WireError);
    expect(() => encodeAudioConfig({ ...goldenAudioConfig, sampleRate: 0 })).toThrow(WireError);
    expect(() => encodeAudioConfig({ ...goldenAudioConfig, channels: 0 })).toThrow(WireError);
    expect(() => encodeAudioConfig({ ...goldenAudioConfig, codec: '' })).toThrow(WireError);
  });

  it('round-trips a max-payload audio frame at exactly MAX_DATAGRAM_SIZE', () => {
    const payload = new Uint8Array(MAX_AUDIO_PAYLOAD).fill(0x5a);
    const dgram = encodeAudioFrame({ seq: 0xffffffff, timestampUs: 1n }, payload);
    expect(dgram.length).toBe(MAX_DATAGRAM_SIZE);
    const parsed = parseAudioFrame(dgram);
    expect(parsed.header.seq).toBe(0xffffffff);
    expect(parsed.payload.length).toBe(MAX_AUDIO_PAYLOAD);
  });

  it('parses the golden broadcast announce', () => {
    const id = parseBroadcastAnnounce(fromHex(GOLDEN_BROADCAST_ANNOUNCE_HEX));
    expect(id).toBe('K7XQ2M');
  });

  it('encodes the golden stream frame header byte-for-byte', () => {
    expect(toHex(encodeStreamFrameHeader(goldenStreamFrameHeader))).toBe(GOLDEN_STREAM_FRAME_HEADER_HEX);
  });

  it('parses the golden stream frame header', () => {
    expect(parseStreamFrameHeader(fromHex(GOLDEN_STREAM_FRAME_HEADER_HEX))).toEqual(goldenStreamFrameHeader);
  });

  it('encodes and parses the golden carrier prologue + record byte-for-byte (R19)', () => {
    expect(toHex(encodeCarrierPrologue())).toBe(GOLDEN_CARRIER_PROLOGUE_HEX);
    expect(() => parseCarrierPrologue(fromHex(GOLDEN_CARRIER_PROLOGUE_HEX))).not.toThrow();

    const dgram = fromHex(GOLDEN_VIDEO_CHUNK_HEX);
    expect(toHex(encodeCarrierRecord(dgram))).toBe(GOLDEN_CARRIER_RECORD_HEX);

    const parser = new CarrierRecordParser();
    const records = parser.push(fromHex(GOLDEN_CARRIER_RECORD_HEX));
    expect(records.length).toBe(1);
    expect(toHex(records[0])).toBe(GOLDEN_VIDEO_CHUNK_HEX);
    expect(parser.hasPartial()).toBe(false);
  });
});

describe('carrier record parser (R19)', () => {
  it('reassembles records split across arbitrary chunk boundaries', () => {
    const a = encodeCarrierRecord(fromHex(GOLDEN_VIDEO_CHUNK_HEX));
    const b = encodeCarrierRecord(fromHex(GOLDEN_DECODER_CONFIG_AVC_HEX));
    const stream = new Uint8Array(a.length + b.length);
    stream.set(a, 0);
    stream.set(b, a.length);

    // Every possible split point yields the same two records.
    for (let split = 0; split <= stream.length; split++) {
      const parser = new CarrierRecordParser();
      const records = [
        ...parser.push(stream.subarray(0, split)),
        ...parser.push(stream.subarray(split)),
      ];
      expect(records.map(toHex)).toEqual([GOLDEN_VIDEO_CHUNK_HEX, GOLDEN_DECODER_CONFIG_AVC_HEX]);
      expect(parser.hasPartial()).toBe(false);
    }
  });

  it('returns copies that own their buffers (transfer-safe)', () => {
    const parser = new CarrierRecordParser();
    const [record] = parser.push(encodeCarrierRecord(new Uint8Array([1, 2, 3])));
    expect(record.byteOffset).toBe(0);
    expect(record.buffer.byteLength).toBe(3);
  });

  it('reports a partial record buffered at a truncation point', () => {
    const parser = new CarrierRecordParser();
    const record = encodeCarrierRecord(fromHex(GOLDEN_VIDEO_CHUNK_HEX));
    expect(parser.push(record.subarray(0, 5))).toEqual([]);
    expect(parser.hasPartial()).toBe(true);
  });

  it('throws on a zero or oversize declared length', () => {
    expect(() => new CarrierRecordParser().push(new Uint8Array([0x00, 0x00, 0x42]))).toThrow(WireError);
    const oversize = new Uint8Array(2);
    new DataView(oversize.buffer).setUint16(0, MAX_DATAGRAM_SIZE + 1);
    expect(() => new CarrierRecordParser().push(oversize)).toThrow(WireError);
  });

  // The inclusive upper boundary of the uint16 length prefix. A full delta
  // chunk is exactly MAX_DATAGRAM_SIZE, so this is the *common* record on a
  // carrier — not an exotic edge — and it must survive both the framing and a
  // read split landing on the boundary itself (docs/24 finding 15).
  it('round-trips a record whose datagram is exactly MAX_DATAGRAM_SIZE', () => {
    const dgram = encodeVideoChunk(
      { keyframe: false, frameId: 43, chunkIndex: 1, chunkCount: 2, timestampUs: 7654321n },
      new Uint8Array(MAX_CHUNK_PAYLOAD).fill(0xab),
    );
    expect(dgram.length).toBe(MAX_DATAGRAM_SIZE);

    const record = encodeCarrierRecord(dgram);
    expect(record.length).toBe(CARRIER_RECORD_HEADER_SIZE + MAX_DATAGRAM_SIZE);
    expect(toHex(record.subarray(0, CARRIER_RECORD_HEADER_SIZE))).toBe(
      GOLDEN_CARRIER_MAX_RECORD_PREFIX_HEX,
    );

    // A second record behind it: the boundary must not swallow or truncate
    // what follows on the same carrier stream.
    const trailer = encodeCarrierRecord(fromHex(GOLDEN_VIDEO_CHUNK_HEX));
    const stream = new Uint8Array(record.length + trailer.length);
    stream.set(record, 0);
    stream.set(trailer, record.length);

    for (const split of [record.length - 1, record.length, record.length + 1]) {
      const parser = new CarrierRecordParser();
      const records = [
        ...parser.push(stream.subarray(0, split)),
        ...parser.push(stream.subarray(split)),
      ];
      expect(records.map(toHex)).toEqual([toHex(dgram), GOLDEN_VIDEO_CHUNK_HEX]);
      expect(parser.hasPartial()).toBe(false);
    }
  });

  it('rejects oversize and empty datagrams at encode', () => {
    expect(() => encodeCarrierRecord(new Uint8Array(0))).toThrow(WireError);
    expect(() => encodeCarrierRecord(new Uint8Array(MAX_DATAGRAM_SIZE + 1))).toThrow(WireError);
  });

  it('rejects a bad prologue', () => {
    expect(() => parseCarrierPrologue(new Uint8Array([0x01]))).toThrow(WireError);
    expect(() => parseCarrierPrologue(new Uint8Array([0x02, 0x0a]))).toThrow(WireError);
    expect(() => parseCarrierPrologue(new Uint8Array([0x01, 0x04]))).toThrow(WireError);
  });
});

describe('stream frame round trip', () => {
  it('encodes header + config + payload and parses the header back', () => {
    const config = new Uint8Array([0x01, 0x02, 0x00, 0x03, 0x76, 0x70, 0x38]); // a vp8 DecoderConfig datagram
    const payload = new Uint8Array([0xde, 0xad, 0xbe, 0xef, 0x00]);
    const msg = encodeStreamFrame({ keyframe: true, frameId: 7, timestampUs: 99n }, config, payload);

    expect(peekType(msg).msgType).toBe(TYPE_STREAM_FRAME);
    const header = parseStreamFrameHeader(msg);
    expect(header).toEqual({
      keyframe: true,
      frameId: 7,
      timestampUs: 99n,
      configLen: config.length,
      payloadLen: payload.length,
    });
    // Body slices land where the lengths say.
    const cfg = msg.subarray(STREAM_FRAME_HEADER_SIZE, STREAM_FRAME_HEADER_SIZE + header.configLen);
    const pay = msg.subarray(STREAM_FRAME_HEADER_SIZE + header.configLen);
    expect(toHex(cfg)).toBe(toHex(config));
    expect(toHex(pay)).toBe(toHex(payload));
  });

  it('supports a keyframe with no embedded config', () => {
    const payload = new Uint8Array([1, 2, 3]);
    const msg = encodeStreamFrame({ keyframe: true, frameId: 0, timestampUs: 0n }, new Uint8Array(0), payload);
    const header = parseStreamFrameHeader(msg);
    expect(header.configLen).toBe(0);
    expect(header.payloadLen).toBe(3);
  });

  it('rejects headers that are too short or mistyped', () => {
    expect(() => parseStreamFrameHeader(new Uint8Array(23))).toThrow(/too short/);
    const badType = encodeStreamFrameHeader(goldenStreamFrameHeader);
    badType[1] = 0x01;
    expect(() => parseStreamFrameHeader(badType)).toThrow(/stream frame/);
  });

  it('rejects a declared body exceeding MAX_KEYFRAME_BYTES', () => {
    expect(() =>
      encodeStreamFrameHeader({ keyframe: true, frameId: 0, timestampUs: 0n, configLen: 0, payloadLen: MAX_KEYFRAME_BYTES }),
    ).toThrow(WireError);
    // Parse must also reject an untrusted oversize declaration.
    const buf = encodeStreamFrameHeader({ keyframe: true, frameId: 0, timestampUs: 0n, configLen: 0, payloadLen: 1 });
    new DataView(buf.buffer).setUint32(20, MAX_KEYFRAME_BYTES);
    expect(() => parseStreamFrameHeader(buf)).toThrow(WireError);
  });
});

describe('peekType', () => {
  it('reports version and type without validating them', () => {
    expect(peekType(fromHex(GOLDEN_VIDEO_CHUNK_HEX))).toEqual({
      version: WIRE_VERSION,
      msgType: TYPE_VIDEO_CHUNK,
    });
    expect(peekType(new Uint8Array([0xff, 0xee]))).toEqual({ version: 0xff, msgType: 0xee });
  });

  it('throws on datagrams shorter than the prefix', () => {
    expect(() => peekType(new Uint8Array(0))).toThrow(WireError);
    expect(() => peekType(new Uint8Array([0x01]))).toThrow(WireError);
  });
});

describe('video chunk round trip', () => {
  it.each([
    ['keyframe', { keyframe: true, frameId: 42, chunkIndex: 0, chunkCount: 3, timestampUs: 1234567n }, new TextEncoder().encode('hello')],
    ['delta frame', { keyframe: false, frameId: 43, chunkIndex: 2, chunkCount: 3, timestampUs: 7654321n }, new Uint8Array([0xde, 0xad, 0xbe, 0xef])],
    ['empty payload', { keyframe: false, frameId: 0, chunkIndex: 0, chunkCount: 1, timestampUs: 0n }, new Uint8Array(0)],
    ['max payload', { keyframe: true, frameId: 0xffffffff, chunkIndex: MAX_CHUNK_COUNT - 1, chunkCount: MAX_CHUNK_COUNT, timestampUs: 0xffffffffffffffffn }, new Uint8Array(MAX_CHUNK_PAYLOAD).fill(0xab)],
  ] satisfies [string, VideoChunkHeader, Uint8Array][])('%s', (_name, header, payload) => {
    const dgram = encodeVideoChunk(header, payload);
    expect(dgram.length).toBeLessThanOrEqual(MAX_DATAGRAM_SIZE);
    const parsed = parseVideoChunk(dgram);
    expect(parsed.header).toEqual(header);
    expect(toHex(parsed.payload)).toBe(toHex(payload));
  });

  it('parses payloads from subarray views with a non-zero byteOffset', () => {
    const dgram = encodeVideoChunk(goldenVideoChunkHeader, goldenVideoChunkPayload);
    const padded = new Uint8Array(dgram.length + 7);
    padded.set(dgram, 7);
    const { header, payload } = parseVideoChunk(padded.subarray(7));
    expect(header).toEqual(goldenVideoChunkHeader);
    expect(new TextDecoder().decode(payload)).toBe('abc');
  });
});

describe('decoder config round trip', () => {
  it.each([
    ['h264 with extradata', { codec: 'avc1.640028', extradata: new Uint8Array([0x01, 0x64, 0x00, 0x28]) }],
    ['vp9 empty extradata', { codec: 'vp09.00.40.08', extradata: new Uint8Array(0) }],
    ['single char codec', { codec: 'x', extradata: new Uint8Array([0x00]) }],
  ] satisfies [string, DecoderConfigMessage][])('%s', (_name, config) => {
    const dgram = encodeDecoderConfig(config);
    expect(peekType(dgram).msgType).toBe(TYPE_DECODER_CONFIG);
    const parsed = parseDecoderConfig(dgram);
    expect(parsed.codec).toBe(config.codec);
    expect(toHex(parsed.extradata)).toBe(toHex(config.extradata));
  });
});

describe('error cases', () => {
  const validChunk = fromHex(GOLDEN_VIDEO_CHUNK_HEX);

  it('rejects malformed video chunks', () => {
    expect(() => parseVideoChunk(new Uint8Array(0))).toThrow(/too short/);
    expect(() => parseVideoChunk(validChunk.subarray(0, 19))).toThrow(/too short/);
    const badVersion = validChunk.slice();
    badVersion[0] = 0x02;
    expect(() => parseVideoChunk(badVersion)).toThrow(/version/);
    const badType = validChunk.slice();
    badType[1] = TYPE_DECODER_CONFIG;
    expect(() => parseVideoChunk(badType)).toThrow(/type/);
    const zeroCount = validChunk.slice();
    zeroCount[10] = 0;
    zeroCount[11] = 0;
    expect(() => parseVideoChunk(zeroCount)).toThrow(/index/);
    const indexBeyondCount = validChunk.slice();
    indexBeyondCount[8] = 0;
    indexBeyondCount[9] = 130; // chunkIndex == chunkCount
    expect(() => parseVideoChunk(indexBeyondCount)).toThrow(/index/);
    // R2: the relay caps keyframe reassembly at MAX_CHUNK_COUNT chunks and
    // counts anything above it as a bad datagram; the TS side must agree.
    const overMaxCount = validChunk.slice();
    const view = new DataView(overMaxCount.buffer);
    view.setUint16(10, MAX_CHUNK_COUNT + 1);
    expect(() => parseVideoChunk(overMaxCount)).toThrow(/count/);
  });

  it('accepts an exactly header-sized chunk with empty payload', () => {
    const { payload } = parseVideoChunk(validChunk.subarray(0, VIDEO_CHUNK_HEADER_SIZE));
    expect(payload.length).toBe(0);
  });

  it('rejects invalid video chunk encode inputs', () => {
    const header = { keyframe: false, frameId: 0, chunkIndex: 0, chunkCount: 1, timestampUs: 0n };
    expect(() => encodeVideoChunk(header, new Uint8Array(MAX_CHUNK_PAYLOAD + 1))).toThrow(/payload/);
    expect(() => encodeVideoChunk({ ...header, chunkCount: 0 }, new Uint8Array(0))).toThrow(/index/);
    expect(() => encodeVideoChunk({ ...header, chunkIndex: 3, chunkCount: 3 }, new Uint8Array(0))).toThrow(/index/);
    // The broadcaster is the side that *produces* chunk counts: encoding a
    // frame the relay would drop as bad must fail loudly here, not silently
    // black-hole the keyframe server-side.
    expect(() => encodeVideoChunk({ ...header, chunkIndex: 0, chunkCount: MAX_CHUNK_COUNT + 1 }, new Uint8Array(0))).toThrow(/count/);
  });

  it('rejects malformed decoder configs', () => {
    expect(() => parseDecoderConfig(new Uint8Array([0x01, 0x02, 0x00]))).toThrow(/too short/);
    expect(() => parseDecoderConfig(new Uint8Array([0x02, 0x02, 0x00, 0x01, 0x78]))).toThrow(/version/);
    expect(() => parseDecoderConfig(new Uint8Array([0x01, 0x01, 0x00, 0x01, 0x78]))).toThrow(/type/);
    expect(() => parseDecoderConfig(new Uint8Array([0x01, 0x02, 0x00, 0x05, 0x76, 0x70, 0x38]))).toThrow(/overruns/);
    expect(() => parseDecoderConfig(new Uint8Array([0x01, 0x02, 0x00, 0x00]))).toThrow(/codec/);
  });

  it('rejects invalid decoder config encode inputs', () => {
    expect(() => encodeDecoderConfig({ codec: '', extradata: new Uint8Array(0) })).toThrow(/codec/);
    expect(() => encodeDecoderConfig({ codec: 'a'.repeat(256), extradata: new Uint8Array(0) })).toThrow(/codec/);
    expect(() =>
      encodeDecoderConfig({ codec: 'vp8', extradata: new Uint8Array(MAX_DATAGRAM_SIZE) }),
    ).toThrow(/exceeds/);
    // Exactly at the limit is fine: 4 + 3 + 1193 = 1200.
    const atLimit = encodeDecoderConfig({
      codec: 'vp8',
      extradata: new Uint8Array(MAX_DATAGRAM_SIZE - 4 - 3),
    });
    expect(atLimit.length).toBe(MAX_DATAGRAM_SIZE);
  });

  it('rejects malformed broadcast announces', () => {
    expect(() => parseBroadcastAnnounce(new Uint8Array(0))).toThrow(/too short/);
    expect(() => parseBroadcastAnnounce(new Uint8Array([0x01, 0x03]))).toThrow(/too short/);
    expect(() => parseBroadcastAnnounce(new Uint8Array([0x02, 0x03, 0x01, 0x4b]))).toThrow(/version/);
    expect(() => parseBroadcastAnnounce(new Uint8Array([0x01, 0x02, 0x01, 0x4b]))).toThrow(/type/);
    expect(() => parseBroadcastAnnounce(new Uint8Array([0x01, 0x03, 0x02, 0x4b]))).toThrow(/length/);
    expect(() => parseBroadcastAnnounce(new Uint8Array([0x01, 0x03, 0x01, 0x6b]))).toThrow(/invalid character/); // lowercase 'k'
    expect(() => parseBroadcastAnnounce(new Uint8Array([0x01, 0x03, 0x01, 0x30]))).toThrow(/invalid character/); // '0'
    expect(() => parseBroadcastAnnounce(new Uint8Array([0x01, 0x03, 0x01, 0x4f]))).toThrow(/invalid character/); // 'O'
  });
});

// ResumeToken (R17 W2, docs/22). Golden vector copied verbatim from
// wire_test.go goldenResumeTokenHex.
const GOLDEN_RESUME_TOKEN_HEX = '010910000102030405060708090a0b0c0d0e0f';

describe('resume token (R17 W2)', () => {
  const goldenToken = fromHex('000102030405060708090a0b0c0d0e0f');

  it('encodes the golden resume token byte-for-byte', () => {
    expect(toHex(encodeResumeToken(goldenToken))).toBe(GOLDEN_RESUME_TOKEN_HEX);
  });

  it('parses the golden resume token', () => {
    expect(Array.from(parseResumeToken(fromHex(GOLDEN_RESUME_TOKEN_HEX)))).toEqual(
      Array.from(goldenToken),
    );
  });

  it('round-trips arbitrary tokens', () => {
    for (const token of [new Uint8Array([0x42]), new Uint8Array(255).fill(0xab)]) {
      expect(Array.from(parseResumeToken(encodeResumeToken(token)))).toEqual(Array.from(token));
    }
  });

  it('rejects malformed resume tokens', () => {
    expect(() => parseResumeToken(new Uint8Array(0))).toThrow(/too short/);
    expect(() => parseResumeToken(new Uint8Array([0x01, 0x09]))).toThrow(/too short/);
    expect(() => parseResumeToken(new Uint8Array([0x02, 0x09, 0x01, 0x42]))).toThrow(/version/);
    expect(() => parseResumeToken(new Uint8Array([0x01, 0x03, 0x01, 0x42]))).toThrow(/type/);
    expect(() => parseResumeToken(new Uint8Array([0x01, 0x09, 0x00]))).toThrow(/resume token/);
    expect(() => parseResumeToken(new Uint8Array([0x01, 0x09, 0x05, 0x42]))).toThrow(/resume token/);
    expect(() => parseResumeToken(new Uint8Array([0x01, 0x09, 0x01, 0x42, 0x43]))).toThrow(/resume token/);
    expect(() => encodeResumeToken(new Uint8Array(0))).toThrow(WireError);
    expect(() => encodeResumeToken(new Uint8Array(256))).toThrow(WireError);
  });
});

// R21 (docs/26 Decision 7a): the join-time delivery ack. Golden vectors are
// byte-identical to gawk-server/wire's — this is the mirror that keeps them so.
describe('DeliveryAck (R21)', () => {
  const GOLDEN_DVR_HEX = '010c020bb8';
  const GOLDEN_DATAGRAMS_HEX = '010c000000';

  it('matches the Go golden vectors', () => {
    expect(toHex(encodeDeliveryAck('dvr', 3000))).toBe(GOLDEN_DVR_HEX);
    expect(toHex(encodeDeliveryAck('datagrams', 0))).toBe(GOLDEN_DATAGRAMS_HEX);
  });

  it('round-trips every mode', () => {
    for (const mode of ['datagrams', 'reliable', 'dvr'] as const) {
      for (const bufferMs of [0, 1, 1000, 3000, 65535]) {
        expect(parseDeliveryAck(encodeDeliveryAck(mode, bufferMs))).toEqual({ mode, bufferMs });
      }
    }
  });

  it('rejects malformed acks strictly', () => {
    const good = encodeDeliveryAck('dvr', 3000);
    expect(() => parseDeliveryAck(good.subarray(0, 4))).toThrow();
    expect(() => parseDeliveryAck(fromHex('010c020bb800'))).toThrow();
    expect(() => parseDeliveryAck(fromHex('020c020bb8'))).toThrow(); // version
    expect(() => parseDeliveryAck(fromHex('010b020bb8'))).toThrow(); // type
    // An unknown mode must throw, not silently read as datagrams.
    expect(() => parseDeliveryAck(fromHex('010c090bb8'))).toThrow();
  });
});

// R28 (docs/33 §4.1): the telemetry hello. Golden vectors are byte-identical
// to gawk-server/wire's and gawk-broadcast's wirecheck — this is one of the
// three mirrors that keeps them so.
describe('TelemetryHello (R28)', () => {
  const GOLDEN_HEX =
    '010d0107d000012345000102030405060708090a0ba1a2a3a4a5a6a7a81a2b3c4d5e6f';
  const GOLDEN_DISABLED_HEX =
    '010d000000' + '000000000000000000000000000000000000000000000000000000000000';
  const GOLDEN_TOKEN = '00012345000102030405060708090a0ba1a2a3a4a5a6a7a8';
  const GOLDEN_KEY = '1a2b3c4d5e6f';

  it('matches the Go golden vectors', () => {
    expect(
      toHex(
        encodeTelemetryHello({
          enabled: true,
          reportIntervalMs: 2000,
          token: GOLDEN_TOKEN,
          broadcastKey: GOLDEN_KEY,
        }),
      ),
    ).toBe(GOLDEN_HEX);
    expect(
      toHex(
        encodeTelemetryHello({
          enabled: false,
          reportIntervalMs: 0,
          token: '0'.repeat(48),
          broadcastKey: '0'.repeat(12),
        }),
      ),
    ).toBe(GOLDEN_DISABLED_HEX);
  });

  it('parses the Go golden vectors', () => {
    expect(parseTelemetryHello(fromHex(GOLDEN_HEX))).toEqual({
      enabled: true,
      reportIntervalMs: 2000,
      token: GOLDEN_TOKEN,
      broadcastKey: GOLDEN_KEY,
    });
    const off = parseTelemetryHello(fromHex(GOLDEN_DISABLED_HEX));
    expect(off.enabled).toBe(false);
  });

  it('round-trips', () => {
    for (const enabled of [true, false]) {
      for (const reportIntervalMs of [0, 1, 500, 2000, 65535]) {
        const msg = { enabled, reportIntervalMs, token: GOLDEN_TOKEN, broadcastKey: GOLDEN_KEY };
        expect(parseTelemetryHello(encodeTelemetryHello(msg))).toEqual(msg);
      }
    }
  });

  it('rejects malformed hellos strictly', () => {
    const good = encodeTelemetryHello({
      enabled: true,
      reportIntervalMs: 2000,
      token: GOLDEN_TOKEN,
      broadcastKey: GOLDEN_KEY,
    });
    expect(() => parseTelemetryHello(good.subarray(0, TELEMETRY_HELLO_SIZE - 1))).toThrow(WireError);
    expect(() => parseTelemetryHello(fromHex(GOLDEN_HEX + '00'))).toThrow(WireError);
    expect(() => parseTelemetryHello(fromHex('02' + GOLDEN_HEX.slice(2)))).toThrow(/version/);
    expect(() => parseTelemetryHello(fromHex('010c' + GOLDEN_HEX.slice(4)))).toThrow(/type/);
    // A reserved flag bit means a future field this build would misread.
    expect(() => parseTelemetryHello(fromHex('010d02' + GOLDEN_HEX.slice(6)))).toThrow(/reserved/);
  });

  it('rejects fixed-width fields of the wrong size on encode', () => {
    const base = { enabled: true, reportIntervalMs: 2000, token: GOLDEN_TOKEN, broadcastKey: GOLDEN_KEY };
    expect(() => encodeTelemetryHello({ ...base, token: GOLDEN_TOKEN.slice(2) })).toThrow(WireError);
    expect(() => encodeTelemetryHello({ ...base, broadcastKey: 'zzzzzzzzzzzz' })).toThrow(WireError);
  });

  // The sessionId projection (docs/33 §4.2). The nonce below is the same one
  // Go's TestGoldenTelemetrySessionToken mints against, so both languages pin
  // the same 24 characters — this is the value the relay records, the ingest
  // re-derives, the dashboard prints and (since docs/33 §4.13) the viewer
  // overlay shows.
  it('derives the sessionId a token names, from the nonce alone', () => {
    expect(telemetrySessionId(GOLDEN_TOKEN)).toBe('000102030405060708090a0b');
    expect(telemetrySessionId(GOLDEN_TOKEN)).toHaveLength(TELEMETRY_SESSION_ID_LEN);
    // Neither the expiry hour nor the tag leaks into it: change them and the
    // id is unmoved — the same session under a re-mint would be a different
    // session, and the tag is the half that must never be displayed.
    expect(telemetrySessionId('ffffffff' + GOLDEN_TOKEN.slice(8, 32) + '0'.repeat(16))).toBe(
      telemetrySessionId(GOLDEN_TOKEN),
    );
    expect(() => telemetrySessionId(GOLDEN_TOKEN.slice(2))).toThrow(WireError);
    expect(() => telemetrySessionId('z'.repeat(48))).toThrow(WireError);
  });
});

// R30 (docs/35 §5.3): the stripe suppression signal. Golden vectors are
// byte-identical to gawk-server/wire's and gawk-broadcast's wirecheck.
describe('StripeState (R30)', () => {
  const GOLDEN_STRIPED_HEX = '0110010300';
  const GOLDEN_UNSTRIPED_HEX = '0110000000';

  it('exposes the mirrored constants', () => {
    expect(TYPE_STRIPE_STATE).toBe(0x10);
    expect(STRIPE_STATE_SIZE).toBe(5);
    expect(MAX_STRIPE_LEGS).toBe(4);
  });

  it('matches the Go golden vectors', () => {
    expect(toHex(encodeStripeState({ striped: true, stripeN: 3 }))).toBe(GOLDEN_STRIPED_HEX);
    expect(toHex(encodeStripeState({ striped: false, stripeN: 0 }))).toBe(GOLDEN_UNSTRIPED_HEX);
  });

  it('round-trips every legal shape', () => {
    for (let n = 1; n <= MAX_STRIPE_LEGS; n++) {
      expect(parseStripeState(encodeStripeState({ striped: true, stripeN: n }))).toEqual({
        striped: true,
        stripeN: n,
      });
    }
    expect(parseStripeState(encodeStripeState({ striped: false, stripeN: 0 }))).toEqual({
      striped: false,
      stripeN: 0,
    });
  });

  it('rejects malformed messages strictly', () => {
    const good = encodeStripeState({ striped: true, stripeN: 2 });
    expect(() => parseStripeState(good.subarray(0, STRIPE_STATE_SIZE - 1))).toThrow(WireError);
    expect(() => parseStripeState(fromHex('011001030000'))).toThrow(WireError); // long
    expect(() => parseStripeState(fromHex('0210010300'))).toThrow(WireError); // version
    expect(() => parseStripeState(fromHex('0101010300'))).toThrow(WireError); // type
    expect(() => parseStripeState(fromHex('0110030300'))).toThrow(WireError); // unknown flag bit
    expect(() => parseStripeState(fromHex('0110010000'))).toThrow(WireError); // striped, zero n
    expect(() => parseStripeState(fromHex('0110010500'))).toThrow(WireError); // striped, n > max
    expect(() => parseStripeState(fromHex('0110000100'))).toThrow(WireError); // unstriped, nonzero n
  });

  it('refuses bad shapes at encode too', () => {
    expect(() => encodeStripeState({ striped: true, stripeN: 0 })).toThrow(WireError);
    expect(() => encodeStripeState({ striped: true, stripeN: MAX_STRIPE_LEGS + 1 })).toThrow(WireError);
    expect(() => encodeStripeState({ striped: false, stripeN: 1 })).toThrow(WireError);
  });

  it('assigns stripe ordinals with parity at the tail', () => {
    // Data chunks keep their index; parity follows the data, preserving the
    // measured tail-of-burst position per leg (docs/34 finding 4).
    expect(stripeOrdinal(7, 20, null)).toBe(7);
    expect(stripeOrdinal(0, 20, 0)).toBe(20);
    expect(stripeOrdinal(0, 20, 1)).toBe(21);
    // A 20-chunk k=2 frame at N=3: parity lands on legs 2 and 0, and on each
    // leg its ordinal is the highest that leg carries.
    const legOf = (d: number) => d % 3;
    expect(legOf(stripeOrdinal(0, 20, 0))).toBe(2);
    expect(legOf(stripeOrdinal(0, 20, 1))).toBe(0);
  });
});

// --- R37 RelayIdentity (0x11) + TelemetryEndpoint (0x12) (docs/40 SP4) ---
// Golden vectors restated byte-identically from gawk-server/wire/wire_test.go.

const GOLDEN_RELAY_IDENTITY_HEX = '01110006312e34322e30096761776b20686f6d65';
const GOLDEN_RELAY_IDENTITY_NO_NAME_HEX = '01110006312e34322e3000';
const GOLDEN_TELEMETRY_ENDPOINT_HEX =
  '011200003068747470733a2f2f6761776b2e6578616d706c652e636f6d2f6170692f74656c656d657472792f76312f696e67657374';
const GOLDEN_TELEMETRY_ENDPOINT_URL = 'https://gawk.example.com/api/telemetry/v1/ingest';

describe('relay identity (0x11)', () => {
  it('encodes the golden vectors byte-identically', () => {
    expect(toHex(encodeRelayIdentity({ serverVersion: '1.42.0', name: 'gawk home' }))).toBe(
      GOLDEN_RELAY_IDENTITY_HEX,
    );
    expect(toHex(encodeRelayIdentity({ serverVersion: '1.42.0', name: '' }))).toBe(
      GOLDEN_RELAY_IDENTITY_NO_NAME_HEX,
    );
  });

  it('parses the golden vectors', () => {
    expect(parseRelayIdentity(fromHex(GOLDEN_RELAY_IDENTITY_HEX))).toEqual({
      serverVersion: '1.42.0',
      name: 'gawk home',
    });
    expect(parseRelayIdentity(fromHex(GOLDEN_RELAY_IDENTITY_NO_NAME_HEX))).toEqual({
      serverVersion: '1.42.0',
      name: '',
    });
  });

  // The extension-point contract (docs/40 §4.9): trailing bytes parse as if
  // absent — appended future fields must not break this build.
  it('tolerates trailing extension bytes', () => {
    expect(parseRelayIdentity(fromHex(GOLDEN_RELAY_IDENTITY_HEX + 'a1b2c3'))).toEqual({
      serverVersion: '1.42.0',
      name: 'gawk home',
    });
  });

  it('rejects nonzero flags and length overruns', () => {
    const flagged = fromHex(GOLDEN_RELAY_IDENTITY_HEX);
    flagged[2] = 0x01;
    expect(() => parseRelayIdentity(flagged)).toThrow(/reserved flag/);

    const overrun = fromHex(GOLDEN_RELAY_IDENTITY_HEX);
    overrun[3] = 0xff;
    expect(() => parseRelayIdentity(overrun)).toThrow(/out of range|overruns/);

    const nameOverrun = fromHex(GOLDEN_RELAY_IDENTITY_HEX);
    nameOverrun[10] = 0x40;
    expect(() => parseRelayIdentity(nameOverrun)).toThrow(/overruns/);

    const badUtf8 = fromHex(GOLDEN_RELAY_IDENTITY_HEX);
    badUtf8[11] = 0xff;
    expect(() => parseRelayIdentity(badUtf8)).toThrow(/UTF-8/);
  });
});

describe('telemetry endpoint (0x12)', () => {
  it('round-trips the golden vector byte-identically', () => {
    expect(toHex(encodeTelemetryEndpoint(GOLDEN_TELEMETRY_ENDPOINT_URL))).toBe(
      GOLDEN_TELEMETRY_ENDPOINT_HEX,
    );
    expect(parseTelemetryEndpoint(fromHex(GOLDEN_TELEMETRY_ENDPOINT_HEX))).toBe(
      GOLDEN_TELEMETRY_ENDPOINT_URL,
    );
  });

  it('tolerates trailing extension bytes', () => {
    expect(parseTelemetryEndpoint(fromHex(GOLDEN_TELEMETRY_ENDPOINT_HEX + '7f'))).toBe(
      GOLDEN_TELEMETRY_ENDPOINT_URL,
    );
  });

  it('rejects nonzero flags, overruns, and non-https URLs', () => {
    const flagged = fromHex(GOLDEN_TELEMETRY_ENDPOINT_HEX);
    flagged[2] = 0x80;
    expect(() => parseTelemetryEndpoint(flagged)).toThrow(/reserved flag/);

    const overrun = fromHex(GOLDEN_TELEMETRY_ENDPOINT_HEX);
    overrun[3] = 0x01;
    overrun[4] = 0x00;
    expect(() => parseTelemetryEndpoint(overrun)).toThrow(/overruns/);

    expect(() => encodeTelemetryEndpoint('http://insecure.example/x')).toThrow(/https/);
    expect(() => encodeTelemetryEndpoint('not a url')).toThrow(/non-URL byte|parse/);
  });
});

// --- R42 room control protocol (0x13–0x16) (docs/44 §4.6) -------------------
// Golden vectors restated byte-identically from gawk-server/wire/room_test.go
// (the same concatenation pieces, so a diff against Go lines up). Do not
// regenerate them from code; if they change, the wire format changed.

// RoomHello: protocol 1, clientKind 1 (web-broadcaster), wantCaps 0,
// nickname "tuhis".
const GOLDEN_ROOM_HELLO_HEX = '0113010100057475686973';
// One room record framing the golden RoomHello (11 bytes).
const GOLDEN_ROOM_RECORD_HEX = '000b' + GOLDEN_ROOM_HELLO_HEX;
// RoomState, dynamic room right after /room/new: flags dynamic|creator
// (0x03), caps none, seq 7, yourID 1, code "5UP4XW", no display name,
// creator token 00..0f, one attachment (ABCDEF, label "tuhis", live,
// 3 viewers), one participant (id 1, web-broadcaster, streaming, "tuhis",
// no identity).
const GOLDEN_ROOM_STATE_DYNAMIC_HEX =
  '0114' + '03' + '00' + '00000007' + '0001' +
  '06355550345857' + '00' +
  '10000102030405060708090a0b0c0d0e0f' +
  '061a2b3c4d5e6f' +
  '01' + '06414243444546' + '057475686973' + '01' + '00000003' +
  '0001' + '0001' + '01' + '02' + '057475686973' + '00';
// RoomState, static room, empty: flags attachOK (0x04), caps none, seq 0,
// yourID 2, code "TuhisRoom", display name "Tuhis' room", no token, no
// attachments, one participant (id 2, web-viewer, no flags, "viewer").
const GOLDEN_ROOM_STATE_STATIC_HEX =
  '0114' + '04' + '00' + '00000000' + '0002' +
  '095475686973526f6f6d' + '0b54756869732720726f6f6d' + '00' + '00' + '00' +
  '0001' + '0002' + '00' + '00' + '06766965776572' + '00';
// RoomEvent ParticipantJoined, seq 8: id 3, native, streaming, "pc".
const GOLDEN_ROOM_EVENT_JOINED_HEX = '011500000008' + '01' + '0003' + '02' + '02' + '027063' + '00';
// RoomEvent ParticipantLeft, seq 9, id 3.
const GOLDEN_ROOM_EVENT_LEFT_HEX = '011500000009' + '02' + '0003';
// RoomEvent AttachmentUpdated, seq 13: ABCDEF, "tuhis", AWAY, 12 viewers.
const GOLDEN_ROOM_EVENT_ATTACHMENT_UPDATED_HEX =
  '01150000000d' + '12' + '06414243444546' + '057475686973' + '00' + '0000000c';
// RoomEvent AttachmentRemoved, seq 10: ABCDEF, reason expired (2).
const GOLDEN_ROOM_EVENT_ATTACHMENT_REMOVED_HEX = '01150000000a' + '11' + '06414243444546' + '02';
// RoomEvent RoomEnding, seq 11, reason creator (2).
const GOLDEN_ROOM_EVENT_ENDING_HEX = '01150000000b' + '20' + '02';
// RoomEvent CommandRejected, seq 12: command attach (1), reason limit (1),
// message "room full".
const GOLDEN_ROOM_EVENT_REJECTED_HEX = '01150000000c' + '30' + '01' + '01' + '09726f6f6d2066756c6c';
// RoomCommand Attach: ABCDEF, resume token a0..af, label "tuhis".
const GOLDEN_ROOM_COMMAND_ATTACH_HEX =
  '0116' + '01' + '06414243444546' + '10a0a1a2a3a4a5a6a7a8a9aaabacadaeaf' + '057475686973';
// RoomCommand Detach ABCDEF.
const GOLDEN_ROOM_COMMAND_DETACH_HEX = '0116' + '02' + '06414243444546';
// RoomCommand SetNickname "tuhis".
const GOLDEN_ROOM_COMMAND_NICK_HEX = '0116' + '03' + '057475686973';
// RoomCommand EndRoom / Resync: no payload.
const GOLDEN_ROOM_COMMAND_END_HEX = '011604';
const GOLDEN_ROOM_COMMAND_RESYNC_HEX = '011605';

const goldenRoomHello: RoomHello = {
  protocol: 1,
  clientKind: ROOM_CLIENT_WEB_BROADCASTER,
  wantCaps: 0,
  nickname: 'tuhis',
};
const goldenCreatorToken = fromHex('000102030405060708090a0b0c0d0e0f');
const goldenResumeToken = fromHex('a0a1a2a3a4a5a6a7a8a9aaabacadaeaf');

const goldenRoomStateDynamic: RoomState = {
  flags: ROOM_STATE_FLAG_DYNAMIC | ROOM_STATE_FLAG_CREATOR,
  caps: 0,
  seq: 7,
  yourId: 1,
  code: '5UP4XW',
  displayName: '',
  creatorToken: goldenCreatorToken,
  key: new Uint8Array([0x1a, 0x2b, 0x3c, 0x4d, 0x5e, 0x6f]),
  attachments: [{ broadcastId: 'ABCDEF', label: 'tuhis', live: true, viewerCount: 3 }],
  participants: [
    { id: 1, kind: ROOM_CLIENT_WEB_BROADCASTER, flags: ROOM_PARTICIPANT_FLAG_STREAMING, nickname: 'tuhis', identity: '' },
  ],
};
const goldenRoomStateStatic: RoomState = {
  flags: ROOM_STATE_FLAG_ATTACH_OK,
  caps: 0,
  seq: 0,
  yourId: 2,
  code: 'TuhisRoom',
  displayName: "Tuhis' room",
  creatorToken: new Uint8Array(0),
  key: new Uint8Array(0),
  attachments: [],
  participants: [{ id: 2, kind: ROOM_CLIENT_WEB_VIEWER, flags: 0, nickname: 'viewer', identity: '' }],
};

function catchError(fn: () => unknown): unknown {
  try {
    fn();
  } catch (err) {
    return err;
  }
  throw new Error('expected a throw');
}

describe('room control protocol (R42)', () => {
  it('exposes the mirrored constants', () => {
    expect(ROOM_PROTOCOL_VERSION).toBe(1);
    expect(ROOM_RECORD_HEADER_SIZE).toBe(2);
    expect(MAX_ROOM_RECORD_SIZE).toBe(16384);
    expect(MAX_ROOM_NICKNAME_LEN).toBe(32);
    expect(MAX_ROOM_CODE_LEN).toBe(32);
    expect(MAX_ROOM_DISPLAY_NAME_LEN).toBe(64);
    expect(MAX_ROOM_LABEL_LEN).toBe(32);
    expect(MAX_ROOM_IDENTITY_LEN).toBe(64);
    expect(MAX_ROOM_REJECT_MESSAGE_LEN).toBe(128);
    expect(ROOM_CREATOR_TOKEN_SIZE).toBe(16);
    expect(ROOM_KEY_SIZE).toBe(6);
    expect(RESUME_TOKEN_SIZE).toBe(16);
  });

  it('encodes and parses the golden RoomHello byte-identically', () => {
    expect(toHex(encodeRoomHello(goldenRoomHello))).toBe(GOLDEN_ROOM_HELLO_HEX);
    expect(parseRoomHello(fromHex(GOLDEN_ROOM_HELLO_HEX))).toEqual(goldenRoomHello);
  });

  it('frames and unframes the golden room record', () => {
    const want = fromHex(GOLDEN_ROOM_RECORD_HEX);
    expect(toHex(encodeRoomRecord(fromHex(GOLDEN_ROOM_HELLO_HEX)))).toBe(GOLDEN_ROOM_RECORD_HEX);
    expect(parseRoomRecordLength(want)).toBe(11);
    expect(() => parseRoomRecordLength(new Uint8Array([0x00]))).toThrow(WireError);
    expect(() => parseRoomRecordLength(new Uint8Array([0x00, 0x00]))).toThrow(WireError);
    expect(() => parseRoomRecordLength(new Uint8Array([0x00, 0x01]))).toThrow(WireError);
    expect(() => parseRoomRecordLength(new Uint8Array([0xff, 0xff]))).toThrow(WireError);
    expect(() => encodeRoomRecord(new Uint8Array(1))).toThrow(WireError);
    expect(() => encodeRoomRecord(new Uint8Array(MAX_ROOM_RECORD_SIZE + 1))).toThrow(WireError);
    // The inclusive upper boundary frames.
    expect(encodeRoomRecord(new Uint8Array(MAX_ROOM_RECORD_SIZE)).length).toBe(
      ROOM_RECORD_HEADER_SIZE + MAX_ROOM_RECORD_SIZE,
    );
  });

  it.each([
    ['dynamic', GOLDEN_ROOM_STATE_DYNAMIC_HEX, goldenRoomStateDynamic],
    ['static', GOLDEN_ROOM_STATE_STATIC_HEX, goldenRoomStateStatic],
  ] satisfies [string, string, RoomState][])('encodes and parses the golden %s RoomState', (_name, hex, want) => {
    expect(toHex(encodeRoomState(want))).toBe(hex);
    const got = parseRoomState(fromHex(hex));
    expect(got).toEqual(want);
    expect(toHex(got.creatorToken)).toBe(toHex(want.creatorToken));
  });

  it.each([
    [
      'joined',
      GOLDEN_ROOM_EVENT_JOINED_HEX,
      {
        seq: 8,
        kind: ROOM_EVENT_PARTICIPANT_JOINED,
        participant: { id: 3, kind: ROOM_CLIENT_NATIVE, flags: ROOM_PARTICIPANT_FLAG_STREAMING, nickname: 'pc', identity: '' },
      },
    ],
    ['left', GOLDEN_ROOM_EVENT_LEFT_HEX, { seq: 9, kind: ROOM_EVENT_PARTICIPANT_LEFT, participant: { id: 3 } }],
    [
      'attachment updated',
      GOLDEN_ROOM_EVENT_ATTACHMENT_UPDATED_HEX,
      {
        seq: 13,
        kind: ROOM_EVENT_ATTACHMENT_UPDATED,
        attachment: { broadcastId: 'ABCDEF', label: 'tuhis', live: false, viewerCount: 12 },
      },
    ],
    [
      'attachment removed',
      GOLDEN_ROOM_EVENT_ATTACHMENT_REMOVED_HEX,
      {
        seq: 10,
        kind: ROOM_EVENT_ATTACHMENT_REMOVED,
        attachment: { broadcastId: 'ABCDEF' },
        reason: ROOM_DETACH_REASON_EXPIRED,
      },
    ],
    ['ending', GOLDEN_ROOM_EVENT_ENDING_HEX, { seq: 11, kind: ROOM_EVENT_ROOM_ENDING, reason: ROOM_END_REASON_CREATOR }],
    [
      'rejected',
      GOLDEN_ROOM_EVENT_REJECTED_HEX,
      {
        seq: 12,
        kind: ROOM_EVENT_COMMAND_REJECTED,
        command: ROOM_COMMAND_ATTACH,
        reason: ROOM_REJECT_LIMIT,
        message: 'room full',
      },
    ],
  ] satisfies [string, string, RoomEvent][])('encodes and parses the golden %s RoomEvent', (_name, hex, want) => {
    expect(toHex(encodeRoomEvent(want))).toBe(hex);
    expect(parseRoomEvent(fromHex(hex))).toEqual(want);
  });

  it.each([
    [
      'attach',
      GOLDEN_ROOM_COMMAND_ATTACH_HEX,
      { kind: ROOM_COMMAND_ATTACH, broadcastId: 'ABCDEF', resumeToken: goldenResumeToken, label: 'tuhis' },
    ],
    ['detach', GOLDEN_ROOM_COMMAND_DETACH_HEX, { kind: ROOM_COMMAND_DETACH, broadcastId: 'ABCDEF' }],
    ['nick', GOLDEN_ROOM_COMMAND_NICK_HEX, { kind: ROOM_COMMAND_SET_NICKNAME, nickname: 'tuhis' }],
    ['end', GOLDEN_ROOM_COMMAND_END_HEX, { kind: ROOM_COMMAND_END_ROOM }],
    ['resync', GOLDEN_ROOM_COMMAND_RESYNC_HEX, { kind: ROOM_COMMAND_RESYNC }],
  ] satisfies [string, string, RoomCommand][])('encodes and parses the golden %s RoomCommand', (_name, hex, want) => {
    expect(toHex(encodeRoomCommand(want))).toBe(hex);
    expect(parseRoomCommand(fromHex(hex))).toEqual(want);
  });

  // The docs/44 §4.11 reserved ranges: an unknown kind is reported as such
  // with the header fields filled in, so a reader can skip the record and a
  // relay can answer ROOM_REJECT_UNSUPPORTED.
  it('reports reserved kinds with the header fields filled in', () => {
    const ev = catchError(() => parseRoomEvent(fromHex('0115000000014041')));
    expect(ev).toBeInstanceOf(RoomUnknownKindError);
    expect(ev).toBeInstanceOf(WireError);
    expect((ev as RoomUnknownKindError).seq).toBe(1);
    expect((ev as RoomUnknownKindError).kind).toBe(0x40);

    const cmd = catchError(() => parseRoomCommand(fromHex('01165000')));
    expect(cmd).toBeInstanceOf(RoomUnknownKindError);
    expect((cmd as RoomUnknownKindError).kind).toBe(0x50);
    expect((cmd as RoomUnknownKindError).seq).toBeUndefined();

    expect(() => encodeRoomEvent({ seq: 0, kind: 0x4f } as unknown as RoomEvent)).toThrow(RoomUnknownKindError);
    expect(() => encodeRoomCommand({ kind: 0x5f } as unknown as RoomCommand)).toThrow(RoomUnknownKindError);
  });

  it('rejects malformed hellos strictly', () => {
    const hello = fromHex(GOLDEN_ROOM_HELLO_HEX);
    // Trailing bytes are a framing error on strict messages.
    expect(() => parseRoomHello(fromHex(GOLDEN_ROOM_HELLO_HEX + '00'))).toThrow(/trailing/);
    const mutate = (i: number, v: number): Uint8Array => {
      const bad = hello.slice();
      bad[i] = v;
      return bad;
    };
    expect(() => parseRoomHello(mutate(2, 2))).toThrow(/protocol/);
    expect(() => parseRoomHello(mutate(3, 3))).toThrow(/client kind/);
    expect(() => parseRoomHello(mutate(4, 0x04))).toThrow(/reserved capability/);
    expect(() => parseRoomHello(mutate(5, 0x20))).toThrow(/overruns/);
    expect(() => parseRoomHello(mutate(6, 0xff))).toThrow(/UTF-8/);
    expect(() => parseRoomHello(hello.subarray(0, 5))).toThrow(/too short/);
    expect(() => parseRoomHello(fromHex('02' + GOLDEN_ROOM_HELLO_HEX.slice(2)))).toThrow(/version/);
    expect(() => parseRoomHello(fromHex(GOLDEN_ROOM_STATE_STATIC_HEX))).toThrow(/type/);
    expect(() => encodeRoomHello({ ...goldenRoomHello, nickname: 'x'.repeat(MAX_ROOM_NICKNAME_LEN + 1) })).toThrow(
      WireError,
    );
    expect(() => encodeRoomHello({ ...goldenRoomHello, nickname: '\ud800' })).toThrow(/UTF-8/);
    expect(() => encodeRoomHello({ ...goldenRoomHello, protocol: 2 })).toThrow(WireError);
    expect(() => encodeRoomHello({ ...goldenRoomHello, clientKind: 3 })).toThrow(WireError);
    expect(() => encodeRoomHello({ ...goldenRoomHello, wantCaps: 0x04 })).toThrow(WireError);
    // A multi-byte nickname is measured in UTF-8 bytes, like Go's len().
    expect(parseRoomHello(encodeRoomHello({ ...goldenRoomHello, nickname: 'ääää' })).nickname).toBe('ääää');
  });

  it('rejects malformed states strictly', () => {
    const state = fromHex(GOLDEN_ROOM_STATE_DYNAMIC_HEX);
    const mutate = (i: number, v: number): Uint8Array => {
      const bad = state.slice();
      bad[i] = v;
      return bad;
    };
    expect(() => parseRoomState(mutate(2, 0x08))).toThrow(/reserved flag/);
    expect(() => parseRoomState(mutate(3, 0x04))).toThrow(/reserved capability/);
    expect(() => parseRoomState(state.subarray(0, state.length - 1))).toThrow(/truncated/);
    expect(() => parseRoomState(fromHex(GOLDEN_ROOM_STATE_DYNAMIC_HEX + '00'))).toThrow(/trailing/);
    expect(() => parseRoomState(mutate(17, 0x05))).toThrow(/creator token/); // token length neither 0 nor 16
    expect(() => parseRoomState(mutate(10, 0x00))).toThrow(WireError); // empty code (then misaligned)
    expect(() => parseRoomState(state.subarray(0, 14))).toThrow(/too short/);
    expect(() => encodeRoomState({ ...goldenRoomStateDynamic, creatorToken: new Uint8Array([1]) })).toThrow(
      /creator token/,
    );
    expect(() => encodeRoomState({ ...goldenRoomStateDynamic, code: '' })).toThrow(/empty code/);
    expect(() => encodeRoomState({ ...goldenRoomStateDynamic, flags: 0x08 })).toThrow(/reserved flag/);
    expect(() => encodeRoomState({ ...goldenRoomStateDynamic, caps: 0x04 })).toThrow(/reserved capability/);
    expect(() =>
      encodeRoomState({
        ...goldenRoomStateDynamic,
        attachments: [{ broadcastId: '0OIL11', label: '', live: false, viewerCount: 0 }],
      }),
    ).toThrow(/broadcast id/);
    expect(() =>
      encodeRoomState({
        ...goldenRoomStateDynamic,
        participants: [{ id: 1, kind: 0, flags: 0x80, nickname: '', identity: '' }],
      }),
    ).toThrow(/reserved participant flag/);
    expect(() =>
      encodeRoomState({
        ...goldenRoomStateDynamic,
        participants: [{ id: 1, kind: 3, flags: 0, nickname: '', identity: '' }],
      }),
    ).toThrow(/participant kind/);
    // Reserved attachment flag bits and participant kinds reject on parse too
    // (attachment flags at byte 56, participant kind at byte 65).
    expect(() => parseRoomState(mutate(56, 0x03))).toThrow(/reserved attachment flag/);
    expect(() => parseRoomState(mutate(65, 0x03))).toThrow(/participant kind/);
  });

  it('rejects malformed events strictly', () => {
    const ev = fromHex(GOLDEN_ROOM_EVENT_JOINED_HEX);
    expect(() => parseRoomEvent(fromHex(GOLDEN_ROOM_EVENT_JOINED_HEX + '00'))).toThrow(/trailing/);
    expect(() => parseRoomEvent(ev.subarray(0, 6))).toThrow(/too short/);
    expect(() => parseRoomEvent(ev.subarray(0, ev.length - 1))).toThrow(/truncated|overruns/);
    expect(() => parseRoomEvent(fromHex(GOLDEN_ROOM_COMMAND_ATTACH_HEX))).toThrow(/type/);
    // A reserved kind with the wrong header length is still a short-message
    // error, never an unknown-kind one.
    expect(() => parseRoomEvent(fromHex('011500000001'))).toThrow(/too short/);
  });

  it('rejects malformed commands strictly and normalizes broadcast IDs', () => {
    const cmd = fromHex(GOLDEN_ROOM_COMMAND_ATTACH_HEX);
    const bad = cmd.slice();
    bad[10] = 0x0f; // token length 15
    expect(() => parseRoomCommand(bad)).toThrow(/resume token/);
    expect(() =>
      encodeRoomCommand({ kind: ROOM_COMMAND_ATTACH, broadcastId: 'ABCDEF', resumeToken: new Uint8Array([1]), label: '' }),
    ).toThrow(/resume token/);
    // Broadcast IDs normalize on parse: a lower-case ID on the wire is
    // accepted and returned upper-case, so the relay never compares raw.
    const lower = parseRoomCommand(fromHex('0116' + '02' + '06616263646566'));
    expect(lower).toEqual({ kind: ROOM_COMMAND_DETACH, broadcastId: 'ABCDEF' });
    expect(() => parseRoomCommand(fromHex('0116' + '02' + '06304f494c3131'))).toThrow(/broadcast id/);
    expect(() => parseRoomCommand(fromHex('011604ff'))).toThrow(/trailing/);
    expect(() => parseRoomCommand(fromHex('0116'))).toThrow(/too short/);
    // ...and on encode.
    expect(toHex(encodeRoomCommand({ kind: ROOM_COMMAND_DETACH, broadcastId: 'abcdef' }))).toBe(
      GOLDEN_ROOM_COMMAND_DETACH_HEX,
    );
    expect(() => encodeRoomCommand({ kind: ROOM_COMMAND_DETACH, broadcastId: '0OIL11' })).toThrow(/broadcast id/);
    expect(() => encodeRoomCommand({ kind: ROOM_COMMAND_DETACH, broadcastId: 'ABCDE' })).toThrow(/broadcast id/);
  });

  it('normalizes broadcast IDs like the relay', () => {
    expect(normalizeBroadcastId('23456a')).toBe('23456A');
    expect(normalizeBroadcastId('234567')).toBe('234567');
    for (const bad of ['2345', '2345678', '23456O', '234560', '23456I', '234561', '23456L', '23456l', 'ßßß']) {
      expect(() => normalizeBroadcastId(bad)).toThrow(WireError);
    }
  });

  it('returns creator and resume tokens as views of the input record', () => {
    const state = fromHex(GOLDEN_ROOM_STATE_DYNAMIC_HEX);
    const tok = parseRoomState(state).creatorToken;
    expect(tok.buffer).toBe(state.buffer);
    expect(toHex(tok)).toBe('000102030405060708090a0b0c0d0e0f');
    const cmd = fromHex(GOLDEN_ROOM_COMMAND_ATTACH_HEX);
    const parsed = parseRoomCommand(cmd);
    expect(parsed.kind).toBe(ROOM_COMMAND_ATTACH);
    if (parsed.kind === ROOM_COMMAND_ATTACH) expect(parsed.resumeToken.buffer).toBe(cmd.buffer);
  });
});

describe('room record reader (R42)', () => {
  it('reassembles records split across arbitrary chunk boundaries', () => {
    const a = encodeRoomRecord(fromHex(GOLDEN_ROOM_HELLO_HEX));
    const b = encodeRoomRecord(fromHex(GOLDEN_ROOM_STATE_DYNAMIC_HEX));
    const c = encodeRoomRecord(fromHex(GOLDEN_ROOM_COMMAND_END_HEX));
    const stream = new Uint8Array(a.length + b.length + c.length);
    stream.set(a, 0);
    stream.set(b, a.length);
    stream.set(c, a.length + b.length);

    // Every possible split point yields the same three records.
    for (let split = 0; split <= stream.length; split++) {
      const reader = new RoomRecordReader();
      const records = [...reader.push(stream.subarray(0, split)), ...reader.push(stream.subarray(split))];
      expect(records.map(toHex)).toEqual([
        GOLDEN_ROOM_HELLO_HEX,
        GOLDEN_ROOM_STATE_DYNAMIC_HEX,
        GOLDEN_ROOM_COMMAND_END_HEX,
      ]);
      expect(reader.hasPartial()).toBe(false);
    }
  });

  it('returns copies that own their buffers and reports a partial record', () => {
    const reader = new RoomRecordReader();
    const record = encodeRoomRecord(fromHex(GOLDEN_ROOM_HELLO_HEX));
    expect(reader.push(record.subarray(0, 5))).toEqual([]);
    expect(reader.hasPartial()).toBe(true);
    const [got] = reader.push(record.subarray(5));
    expect(got.byteOffset).toBe(0);
    expect(got.buffer.byteLength).toBe(11);
    expect(toHex(got)).toBe(GOLDEN_ROOM_HELLO_HEX);
    expect(reader.hasPartial()).toBe(false);
  });

  it('throws on a zero, one-byte, or oversize declared length', () => {
    expect(() => new RoomRecordReader().push(new Uint8Array([0x00, 0x00, 0x01]))).toThrow(WireError);
    expect(() => new RoomRecordReader().push(new Uint8Array([0x00, 0x01, 0x01]))).toThrow(WireError);
    const oversize = new Uint8Array(2);
    new DataView(oversize.buffer).setUint16(0, MAX_ROOM_RECORD_SIZE + 1);
    expect(() => new RoomRecordReader().push(oversize)).toThrow(WireError);
    // A one-byte push is not a decision yet: the length is unknown.
    const reader = new RoomRecordReader();
    expect(reader.push(new Uint8Array([0x00]))).toEqual([]);
    expect(reader.hasPartial()).toBe(true);
  });

  it('feeds parsed messages straight to the typed parsers', () => {
    const reader = new RoomRecordReader();
    const [hello, ev] = reader.push(
      new Uint8Array([
        ...encodeRoomRecord(encodeRoomHello(goldenRoomHello)),
        ...encodeRoomRecord(fromHex(GOLDEN_ROOM_EVENT_ENDING_HEX)),
      ]),
    );
    expect(parseRoomHello(hello)).toEqual(goldenRoomHello);
    expect(parseRoomEvent(ev)).toEqual({ seq: 11, kind: ROOM_EVENT_ROOM_ENDING, reason: ROOM_END_REASON_CREATOR });
  });
});
