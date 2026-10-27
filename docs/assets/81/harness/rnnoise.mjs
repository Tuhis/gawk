// R80 NS0: the minimal RNNoise loader the spike uses (docs/81 §12). The same
// shape as the planned worklet loader: the module has no imports, so one
// synchronous `new WebAssembly.Instance(module, {})` is the whole start-up.
// Samples cross the boundary as ±1 floats; RNNoise wants int16 scale.

export const FRAME = 480;

export function instantiate(module) {
  const inst = new WebAssembly.Instance(module, {});
  const e = inst.exports;
  e._initialize();
  return {
    create() {
      const st = e.rnnoise_create(0);
      const inP = e.malloc(FRAME * 4);
      const outP = e.malloc(FRAME * 4);
      const heap = new Float32Array(e.memory.buffer);
      const inO = inP >> 2;
      const outO = outP >> 2;
      return {
        // One 480-sample frame in, one out; returns the VAD probability.
        process(input, output) {
          for (let i = 0; i < FRAME; i++) heap[inO + i] = input[i] * 32768;
          const p = e.rnnoise_process_frame(st, outP, inP);
          for (let i = 0; i < FRAME; i++) output[i] = heap[outO + i] / 32768;
          return p;
        },
        destroy() {
          e.free(inP);
          e.free(outP);
          e.rnnoise_destroy(st);
        },
      };
    },
  };
}

// A file of f32le samples. Copies out of the Buffer: readFileSync may hand
// back a slice of a shared pool, so `.buffer` alone is not the file.
export function f32(buf) {
  return new Float32Array(buf.buffer.slice(buf.byteOffset, buf.byteOffset + buf.byteLength));
}
