// The browser broadcaster's keyframe streams, on the engine's F-12 rule
// (gawk-broadcast-desktop/crates/engine/src/sender.rs, docs/38 F-12) with the
// browser's two clocks (docs/75 RU9): a stream is handed over when its
// write() resolves and acknowledged when its close() does. One being written,
// one draining, a single pending slot where the newest waiter wins, a 2 s
// abort checked only when a newer keyframe arrives — and a keyframe is never
// dropped for age. Today every keyframe opened its own stream at once and
// they shared the link without bound (BUGS.md).
//
// The streams are hand-driven fakes: each open, write and close is a
// deferred the test settles, so every event is explicit.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { KeyframeSender, type KeyframeStreamWriter } from './keyframe-sender';

interface Deferred<T> {
  promise: Promise<T>;
  resolve: (v: T) => void;
  reject: (e: unknown) => void;
}

function deferred<T>(): Deferred<T> {
  let resolve!: (v: T) => void;
  let reject!: (e: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  promise.catch(() => {});
  return { promise, resolve, reject };
}

interface FakeStream {
  // The keyframe written to it (its first byte), once write() was called.
  id: number | null;
  opened: Deferred<{ getWriter(): KeyframeStreamWriter }>;
  write: Deferred<void>;
  close: Deferred<void>;
  aborted: boolean;
  // Opens the stream (resolves createUnidirectionalStream()).
  open(): void;
  handOver(): void;
  ack(): void;
}

function harness() {
  const streams: FakeStream[] = [];
  const openStream = () => {
    const s: FakeStream = {
      id: null,
      opened: deferred(),
      write: deferred(),
      close: deferred(),
      aborted: false,
      open: () => s.opened.resolve({ getWriter: () => writer }),
      handOver: () => s.write.resolve(),
      ack: () => s.close.resolve(),
    };
    const writer: KeyframeStreamWriter = {
      write: (chunk) => {
        s.id = chunk[0];
        return s.write.promise;
      },
      close: () => s.close.promise,
      abort: () => {
        s.aborted = true;
        s.write.reject(new Error('aborted'));
        s.close.reject(new Error('aborted'));
        return Promise.resolve();
      },
    };
    streams.push(s);
    return s.opened.promise;
  };
  const events: string[] = [];
  const kf = new KeyframeSender({
    now: () => Date.now(),
    gopMs: 500,
    events: {
      handedOver: (gop) => events.push(`handedOver ${gop}`),
      waiting: (gop) => events.push(`waiting ${gop}`),
      ended: (gop, outcome) => events.push(`${outcome} ${gop}`),
    },
  });
  kf.attach(openStream);
  // Keyframe `gop` carries one byte, its GOP number, so a stream's content
  // says which keyframe it is.
  const produce = (gop: number) => kf.produce(gop, new Uint8Array([gop]));
  return { kf, streams, events, produce };
}

const settle = () => vi.advanceTimersByTimeAsync(0);
const at = async (t: number) => vi.advanceTimersByTimeAsync(t - Date.now());

beforeEach(() => {
  vi.useFakeTimers();
  vi.setSystemTime(0);
});

afterEach(() => {
  vi.useRealTimers();
});

describe('KeyframeSender — one being written, one draining (RU9)', () => {
  it('a keyframe waits while another is being written, and starts on its handover, not its acknowledgement', async () => {
    const { streams, produce } = harness();
    produce(1);
    await settle();
    streams[0].open();
    await settle();
    expect(streams[0].id).toBe(1);

    produce(2);
    await settle();
    expect(streams.length).toBe(1); // waiting: nothing opened for it

    streams[0].handOver(); // handed over, not yet acknowledged
    await settle();
    expect(streams.length).toBe(2);
    streams[1].open();
    await settle();
    expect(streams[1].id).toBe(2);
  });

  it('at most two in Chrome: with two draining, a third waits for an acknowledgement', async () => {
    const { streams, produce } = harness();
    produce(1);
    await settle();
    streams[0].open();
    await settle();
    streams[0].handOver();
    await settle();
    produce(2);
    await settle();
    streams[1].open();
    await settle();
    streams[1].handOver();
    await settle();

    produce(3);
    await settle();
    expect(streams.length).toBe(2); // two draining: 3 waits

    streams[0].ack();
    await settle();
    expect(streams.length).toBe(3);
  });

  it('the waiter also starts on a failure', async () => {
    const { kf, streams, produce } = harness();
    produce(1);
    await settle();
    streams[0].open();
    await settle();
    produce(2);
    await settle();
    expect(streams.length).toBe(1);
    streams[0].write.reject(new Error('stream reset by peer'));
    await settle();
    expect(streams.length).toBe(2);
    expect(kf.counters.failed).toBe(1);
  });

  it('a keyframe is never dropped for age: a waiter 10 s old still starts', async () => {
    const { kf, streams, produce } = harness();
    produce(1);
    await settle();
    streams[0].open();
    await settle();
    produce(2);
    await at(10_000);
    streams[0].handOver();
    await settle();
    expect(streams.length).toBe(2);
    expect(kf.counters.superseded).toBe(0);
  });
});

describe('KeyframeSender — not in flight until open (RU9)', () => {
  it('an opening stream neither makes the next keyframe wait nor counts toward the limit', async () => {
    const { streams, produce } = harness();
    produce(1);
    await settle();
    produce(2);
    await settle();
    produce(3);
    await settle();
    // An open that never resolves blocks nothing.
    expect(streams.length).toBe(3);
  });

  it('newest registration wins: a stream that opens aborts the one being written (superseded and failed)', async () => {
    const { kf, streams, produce } = harness();
    produce(1);
    await settle();
    produce(2);
    await settle();
    streams[0].open();
    await settle();
    expect(streams[0].id).toBe(1);

    streams[1].open();
    await settle();
    expect(streams[0].aborted).toBe(true);
    expect(streams[1].id).toBe(2);
    expect(kf.counters.superseded).toBe(1);
    expect(kf.counters.failed).toBe(1);

    // The abort's own rejections are not counted a second time.
    await settle();
    expect(kf.counters.failed).toBe(1);
  });

  it('an open failing counts as failed', async () => {
    const { kf, streams, produce } = harness();
    produce(1);
    await settle();
    streams[0].opened.reject(new Error('stream limit'));
    await settle();
    expect(kf.counters.failed).toBe(1);
    expect(kf.counters.superseded).toBe(0);
  });
});

describe('KeyframeSender — the pending slot and the 2 s abort (RU9, F-12)', () => {
  it('a newer waiter replaces an older one, which counts as superseded and never opens', async () => {
    const { kf, streams, produce } = harness();
    produce(1);
    await settle();
    streams[0].open();
    await settle();
    produce(2);
    produce(3);
    await settle();
    expect(kf.counters.superseded).toBe(1);
    streams[0].handOver();
    await settle();
    streams[1].open();
    await settle();
    expect(streams.length).toBe(2);
    expect(streams[1].id).toBe(3);
  });

  it('a stream unacknowledged 2 s after it opened is aborted only when a newer keyframe arrives; the waiter is discarded', async () => {
    const { kf, streams, produce } = harness();
    produce(1);
    await settle();
    streams[0].open(); // opened at t=0; its write never finishes
    await settle();
    await at(500);
    produce(2);
    await at(1000);
    produce(3); // replaces 2
    await at(1999);
    produce(4); // 1999 ms: not yet — replaces 3
    await settle();
    expect(streams[0].aborted).toBe(false);
    expect(kf.counters.superseded).toBe(2);

    await at(2500); // no timer aborts anything on its own
    expect(streams[0].aborted).toBe(false);

    produce(5);
    await settle();
    expect(streams[0].aborted).toBe(true);
    // 2 and 3 replaced, 1 aborted, 4 discarded with the abort; 5 starts.
    expect(kf.counters.superseded).toBe(4);
    expect(kf.counters.failed).toBe(1);
    expect(streams.length).toBe(2);
    streams[1].open();
    await settle();
    expect(streams[1].id).toBe(5);
  });

  it('a draining stream past 2 s is aborted too, and is never also counted sent', async () => {
    const { kf, streams, produce } = harness();
    produce(1);
    await settle();
    streams[0].open();
    await settle();
    streams[0].handOver();
    await at(2000);
    produce(2);
    await settle();
    expect(streams[0].aborted).toBe(true);
    expect(kf.counters.failed).toBe(1);
    expect(kf.counters.superseded).toBe(1);
    expect(kf.counters.sent).toBe(0);
  });

  it('counts a stream sent only once close() resolves', async () => {
    const { kf, streams, produce, events } = harness();
    produce(1);
    await settle();
    streams[0].open();
    await settle();
    streams[0].handOver();
    await settle();
    expect(kf.counters.sent).toBe(0);
    streams[0].ack();
    await settle();
    expect(kf.counters.sent).toBe(1);
    expect(kf.counters.bytesSent).toBe(1);
    expect(events).toEqual(['handedOver 1', 'sent 1']);
  });
});

describe('KeyframeSender — which GOPs give way (RU9)', () => {
  it('a GOP whose keyframe waits behind a late keyframe gives way; behind an on-time one it does not', async () => {
    const { kf, streams, produce } = harness();
    produce(1); // produced at t=0
    await settle();
    streams[0].open();
    await settle();
    await at(100);
    produce(2); // waits behind 1
    await settle();
    expect(kf.deltasBlocked(2)).toBe(false); // 1 is 100 ms old: on time
    expect(kf.deltasBlocked(1)).toBe(false); // 1 itself is being written

    await at(500);
    expect(kf.deltasBlocked(2)).toBe(false); // exactly one GOP: still on time
    await at(501);
    expect(kf.deltasBlocked(2)).toBe(true); // 1 not handed over within one GOP
  });

  it('a GOP whose keyframe waits behind the two-stream limit gives way', async () => {
    const { kf, streams, produce } = harness();
    produce(1);
    await settle();
    streams[0].open();
    await settle();
    streams[0].handOver();
    await settle();
    produce(2);
    await settle();
    streams[1].open();
    await settle();
    streams[1].handOver();
    await settle();
    produce(3);
    await settle();
    expect(kf.deltasBlocked(3)).toBe(true);
    expect(kf.deltasBlocked(2)).toBe(false);
  });

  it("a GOP whose keyframe did not go out — replaced, discarded, aborted or failed — gives way", async () => {
    const { kf, streams, produce } = harness();
    produce(1);
    await settle();
    streams[0].open();
    await settle();
    produce(2);
    produce(3); // 2 replaced
    await settle();
    expect(kf.deltasBlocked(2)).toBe(true);
    await at(2000);
    produce(4); // 1 aborted, 3 discarded, 4 starts
    await settle();
    expect(kf.deltasBlocked(1)).toBe(true);
    expect(kf.deltasBlocked(3)).toBe(true);
    expect(kf.deltasBlocked(4)).toBe(false); // opening: not waiting
    streams[1].opened.reject(new Error('open failed'));
    await settle();
    expect(kf.deltasBlocked(4)).toBe(true);
  });
});

describe('KeyframeSender — resume (RU9)', () => {
  it('streams still in Chrome die with the old session as failed, the waiter is superseded, and the next keyframe starts at once', async () => {
    const { kf, streams, produce } = harness();
    produce(1);
    await settle();
    streams[0].open();
    await settle();
    streams[0].handOver(); // draining
    await settle();
    produce(2);
    await settle();
    streams[1].open(); // being written
    await settle();
    produce(3); // waiting
    await settle();

    kf.detach(true);
    expect(kf.counters.failed).toBe(2);
    expect(kf.counters.superseded).toBe(1);
    expect(kf.deltasBlocked(3)).toBe(true);

    // The old session's late settles are ignored.
    streams[0].ack();
    streams[1].write.reject(new Error('session gone'));
    await settle();
    expect(kf.counters.sent).toBe(0);
    expect(kf.counters.failed).toBe(2);

    const fresh: number[] = [];
    kf.attach(() => {
      fresh.push(1);
      return new Promise(() => {});
    });
    produce(4); // the forced keyframe
    await settle();
    expect(fresh.length).toBe(1);
  });

  it('a stop counts nothing', async () => {
    const { kf, streams, produce } = harness();
    produce(1);
    await settle();
    streams[0].open();
    await settle();
    produce(2);
    kf.detach(false);
    expect(kf.counters.failed).toBe(0);
    expect(kf.counters.superseded).toBe(0);
  });
});
