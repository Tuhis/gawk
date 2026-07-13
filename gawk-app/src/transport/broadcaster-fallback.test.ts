// R4 pipeline-integration tests (docs/09, chunk I2). Unlike broadcaster.test.ts
// (URLs / announce / start failures) these drive real frames through the
// pipeline with a controllable fake Encoder and an injected clock, so the
// FallbackController's time-based decisions are deterministic. The
// FramePreprocessor is mocked to a passthrough that records the rung the
// pipeline commands (real scaling needs OffscreenCanvas/VideoFrame, and it
// has its own test); the real FallbackController and ladder run.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

const connectWebTransport = vi.fn();
const startCapture = vi.fn();

const h = vi.hoisted(() => ({
  preprocessors: [] as Array<{ targets: Array<{ res: unknown; fps: unknown }> }>,
  encoders: [] as Array<{ disposed: boolean; cbs: { onError: (e: Error) => void } }>,
  encodeReject: { value: false },
  frameCb: { value: null as null | ((frame: unknown) => void) },
}));

vi.mock('./connection', () => ({
  connectWebTransport: (...args: unknown[]) => connectWebTransport(...args),
  DatagramSender: class {
    send = vi.fn(() => Promise.resolve());
    close = vi.fn();
  },
}));

vi.mock('../media/capture', () => ({
  startCapture: (...args: unknown[]) => startCapture(...args),
  stopCapture: vi.fn(),
}));

vi.mock('../media/preprocess', () => ({
  FramePreprocessor: class {
    targets: Array<{ res: unknown; fps: unknown }> = [];
    constructor() {
      h.preprocessors.push(this);
    }
    setTarget(res: unknown, fps: unknown) {
      this.targets.push({ res, fps });
    }
    process(frame: unknown) {
      return frame;
    }
    getStats() {
      return { gateDropped: 0, scaledFrames: 0 };
    }
  },
}));

vi.mock('../media/encoder', () => ({
  Encoder: class {
    disposed = false;
    config: { width: number; height: number; framerate: number; bitrate: number };
    cbs: { onError: (e: Error) => void };
    constructor(config: typeof this.config, cbs: typeof this.cbs) {
      this.config = config;
      this.cbs = cbs;
      h.encoders.push(this);
    }
    configure() {
      return Promise.resolve({
        codec: 'avc1.42E01F',
        variantLabel: 'fake',
        acceleration: 'hardware',
        width: this.config.width,
        height: this.config.height,
        framerate: this.config.framerate,
        bitrate: this.config.bitrate,
      });
    }
    encode() {
      return !h.encodeReject.value;
    }
    get queueSize() {
      return 0;
    }
    dispose() {
      this.disposed = true;
    }
    close() {
      return Promise.resolve();
    }
  },
}));

import {
  BroadcastPipeline,
  type BroadcastCallbacks,
  type BroadcastStats,
} from './broadcaster';
import { DEFAULT_CAPTURE_CONFIG } from '../media/types';
import type { ResolutionSelection } from '../media/ladder';

function makeFakeWT() {
  let resolveClosed: (v: unknown) => void = () => {};
  const closed = new Promise((res) => {
    resolveClosed = res;
  });
  const incomingUnidirectionalStreams = new ReadableStream({ start() {} });
  const close = vi.fn(() => resolveClosed({ closeCode: 0, reason: '' }));
  return {
    closed,
    incomingUnidirectionalStreams,
    datagrams: { maxDatagramSize: 1200 },
    close,
  } as unknown as WebTransport;
}

function makeCaptureHandle() {
  return {
    stream: {} as MediaStream,
    track: { getSettings: () => ({ frameRate: 60 }), addEventListener: vi.fn() },
    capturePath: 'fake',
    startFrames: (cb: (frame: unknown) => void) => {
      h.frameCb.value = cb;
      return Promise.resolve();
    },
  };
}

function fakeFrame(tsUs: number, w = 3840, hgt = 2160) {
  return {
    timestamp: tsUs,
    displayWidth: w,
    displayHeight: hgt,
    codedWidth: w,
    codedHeight: hgt,
    close: vi.fn(),
  };
}

async function flush() {
  for (let i = 0; i < 4; i++) await Promise.resolve();
}

interface Setup {
  pipeline: BroadcastPipeline;
  clock: { t: number };
  statsLog: BroadcastStats[];
  errors: Error[];
  prime: () => Promise<void>;
  drive: (ms: number, opts?: { fps?: number; rejectRatio?: number }) => Promise<void>;
  snapshot: () => Promise<BroadcastStats>;
  lastEncoder: () => (typeof h.encoders)[number];
}

async function setup(selection: ResolutionSelection, fps: 'native' | 60 | 30 | 5 = 'native'): Promise<Setup> {
  const clock = { t: 1000 };
  const statsLog: BroadcastStats[] = [];
  const errors: Error[] = [];
  const cbs: BroadcastCallbacks = {
    onSourceStream: vi.fn(),
    onEncoderConfigured: vi.fn(),
    onCapturePathChosen: vi.fn(),
    onStats: (s) => statsLog.push(s),
    onError: (e) => errors.push(e),
    onEnded: vi.fn(),
    onBroadcastId: vi.fn(),
  };
  connectWebTransport.mockResolvedValue(makeFakeWT());
  startCapture.mockResolvedValue(makeCaptureHandle());

  const pipeline = new BroadcastPipeline(
    { ...DEFAULT_CAPTURE_CONFIG },
    'https://relay.test:4433',
    {},
    cbs,
    undefined,
    () => clock.t,
  );
  pipeline.setLadder(selection, fps);
  await pipeline.start();

  let rejectAcc = 0;
  const prime = async () => {
    clock.t += 33;
    h.frameCb.value!(fakeFrame(Math.round(clock.t * 1000)));
    await flush();
  };
  const drive = async (ms: number, opts: { fps?: number; rejectRatio?: number } = {}) => {
    const f = opts.fps ?? 20;
    const rr = opts.rejectRatio ?? 0;
    const dt = 1000 / f;
    const n = Math.round(ms / dt);
    for (let i = 0; i < n; i++) {
      clock.t += dt;
      rejectAcc += rr;
      if (rejectAcc >= 1) {
        rejectAcc -= 1;
        h.encodeReject.value = true;
      } else {
        h.encodeReject.value = false;
      }
      h.frameCb.value!(fakeFrame(Math.round(clock.t * 1000)));
      await flush();
    }
  };
  const snapshot = async () => {
    await vi.advanceTimersByTimeAsync(600);
    return statsLog[statsLog.length - 1];
  };
  const lastEncoder = () => h.encoders[h.encoders.length - 1];

  return { pipeline, clock, statsLog, errors, prime, drive, snapshot, lastEncoder };
}

beforeEach(() => {
  vi.stubGlobal('window', globalThis);
  vi.useFakeTimers();
  connectWebTransport.mockReset();
  startCapture.mockReset();
  h.preprocessors.length = 0;
  h.encoders.length = 0;
  h.encodeReject.value = false;
  h.frameCb.value = null;
});

afterEach(async () => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe('explicit selection is never auto-stepped', () => {
  it('holds the rung under sustained rejection: only droppedFrames grows and pressure sets', async () => {
    const s = await setup(1080);
    await s.prime();
    await s.drive(7000, { rejectRatio: 0.5 });
    const stats = await s.snapshot();

    // The preprocessor was commanded exactly once — the explicit 1080 rung.
    const targets = h.preprocessors[0].targets;
    expect(targets).toHaveLength(1);
    expect(targets[0].res).toBe(1080);

    expect(stats.autoStepDowns).toBe(0);
    expect(stats.autoStepUps).toBe(0);
    expect(stats.autoRung).toBeNull();
    expect(stats.droppedFrames).toBeGreaterThan(0);
    expect(stats.encoderPressure).toBe(true);
  });

  it('encoder error stays fatal in explicit mode', async () => {
    const s = await setup(1080);
    await s.prime();
    s.lastEncoder().cbs.onError(new Error('gpu reset'));
    await flush();
    expect(s.errors).toHaveLength(1);
    expect(s.errors[0].message).toBe('gpu reset');
  });
});

describe('auto mode steps', () => {
  it('steps down one rung under sustained rejection', async () => {
    const s = await setup('auto');
    await s.prime();
    await s.drive(7000, { rejectRatio: 0.5 });
    const stats = await s.snapshot();

    expect(stats.autoStepDowns).toBe(1);
    expect(stats.autoRung).toBe(1080); // 3840 source: autoLadder = [native,1080,720,480]
    expect(stats.autoAtFloor).toBe(false);
    // Preprocessor was commanded native (start) then 1080 (the step).
    const targets = h.preprocessors[0].targets;
    expect(targets[targets.length - 1].res).toBe(1080);
  });

  it('steps back up after a sustained healthy period', async () => {
    const s = await setup('auto');
    await s.prime();
    await s.drive(7000, { rejectRatio: 0.5 }); // → step down to 1080
    expect((await s.snapshot()).autoRung).toBe(1080);
    await s.drive(44000, { rejectRatio: 0 }); // cooldown + UP_PROBE_MS healthy → step up
    const stats = await s.snapshot();
    expect(stats.autoStepUps).toBe(1);
    expect(stats.autoRung).toBe('native');
  });
});

describe('mid-broadcast selection changes', () => {
  it('switching to an explicit rung applies immediately and stops stepping', async () => {
    const s = await setup('auto');
    await s.prime();
    await s.drive(7000, { rejectRatio: 0.5 }); // → 1080
    expect((await s.snapshot()).autoStepDowns).toBe(1);

    s.pipeline.setLadder(720, 'native');
    // The selection change recreates the encoder → 8s cooldown; drive past it
    // plus a full window of rejections so the pressure warning can latch.
    await s.drive(14000, { rejectRatio: 0.5 }); // heavy pressure, but explicit now
    const stats = await s.snapshot();

    expect(stats.autoRung).toBeNull(); // no longer auto
    expect(stats.autoStepDowns).toBe(1); // unchanged — explicit doesn't step
    const targets = h.preprocessors[0].targets;
    expect(targets[targets.length - 1].res).toBe(720); // applied immediately
    expect(stats.encoderPressure).toBe(true);
  });

  it('switching back to auto restarts at the ceiling', async () => {
    const s = await setup(480); // explicit floor
    await s.prime();
    await s.drive(3000, { rejectRatio: 0.5 });

    s.pipeline.setLadder('auto', 'native');
    await s.drive(1000); // one healthy frame recomputes the ladder
    const stats = await s.snapshot();
    expect(stats.autoRung).toBe('native'); // ceiling
    const targets = h.preprocessors[0].targets;
    expect(targets[targets.length - 1].res).toBe('native');
  });

  it('a framerate-only change keeps the current auto rung', async () => {
    const s = await setup('auto', 60);
    await s.prime();
    await s.drive(7000, { rejectRatio: 0.5 }); // → 1080
    expect((await s.snapshot()).autoRung).toBe(1080);

    s.pipeline.setLadder('auto', 30); // fps change only
    const stats = await s.snapshot();
    expect(stats.autoRung).toBe(1080); // rung preserved
    expect(stats.autoStepDowns).toBe(1); // no re-descent
    const last = h.preprocessors[0].targets[h.preprocessors[0].targets.length - 1];
    expect(last.res).toBe(1080);
    expect(last.fps).toBe(30);
  });
});

describe('auto-mode encoder errors', () => {
  it('steps down on error and recreates, then fails on a second error inside the bound', async () => {
    const s = await setup('auto');
    await s.prime();
    const encA = s.lastEncoder();

    encA.cbs.onError(new Error('encoder glitch'));
    expect(s.errors).toHaveLength(0); // stepped down, not failed
    expect(encA.disposed).toBe(true);

    await s.drive(200); // feed a frame → recreate at the stepped rung
    expect((await s.snapshot()).autoRung).toBe(1080);
    const encB = s.lastEncoder();
    expect(encB).not.toBe(encA);

    // Second error within ERROR_FAIL_WINDOW_MS: the problem isn't resolution.
    encB.cbs.onError(new Error('encoder glitch again'));
    await flush();
    expect(s.errors).toHaveLength(1);
    expect(s.errors[0].message).toBe('encoder glitch again');
  });
});
