// The bytes of a WebCodecs BufferSource, as a view (no copy). Tests for a view
// instead of naming SharedArrayBuffer, which is undefined on pages that are
// not cross-origin isolated (this app never is).
export function bufferSourceBytes(src: AllowSharedBufferSource): Uint8Array {
  return ArrayBuffer.isView(src)
    ? new Uint8Array(src.buffer, src.byteOffset, src.byteLength)
    : new Uint8Array(src);
}

// Reads a server-opened uni stream to its end and returns its bytes as one
// message (the relay's control messages each ride a stream of their own, and
// may arrive in several chunks).
export async function readStreamBytes(stream: ReadableStream<Uint8Array>): Promise<Uint8Array> {
  const chunks: Uint8Array[] = [];
  const reader = stream.getReader();
  try {
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      if (value) chunks.push(value);
    }
  } finally {
    reader.releaseLock();
  }
  let total = 0;
  for (const c of chunks) total += c.length;
  const out = new Uint8Array(total);
  let offset = 0;
  for (const c of chunks) {
    out.set(c, offset);
    offset += c.length;
  }
  return out;
}
