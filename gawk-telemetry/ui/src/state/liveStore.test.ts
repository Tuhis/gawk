// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { isStale, POLL_MS, STALE_AFTER_MS, useLiveStore } from './liveStore.ts';

// UD22 was flagged in docs/36 §8 as the author's call, not the owner's, and the
// objection it invites is the right one: it adds an endpoint and reconnect
// logic. So the fallback is what these tests are mostly about — the stream has
// to be OPTIONAL, or the page has become worse than the poll it replaced.

// The mock layer is a fake STREAM, not a fake EventSource: since docs/55 D4 the
// feed is a `fetch` of `live/stream` read through lib/sse.ts, because in OIDC
// mode it must carry a bearer header that EventSource cannot send. The tests
// below are the same tests — only the verbs that drive the fake changed:
// `open` is the response headers arriving, `error` is the connection failing,
// and every other event is written onto the body as the server writes it.
// Delivery is asynchronous now (a body is read, not dispatched), hence the
// awaits.

/**
 * Let the stream reader run: a body read resolves through microtasks, then
 * one REAL event-loop turn for good measure — the real `setTimeout`, captured
 * before any test installs fake timers, so this works under either clock.
 */
const realSetTimeout = globalThis.setTimeout;
async function flush() {
  for (let i = 0; i < 50; i++) await Promise.resolve();
  await new Promise((resolve) => realSetTimeout(resolve, 0));
}

class FakeStream {
  static instances: FakeStream[] = [];
  url: string;
  signal: AbortSignal | undefined;
  readonly response: Promise<Response>;
  private respond!: (res: Response) => void;
  private refuse!: (err: Error) => void;
  private body: ReadableStreamDefaultController<Uint8Array> | null = null;

  constructor(url: string, init: RequestInit = {}) {
    this.url = url;
    this.signal = init.signal ?? undefined;
    this.response = new Promise<Response>((resolve, reject) => {
      this.respond = resolve;
      this.refuse = reject;
    });
    // What a real fetch does when its signal fires: the request rejects, or,
    // once the body is flowing, the body errors.
    this.signal?.addEventListener('abort', () => {
      const err = new DOMException('aborted', 'AbortError');
      this.refuse(err);
      this.body?.error(err);
    });
    FakeStream.instances.push(this);
  }

  get closed(): boolean {
    return this.signal?.aborted ?? false;
  }

  async emit(type: string, data?: string) {
    if (type === 'open') {
      const body = new ReadableStream<Uint8Array>({
        start: (c) => {
          this.body = c;
        },
      });
      this.respond(new Response(body, { headers: { 'content-type': 'text/event-stream' } }));
    } else if (type === 'error') {
      const err = new TypeError('network error');
      if (this.body) this.body.error(err);
      else this.refuse(err);
    } else {
      this.write(`event: ${type}\ndata: ${data ?? ''}\n\n`);
    }
    await flush();
  }

  /** Raw bytes onto the body, exactly as the server would write them. */
  write(text: string) {
    this.body?.enqueue(new TextEncoder().encode(text));
  }

  /** The server ending the response. */
  async end() {
    this.body?.close();
    await flush();
  }
}

const snapshot = { atMs: 1_700_000_000_000, live: [], ended: [] };

beforeEach(() => {
  FakeStream.instances = [];
  vi.stubGlobal(
    'fetch',
    vi.fn(async (url: string, init?: RequestInit) =>
      url === 'live/stream'
        ? new FakeStream(url, init).response
        : new Response(JSON.stringify(snapshot), { status: 200 }),
    ),
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
  it('opens the stream and takes its snapshots', async () => {
    const stop = useLiveStore.getState().start();
    const es = FakeStream.instances[0];
    expect(es.url).toBe('live/stream');

    await es.emit('open');
    expect(useLiveStore.getState().mode).toBe('stream');

    await es.emit('snapshot', JSON.stringify(snapshot));
    expect(useLiveStore.getState().snapshot?.atMs).toBe(snapshot.atMs);
    stop();
    expect(es.closed).toBe(true);
  });

  it('falls back to the poll when the stream errors', async () => {
    vi.useFakeTimers();
    const stop = useLiveStore.getState().start();
    await FakeStream.instances[0].emit('error');

    expect(useLiveStore.getState().mode).toBe('poll');
    await vi.advanceTimersByTimeAsync(POLL_MS + 1);
    expect(useLiveStore.getState().snapshot?.atMs).toBe(snapshot.atMs);
    stop();
  });

  it('polls when the browser cannot hold a stream open (no AbortController)', async () => {
    vi.useFakeTimers();
    vi.stubGlobal('AbortController', undefined);
    const stop = useLiveStore.getState().start();
    expect(useLiveStore.getState().mode).toBe('poll');
    await vi.advanceTimersByTimeAsync(1);
    expect(useLiveStore.getState().snapshot).not.toBeNull();
    stop();
  });

  it('ignores a frame it cannot parse and keeps the last good snapshot', async () => {
    const stop = useLiveStore.getState().start();
    const es = FakeStream.instances[0];
    await es.emit('open');
    await es.emit('snapshot', JSON.stringify(snapshot));
    await es.emit('snapshot', '{ this is not json');
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

// What EventSource used to do implicitly is written out in liveStore since
// docs/55 D4; these pin that it is the same policy.
describe('reconnection (docs/55 D4)', () => {
  it('reconnects after the retry hint when the stream ends, polling in between', async () => {
    vi.useFakeTimers();
    const stop = useLiveStore.getState().start();
    const first = FakeStream.instances[0];
    await first.emit('open');
    first.write('retry: 3000\n\n');
    await first.end();
    expect(useLiveStore.getState().mode).toBe('poll');

    await vi.advanceTimersByTimeAsync(2999);
    expect(FakeStream.instances).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(1);
    const second = FakeStream.instances[1];
    expect(second).toBeDefined();

    // The poll stops the moment the stream is back: never both.
    await second.emit('open');
    expect(useLiveStore.getState().mode).toBe('stream');
    const polls = () =>
      vi.mocked(fetch).mock.calls.filter(([url]) => String(url) === 'live').length;
    const before = polls();
    await vi.advanceTimersByTimeAsync(POLL_MS * 3);
    expect(polls()).toBe(before);
    stop();
  });

  it('takes an HTTP refusal as final and leaves the page on the poll', async () => {
    vi.useFakeTimers();
    vi.stubGlobal(
      'fetch',
      vi.fn(async (url: string) =>
        url === 'live/stream'
          ? new Response('streaming is not supported by this server', { status: 501 })
          : new Response(JSON.stringify(snapshot), { status: 200 }),
      ),
    );
    const stop = useLiveStore.getState().start();
    await vi.advanceTimersByTimeAsync(POLL_MS * 10);
    expect(useLiveStore.getState().mode).toBe('poll');
    const streams = vi.mocked(fetch).mock.calls.filter(([url]) => String(url) === 'live/stream');
    expect(streams).toHaveLength(1);
    stop();
  });

  it('reconnects at once, without the poll, when the server says the token expired', async () => {
    vi.useFakeTimers();
    const stop = useLiveStore.getState().start();
    const first = FakeStream.instances[0];
    await first.emit('open');
    await vi.advanceTimersByTimeAsync(60_000);
    await first.emit('expired', '{}');
    await first.end();
    expect(FakeStream.instances).toHaveLength(2);
    expect(useLiveStore.getState().mode).toBe('stream');
    stop();
    expect(FakeStream.instances[1].closed).toBe(true);
  });

  it('does not spin when a stream expires as soon as it opens', async () => {
    vi.useFakeTimers();
    const stop = useLiveStore.getState().start();
    const first = FakeStream.instances[0];
    await first.emit('open');
    await first.emit('expired', '{}');
    await first.end();
    // The ordinary delay, with the poll covering it.
    expect(FakeStream.instances).toHaveLength(1);
    expect(useLiveStore.getState().mode).toBe('poll');
    await vi.advanceTimersByTimeAsync(POLL_MS);
    expect(FakeStream.instances).toHaveLength(2);
    stop();
  });

  it('stops reconnecting once torn down', async () => {
    vi.useFakeTimers();
    const stop = useLiveStore.getState().start();
    await FakeStream.instances[0].emit('error');
    stop();
    await vi.advanceTimersByTimeAsync(POLL_MS * 10);
    expect(FakeStream.instances).toHaveLength(1);
  });
});

describe('pause (TH11, Q11)', () => {
  it('freezes updates and names the instant it froze at', async () => {
    const stop = useLiveStore.getState().start();
    const es = FakeStream.instances[0];
    await es.emit('open');
    await es.emit('snapshot', JSON.stringify(snapshot));

    useLiveStore.getState().setPaused(true);
    expect(useLiveStore.getState().pausedAtMs).toBe(snapshot.atMs);

    await es.emit('snapshot', JSON.stringify({ ...snapshot, atMs: snapshot.atMs + 60_000 }));
    // Nothing moved while it was read.
    expect(useLiveStore.getState().snapshot?.atMs).toBe(snapshot.atMs);

    useLiveStore.getState().setPaused(false);
    await es.emit('snapshot', JSON.stringify({ ...snapshot, atMs: snapshot.atMs + 60_000 }));
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
