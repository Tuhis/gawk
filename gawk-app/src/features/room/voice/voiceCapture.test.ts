// The microphone → Opus path (docs/80 D21; docs/81 NS2) against fakes: the
// 10 ms / 20 ms encoder probe, the AudioWorklet path on every browser, the
// gate deciding what is encoded, the spurt end after the encoder drains, the
// noise modes (constraints, the in-worklet switch, the re-open and when it
// may happen), every RNNoise fallback, and every failure releasing the track.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  VOICE_BITRATE_BPS,
  VOICE_FALLBACK_FRAME_US,
  VOICE_FRAME_US,
  VoiceCapture,
  chooseFrameDuration,
  queryMicPermission,
  seqUnitsFor,
  voiceAudioConstraints,
  voiceCaptureSupported,
  voiceEncoderConfig,
  type NoiseStatus,
  type VoiceAudioData,
  type VoiceCaptureDeps,
  type VoiceEncoderConfig,
} from './voiceCapture';
import { VoiceError } from './voiceSession';
import { CAPTURE_PROCESSOR_NAME, type CaptureWorkletCommand, type CaptureWorkletEvent } from './voiceFraming';
import type { NoiseMode } from './voicePrefs';

class FakeTrack {
  stopped = 0;
  listeners: Record<string, (() => void)[]> = {};
  readonly id: number;
  constructor(id: number) {
    this.id = id;
  }
  stop() {
    this.stopped++;
    log.push(`stop track ${this.id}`);
  }
  addEventListener(type: string, fn: () => void) {
    (this.listeners[type] ??= []).push(fn);
  }
  removeEventListener(type: string, fn: () => void) {
    this.listeners[type] = (this.listeners[type] ?? []).filter((f) => f !== fn);
  }
  fire(type: string) {
    for (const fn of this.listeners[type] ?? []) fn();
  }
}

class FakeData implements VoiceAudioData {
  closed = 0;
  readonly numberOfChannels = 1;
  readonly numberOfFrames = 480;
  readonly level: number;
  readonly timestamp: number;
  readonly sampleRate: number;
  constructor(level: number, timestamp: number, sampleRate: number) {
    this.level = level;
    this.timestamp = timestamp;
    this.sampleRate = sampleRate;
  }
  close() {
    this.closed++;
  }
}

interface FakeNode {
  options: { processorOptions: { denoise: boolean; module: WebAssembly.Module | null } };
  port: { onmessage: ((e: { data: CaptureWorkletEvent }) => void) | null; posted: CaptureWorkletCommand[]; postMessage(m: CaptureWorkletCommand): void };
  onprocessorerror: (() => void) | null;
  connected: unknown[];
  disconnected: number;
}

let log: string[];
let tracks: FakeTrack[];
let gumCalls: MediaTrackConstraints[];
let gumError: unknown;
let supported: number[];
let granted: boolean;
let simd: boolean;
let suppressible: boolean;
let moduleLoad: { resolve: (m: WebAssembly.Module) => void; reject: (e: Error) => void } | null;
let loads: number;
let encoders: {
  config: VoiceEncoderConfig;
  encoded: FakeData[];
  flushes: number;
  closed: number;
  output: (d: Uint8Array, dur: number | null) => void;
  error: (e: Error) => void;
}[];
let workletNodes: FakeNode[];
let sources: { stream: MediaStream; connected: unknown[]; disconnected: number; disconnectedFrom: unknown[] }[];
let ctxRate: number;

const MODULE = { fake: 'module' } as unknown as WebAssembly.Module;

function deps(over: Partial<VoiceCaptureDeps> = {}): VoiceCaptureDeps {
  return {
    getUserMedia: async (c) => {
      gumCalls.push(c.audio as MediaTrackConstraints);
      log.push('getUserMedia');
      if (gumError) throw gumError;
      const t = new FakeTrack(tracks.length);
      tracks.push(t);
      return { getAudioTracks: () => [t], getTracks: () => [t] } as unknown as MediaStream;
    },
    isConfigSupported: async (c) => ({ supported: supported.includes(c.opus.frameDuration) }),
    createEncoder: (config, cb) => {
      const e = { config, encoded: [] as FakeData[], flushes: 0, closed: 0, output: cb.output, error: cb.error };
      encoders.push(e);
      return {
        encode: (d) => void e.encoded.push(d as FakeData),
        flush: async () => void e.flushes++,
        close: () => void e.closed++,
      };
    },
    createAudioData: (init) => new FakeData((init.data as Float32Array)[0] ?? 0, init.timestamp, init.sampleRate),
    wasmSimd: () => simd,
    loadRnnoise: () => {
      loads++;
      log.push('loadRnnoise');
      return new Promise((resolve, reject) => {
        moduleLoad = { resolve, reject };
      });
    },
    micPermissionGranted: async () => granted,
    suppressionConstraint: () => suppressible,
    ...over,
  };
}

function callbacks(gateOpen: (rms: number, prob: number | null) => boolean = (rms) => rms > 0.05) {
  const cb = {
    packets: [] as [number, number][],
    ends: 0,
    errors: [] as VoiceError[],
    gated: [] as [number, number | null][],
    gatedAt: [] as number[],
    noise: [] as NoiseStatus[],
    shouldSend: (rms: number, prob: number | null, nowMs: number) => {
      cb.gated.push([rms, prob]);
      cb.gatedAt.push(nowMs);
      return gateOpen(rms, prob);
    },
    onPacket: (p: Uint8Array, units: number) => cb.packets.push([p.length, units]),
    onSpurtEnd: () => void cb.ends++,
    onError: (e: VoiceError) => void cb.errors.push(e),
    onNoise: (n: NoiseStatus) => void cb.noise.push(n),
  };
  return cb;
}

function fakeContext() {
  const node = (kind: string) => {
    const n = {
      kind,
      connected: [] as unknown[],
      disconnected: 0,
      disconnectedFrom: [] as unknown[],
      connect(to: unknown) {
        n.connected.push(to);
      },
      disconnect(from?: unknown) {
        if (from === undefined) n.disconnected++;
        else n.disconnectedFrom.push(from);
      },
    };
    return n;
  };
  return {
    sampleRate: ctxRate,
    destination: { kind: 'destination' },
    audioWorklet: { addModule: vi.fn(async () => {}) },
    createMediaStreamSource: (stream: MediaStream) => {
      const s = Object.assign(node('source'), { stream });
      sources.push(s);
      return s;
    },
    createGain: () => Object.assign(node('gain'), { gain: { value: 1 } }),
  } as unknown as AudioContext;
}

const flush = async () => {
  for (let i = 0; i < 10; i++) await new Promise((r) => setTimeout(r, 0));
};

async function started(noise: NoiseMode, cb = callbacks(), over: Partial<VoiceCaptureDeps> = {}) {
  const ctx = fakeContext();
  const cap = new VoiceCapture(cb, deps(over), noise);
  await cap.start(() => ({ context: ctx }));
  return { cap, cb, ctx, node: () => workletNodes[workletNodes.length - 1] };
}

function block(n: FakeNode, level: number, prob = -1) {
  n.port.onmessage!({ data: { t: 'block', seq: 0, pcm: new Float32Array(480).fill(level), prob } });
}

const last = (cb: ReturnType<typeof callbacks>) => cb.noise[cb.noise.length - 1];

beforeEach(() => {
  log = [];
  tracks = [];
  gumCalls = [];
  gumError = null;
  supported = [VOICE_FRAME_US, VOICE_FALLBACK_FRAME_US];
  granted = true;
  simd = true;
  suppressible = true;
  moduleLoad = null;
  loads = 0;
  encoders = [];
  workletNodes = [];
  sources = [];
  ctxRate = 48000;
  vi.stubGlobal(
    'AudioWorkletNode',
    class implements FakeNode {
      options: FakeNode['options'];
      port: FakeNode['port'] = {
        onmessage: null,
        posted: [],
        postMessage(m: CaptureWorkletCommand) {
          this.posted.push(m);
        },
      };
      onprocessorerror: (() => void) | null = null;
      connected: unknown[] = [];
      disconnected = 0;
      constructor(_ctx: unknown, name: string, options: FakeNode['options']) {
        expect(name).toBe(CAPTURE_PROCESSOR_NAME);
        this.options = options;
        workletNodes.push(this);
      }
      connect(to: unknown) {
        this.connected.push(to);
      }
      disconnect() {
        this.disconnected++;
      }
    },
  );
  vi.stubGlobal('URL', { createObjectURL: () => 'blob:x', revokeObjectURL: () => {} });
});
afterEach(() => vi.unstubAllGlobals());

describe('encoder config', () => {
  it('is Opus at 40 kbit/s, DTX off, at the asked frame duration', () => {
    expect(voiceEncoderConfig(48000, 1, VOICE_FRAME_US)).toEqual({
      codec: 'opus',
      sampleRate: 48000,
      numberOfChannels: 1,
      bitrate: VOICE_BITRATE_BPS,
      opus: { frameDuration: 10_000, complexity: 8, usedtx: false },
    });
  });

  it('probes 10 ms first, falls back to 20 ms, and gives up when neither is supported', async () => {
    const probe = (ok: number[]) => async (c: VoiceEncoderConfig) => {
      if (c.opus.frameDuration === 99) throw new Error('unparseable');
      return { supported: ok.includes(c.opus.frameDuration) };
    };
    expect(await chooseFrameDuration(probe([10_000, 20_000]))).toBe(10_000);
    expect(await chooseFrameDuration(probe([20_000]))).toBe(20_000);
    expect(await chooseFrameDuration(probe([]))).toBeNull();
    expect(await chooseFrameDuration(async () => Promise.reject(new Error('x')))).toBeNull();
  });

  it('counts seq units from the packet duration', () => {
    expect(seqUnitsFor(10_000, 10_000)).toBe(1);
    expect(seqUnitsFor(20_000, 10_000)).toBe(2);
    expect(seqUnitsFor(null, 20_000)).toBe(2);
    expect(seqUnitsFor(0, 10_000)).toBe(1);
    expect(seqUnitsFor(1, 10_000)).toBe(1);
  });
});

describe('constraints (docs/81 D11, docs/82 D5)', () => {
  it('echo cancellation always; the browser’s suppression and auto gain only for Browser', () => {
    expect(voiceAudioConstraints('rnnoise')).toEqual({
      echoCancellation: true,
      noiseSuppression: false,
      autoGainControl: false,
      channelCount: 1,
    });
    expect(voiceAudioConstraints('off')).toEqual(voiceAudioConstraints('rnnoise'));
    expect(voiceAudioConstraints('browser')).toEqual({
      echoCancellation: true,
      noiseSuppression: true,
      autoGainControl: true,
      channelCount: 1,
    });
  });
});

describe('VoiceCapture, the gain after the gate (docs/82 D6)', () => {
  it('a denoised block is gated as captured, then gained', async () => {
    const cb = { ...callbacks(), gainDb: () => 20 * Math.log10(2) };
    const { cap, node } = await started('off', cb);
    block(node(), 0.1, 0.97);
    expect(cb.gated.map(([rms]) => rms)).toEqual([expect.closeTo(0.1, 6)]);
    expect(encoders[0].encoded.map((d) => d.level)).toEqual([expect.closeTo(0.2, 6)]);
    cap.stop();
  });

  it('a block that was not denoised is gained before the level gate (review round 1: Off lost auto gain)', async () => {
    const cb = { ...callbacks(), gainDb: () => 20 * Math.log10(4) };
    const { cap, node } = await started('off', cb);
    // 0.02 alone is under the fake gate's 0.05; gained ×4 it opens it.
    block(node(), 0.02);
    expect(cb.gated.map(([rms]) => rms)).toEqual([expect.closeTo(0.08, 6)]);
    expect(encoders[0].encoded.map((d) => d.level)).toEqual([expect.closeTo(0.08, 6)]);
    cap.stop();
  });

  it('the gain is capped so the block’s peak stays under full scale (review round 1)', async () => {
    const cb = { ...callbacks(), gainDb: () => 20 * Math.log10(4) };
    const { cap, node } = await started('off', cb);
    block(node(), 0.5, 0.97);
    expect(encoders[0].encoded[0].level).toBeCloseTo(0.98, 6);
    cap.stop();
  });

  it('Browser mode never applies it: the browser’s auto gain is on there (review)', async () => {
    const cb = { ...callbacks(), gainDb: () => 18 };
    const { cap, node } = await started('browser', cb);
    block(node(), 0.1);
    expect(encoders[0].encoded[0].level).toBeCloseTo(0.1, 6);
    cap.stop();
  });

  it('a loud block after a quiet one is not ramped down from the quiet one’s gain (review round 2)', async () => {
    const cb = { ...callbacks(), gainDb: () => 20 * Math.log10(4) };
    const { cap, node } = await started('off', cb);
    block(node(), 0.01); // ×4 → 0.04: under the fake gate, but it sets the gain
    block(node(), 0.5); // capped at 0.98 / 0.5 from its first sample
    expect(encoders[0].encoded.map((d) => d.level)).toEqual([expect.closeTo(0.98, 6)]);
    cap.stop();
  });

  it('after a capped loud block the gain rises at most 0.5 dB a block (review round 3)', async () => {
    const cb = { ...callbacks(), gainDb: () => 20 * Math.log10(4) };
    const { cap, node } = await started('off', cb);
    block(node(), 0.5, 0.97); // capped at ×1.96
    block(node(), 0.06, 0.97); // wants ×4: rises to ×1.96 · 10^(0.5/20)
    block(node(), 0.06, 0.97); // starts where the last one ended
    expect(encoders[0].encoded[2].level).toBeCloseTo(0.06 * 1.96 * 10 ** (0.5 / 20), 6);
    cap.stop();
  });

  it('no gain callback, or 0 dB, leaves the block untouched', async () => {
    const { cap, node } = await started('off');
    block(node(), 0.3);
    expect(encoders[0].encoded[0].level).toBeCloseTo(0.3, 6);
    cap.stop();
  });
});

describe('VoiceCapture, the worklet path', () => {
  it('encodes only what the gate passes, as 48 kHz AudioData on a 10 ms clock, muted to the speakers', async () => {
    const { cap, cb, node } = await started('off');
    const n = node();
    expect(n.options.processorOptions).toEqual({ denoise: false, module: null });
    expect(n.connected[0]).toMatchObject({ kind: 'gain', gain: { value: 0 } });
    block(n, 0);
    block(n, 0.5);
    block(n, 0.5, 0.97);
    n.port.onmessage!({ data: null as never });
    expect(cb.gated.map(([, p]) => p)).toEqual([null, null, 0.97]);
    expect(encoders).toHaveLength(1);
    expect(encoders[0].config.sampleRate).toBe(48000);
    expect(encoders[0].config.opus.frameDuration).toBe(VOICE_FRAME_US);
    expect(encoders[0].encoded.map((d) => d.timestamp)).toEqual([10_000, 20_000]);
    expect(encoders[0].encoded.every((d) => d.closed === 1)).toBe(true);
    encoders[0].output(new Uint8Array(50), 10_000);
    encoders[0].output(new Uint8Array(0), 10_000); // empty: never sent
    expect(cb.packets).toEqual([[50, 1]]);
    // The spurt ends: the encoder drains, THEN the terminator is due.
    block(n, 0);
    await flush();
    expect(encoders[0].flushes).toBe(1);
    expect(cb.ends).toBe(1);
    cap.stop();
    expect(tracks[0].stopped).toBe(1);
    expect(encoders[0].closed).toBe(1);
    expect(n.port.posted).toContainEqual({ t: 'close' });
    expect(sources[0].disconnected).toBe(1);
    expect(n.port.onmessage).toBeNull();
  });

  // Mumble's client resets its encoder at the start of every transmission
  // and Mumble listeners decode each one on fresh state (docs/80 §12.8):
  // a spurt encoded on the last spurt's state reaches them garbled.
  it('encodes every spurt on a fresh encoder, after the last one drained and its terminator went', async () => {
    const { cap, cb, node } = await started('off');
    const n = node();
    block(n, 0.5);
    block(n, 0);
    // The next spurt starts before the drain is done: held, not encoded on
    // the old encoder (it would carry its state) nor on a new one yet (its
    // packets could overtake the terminator).
    block(n, 0.5);
    expect(encoders).toHaveLength(1);
    expect(encoders[0].encoded).toHaveLength(1);
    await flush();
    expect(cb.ends).toBe(1);
    expect(encoders[0].closed).toBe(1);
    expect(encoders).toHaveLength(2);
    expect(encoders[1].encoded.map((d) => d.timestamp)).toEqual([20_000]);
    cap.stop();
    expect(encoders[1].closed).toBe(1);
  });

  it('uses 20 ms frames where 10 ms is refused', async () => {
    supported = [VOICE_FALLBACK_FRAME_US];
    const { cap, cb, node } = await started('off');
    block(node(), 0.5);
    expect(encoders[0].config.opus.frameDuration).toBe(VOICE_FALLBACK_FRAME_US);
    encoders[0].output(new Uint8Array(60), null);
    expect(cb.packets).toEqual([[60, 2]]);
    cap.stop();
  });

  it('a block the gate refuses is never built into AudioData, and a bad AudioData fails', async () => {
    const create = vi.fn(() => {
      throw new Error('bad init');
    });
    const { cb, node } = await started('off', callbacks(), { createAudioData: create });
    block(node(), 0);
    expect(create).not.toHaveBeenCalled();
    block(node(), 0.5);
    expect(cb.errors[0].phase).toBe('capture');
    expect(tracks[0].stopped).toBe(1);
  });

  it('an encoder that cannot be built fails the capture and releases the track', async () => {
    const { cb, node } = await started('off', callbacks(), {
      createEncoder: () => {
        throw new Error('no encoder');
      },
    });
    block(node(), 0.5);
    expect(cb.errors[0].message).toMatch(/no encoder/);
    expect(tracks[0].stopped).toBe(1);
  });
});

describe('VoiceCapture, RNNoise (docs/81 D4, D13)', () => {
  it('starts the download before the prompt and runs the worklet as RNNoise, loading, until the module is in', async () => {
    const { cb, node } = await started('rnnoise');
    expect(log.slice(0, 2)).toEqual(['loadRnnoise', 'getUserMedia']);
    expect(gumCalls[0].noiseSuppression).toBe(false);
    const n = node();
    expect(n.options.processorOptions).toEqual({ denoise: true, module: null });
    expect(last(cb)).toMatchObject({ requested: 'rnnoise', running: 'rnnoise', loading: true, fallback: null });
    // Meanwhile the level gate decides (no probability).
    block(n, 0.5);
    expect(cb.gated[0][1]).toBeNull();
    moduleLoad!.resolve(MODULE);
    await flush();
    // The module rides a new node's processorOptions, never the port (D5):
    // the new node takes the old one's place between source and sink.
    expect(workletNodes).toHaveLength(2);
    const fresh = node();
    expect(fresh.options.processorOptions).toEqual({ denoise: true, module: MODULE });
    expect(n.disconnected).toBe(1);
    // The source lets go of the old node too, or it keeps the old processor
    // (and its wasm memory) alive.
    expect(sources[0].disconnectedFrom).toEqual([n]);
    expect(n.port.onmessage).toBeNull();
    expect(n.port.posted).toContainEqual({ t: 'close' });
    expect(sources[0].connected).toContain(fresh);
    expect(fresh.connected[0]).toMatchObject({ kind: 'gain' });
    expect(workletNodes.every((w) => w.port.posted.every((m) => (m as { t: string }).t !== 'module'))).toBe(true);
    fresh.port.onmessage!({ data: { t: 'ready' } });
    expect(last(cb)).toMatchObject({ running: 'rnnoise', loading: false });
    // The block clock carries on across the swap; no re-open for any of it.
    block(fresh, 0.5);
    expect(encoders[0].encoded.map((d) => d.timestamp)).toEqual([0, 10_000]);
    expect(gumCalls).toHaveLength(1);
  });

  it('without wasm SIMD opens the microphone as Browser before any prompt, and never downloads', async () => {
    simd = false;
    const { cb } = await started('rnnoise');
    expect(loads).toBe(0);
    expect(gumCalls).toEqual([voiceAudioConstraints('browser')]);
    expect(last(cb)).toMatchObject({ requested: 'rnnoise', running: 'browser', fallback: expect.stringMatching(/SIMD/) });
  });

  it('Browser and Off never download the module', async () => {
    await started('browser');
    await started('off');
    expect(loads).toBe(0);
  });

  it('RNNoise ↔ Off switches inside the worklet and never re-opens', async () => {
    const { cap, cb, node } = await started('rnnoise');
    moduleLoad!.resolve(MODULE);
    await cap.setNoise('off');
    expect(node().port.posted).toContainEqual({ t: 'denoise', on: false });
    expect(last(cb)).toMatchObject({ requested: 'off', running: 'off', loading: false });
    await cap.setNoise('rnnoise');
    expect(node().port.posted.at(-1)).toEqual({ t: 'denoise', on: true });
    expect(last(cb)).toMatchObject({ running: 'rnnoise', loading: true });
    expect(gumCalls).toHaveLength(1);
    expect(loads).toBe(1);
  });

  it('Off → RNNoise downloads then, and a node carrying the module takes over when it lands', async () => {
    const { cap, node } = await started('off');
    await cap.setNoise('rnnoise');
    expect(loads).toBe(1);
    expect(workletNodes).toHaveLength(1);
    moduleLoad!.resolve(MODULE);
    await flush();
    expect(workletNodes).toHaveLength(2);
    expect(node().options.processorOptions).toEqual({ denoise: true, module: MODULE });
  });

  it('a module that landed while Off is used by the next switch to RNNoise, without another download', async () => {
    const { cap, node } = await started('rnnoise');
    moduleLoad!.resolve(MODULE);
    await flush();
    await cap.setNoise('off');
    expect(node().port.posted.at(-1)).toEqual({ t: 'denoise', on: false });
    const nodes = workletNodes.length;
    await cap.setNoise('rnnoise');
    // The current node already has the module: no new node, just denoise on.
    expect(workletNodes).toHaveLength(nodes);
    expect(node().port.posted.at(-1)).toEqual({ t: 'denoise', on: true });
    expect(loads).toBe(1);
  });
});

describe('VoiceCapture, the gate clock (review round 1)', () => {
  it('the gate reads audio time (10 ms a block), not when a block reached the main thread', async () => {
    // A burst: three blocks delivered at one main-thread instant.
    const { cb, node } = await started('off');
    block(node(), 0);
    block(node(), 0);
    block(node(), 0);
    expect(cb.gatedAt).toEqual([0, 10, 20]);
  });
});

describe('VoiceCapture, the re-open (docs/81 D11, §10)', () => {
  it('a module that lands while the re-open waits for getUserMedia: the new source feeds the live node', async () => {
    let release!: () => void;
    let calls = 0;
    const base = deps();
    const { cap, node } = await started('rnnoise', callbacks(), {
      getUserMedia: async (c) => {
        if (++calls === 2) await new Promise<void>((r) => (release = r));
        return base.getUserMedia(c);
      },
    });
    const p = cap.setNoise('browser');
    await flush();
    // Mid re-open, RNNoise still the running mode: the module's node swap.
    moduleLoad!.resolve(MODULE);
    await flush();
    release();
    await p;
    const live = node();
    expect(sources.at(-1)!.connected).toEqual([live]);
    expect(live.connected[0]).toMatchObject({ kind: 'gain' });
    expect(live.port.posted.at(-1)).toEqual({ t: 'denoise', on: false });
  });


  it('to Browser: stops the old track first, re-opens with suppression, keeps the worklet, encoder and clock', async () => {
    const { cap, cb, node } = await started('rnnoise');
    const n = node();
    block(n, 0.5);
    log = [];
    await cap.setNoise('browser');
    expect(log).toEqual(['stop track 0', 'getUserMedia']);
    expect(gumCalls[1]).toEqual(voiceAudioConstraints('browser'));
    expect(workletNodes).toHaveLength(1);
    expect(sources[0].disconnected).toBe(1);
    expect(sources[1].connected).toEqual([n]);
    expect(n.port.posted.at(-1)).toEqual({ t: 'denoise', on: false });
    expect(last(cb)).toMatchObject({ requested: 'browser', running: 'browser', pending: false });
    // The deliberately stopped track ending is not a lost microphone.
    tracks[0].fire('ended');
    expect(cb.errors).toHaveLength(0);
    block(n, 0.5);
    expect(encoders).toHaveLength(1);
    expect(encoders[0].encoded.map((d) => d.timestamp)).toEqual([0, 10_000]);
    // And back.
    await cap.setNoise('rnnoise');
    expect(gumCalls[2].noiseSuppression).toBe(false);
    expect(last(cb)).toMatchObject({ running: 'rnnoise' });
    // The new track ending is.
    tracks[2].fire('ended');
    expect(cb.errors[0].message).toMatch(/stopped/);
  });

  it('without a granted permission it never prompts: the choice waits for the next join', async () => {
    granted = false;
    const { cap, cb } = await started('rnnoise');
    await cap.setNoise('browser');
    expect(gumCalls).toHaveLength(1);
    expect(last(cb)).toMatchObject({ requested: 'browser', running: 'rnnoise', pending: true });
    // Choosing the running mode again clears it.
    await cap.setNoise('rnnoise');
    expect(last(cb)).toMatchObject({ requested: 'rnnoise', pending: false });
  });

  it('on WebKit (no suppression constraint) nothing re-opens and Off is Browser', async () => {
    suppressible = false;
    const { cap, cb } = await started('off');
    expect(last(cb)).toMatchObject({ requested: 'off', running: 'browser', offAvailable: false });
    await cap.setNoise('rnnoise');
    await cap.setNoise('browser');
    await cap.setNoise('rnnoise');
    expect(gumCalls).toHaveLength(1);
  });

  it('a re-open that fails ends the capture like a lost microphone', async () => {
    const { cap, cb } = await started('rnnoise');
    gumError = { name: 'NotReadableError' };
    await cap.setNoise('browser');
    expect(cb.errors).toHaveLength(1);
    expect(cb.errors[0].phase).toBe('capture');
    expect(cb.errors[0].message).toMatch(/re-opened/);
    expect(tracks[0].stopped).toBe(1);
    expect(encoders.every((e) => e.closed === 1)).toBe(true);
  });

  it('a stop during the re-open releases the new track', async () => {
    const { cap } = await started('rnnoise');
    const p = cap.setNoise('browser');
    await Promise.resolve();
    await Promise.resolve();
    await Promise.resolve();
    cap.stop();
    await p;
    expect(tracks.every((t) => t.stopped >= 1)).toBe(true);
  });
});

describe('VoiceCapture, fallbacks after the microphone opened (docs/81 D13)', () => {
  const failures: [string, (n: FakeNode) => void, RegExp][] = [
    ['download error', () => moduleLoad!.reject(new Error('the download failed (HTTP 404)')), /HTTP 404/],
    ['compile error', () => moduleLoad!.reject(new Error('it could not be compiled (bad magic)')), /compiled/],
    ['worklet error', (n) => n.port.onmessage!({ data: { t: 'error', reason: 'no exports' } }), /failed to start \(no exports\)/],
    ['watchdog', (n) => n.port.onmessage!({ data: { t: 'slow', load: 0.52 } }), /too slow.*52 %/],
  ];

  it.each(failures)('%s, permission granted: Browser by a re-open, with the reason', async (_name, trigger, reason) => {
    const { cb, node } = await started('rnnoise');
    trigger(node());
    await flush();
    expect(node().port.posted).toContainEqual({ t: 'denoise', on: false });
    expect(gumCalls.map((c) => c.noiseSuppression)).toEqual([false, true]);
    expect(last(cb)).toMatchObject({ requested: 'rnnoise', running: 'browser', fallback: expect.stringMatching(reason) });
  });

  it.each(failures)('%s, permission not granted: Off on the open track, with the reason', async (_name, trigger, reason) => {
    granted = false;
    const { cb, node } = await started('rnnoise');
    trigger(node());
    await flush();
    expect(gumCalls).toHaveLength(1);
    expect(last(cb)).toMatchObject({ running: 'off', fallback: expect.stringMatching(reason) });
  });

  it('on WebKit a fallback lands on Browser without a re-open', async () => {
    suppressible = false;
    const { cb } = await started('rnnoise');
    moduleLoad!.reject(new Error('the download failed'));
    await flush();
    expect(gumCalls).toHaveLength(1);
    expect(last(cb)).toMatchObject({ running: 'browser', fallback: 'the download failed' });
  });

  it('falls back once; RNNoise chosen again waits for the next join', async () => {
    const { cap, cb, node } = await started('rnnoise');
    node().port.onmessage!({ data: { t: 'slow', load: 0.5 } });
    node().port.onmessage!({ data: { t: 'error', reason: 'x' } });
    await flush();
    expect(gumCalls).toHaveLength(2);
    await cap.setNoise('rnnoise');
    expect(last(cb)).toMatchObject({ requested: 'rnnoise', running: 'browser', pending: true });
  });

  it('a failed download while the user already chose Browser changes nothing but the reason', async () => {
    const { cap, cb } = await started('rnnoise');
    await cap.setNoise('browser');
    moduleLoad!.reject(new Error('the download failed'));
    await flush();
    expect(gumCalls).toHaveLength(2);
    expect(last(cb)).toMatchObject({ running: 'browser', fallback: 'the download failed' });
  });

  it('on a 44.1 kHz context, the watchdog fallback and RNNoise → Off keep the encoder on 48 kHz blocks', async () => {
    ctxRate = 44100;
    granted = false;
    const { cap, node } = await started('rnnoise');
    moduleLoad!.resolve(MODULE);
    block(node(), 0.5, 0.95);
    node().port.onmessage!({ data: { t: 'slow', load: 0.5 } });
    await flush();
    block(node(), 0.5);
    await cap.setNoise('rnnoise');
    await cap.setNoise('off');
    block(node(), 0.5);
    expect(encoders).toHaveLength(1);
    expect(encoders[0].config.sampleRate).toBe(48000);
    expect(encoders[0].encoded.map((d) => d.sampleRate)).toEqual([48000, 48000, 48000]);
  });

  it('a processor error rebuilds the worklet without RNNoise and falls back', async () => {
    const { cb, node } = await started('rnnoise');
    moduleLoad!.resolve(MODULE);
    await flush();
    const dead = node();
    dead.onprocessorerror!();
    await flush();
    // One node from start, one carrying the module, one rebuilt plain.
    expect(workletNodes).toHaveLength(3);
    expect(dead.disconnected).toBe(1);
    const fresh = node();
    expect(fresh.options.processorOptions).toEqual({ denoise: false, module: MODULE });
    expect(sources.at(-1)!.connected).toContain(fresh);
    expect(last(cb)).toMatchObject({ running: 'browser', fallback: 'the audio worklet failed' });
  });
});

describe('VoiceCapture failures release the microphone', () => {
  it.each([
    [{ name: 'NotAllowedError' }, /refused/],
    [{ name: 'NotFoundError' }, /No microphone/],
    [new Error('busy'), /busy/],
  ])('getUserMedia rejecting is a capture error (%#)', async (err, msg) => {
    gumError = err;
    const cap = new VoiceCapture(callbacks(), deps(), 'off');
    const e = await cap.start(() => ({ context: fakeContext() })).catch((x: unknown) => x);
    expect(e).toBeInstanceOf(VoiceError);
    expect((e as VoiceError).phase).toBe('capture');
    expect((e as VoiceError).message).toMatch(msg);
  });

  it('no Opus encoder at all fails before the prompt', async () => {
    supported = [];
    const cap = new VoiceCapture(callbacks(), deps(), 'rnnoise');
    const e = await cap.start(() => null).catch((x: unknown) => x);
    expect((e as VoiceError).message).toMatch(/Opus/);
    expect(gumCalls).toHaveLength(0);
    expect(loads).toBe(0);
  });

  it('a track without audio, or stopped while starting, releases the stream', async () => {
    const t = new FakeTrack(9);
    const empty = new VoiceCapture(callbacks(), {
      ...deps(),
      getUserMedia: async () => ({ getAudioTracks: () => [], getTracks: () => [t] }) as unknown as MediaStream,
    });
    await expect(empty.start(() => null)).rejects.toBeInstanceOf(VoiceError);
    expect(t.stopped).toBe(1);

    const cap = new VoiceCapture(callbacks(), deps(), 'off');
    const s = cap.start(() => ({ context: fakeContext() }));
    cap.stop();
    await expect(s).rejects.toBeInstanceOf(VoiceError);
    expect(tracks[0].stopped).toBeGreaterThanOrEqual(1);
    // A mode change after a failed start does nothing.
    await cap.setNoise('browser');
    expect(gumCalls).toHaveLength(1);
  });

  it('the track ending and an encoder error each fail once, releasing everything', async () => {
    const { cb, node } = await started('off');
    block(node(), 0.5);
    encoders[0].error(new Error('opus died'));
    tracks[0].fire('ended');
    expect(cb.errors).toHaveLength(1);
    expect(cb.errors[0].message).toMatch(/opus died/);
    expect(tracks[0].stopped).toBe(1);
    expect(encoders[0].closed).toBe(1);
  });

  it('needs a context', async () => {
    const cap = new VoiceCapture(callbacks(), deps(), 'off');
    const e = await cap.start(() => null).catch((x: unknown) => x);
    expect((e as VoiceError).message).toMatch(/Web Audio/);
    expect(tracks[0].stopped).toBe(1);
  });
});

describe('voiceCaptureSupported', () => {
  it('needs getUserMedia, AudioEncoder, AudioWorkletNode and AudioData', () => {
    vi.stubGlobal('navigator', { mediaDevices: { getUserMedia: () => {} } });
    vi.stubGlobal('AudioEncoder', undefined);
    expect(voiceCaptureSupported()).toBe(false);
    vi.stubGlobal('AudioEncoder', class {});
    vi.stubGlobal('AudioData', undefined);
    expect(voiceCaptureSupported()).toBe(false);
    vi.stubGlobal('AudioData', class {});
    expect(voiceCaptureSupported()).toBe(true);
    vi.stubGlobal('navigator', {});
    expect(voiceCaptureSupported()).toBe(false);
  });
});

describe('queryMicPermission', () => {
  it('is granted only when the Permissions API says granted', async () => {
    const q = (state: string) => ({ query: async () => ({ state }) });
    expect(await queryMicPermission(q('granted'))).toBe(true);
    expect(await queryMicPermission(q('prompt'))).toBe(false);
    expect(await queryMicPermission(q('denied'))).toBe(false);
    expect(await queryMicPermission({ query: async () => Promise.reject(new TypeError('microphone')) })).toBe(false);
    expect(await queryMicPermission(undefined)).toBe(false);
  });
});
