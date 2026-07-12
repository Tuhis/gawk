// Reconnect policy tests. The pipeline is faked so no WebTransport or
// WebCodecs is involved — the fake's callbacks let tests simulate session
// drops exactly as ViewerPipeline reports them (onError then onEnded).

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { ViewerCallbacks } from './viewer';
import {
  RECONNECT_MAX_ATTEMPTS,
  reconnectDelayMs,
  ViewerSession,
  type PipelineHandle,
  type ViewerSessionCallbacks,
} from './viewer-session';

class FakePipeline implements PipelineHandle {
  stopped = false;
  cbs: ViewerCallbacks;
  private startResult: 'ok' | 'fail' | 'fail-4000';

  constructor(cbs: ViewerCallbacks, startResult: 'ok' | 'fail' | 'fail-4000') {
    this.cbs = cbs;
    this.startResult = startResult;
  }

  start(): Promise<void> {
    if (this.startResult === 'ok') {
      return Promise.resolve();
    }
    const err = new Error('connect failed') as any;
    if (this.startResult === 'fail-4000') {
      err.closeCode = 4000;
    }
    return Promise.reject(err);
  }

  async stop(): Promise<void> {
    if (this.stopped) return;
    this.stopped = true;
    this.cbs.onEnded();
  }

  // Simulates the pipeline dying the way ViewerPipeline.fail() does:
  // onError with the reason, then onEnded via its own stop().
  crash(reason: string, closeCode?: number): void {
    const err = new Error(reason) as any;
    if (closeCode !== undefined) {
      err.closeCode = closeCode;
    }
    this.cbs.onError(err);
    void this.stop();
  }
}

interface Harness {
  session: ViewerSession;
  pipelines: FakePipeline[];
  events: string[];
  cb: ViewerSessionCallbacks;
}

// startResults[i] applies to the i-th created pipeline; past the end of the
// array, pipelines start successfully.
function makeHarness(startResults: ('ok' | 'fail' | 'fail-4000')[] = []): Harness {
  const pipelines: FakePipeline[] = [];
  const events: string[] = [];
  const cb: ViewerSessionCallbacks = {
    onDecodedFrame: () => {},
    onConfig: () => {},
    onStats: () => {},
    onConnected: () => events.push('connected'),
    onReconnecting: ({ attempt, delayMs }) => events.push(`reconnecting:${attempt}:${delayMs}`),
    onError: (e) => events.push(`error:${e.message}`),
    onEnded: () => events.push('ended'),
  };
  const session = new ViewerSession('https://relay.test:4433', 'ABC123', {}, cb, (_url, _id, _opts, cbs) => {
    const p = new FakePipeline(cbs, startResults[pipelines.length] ?? 'ok');
    pipelines.push(p);
    return p;
  });
  return { session, pipelines, events, cb };
}

beforeEach(() => {
  vi.useFakeTimers();
});

afterEach(() => {
  vi.useRealTimers();
});

describe('reconnectDelayMs', () => {
  it('doubles from 1s and caps at 15s', () => {
    expect([1, 2, 3, 4, 5, 6, 10].map(reconnectDelayMs)).toEqual([
      1000, 2000, 4000, 8000, 15000, 15000, 15000,
    ]);
  });
});

describe('ViewerSession', () => {
  it('propagates an initial start failure without retrying', async () => {
    const { session, pipelines, events } = makeHarness(['fail']);
    await expect(session.start()).rejects.toThrow('connect failed');
    await vi.advanceTimersByTimeAsync(60_000);
    expect(pipelines).toHaveLength(1);
    expect(events).toEqual([]);
  });

  it('reconnects after an unexpected drop, only once the delay elapses', async () => {
    const { session, pipelines, events } = makeHarness();
    await session.start();
    expect(events).toEqual(['connected']);

    pipelines[0].crash('session closed by server');
    expect(events).toEqual(['connected', 'reconnecting:1:1000']);
    expect(pipelines).toHaveLength(1); // not before the delay

    await vi.advanceTimersByTimeAsync(999);
    expect(pipelines).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(1);
    expect(pipelines).toHaveLength(2);
    expect(events).toEqual(['connected', 'reconnecting:1:1000', 'connected']);
  });

  it('resets the attempt counter after a successful reconnect', async () => {
    const { session, pipelines, events } = makeHarness();
    await session.start();
    pipelines[0].crash('drop 1');
    await vi.advanceTimersByTimeAsync(1000);

    pipelines[1].crash('drop 2');
    // A fresh drop after a successful reconnect starts over at attempt 1.
    expect(events.at(-1)).toBe('reconnecting:1:1000');
  });

  it('backs off exponentially across consecutive failed attempts', async () => {
    const { session, pipelines, events } = makeHarness(['ok', 'fail', 'fail', 'ok']);
    await session.start();
    pipelines[0].crash('drop');
    await vi.advanceTimersByTimeAsync(1000); // attempt 1 fails
    await vi.advanceTimersByTimeAsync(2000); // attempt 2 fails
    await vi.advanceTimersByTimeAsync(4000); // attempt 3 connects
    expect(events).toEqual([
      'connected',
      'reconnecting:1:1000',
      'reconnecting:2:2000',
      'reconnecting:3:4000',
      'connected',
    ]);
  });

  it('gives up with onError after the reconnect budget is exhausted', async () => {
    const failures = Array.from({ length: RECONNECT_MAX_ATTEMPTS }, () => 'fail' as const);
    const { session, pipelines, events } = makeHarness(['ok', ...failures]);
    await session.start();
    pipelines[0].crash('gone');

    for (let i = 0; i < RECONNECT_MAX_ATTEMPTS; i++) {
      await vi.advanceTimersByTimeAsync(15_000);
    }
    await vi.advanceTimersByTimeAsync(120_000);

    expect(pipelines).toHaveLength(1 + RECONNECT_MAX_ATTEMPTS);
    expect(events.filter((e) => e.startsWith('reconnecting'))).toHaveLength(RECONNECT_MAX_ATTEMPTS);
    expect(events.at(-1)).toBe(`error:reconnect failed after ${RECONNECT_MAX_ATTEMPTS} attempts: connect failed`);
  });

  it('stop during backoff cancels the pending attempt and ends cleanly', async () => {
    const { session, pipelines, events } = makeHarness();
    await session.start();
    pipelines[0].crash('drop');
    expect(events.at(-1)).toBe('reconnecting:1:1000');

    await session.stop();
    expect(events.at(-1)).toBe('ended');

    await vi.advanceTimersByTimeAsync(60_000);
    expect(pipelines).toHaveLength(1); // no new pipeline after stop
  });

  it('user stop of a healthy session forwards a single onEnded', async () => {
    const { session, pipelines, events } = makeHarness();
    await session.start();
    await session.stop();
    expect(events).toEqual(['connected', 'ended']);
    expect(pipelines[0].stopped).toBe(true);

    await session.stop(); // idempotent
    expect(events).toEqual(['connected', 'ended']);
  });

  it('stops reconnecting and ends cleanly when closed with code 4000', async () => {
    const { session, pipelines, events } = makeHarness();
    await session.start();
    expect(events).toEqual(['connected']);

    pipelines[0].crash('broadcast ended', 4000);
    expect(events).toEqual(['connected', 'ended']);
    expect(pipelines).toHaveLength(1);
  });

  it('stops reconnecting and ends cleanly when reconnect start rejects with code 4000', async () => {
    const { session, pipelines, events } = makeHarness(['ok', 'fail-4000']);
    await session.start();

    pipelines[0].crash('drop');
    expect(events).toEqual(['connected', 'reconnecting:1:1000']);

    await vi.advanceTimersByTimeAsync(1000);
    expect(events).toEqual(['connected', 'reconnecting:1:1000', 'ended']);
    expect(pipelines).toHaveLength(2);
  });
});
