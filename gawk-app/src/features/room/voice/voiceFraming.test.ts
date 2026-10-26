// The AudioWorklet capture path's framing (docs/80 D21): arbitrary render
// quanta in, exact 10 ms blocks out, timestamps on the sample clock.
import { describe, expect, it } from 'vitest';
import {
  CAPTURE_PROCESSOR_NAME,
  CAPTURE_PROCESSOR_SOURCE,
  PcmBlocker,
  blockAudioDataInit,
  blockFramesFor,
  downmixToMono,
} from './voiceFraming';

const ramp = (n: number, from = 0) => Float32Array.from({ length: n }, (_, i) => (from + i) / 10000);

describe('blockFramesFor', () => {
  it('is 10 ms of samples at the context rate', () => {
    expect(blockFramesFor(48000)).toBe(480);
    expect(blockFramesFor(44100)).toBe(441);
    expect(blockFramesFor(16000, 20)).toBe(320);
    expect(blockFramesFor(10)).toBe(1);
  });
});

describe('PcmBlocker', () => {
  it('cuts 128-frame render quanta into 480-sample blocks, in order and gapless', () => {
    const b = new PcmBlocker(48000);
    const out: { pcm: Float32Array; timestampUs: number }[] = [];
    let fed = 0;
    for (let q = 0; q < 15; q++) {
      out.push(...b.push(ramp(128, fed)));
      fed += 128;
    }
    // 15 × 128 = 1920 = exactly four blocks.
    expect(out).toHaveLength(4);
    expect(out.map((x) => x.pcm.length)).toEqual([480, 480, 480, 480]);
    expect(out.map((x) => x.timestampUs)).toEqual([0, 10_000, 20_000, 30_000]);
    const joined = new Float32Array(1920);
    out.forEach((x, i) => joined.set(x.pcm, i * 480));
    expect(Array.from(joined)).toEqual(Array.from(ramp(1920)));
  });

  it('holds a partial block until it fills, and every block owns its buffer', () => {
    const b = new PcmBlocker(48000);
    expect(b.push(ramp(479))).toHaveLength(0);
    const [first] = b.push(ramp(2, 479));
    expect(first.pcm[479]).toBeCloseTo(479 / 10000);
    const [second] = b.push(ramp(479, 481));
    expect(second.pcm.buffer).not.toBe(first.pcm.buffer);
    expect(second.timestampUs).toBe(10_000);
  });

  it('emits several blocks from one big chunk, at a 44.1 kHz context', () => {
    const b = new PcmBlocker(44100);
    const out = b.push(ramp(441 * 3 + 5));
    expect(out).toHaveLength(3);
    expect(out[2].timestampUs).toBe(20_000);
  });

  it('refuses a nonsensical rate', () => {
    expect(() => new PcmBlocker(0)).toThrow();
  });
});

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

describe('downmixToMono', () => {
  it('copies mono, averages stereo, and is empty for no channels', () => {
    const mono = new Float32Array([0.1, 0.2]);
    const copy = downmixToMono([mono]);
    expect(Array.from(copy)).toEqual(Array.from(mono));
    expect(copy).not.toBe(mono);
    expect(Array.from(downmixToMono([new Float32Array([1, 0]), new Float32Array([0, 1])]))).toEqual([0.5, 0.5]);
    expect(downmixToMono([]).length).toBe(0);
  });
});

describe('the capture worklet source', () => {
  it('registers the processor under the name the node is built with', () => {
    expect(CAPTURE_PROCESSOR_SOURCE).toContain(`registerProcessor('${CAPTURE_PROCESSOR_NAME}'`);
  });

  it('posts each quantum down-mixed to mono', () => {
    // Run the source against a minimal AudioWorkletGlobalScope.
    let Processor: (new () => { port: { postMessage: (m: Float32Array) => void }; process: (i: Float32Array[][]) => boolean }) | null = null;
    const posted: Float32Array[] = [];
    class AudioWorkletProcessor {
      port = { postMessage: (m: Float32Array) => posted.push(m) };
    }
    const registerProcessor = (_name: string, cls: typeof Processor) => {
      Processor = cls;
    };
    new Function('AudioWorkletProcessor', 'registerProcessor', CAPTURE_PROCESSOR_SOURCE)(AudioWorkletProcessor, registerProcessor);
    const p = new Processor!();
    expect(p.process([[new Float32Array([1, 1]), new Float32Array([0, 1])]])).toBe(true);
    expect(p.process([[]])).toBe(true);
    expect(posted).toHaveLength(1);
    expect(Array.from(posted[0])).toEqual([0.5, 1]);
  });
});
