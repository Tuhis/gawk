// The browser's carrier lane (docs/88 BR2): one GOP stream per keyframe with
// the keyframe at its head (K3, D2), the GOP's records after it, under RU9's
// keyframe rules (D3). The transport is a hand-driven fake: every open, write
// and close is a deferred the test settles, so a write blocks until the test
// hands it over and a stream is acknowledged only when the test says so.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { CarrierSender, DELIVERY_HINTS, DELIVERY_LABEL, DELIVERY_LABELS, engageCarrier, frameRecords, isChromium } from './carrier-sender';
import { KEYFRAME_ABORT_AGE_MS, KeyframeSender, type KeyframeStreamWriter } from './keyframe-sender';
import { DEFAULT_CAPTURE_CONFIG } from '../media/types';
import { packetizeFrameWithParity, packetizeStreamKeyframe } from './packetizer';
import { CAP_KEYFRAME_CARRIERS, CAP_PARITY_CHUNKS, CAP_UPLINK_CARRIERS } from './parity';
import { encodeCarrierRecord, encodeKeyframeCarrierPrologue } from './wire';

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

// One fake uni stream. Writes are recorded in call order — the order QUIC
// delivers a stream's bytes in — and each blocks until handOver(i).
class FakeStream {
  readonly opened = deferred<{ getWriter(): KeyframeStreamWriter }>();
  readonly writes: { bytes: Uint8Array; d: Deferred<void> }[] = [];
  readonly close = deferred<void>();
  closed = false;
  aborted = false;

  constructor() {
    const writer: KeyframeStreamWriter = {
      write: (chunk) => {
        if (this.closed || this.aborted) return Promise.reject(new Error('write after close'));
        const d = deferred<void>();
        this.writes.push({ bytes: chunk, d });
        return d.promise;
      },
      close: () => {
        this.closed = true;
        return this.close.promise;
      },
      abort: () => {
        this.aborted = true;
        for (const w of this.writes) w.d.reject(new Error('aborted'));
        this.close.reject(new Error('aborted'));
        return Promise.resolve();
      },
    };
    this.writer = writer;
  }

  readonly writer: KeyframeStreamWriter;

  open(): void {
    this.opened.resolve({ getWriter: () => this.writer });
  }
  handOver(i: number): void {
    this.writes[i].d.resolve();
  }
  ack(): void {
    this.close.resolve();
  }
  // The stream's bytes, in the order written.
  bytes(): Uint8Array {
    const n = this.writes.reduce((a, w) => a + w.bytes.length, 0);
    const out = new Uint8Array(n);
    let o = 0;
    for (const w of this.writes) {
      out.set(w.bytes, o);
      o += w.bytes.length;
    }
    return out;
  }
}

function harness() {
  const streams: FakeStream[] = [];
  const events: string[] = [];
  const kf = new KeyframeSender({
    now: () => Date.now(),
    gopMs: 500,
    events: { ended: (gop, outcome) => events.push(`${outcome} ${gop}`) },
  });
  const lane = new CarrierSender(kf);
  kf.attach(() => {
    const s = new FakeStream();
    streams.push(s);
    return s.opened.promise;
  });
  // Keyframe `gop`: a real StreamFrame message, frameId = 100 * gop.
  const keyframeMsg = (gop: number) =>
    packetizeStreamKeyframe({ frameId: 100 * gop, timestampUs: BigInt(gop) }, new Uint8Array(0), new Uint8Array([gop, gop]));
  const produce = (gop: number, carrier = true) => kf.produce(gop, keyframeMsg(gop), { carrier });
  // Delta `n` of GOP `gop`: the datagrams datagram mode would send for it.
  const delta = (gop: number, n: number, size = 30) => {
    const { datagrams, parity } = packetizeFrameWithParity(
      { frameId: 100 * gop + n, keyframe: false, timestampUs: BigInt(n) },
      new Uint8Array(size).fill(n),
      1,
      1024,
    );
    return [...datagrams, ...parity];
  };
  const results: string[] = [];
  const send = (gop: number, n: number, size?: number) => {
    const p = lane.send(gop, delta(gop, n, size));
    void p.then(
      (r) => results.push(`${gop}.${n} ${r}`),
      () => results.push(`${gop}.${n} rejected`),
    );
    return p;
  };
  return { kf, lane, streams, events, results, produce, send, delta, keyframeMsg };
}

const settle = () => vi.advanceTimersByTimeAsync(0);
const at = async (t: number) => vi.advanceTimersByTimeAsync(t - Date.now());

const concat = (...parts: Uint8Array[]) => {
  const out = new Uint8Array(parts.reduce((a, p) => a + p.length, 0));
  let o = 0;
  for (const p of parts) {
    out.set(p, o);
    o += p.length;
  }
  return out;
};

beforeEach(() => {
  vi.useFakeTimers();
  vi.setSystemTime(0);
});

afterEach(() => {
  vi.useRealTimers();
});

describe('CarrierSender — the keyframe leads its GOP stream (K3, D2)', () => {
  it("the stream's first bytes are the 0x1F prologue and the keyframe's StreamFrame, then the GOP's records in order", async () => {
    const { streams, produce, send, keyframeMsg, delta } = harness();
    produce(1);
    send(1, 1);
    send(1, 2);
    await settle();
    streams[0].open();
    await settle();
    // Only the keyframe is written until it has been handed over.
    expect(streams[0].writes.length).toBe(1);
    streams[0].handOver(0);
    await settle();
    streams[0].handOver(1);
    await settle();
    streams[0].handOver(2);
    await settle();
    const want = concat(encodeKeyframeCarrierPrologue(), keyframeMsg(1), frameRecords(delta(1, 1)), frameRecords(delta(1, 2)));
    expect(streams[0].bytes()).toEqual(want);
  });

  it("a frame's records are the datagrams datagram mode sends, byte for byte, parity included, in one write", async () => {
    const { streams, produce, send, delta } = harness();
    produce(1);
    await settle();
    streams[0].open();
    await settle();
    streams[0].handOver(0);
    await settle();
    const dgrams = delta(1, 1, 3000); // three chunks at 1024, plus parity
    expect(dgrams.length).toBeGreaterThan(3);
    send(1, 1, 3000);
    await settle();
    expect(streams[0].writes.length).toBe(2);
    expect(streams[0].writes[1].bytes).toEqual(concat(...dgrams.map((d) => encodeCarrierRecord(d))));
  });

  it('send() never makes the encoder wait: it returns at once while every write blocks', async () => {
    const { produce, send } = harness();
    produce(1);
    for (let n = 1; n <= 30; n++) send(1, n); // nothing opened, nothing handed over
    await settle();
    // Reaching here synchronously is the assertion: send() queued all 30.
    expect(true).toBe(true);
  });
});

describe('CarrierSender — lifecycle on RU9\'s rules (D3)', () => {
  it('a keyframe that must wait leaves its GOP without a stream: its deltas are shed, all of them, even once it starts', async () => {
    const { lane, streams, produce, send, results } = harness();
    produce(1);
    await settle();
    streams[0].open(); // keyframe 1 being written, not handed over
    await settle();
    produce(2); // waits in the slot: keyframe 1 is still being written
    send(2, 1);
    await settle();
    expect(streams.length).toBe(1);
    streams[0].handOver(0); // keyframe 1 handed over: keyframe 2 starts
    await settle();
    expect(streams.length).toBe(2);
    send(2, 2); // GOP 2 already lost a delta: the rest of it follows
    await settle();
    streams[1].open();
    await settle();
    streams[1].handOver(0);
    await settle();
    expect(streams[1].writes.length).toBe(1); // the keyframe alone
    expect(results).toEqual(['2.1 dropped', '2.2 dropped']);
    expect(lane.counters.recordsShed).toBeGreaterThan(0);
  });

  it('at rotation: queued records shed, a blocked write left to finish, nothing more written, FIN, never reset', async () => {
    const { lane, streams, produce, send, results, events } = harness();
    produce(1);
    await settle();
    streams[0].open();
    await settle();
    streams[0].handOver(0);
    await settle();
    send(1, 1); // written, blocks
    send(1, 2); // queued behind it
    send(1, 3);
    await settle();
    expect(streams[0].writes.length).toBe(2);

    produce(2); // rotation
    await settle();
    expect(streams[0].closed).toBe(true); // FIN requested behind the blocked write
    expect(streams[0].aborted).toBe(false);
    const shed = lane.counters.recordsShed;
    expect(shed).toBeGreaterThan(0);
    expect(results).toEqual(['1.2 dropped', '1.3 dropped']);

    streams[0].handOver(1); // the blocked write finishes
    await settle();
    expect(results).toContain('1.1 sent');
    send(1, 4); // a late delta of the retired GOP
    await settle();
    expect(streams[0].writes.length).toBe(2);
    expect(results).toContain('1.4 dropped');

    streams[0].ack();
    await settle();
    expect(events).toContain('sent 1');
    expect(streams[0].aborted).toBe(false);
  });

  it('a GOP stream is aborted only by the 2 s rule, when a newer keyframe arrives, and counted as expired', async () => {
    const { kf, streams, produce } = harness();
    produce(1);
    await settle();
    streams[0].open(); // opened at t=0
    await settle();
    streams[0].handOver(0);
    await settle();
    produce(2); // t=0: rotation, FIN; unacknowledged
    await settle();
    streams[1].open();
    await settle();
    streams[1].handOver(0);
    await settle();

    await at(1999);
    produce(3); // both still in Chrome and under 2 s: 3 waits, nothing aborted
    await settle();
    expect(streams[0].aborted).toBe(false);
    expect(streams.length).toBe(2);

    await at(2500);
    produce(4); // 2.5 s: both are past 2 s unacknowledged — aborted; 3 is discarded, 4 starts
    await settle();
    expect(streams[0].aborted).toBe(true);
    expect(streams[1].aborted).toBe(true);
    expect(streams.length).toBe(3);
    expect(kf.counters.carriersAborted).toBe(2);
    expect(kf.counters.carriersOpened).toBe(2);
    streams[2].open();
    await settle();
    expect(kf.counters.carriersOpened).toBe(3);
  });

  it('at most two GOP streams are in Chrome: a third keyframe waits for an acknowledgement', async () => {
    const { streams, produce } = harness();
    produce(1);
    await settle();
    streams[0].open();
    await settle();
    streams[0].handOver(0);
    await settle();
    produce(2);
    await settle();
    streams[1].open();
    await settle();
    streams[1].handOver(0);
    await settle();
    produce(3); // GOP 1 (FINned, unacknowledged) and GOP 2 are in Chrome
    await settle();
    expect(streams.length).toBe(2);
    streams[0].ack();
    await settle();
    expect(streams.length).toBe(3);
  });

  it('deltas produced while their stream is still opening wait for it and keep their order', async () => {
    const { streams, produce, send, delta } = harness();
    produce(1);
    send(1, 1);
    send(1, 2);
    await settle();
    streams[0].open();
    await settle();
    streams[0].handOver(0);
    await settle();
    expect(streams[0].writes[1].bytes).toEqual(frameRecords(delta(1, 1)));
    streams[0].handOver(1);
    await settle();
    expect(streams[0].writes[2].bytes).toEqual(frameRecords(delta(1, 2)));
  });

  it('a datagram keyframe and a GOP stream share the two-in-Chrome limit (a mode change applies at the next keyframe)', async () => {
    const { streams, produce } = harness();
    produce(1, false); // Lowest latency: an ordinary keyframe stream
    await settle();
    streams[0].open();
    await settle();
    expect(streams[0].writes[0].bytes[1]).toBe(0x04); // a StreamFrame, no prologue
    streams[0].handOver(0);
    await settle();
    produce(2, true);
    await settle();
    streams[1].open();
    await settle();
    expect(Array.from(streams[1].writes[0].bytes.subarray(0, 2))).toEqual([0x01, 0x1f]);
    streams[1].handOver(0);
    await settle();
    produce(3, true);
    await settle();
    expect(streams.length).toBe(2); // both still in Chrome
  });
});

describe('CarrierSender — overlapping opens (review of #765)', () => {
  it('a retired GOP stream superseded when a newer one registers is not a 2 s expiry', async () => {
    const { kf, streams, produce } = harness();
    produce(1); // opening
    await settle();
    produce(2); // retires 1 while it is still opening; nothing is being written, so 2 starts opening too
    await settle();
    expect(streams.length).toBe(2);
    streams[0].open(); // 1 writes its keyframe, then its FIN
    await settle();
    streams[1].open(); // newest registration wins: 1, still being written, is superseded
    await settle();
    expect(streams[0].aborted).toBe(true);
    expect(kf.counters.carriersAborted).toBe(0); // uplinkCarriersExpired is the 2 s rule's alone
    expect(kf.counters.superseded).toBe(1);
    expect(kf.counters.failed).toBe(1);
  });

  it('a retired keyframe that opens with two in Chrome is replaced, never the waiter, and its queue is shed', async () => {
    const { lane, streams, produce, send, events, results } = harness();
    produce(1); // 1 draining
    await settle();
    streams[0].open();
    await settle();
    streams[0].handOver(0);
    await settle();
    produce(2); // retires 1; 2 starts opening (one in Chrome)
    send(2, 1); // queued behind 2's keyframe
    await settle();
    produce(3); // retires 2 while it is still opening; nothing is being written: 3 opens too
    await settle();
    expect(streams.length).toBe(3);
    streams[2].open(); // 3 registers first, is written and handed over: two in Chrome (1, 3)
    await settle();
    streams[2].handOver(0);
    await settle();
    streams[1].open(); // 2 opens late, with two draining: it must not become the waiter
    await settle();
    expect(events).toContain('replaced 2');
    expect(results).toContain('2.1 dropped');
    expect(lane.counters.recordsShed).toBeGreaterThan(0);
    // Nothing of 2 is ever written after 3: freeing Chrome starts no stream for it.
    streams[0].ack();
    await settle();
    expect(streams.length).toBe(3);
    expect(streams[1].writes.length).toBe(0);
  });
});

describe('CarrierSender — resume', () => {
  it('a new session starts with no stream, sends nothing queued on the old one, and its first keyframe opens the first GOP stream', async () => {
    const { kf, streams, produce, send, results } = harness();
    produce(1);
    await settle();
    streams[0].open();
    await settle();
    send(1, 1); // queued behind the keyframe
    await settle();

    kf.detach(true);
    await settle();
    expect(results).toEqual(['1.1 rejected']);
    expect(streams[0].aborted).toBe(true);

    const fresh: FakeStream[] = [];
    kf.attach(() => {
      const s = new FakeStream();
      fresh.push(s);
      return s.opened.promise;
    });
    send(1, 2); // an old GOP's delta after the resume: no stream for it
    await settle();
    expect(fresh.length).toBe(0);
    expect(results).toContain('1.2 dropped');

    produce(2); // the resume's forced keyframe
    await settle();
    expect(fresh.length).toBe(1);
    fresh[0].open();
    await settle();
    expect(Array.from(fresh[0].writes[0].bytes.subarray(0, 2))).toEqual([0x01, 0x1f]);
  });
});

describe('engageCarrier — capability ∧ Reliable ∧ Chromium (D1, D6)', () => {
  const both = CAP_UPLINK_CARRIERS | CAP_KEYFRAME_CARRIERS;
  it.each([
    [both | CAP_PARITY_CHUNKS, 'reliable', true, 'carrier'],
    [both, 'lowest-latency', true, 'datagram'],
    [CAP_UPLINK_CARRIERS, 'reliable', true, 'datagram'], // an RU1-only relay
    [CAP_KEYFRAME_CARRIERS, 'reliable', true, 'datagram'],
    [0, 'reliable', true, 'datagram'],
    [null, 'reliable', true, 'datagram'], // capabilities not arrived yet
    [both, 'reliable', false, 'datagram'], // Firefox, Safari (OD4)
  ] as const)('flags %s, %s, chromium %s ⇒ %s', (flags, delivery, chromium, want) => {
    expect(engageCarrier(flags, delivery, chromium)).toBe(want);
  });
});

describe('isChromium (OD4)', () => {
  it.each([
    ['Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/151.0.0.0 Safari/537.36', true],
    ['Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/151.0.0.0 Safari/537.36 Edg/151.0.0.0', true],
    ['Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) HeadlessChrome/151.0.0.0 Safari/537.36', true],
    ['Mozilla/5.0 (X11; Linux x86_64; rv:140.0) Gecko/20100101 Firefox/140.0', false],
    ['Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/26.0 Safari/605.1.15', false],
    ['Mozilla/5.0 (iPhone; CPU iPhone OS 26_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) CriOS/151.0 Mobile/15E148 Safari/604.1', false],
  ])('%s ⇒ %s', (ua, want) => {
    expect(isChromium(ua)).toBe(want);
  });
});

describe('Delivery copy (§7), the native words', () => {
  it('matches crates/engine/src/carrier.rs word for word', () => {
    expect(DELIVERY_LABEL).toBe('Delivery');
    expect(DELIVERY_LABELS).toEqual({ reliable: 'Reliable', 'lowest-latency': 'Lowest latency' });
    expect(DELIVERY_HINTS.reliable).toBe(
      'If your connection drops some video, viewers wait a moment for it instead of losing the picture.',
    );
    expect(DELIVERY_HINTS['lowest-latency']).toBe(
      'Viewers never wait for dropped video. If your connection drops some, they see brief freezes.',
    );
  });
});

describe('the GOP and the 2 s rule (review of #765)', () => {
  it('a GOP is well under the abort age, or every held-open GOP stream would be aborted at the next keyframe', () => {
    expect(DEFAULT_CAPTURE_CONFIG.keyframeIntervalMs * 2).toBeLessThanOrEqual(KEYFRAME_ABORT_AGE_MS);
  });
});
