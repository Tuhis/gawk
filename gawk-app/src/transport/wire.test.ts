import { describe, expect, it } from 'vitest';

import {
  MAX_CHUNK_PAYLOAD,
  MAX_DATAGRAM_SIZE,
  TYPE_DECODER_CONFIG,
  TYPE_VIDEO_CHUNK,
  VIDEO_CHUNK_HEADER_SIZE,
  WIRE_VERSION,
  WireError,
  encodeDecoderConfig,
  encodeVideoChunk,
  parseDecoderConfig,
  parseVideoChunk,
  peekType,
  type DecoderConfigMessage,
  type VideoChunkHeader,
} from './wire';

// Golden vectors copied verbatim from gawk-server/internal/wire/wire_test.go
// — the cross-language portability guarantee. Do not regenerate them from
// code; if they change, the wire format changed.
const GOLDEN_VIDEO_CHUNK_HEX = '0101010001020304000500820000005d21dba5f0616263';
const GOLDEN_DECODER_CONFIG_AVC_HEX = '0102000b617663312e3432453032410142e02affe1';
const GOLDEN_DECODER_CONFIG_VP8_HEX = '01020003767038';

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

describe('constants', () => {
  it('match the Go package', () => {
    expect(MAX_DATAGRAM_SIZE).toBe(1200);
    expect(VIDEO_CHUNK_HEADER_SIZE).toBe(20);
    expect(MAX_CHUNK_PAYLOAD).toBe(1180);
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
    ['max payload', { keyframe: true, frameId: 0xffffffff, chunkIndex: 0xfffe, chunkCount: 0xffff, timestampUs: 0xffffffffffffffffn }, new Uint8Array(MAX_CHUNK_PAYLOAD).fill(0xab)],
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
});
