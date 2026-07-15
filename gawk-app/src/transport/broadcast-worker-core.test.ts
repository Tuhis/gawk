// R11 K2 acceptance (docs/16): BroadcastWorkerCore is unit-testable
// synchronously with a fake host + fake pipeline factory — no real Worker,
// MSTP, or WebTransport. Pins the command/event mapping, the
// awaitingCapture/capture handshake, phase preservation, and the generation
// guard.

import { describe, expect, it, vi } from 'vitest';

import {
  BroadcastWorkerCore,
  type BroadcastPipelineFactory,
  type BroadcastWorkerEvent,
  type BroadcastWorkerHost,
} from './broadcast-worker-core';
import { BroadcastStartError, type BroadcastCallbacks, type BroadcastStats } from './broadcaster';
import type { BroadcastMediaSourceFactory } from '../media/capture';
import type { EncoderConfigured } from '../media/encoder';
import { DEFAULT_CAPTURE_CONFIG } from '../media/types';

function fakeHost() {
  const events: BroadcastWorkerEvent[] = [];
  const host: BroadcastWorkerHost = { post: (ev) => events.push(ev) };
  return { host, events };
}

function makeFakePipeline() {
  let resolveStart!: () => void;
  let rejectStart!: (e: unknown) => void;
  const outcome = new Promise<void>((res, rej) => {
    resolveStart = res;
    rejectStart = rej;
  });
  const pipeline = {
    start: vi.fn(() => outcome),
    stop: vi.fn(() => Promise.resolve()),
    setLadder: vi.fn(),
    setEncoderSettings: vi.fn(),
  };
  return { pipeline, resolveStart, rejectStart };
}

interface Captured {
  cbs: BroadcastCallbacks;
  mediaSource: BroadcastMediaSourceFactory;
  broadcastId: string | undefined;
}

// Wires a core around one fake pipeline; hands back the callbacks + media
// source factory the core built so tests can drive them directly.
function makeCore() {
  const { host, events } = fakeHost();
  const fake = makeFakePipeline();
  const captured: Captured[] = [];
  const factory: BroadcastPipelineFactory = (_config, _url, _opts, cbs, broadcastId, mediaSource) => {
    captured.push({ cbs, mediaSource, broadcastId });
    return fake.pipeline;
  };
  const core = new BroadcastWorkerCore(host, factory);
  return { core, host, events, fake, captured };
}

const START_PARAMS = {
  config: { ...DEFAULT_CAPTURE_CONFIG },
  serverUrl: 'https://relay.test:4433',
  connectOpts: {},
  broadcastId: 'K7XQ2M',
  selection: 'auto' as const,
  framerate: 30 as const,
};

function makeFakeTrack() {
  return {
    addEventListener: vi.fn(),
    stop: vi.fn(),
  } as unknown as MediaStreamTrack;
}

async function flush(): Promise<void> {
  await new Promise((r) => setTimeout(r, 0));
}

describe('BroadcastWorkerCore start', () => {
  it('builds the pipeline, applies the ladder, posts started on resolve', async () => {
    const { core, events, fake, captured } = makeCore();
    core.start(START_PARAMS);
    expect(captured[0].broadcastId).toBe('K7XQ2M');
    expect(fake.pipeline.setLadder).toHaveBeenCalledWith('auto', 30);
    expect(fake.pipeline.start).toHaveBeenCalled();
    fake.resolveStart();
    await flush();
    expect(events).toContainEqual({ type: 'started' });
  });

  it('forwards encoder settings on start and via the live command (R13 L3)', () => {
    const { core, fake } = makeCore();
    const settings = { hwPreference: 'software' as const, bitrateOverride: 2_000_000, codecOverride: null };
    core.start({ ...START_PARAMS, encoderSettings: settings });
    expect(fake.pipeline.setEncoderSettings).toHaveBeenCalledWith(settings);
    const changed = { ...settings, codecOverride: 'vp8' };
    core.setEncoderSettings(changed);
    expect(fake.pipeline.setEncoderSettings).toHaveBeenCalledWith(changed);
  });

  it('accepts the R13 FramerateSelection auto on both ladder paths', () => {
    const { core, fake } = makeCore();
    core.start({ ...START_PARAMS, framerate: 'auto' });
    expect(fake.pipeline.setLadder).toHaveBeenCalledWith('auto', 'auto');
    core.setLadder(720, 'auto');
    expect(fake.pipeline.setLadder).toHaveBeenCalledWith(720, 'auto');
  });

  it('preserves BroadcastStartError.phase in startError', async () => {
    const { core, events, fake } = makeCore();
    core.start(START_PARAMS);
    fake.rejectStart(new BroadcastStartError('connect', new Error('409 publisher exists')));
    await flush();
    expect(events).toContainEqual({
      type: 'startError',
      phase: 'connect',
      message: '409 publisher exists',
    });
  });

  it('marks non-BroadcastStartError failures with phase null', async () => {
    const { core, events, fake } = makeCore();
    core.start(START_PARAMS);
    fake.rejectStart(new Error('something exotic'));
    await flush();
    expect(events).toContainEqual({ type: 'startError', phase: null, message: 'something exotic' });
  });
});

describe('BroadcastWorkerCore capture handshake', () => {
  it('posts awaitingCapture when the media factory runs, resolves it on capture()', async () => {
    const { core, events, captured } = makeCore();
    core.start(START_PARAMS);
    expect(events).not.toContainEqual({ type: 'awaitingCapture' });

    const sourcePromise = captured[0].mediaSource(START_PARAMS.config);
    expect(events).toContainEqual({ type: 'awaitingCapture' });

    const track = makeFakeTrack();
    core.capture(track, 60);
    const source = await sourcePromise;
    expect(source.capturePath).toBe('mstp-worker');
    expect(source.stream).toBeNull();
    expect(source.nativeFps).toBe(60);
    // The ended signal must wire to the transferred track.
    const onEnded = vi.fn();
    source.onEnded(onEnded);
    expect(track.addEventListener).toHaveBeenCalledWith('ended', onEnded);
  });

  it('rejects the media factory on captureFailed', async () => {
    const { core, captured } = makeCore();
    core.start(START_PARAMS);
    const sourcePromise = captured[0].mediaSource(START_PARAMS.config);
    core.captureFailed('user cancelled the picker');
    await expect(sourcePromise).rejects.toThrow('user cancelled the picker');
  });

  it('stops a stray track arriving with no pending capture', () => {
    const { core } = makeCore();
    const track = makeFakeTrack();
    core.capture(track, null);
    expect(track.stop).toHaveBeenCalled();
  });
});

describe('BroadcastWorkerCore event mapping', () => {
  it('forwards pipeline callbacks as events', () => {
    const { core, events, captured } = makeCore();
    core.start(START_PARAMS);
    const cbs = captured[0].cbs;

    cbs.onCapturePathChosen('mstp-worker');
    const info = { codec: 'avc1.42E02A' } as EncoderConfigured;
    cbs.onEncoderConfigured(info);
    cbs.onBroadcastId?.('K7XQ2M');
    const stats = { encodedFrames: 7 } as BroadcastStats;
    cbs.onStats(stats);
    cbs.onError(new Error('mid-stream failure'));
    cbs.onEnded();

    expect(events).toContainEqual({ type: 'capturePath', path: 'mstp-worker' });
    expect(events).toContainEqual({ type: 'encoderConfigured', info });
    expect(events).toContainEqual({ type: 'broadcastId', id: 'K7XQ2M' });
    expect(events).toContainEqual({ type: 'stats', stats });
    expect(events).toContainEqual({ type: 'error', message: 'mid-stream failure' });
    expect(events).toContainEqual({ type: 'ended' });
  });

  it('drops events from a superseded generation', async () => {
    const { core, events, fake, captured } = makeCore();
    core.start(START_PARAMS);
    const first = captured[0].cbs;
    core.start(START_PARAMS); // supersedes; also stops the old pipeline
    expect(fake.pipeline.stop).toHaveBeenCalled();

    first.onStats({ encodedFrames: 1 } as BroadcastStats);
    first.onEnded();
    expect(events.filter((e) => e.type === 'stats')).toHaveLength(0);
    expect(events.filter((e) => e.type === 'ended')).toHaveLength(0);
  });
});

describe('BroadcastWorkerCore stop', () => {
  it('stops the pipeline and always answers with ended', async () => {
    const { core, events, fake } = makeCore();
    core.start(START_PARAMS);
    await core.stop();
    expect(fake.pipeline.stop).toHaveBeenCalled();
    expect(events.filter((e) => e.type === 'ended')).toHaveLength(1);
  });

  it('rejects a pending capture wait on stop', async () => {
    const { core, captured } = makeCore();
    core.start(START_PARAMS);
    const sourcePromise = captured[0].mediaSource(START_PARAMS.config);
    await core.stop();
    await expect(sourcePromise).rejects.toThrow('broadcast stopped');
  });

  it('answers ended even when nothing was started', async () => {
    const { core, events } = makeCore();
    await core.stop();
    expect(events).toContainEqual({ type: 'ended' });
  });
});
