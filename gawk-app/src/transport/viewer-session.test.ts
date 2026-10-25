// Reconnect policy tests. The pipeline is faked so no WebTransport or
// WebCodecs is involved — the fake's callbacks let tests simulate session
// drops exactly as ViewerPipeline reports them (onError then onEnded).

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { ConnectOptions } from './connection';
import type { ViewerCallbacks } from './viewer';
import {
  ABRUPT_DROP_RETRY_DELAY_MS,
  RECONNECT_MAX_ATTEMPTS,
  reconnectDelayMs,
  ViewerSession,
  type PipelineHandle,
  type ViewerEndReason,
  type ViewerSessionCallbacks,
} from './viewer-session';
import {
  BROADCASTER_STOPPED_RETRY_MS,
  POP_UPLINK_FULL_RETRY_MS,
  isBroadcasterStopped,
  isTerminalPublisherClose,
  isTerminalViewerClose,
} from './reconnect';
import {
  CLOSE_CODE_BROADCAST_ENDED,
  CLOSE_CODE_BROADCASTER_STOPPED,
  CLOSE_CODE_POP_UPLINK_FULL,
  CLOSE_CODE_PUBLISHER_SUPERSEDED,
  CLOSE_CODE_SERVER_DRAINING,
  CLOSE_CODE_TERMINATED_BY_OPERATOR,
} from './wire';

class FakePipeline implements PipelineHandle {
  stopped = false;
  cbs: ViewerCallbacks;
  private startResult: StartResult;

  constructor(cbs: ViewerCallbacks, startResult: StartResult) {
    this.cbs = cbs;
    this.startResult = startResult;
  }

  // Settles a 'hold' start; undefined resolves, anything else rejects.
  settleStart: (err?: unknown) => void = () => {};

  start(): Promise<void> {
    if (this.startResult === 'ok') {
      return Promise.resolve();
    }
    if (this.startResult === 'hold') {
      return new Promise((resolve, reject) => {
        this.settleStart = (err) => (err === undefined ? resolve() : reject(err));
      });
    }
    const err = new Error('connect failed') as any;
    if (this.startResult === 'fail-4000') {
      err.closeCode = CLOSE_CODE_BROADCAST_ENDED;
    }
    if (this.startResult === 'fail-4006') {
      err.closeCode = CLOSE_CODE_TERMINATED_BY_OPERATOR;
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

  // A non-recoverable failure, the way ViewerPipeline.failDecode() reports it.
  crashFatal(reason: string): void {
    const err = new Error(reason) as any;
    err.fatal = true;
    this.cbs.onError(err);
    void this.stop();
  }
}

// A pipeline's start() outcome: connect, fail without a code, or fail with a
// terminal close code the relay sent before the dial completed.
type StartResult = 'ok' | 'fail' | 'fail-4000' | 'fail-4006' | 'hold';

interface Harness {
  session: ViewerSession;
  pipelines: FakePipeline[];
  events: string[];
  // The reason of every onEnded, in order — kept beside `events` so the
  // existing ordering assertions stay readable.
  endReasons: ViewerEndReason[];
  cb: ViewerSessionCallbacks;
}

// startResults[i] applies to the i-th created pipeline; past the end of the
// array, pipelines start successfully. `random` is the session's jitter
// source (docs/79 D7), fixed here so the waits are exact.
function makeHarness(startResults: StartResult[] = [], random: () => number = () => 0): Harness {
  const pipelines: FakePipeline[] = [];
  const events: string[] = [];
  const endReasons: ViewerEndReason[] = [];
  const cb: ViewerSessionCallbacks = {
    onDecodedFrame: () => {},
    onConfig: () => {},
    onStats: () => {},
    onConnected: () => events.push('connected'),
    onReconnecting: ({ attempt, delayMs }) => events.push(`reconnecting:${attempt}:${delayMs}`),
    onError: (e) => events.push(`error:${e.message}`),
    onEnded: (reason) => {
      events.push('ended');
      endReasons.push(reason);
    },
    onStopped: () => events.push('stopped'),
  };
  const session = new ViewerSession(
    'https://relay.test:4433',
    'ABC123',
    {},
    cb,
    (_url, _id, _opts, cbs) => {
      const p = new FakePipeline(cbs, startResults[pipelines.length] ?? 'ok');
      pipelines.push(p);
      return p;
    },
    random,
  );
  return { session, pipelines, events, endReasons, cb };
}

beforeEach(() => {
  vi.useFakeTimers();
});

afterEach(() => {
  vi.useRealTimers();
});

describe('isBroadcasterStopped', () => {
  it('is 4014 and nothing else', () => {
    expect(isBroadcasterStopped(CLOSE_CODE_BROADCASTER_STOPPED)).toBe(true);
    for (const code of [4000, 4002, 4006, 4012, 0]) expect(isBroadcasterStopped(code)).toBe(false);
    expect(isBroadcasterStopped(null)).toBe(false);
    expect(isBroadcasterStopped(undefined)).toBe(false);
  });
});

describe('reconnectDelayMs', () => {
  it('doubles from 1s and caps at 15s (coded non-drain close)', () => {
    // 4001 stands in for any coded, non-drain close — the ladder base case.
    expect([1, 2, 3, 4, 5, 6, 10].map((a) => reconnectDelayMs(a, 4001))).toEqual([
      1000, 2000, 4000, 8000, 15000, 15000, 15000,
    ]);
  });

  // The close-code-aware first-retry table.
  it('waits 20 s after 4012 on every attempt', () => {
    for (const attempt of [1, 2, 5, 10]) {
      expect(reconnectDelayMs(attempt, CLOSE_CODE_POP_UPLINK_FULL)).toBe(20_000);
    }
  });

  it('applies the fast first-retry policy per close code', () => {
    // 4002 planned drain ⇒ reconnect now.
    expect(reconnectDelayMs(1, CLOSE_CODE_SERVER_DRAINING)).toBe(0);
    // Abrupt drop (no close code — crash / stateless reset) ⇒ ≤ 250 ms.
    expect(reconnectDelayMs(1, null)).toBe(ABRUPT_DROP_RETRY_DELAY_MS);
    expect(reconnectDelayMs(1, undefined)).toBe(ABRUPT_DROP_RETRY_DELAY_MS);
    expect(ABRUPT_DROP_RETRY_DELAY_MS).toBeLessThanOrEqual(250);
    // Coded, non-drain closes (4001 eviction, 429/500) keep the 1 s ladder.
    expect(reconnectDelayMs(1, 4001)).toBe(1000);
    expect(reconnectDelayMs(1, 429)).toBe(1000);
    // The fast path is first-attempt-only: attempt 2+ is the ladder.
    expect(reconnectDelayMs(2, CLOSE_CODE_SERVER_DRAINING)).toBe(2000);
    expect(reconnectDelayMs(2, null)).toBe(2000);
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

  it('reconnects after an abrupt drop after the fast 250ms retry delay', async () => {
    const { session, pipelines, events } = makeHarness();
    await session.start();
    expect(events).toEqual(['connected']);

    // No close code — an abrupt drop (crash / stateless reset): fast retry.
    pipelines[0].crash('session closed by server');
    expect(events).toEqual(['connected', `reconnecting:1:${ABRUPT_DROP_RETRY_DELAY_MS}`]);
    expect(pipelines).toHaveLength(1); // not before the delay

    await vi.advanceTimersByTimeAsync(ABRUPT_DROP_RETRY_DELAY_MS - 1);
    expect(pipelines).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(1);
    expect(pipelines).toHaveLength(2);
    expect(events).toEqual(['connected', `reconnecting:1:${ABRUPT_DROP_RETRY_DELAY_MS}`, 'connected']);
  });

  it('marks every pipeline after the first as a rejoin (R59)', async () => {
    const seen: ConnectOptions[] = [];
    const pipelines: FakePipeline[] = [];
    const base: ConnectOptions = { deliveryMode: 'reliable' };
    const session = new ViewerSession('https://relay.test:4433', 'ABC123', base, makeHarness().cb, (_u, _id, opts, cbs) => {
      seen.push(opts);
      const p = new FakePipeline(cbs, 'ok');
      pipelines.push(p);
      return p;
    });
    await session.start();
    pipelines[0].crash('session closed by server');
    await vi.advanceTimersByTimeAsync(ABRUPT_DROP_RETRY_DELAY_MS);
    expect(pipelines).toHaveLength(2);

    expect(seen[0]).toBe(base); // the first dial is untouched: no rejoin
    expect(seen[0].rejoin).toBeUndefined();
    expect(seen[1]).toEqual({ deliveryMode: 'reliable', rejoin: true });
    expect(base.rejoin).toBeUndefined(); // the caller's options are not mutated
    await session.stop();
  });

  it('reconnects immediately (0ms) after a 4002 server drain', async () => {
    const { session, pipelines, events } = makeHarness();
    await session.start();

    pipelines[0].crash('server draining', CLOSE_CODE_SERVER_DRAINING);
    expect(events).toEqual(['connected', 'reconnecting:1:0']);

    await vi.advanceTimersByTimeAsync(0);
    expect(pipelines).toHaveLength(2);
    expect(events).toEqual(['connected', 'reconnecting:1:0', 'connected']);
  });

  it('keeps the 1s ladder for a coded non-drain close (4001 eviction)', async () => {
    const { session, pipelines, events } = makeHarness();
    await session.start();

    pipelines[0].crash('subscriber unresponsive', 4001);
    expect(events).toEqual(['connected', 'reconnecting:1:1000']);

    await vi.advanceTimersByTimeAsync(999);
    expect(pipelines).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(1);
    expect(pipelines).toHaveLength(2);
  });

  // docs/76 item 28: 4012 PopUplinkFull is temporary. The viewer retries
  // every 20 s for as long as it is refused, and the refusals never spend
  // the reconnect budget: the slot frees when another outside viewer leaves.
  it('retries every 20 s after 4012, without spending the reconnect budget', async () => {
    const { session, pipelines, events } = makeHarness();
    await session.start();
    for (let i = 0; i < RECONNECT_MAX_ATTEMPTS + 2; i++) {
      pipelines[i].crash('uplink full', CLOSE_CODE_POP_UPLINK_FULL);
      expect(events.at(-1)).toBe(`reconnecting:1:${POP_UPLINK_FULL_RETRY_MS}`);
      await vi.advanceTimersByTimeAsync(POP_UPLINK_FULL_RETRY_MS - 1);
      expect(pipelines).toHaveLength(i + 1);
      await vi.advanceTimersByTimeAsync(1);
      expect(pipelines).toHaveLength(i + 2);
    }
    expect(events.some((e) => e.startsWith('error:'))).toBe(false);
    await session.stop();
  });

  it('resets the attempt counter after a successful reconnect', async () => {
    const { session, pipelines, events } = makeHarness();
    await session.start();
    pipelines[0].crash('drop 1');
    await vi.advanceTimersByTimeAsync(1000);

    pipelines[1].crash('drop 2');
    // A fresh drop after a successful reconnect starts over at attempt 1
    // (abrupt drop ⇒ the fast 250ms first retry again).
    expect(events.at(-1)).toBe(`reconnecting:1:${ABRUPT_DROP_RETRY_DELAY_MS}`);
  });

  it('backs off exponentially across consecutive failed attempts', async () => {
    const { session, pipelines, events } = makeHarness(['ok', 'fail', 'fail', 'ok']);
    await session.start();
    pipelines[0].crash('drop');
    await vi.advanceTimersByTimeAsync(ABRUPT_DROP_RETRY_DELAY_MS); // attempt 1 (fast) fails
    await vi.advanceTimersByTimeAsync(2000); // attempt 2 fails
    await vi.advanceTimersByTimeAsync(4000); // attempt 3 connects
    expect(events).toEqual([
      'connected',
      `reconnecting:1:${ABRUPT_DROP_RETRY_DELAY_MS}`,
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
    expect(events.at(-1)).toBe(`reconnecting:1:${ABRUPT_DROP_RETRY_DELAY_MS}`);

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

  it('surfaces a fatal (unplayable-codec) error and does not reconnect', async () => {
    const { session, pipelines, events } = makeHarness();
    await session.start();
    expect(events).toEqual(['connected']);

    pipelines[0].crashFatal("Can't play this stream — codec avc1.640034");
    // Error surfaced to the UI, then a single clean end — no reconnect event.
    expect(events).toEqual([
      'connected',
      "error:Can't play this stream — codec avc1.640034",
      'ended',
    ]);

    await vi.advanceTimersByTimeAsync(60_000);
    expect(pipelines).toHaveLength(1); // never retried
    expect(events.some((e) => e.startsWith('reconnecting'))).toBe(false);
  });

  it('stops reconnecting and ends cleanly when closed with code 4000', async () => {
    const { session, pipelines, events, endReasons } = makeHarness();
    await session.start();
    expect(events).toEqual(['connected']);

    pipelines[0].crash('broadcast ended', CLOSE_CODE_BROADCAST_ENDED);
    expect(events).toEqual(['connected', 'ended']);
    expect(endReasons).toEqual(['normal']);
    expect(pipelines).toHaveLength(1);
  });

  // 4006 joins 4000 in the terminal set, and carries the distinct reason the
  // end card renders. Without both halves a kill turns into a reconnect loop
  // against the relay's ban gate.
  it('stops reconnecting and reports "moderated" when closed with code 4006', async () => {
    const { session, pipelines, events, endReasons } = makeHarness();
    await session.start();

    pipelines[0].crash('terminated by operator', CLOSE_CODE_TERMINATED_BY_OPERATOR);
    await vi.advanceTimersByTimeAsync(60_000);
    expect(events).toEqual(['connected', 'ended']);
    expect(endReasons).toEqual(['moderated']);
    // No second pipeline was ever built — nothing was retried.
    expect(pipelines).toHaveLength(1);
  });

  it('stops reconnecting and ends cleanly when reconnect start rejects with code 4000', async () => {
    const { session, pipelines, events, endReasons } = makeHarness(['ok', 'fail-4000']);
    await session.start();

    pipelines[0].crash('drop');
    const retry = `reconnecting:1:${ABRUPT_DROP_RETRY_DELAY_MS}`;
    expect(events).toEqual(['connected', retry]);

    await vi.advanceTimersByTimeAsync(ABRUPT_DROP_RETRY_DELAY_MS);
    expect(events).toEqual(['connected', retry, 'ended']);
    expect(endReasons).toEqual(['normal']);
    expect(pipelines).toHaveLength(2);
  });

  it('ends exactly once when stop() races a reconnect that the relay ends', async () => {
    const { session, pipelines, endReasons } = makeHarness(['ok', 'hold']);
    await session.start();
    pipelines[0].crash('drop');
    await vi.advanceTimersByTimeAsync(ABRUPT_DROP_RETRY_DELAY_MS);
    expect(pipelines).toHaveLength(2);

    const stopped = session.stop();
    pipelines[1].settleStart(Object.assign(new Error('ended'), { closeCode: CLOSE_CODE_BROADCAST_ENDED }));
    await stopped;
    expect(endReasons).toHaveLength(1);
  });

  // The second terminal check (the reconnect dial's own rejection) has to know
  // about 4006 too — the kill can land between two attempts.
  it('stops reconnecting when a reconnect start rejects with code 4006', async () => {
    const { session, pipelines, events, endReasons } = makeHarness(['ok', 'fail-4006']);
    await session.start();

    pipelines[0].crash('drop');
    const retry = `reconnecting:1:${ABRUPT_DROP_RETRY_DELAY_MS}`;
    await vi.advanceTimersByTimeAsync(ABRUPT_DROP_RETRY_DELAY_MS);
    expect(events).toEqual(['connected', retry, 'ended']);
    expect(endReasons).toEqual(['moderated']);

    // And nothing further is scheduled.
    await vi.advanceTimersByTimeAsync(60_000);
    expect(pipelines).toHaveLength(2);
  });
});

// docs/79 D7: a 4014 (the broadcaster pressed Stop) is "ended, waiting". The
// session reports the end once, then redials the same code every 5 s plus up
// to 1 s of jitter, forever and off the reconnect budget, until a broadcast
// is back, a terminal code ends it or the user stops.
describe('ViewerSession broadcaster stopped (R78)', () => {
  const JITTER = 300; // random() = 0.3 → 300 ms of the 1000 ms jitter
  const WAIT = BROADCASTER_STOPPED_RETRY_MS + JITTER;
  const random = () => JITTER / 1000;

  it('fires onStopped, then redials on the fixed cadence and resumes with onConnected', async () => {
    const { session, pipelines, events } = makeHarness([], random);
    await session.start();
    pipelines[0].crash('broadcaster stopped', CLOSE_CODE_BROADCASTER_STOPPED);
    expect(events).toEqual(['connected', 'stopped']);

    await vi.advanceTimersByTimeAsync(WAIT - 1);
    expect(pipelines).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(1);
    expect(pipelines).toHaveLength(2);
    // No reconnecting pill, no error: the UI shows the ended card throughout.
    expect(events).toEqual(['connected', 'stopped', 'connected']);
    await session.stop();
  });

  it('applies up to 1 s of jitter from the injected source', async () => {
    const { session, pipelines } = makeHarness([], () => 0.999);
    await session.start();
    pipelines[0].crash('broadcaster stopped', CLOSE_CODE_BROADCASTER_STOPPED);
    await vi.advanceTimersByTimeAsync(BROADCASTER_STOPPED_RETRY_MS + 998);
    expect(pipelines).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(1);
    expect(pipelines).toHaveLength(2);
    await session.stop();
  });

  it('keeps waiting through dial failures without spending the reconnect budget', async () => {
    const failures = Array.from({ length: RECONNECT_MAX_ATTEMPTS + 3 }, () => 'fail' as const);
    const { session, pipelines, events } = makeHarness(['ok', ...failures], random);
    await session.start();
    pipelines[0].crash('broadcaster stopped', CLOSE_CODE_BROADCASTER_STOPPED);
    for (let i = 0; i < failures.length; i++) {
      await vi.advanceTimersByTimeAsync(WAIT - 1);
      expect(pipelines).toHaveLength(i + 1);
      await vi.advanceTimersByTimeAsync(1);
      expect(pipelines).toHaveLength(i + 2);
    }
    // The next dial finds the restarted broadcast.
    await vi.advanceTimersByTimeAsync(WAIT);
    expect(pipelines).toHaveLength(failures.length + 2);
    expect(events).toEqual(['connected', 'stopped', 'connected']);
    await session.stop();
  });

  it('a drop after resuming is an ordinary drop again', async () => {
    const { session, pipelines, events } = makeHarness([], random);
    await session.start();
    pipelines[0].crash('broadcaster stopped', CLOSE_CODE_BROADCASTER_STOPPED);
    await vi.advanceTimersByTimeAsync(WAIT);
    pipelines[1].crash('drop');
    expect(events.at(-1)).toBe(`reconnecting:1:${ABRUPT_DROP_RETRY_DELAY_MS}`);
    await session.stop();
  });

  it('a second stop after resuming reports onStopped again', async () => {
    const { session, pipelines, events } = makeHarness([], random);
    await session.start();
    pipelines[0].crash('broadcaster stopped', CLOSE_CODE_BROADCASTER_STOPPED);
    await vi.advanceTimersByTimeAsync(WAIT);
    pipelines[1].crash('broadcaster stopped', CLOSE_CODE_BROADCASTER_STOPPED);
    expect(events).toEqual(['connected', 'stopped', 'connected', 'stopped']);
    await session.stop();
  });

  it('ends for good on 4000 during the wait', async () => {
    const { session, pipelines, events, endReasons } = makeHarness(['ok', 'fail', 'fail-4000'], random);
    await session.start();
    pipelines[0].crash('broadcaster stopped', CLOSE_CODE_BROADCASTER_STOPPED);
    await vi.advanceTimersByTimeAsync(WAIT * 2);
    expect(events).toEqual(['connected', 'stopped', 'ended']);
    expect(endReasons).toEqual(['normal']);
    await vi.advanceTimersByTimeAsync(60_000);
    expect(pipelines).toHaveLength(3);
    await session.stop();
    expect(endReasons).toHaveLength(1);
  });

  it('ends as moderated on 4006 during the wait', async () => {
    const { session, pipelines, endReasons } = makeHarness(['ok', 'fail-4006'], random);
    await session.start();
    pipelines[0].crash('broadcaster stopped', CLOSE_CODE_BROADCASTER_STOPPED);
    await vi.advanceTimersByTimeAsync(WAIT);
    expect(endReasons).toEqual(['moderated']);
    await vi.advanceTimersByTimeAsync(60_000);
    expect(pipelines).toHaveLength(2);
  });

  it('a user stop during the wait cancels it and ends once', async () => {
    const { session, pipelines, events, endReasons } = makeHarness([], random);
    await session.start();
    pipelines[0].crash('broadcaster stopped', CLOSE_CODE_BROADCASTER_STOPPED);
    await session.stop();
    expect(events).toEqual(['connected', 'stopped', 'ended']);
    expect(endReasons).toEqual(['normal']);
    await vi.advanceTimersByTimeAsync(60_000);
    expect(pipelines).toHaveLength(1);
  });

  it('a user stop during an in-flight wait dial ends once', async () => {
    const { session, pipelines, endReasons } = makeHarness(['ok', 'hold'], random);
    await session.start();
    pipelines[0].crash('broadcaster stopped', CLOSE_CODE_BROADCASTER_STOPPED);
    await vi.advanceTimersByTimeAsync(WAIT);
    expect(pipelines).toHaveLength(2);
    const stopped = session.stop();
    pipelines[1].settleStart(new Error('no such broadcast'));
    await stopped;
    expect(endReasons).toEqual(['normal']);
    await vi.advanceTimersByTimeAsync(60_000);
    expect(pipelines).toHaveLength(2);
  });
});

// One named set per role, so the two viewer call sites and the browser
// broadcaster cannot drift apart.
describe('terminal close-code sets', () => {
  it('holds exactly 4000 and 4006 for viewers', () => {
    expect(isTerminalViewerClose(CLOSE_CODE_BROADCAST_ENDED)).toBe(true);
    expect(isTerminalViewerClose(CLOSE_CODE_TERMINATED_BY_OPERATOR)).toBe(true);
    // 4001 eviction, 4002 drain, 4003 origin-moved, 4005 leg-orphaned are all
    // explicitly retryable; an abrupt drop (no code) is the reconnect case.
    // 4012 PopUplinkFull is temporary by design (docs/76 item 28).
    // 4014 BroadcasterStopped waits for a restart (docs/79 D7).
    for (const code of [4001, 4002, 4003, 4004, 4005, 4012, 4014, 429, 0]) {
      expect(isTerminalViewerClose(code)).toBe(false);
    }
    expect(isTerminalViewerClose(null)).toBe(false);
    expect(isTerminalViewerClose(undefined)).toBe(false);
  });

  it('holds 4000, 4004 and 4006 for publishers, matching the natives', () => {
    expect(isTerminalPublisherClose(CLOSE_CODE_BROADCAST_ENDED)).toBe(true);
    expect(isTerminalPublisherClose(CLOSE_CODE_PUBLISHER_SUPERSEDED)).toBe(true);
    expect(isTerminalPublisherClose(CLOSE_CODE_TERMINATED_BY_OPERATOR)).toBe(true);
    for (const code of [4001, 4002, 4003, 4005, 4014, 0]) {
      expect(isTerminalPublisherClose(code)).toBe(false);
    }
    expect(isTerminalPublisherClose(null)).toBe(false);
  });
});
