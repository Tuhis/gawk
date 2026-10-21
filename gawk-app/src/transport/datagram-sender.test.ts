// DatagramSender's bounded queue (docs/75 RU9, BUGS.md "a slow uplink becomes
// unbounded delay"). The sender used to chain every frame onto one promise
// and never refuse one, so a link slower than the stream turned into tens of
// seconds of backlog. Now a delta or an audio packet more than one GOP old
// when its turn comes is dropped — with a result of its own, never a
// rejection — and a dropped delta takes the rest of its GOP with it.
//
// The clock is injected and driven by fake timers; the datagram writer is a
// real WritableStream whose sink can be stalled for a set time.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { DatagramSender, type SendResult } from './connection';

const GOP_MS = 500;

interface FakeLink {
  wt: WebTransport;
  // Every datagram the sink accepted, in order.
  written: number[];
  // Stalls the sink's NEXT write for `ms` (fake time).
  stallNext(ms: number): void;
  // Makes every later write reject (the session died).
  die(): void;
}

function fakeLink(): FakeLink {
  const written: number[] = [];
  let stall = 0;
  let dead = false;
  const writable = new WritableStream<Uint8Array>({
    async write(chunk) {
      if (dead) throw new Error('session gone');
      if (stall > 0) {
        const ms = stall;
        stall = 0;
        await new Promise((r) => setTimeout(r, ms));
      }
      written.push(chunk[0]);
    },
  });
  return {
    wt: { datagrams: { writable } } as unknown as WebTransport,
    written,
    stallNext: (ms) => {
      stall = ms;
    },
    die: () => {
      dead = true;
    },
  };
}

// A one-byte datagram whose byte identifies it in `written`.
const dg = (id: number): Uint8Array<ArrayBuffer> => new Uint8Array([id]);

// Records how each send settled.
function track(p: Promise<SendResult>, into: Map<number, string>, id: number): void {
  p.then(
    (r) => into.set(id, r),
    () => into.set(id, 'rejected'),
  );
}

beforeEach(() => {
  vi.useFakeTimers();
  vi.setSystemTime(0);
});

afterEach(() => {
  vi.useRealTimers();
});

const now = () => Date.now();

describe('DatagramSender — a drop is not a failure (RU9)', () => {
  it("resolves a stale frame's send with 'dropped', never rejects, and keeps sending the frames behind it", async () => {
    const link = fakeLink();
    const sender = new DatagramSender(link.wt, { now, maxAgeMs: GOP_MS });
    const results = new Map<number, string>();

    link.stallNext(600);
    track(sender.send([dg(1)], { kind: 'delta', gop: 0 }), results, 1);
    await vi.advanceTimersByTimeAsync(10);
    track(sender.send([dg(2)], { kind: 'delta', gop: 0 }), results, 2);
    await vi.advanceTimersByTimeAsync(700);

    expect(results.get(1)).toBe('sent');
    expect(results.get(2)).toBe('dropped');

    // Nothing latched: a fresh frame of the next GOP still goes out.
    track(sender.send([dg(3)], { kind: 'delta', gop: 1 }), results, 3);
    await vi.advanceTimersByTimeAsync(1);
    expect(results.get(3)).toBe('sent');
    expect(link.written).toEqual([1, 3]);
  });

  it('a real write failure still rejects and latches, as before', async () => {
    const link = fakeLink();
    const sender = new DatagramSender(link.wt, { now, maxAgeMs: GOP_MS });
    link.die();
    await expect(sender.send([dg(1)], { kind: 'delta', gop: 0 })).rejects.toThrow('session gone');
    await expect(sender.send([dg(2)], { kind: 'control' })).rejects.toThrow('session gone');
  });
});

describe('DatagramSender — age is enqueue time, decided once per frame (RU9)', () => {
  it("drops a delta more than one GOP old, and the rest of its GOP's deltas with it until the next keyframe", async () => {
    const link = fakeLink();
    const sender = new DatagramSender(link.wt, { now, maxAgeMs: GOP_MS });
    const results = new Map<number, string>();

    link.stallNext(600);
    track(sender.send([dg(1)], { kind: 'delta', gop: 0 }), results, 1); // writing at t=0
    await vi.advanceTimersByTimeAsync(50);
    track(sender.send([dg(2)], { kind: 'delta', gop: 0 }), results, 2); // 550 ms old at its turn
    await vi.advanceTimersByTimeAsync(500);
    track(sender.send([dg(3)], { kind: 'delta', gop: 0 }), results, 3); // 50 ms old, same GOP
    track(sender.send([dg(4)], { kind: 'delta', gop: 1 }), results, 4); // the next GOP
    await vi.advanceTimersByTimeAsync(100);
    track(sender.send([dg(5)], { kind: 'delta', gop: 0 }), results, 5); // late straggler of GOP 0
    await vi.advanceTimersByTimeAsync(1);

    expect(results.get(2)).toBe('dropped');
    expect(results.get(3)).toBe('dropped');
    expect(results.get(4)).toBe('sent');
    expect(results.get(5)).toBe('dropped');
    expect(link.written).toEqual([1, 4]);
  });

  it('never half-sends a frame: a frame whose first datagram came up in time is sent whole', async () => {
    const link = fakeLink();
    const sender = new DatagramSender(link.wt, { now, maxAgeMs: GOP_MS });
    const results = new Map<number, string>();

    // The frame's first datagram goes at t=0; the second waits 900 ms behind
    // a stalled write. Its age is decided once, at the first datagram.
    track(sender.send([dg(1), dg(2), dg(3)], { kind: 'delta', gop: 0 }), results, 1);
    link.stallNext(900);
    await vi.advanceTimersByTimeAsync(1000);
    expect(results.get(1)).toBe('sent');
    expect(link.written).toEqual([1, 2, 3]);
  });

  it('reads the age from the injected clock, not from timers', async () => {
    const link = fakeLink();
    let t = 0;
    const sender = new DatagramSender(link.wt, { now: () => t, maxAgeMs: GOP_MS });
    const results = new Map<number, string>();

    link.stallNext(10);
    track(sender.send([dg(1)], { kind: 'delta', gop: 0 }), results, 1);
    track(sender.send([dg(2)], { kind: 'delta', gop: 0 }), results, 2);
    t = 501; // the injected clock jumped past the bound while 2 waited
    await vi.advanceTimersByTimeAsync(20);
    expect(results.get(2)).toBe('dropped');
  });
});

describe('DatagramSender — audio and control (RU9)', () => {
  it('drops an audio packet more than 500 ms old when its turn comes, and only that packet', async () => {
    const link = fakeLink();
    const sender = new DatagramSender(link.wt, { now, maxAgeMs: GOP_MS });
    const results = new Map<number, string>();

    link.stallNext(600);
    track(sender.send([dg(1)], { kind: 'audio' }), results, 1);
    await vi.advanceTimersByTimeAsync(20);
    track(sender.send([dg(2)], { kind: 'audio' }), results, 2); // 580 ms old at its turn
    await vi.advanceTimersByTimeAsync(200);
    track(sender.send([dg(3)], { kind: 'audio' }), results, 3); // 0–200 ms old
    track(sender.send([dg(4)], { kind: 'delta', gop: 0 }), results, 4);
    await vi.advanceTimersByTimeAsync(400);

    expect(results.get(2)).toBe('dropped');
    expect(results.get(3)).toBe('sent');
    // An audio drop has no GOP: the video behind it is untouched.
    expect(results.get(4)).toBe('sent');
    expect(link.written).toEqual([1, 3, 4]);
  });

  it('never drops a control datagram for age (audio config, TimeSync, clock mapping)', async () => {
    const link = fakeLink();
    const sender = new DatagramSender(link.wt, { now, maxAgeMs: GOP_MS });
    const results = new Map<number, string>();

    link.stallNext(3000);
    track(sender.send([dg(1)], { kind: 'delta', gop: 0 }), results, 1);
    track(sender.send([dg(2)], { kind: 'control' }), results, 2);
    await vi.advanceTimersByTimeAsync(3100);

    expect(results.get(2)).toBe('sent');
    expect(link.written).toEqual([1, 2]);
  });
});

describe('DatagramSender — the boundary (RU9)', () => {
  // 60 fps of deltas, a keyframe every 500 ms (GOP n = frames 30n..30n+29;
  // keyframes ride their own streams and are not in this queue). The writer
  // stalls once, on the first delta.
  async function run(stallMs: number): Promise<{ results: Map<number, string>; written: number[] }> {
    const link = fakeLink();
    const sender = new DatagramSender(link.wt, { now, maxAgeMs: GOP_MS });
    const results = new Map<number, string>();
    link.stallNext(stallMs);
    for (let f = 1; f < 90; f++) {
      await vi.advanceTimersByTimeAsync(Math.round((f * 1000) / 60) - Date.now());
      if (f % 30 !== 0) track(sender.send([dg(f)], { kind: 'delta', gop: Math.floor(f / 30) }), results, f);
    }
    await vi.advanceTimersByTimeAsync(2000);
    return { results, written: link.written };
  }

  it('a writer stalled 400 ms drops nothing', async () => {
    const { results } = await run(400);
    expect([...results.values()].every((r) => r === 'sent')).toBe(true);
    expect(results.size).toBe(89 - 2);
  });

  it('a writer stalled 600 ms drops only the frames past the bound and the rest of their GOP', async () => {
    // Frame 1 is written at t≈17 and stalls until ≈617. Frame 2 (t≈33) is
    // then ≈584 ms old: dropped, and every later delta of GOP 0 with it,
    // however fresh. GOP 1 (frames 31–59, the oldest ≈100 ms old) all goes.
    const { results } = await run(600);
    expect(results.get(1)).toBe('sent');
    for (let f = 2; f < 30; f++) expect(results.get(f), `frame ${f}`).toBe('dropped');
    for (let f = 31; f < 90; f++) if (f % 30 !== 0) expect(results.get(f), `frame ${f}`).toBe('sent');
  });

  it('on a link that keeps up, the output is byte-identical to the input, in the same order', async () => {
    const link = fakeLink();
    const sender = new DatagramSender(link.wt, { now, maxAgeMs: GOP_MS });
    const sent: number[] = [];
    let id = 0;
    for (let f = 0; f < 60; f++) {
      const kind = f % 3 === 0 ? 'audio' : f % 7 === 0 ? 'control' : 'delta';
      const batch = [dg(++id), dg(++id)];
      sent.push(...batch.map((d) => d[0]));
      void sender.send(batch, kind === 'delta' ? { kind, gop: Math.floor(f / 30) } : { kind });
      await vi.advanceTimersByTimeAsync(16);
    }
    expect(link.written).toEqual(sent);
  });
});

describe('DatagramSender — deltas give way to a late keyframe (RU9)', () => {
  it("drops a delta whose GOP the keyframe sender blocks, and the rest of that GOP's deltas with it", async () => {
    const link = fakeLink();
    const blocked = new Set<number>([1]);
    const sender = new DatagramSender(link.wt, { now, maxAgeMs: GOP_MS, gopBlocked: (g) => blocked.has(g) });
    const results = new Map<number, string>();

    track(sender.send([dg(1)], { kind: 'delta', gop: 0 }), results, 1);
    track(sender.send([dg(2)], { kind: 'delta', gop: 1 }), results, 2);
    await vi.advanceTimersByTimeAsync(1);
    blocked.delete(1); // the keyframe went out after all — GOP 1 stays dropped
    track(sender.send([dg(3)], { kind: 'delta', gop: 1 }), results, 3);
    track(sender.send([dg(4)], { kind: 'delta', gop: 2 }), results, 4);
    await vi.advanceTimersByTimeAsync(1);

    expect([results.get(1), results.get(2), results.get(3), results.get(4)]).toEqual([
      'sent',
      'dropped',
      'dropped',
      'sent',
    ]);
  });
});

describe('DatagramSender — close (RU9 resume)', () => {
  it('nothing queued on a closed sender is sent: its queued sends reject', async () => {
    const link = fakeLink();
    const sender = new DatagramSender(link.wt, { now, maxAgeMs: GOP_MS });
    const results = new Map<number, string>();
    link.stallNext(100);
    track(sender.send([dg(1)], { kind: 'delta', gop: 0 }), results, 1);
    track(sender.send([dg(2)], { kind: 'delta', gop: 0 }), results, 2);
    sender.close();
    await vi.advanceTimersByTimeAsync(200);
    expect(results.get(2)).toBe('rejected');
    expect(link.written).not.toContain(2);
  });
});
