// RNNoise for room voice (R80, docs/81 D2–D4, D13): the SIMD check and the
// module download. The wasm is built from pinned source in gawk-app/rnnoise/
// (never hand-edited: CI rebuilds it and byte-compares) and Vite emits it as
// a content-hashed asset, fetched only when someone joins voice with the
// microphone. The worklet instantiates the compiled module itself
// (voiceFraming.ts).

import rnnoiseUrl from '../../../../rnnoise/rnnoise.wasm?url';

export const RNNOISE_URL: string = rnnoiseUrl;

// The smallest module that uses a v128 instruction (wasm-feature-detect's
// SIMD probe): it validates only where the engine has wasm SIMD, which the
// one RNNoise build needs (D3).
const SIMD_PROBE = new Uint8Array([
  0, 97, 115, 109, 1, 0, 0, 0, 1, 5, 1, 96, 0, 1, 123, 3, 2, 1, 0, 10, 10, 1, 8, 0, 65, 0, 253, 15, 253, 98, 11,
]);

// Synchronous, so it runs before the first getUserMedia (D13).
export function wasmSimdSupported(): boolean {
  try {
    return typeof WebAssembly === 'object' && WebAssembly.validate(SIMD_PROBE);
  } catch {
    return false;
  }
}

// Fetches and compiles the module. Streaming where the server says
// application/wasm (nginx and Vite both do), else from the bytes. Rejects
// with a short reason the Audio section can show.
export async function compileRnnoise(
  url: string = RNNOISE_URL,
  fetchFn: typeof fetch = (...a) => fetch(...a),
): Promise<WebAssembly.Module> {
  let res: Response;
  try {
    res = await fetchFn(url);
  } catch {
    throw new Error('the download failed');
  }
  if (!res.ok) throw new Error(`the download failed (HTTP ${res.status})`);
  try {
    const streaming =
      typeof WebAssembly.compileStreaming === 'function' &&
      (res.headers.get('content-type') ?? '').startsWith('application/wasm');
    return streaming ? await WebAssembly.compileStreaming(res) : await WebAssembly.compile(await res.arrayBuffer());
  } catch (e) {
    throw new Error(`it could not be compiled (${e instanceof Error ? e.message : String(e)})`);
  }
}
