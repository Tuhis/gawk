// TypeScript mirror of the relay's wire format (gawk-server/internal/wire).
// The Go package is the source of truth; the golden hex vectors in
// wire.test.ts are copied verbatim from wire_test.go and guarantee the two
// implementations stay byte-compatible. If a vector fails, the wire format
// changed — fix the drift, don't regenerate the vector.
//
// Every datagram starts with a 2-byte prefix: byte 0 protocol version, byte
// 1 message type. All multi-byte integers are big-endian (DataView default).
//
//   0x01 VideoChunk (20-byte header, then payload):
//     byte 2       uint8   flags        bit0 = keyframe
//     byte 3       uint8   reserved (0)
//     bytes 4-7    uint32  frameID
//     bytes 8-9    uint16  chunkIndex   0-based
//     bytes 10-11  uint16  chunkCount   total chunks in the frame (>= 1)
//     bytes 12-19  uint64  timestampUs
//
//   0x02 DecoderConfig:
//     byte 2       uint8   reserved (0)
//     byte 3       uint8   codecLen
//     bytes 4..    codecLen bytes of ASCII codec string, then extradata
//
// Parse functions never copy: returned payload/extradata are subarray views
// of the input. Callers that retain them past the datagram buffer's
// lifetime must copy.

export const WIRE_VERSION = 0x01;

export const TYPE_VIDEO_CHUNK = 0x01;
export const TYPE_DECODER_CONFIG = 0x02;
export const TYPE_BROADCAST_ANNOUNCE = 0x03;

export const CLOSE_CODE_BROADCAST_ENDED = 4000;

export const MAX_DATAGRAM_SIZE = 1200;
export const VIDEO_CHUNK_HEADER_SIZE = 20;
export const MAX_CHUNK_PAYLOAD = MAX_DATAGRAM_SIZE - VIDEO_CHUNK_HEADER_SIZE;

const FLAG_KEYFRAME = 0x01;

export class WireError extends Error {
  constructor(message: string) {
    super(message);
    this.name = 'WireError';
  }
}

export interface VideoChunkHeader {
  keyframe: boolean;
  frameId: number; // uint32, monotonic per publisher session
  chunkIndex: number; // uint16, 0-based
  chunkCount: number; // uint16, >= 1 and > chunkIndex
  timestampUs: bigint; // uint64, EncodedVideoChunk.timestamp
}

export interface DecoderConfigMessage {
  codec: string; // WebCodecs codec string, 1-255 ASCII bytes on the wire
  extradata: Uint8Array; // AVCC bytes for H.264; empty for VP8/VP9
}

export function peekType(dgram: Uint8Array): { version: number; msgType: number } {
  if (dgram.length < 2) {
    throw new WireError(`datagram too short: ${dgram.length} bytes, need at least 2`);
  }
  return { version: dgram[0], msgType: dgram[1] };
}

export function encodeVideoChunk(header: VideoChunkHeader, payload: Uint8Array): Uint8Array<ArrayBuffer> {
  if (payload.length > MAX_CHUNK_PAYLOAD) {
    throw new WireError(`payload ${payload.length} bytes exceeds MAX_CHUNK_PAYLOAD ${MAX_CHUNK_PAYLOAD}`);
  }
  if (header.chunkCount === 0 || header.chunkIndex >= header.chunkCount) {
    throw new WireError(`invalid chunk index ${header.chunkIndex} / count ${header.chunkCount}`);
  }
  const dgram = new Uint8Array(VIDEO_CHUNK_HEADER_SIZE + payload.length);
  const view = new DataView(dgram.buffer);
  dgram[0] = WIRE_VERSION;
  dgram[1] = TYPE_VIDEO_CHUNK;
  dgram[2] = header.keyframe ? FLAG_KEYFRAME : 0;
  dgram[3] = 0;
  view.setUint32(4, header.frameId);
  view.setUint16(8, header.chunkIndex);
  view.setUint16(10, header.chunkCount);
  view.setBigUint64(12, header.timestampUs);
  dgram.set(payload, VIDEO_CHUNK_HEADER_SIZE);
  return dgram;
}

export function parseVideoChunk(dgram: Uint8Array): { header: VideoChunkHeader; payload: Uint8Array } {
  if (dgram.length < VIDEO_CHUNK_HEADER_SIZE) {
    throw new WireError(
      `datagram too short: ${dgram.length} bytes, need at least ${VIDEO_CHUNK_HEADER_SIZE} for video chunk`,
    );
  }
  if (dgram[0] !== WIRE_VERSION) {
    throw new WireError(`unsupported version 0x${dgram[0].toString(16)}`);
  }
  if (dgram[1] !== TYPE_VIDEO_CHUNK) {
    throw new WireError(`unexpected message type 0x${dgram[1].toString(16)}, want video chunk`);
  }
  const view = new DataView(dgram.buffer, dgram.byteOffset, dgram.byteLength);
  const header: VideoChunkHeader = {
    keyframe: (dgram[2] & FLAG_KEYFRAME) !== 0,
    frameId: view.getUint32(4),
    chunkIndex: view.getUint16(8),
    chunkCount: view.getUint16(10),
    timestampUs: view.getBigUint64(12),
  };
  if (header.chunkCount === 0 || header.chunkIndex >= header.chunkCount) {
    throw new WireError(`invalid chunk index ${header.chunkIndex} / count ${header.chunkCount}`);
  }
  return { header, payload: dgram.subarray(VIDEO_CHUNK_HEADER_SIZE) };
}

export function encodeDecoderConfig(config: DecoderConfigMessage): Uint8Array<ArrayBuffer> {
  const codecBytes = new TextEncoder().encode(config.codec);
  if (codecBytes.length === 0 || codecBytes.length > 255) {
    throw new WireError(`invalid codec string: ${codecBytes.length} bytes, want 1-255`);
  }
  const total = 4 + codecBytes.length + config.extradata.length;
  if (total > MAX_DATAGRAM_SIZE) {
    throw new WireError(`datagram ${total} bytes exceeds MAX_DATAGRAM_SIZE ${MAX_DATAGRAM_SIZE}`);
  }
  const dgram = new Uint8Array(total);
  dgram[0] = WIRE_VERSION;
  dgram[1] = TYPE_DECODER_CONFIG;
  dgram[2] = 0;
  dgram[3] = codecBytes.length;
  dgram.set(codecBytes, 4);
  dgram.set(config.extradata, 4 + codecBytes.length);
  return dgram;
}

export function parseDecoderConfig(dgram: Uint8Array): DecoderConfigMessage {
  if (dgram.length < 4) {
    throw new WireError(`datagram too short: ${dgram.length} bytes, need at least 4 for decoder config`);
  }
  if (dgram[0] !== WIRE_VERSION) {
    throw new WireError(`unsupported version 0x${dgram[0].toString(16)}`);
  }
  if (dgram[1] !== TYPE_DECODER_CONFIG) {
    throw new WireError(`unexpected message type 0x${dgram[1].toString(16)}, want decoder config`);
  }
  const codecLen = dgram[3];
  if (codecLen === 0) {
    throw new WireError('invalid codec string: empty');
  }
  if (4 + codecLen > dgram.length) {
    throw new WireError(`codecLen ${codecLen} overruns ${dgram.length}-byte datagram`);
  }
  return {
    codec: new TextDecoder().decode(dgram.subarray(4, 4 + codecLen)),
    extradata: dgram.subarray(4 + codecLen),
  };
}

// The 31 allowed broadcast-ID symbols (no 0/O, 1/I/L) — mirrors
// gawk-server/internal/broadcastid.
export const BROADCAST_ID_ALPHABET = '23456789ABCDEFGHJKMNPQRSTUVWXYZ';

export function parseBroadcastAnnounce(dgram: Uint8Array): string {
  if (dgram.length < 3) {
    throw new WireError(`message too short: ${dgram.length} bytes, need at least 3 for broadcast announce`);
  }
  if (dgram[0] !== WIRE_VERSION) {
    throw new WireError(`unsupported version 0x${dgram[0].toString(16)}`);
  }
  if (dgram[1] !== TYPE_BROADCAST_ANNOUNCE) {
    throw new WireError(`unexpected message type 0x${dgram[1].toString(16)}, want broadcast announce`);
  }
  const idLen = dgram[2];
  if (3 + idLen !== dgram.length) {
    throw new WireError(`expected ${3 + idLen} bytes for ID length ${idLen}, got ${dgram.length}`);
  }
  const id = new TextDecoder().decode(dgram.subarray(3));
  for (let i = 0; i < id.length; i++) {
    if (BROADCAST_ID_ALPHABET.indexOf(id[i]) === -1) {
      throw new WireError(`invalid character "${id[i]}" in broadcast ID`);
    }
  }
  return id;
}
