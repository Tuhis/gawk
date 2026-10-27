// The capture worklet (docs/80 D21; docs/81 D5–D7, D14), its source run in a
// fake worklet scope with the real rnnoise.wasm: render quanta in at 44.1,
// 48 or 96 kHz, 480-sample 48 kHz blocks out with nothing lost or repeated
// across quanta and the highs kept, RNNoise at the right scale, its late
// arrival, its failure and the watchdog.
import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  CAPTURE_PROCESSOR_NAME,
  CAPTURE_PROCESSOR_SOURCE,
  RESAMPLER_HALF_TAPS,
  WATCHDOG_WINDOW_MS,
  blockAudioDataInit,
} from './voiceFraming';
import { FRAME, frameEnergy, rnnoiseModule, runWorklet, scaleTo, syntheticSpeech, syntheticWhite } from './voiceTestKit';

const ramp = (n: number, from = 0, step = 1e-4) => Float32Array.from({ length: n }, (_, i) => (from + i) * step);

function joined(blocks: { pcm: Float32Array }[]): Float32Array {
  const out = new Float32Array(blocks.length * FRAME);
  blocks.forEach((b, i) => out.set(b.pcm, i * FRAME));
  return out;
}

function powerDb(x: Float32Array, from = 0): number {
  const e = frameEnergy(x).slice(from);
  return 10 * Math.log10(e.reduce((a, v) => a + v, 0) / e.length + 1e-20);
}

afterEach(() => vi.restoreAllMocks());

describe('blockAudioDataInit', () => {
  it('describes a block as f32-planar mono AudioData', () => {
    const pcm = ramp(480);
    expect(blockAudioDataInit(pcm, 48000, 1234)).toEqual({
      format: 'f32-planar',
      sampleRate: 48000,
      numberOfFrames: 480,
      numberOfChannels: 1,
      timestamp: 1234,
      data: pcm,
    });
  });
});

describe('the capture worklet: framing and resampling (D6)', () => {
  it('registers the processor under the name the node is built with', () => {
    expect(CAPTURE_PROCESSOR_SOURCE).toContain(`registerProcessor('${CAPTURE_PROCESSOR_NAME}'`);
  });

  it('at 48 kHz passes samples through untouched, in 480-sample blocks with consecutive seqs', () => {
    const w = runWorklet(48000, false);
    w.feed(ramp(15 * 128)); // 1920 = four blocks
    const blocks = w.blocks();
    expect(blocks.map((b) => b.seq)).toEqual([0, 1, 2, 3]);
    expect(blocks.every((b) => b.pcm.length === 480 && b.prob === -1)).toBe(true);
    expect(Array.from(joined(blocks))).toEqual(Array.from(ramp(1920)));
    // Each block owns its buffer (it is transferred).
    expect(blocks[0].pcm.buffer).not.toBe(blocks[1].pcm.buffer);
  });

  it('down-mixes stereo to the average', () => {
    const w = runWorklet(48000, false);
    for (let q = 0; q < 4; q++) w.processor.process([[new Float32Array(128).fill(1), new Float32Array(128).fill(0)]]);
    expect(w.blocks()[0].pcm.every((v) => v === 0.5)).toBe(true);
    // An empty input is skipped, not framed.
    expect(w.processor.process([[]])).toBe(true);
    expect(w.processor.process([])).toBe(true);
  });

  // A tone in, at a context rate; the 48 kHz blocks out, joined.
  function resampled(rate: number, hz: number, seconds = 0.5, quantum = 128): Float32Array {
    const w = runWorklet(rate, false);
    w.feed(Float32Array.from({ length: Math.floor(rate * seconds) }, (_, i) => 0.5 * Math.sin((2 * Math.PI * hz * i) / rate)), quantum);
    return joined(w.blocks());
  }
  // Amplitude of a tone in x, skipping the start-up, against 0.5, in dB.
  function toneDb(x: Float32Array): number {
    const tail = x.subarray(4800);
    const rms = Math.sqrt(tail.reduce((a, v) => a + v * v, 0) / tail.length);
    return 20 * Math.log10(rms / (0.5 / Math.SQRT2));
  }

  it.each([128, 100, 441, 7])('at 44.1 kHz resamples to 48 kHz with a fixed delay, across %i-sample quanta', (quantum) => {
    const out = resampled(44100, 1000, 0.5, quantum);
    // The kernel's half-width plus one input sample of delay.
    const delay = (RESAMPLER_HALF_TAPS + 1) / 44100;
    let worst = 0;
    for (let k = 4800; k < out.length; k++) {
      worst = Math.max(worst, Math.abs(out[k] - 0.5 * Math.sin(2 * Math.PI * 1000 * (k / 48000 - delay))));
    }
    expect(worst).toBeLessThan(1e-3);
    // 0.5 s in is 0.5 s out, less the delay and the block still filling.
    expect(out.length).toBeGreaterThan(24000 - 2 * FRAME);
    expect(out.length).toBeLessThanOrEqual(24000);
  });

  it('keeps the highs RNNoise hears: flat to 20 kHz from 44.1 kHz, where linear interpolation is not', () => {
    expect(Math.abs(toneDb(resampled(44100, 18000)))).toBeLessThan(0.5);
    expect(Math.abs(toneDb(resampled(44100, 20000)))).toBeLessThan(1);
    // Constant in, the same constant out (unity gain in every phase).
    const w = runWorklet(44100, false);
    w.feed(new Float32Array(44100 / 4).fill(0.25));
    const dc = joined(w.blocks()).subarray(480);
    expect(Math.max(...dc.map((v) => Math.abs(v - 0.25)))).toBeLessThan(1e-5);
  });

  it('from 96 kHz keeps a voice-band tone and removes what 48 kHz cannot carry', () => {
    expect(Math.abs(toneDb(resampled(96000, 1000)))).toBeLessThan(0.1);
    expect(toneDb(resampled(96000, 30000))).toBeLessThan(-40);
  });

  it('an odd rate too finely divided for a phase table falls back to linear interpolation', () => {
    const w = runWorklet(47999, false);
    w.feed(ramp(47999 / 4 | 0));
    const out = joined(w.blocks());
    const ratio = 47999 / 48000;
    let worst = 0;
    for (let k = 0; k < out.length; k++) worst = Math.max(worst, Math.abs(out[k] - k * ratio * 1e-4));
    expect(worst).toBeLessThan(1e-5);
  });

  it('the block stream does not depend on how the input is cut into quanta', () => {
    const x = syntheticWhite(0.3, 3).map((v) => v * 0.1);
    const a = runWorklet(44100, false);
    const b = runWorklet(44100, false);
    a.feed(x, 128);
    b.feed(x, 37);
    expect(Array.from(joined(b.blocks()))).toEqual(Array.from(joined(a.blocks())));
  });
});

describe('the capture worklet: RNNoise (D3, D7, D13, D14)', () => {
  it('denoises with the real wasm at int16 scale: noise drops, speech stays, probability per block', () => {
    const noise = runWorklet(48000, true);
    noise.send({ t: 'module', module: rnnoiseModule() });
    expect(noise.events[0]).toEqual({ t: 'ready' });
    const white = scaleTo(syntheticWhite(2, 1), -30);
    noise.feed(white);
    const nb = noise.blocks();
    expect(nb.every((b) => b.prob >= 0 && b.prob <= 1)).toBe(true);
    // After RNNoise's 20 ms, steady white noise comes out far quieter.
    expect(powerDb(white, 10) - powerDb(joined(nb), 10)).toBeGreaterThan(8);

    const speech = runWorklet(48000, true);
    speech.send({ t: 'module', module: rnnoiseModule() });
    const voiced = scaleTo(syntheticSpeech(3, 2), -26);
    speech.feed(voiced);
    const sb = speech.blocks();
    expect(powerDb(voiced) - powerDb(joined(sb))).toBeLessThan(3);
    // Fed at ±1 instead of int16 scale, RNNoise would hear near-silence and
    // never say speech.
    expect(sb.filter((b) => b.prob >= 0.9).length / sb.length).toBeGreaterThan(0.5);
  });

  it('runs without RNNoise until the module arrives, then switches with no gap (D13)', () => {
    const w = runWorklet(48000, true);
    w.feed(new Float32Array(960));
    w.send({ t: 'module', module: rnnoiseModule() });
    w.feed(new Float32Array(960));
    const blocks = w.blocks();
    expect(blocks.map((b) => b.prob >= 0)).toEqual([false, false, true, true]);
    expect(blocks.map((b) => b.seq)).toEqual([0, 1, 2, 3]);
  });

  it('denoise off stops RNNoise on the same stream, and on starts a fresh state from the kept module', () => {
    const w = runWorklet(48000, true);
    w.send({ t: 'module', module: rnnoiseModule() });
    w.feed(new Float32Array(480));
    w.send({ t: 'denoise', on: false });
    const x = ramp(480);
    w.feed(x);
    w.send({ t: 'denoise', on: true });
    w.feed(new Float32Array(480));
    const b = w.blocks();
    expect(b.map((x) => x.prob >= 0)).toEqual([true, false, true]);
    expect(Array.from(b[1].pcm)).toEqual(Array.from(x));
    expect(w.events.filter((e) => e.t === 'ready')).toHaveLength(2);
  });

  it('a module that is not RNNoise posts an error and the audio carries on undenoised', () => {
    const w = runWorklet(48000, true);
    // A valid module with none of RNNoise's exports.
    const empty = new WebAssembly.Module(new Uint8Array([0, 97, 115, 109, 1, 0, 0, 0]));
    w.send({ t: 'module', module: empty });
    expect(w.events[0].t).toBe('error');
    w.feed(new Float32Array(480));
    expect(w.blocks()[0].prob).toBe(-1);
  });

  it('the watchdog bypasses RNNoise above 40 % of real time over 2 s, and says how slow', () => {
    let t = 0;
    // Each Date.now() advances 5 ms: 5 ms per 10 ms block, 50 % of real time.
    vi.spyOn(Date, 'now').mockImplementation(() => (t += 5));
    const w = runWorklet(48000, true);
    w.send({ t: 'module', module: rnnoiseModule() });
    const windowBlocks = WATCHDOG_WINDOW_MS / 10;
    w.feed(new Float32Array(FRAME * (windowBlocks + 2)));
    const slow = w.events.find((e) => e.t === 'slow');
    expect(slow).toEqual({ t: 'slow', load: 0.5 });
    const blocks = w.blocks();
    expect(blocks[windowBlocks - 1].prob).toBeGreaterThanOrEqual(0);
    expect(blocks[windowBlocks].prob).toBe(-1);
  });

  it('stays under the watchdog at a fast clock', () => {
    let t = 0;
    vi.spyOn(Date, 'now').mockImplementation(() => (t += 1));
    const w = runWorklet(48000, true);
    w.send({ t: 'module', module: rnnoiseModule() });
    w.feed(new Float32Array(FRAME * 450));
    expect(w.events.some((e) => e.t === 'slow')).toBe(false);
  });

  it('close frees RNNoise and ends the processor', () => {
    const w = runWorklet(48000, true);
    w.send({ t: 'module', module: rnnoiseModule() });
    w.send({ t: 'close' });
    expect(w.processor.process([[new Float32Array(128)]])).toBe(false);
    w.send({ t: 'denoise', on: true });
    expect(w.events.filter((e) => e.t === 'ready')).toHaveLength(1);
    w.send(null as never);
  });
});
