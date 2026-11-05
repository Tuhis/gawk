// @vitest-environment jsdom
// WorkerBroadcastSession against a fake worker + injected acquire function —
// the capture handoff (clone transferred, original kept for the preview),
// typed start failures, local teardown, and the jsdom fallback of
// createBroadcastSession to the main-thread pipeline.

import { afterEach, describe, expect, it, vi } from 'vitest';

import {
  createBroadcastSession,
  tryCreateBroadcastWorker,
  WORKER_SILENCE_MS,
  WorkerBroadcastSession,
  type WorkerLike,
} from './workerBroadcastSession';
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
  onerror: ((e: ErrorEvent) => void) | null = null;
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

  crash(message: string): void {
    this.onerror?.({ message, preventDefault: () => {} } as ErrorEvent);
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

function makeAcquired(withAudio = false) {
  const clone = { stop: vi.fn(), addEventListener: vi.fn() };
  const track = {
    clone: vi.fn(() => clone),
    getSettings: vi.fn(() => ({ frameRate: 60 })),
    addEventListener: vi.fn(),
    stop: vi.fn(),
  };
  const audioClone = { stop: vi.fn(), addEventListener: vi.fn() };
  const audioTrack = {
    clone: vi.fn(() => audioClone),
    addEventListener: vi.fn(),
    stop: vi.fn(),
  };
  const audioTracks = withAudio ? [audioTrack] : [];
  const stream = {
    getTracks: () => [track, ...audioTracks],
    getAudioTracks: () => audioTracks,
  } as unknown as MediaStream;
  return { stream, track, clone, audioTrack, audioClone };
}

function makeSession(overrides?: {
  acquire?: () => Promise<{ stream: MediaStream; track: MediaStreamTrack }>;
  broadcastId?: string;
  audio?: boolean;
  acquiredAudio?: boolean;
  isHidden?: () => boolean;
}) {
  const worker = new FakeWorker();
  const cbs = makeCallbacks();
  const acquired = makeAcquired(overrides?.acquiredAudio ?? false);
  const acquire =
    overrides?.acquire ??
    (() => Promise.resolve({ stream: acquired.stream, track: acquired.track as unknown as MediaStreamTrack }));
  const session = new WorkerBroadcastSession(
    worker,
    { ...DEFAULT_CAPTURE_CONFIG, ...(overrides?.audio ? { audio: true } : {}) },
    'https://relay.test:4433',
    { certHashHex: 'ab' },
    cbs,
    overrides?.broadcastId,
    acquire,
    { isHidden: overrides?.isHidden ?? (() => false) },
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

  it('posts setEncoderSettings for changes after start (R13 L4 — no restart)', () => {
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

  // The audio clone transfers beside the video clone when the config
  // asked for audio and the grant delivered a track.
  it('transfers the audio clone alongside the video clone when audio is enabled', async () => {
    const { session, worker, acquired } = makeSession({ audio: true, acquiredAudio: true });
    void session.start().catch(() => {});
    worker.emit({ type: 'awaitingCapture' });
    await flush();

    const capture = worker.posted.find((p) => p.msg.type === 'capture');
    expect(capture).toBeDefined();
    expect((capture!.msg as { track: unknown }).track).toBe(acquired.clone);
    expect((capture!.msg as { audioTrack: unknown }).audioTrack).toBe(acquired.audioClone);
    expect(capture!.transfer).toEqual([acquired.clone, acquired.audioClone]);
  });

  it('audio enabled but no audio track in the grant transfers video only', async () => {
    const { session, worker } = makeSession({ audio: true, acquiredAudio: false });
    void session.start().catch(() => {});
    worker.emit({ type: 'awaitingCapture' });
    await flush();

    const capture = worker.posted.find((p) => p.msg.type === 'capture');
    expect(capture).toBeDefined();
    expect((capture!.msg as { audioTrack: unknown }).audioTrack).toBeNull();
    expect(capture!.transfer).toHaveLength(1);
  });

  it('audio disabled never inspects or clones audio tracks', async () => {
    const { session, worker, acquired } = makeSession({ audio: false, acquiredAudio: true });
    void session.start().catch(() => {});
    worker.emit({ type: 'awaitingCapture' });
    await flush();

    const capture = worker.posted.find((p) => p.msg.type === 'capture');
    expect(capture).toBeDefined();
    expect((capture!.msg as { audioTrack: unknown }).audioTrack).toBeNull();
    expect(acquired.audioTrack.clone).not.toHaveBeenCalled();
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

// docs/79 D9: an end stop says so to the worker; a plain one is unchanged.
describe('WorkerBroadcastSession end stop (R78)', () => {
  it('stop({ end: true }) posts an end stop', async () => {
    const { session, worker } = makeSession();
    void session.start().catch(() => {});
    worker.emit({ type: 'awaitingCapture' });
    await flush();
    const stopping = session.stop({ end: true });
    expect(worker.commands()).toContainEqual({ type: 'stop', end: true });
    worker.emit({ type: 'ended' });
    await stopping;
  });

  it('a plain stop posts the plain command', async () => {
    const { session, worker } = makeSession();
    void session.start().catch(() => {});
    worker.emit({ type: 'awaitingCapture' });
    await flush();
    const stopping = session.stop();
    const stops = worker.commands().filter((c) => c.type === 'stop');
    expect(stops).toEqual([{ type: 'stop' }]);
    expect('end' in stops[0]).toBe(false);
    worker.emit({ type: 'ended' });
    await stopping;
  });
});

// BUGS.md "Broadcaster screen shows LIVE indefinitely if the broadcast worker
// dies silently": a killed worker posts nothing, so the session watches the
// pipeline's 500 ms stats heartbeat; an uncaught worker exception arrives on
// onerror and ends the broadcast the same way.
describe('WorkerBroadcastSession worker liveness', () => {
  afterEach(() => {
    vi.useRealTimers();
  });

  const stats = { encodedFrames: 1 } as BroadcastStats;

  // Live session: started, heartbeat flowing.
  function goLive(overrides?: Parameters<typeof makeSession>[0]) {
    vi.useFakeTimers();
    const made = makeSession(overrides);
    void made.session.start().catch(() => {});
    made.worker.emit({ type: 'started' });
    made.worker.emit({ type: 'stats', stats });
    return made;
  }

  // The worker keeps posting stats every 500 ms for `ms`.
  function beat(worker: FakeWorker, ms: number): void {
    for (let t = 0; t < ms; t += 500) {
      vi.advanceTimersByTime(500);
      worker.emit({ type: 'stats', stats });
    }
  }

  it('a silent worker surfaces an error, terminates, then fires onEnded', () => {
    const { worker, cbs } = goLive();
    vi.advanceTimersByTime(WORKER_SILENCE_MS + 1500);
    expect(cbs.onError).toHaveBeenCalledTimes(1);
    expect(cbs.onError.mock.calls[0][0].message).toMatch(/stopped responding/i);
    expect(worker.terminated).toBe(true);
    expect(cbs.onEnded).toHaveBeenCalledTimes(1);
    // onError lands before onEnded, so the screen keeps the error card.
    expect(cbs.onError.mock.invocationCallOrder[0]).toBeLessThan(cbs.onEnded.mock.invocationCallOrder[0]);
    // Fires once.
    vi.advanceTimersByTime(WORKER_SILENCE_MS * 3);
    expect(cbs.onError).toHaveBeenCalledTimes(1);
    expect(cbs.onEnded).toHaveBeenCalledTimes(1);
  });

  it('stops the preview tracks when the watchdog fires', async () => {
    vi.useFakeTimers();
    const { session, worker, acquired } = makeSession();
    void session.start().catch(() => {});
    worker.emit({ type: 'awaitingCapture' });
    await vi.advanceTimersByTimeAsync(0);
    worker.emit({ type: 'started' });
    vi.advanceTimersByTime(WORKER_SILENCE_MS + 1500);
    expect(acquired.track.stop).toHaveBeenCalled();
  });

  it('a steady heartbeat never fires', () => {
    const { worker, cbs } = goLive();
    beat(worker, WORKER_SILENCE_MS * 4);
    expect(cbs.onError).not.toHaveBeenCalled();
    expect(cbs.onEnded).not.toHaveBeenCalled();
    expect(worker.terminated).toBe(false);
  });

  it('does not arm before started (the share picker can stay open indefinitely)', () => {
    vi.useFakeTimers();
    const { session, worker, cbs } = makeSession();
    void session.start().catch(() => {});
    worker.emit({ type: 'awaitingCapture' });
    vi.advanceTimersByTime(WORKER_SILENCE_MS * 4);
    expect(cbs.onError).not.toHaveBeenCalled();
    expect(cbs.onEnded).not.toHaveBeenCalled();
    expect(worker.terminated).toBe(false);
  });

  it('does not fire while stopping (the worker is tearing down; stop() owns the timeout)', async () => {
    const { session, worker, cbs } = goLive();
    void session.stop();
    // STOP_TIMEOUT_MS (3 s) ends it, once; the watchdog adds no error.
    await vi.advanceTimersByTimeAsync(WORKER_SILENCE_MS * 4);
    expect(cbs.onError).not.toHaveBeenCalled();
    expect(cbs.onEnded).toHaveBeenCalledTimes(1);
    expect(worker.terminated).toBe(true);
  });

  it('does not judge while the tab is hidden, and gives a fresh window on return', () => {
    let hidden = false;
    const { worker, cbs } = goLive({ isHidden: () => hidden });
    hidden = true;
    // A hidden tab may throttle the worker's own timers: no verdict.
    vi.advanceTimersByTime(WORKER_SILENCE_MS * 6);
    expect(cbs.onError).not.toHaveBeenCalled();
    hidden = false;
    // Visible again: the silence window starts now, not at the last stats.
    vi.advanceTimersByTime(WORKER_SILENCE_MS - 1000);
    expect(cbs.onError).not.toHaveBeenCalled();
    worker.emit({ type: 'stats', stats });
    beat(worker, WORKER_SILENCE_MS * 2);
    expect(cbs.onError).not.toHaveBeenCalled();
    // A worker that is really dead is caught once visible.
    vi.advanceTimersByTime(WORKER_SILENCE_MS + 1500);
    expect(cbs.onError).toHaveBeenCalledTimes(1);
    expect(cbs.onEnded).toHaveBeenCalledTimes(1);
  });

  it('a stalled main thread (late watchdog tick, e.g. system sleep) resets the window instead of firing', () => {
    const { worker, cbs } = goLive();
    // The clock jumps without the interval having run (sleep / frozen page).
    vi.setSystemTime(Date.now() + WORKER_SILENCE_MS * 10);
    vi.advanceTimersByTime(1000);
    expect(cbs.onError).not.toHaveBeenCalled();
    // The worker's queued stats arrive right after; heartbeat continues.
    worker.emit({ type: 'stats', stats });
    beat(worker, WORKER_SILENCE_MS * 2);
    expect(cbs.onError).not.toHaveBeenCalled();
    expect(cbs.onEnded).not.toHaveBeenCalled();
  });

  it('stops watching after the worker ends the broadcast itself', () => {
    const { worker, cbs } = goLive();
    worker.emit({ type: 'ended' });
    vi.advanceTimersByTime(WORKER_SILENCE_MS * 4);
    expect(cbs.onError).not.toHaveBeenCalled();
    expect(cbs.onEnded).toHaveBeenCalledTimes(1);
  });

  it('an uncaught worker exception after start maps to onError + teardown + onEnded', () => {
    const { worker, cbs } = goLive();
    worker.crash('Uncaught TypeError: boom');
    expect(cbs.onError).toHaveBeenCalledWith(expect.objectContaining({ message: expect.stringContaining('boom') }));
    expect(worker.terminated).toBe(true);
    expect(cbs.onEnded).toHaveBeenCalledTimes(1);
    vi.advanceTimersByTime(WORKER_SILENCE_MS * 4);
    expect(cbs.onError).toHaveBeenCalledTimes(1);
  });

  it('an uncaught worker exception during start rejects start() without onEnded', async () => {
    const { session, worker, cbs } = makeSession();
    const started = session.start();
    worker.crash('Uncaught TypeError: boom');
    await expect(started).rejects.toThrow(/boom/);
    expect(worker.terminated).toBe(true);
    expect(cbs.onEnded).not.toHaveBeenCalled();
    expect(cbs.onError).not.toHaveBeenCalled();
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

  it('records the rejecting gate on the fallback pipeline\'s stats', async () => {
    const session = await createBroadcastSession({ ...DEFAULT_CAPTURE_CONFIG }, 'https://relay.test:4433', {}, makeCallbacks());
    expect(session).toBeInstanceOf(BroadcastPipeline);
    expect((session as BroadcastPipeline).getPipelineFallbackReason()).toBe('Web Workers unavailable');
  });
});

// A worker whose boot handshake the test scripts: `boot` is emitted on the
// next macrotask after the session attaches its onmessage.
class FakeBootWorker implements WorkerLike {
  onmessage: ((e: MessageEvent) => void) | null = null;
  onerror: ((e: ErrorEvent) => void) | null = null;
  terminated = false;
  posted: unknown[] = [];
  postMessage(msg: unknown): void {
    this.posted.push(msg);
  }
  terminate(): void {
    this.terminated = true;
  }
  emit(msg: unknown): void {
    this.onmessage?.({ data: msg } as MessageEvent);
  }
}

// One test per gate of tryCreateBroadcastWorker (docs/16 Decision 1): each
// rejection names itself, so a fallback is never silent.
describe('tryCreateBroadcastWorker fallback reasons', () => {
  function deps(worker: FakeBootWorker, over: Partial<Parameters<typeof tryCreateBroadcastWorker>[0]> = {}) {
    return {
      mainThreadGate: () => null,
      spawn: () => worker,
      probeTransfer: () => null,
      bootTimeoutMs: 50,
      ...over,
    };
  }

  it('gate 1: names the missing main-thread capability', async () => {
    const res = await tryCreateBroadcastWorker(
      deps(new FakeBootWorker(), { mainThreadGate: () => 'canvas.captureStream unavailable' }),
    );
    expect(res).toEqual({ worker: null, reason: 'canvas.captureStream unavailable' });
  });

  it('gate 1 (real check): jsdom has no Worker', async () => {
    const res = await tryCreateBroadcastWorker({ spawn: () => new FakeBootWorker() });
    expect(res).toEqual({ worker: null, reason: 'Web Workers unavailable' });
  });

  it('gate 2: a worker that fails to construct says so', async () => {
    const res = await tryCreateBroadcastWorker(
      deps(new FakeBootWorker(), {
        // new Worker throws synchronously only for a bad URL or a
        // cross-origin SecurityError; a 404/MIME/CSP failure arrives later
        // as a message-less error event (gate 3d).
        spawn: () => {
          throw new Error('cross-origin worker script');
        },
      }),
    );
    expect(res).toEqual({ worker: null, reason: 'broadcast worker failed to load: cross-origin worker script' });
  });

  it('gate 3a: boot reporting supported:false names the APIs missing in worker scope', async () => {
    const w = new FakeBootWorker();
    const p = tryCreateBroadcastWorker(deps(w));
    w.emit({ type: 'boot', supported: false, missing: ['MediaStreamTrackProcessor'] });
    expect(await p).toEqual({ worker: null, reason: 'MediaStreamTrackProcessor unavailable in worker scope' });
    expect(w.terminated).toBe(true);
  });

  it('gate 3b: a boot that never arrives is a timeout', async () => {
    const w = new FakeBootWorker();
    const res = await tryCreateBroadcastWorker(deps(w, { bootTimeoutMs: 5 }));
    expect(res).toEqual({ worker: null, reason: 'broadcast worker boot timed out after 5 ms' });
    expect(w.terminated).toBe(true);
  });

  it('gate 3c: a worker script error before boot says it failed to start', async () => {
    const w = new FakeBootWorker();
    const p = tryCreateBroadcastWorker(deps(w));
    w.onerror?.({ message: 'SyntaxError: bad import' } as ErrorEvent);
    expect(await p).toEqual({ worker: null, reason: 'broadcast worker failed to start: SyntaxError: bad import' });
  });

  it('gate 3d: a message-less error event (404, MIME, CSP worker-src) reads as load-or-start', async () => {
    const w = new FakeBootWorker();
    const p = tryCreateBroadcastWorker(deps(w));
    w.onerror?.({} as ErrorEvent);
    expect(await p).toEqual({ worker: null, reason: 'broadcast worker failed to load or start' });
  });

  it('gate 4: a track-transfer failure names itself', async () => {
    const w = new FakeBootWorker();
    const p = tryCreateBroadcastWorker(deps(w, { probeTransfer: () => 'MediaStreamTrack not transferable (DataCloneError)' }));
    w.emit({ type: 'boot', supported: true, missing: [] });
    expect(await p).toEqual({ worker: null, reason: 'MediaStreamTrack not transferable (DataCloneError)' });
    expect(w.terminated).toBe(true);
  });

  // The real probe, against a stubbed captureStream (jsdom has none).
  describe('the real track-transfer probe', () => {
    const proto = HTMLCanvasElement.prototype as unknown as { captureStream?: unknown };
    afterEach(() => {
      delete proto.captureStream;
    });

    async function probeWith(captureStream: () => unknown, worker = new FakeBootWorker()) {
      proto.captureStream = captureStream;
      const p = tryCreateBroadcastWorker({ mainThreadGate: () => null, spawn: () => worker, bootTimeoutMs: 50 });
      worker.emit({ type: 'boot', supported: true, missing: [] });
      return { res: await p, worker };
    }

    it('a DataCloneError on postMessage is a transfer failure', async () => {
      const w = new FakeBootWorker();
      w.postMessage = () => {
        throw new DOMException('Value at index 0 does not have a transferable type', 'DataCloneError');
      };
      const { res } = await probeWith(() => ({ getVideoTracks: () => [{}] }), w);
      expect(res).toEqual({ worker: null, reason: 'MediaStreamTrack not transferable (DataCloneError)' });
    });

    it('a captureStream throw is its own gate, not a transfer failure', async () => {
      const { res } = await probeWith(() => {
        throw new DOMException('no', 'NotSupportedError');
      });
      expect(res).toEqual({ worker: null, reason: 'canvas.captureStream failed (NotSupportedError)' });
    });

    it('a track that transfers passes the probe', async () => {
      const { res, worker } = await probeWith(() => ({ getVideoTracks: () => [{}] }));
      expect(res).toEqual({ worker, reason: null });
      expect(worker.posted).toContainEqual(expect.objectContaining({ type: 'probeTrack' }));
    });
  });

  it('every gate passing returns the worker and no reason', async () => {
    const w = new FakeBootWorker();
    const p = tryCreateBroadcastWorker(deps(w));
    w.emit({ type: 'boot', supported: true, missing: [] });
    expect(await p).toEqual({ worker: w, reason: null });
    expect(w.terminated).toBe(false);
  });
});
