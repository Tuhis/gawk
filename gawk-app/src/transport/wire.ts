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
// TypeStreamFrame (R8): never a datagram — the payload of one unidirectional
// stream carrying exactly one keyframe. See encode/parseStreamFrame below.
export const TYPE_STREAM_FRAME = 0x04;

export const CLOSE_CODE_BROADCAST_ENDED = 4000;
// The relay evicted this subscriber because its keyframe stream opens failed
// persistently (R10, docs/14 — typically a zombie session with exhausted
// stream credit). Non-terminal by design: the viewer's normal reconnect
// applies (a fresh session restores stream credit), so no special handling —
// mirrored from Go wire.CloseCodeSubscriberUnresponsive for namespace parity.
export const CLOSE_CODE_SUBSCRIBER_UNRESPONSIVE = 4001;

export const MAX_DATAGRAM_SIZE = 1200;
export const VIDEO_CHUNK_HEADER_SIZE = 20;
export const MAX_CHUNK_PAYLOAD = MAX_DATAGRAM_SIZE - VIDEO_CHUNK_HEADER_SIZE;
// Mirrors wire.MaxChunkCount: the relay drops any chunk whose count exceeds
// this (memory-inflation defense), so a frame that needs more chunks can
// never reach viewers — the encoder must fail loudly instead.
export const MAX_CHUNK_COUNT = 3000;

// StreamFrame constants (mirror gawk-server/internal/wire).
export const STREAM_FRAME_HEADER_SIZE = 24;
// Absolute ceiling on one StreamFrame message (header + config + payload); the
// stream analogue of MAX_CHUNK_COUNT. A reader must never allocate beyond it
// from an untrusted length field.
export const MAX_KEYFRAME_BYTES = 8 * 1024 * 1024;

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
  if (header.chunkCount > MAX_CHUNK_COUNT) {
    throw new WireError(`chunk count ${header.chunkCount} exceeds MAX_CHUNK_COUNT ${MAX_CHUNK_COUNT}`);
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
  if (header.chunkCount > MAX_CHUNK_COUNT) {
    throw new WireError(`chunk count ${header.chunkCount} exceeds MAX_CHUNK_COUNT ${MAX_CHUNK_COUNT}`);
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

export interface StreamFrameHeader {
  keyframe: boolean;
  frameId: number; // uint32, shared numbering with datagram VideoChunks
  timestampUs: bigint; // uint64
  configLen: number; // uint32, byte length of the embedded DecoderConfig datagram (0 = none)
  payloadLen: number; // uint32, byte length of the encoded keyframe
}

// Encodes the 24-byte StreamFrame header (R8). Rejects a declared total
// exceeding MAX_KEYFRAME_BYTES.
export function encodeStreamFrameHeader(header: StreamFrameHeader): Uint8Array<ArrayBuffer> {
  if (STREAM_FRAME_HEADER_SIZE + header.configLen + header.payloadLen > MAX_KEYFRAME_BYTES) {
    throw new WireError(
      `stream frame ${header.configLen + header.payloadLen} body bytes exceeds MAX_KEYFRAME_BYTES ${MAX_KEYFRAME_BYTES}`,
    );
  }
  const buf = new Uint8Array(STREAM_FRAME_HEADER_SIZE);
  const view = new DataView(buf.buffer);
  buf[0] = WIRE_VERSION;
  buf[1] = TYPE_STREAM_FRAME;
  buf[2] = header.keyframe ? FLAG_KEYFRAME : 0;
  buf[3] = 0;
  view.setUint32(4, header.frameId);
  view.setBigUint64(8, header.timestampUs);
  view.setUint32(16, header.configLen);
  view.setUint32(20, header.payloadLen);
  return buf;
}

// Builds a complete StreamFrame message: header + optional config block + the
// encoded keyframe payload. config is a full DecoderConfig datagram (its
// 0x01/0x02 prefix included) or empty.
export function encodeStreamFrame(
  meta: { keyframe: boolean; frameId: number; timestampUs: bigint },
  config: Uint8Array,
  payload: Uint8Array,
): Uint8Array<ArrayBuffer> {
  const header = encodeStreamFrameHeader({
    ...meta,
    configLen: config.length,
    payloadLen: payload.length,
  });
  const out = new Uint8Array(header.length + config.length + payload.length);
  out.set(header, 0);
  out.set(config, header.length);
  out.set(payload, header.length + config.length);
  return out;
}

// Parses the 24-byte StreamFrame header at the start of buf. Does not require
// buf to hold the whole message (the reader consumes configLen + payloadLen
// further bytes from the stream). Rejects a declared total exceeding
// MAX_KEYFRAME_BYTES.
export function parseStreamFrameHeader(buf: Uint8Array): StreamFrameHeader {
  if (buf.length < STREAM_FRAME_HEADER_SIZE) {
    throw new WireError(
      `stream frame header too short: ${buf.length} bytes, need at least ${STREAM_FRAME_HEADER_SIZE}`,
    );
  }
  if (buf[0] !== WIRE_VERSION) {
    throw new WireError(`unsupported version 0x${buf[0].toString(16)}`);
  }
  if (buf[1] !== TYPE_STREAM_FRAME) {
    throw new WireError(`unexpected message type 0x${buf[1].toString(16)}, want stream frame`);
  }
  const view = new DataView(buf.buffer, buf.byteOffset, buf.byteLength);
  const header: StreamFrameHeader = {
    keyframe: (buf[2] & FLAG_KEYFRAME) !== 0,
    frameId: view.getUint32(4),
    timestampUs: view.getBigUint64(8),
    configLen: view.getUint32(16),
    payloadLen: view.getUint32(20),
  };
  if (STREAM_FRAME_HEADER_SIZE + header.configLen + header.payloadLen > MAX_KEYFRAME_BYTES) {
    throw new WireError(
      `stream frame ${header.configLen + header.payloadLen} body bytes exceeds MAX_KEYFRAME_BYTES ${MAX_KEYFRAME_BYTES}`,
    );
  }
  return header;
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
