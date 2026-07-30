// Broadcaster-side packetization: one encoded frame → a sequence of
// VideoChunk datagrams that each fit the relay's safe datagram size.
// Pure functions over bytes (no WebCodecs types) so they are unit-testable
// in node; the broadcaster pipeline does the EncodedVideoChunk.copyTo.

import {
  MAX_CHUNK_PAYLOAD,
  MAX_DATAGRAM_SIZE,
  VIDEO_CHUNK_HEADER_SIZE,
  encodeDecoderConfig,
  encodeStreamFrame,
  encodeVideoChunk,
  WireError,
} from './wire';
import { MAX_PARITY_DATA_CHUNKS, computeParity, encodeParityChunk } from './parity';

export interface FrameInfo {
  frameId: number; // uint32, monotonic per broadcast session
  keyframe: boolean;
  timestampUs: bigint;
}

// Splits data into datagrams that fit within the path MTU. A zero-length
// frame still produces one chunk so the frame exists on the wire. Throws
// WireError if the frame would need more than 65535 chunks.
export function packetizeFrame(
  info: FrameInfo,
  data: Uint8Array,
  pathMaxDatagramSize = MAX_DATAGRAM_SIZE,
): Uint8Array<ArrayBuffer>[] {
  const maxPayload = Math.min(MAX_CHUNK_PAYLOAD, Math.max(1, pathMaxDatagramSize - VIDEO_CHUNK_HEADER_SIZE));
  const chunkCount = Math.max(1, Math.ceil(data.length / maxPayload));
  if (chunkCount > 0xffff) {
    throw new WireError(`frame of ${data.length} bytes needs ${chunkCount} chunks, max 65535`);
  }
  const datagrams: Uint8Array<ArrayBuffer>[] = new Array(chunkCount);
  for (let i = 0; i < chunkCount; i++) {
    const payload = data.subarray(i * maxPayload, (i + 1) * maxPayload);
    datagrams[i] = encodeVideoChunk(
      {
        keyframe: info.keyframe,
        frameId: info.frameId,
        chunkIndex: i,
        chunkCount,
        timestampUs: info.timestampUs,
      },
      payload,
    );
  }
  return datagrams;
}

// Splits data into datagrams and, when parityLevel > 0, computes up to that
// many RAID-6 P/Q parity symbols over the chunk PAYLOADS (R29, docs/34).
//
// Parity covers payloads rather than whole datagrams because payloads are
// what the viewer reassembles — covering headers would still round-trip on a
// clean link and fail only under the loss the feature exists for.
//
// Deltas only. Callers must not ask for parity on a keyframe: keyframes ride
// reliable uni streams (R8) and are not exposed to datagram loss.
//
// A frame needing more than MAX_PARITY_DATA_CHUNKS chunks degrades to plain
// datagrams rather than throwing — past that bound the Q coefficients wrap
// and the code stops being MDS, and a ~300 KB delta is not worth failing a
// broadcast over.
export function packetizeFrameWithParity(
  info: FrameInfo,
  data: Uint8Array,
  parityLevel: number,
  pathMaxDatagramSize = MAX_DATAGRAM_SIZE,
): { datagrams: Uint8Array<ArrayBuffer>[]; parity: Uint8Array<ArrayBuffer>[] } {
  const datagrams = packetizeFrame(info, data, pathMaxDatagramSize);
  if (parityLevel <= 0 || datagrams.length > MAX_PARITY_DATA_CHUNKS) {
    return { datagrams, parity: [] };
  }
  const payloads = datagrams.map((d) => d.subarray(VIDEO_CHUNK_HEADER_SIZE));
  const symbols = computeParity(payloads, parityLevel);
  const parity = symbols.map((payload, i) =>
    encodeParityChunk(
      { frameId: info.frameId, parityIndex: i, chunkCount: datagrams.length, frameBytes: data.length },
      payload,
    ),
  );
  return { datagrams, parity };
}

// Builds the DecoderConfig datagram for a WebCodecs decoder config. The
// wire format carries only codec + extradata; width/height come from the
// bitstream on the viewer side.
export function packetizeDecoderConfig(
  codec: string,
  description?: AllowSharedBufferSource,
): Uint8Array<ArrayBuffer> {
  return encodeDecoderConfig({ codec, extradata: toUint8Array(description) });
}

// Builds the single StreamFrame message a keyframe travels in over a reliable
// unidirectional stream (R8): header + the current DecoderConfig datagram
// (embedded so a delivered keyframe is self-sufficient to decode) + the encoded
// keyframe payload. configDatagram may be empty when no config is available yet
// (the viewer then relies on an earlier one).
export function packetizeStreamKeyframe(
  info: { frameId: number; timestampUs: bigint },
  configDatagram: Uint8Array,
  payload: Uint8Array,
): Uint8Array<ArrayBuffer> {
  return encodeStreamFrame(
    { keyframe: true, frameId: info.frameId, timestampUs: info.timestampUs },
    configDatagram,
    payload,
  );
}

function toUint8Array(src?: AllowSharedBufferSource): Uint8Array {
  if (!src) return new Uint8Array(0);
  if (src instanceof ArrayBuffer || src instanceof SharedArrayBuffer) return new Uint8Array(src);
  return new Uint8Array(src.buffer, src.byteOffset, src.byteLength);
}
