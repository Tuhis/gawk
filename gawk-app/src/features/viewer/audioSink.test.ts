// R15 N4 (docs/20 Decision 7) + post-implementation review finding 3: the
// audio sink runs on the viewer's message path, so nothing inside it may
// propagate — audio is never allowed to break video. A detached buffer or a
// closed worklet port must cost one dropped packet, which is the correct
// live-edge outcome anyway.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { AudioSink, PROCESSOR_SOURCE } from './audioSink';
import type { AudioChunk } from '../../transport/audio-buffer';

const SAMPLE_RATE = 48000;
const FRAME_COUNT = SAMPLE_RATE / 50; // 20 ms

function chunk(timestampUs: number): AudioChunk {
  return {
    timestampUs,
    channels: [new Float32Array(FRAME_COUNT), new Float32Array(FRAME_COUNT)],
    sampleRate: SAMPLE_RATE,
    frameCount: FRAME_COUNT,
  };
}

// Stubs the minimum Web Audio surface the sink touches. `postMessage` is
// injectable so a test can make the worklet port throw.
function stubWebAudio(postMessage: (msg: unknown, transfer?: unknown) => void) {
  class FakeAudioWorkletNode {
    port = { postMessage, onmessage: null as unknown };
    connect = vi.fn();
    disconnect = vi.fn();
  }
  class FakeAudioContext {
    state = 'running';
    destination = {};
    audioWorklet = { addModule: vi.fn(() => Promise.resolve()) };
    createGain = vi.fn(() => ({ gain: { value: 1 }, connect: vi.fn(), disconnect: vi.fn() }));
    resume = vi.fn(() => Promise.resolve());
    close = vi.fn(() => Promise.resolve());
  }
  vi.stubGlobal('AudioContext', FakeAudioContext);
  vi.stubGlobal('AudioWorkletNode', FakeAudioWorkletNode);
  vi.stubGlobal('Blob', class {});
  vi.stubGlobal('URL', {
    createObjectURL: vi.fn(() => 'blob:stub'),
    revokeObjectURL: vi.fn(),
  });
}

beforeEach(() => {
  vi.stubGlobal('performance', { now: () => 0, timeOrigin: 0 });
});
afterEach(() => vi.unstubAllGlobals());

describe('AudioSink worklet delivery', () => {
  it('forwards chunks to the worklet with their channel buffers transferred', async () => {
    const posted: { msg: unknown; transfer: unknown }[] = [];
    stubWebAudio((msg, transfer) => posted.push({ msg, transfer }));
    const sink = new AudioSink();
    await sink.start(SAMPLE_RATE);

    // Three 20 ms chunks prime the 60 ms cushion (field finding 3), then the
    // whole cushion reaches the worklet in order.
    for (let i = 0; i < 3; i++) sink.push(chunk(i * 20_000));
    expect(posted).toHaveLength(3);
    const msg = posted[0].msg as { type: string; frameCount: number };
    expect(msg.type).toBe('chunk');
    expect(msg.frameCount).toBe(FRAME_COUNT);
    // The channel buffers ride the transfer list — no structured clone of PCM.
    expect((posted[0].transfer as ArrayBuffer[]).length).toBe(2);
    sink.dispose();
  });

  // The finding: without a guard this throw escapes sink.push() → the viewer's
  // message handler. Audio must never break video.
  it('swallows a throwing worklet port instead of propagating', async () => {
    stubWebAudio(() => {
      throw new DOMException('an ArrayBuffer is detached', 'DataCloneError');
    });
    const sink = new AudioSink();
    await sink.start(SAMPLE_RATE);

    expect(() => sink.push(chunk(0))).not.toThrow();
    // And the sink stays usable — a later packet is attempted, not latched off.
    expect(() => sink.push(chunk(20_000))).not.toThrow();
    sink.dispose();
  });

  it('drops audio silently before the worklet exists rather than throwing', () => {
    stubWebAudio(() => {});
    const sink = new AudioSink();
    // No start() — the node is null; the first packets of a stream can land
    // during the worklet's async boot.
    expect(() => sink.push(chunk(0))).not.toThrow();
    sink.dispose();
  });
});

// Field finding 7 (docs/20): the worklet drives the buffer's drain accounting
// through ~4 Hz playhead reports. When the AudioContext is suspended (Safari
// does this at will) the reports stop, the buffer's depth estimate freezes
// above the overflow ceiling, and every further chunk is dropped forever with
// no path back — total silence. The sink must detect the stall and recover.
describe('AudioSink stall recovery', () => {
  // A stub that hands back the created context + node so a test can drive the
  // playhead-report channel and flip the context to 'suspended'. Both stub
  // constructors return a shared object, so the handle the test holds is the
  // exact instance the sink uses.
  function stubWebAudioCapturing() {
    const posts: { type: string }[] = [];
    const node = {
      port: {
        postMessage: (m: unknown) => posts.push(m as { type: string }),
        onmessage: null as ((e: { data: unknown }) => void) | null,
      },
      connect: vi.fn(),
      disconnect: vi.fn(),
    };
    const ctx = {
      state: 'running' as AudioContextState,
      destination: {},
      audioWorklet: { addModule: vi.fn(() => Promise.resolve()) },
      createGain: vi.fn(() => ({ gain: { value: 1 }, connect: vi.fn(), disconnect: vi.fn() })),
      resume: vi.fn(() => Promise.resolve()),
      close: vi.fn(() => Promise.resolve()),
    };
    // A constructor returning an object makes `new` yield that object.
    vi.stubGlobal(
      'AudioContext',
      function () {
        return ctx;
      } as unknown as typeof AudioContext,
    );
    vi.stubGlobal(
      'AudioWorkletNode',
      function () {
        return node;
      } as unknown as typeof AudioWorkletNode,
    );
    vi.stubGlobal('Blob', class {});
    vi.stubGlobal('URL', { createObjectURL: vi.fn(() => 'blob:stub'), revokeObjectURL: vi.fn() });
    return { posts, ctx, getNode: () => node };
  }

  function playhead(node: { port: { onmessage: ((e: { data: unknown }) => void) | null } }) {
    node.port.onmessage!({
      data: { type: 'playhead', playheadUs: 1000, playedFrames: 100, underruns: 0, contextTime: 0 },
    });
  }

  it('flushes the worklet and resumes when reports stop (suspended context)', async () => {
    let now = 0;
    const { posts, ctx, getNode } = stubWebAudioCapturing();
    const sink = new AudioSink({}, undefined, { now: () => now });
    await sink.start(SAMPLE_RATE);
    const node = getNode();

    // Audio was playing: one playhead report has landed.
    playhead(node);
    posts.length = 0;

    // The context suspends and the worklet goes silent, but audio keeps
    // arriving. Time advances past the stall threshold.
    ctx.state = 'suspended';
    now = 3000;
    sink.push(chunk(0));

    expect(ctx.resume).toHaveBeenCalled();
    expect(posts.some((m) => m.type === 'flush')).toBe(true);
    sink.dispose();
  });

  it('does not flush while the worklet is reporting normally', async () => {
    let now = 0;
    const { posts, getNode } = stubWebAudioCapturing();
    const sink = new AudioSink({}, undefined, { now: () => now });
    await sink.start(SAMPLE_RATE);
    const node = getNode();

    // Healthy: reports arrive every ~250 ms, so no gap ever exceeds the
    // threshold. Push audio across a couple of report intervals.
    for (let i = 0; i < 20; i++) {
      now = i * 200;
      if (i % 1 === 0) playhead(node);
      sink.push(chunk(i * 20_000));
    }
    expect(posts.some((m) => m.type === 'flush')).toBe(false);
    sink.dispose();
  });
});

// The worklet processor ships as a source string, so it never ran under test —
// yet with the drift trim (docs/20 field finding 4) it is the code that touches
// every audio sample. Instantiate it against stubbed worklet globals and drive
// it directly: a resampler that steps or skips is exactly what the "smooth over
// a long period" requirement forbids.
describe('audio worklet resampler', () => {
  function instantiate() {
    class FakeAudioWorkletProcessor {
      port = { onmessage: null as ((e: { data: unknown }) => void) | null, postMessage: vi.fn() };
    }
    let Processor: (new () => WorkletProcessor) | null = null;
    const register = (_name: string, cls: new () => WorkletProcessor) => {
      Processor = cls;
    };
    // currentTime as a constant keeps the 4 Hz report from firing here.
    new Function('AudioWorkletProcessor', 'registerProcessor', 'currentTime', PROCESSOR_SOURCE)(
      FakeAudioWorkletProcessor,
      register,
      0,
    );
    return new Processor!();
  }

  interface WorkletProcessor {
    port: { onmessage: ((e: { data: unknown }) => void) | null; postMessage: (m: unknown) => void };
    offset: number;
    playedFrames: number;
    underruns: number;
    process(inputs: unknown[], outputs: Float32Array[][]): boolean;
  }

  const QUANTUM = 128;
  // A ramp makes any discontinuity visible: a skip or a step shows up as a
  // jump in the first difference, which silence or a constant would hide.
  function ramp(start: number, count: number): Float32Array {
    const a = new Float32Array(count);
    for (let i = 0; i < count; i++) a[i] = (start + i) / 100_000;
    return a;
  }
  function feed(p: WorkletProcessor, startSample: number, frameCount: number, timestampUs: number) {
    p.port.onmessage!({
      data: {
        type: 'chunk',
        channels: [ramp(startSample, frameCount), ramp(startSample, frameCount)],
        frameCount,
        sampleRate: SAMPLE_RATE,
        timestampUs,
      },
    });
  }
  function drain(p: WorkletProcessor, quanta: number): number[] {
    const out: number[] = [];
    for (let q = 0; q < quanta; q++) {
      const buf = [new Float32Array(QUANTUM), new Float32Array(QUANTUM)];
      p.process([], [buf]);
      for (let i = 0; i < QUANTUM; i++) out.push(buf[0]![i]!);
    }
    return out;
  }

  it('reproduces the source exactly at 1x', () => {
    const p = instantiate();
    feed(p, 0, FRAME_COUNT, 0);
    const out = drain(p, 4);
    for (let i = 0; i < out.length; i++) expect(out[i]).toBeCloseTo(i / 100_000, 9);
  });

  it('consumes source faster/slower than realtime under trim, without discontinuity', () => {
    for (const rate of [1.004, 0.996]) {
      const p = instantiate();
      p.port.onmessage!({ data: { type: 'rate', rate } });
      // Several chunks so the read crosses chunk boundaries repeatedly.
      for (let c = 0; c < 6; c++) feed(p, c * FRAME_COUNT, FRAME_COUNT, c * 20_000);
      const out = drain(p, 8);

      // The trim's whole purpose: source consumed ≠ output produced.
      expect(p.playedFrames).toBeCloseTo(8 * QUANTUM * rate, 3);
      expect(p.underruns).toBe(0);

      // And it stays a ramp: every step is the same size, including across
      // chunk boundaries, where dropping the fractional remainder would show
      // up as a visible seam.
      // Tolerance is float32 quantization of the ramp itself (~1e-10 on these
      // deltas), not slack: a dropped sample would be a step of 1e-5 here —
      // four orders of magnitude larger and impossible to miss.
      const step = rate / 100_000;
      for (let i = 1; i < out.length; i++) {
        expect(out[i]! - out[i - 1]!).toBeCloseTo(step, 8);
      }
    }
  });

  it('counts an underrun and emits silence when it runs dry', () => {
    const p = instantiate();
    feed(p, 0, QUANTUM, 0);
    drain(p, 1); // consumes the single chunk exactly
    const buf = [new Float32Array(QUANTUM), new Float32Array(QUANTUM)];
    p.process([], [buf]);
    expect(p.underruns).toBeGreaterThan(0);
    expect(buf[0]!.every((s) => s === 0)).toBe(true);
  });
});
