// R13 pipeline-integration tests (docs/18, chunk L2): the advanced encoder
// settings — bitrate override, codec pin, acceleration tri-state — reach the
// encoder's negotiated config, changes recreate the encoder mid-stream, and
// the old >1080p@>30 force-cap is gone (an explicit 4K@60 choice is honored
// as-is). Same fake-encoder harness shape as broadcaster-fallback.test.ts.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

const connectWebTransport = vi.fn();
const startCapture = vi.fn();

const h = vi.hoisted(() => ({
  encoders: [] as Array<{
    disposed: boolean;
    config: {
      width: number;
      height: number;
      framerate: number;
      bitrate: number;
      codecPreferences: string[];
      hwPreference?: string;
    };
    cbs: { onError: (e: Error) => void };
  }>,
  targets: [] as Array<{ res: unknown; fps: unknown }>,
  frameCb: { value: null as null | ((frame: unknown) => void) },
  constraintCalls: [] as MediaTrackConstraints[],
  constraintsReject: { value: false },
}));

vi.mock('./connection', () => ({
  connectWebTransport: (...args: unknown[]) => connectWebTransport(...args),
  readDatagrams: () => new Promise(() => {}),
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
    setTarget(res: unknown, fps: unknown) {
      h.targets.push({ res, fps });
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
    config: (typeof h.encoders)[number]['config'];
    cbs: { onError: (e: Error) => void };
    constructor(
      config: (typeof h.encoders)[number]['config'],
      cbs: { onError: (e: Error) => void },
    ) {
      this.config = config;
      this.cbs = cbs;
      h.encoders.push(this);
    }
    configure() {
      return Promise.resolve({
        codec: this.config.codecPreferences[0],
        variantLabel: 'fake',
        acceleration: 'hardware',
        width: this.config.width,
        height: this.config.height,
        framerate: this.config.framerate,
        bitrate: this.config.bitrate,
      });
    }
    encode() {
      return true;
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
  DEFAULT_ENCODER_SETTINGS,
  type BroadcastCallbacks,
  type EncoderSettings,
} from './broadcaster';
import { EncoderSupportProber, type IsConfigSupportedFn } from '../media/probe';
import type { FramerateSelection, ResolutionSelection } from '../media/ladder';
import { DEFAULT_CAPTURE_CONFIG } from '../media/types';

// Prober whose stub browser supports hardware where `hw` approves and
// software everywhere (the real EncoderSupportProber logic runs).
function proberWhere(hw: (config: VideoEncoderConfig) => boolean): EncoderSupportProber {
  const fn: IsConfigSupportedFn = (config) =>
    Promise.resolve({
      supported: config.hardwareAcceleration === 'prefer-hardware' ? hw(config) : true,
      config,
    });
  return new EncoderSupportProber(fn);
}

function makeFakeWT() {
  return {
    closed: new Promise(() => {}),
    incomingUnidirectionalStreams: new ReadableStream({ start() {} }),
    datagrams: { maxDatagramSize: 1200 },
    close: vi.fn(),
  } as unknown as WebTransport;
}

function makeCaptureHandle() {
  return {
    stream: {} as MediaStream,
    track: {
      getSettings: () => ({ frameRate: 60 }),
      addEventListener: vi.fn(),
      applyConstraints: (c: MediaTrackConstraints) => {
        h.constraintCalls.push(c);
        return h.constraintsReject.value ? Promise.reject(new Error('nope')) : Promise.resolve();
      },
    },
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

const clock = { t: 1000 };
const errors: Error[] = [];

async function startPipeline(
  settings?: Partial<EncoderSettings>,
  opts: {
    prober?: EncoderSupportProber;
    ladder?: { selection: ResolutionSelection; framerate: FramerateSelection };
  } = {},
): Promise<BroadcastPipeline> {
  const cbs: BroadcastCallbacks = {
    onSourceStream: vi.fn(),
    onEncoderConfigured: vi.fn(),
    onCapturePathChosen: vi.fn(),
    onStats: vi.fn(),
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
    undefined,
    opts.prober,
  );
  if (settings) pipeline.setEncoderSettings({ ...DEFAULT_ENCODER_SETTINGS, ...settings });
  if (opts.ladder) pipeline.setLadder(opts.ladder.selection, opts.ladder.framerate);
  await pipeline.start();
  return pipeline;
}

async function prime() {
  clock.t += 33;
  h.frameCb.value!(fakeFrame(Math.round(clock.t * 1000)));
  await flush();
}

beforeEach(() => {
  vi.stubGlobal('window', globalThis);
  vi.useFakeTimers();
  clock.t = 1000;
  errors.length = 0;
  connectWebTransport.mockReset();
  startCapture.mockReset();
  h.encoders.length = 0;
  h.targets.length = 0;
  h.frameCb.value = null;
  h.constraintCalls.length = 0;
  h.constraintsReject.value = false;
});

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe('bitrate override (docs/18 Decision 11)', () => {
  it('replaces the ladder bitrate, clamped to the 50 Mbps ceiling', async () => {
    const p = await startPipeline({ bitrateOverride: 80_000_000 });
    await prime();
    expect(h.encoders[0].config.bitrate).toBe(50_000_000);
    await p.stop();
  });

  it('clamps to the 0.5 Mbps floor', async () => {
    const p = await startPipeline({ bitrateOverride: 1 });
    await prime();
    expect(h.encoders[0].config.bitrate).toBe(500_000);
    await p.stop();
  });

  it('null override keeps the ladder math (4K@60 clamps to the 10 Mbps ladder cap)', async () => {
    const p = await startPipeline();
    await prime();
    expect(h.encoders[0].config.bitrate).toBe(10_000_000);
    await p.stop();
  });
});

describe('codec pin (docs/18 Decision 12)', () => {
  it('narrows the preference walk to the pinned codec', async () => {
    const p = await startPipeline({ codecOverride: 'vp8' });
    await prime();
    expect(h.encoders[0].config.codecPreferences).toEqual(['vp8']);
    await p.stop();
  });

  it('no pin keeps the full preference list', async () => {
    const p = await startPipeline();
    await prime();
    expect(h.encoders[0].config.codecPreferences).toEqual(
      DEFAULT_CAPTURE_CONFIG.codecPreferences,
    );
    await p.stop();
  });
});

describe('acceleration tri-state plumbing', () => {
  it('hwPreference reaches the encoder config', async () => {
    const p = await startPipeline({ hwPreference: 'software' });
    await prime();
    expect(h.encoders[0].config.hwPreference).toBe('software');
    await p.stop();
  });
});

describe('mid-stream settings changes', () => {
  it('a settings change disposes the encoder and renegotiates from the next frame', async () => {
    const p = await startPipeline();
    await prime();
    expect(h.encoders.length).toBe(1);
    p.setEncoderSettings({ ...DEFAULT_ENCODER_SETTINGS, bitrateOverride: 2_000_000 });
    await prime();
    expect(h.encoders.length).toBe(2);
    expect(h.encoders[0].disposed).toBe(true);
    expect(h.encoders[1].config.bitrate).toBe(2_000_000);
    await p.stop();
  });

  it('setting identical settings is a no-op (no encoder churn)', async () => {
    const p = await startPipeline();
    await prime();
    p.setEncoderSettings({ ...DEFAULT_ENCODER_SETTINGS });
    await prime();
    expect(h.encoders.length).toBe(1);
    await p.stop();
  });
});

describe('the >1080p@>30 force-cap is gone (docs/18 Decision 10)', () => {
  it('explicit 4K@60 configures at 60 fps with no probe gate', async () => {
    const p = await startPipeline({ hwPreference: 'software' });
    p.setLadder('native', 60);
    await prime();
    expect(h.encoders[h.encoders.length - 1].config.framerate).toBe(60);
    expect(h.encoders[h.encoders.length - 1].config.width).toBe(3840);
    await p.stop();
  });
});

describe('HW-aware auto ceiling + auto fps (docs/18 Decisions 3+4, L3)', () => {
  it('auto/auto with HW up to 1080p starts at the 1080 rung at 60 fps', async () => {
    const p = await startPipeline(undefined, {
      prober: proberWhere((c) => (c.width ?? 0) <= 1920),
      ladder: { selection: 'auto', framerate: 'auto' },
    });
    await prime(); // 4K source frame
    const last = h.targets[h.targets.length - 1];
    expect(last).toEqual({ res: 1080, fps: 60 });
    expect(h.encoders[0].config.framerate).toBe(60);
    await p.stop();
  });

  it('an all-software matrix resolves 1080p30 (Firefox shape)', async () => {
    const p = await startPipeline(undefined, {
      prober: proberWhere(() => false),
      ladder: { selection: 'auto', framerate: 'auto' },
    });
    await prime();
    expect(h.targets[h.targets.length - 1]).toEqual({ res: 1080, fps: 30 });
    await p.stop();
  });

  it('full-hardware support keeps the native ceiling at 60 fps', async () => {
    const p = await startPipeline(undefined, {
      prober: proberWhere(() => true),
      ladder: { selection: 'auto', framerate: 'auto' },
    });
    await prime();
    expect(h.targets[h.targets.length - 1]).toEqual({ res: 'native', fps: 60 });
    await p.stop();
  });

  it('explicit rungs bypass the ceiling entirely', async () => {
    const p = await startPipeline(undefined, {
      prober: proberWhere(() => false), // nothing does HW
      ladder: { selection: 'native', framerate: 60 },
    });
    await prime();
    expect(h.targets[h.targets.length - 1]).toEqual({ res: 'native', fps: 60 });
    expect(h.encoders[0].config.width).toBe(3840);
    expect(h.encoders[0].config.framerate).toBe(60);
    await p.stop();
  });

  it('no prober (no WebCodecs in scope) keeps the optimistic pre-R13 defaults', async () => {
    const p = await startPipeline(undefined, {
      ladder: { selection: 'auto', framerate: 'native' },
    });
    await prime();
    expect(h.targets[h.targets.length - 1]).toEqual({ res: 'native', fps: 'native' });
    await p.stop();
  });
});

describe('capture alignment via applyConstraints (docs/18 Decision 6, L3)', () => {
  it('constrains capture to the auto ceiling at start', async () => {
    const p = await startPipeline(undefined, {
      prober: proberWhere((c) => (c.width ?? 0) <= 1920),
      ladder: { selection: 'auto', framerate: 'auto' },
    });
    expect(h.constraintCalls).toEqual([{ width: { max: 1920 }, frameRate: { max: 60 } }]);
    await p.stop();
  });

  it('an explicit rung change mid-stream re-constrains capture — no restart', async () => {
    const p = await startPipeline(undefined, {
      prober: proberWhere(() => true),
      ladder: { selection: 'native', framerate: 60 },
    });
    await prime();
    expect(startCapture).toHaveBeenCalledTimes(1);
    p.setLadder(720, 30);
    await flush(); // the constraint application is promise-wrapped
    expect(h.constraintCalls[h.constraintCalls.length - 1]).toEqual({
      width: { max: 1280 },
      frameRate: { max: 30 },
    });
    expect(startCapture).toHaveBeenCalledTimes(1); // still the same capture
    await p.stop();
  });

  it('identical constraints are not re-applied', async () => {
    const p = await startPipeline(undefined, {
      prober: proberWhere(() => true),
      ladder: { selection: 'native', framerate: 60 },
    });
    const calls = h.constraintCalls.length;
    p.setLadder('native', 60);
    await flush(); // give a wrongly-queued application the chance to land
    expect(h.constraintCalls.length).toBe(calls);
    await p.stop();
  });

  it('auto steps are encode-only: an encoder-error step-down never touches capture', async () => {
    const p = await startPipeline(undefined, {
      prober: proberWhere(() => true),
      ladder: { selection: 'auto', framerate: 'auto' },
    });
    await prime();
    const calls = h.constraintCalls.length;
    // Strongest step-down evidence: a mid-stream encoder error in auto mode.
    h.encoders[0].cbs.onError(new Error('encode failed'));
    await prime();
    expect(h.targets[h.targets.length - 1]).toEqual({ res: 1080, fps: 60 }); // stepped down
    expect(h.constraintCalls.length).toBe(calls); // capture untouched
    await p.stop();
  });

  it('an applyConstraints rejection is swallowed; the pipeline keeps running', async () => {
    h.constraintsReject.value = true;
    const p = await startPipeline(undefined, {
      prober: proberWhere(() => true),
      ladder: { selection: 720, framerate: 30 },
    });
    await prime();
    await prime();
    expect(errors).toEqual([]);
    expect(h.encoders.length).toBe(1);
    await p.stop();
  });
});
