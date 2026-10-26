// The microphone → Opus path (docs/80 D21) against fakes: the 10 ms / 20 ms
// encoder probe, the MSTP pump and the AudioWorklet path, the gate deciding
// what is encoded, the spurt end after the encoder drains, and every failure
// releasing the track.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  VOICE_AUDIO_CONSTRAINTS,
  VOICE_BITRATE_BPS,
  VOICE_FALLBACK_FRAME_US,
  VOICE_FRAME_US,
  VoiceCapture,
  chooseFrameDuration,
  seqUnitsFor,
  voiceCaptureSupported,
  voiceEncoderConfig,
  type VoiceAudioData,
  type VoiceCaptureDeps,
  type VoiceEncoderConfig,
} from './voiceCapture';
import { VoiceError } from './voiceSession';
import { CAPTURE_PROCESSOR_NAME } from './voiceFraming';

class FakeTrack {
  stopped = 0;
  listeners: Record<string, (() => void)[]> = {};
  stop() {
    this.stopped++;
  }
  addEventListener(type: string, fn: () => void) {
    (this.listeners[type] ??= []).push(fn);
  }
  fire(type: string) {
    for (const fn of this.listeners[type] ?? []) fn();
  }
}

class FakeData implements VoiceAudioData {
  closed = 0;
  readonly level: number;
  readonly timestamp: number;
  readonly sampleRate: number;
  readonly numberOfChannels = 1;
  readonly numberOfFrames = 480;
  constructor(level: number, timestamp = 0, sampleRate = 48000) {
    this.level = level;
    this.timestamp = timestamp;
    this.sampleRate = sampleRate;
  }
  copyTo(dst: Float32Array) {
    dst.fill(this.level);
  }
  close() {
    this.closed++;
  }
}

let track: FakeTrack;
let encoders: {
  config: VoiceEncoderConfig;
  encoded: VoiceAudioData[];
  flushes: number;
  closed: number;
  output: (d: Uint8Array, dur: number | null) => void;
  error: (e: Error) => void;
}[];
let gumError: unknown;
let supported: number[];
let mstpCtl: ReadableStreamDefaultController<VoiceAudioData> | null;

function deps(over: Partial<VoiceCaptureDeps> = {}): VoiceCaptureDeps {
  return {
    getUserMedia: async (c) => {
      expect(c.audio).toEqual(VOICE_AUDIO_CONSTRAINTS);
      if (gumError) throw gumError;
      return { getAudioTracks: () => [track], getTracks: () => [track] } as unknown as MediaStream;
    },
    isConfigSupported: async (c) => ({ supported: supported.includes(c.opus.frameDuration) }),
    createEncoder: (config, cb) => {
      const e = {
        config,
        encoded: [] as VoiceAudioData[],
        flushes: 0,
        closed: 0,
        output: cb.output,
        error: cb.error,
      };
      encoders.push(e);
      return {
        encode: (d) => void e.encoded.push(d),
        flush: async () => void e.flushes++,
        close: () => void e.closed++,
      };
    },
    createProcessor: () =>
      new ReadableStream<VoiceAudioData>({
        start: (c) => {
          mstpCtl = c;
        },
      }),
    createAudioData: (init) => new FakeData((init.data as Float32Array)[0] ?? 0, init.timestamp, init.sampleRate),
    now: () => 0,
    ...over,
  };
}

function callbacks(gateOpen: (rms: number) => boolean = (rms) => rms > 0.05) {
  const cb = {
    packets: [] as [number, number][],
    ends: 0,
    errors: [] as VoiceError[],
    shouldSend: (rms: number) => gateOpen(rms),
    onPacket: (p: Uint8Array, units: number) => cb.packets.push([p.length, units]),
    onSpurtEnd: () => void cb.ends++,
    onError: (e: VoiceError) => void cb.errors.push(e),
  };
  return cb;
}

const flush = () => new Promise((r) => setTimeout(r, 0));

beforeEach(() => {
  track = new FakeTrack();
  encoders = [];
  gumError = null;
  supported = [VOICE_FRAME_US, VOICE_FALLBACK_FRAME_US];
  mstpCtl = null;
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

describe('VoiceCapture, MSTP path', () => {
  it('encodes only what the gate passes, configured from the frame in hand', async () => {
    const cb = callbacks();
    const cap = new VoiceCapture(cb, deps());
    await cap.start(() => null);
    const quiet = new FakeData(0);
    const loud = new FakeData(0.5, 0, 44100);
    mstpCtl!.enqueue(quiet);
    mstpCtl!.enqueue(loud);
    await flush();
    expect(quiet.closed).toBe(1);
    expect(encoders).toHaveLength(1);
    expect(encoders[0].config.sampleRate).toBe(44100);
    expect(encoders[0].config.opus.frameDuration).toBe(VOICE_FRAME_US);
    expect(encoders[0].encoded).toEqual([loud]);
    expect(loud.closed).toBe(1);
    encoders[0].output(new Uint8Array(50), 10_000);
    encoders[0].output(new Uint8Array(0), 10_000); // empty: never sent
    expect(cb.packets).toEqual([[50, 1]]);
    // The spurt ends: the encoder drains, THEN the terminator is due.
    mstpCtl!.enqueue(new FakeData(0));
    await flush();
    expect(encoders[0].flushes).toBe(1);
    expect(cb.ends).toBe(1);
    cap.stop();
    expect(track.stopped).toBe(1);
    expect(encoders[0].closed).toBe(1);
  });

  it('uses 20 ms frames where 10 ms is refused', async () => {
    supported = [VOICE_FALLBACK_FRAME_US];
    const cb = callbacks();
    const cap = new VoiceCapture(cb, deps());
    await cap.start(() => null);
    mstpCtl!.enqueue(new FakeData(0.5));
    await flush();
    expect(encoders[0].config.opus.frameDuration).toBe(VOICE_FALLBACK_FRAME_US);
    encoders[0].output(new Uint8Array(60), null);
    expect(cb.packets).toEqual([[60, 2]]);
    cap.stop();
  });

  it('an encoder that cannot be built fails the capture and releases the track', async () => {
    let open = true;
    const cb = callbacks(() => open);
    const cap = new VoiceCapture(cb, {
      ...deps(),
      createEncoder: () => {
        throw new Error('no encoder');
      },
    });
    await cap.start(() => null);
    mstpCtl!.enqueue(new FakeData(0.5));
    await flush();
    // The encoder could not be built: a capture failure, track released.
    expect(cb.errors[0].phase).toBe('capture');
    expect(track.stopped).toBe(1);
    open = false;
    cap.stop();
  });
});

describe('VoiceCapture failures release the microphone', () => {
  it.each([
    [{ name: 'NotAllowedError' }, /refused/],
    [{ name: 'NotFoundError' }, /No microphone/],
    [new Error('busy'), /busy/],
  ])('getUserMedia rejecting is a capture error (%#)', async (err, msg) => {
    gumError = err;
    const cap = new VoiceCapture(callbacks(), deps());
    const e = await cap.start(() => null).catch((x: unknown) => x);
    expect(e).toBeInstanceOf(VoiceError);
    expect((e as VoiceError).phase).toBe('capture');
    expect((e as VoiceError).message).toMatch(msg);
  });

  it('no Opus encoder at all fails before the prompt', async () => {
    supported = [];
    const gum = vi.fn();
    const cap = new VoiceCapture(callbacks(), deps({ getUserMedia: gum }));
    const e = await cap.start(() => null).catch((x: unknown) => x);
    expect((e as VoiceError).message).toMatch(/Opus/);
    expect(gum).not.toHaveBeenCalled();
  });

  it('a track without audio, or stopped while starting, releases the stream', async () => {
    const empty = new VoiceCapture(callbacks(), {
      ...deps(),
      getUserMedia: async () => ({ getAudioTracks: () => [], getTracks: () => [track] }) as unknown as MediaStream,
    });
    await expect(empty.start(() => null)).rejects.toBeInstanceOf(VoiceError);
    expect(track.stopped).toBe(1);

    track = new FakeTrack();
    const cap = new VoiceCapture(callbacks(), deps());
    const started = cap.start(() => null);
    cap.stop();
    await expect(started).rejects.toBeInstanceOf(VoiceError);
    expect(track.stopped).toBeGreaterThanOrEqual(1);
  });

  it('the track ending and an encoder error each fail once, releasing everything', async () => {
    const cb = callbacks();
    const cap = new VoiceCapture(cb, deps());
    await cap.start(() => null);
    mstpCtl!.enqueue(new FakeData(0.5));
    await flush();
    encoders[0].error(new Error('opus died'));
    track.fire('ended');
    expect(cb.errors).toHaveLength(1);
    expect(cb.errors[0].message).toMatch(/opus died/);
    expect(track.stopped).toBe(1);
    expect(encoders[0].closed).toBe(1);
  });

  it('the worklet path needs a context', async () => {
    const cap = new VoiceCapture(callbacks(), deps({ createProcessor: null }));
    const e = await cap.start(() => null).catch((x: unknown) => x);
    expect((e as VoiceError).message).toMatch(/Web Audio/);
    expect(track.stopped).toBe(1);
  });
});

describe('VoiceCapture, AudioWorklet path (Firefox, Safari)', () => {
  function fakeContext(sampleRate = 48000) {
    const nodes: { kind: string; connected: unknown[]; disconnected: number; gain?: { value: number } }[] = [];
    const node = (kind: string) => {
      const n = {
        kind,
        connected: [] as unknown[],
        disconnected: 0,
        connect(to: unknown) {
          n.connected.push(to);
        },
        disconnect() {
          n.disconnected++;
        },
      };
      nodes.push(n);
      return n;
    };
    const ctx = {
      sampleRate,
      destination: { kind: 'destination' },
      audioWorklet: { addModule: vi.fn(async () => {}) },
      createMediaStreamSource: () => node('source'),
      createGain: () => Object.assign(node('gain'), { gain: { value: 1 } }),
    };
    const worklets: { name: string; port: { onmessage: ((e: { data: unknown }) => void) | null } }[] = [];
    vi.stubGlobal(
      'AudioWorkletNode',
      class {
        port = { onmessage: null as ((e: { data: unknown }) => void) | null };
        connected: unknown[] = [];
        disconnected = 0;
        constructor(_ctx: unknown, name: string) {
          worklets.push({ name, port: this.port });
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
    return { ctx: ctx as unknown as AudioContext, nodes, worklets };
  }

  it('frames the worklet PCM into 10 ms AudioData for the encoder, muted to the speakers', async () => {
    const { ctx, nodes, worklets } = fakeContext();
    const cb = callbacks();
    const cap = new VoiceCapture(cb, deps({ createProcessor: null }));
    await cap.start(() => ({ context: ctx }));
    expect(worklets[0].name).toBe(CAPTURE_PROCESSOR_NAME);
    // The mic is never heard: a zero gain is what pulls the node.
    const gain = nodes.find((n) => n.kind === 'gain')!;
    expect(gain.gain!.value).toBe(0);
    for (let q = 0; q < 8; q++) worklets[0].port.onmessage!({ data: new Float32Array(128).fill(0.5) });
    worklets[0].port.onmessage!({ data: 'not pcm' });
    // 1024 samples → two 480-sample blocks.
    expect(encoders[0].encoded).toHaveLength(2);
    const d = encoders[0].encoded[1] as FakeData;
    expect(d.timestamp).toBe(10_000);
    expect(d.closed).toBe(1);
    cap.stop();
    expect(track.stopped).toBe(1);
    expect(nodes.every((n) => n.disconnected === 1)).toBe(true);
    worklets[0].port.onmessage?.({ data: new Float32Array(480).fill(0.5) });
    expect(encoders[0].encoded).toHaveLength(2);
  });

  it('a block the gate refuses is never built into AudioData, and a bad AudioData fails', async () => {
    const { ctx, worklets } = fakeContext();
    const create = vi.fn(() => {
      throw new Error('bad init');
    });
    const cb = callbacks();
    const cap = new VoiceCapture(cb, deps({ createProcessor: null, createAudioData: create }));
    await cap.start(() => ({ context: ctx }));
    worklets[0].port.onmessage!({ data: new Float32Array(480) });
    expect(create).not.toHaveBeenCalled();
    worklets[0].port.onmessage!({ data: new Float32Array(480).fill(0.5) });
    expect(cb.errors[0].phase).toBe('capture');
    expect(track.stopped).toBe(1);
  });
});

describe('voiceCaptureSupported', () => {
  it('needs getUserMedia, AudioEncoder and a frame source', () => {
    vi.stubGlobal('navigator', { mediaDevices: { getUserMedia: () => {} } });
    vi.stubGlobal('AudioEncoder', undefined);
    expect(voiceCaptureSupported()).toBe(false);
    vi.stubGlobal('AudioEncoder', class {});
    vi.stubGlobal('MediaStreamTrackProcessor', class {});
    expect(voiceCaptureSupported()).toBe(true);
    vi.stubGlobal('MediaStreamTrackProcessor', undefined);
    vi.stubGlobal('AudioWorkletNode', class {});
    vi.stubGlobal('AudioData', class {});
    expect(voiceCaptureSupported()).toBe(true);
    vi.stubGlobal('navigator', {});
    expect(voiceCaptureSupported()).toBe(false);
  });
});
