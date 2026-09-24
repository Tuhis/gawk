// The bytes of a WebCodecs BufferSource, as a view (no copy). Tests for a view
// instead of naming SharedArrayBuffer, which is undefined on pages that are
// not cross-origin isolated (this app never is).
export function bufferSourceBytes(src: AllowSharedBufferSource): Uint8Array {
  return ArrayBuffer.isView(src)
    ? new Uint8Array(src.buffer, src.byteOffset, src.byteLength)
    : new Uint8Array(src);
}
