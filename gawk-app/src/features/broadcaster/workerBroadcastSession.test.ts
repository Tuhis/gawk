// @vitest-environment jsdom
// R11 K3 acceptance (docs/16): WorkerBroadcastSession drives a fake worker +
// injected acquire function — the capture handoff (clone transferred, original
// kept for the preview), typed start failures, local teardown, and the
// jsdom fallback of createBroadcastSession to the main-thread pipeline.

import { describe, expect, it, vi } from 'vitest';

import { createBroadcastSession, WorkerBroadcastSession, type WorkerLike } from './workerBroadcastSession';
import {
  BroadcastPipeline,
  BroadcastStartError,
  DEFAULT_ENCODER_SETTINGS,
  type BroadcastCallbacks,
  type BroadcastStats,
} from '../../transport/broadcaster';
import type { BroadcastWorkerCommand } from '../../transport/broadcast-worker-core';
import type { EncoderConfigured } from '../../media/encoder';
import { DEFAULT_CAPTURE_CONFIG } from '../../media/types';

class FakeWorker implements WorkerLike {
  posted: { msg: BroadcastWorkerCommand; transfer?: Transferable[] }[] = [];
  onmessage: ((e: MessageEvent) => void) | null = null;
  terminated = false;

  postMessage(msg: unknown, transfer?: Transferable[]): void {
    this.posted.push({ msg: msg as BroadcastWorkerCommand, transfer });
  }

  terminate(): void {
    this.terminated = true;
  }

  emit(msg: unknown): void {
    this.onmessage?.({ data: msg } as MessageEvent);
  }

  commands(): BroadcastWorkerCommand[] {
    return this.posted.map((p) => p.msg);
  }
}

function makeCallbacks() {
  return {
    onSourceStream: vi.fn<BroadcastCallbacks['onSourceStream']>(),
    onEncoderConfigured: vi.fn<BroadcastCallbacks['onEncoderConfigured']>(),
    onCapturePathChosen: vi.fn<BroadcastCallbacks['onCapturePathChosen']>(),
    onStats: vi.fn<BroadcastCallbacks['onStats']>(),
    onError: vi.fn<BroadcastCallbacks['onError']>(),
    onEnded: vi.fn<BroadcastCallbacks['onEnded']>(),
    onBroadcastId: vi.fn<NonNullable<BroadcastCallbacks['onBroadcastId']>>(),
  };
}

function makeAcquired() {
  const clone = { stop: vi.fn(), addEventListener: vi.fn() };
  const track = {
    clone: vi.fn(() => clone),
    getSettings: vi.fn(() => ({ frameRate: 60 })),
    addEventListener: vi.fn(),
    stop: vi.fn(),
  };
  const stream = { getTracks: () => [track] } as unknown as MediaStream;
  return { stream, track, clone };
}

function makeSession(overrides?: {
  acquire?: () => Promise<{ stream: MediaStream; track: MediaStreamTrack }>;
  broadcastId?: string;
}) {
  const worker = new FakeWorker();
  const cbs = makeCallbacks();
  const acquired = makeAcquired();
  const acquire =
    overrides?.acquire ??
    (() => Promise.resolve({ stream: acquired.stream, track: acquired.track as unknown as MediaStreamTrack }));
  const session = new WorkerBroadcastSession(
    worker,
    { ...DEFAULT_CAPTURE_CONFIG },
    'https://relay.test:4433',
    { certHashHex: 'ab' },
    cbs,
    overrides?.broadcastId,
    acquire,
  );
  return { session, worker, cbs, acquired };
}

async function flush(): Promise<void> {
  await new Promise((r) => setTimeout(r, 0));
}

describe('WorkerBroadcastSession start + ladder', () => {
  it('posts the start command with the buffered ladder and connect opts', () => {
    const { session, worker } = makeSession({ broadcastId: 'K7XQ2M' });
    session.setLadder(720, 30);
    void session.start().catch(() => {});
    expect(worker.commands()).toEqual([
      {
        type: 'start',
        config: { ...DEFAULT_CAPTURE_CONFIG },
        serverUrl: 'https://relay.test:4433',
        connectOpts: { certHashHex: 'ab' },
        broadcastId: 'K7XQ2M',
        selection: 720,
        framerate: 30,
        encoderSettings: DEFAULT_ENCODER_SETTINGS,
      },
    ]);
  });

  it('posts setLadder for changes after start', () => {
    const { session, worker } = makeSession();
    void session.start().catch(() => {});
    session.setLadder('auto', 60);
    expect(worker.commands()).toContainEqual({ type: 'setLadder', selection: 'auto', framerate: 60 });
  });

  it('posts setEncoderSettings for changes after start (R12 L4 — no restart)', () => {
    const { session, worker } = makeSession();
    void session.start().catch(() => {});
    const settings = { hwPreference: 'software' as const, bitrateOverride: 2_000_000, codecOverride: 'vp8' };
    session.setEncoderSettings(settings);
    expect(worker.commands()).toContainEqual({ type: 'setEncoderSettings', settings });
  });

  it('resolves start() on the started event', async () => {
    const { session, worker } = makeSession();
    const started = session.start();
    worker.emit({ type: 'started' });
    await expect(started).resolves.toBeUndefined();
  });
});

describe('WorkerBroadcastSession capture handoff', () => {
  it('acquires on awaitingCapture, transfers a clone, keeps the original for the preview', async () => {
    const { session, worker, cbs, acquired } = makeSession();
    void session.start().catch(() => {});
    worker.emit({ type: 'awaitingCapture' });
    await flush();

    const capture = worker.posted.find((p) => p.msg.type === 'capture');
    expect(capture).toBeDefined();
    expect((capture!.msg as { track: unknown }).track).toBe(acquired.clone);
    expect((capture!.msg as { nativeFps: number | null }).nativeFps).toBe(60);
    // The clone travels in the transfer list; the original never leaves.
    expect(capture!.transfer).toEqual([acquired.clone]);
    expect(cbs.onSourceStream).toHaveBeenCalledWith(acquired.stream);
    // Main-side safety net for "Stop sharing".
    expect(acquired.track.addEventListener).toHaveBeenCalledWith('ended', expect.any(Function));
  });

  it('posts captureFailed when acquisition is denied', async () => {
    const { session, worker, cbs } = makeSession({
      acquire: () => Promise.reject(new DOMException('Permission denied', 'NotAllowedError')),
    });
    void session.start().catch(() => {});
    worker.emit({ type: 'awaitingCapture' });
    await flush();
    const failed = worker.commands().find((c) => c.type === 'captureFailed');
    expect(failed).toBeDefined();
    expect((failed as { message: string }).message).toContain('Permission denied');
    expect(cbs.onSourceStream).not.toHaveBeenCalled();
  });
});

describe('WorkerBroadcastSession failure + end mapping', () => {
  it('rejects start() typed on startError and terminates the worker', async () => {
    const { session, worker } = makeSession();
    const started = session.start();
    worker.emit({ type: 'startError', phase: 'connect', message: '409 publisher exists' });
    await expect(started).rejects.toMatchObject({
      name: 'BroadcastStartError',
      phase: 'connect',
      message: '409 publisher exists',
    });
    expect(worker.terminated).toBe(true);
  });

  it('rejects start() with a plain Error when phase is null', async () => {
    const { session, worker } = makeSession();
    const started = session.start();
    worker.emit({ type: 'startError', phase: null, message: 'exotic' });
    await expect(started).rejects.toSatisfy((e: unknown) => !(e instanceof BroadcastStartError));
  });

  it('startError does not fire onEnded (start rejection is the error surface)', async () => {
    const { session, worker, cbs } = makeSession();
    const started = session.start();
    worker.emit({ type: 'startError', phase: 'capture', message: 'picker cancelled' });
    await expect(started).rejects.toThrow();
    expect(cbs.onEnded).not.toHaveBeenCalled();
  });

  it('forwards telemetry events to the callbacks', () => {
    const { session, worker, cbs } = makeSession();
    void session.start().catch(() => {});
    const info = { codec: 'avc1.42E02A' } as EncoderConfigured;
    const stats = { encodedFrames: 7 } as BroadcastStats;
    worker.emit({ type: 'capturePath', path: 'mstp-worker' });
    worker.emit({ type: 'encoderConfigured', info });
    worker.emit({ type: 'broadcastId', id: 'K7XQ2M' });
    worker.emit({ type: 'stats', stats });
    worker.emit({ type: 'error', message: 'mid-stream failure' });
    expect(cbs.onCapturePathChosen).toHaveBeenCalledWith('mstp-worker');
    expect(cbs.onEncoderConfigured).toHaveBeenCalledWith(info);
    expect(cbs.onBroadcastId).toHaveBeenCalledWith('K7XQ2M');
    expect(cbs.onStats).toHaveBeenCalledWith(stats);
    expect(cbs.onError).toHaveBeenCalledWith(expect.objectContaining({ message: 'mid-stream failure' }));
  });

  it('ended stops the preview tracks, fires onEnded, terminates the worker', async () => {
    const { session, worker, cbs, acquired } = makeSession();
    void session.start().catch(() => {});
    worker.emit({ type: 'awaitingCapture' });
    await flush();
    worker.emit({ type: 'ended' });
    expect(acquired.track.stop).toHaveBeenCalled();
    expect(cbs.onEnded).toHaveBeenCalledTimes(1);
    expect(worker.terminated).toBe(true);
  });

  it('stop() posts stop and resolves when ended arrives', async () => {
    const { session, worker, cbs, acquired } = makeSession();
    void session.start().catch(() => {});
    worker.emit({ type: 'awaitingCapture' });
    await flush();

    const stopping = session.stop();
    // Preview tracks die immediately — no lingering capture indicator.
    expect(acquired.track.stop).toHaveBeenCalled();
    expect(worker.commands()).toContainEqual({ type: 'stop' });
    worker.emit({ type: 'ended' });
    await stopping;
    expect(cbs.onEnded).toHaveBeenCalledTimes(1);
    expect(worker.terminated).toBe(true);
  });
});

describe('createBroadcastSession fallback', () => {
  it('falls back to the main-thread BroadcastPipeline without Worker/captureStream (jsdom)', async () => {
    const session = await createBroadcastSession(
      { ...DEFAULT_CAPTURE_CONFIG },
      'https://relay.test:4433',
      {},
      makeCallbacks(),
    );
    expect(session).toBeInstanceOf(BroadcastPipeline);
  });
});
