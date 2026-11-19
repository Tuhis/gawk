// The striped transport in the deep-buffer layout (docs/91 D6–D8, D11): legs
// are j = 1 … N−1 on the primary's URL, StripeN on the wire is the width with
// the primary, and a leg's keyframe streams and carriers are read into the same
// handlers as the primary's, tagged with the leg's connection index. The live
// layout is covered, unchanged, by striped-transport.test.ts.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { LocalViewerTransport, maxLegs, stripeLegUrl } from './viewer-transport';
import type { KeyframeStreamFrame } from './connection';
import {
  MAX_STRIPE_LEGS,
  encodeCarrierPrologue,
  encodeCarrierRecord,
  encodeStreamFrame,
  encodeVideoChunk,
  parseStripeState,
} from './wire';

class FakeWT {
  static instances: FakeWT[] = [];
  static autoReady = true;

  url: string;
  ready: Promise<void>;
  readyResolve!: () => void;
  closedResolve!: (info?: unknown) => void;
  closed: Promise<unknown>;
  written: Uint8Array[] = [];
  closeCalled = false;
  pushStream: ((chunks: Uint8Array[]) => void) | null = null;
  datagrams: { maxDatagramSize: number; readable: ReadableStream<Uint8Array>; writable: WritableStream<BufferSource> };
  incomingUnidirectionalStreams: ReadableStream<ReadableStream<Uint8Array>>;

  constructor(url: string) {
    this.url = url;
    FakeWT.instances.push(this);
    this.ready = new Promise((r) => {
      this.readyResolve = r;
    });
    if (FakeWT.autoReady) this.readyResolve();
    this.closed = new Promise((r) => {
      this.closedResolve = (info?: unknown) => r(info ?? {});
    });
    this.datagrams = {
      maxDatagramSize: 1200,
      readable: new ReadableStream<Uint8Array>({ start() {} }),
      writable: new WritableStream<BufferSource>({
        write: (chunk) => {
          this.written.push(new Uint8Array(chunk as Uint8Array));
        },
      }),
    };
    this.incomingUnidirectionalStreams = new ReadableStream<ReadableStream<Uint8Array>>({
      start: (controller) => {
        this.pushStream = (chunks) =>
          controller.enqueue(
            new ReadableStream<Uint8Array>({
              start(inner) {
                for (const c of chunks) inner.enqueue(c);
                inner.close();
              },
            }),
          );
      },
    });
  }

  close(): void {
    this.closeCalled = true;
    this.closedResolve({ reason: 'locally closed' });
  }

  static legs(): FakeWT[] {
    return FakeWT.instances.filter((i) => i.url.includes('stripe='));
  }
}

const PRIMARY =
  'https://relay.test:4433/subscribe/ABC123?delivery=reliable&buffer=3000&parity=1&owner=aabbccdd00112233&lr=1';

function stripeStates(wt: FakeWT): { striped: boolean; stripeN: number }[] {
  return wt.written.filter((d) => d.length === 5 && d[1] === 0x10).map((d) => parseStripeState(d));
}

async function flush(): Promise<void> {
  for (let i = 0; i < 10; i++) await Promise.resolve();
  await new Promise((r) => setTimeout(r, 0));
}

let transport: LocalViewerTransport;
let datagrams: { data: Uint8Array; conn: number | undefined }[];
let keyframes: { kf: KeyframeStreamFrame; conn: number | undefined }[];
let stripeChanges: number[];

async function connect(url = PRIMARY): Promise<void> {
  transport = new LocalViewerTransport(url, {});
  await transport.connect({
    onDatagram: (data, conn) => datagrams.push({ data, conn }),
    onKeyframe: (kf, conn) => keyframes.push({ kf, conn }),
    onStripeChange: (n) => stripeChanges.push(n),
    onClosed: () => {},
  });
}

beforeEach(() => {
  FakeWT.instances = [];
  FakeWT.autoReady = true;
  datagrams = [];
  keyframes = [];
  stripeChanges = [];
  vi.stubGlobal('WebTransport', FakeWT);
});

afterEach(() => {
  transport?.close();
  vi.unstubAllGlobals();
});

describe('stripeLegUrl', () => {
  const cases: { name: string; member: number; stripeN: number }[] = [
    { name: 'the first deep-buffer leg of ×2', member: 1, stripeN: 2 },
    { name: 'the last deep-buffer leg of ×4', member: 3, stripeN: 4 },
    { name: 'a live leg', member: 0, stripeN: 4 },
  ];
  for (const c of cases) {
    it(`${c.name}: the primary's parameters plus stripe and leg`, () => {
      const u = new URL(stripeLegUrl(PRIMARY, c.member, c.stripeN));
      expect(u.pathname).toBe('/subscribe/ABC123');
      expect(Object.fromEntries(u.searchParams)).toEqual({
        delivery: 'reliable',
        buffer: '3000',
        parity: '1',
        owner: 'aabbccdd00112233',
        lr: '1',
        stripe: String(c.stripeN),
        leg: String(c.member),
      });
    });
  }

  it('overwrites rather than duplicates a stripe already on the URL', () => {
    const u = new URL(stripeLegUrl(`${PRIMARY}&stripe=2&leg=1`, 2, 3));
    expect(u.searchParams.getAll('stripe')).toEqual(['3']);
    expect(u.searchParams.getAll('leg')).toEqual(['2']);
  });
});

describe('LocalViewerTransport in the deep-buffer layout (docs/91)', () => {
  it('dials legs from 1, and tells the primary the width with itself (D6, D7)', async () => {
    await connect();
    transport.setStripe(2, 'dvr');
    await flush();
    const legs = FakeWT.legs();
    expect(legs.map((l) => new URL(l.url).searchParams.get('leg'))).toEqual(['1', '2']);
    expect(legs.every((l) => new URL(l.url).searchParams.get('stripe') === '3')).toBe(true);
    expect(legs.every((l) => new URL(l.url).searchParams.get('buffer') === '3000')).toBe(true);
    expect(stripeStates(FakeWT.instances[0])).toEqual([{ striped: true, stripeN: 3 }]);
    for (const leg of legs) expect(stripeStates(leg)).toEqual([{ striped: true, stripeN: 3 }]);
    expect(stripeChanges).toEqual([2]);
    expect(transport.sampleStripe()).toMatchObject({ active: 2, target: 2 });
  });

  it(`caps the legs at ${MAX_STRIPE_LEGS - 1}: ${MAX_STRIPE_LEGS} connections with the primary (D12)`, async () => {
    expect(maxLegs('dvr')).toBe(MAX_STRIPE_LEGS - 1);
    expect(maxLegs('live')).toBe(MAX_STRIPE_LEGS);
    await connect();
    transport.setStripe(99, 'dvr');
    await flush();
    expect(FakeWT.legs().map((l) => new URL(l.url).searchParams.get('leg'))).toEqual(['1', '2', '3']);
    expect(stripeStates(FakeWT.instances[0]).at(-1)).toEqual({ striped: true, stripeN: MAX_STRIPE_LEGS });
  });

  it("reads a leg's keyframe stream and carrier into the same handlers, tagged with its index", async () => {
    await connect();
    transport.setStripe(1, 'dvr');
    await flush();
    const [leg] = FakeWT.legs();
    const payload = new Uint8Array([9, 9, 9]);
    leg.pushStream?.([encodeStreamFrame({ keyframe: true, frameId: 30, timestampUs: 500_000n }, new Uint8Array(0), payload)]);
    const record = encodeVideoChunk(
      { keyframe: false, frameId: 31, chunkIndex: 0, chunkCount: 1, timestampUs: 516_667n },
      new Uint8Array([1, 2, 3]),
    );
    leg.pushStream?.([encodeCarrierPrologue(), encodeCarrierRecord(record)]);
    await flush();
    expect(keyframes).toHaveLength(1);
    expect(keyframes[0].kf.frameId).toBe(30);
    expect(keyframes[0].conn).toBe(1);
    expect(datagrams).toHaveLength(1);
    expect(datagrams[0].conn).toBe(1);
    expect(Array.from(datagrams[0].data)).toEqual(Array.from(record));
    // Carrier tallies count every connection's carriers.
    expect(transport.sampleCarrierStats()).toMatchObject({ streamsOpened: 1, recordsReceived: 1 });
  });

  it('grows make-before-break: the new set is dialed at the new width before the primary hears it (D11)', async () => {
    await connect();
    transport.setStripe(1, 'dvr');
    await flush();
    const [old] = FakeWT.legs();
    transport.setStripe(2, 'dvr');
    await flush();
    const fresh = FakeWT.legs().slice(1);
    expect(fresh.map((l) => new URL(l.url).searchParams.get('leg'))).toEqual(['1', '2']);
    expect(fresh.every((l) => new URL(l.url).searchParams.get('stripe') === '3')).toBe(true);
    expect(old.closeCalled).toBe(false); // it drains; see below
    expect(stripeStates(FakeWT.instances[0])).toEqual([
      { striped: true, stripeN: 2 },
      { striped: true, stripeN: 3 },
    ]);
  });

  // Review round 2, finding 2: asking for the width already up cancels a
  // transition still dialing, so it cannot commit later and leave the
  // transport wider than asked.
  it('a request for the active width cancels a transition still in flight', async () => {
    await connect();
    transport.setStripe(1, 'dvr');
    await flush();
    FakeWT.autoReady = false;
    transport.setStripe(2, 'dvr');
    await flush();
    const pending = FakeWT.legs().slice(1);
    expect(pending).toHaveLength(2);
    transport.setStripe(1, 'dvr');
    for (const l of pending) l.readyResolve();
    await flush();
    expect(transport.sampleStripe()).toMatchObject({ active: 1, target: 1 });
    expect(pending.every((l) => l.closeCalled)).toBe(true);
    expect(stripeStates(FakeWT.instances[0])).toEqual([{ striped: true, stripeN: 2 }]);
    expect(stripeChanges).toEqual([1]);
  });

  it('a disengage cancels a first engage still in flight', async () => {
    await connect();
    FakeWT.autoReady = false;
    transport.setStripe(1, 'dvr');
    await flush();
    const [pending] = FakeWT.legs();
    transport.setStripe(0, 'dvr');
    pending.readyResolve();
    await flush();
    expect(transport.sampleStripe()).toMatchObject({ active: 0 });
    expect(pending.closeCalled).toBe(true);
    expect(stripeChanges).toEqual([]);
  });

  it('a leg death hands every GOP back to the primary', async () => {
    await connect();
    transport.setStripe(1, 'dvr');
    await flush();
    FakeWT.legs()[0].closedResolve({ reason: 'gone' });
    await flush();
    expect(stripeStates(FakeWT.instances[0]).at(-1)).toEqual({ striped: false, stripeN: 0 });
    expect(stripeChanges).toEqual([1, 0]);
  });

  it('keeps live legs off the stream reader (R30 unchanged)', async () => {
    await connect('https://relay.test:4433/subscribe/ABC123?owner=aabbccdd00112233');
    transport.setStripe(2);
    await flush();
    const legs = FakeWT.legs();
    expect(legs.map((l) => new URL(l.url).searchParams.get('leg'))).toEqual(['0', '1']);
    expect(stripeStates(FakeWT.instances[0])).toEqual([{ striped: true, stripeN: 2 }]);
    legs[0].pushStream?.([encodeStreamFrame({ keyframe: true, frameId: 30, timestampUs: 0n }, new Uint8Array(0), new Uint8Array([1]))]);
    await flush();
    expect(keyframes).toHaveLength(0);
  });

  // Review round 1, finding 1: a superseded deep-buffer leg still holds GOPs
  // the primary has already skipped, so it drains for the DVR buffer + 1 s
  // before it is closed. Duplicates are the reorder buffer's to drop.
  describe('drains superseded legs (buffer + 1 s)', () => {
    const DRAIN_MS = 3000 + 1000; // PRIMARY carries buffer=3000

    beforeEach(() => {
      vi.useFakeTimers();
    });
    afterEach(() => {
      vi.useRealTimers();
    });
    const tick = (ms = 0) => vi.advanceTimersByTimeAsync(ms);
    const kfOn = (leg: FakeWT, frameId: number) =>
      leg.pushStream?.([
        encodeStreamFrame({ keyframe: true, frameId, timestampUs: BigInt(frameId) * 1000n }, new Uint8Array(0), new Uint8Array([1])),
      ]);

    const transitions: { name: string; from: number; to: number }[] = [
      { name: 'a grow', from: 1, to: 2 },
      { name: 'a step down', from: 2, to: 1 },
      { name: 'a disengage', from: 1, to: 0 },
    ];
    for (const t of transitions) {
      it(`keeps the old legs reading and heartbeating through ${t.name}, then closes them`, async () => {
        await connect();
        transport.setStripe(t.from, 'dvr');
        await tick();
        const old = FakeWT.legs();
        transport.setStripe(t.to, 'dvr');
        await tick();
        expect(old.every((l) => !l.closeCalled)).toBe(true);
        expect(transport.sampleStripe()).toMatchObject({ active: t.to });

        kfOn(old[0], 90);
        await tick();
        expect(keyframes.map((k) => [k.kf.frameId, k.conn])).toContainEqual([90, 1]);

        const beats = stripeStates(old[0]).length;
        await tick(2000);
        expect(stripeStates(old[0]).length).toBeGreaterThan(beats);
        // A draining leg's heartbeat is the drain signal itself (round 2).
        expect(stripeStates(old[0]).at(-1)).toEqual({ striped: false, stripeN: 0 });

        await tick(DRAIN_MS);
        expect(old.every((l) => l.closeCalled)).toBe(true);
        expect(stripeChanges.at(-1)).toBe(t.to);
      });
    }

    // Review round 2, finding 3: the relay's drain signal is Striped=false on
    // the superseded leg itself; it then serves the leg's backlog and closes
    // it. Sent a few times, since a datagram can be lost.
    it('tells a superseded leg to drain, three times, and never the live legs', async () => {
      await connect();
      transport.setStripe(1, 'dvr');
      await tick();
      const [old] = FakeWT.legs();
      transport.setStripe(2, 'dvr');
      await tick(350);
      const drains = stripeStates(old).filter((st) => !st.striped);
      expect(drains.length).toBeGreaterThanOrEqual(3);
      expect(drains.every((st) => st.stripeN === 0)).toBe(true);
      for (const leg of FakeWT.legs().slice(1)) {
        expect(stripeStates(leg).every((st) => st.striped)).toBe(true);
      }
      // The primary is never told to unstripe by a grow.
      expect(stripeStates(FakeWT.instances[0]).every((st) => st.striped)).toBe(true);
    });

    it('stops with a leg the relay closed after draining it: not a leg death', async () => {
      await connect();
      transport.setStripe(1, 'dvr');
      await tick();
      const [old] = FakeWT.legs();
      transport.setStripe(2, 'dvr');
      await tick(500);
      old.closedResolve({ reason: 'drained' });
      await tick();
      const writes = old.written.length;
      await tick(DRAIN_MS);
      expect(old.written.length).toBe(writes);
      expect(transport.sampleStripe()).toMatchObject({ active: 2, legDeaths: 0 });
      expect(stripeChanges).toEqual([1, 2]);
    });

    // Review round 2, finding 4: a leg-death disengage stops the stripe
    // refresh, but legs still draining from an earlier transition keep their
    // own heartbeat, or the relay's lease reaps them mid-drain.
    it('keeps draining legs heartbeating through a leg-death disengage', async () => {
      await connect();
      transport.setStripe(1, 'dvr');
      await tick();
      const [old] = FakeWT.legs();
      transport.setStripe(2, 'dvr');
      await tick();
      FakeWT.legs()[1].closedResolve({ reason: 'gone' }); // a live leg dies
      await tick();
      expect(transport.sampleStripe()).toMatchObject({ active: 0, legDeaths: 1 });
      const beats = stripeStates(old).length;
      await tick(2500);
      expect(stripeStates(old).length).toBeGreaterThanOrEqual(beats + 2);
    });

    it("ignores a draining leg's death: it is no longer part of the cover", async () => {
      await connect();
      transport.setStripe(1, 'dvr');
      await tick();
      const [old] = FakeWT.legs();
      transport.setStripe(2, 'dvr');
      await tick();
      old.closedResolve({ reason: 'gone' });
      await tick();
      expect(transport.sampleStripe()).toMatchObject({ active: 2, legDeaths: 0 });
      expect(stripeChanges).toEqual([1, 2]);
    });

    it('a leg death still tears everything down at once', async () => {
      await connect();
      transport.setStripe(2, 'dvr');
      await tick();
      const legs = FakeWT.legs();
      legs[0].closedResolve({ reason: 'gone' });
      await tick();
      expect(legs[1].closeCalled).toBe(true);
    });

    it('close() closes draining legs too', async () => {
      await connect();
      transport.setStripe(1, 'dvr');
      await tick();
      const [old] = FakeWT.legs();
      transport.setStripe(2, 'dvr');
      await tick();
      transport.close();
      expect(old.closeCalled).toBe(true);
    });

    it('live legs still close as soon as the new set is up (R30 unchanged)', async () => {
      await connect('https://relay.test:4433/subscribe/ABC123?owner=aabbccdd00112233');
      transport.setStripe(1);
      await tick();
      const [old] = FakeWT.legs();
      transport.setStripe(2);
      await tick();
      expect(old.closeCalled).toBe(true);
    });
  });
});
