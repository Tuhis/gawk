// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { isStale, POLL_MS, STALE_AFTER_MS, useLiveStore } from './liveStore.ts';

// UD22 was flagged in docs/36 §8 as the author's call, not the owner's, and the
// objection it invites is the right one: it adds an endpoint and reconnect
// logic. So the fallback is what these tests are mostly about — the stream has
// to be OPTIONAL, or the page has become worse than the poll it replaced.

class FakeEventSource {
  static instances: FakeEventSource[] = [];
  url: string;
  listeners = new Map<string, Array<(e: Event) => void>>();
  closed = false;

  constructor(url: string) {
    this.url = url;
    FakeEventSource.instances.push(this);
  }
  addEventListener(type: string, cb: (e: Event) => void) {
    const list = this.listeners.get(type) ?? [];
    list.push(cb);
    this.listeners.set(type, list);
  }
  close() {
    this.closed = true;
  }
  emit(type: string, data?: unknown) {
    const ev = data === undefined ? new Event(type) : (new MessageEvent(type, { data }) as Event);
    for (const cb of this.listeners.get(type) ?? []) cb(ev);
  }
}

const snapshot = { atMs: 1_700_000_000_000, live: [], ended: [] };

beforeEach(() => {
  FakeEventSource.instances = [];
  vi.stubGlobal('EventSource', FakeEventSource as unknown as typeof EventSource);
  vi.stubGlobal(
    'fetch',
    vi.fn(async () => new Response(JSON.stringify(snapshot), { status: 200 })),
  );
  useLiveStore.setState({
    snapshot: null,
    error: null,
    lastOkAt: null,
    mode: 'connecting',
    paused: false,
    pausedAtMs: null,
    gapMs: null,
  });
});

afterEach(() => {
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

describe('the live feed (UD22)', () => {
  it('opens the stream and takes its snapshots', () => {
    const stop = useLiveStore.getState().start();
    const es = FakeEventSource.instances[0];
    expect(es.url).toBe('live/stream');

    es.emit('open');
    expect(useLiveStore.getState().mode).toBe('stream');

    es.emit('snapshot', JSON.stringify(snapshot));
    expect(useLiveStore.getState().snapshot?.atMs).toBe(snapshot.atMs);
    stop();
    expect(es.closed).toBe(true);
  });

  it('falls back to the poll when the stream errors', async () => {
    vi.useFakeTimers();
    const stop = useLiveStore.getState().start();
    FakeEventSource.instances[0].emit('error');

    expect(useLiveStore.getState().mode).toBe('poll');
    await vi.advanceTimersByTimeAsync(POLL_MS + 1);
    expect(useLiveStore.getState().snapshot?.atMs).toBe(snapshot.atMs);
    stop();
  });

  it('polls when the browser has no EventSource at all', async () => {
    vi.useFakeTimers();
    vi.stubGlobal('EventSource', undefined);
    const stop = useLiveStore.getState().start();
    expect(useLiveStore.getState().mode).toBe('poll');
    await vi.advanceTimersByTimeAsync(1);
    expect(useLiveStore.getState().snapshot).not.toBeNull();
    stop();
  });

  it('ignores a frame it cannot parse and keeps the last good snapshot', () => {
    const stop = useLiveStore.getState().start();
    const es = FakeEventSource.instances[0];
    es.emit('open');
    es.emit('snapshot', JSON.stringify(snapshot));
    es.emit('snapshot', '{ this is not json');
    expect(useLiveStore.getState().snapshot?.atMs).toBe(snapshot.atMs);
    stop();
  });

  it('keeps the last good snapshot when a poll fails, and says the feed is stale', async () => {
    // Blanking the page on one failed poll would be precisely the "absence of
    // evidence rendered as something else" the health model refuses to do.
    useLiveStore.setState({ snapshot, lastOkAt: Date.now() });
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => {
        throw new Error('offline');
      }),
    );
    await useLiveStore.getState().poll();
    expect(useLiveStore.getState().snapshot?.atMs).toBe(snapshot.atMs);
    expect(useLiveStore.getState().error).toBe('offline');
  });
});

describe('pause (TH11, Q11)', () => {
  it('freezes updates and names the instant it froze at', () => {
    const stop = useLiveStore.getState().start();
    const es = FakeEventSource.instances[0];
    es.emit('open');
    es.emit('snapshot', JSON.stringify(snapshot));

    useLiveStore.getState().setPaused(true);
    expect(useLiveStore.getState().pausedAtMs).toBe(snapshot.atMs);

    es.emit('snapshot', JSON.stringify({ ...snapshot, atMs: snapshot.atMs + 60_000 }));
    // Nothing moved while it was read.
    expect(useLiveStore.getState().snapshot?.atMs).toBe(snapshot.atMs);

    useLiveStore.getState().setPaused(false);
    es.emit('snapshot', JSON.stringify({ ...snapshot, atMs: snapshot.atMs + 60_000 }));
    expect(useLiveStore.getState().snapshot?.atMs).toBe(snapshot.atMs + 60_000);
    stop();
  });

  it('refuses to poll while paused', async () => {
    useLiveStore.getState().setPaused(true);
    await useLiveStore.getState().poll();
    expect(useLiveStore.getState().snapshot).toBeNull();
  });
});

describe('staleness', () => {
  it('is measured from the last SUCCESS, not from the error flag', () => {
    const now = 1_000_000;
    expect(isStale(now - STALE_AFTER_MS + 1, now)).toBe(false);
    expect(isStale(now - STALE_AFTER_MS - 1, now)).toBe(true);
    // Never having succeeded is not the same as having gone stale.
    expect(isStale(null, now)).toBe(false);
  });
});
