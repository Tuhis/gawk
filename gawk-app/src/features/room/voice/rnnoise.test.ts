// The RNNoise loader (docs/81 D3, D4, D13): the SIMD probe, the streaming
// and byte paths, and the reasons a failure is reported with.
import { describe, expect, it, vi } from 'vitest';
import { RNNOISE_URL, compileRnnoise, wasmSimdSupported } from './rnnoise';
import { rnnoiseModule } from './voiceTestKit';
import { readFileSync } from 'node:fs';

const bytes = () => readFileSync(new URL('../../../../rnnoise/rnnoise.wasm', import.meta.url));

function response(body: BodyInit | null, init: ResponseInit) {
  return new Response(body, init);
}

describe('rnnoise.ts', () => {
  it('the asset is the committed wasm, emitted by Vite', () => {
    expect(RNNOISE_URL).toMatch(/rnnoise\.wasm$/);
  });

  it('the committed module imports nothing and exports what the worklet calls (D3)', () => {
    const m = rnnoiseModule();
    expect(WebAssembly.Module.imports(m)).toEqual([]);
    const names = WebAssembly.Module.exports(m).map((e) => e.name);
    for (const n of ['memory', '_initialize', 'malloc', 'free', 'rnnoise_create', 'rnnoise_process_frame', 'rnnoise_destroy']) {
      expect(names).toContain(n);
    }
  });

  it('detects wasm SIMD, and says no where validate throws or WebAssembly is missing', () => {
    expect(wasmSimdSupported()).toBe(true);
    const spy = vi.spyOn(WebAssembly, 'validate').mockReturnValue(false);
    expect(wasmSimdSupported()).toBe(false);
    spy.mockImplementation(() => {
      throw new Error('x');
    });
    expect(wasmSimdSupported()).toBe(false);
    spy.mockRestore();
  });

  it('compiles streaming when served as application/wasm, and from the bytes otherwise', async () => {
    const f = vi.fn(async () => response(bytes(), { headers: { 'content-type': 'application/wasm' } }));
    const streamed = await compileRnnoise('/a.wasm', f as unknown as typeof fetch);
    expect(f).toHaveBeenCalledWith('/a.wasm');
    expect(WebAssembly.Module.exports(streamed).length).toBeGreaterThan(0);
    const plain = await compileRnnoise('/a.wasm', (async () =>
      response(bytes(), { headers: { 'content-type': 'application/octet-stream' } })) as unknown as typeof fetch);
    expect(WebAssembly.Module.imports(plain)).toEqual([]);
  });

  it('rejects with a reason for a failed fetch, an HTTP error and bytes that are not wasm', async () => {
    await expect(
      compileRnnoise('/x', (async () => Promise.reject(new TypeError('offline'))) as unknown as typeof fetch),
    ).rejects.toThrow('the download failed');
    await expect(
      compileRnnoise('/x', (async () => response('nope', { status: 404 })) as unknown as typeof fetch),
    ).rejects.toThrow('the download failed (HTTP 404)');
    await expect(
      compileRnnoise('/x', (async () => response('not wasm', { status: 200 })) as unknown as typeof fetch),
    ).rejects.toThrow(/could not be compiled/);
  });
});
