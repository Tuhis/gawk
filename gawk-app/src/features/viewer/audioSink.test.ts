// R15 N4 (docs/20 Decision 7) + post-implementation review finding 3: the
// audio sink runs on the viewer's message path, so nothing inside it may
// propagate — audio is never allowed to break video. A detached buffer or a
// closed worklet port must cost one dropped packet, which is the correct
// live-edge outcome anyway.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { AudioSink, PROCESSOR_SOURCE } from './audioSink';
import {
  AudioRateController,
  notePlayhead,
  observeVideoPresented,
  resetAvSync,
} from '../../transport/av-sync';
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

  function playhead(
    node: { port: { onmessage: ((e: { data: unknown }) => void) | null } },
    queuedMs = 40,
    receivedMs = 0,
  ) {
    node.port.onmessage!({
      data: { type: 'playhead', playheadUs: 1000, queuedMs, receivedMs, underruns: 0, contextTime: 0 },
    });
  }

  // BUGS.md (2026-07-22): once the stream dies the worklet keeps running and
  // keeps reporting an underrun for every 128-sample quantum, forever — a
  // capture showed `underruns` at 300386 climbing ~375/s with no audio
  // arriving at all. Underrunning with nothing to play is not a defect; it is
  // silence working correctly. Counting it destroys the counter's value as a
  // severity measure exactly when someone is reading it to diagnose a freeze.
  it('stops counting underruns once no audio is arriving', async () => {
    const cap = stubWebAudioCapturing();
    let now = 0;
    vi.stubGlobal('performance', { now: () => now, timeOrigin: 0 });
    const sink = new AudioSink();
    await sink.start(SAMPLE_RATE);
    const node = cap.getNode();

    const report = (underruns: number) =>
      node.port.onmessage!({
        data: {
          type: 'playhead',
          playheadUs: 1000,
          queuedMs: 0,
          receivedMs: 0,
          underruns,
          contextTime: 0,
        },
      });

    // Audio is flowing and playback has started, then the sink runs dry: a
    // real dry-after-playback underrun, counted. Playback needs the 60 ms seed
    // floor to be met first (3 × 20 ms chunks) — before that the worklet's dry
    // quanta are expected pre-roll and are deliberately not counted.
    sink.push(chunk(0));
    sink.push(chunk(20_000));
    sink.push(chunk(40_000));
    report(4);
    expect(sink.getStats().underruns).toBe(4);

    // The stream dies. Past the expectation window the worklet keeps reporting
    // dry quanta forever; none of them says anything about audio health,
    // because there is no audio left to play.
    now += 5000;
    const counted = sink.getStats().underruns;
    for (let i = 0; i < 20; i++) {
      now += 1000;
      report(188);
    }
    expect(sink.getStats().underruns).toBe(counted);
  });

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
  // `sampleRate` and `currentTime` are real AudioWorkletGlobalScope globals, so
  // they are stubbed as globals rather than passed in: the processor reads the
  // context's rate to know how fast to consume source samples, and a test that
  // wants a report drives currentTime past the 4 Hz threshold.
  function instantiate(contextRate = SAMPLE_RATE) {
    vi.stubGlobal('sampleRate', contextRate);
    vi.stubGlobal('currentTime', 0);
    class FakeAudioWorkletProcessor {
      port = { onmessage: null as ((e: { data: unknown }) => void) | null, postMessage: vi.fn() };
    }
    let Processor: (new () => WorkletProcessor) | null = null;
    const register = (_name: string, cls: new () => WorkletProcessor) => {
      Processor = cls;
    };
    new Function('AudioWorkletProcessor', 'registerProcessor', PROCESSOR_SOURCE)(
      FakeAudioWorkletProcessor,
      register,
    );
    return new Processor!();
  }

  // Drives one quantum with the clock past the report threshold and returns the
  // report the worklet posted.
  function report(p: WorkletProcessor): {
    type: string;
    queuedMs: number;
    receivedMs: number;
    playheadUs: number | null;
    underruns: number;
  } {
    vi.stubGlobal('currentTime', 1);
    p.process([], [[new Float32Array(QUANTUM), new Float32Array(QUANTUM)]]);
    vi.stubGlobal('currentTime', 0);
    const calls = (p.port.postMessage as unknown as { mock: { calls: unknown[][] } }).mock.calls;
    return calls[calls.length - 1]![0] as never;
  }

  interface WorkletProcessor {
    port: { onmessage: ((e: { data: unknown }) => void) | null; postMessage: (m: unknown) => void };
    offset: number;
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
      const out = drain(p, 7);

      // The trim's whole purpose: source consumed ≠ output produced. Measured
      // through the depth it leaves behind, which is now what the worklet
      // reports: 6 chunks in, 8 quanta of output, so the source consumed is
      // 8 * QUANTUM * rate frames.
      const consumedMs = ((8 * QUANTUM * rate) / SAMPLE_RATE) * 1000;
      expect(report(p).queuedMs).toBeCloseTo(6 * 20 - consumedMs, 3);
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

  // The sample-rate fix. macOS/Safari hands back a 44.1 kHz context routinely
  // (it is the device rate), while Opus decodes to 48 kHz. Playing 48 kHz
  // content one sample per output frame there is 8.8 % slow and a semitone
  // low — and it also under-drains the queue by 8 %/s, which walks any
  // inferred depth estimate straight to the overflow ceiling (field
  // finding 8). The resampler the drift trim already needed is the fix: the
  // base read rate is content rate ÷ context rate, and the trim multiplies it.
  it('resamples source content to the context rate', () => {
    const p = instantiate(44_100);
    for (let c = 0; c < 4; c++) feed(p, c * FRAME_COUNT, FRAME_COUNT, c * 20_000);
    const out = drain(p, 6);

    // Source advances 48000/44100 samples per output frame — so the ramp's
    // step grows by exactly that ratio, which is what preserves both pitch and
    // duration. At the buggy 1× the step would be 1/100_000.
    const step = (SAMPLE_RATE / 44_100) / 100_000;
    for (let i = 1; i < out.length; i++) {
      expect(out[i]! - out[i - 1]!).toBeCloseTo(step, 8);
    }
    expect(p.underruns).toBe(0);
  });

  it('plays a fixed amount of content in the wall-clock time it represents', () => {
    // 80 ms of 48 kHz content on a 44.1 kHz context must last 80 ms — i.e.
    // 3528 output frames (27.5 quanta), not the 3840 (30 quanta) that playing
    // it at 1× would stretch it to.
    const p = instantiate(44_100);
    for (let c = 0; c < 4; c++) feed(p, c * FRAME_COUNT, FRAME_COUNT, c * 20_000);
    drain(p, 27); // 3456 frames — just short of the content
    expect(p.underruns).toBe(0);
    drain(p, 2); // past 3528: dry
    expect(p.underruns).toBeGreaterThan(0);
  });

  // Findings 7 and 8 were both a *shadow* of this queue diverging from it. The
  // worklet is the only place the truth exists, so it reports it — in content
  // ms (each chunk's own frameCount ÷ its own sampleRate), which is the unit
  // the jitter buffer thinks in and is independent of the context rate.
  it('reports its own queue depth in content ms, whatever the context rate', () => {
    for (const contextRate of [SAMPLE_RATE, 44_100]) {
      const p = instantiate(contextRate);
      for (let c = 0; c < 5; c++) feed(p, c * FRAME_COUNT, FRAME_COUNT, c * 20_000);
      // Nothing drained yet: 5 × 20 ms.
      expect(report(p).queuedMs).toBeCloseTo(100 - (QUANTUM / contextRate) * 1000, 3);
      expect(report(p).receivedMs).toBeCloseTo(100, 5);
    }
  });

  it('reports a partially consumed head chunk as its remainder', () => {
    const p = instantiate();
    feed(p, 0, FRAME_COUNT, 0); // 20 ms
    drain(p, 3); // 384 of 960 frames = 8 ms
    expect(report(p).queuedMs).toBeCloseTo(20 - ((4 * QUANTUM) / SAMPLE_RATE) * 1000, 3);
  });

  it('keeps receivedMs cumulative across a flush so the sink can reconcile', () => {
    // The counter pair (worklet receivedMs, sink deliveredMs) is what lets the
    // sink add back chunks still in flight when the report was generated.
    // Resetting either on flush would race the other across the port.
    const p = instantiate();
    for (let c = 0; c < 3; c++) feed(p, c * FRAME_COUNT, FRAME_COUNT, c * 20_000);
    p.port.onmessage!({ data: { type: 'flush' } });
    expect(report(p).queuedMs).toBe(0);
    expect(report(p).receivedMs).toBeCloseTo(60, 5);
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

// The sample-rate half of docs/20 field finding 8. The sink asks for a context
// at the decoder's rate, but the browser is free to refuse the option outright
// (a throw) or to hand back a context at the device rate. Neither may end with
// audio accounted in one rate and played in another — the worklet resamples,
// and the sink's own accounting is in content ms, so the context rate only
// ever needs to be *known*, never assumed.
describe('AudioSink context sample rate', () => {
  function stubRateAware(opts: { refuseOption?: boolean; deviceRate?: number }) {
    const created: { sampleRate: number | undefined }[] = [];
    const node = {
      port: { postMessage: vi.fn(), onmessage: null as ((e: { data: unknown }) => void) | null },
      connect: vi.fn(),
      disconnect: vi.fn(),
    };
    vi.stubGlobal(
      'AudioContext',
      function (options?: { sampleRate?: number }) {
        if (opts.refuseOption && options?.sampleRate !== undefined) {
          throw new DOMException('sample rate not supported', 'NotSupportedError');
        }
        created.push({ sampleRate: options?.sampleRate });
        return {
          state: 'running' as AudioContextState,
          sampleRate: options?.sampleRate ?? opts.deviceRate ?? 44_100,
          destination: {},
          audioWorklet: { addModule: vi.fn(() => Promise.resolve()) },
          createGain: vi.fn(() => ({ gain: { value: 1 }, connect: vi.fn(), disconnect: vi.fn() })),
          resume: vi.fn(() => Promise.resolve()),
          close: vi.fn(() => Promise.resolve()),
        };
      } as unknown as typeof AudioContext,
    );
    vi.stubGlobal('AudioWorkletNode', function () {
      return node;
    } as unknown as typeof AudioWorkletNode);
    vi.stubGlobal('Blob', class {});
    vi.stubGlobal('URL', { createObjectURL: vi.fn(() => 'blob:stub'), revokeObjectURL: vi.fn() });
    return { created, node };
  }

  it('falls back to the device context when the requested rate is refused', async () => {
    const { created } = stubRateAware({ refuseOption: true, deviceRate: 44_100 });
    const sink = new AudioSink();
    // Pre-fix this rejected and the whole stream went video-only.
    await sink.start(SAMPLE_RATE);
    expect(created).toHaveLength(1);
    expect(created[0].sampleRate).toBeUndefined();
    expect(sink.getStats().contextSampleRate).toBe(44_100);
    sink.dispose();
  });

  it('reports the rate the context actually runs at, not the one requested', async () => {
    // A context that silently ignores the option: the number an operator needs
    // when audio sounds slow, and the one that used to be assumed.
    stubRateAware({ deviceRate: 44_100 });
    const sink = new AudioSink();
    await sink.start(SAMPLE_RATE);
    expect(sink.getStats().contextSampleRate).toBe(SAMPLE_RATE);
    sink.dispose();
  });
});

// The report is generated inside the worklet and read a message-hop later, so
// chunks delivered in between are in the worklet's queue but not in the depth
// it reported. Cumulative counters on both sides make the reconciliation exact
// instead of merely close.
describe('AudioSink depth reconciliation', () => {
  it('adds back chunks delivered after the report was generated', async () => {
    const node = {
      port: { postMessage: vi.fn(), onmessage: null as ((e: { data: unknown }) => void) | null },
      connect: vi.fn(),
      disconnect: vi.fn(),
    };
    vi.stubGlobal('AudioContext', function () {
      return {
        state: 'running' as AudioContextState,
        sampleRate: SAMPLE_RATE,
        destination: {},
        audioWorklet: { addModule: vi.fn(() => Promise.resolve()) },
        createGain: vi.fn(() => ({ gain: { value: 1 }, connect: vi.fn(), disconnect: vi.fn() })),
        resume: vi.fn(() => Promise.resolve()),
        close: vi.fn(() => Promise.resolve()),
      };
    } as unknown as typeof AudioContext);
    vi.stubGlobal('AudioWorkletNode', function () {
      return node;
    } as unknown as typeof AudioWorkletNode);
    vi.stubGlobal('Blob', class {});
    vi.stubGlobal('URL', { createObjectURL: vi.fn(() => 'blob:stub'), revokeObjectURL: vi.fn() });

    const sink = new AudioSink();
    await sink.start(SAMPLE_RATE);
    // 5 × 20 ms delivered (3 prime the cushion, 2 pass through).
    for (let i = 0; i < 5; i++) sink.push(chunk(i * 20_000));

    // The worklet reports having seen only the first 3 (60 ms), holding 50 ms.
    // The other 40 ms were in flight and are still real depth.
    node.port.onmessage!({
      data: { type: 'playhead', playheadUs: 0, queuedMs: 50, receivedMs: 60, underruns: 0, contextTime: 0 },
    });
    expect(sink.getStats().bufferedMs).toBeCloseTo(90, 5);
    sink.dispose();
  });
});

// docs/20 field finding 11 (2026-07-23, measured against the homelab). Toggling
// Paced playback moves the video presentation schedule — capToRenderMs went
// 204 → 65 ms — but audio kept the alignment it chose at start, so `avSkewMs`
// jumped 142 → 333 ms and stayed there. Unlike finding 10 there is no flush to
// get wrong: a playout toggle is a worker command with no reconnect, and after
// release the worklet runs at 1×, so no amount of buffering can move a sample.
// Re-anchoring is the only lever, and it costs a short silence — which is the
// right trade on a deliberate user action that already changes playback.
describe('AudioSink video-schedule re-anchor', () => {
  it('re-anchors when the video presentation schedule shifts materially', async () => {
    stubWebAudio(() => {});
    const sink = new AudioSink({}, undefined, { now: () => 1000 });
    await sink.start(SAMPLE_RATE);

    // Paced: a frame is presented 200 ms after the timestamp it carries.
    sink.setVideoSchedule((ts) => ts / 1000 + 200);
    for (let i = 0; i < 20; i++) sink.push(chunk(i * 20_000));
    const before = sink.getStats().resets;

    // The viewer turns Paced playback off: video snaps to the live edge.
    sink.setVideoSchedule((ts) => ts / 1000);

    expect(sink.getStats().resets).toBe(before + 1);
    sink.dispose();
  });

  it('ignores the adaptive controller slewing the schedule a few ms per tick', async () => {
    stubWebAudio(() => {});
    const sink = new AudioSink({}, undefined, { now: () => 1000 });
    await sink.start(SAMPLE_RATE);

    sink.setVideoSchedule((ts) => ts / 1000 + 200);
    for (let i = 0; i < 20; i++) sink.push(chunk(i * 20_000));
    const before = sink.getStats().resets;

    // PlayoutController slews at most 50 ms/s, so a ~500 ms stats tick can
    // legitimately move the schedule ~25 ms. Re-anchoring on that would shred
    // audio on a perfectly healthy adaptive session.
    for (const offset of [215, 190, 210, 225, 205]) {
      sink.setVideoSchedule((ts) => ts / 1000 + offset);
    }

    expect(sink.getStats().resets).toBe(before);
    sink.dispose();
  });
});

// docs/20 field finding 13 (2026-07-26): the sink is the only place that knows
// where the speaker is. The worklet reports the sample it is WRITING into the
// output buffer; that sample reaches the listener `outputLatency` later. The
// alignment release already compensates (it hands the cushion over early by
// exactly that much) — but av-sync's drift trim then servos the *measured*
// skew to zero, and if that measurement is taken at the write position, zero
// means "audio heard outputLatency late". The trim therefore spends the
// session undoing the alignment: a simulation of the real loop settles at
// `outputLatency - RATE_TRIM_DEADBAND_MS` of lateness (100 ms on a 120 ms
// device, 280 ms on a 300 ms one) while the overlay reads ~0.
describe('AudioSink playhead reference point', () => {
  function stubWithLatency(latency: { outputLatency?: number; baseLatency?: number } & {
    getOutputTimestamp?: () => { contextTime: number; performanceTime: number };
  }) {
    const node = {
      port: {
        postMessage: vi.fn(),
        onmessage: null as ((e: { data: unknown }) => void) | null,
      },
      connect: vi.fn(),
      disconnect: vi.fn(),
    };
    const ctx = {
      state: 'running' as AudioContextState,
      sampleRate: SAMPLE_RATE,
      destination: {},
      audioWorklet: { addModule: vi.fn(() => Promise.resolve()) },
      createGain: vi.fn(() => ({ gain: { value: 1 }, connect: vi.fn(), disconnect: vi.fn() })),
      resume: vi.fn(() => Promise.resolve()),
      close: vi.fn(() => Promise.resolve()),
      ...latency,
    };
    vi.stubGlobal('AudioContext', function () {
      return ctx;
    } as unknown as typeof AudioContext);
    vi.stubGlobal('AudioWorkletNode', function () {
      return node;
    } as unknown as typeof AudioWorkletNode);
    vi.stubGlobal('Blob', class {});
    vi.stubGlobal('URL', { createObjectURL: vi.fn(() => 'blob:stub'), revokeObjectURL: vi.fn() });
    return { ctx, node };
  }

  function report(
    node: { port: { onmessage: ((e: { data: unknown }) => void) | null } },
    playheadUs: number,
    contextTime: number,
  ) {
    node.port.onmessage!({
      data: { type: 'playhead', playheadUs, queuedMs: 200, receivedMs: 0, underruns: 0, contextTime },
    });
  }

  it('reports the sample at the listener, not the one being written', async () => {
    const { node } = stubWithLatency({ outputLatency: 0.12 });
    const reports: { heardUs: number | null; atEpochMs: number }[] = [];
    const sink = new AudioSink({ onPlayhead: (r) => reports.push(r) });
    await sink.start(SAMPLE_RATE);

    report(node, 1_000_000, 5);
    expect(reports).toHaveLength(1);
    // Being written: 1.000 s. Being heard: 120 ms of device buffer earlier.
    expect(reports[0].heardUs).toBeCloseTo(880_000, -1);
    // And the number is on the overlay, because it is the one that decides how
    // far the trim will walk audio if this ever regresses.
    expect(sink.getStats().outputLatencyMs).toBeCloseTo(120, 5);
    sink.dispose();
  });

  it('prefers getOutputTimestamp, which also absorbs the report hop', async () => {
    // The exact answer: contextTime is the position actually audible and
    // performanceTime is when it is audible, so the pair needs no latency
    // estimate and self-corrects for however long the message sat in the
    // main thread's task queue.
    const { node } = stubWithLatency({
      outputLatency: 0.12,
      // Report generated at contextTime 5; by the time we handle it the
      // device is playing what was written at 4.9 (a 100 ms true latency).
      getOutputTimestamp: () => ({ contextTime: 4.9, performanceTime: 7000 }),
    });
    const reports: { heardUs: number | null; atEpochMs: number }[] = [];
    const sink = new AudioSink({ onPlayhead: (r) => reports.push(r) });
    await sink.start(SAMPLE_RATE);

    report(node, 1_000_000, 5);
    expect(reports[0].heardUs).toBeCloseTo(900_000, -1);
    // Anchored at the moment that sample is at the listener, not at receipt.
    expect(reports[0].atEpochMs).toBeCloseTo(performance.timeOrigin + 7000, 5);
    sink.dispose();
  });

  it('falls back to baseLatency, then to a nominal value', async () => {
    const first = stubWithLatency({ baseLatency: 0.05 });
    const a = new AudioSink({}, undefined, {});
    await a.start(SAMPLE_RATE);
    expect(a.getStats().outputLatencyMs).toBeCloseTo(50, 5);
    a.dispose();
    void first;

    stubWithLatency({});
    const b = new AudioSink({}, undefined, {});
    await b.start(SAMPLE_RATE);
    expect(b.getStats().outputLatencyMs).toBeCloseTo(20, 5);
    b.dispose();
  });

  it('ignores an implausible getOutputTimestamp instead of snapping to it', async () => {
    // A stale or nonsensical reading is not a measurement, and av-sync would
    // *snap* to a jump this large rather than slew it — one bad report would
    // poison the mapping. Fall back to the latency estimate.
    const { node } = stubWithLatency({
      outputLatency: 0.12,
      getOutputTimestamp: () => ({ contextTime: 1, performanceTime: 7000 }),
    });
    const reports: { heardUs: number | null }[] = [];
    const sink = new AudioSink({ onPlayhead: (r) => reports.push(r) });
    await sink.start(SAMPLE_RATE);

    report(node, 1_000_000, 5); // a 4 s "correction" — not a device buffer
    expect(reports[0].heardUs).toBeCloseTo(880_000, -1);
    sink.dispose();
  });

  // The bug was in the COMPOSITION, not in either half: the alignment release
  // compensated for output latency and the trim then removed the compensation,
  // each behaving exactly as written. So close the loop across the real sink,
  // the real av-sync mapping and the real controller, and assert the thing
  // neither half can assert alone — that a stream synced at the speaker stays
  // synced.
  it('leaves a speaker-aligned stream alone across the whole loop', async () => {
    const LATENCY_S = 0.12;
    let contextTime = 5;
    const { node } = stubWithLatency({
      outputLatency: LATENCY_S,
      // The device is always playing what was written outputLatency ago.
      getOutputTimestamp: () => ({
        contextTime: contextTime - LATENCY_S,
        performanceTime: contextTime * 1000,
      }),
    });
    resetAvSync();
    const controller = new AudioRateController();
    // One clock for the whole loop: the sink's own `now` must advance with the
    // context, or the mapping anchors every report at the same instant and the
    // loop is self-consistent for the wrong reason (this test passed under a
    // mutation until the injection was added).
    const sink = new AudioSink({ onPlayhead: (r) => notePlayhead(r, contextTime * 1000) }, undefined, {
      now: () => contextTime * 1000,
    });
    await sink.start(SAMPLE_RATE);

    // Perfect sync at the speaker: the sample heard at wall time T carries
    // broadcaster timestamp T, so the one being WRITTEN is T + outputLatency.
    let errorMs = 0; // positive = audio heard behind the picture
    let rate = 1;
    for (let step = 0; step < 1200; step++) {
      contextTime += 0.25;
      const nowMs = contextTime * 1000;
      const writtenUs = (nowMs - errorMs + LATENCY_S * 1000) * 1000;
      node.port.onmessage!({
        data: {
          type: 'playhead',
          playheadUs: writtenUs,
          queuedMs: 200,
          receivedMs: 0,
          underruns: 0,
          contextTime,
        },
      });
      rate = controller.update(observeVideoPresented(nowMs * 1000, nowMs), nowMs);
      errorMs += (1 - rate) * 250;
    }
    // Five minutes of loop. Pre-fix this reads ~100 ms (outputLatency minus
    // the trim's deadband) with avSkewMs sitting at a clean zero throughout.
    expect(Math.abs(errorMs)).toBeLessThan(1);
    sink.dispose();
    resetAvSync();
  });

  it('reports no heard sample before anything has played', async () => {
    const { node } = stubWithLatency({ outputLatency: 0.12 });
    const reports: { heardUs: number | null }[] = [];
    const sink = new AudioSink({ onPlayhead: (r) => reports.push(r) });
    await sink.start(SAMPLE_RATE);

    node.port.onmessage!({
      data: { type: 'playhead', playheadUs: null, queuedMs: 0, receivedMs: 0, underruns: 0, contextTime: 1 },
    });
    expect(reports[0].heardUs).toBeNull();
    sink.dispose();
  });
});

// R42 (docs/44 §4.7): an injected shared output. The sink must neither open
// nor close a context it was handed, must route through the given
// destination, and must let the owner register the worklet exactly once.
describe('AudioSink with an injected output (R42 room mixing)', () => {
  it('uses the shared context and destination, registers the worklet via the owner, and never closes it', async () => {
    const posted: unknown[] = [];
    stubWebAudio((msg) => posted.push(msg));
    const context = new (globalThis as { AudioContext: new () => AudioContext }).AudioContext();
    const destination = { connected: 0 } as unknown as AudioNode;
    const ensureWorklet = vi.fn(() => Promise.resolve());
    const created = vi.spyOn(globalThis as { AudioContext: new () => AudioContext }, 'AudioContext');

    const sinkA = new AudioSink({}, undefined, { output: { context, destination, ensureWorklet } });
    const sinkB = new AudioSink({}, undefined, { output: { context, destination, ensureWorklet } });
    await sinkA.start(SAMPLE_RATE);
    await sinkB.start(SAMPLE_RATE);

    expect(created).not.toHaveBeenCalled();
    expect(ensureWorklet).toHaveBeenCalledTimes(2);
    expect((context as unknown as { audioWorklet: { addModule: unknown } }).audioWorklet.addModule).not.toHaveBeenCalled();
    // Both gains feed the shared destination, not the context's own.
    const gains = (context as unknown as { createGain: ReturnType<typeof vi.fn> }).createGain.mock.results;
    expect(gains).toHaveLength(2);
    for (const g of gains) expect((g.value as { connect: ReturnType<typeof vi.fn> }).connect).toHaveBeenCalledWith(destination);

    for (let i = 0; i < 3; i++) sinkA.push(chunk(i * 20_000));
    expect(posted.length).toBeGreaterThan(0);
    sinkA.dispose();
    sinkB.dispose();
    expect((context as unknown as { close: ReturnType<typeof vi.fn> }).close).not.toHaveBeenCalled();
  });
});
