// Broadcaster-side packetization: one encoded frame → a sequence of
// VideoChunk datagrams that each fit the relay's safe datagram size.
// Pure functions over bytes (no WebCodecs types) so they are unit-testable
// in node; the broadcaster pipeline does the EncodedVideoChunk.copyTo.

import {
  MAX_CHUNK_PAYLOAD,
  MAX_DATAGRAM_SIZE,
  VIDEO_CHUNK_HEADER_SIZE,
  encodeDecoderConfig,
  encodeVideoChunk,
  WireError,
} from './wire';

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

// Builds the DecoderConfig datagram for a WebCodecs decoder config. The
// wire format carries only codec + extradata; width/height come from the
// bitstream on the viewer side.
export function packetizeDecoderConfig(
  codec: string,
  description?: AllowSharedBufferSource,
): Uint8Array<ArrayBuffer> {
  return encodeDecoderConfig({ codec, extradata: toUint8Array(description) });
}

function toUint8Array(src?: AllowSharedBufferSource): Uint8Array {
  if (!src) return new Uint8Array(0);
  if (src instanceof ArrayBuffer || src instanceof SharedArrayBuffer) return new Uint8Array(src);
  return new Uint8Array(src.buffer, src.byteOffset, src.byteLength);
}
