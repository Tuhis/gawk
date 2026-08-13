// R30 ST4 (docs/35 §5.6): the striped transport's transition protocol.
// Everything here runs against a controllable fake WebTransport, because the
// properties under test are ORDERING properties: suppression is sent only
// after a complete leg set is up (duplicates, never holes), a leg death
// releases the primary before anything else, and the unstriped path stays
// byte-identical (no legs dialed, no 0x10 written).

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { LocalViewerTransport } from './viewer-transport';
import { parseStripeState, MAX_STRIPE_LEGS } from './wire';

// A controllable fake WebTransport. Every construction is recorded with its
// URL; ready/closed are resolvable from the test; datagram writes are
// captured; incoming datagrams are pushable.
class FakeWT {
  static instances: FakeWT[] = [];
  static autoReady = true;

  url: string;
  readyResolve!: () => void;
  ready: Promise<void>;
  closedResolve!: (info?: unknown) => void;
  closed: Promise<unknown>;
  closedSettled = false;
  written: Uint8Array[] = [];
  incomingPush: ((d: Uint8Array) => void) | null = null;
  incomingClose: (() => void) | null = null;
  datagrams: {
    maxDatagramSize: number;
    readable: ReadableStream<Uint8Array>;
    writable: WritableStream<BufferSource>;
  };
  incomingUnidirectionalStreams = new ReadableStream<ReadableStream<Uint8Array>>({
    start() {}, // never yields; never closes — like a quiet live session
  });
  closeCalled = false;

  constructor(url: string) {
    this.url = url;
    FakeWT.instances.push(this);
    this.ready = new Promise((r) => {
      this.readyResolve = r;
    });
    if (FakeWT.autoReady) this.readyResolve();
    this.closed = new Promise((r) => {
      this.closedResolve = (info?: unknown) => {
        this.closedSettled = true;
        r(info ?? {});
      };
    });
    this.datagrams = {
      maxDatagramSize: 1200,
      readable: new ReadableStream<Uint8Array>({
        start: (controller) => {
          this.incomingPush = (d) => controller.enqueue(d);
          this.incomingClose = () => {
            try {
              controller.close();
            } catch {
              /* already closed */
            }
          };
        },
      }),
      writable: new WritableStream<BufferSource>({
        write: (chunk) => {
          this.written.push(new Uint8Array(chunk as ArrayBuffer | Uint8Array as Uint8Array));
        },
      }),
    };
  }

  close(): void {
    this.closeCalled = true;
    this.incomingClose?.();
    this.closedResolve({ closeCode: undefined, reason: 'locally closed' });
  }

  static reset(): void {
    FakeWT.instances = [];
    FakeWT.autoReady = true;
  }

  static primary(): FakeWT {
    return FakeWT.instances[0];
  }

  static legs(): FakeWT[] {
    return FakeWT.instances.filter((i) => i.url.includes('stripe='));
  }
}

// The 0x10 messages written on the primary, in order.
function stripeStatesWritten(primary: FakeWT): { striped: boolean; stripeN: number }[] {
  const out: { striped: boolean; stripeN: number }[] = [];
  for (const d of primary.written) {
    if (d.length === 5 && d[1] === 0x10) out.push(parseStripeState(d));
  }
  return out;
}

async function flush(): Promise<void> {
  // Let queued microtasks (dials, stream plumbing) settle.
  for (let i = 0; i < 10; i++) await Promise.resolve();
  await new Promise((r) => setTimeout(r, 0));
}

let transport: LocalViewerTransport;
const received: Uint8Array[] = [];
let stripeChanges: number[] = [];

async function connectTransport(): Promise<void> {
  transport = new LocalViewerTransport('https://relay.test:4433/subscribe/ABC123', {});
  await transport.connect({
    onDatagram: (d) => received.push(d),
    onKeyframe: () => {},
    onStripeChange: (n) => stripeChanges.push(n),
    onClosed: () => {},
  });
}

beforeEach(() => {
  FakeWT.reset();
  received.length = 0;
  stripeChanges = [];
  vi.stubGlobal(
    'WebTransport',
    class extends FakeWT {
      constructor(url: string) {
        super(url);
      }
    },
  );
});

afterEach(() => {
  transport?.close();
  vi.unstubAllGlobals();
});

describe('LocalViewerTransport striping (R30)', () => {
  it('stays byte-identical while unstriped: one session, no 0x10', async () => {
    await connectTransport();
    await flush();
    expect(FakeWT.instances).toHaveLength(1);
    expect(stripeStatesWritten(FakeWT.primary())).toHaveLength(0);
  });

  it('dials the full leg set before suppressing the primary', async () => {
    await connectTransport();
    // Legs must not auto-ready: the ordering property is the test.
    FakeWT.autoReady = false;
    transport.setStripe(2);
    await flush();
    expect(FakeWT.legs()).toHaveLength(2);
    expect(FakeWT.legs().map((l) => new URL(l.url).searchParams.get('leg')).sort()).toEqual(['0', '1']);
    expect(FakeWT.legs().every((l) => new URL(l.url).searchParams.get('stripe') === '2')).toBe(true);
    // One leg up, one still dialing: no suppression yet — the primary is the
    // only complete cover and must keep flowing.
    FakeWT.legs()[0].readyResolve();
    await flush();
    expect(stripeStatesWritten(FakeWT.primary())).toHaveLength(0);
    // Second leg up: NOW the suppression goes out and the change is reported.
    FakeWT.legs()[1].readyResolve();
    await flush();
    expect(stripeStatesWritten(FakeWT.primary())).toEqual([{ striped: true, stripeN: 2 }]);
    expect(stripeChanges).toEqual([2]);
    expect(transport.sampleStripe()).toMatchObject({ active: 2, target: 2 });
  });

  it('merges leg datagrams into the same handler as the primary', async () => {
    await connectTransport();
    transport.setStripe(2);
    await flush();
    const chunk = new Uint8Array([1, 1, 0, 0, 0, 0, 0, 5, 0, 1, 0, 2, 0, 0, 0, 0, 0, 0, 0, 9, 42]);
    FakeWT.legs()[1].incomingPush?.(chunk);
    await flush();
    expect(received.some((d) => d.length === chunk.length && d[20] === 42)).toBe(true);
  });

  it('grows make-before-break: a fresh complete set, old legs closed after, suppression never released', async () => {
    await connectTransport();
    transport.setStripe(2);
    await flush();
    const oldLegs = FakeWT.legs();
    transport.setStripe(3);
    await flush();
    const allLegs = FakeWT.legs();
    expect(allLegs).toHaveLength(5); // 2 old + 3 fresh
    const freshLegs = allLegs.slice(2);
    expect(freshLegs.every((l) => new URL(l.url).searchParams.get('stripe') === '3')).toBe(true);
    expect(oldLegs.every((l) => l.closeCalled)).toBe(true);
    // The 0x10 sequence never contains an unstripe: suppression stays armed
    // through the grow (a release would re-burst the primary mid-transition).
    const states = stripeStatesWritten(FakeWT.primary());
    expect(states.every((s) => s.striped)).toBe(true);
    expect(states.at(-1)).toEqual({ striped: true, stripeN: 3 });
    expect(stripeChanges).toEqual([2, 3]);
  });

  it('a failed dial abandons the transition and keeps the current state', async () => {
    await connectTransport();
    transport.setStripe(2);
    await flush();
    expect(stripeChanges).toEqual([2]);
    // The grow's dials reject (caps pressure: the relay 429s the leg).
    vi.stubGlobal(
      'WebTransport',
      class {
        constructor() {
          throw new Error('429');
        }
      },
    );
    transport.setStripe(3);
    await flush();
    expect(transport.sampleStripe()).toMatchObject({ active: 2, target: 3, legDialFailures: 1 });
    expect(stripeChanges).toEqual([2]); // no change reported — none happened
    const states = stripeStatesWritten(FakeWT.primary());
    expect(states.every((s) => s.striped)).toBe(true); // primary stays covered by the old set
  });

  it('a leg death releases the primary immediately and tears the stripe down', async () => {
    await connectTransport();
    transport.setStripe(2);
    await flush();
    const [leg0, leg1] = FakeWT.legs();
    leg1.closedResolve({ reason: 'gone' });
    await flush();
    const states = stripeStatesWritten(FakeWT.primary());
    expect(states.at(-1)).toEqual({ striped: false, stripeN: 0 });
    expect(leg0.closeCalled).toBe(true);
    expect(stripeChanges).toEqual([2, 0]);
    expect(transport.sampleStripe()).toMatchObject({ active: 0, legDeaths: 1 });
  });

  it('re-sends the unstripe as a burst (a lost release must not cost the TTL)', async () => {
    vi.useFakeTimers();
    try {
      await connectTransport();
      transport.setStripe(2);
      await vi.runOnlyPendingTimersAsync();
      transport.setStripe(0);
      await vi.advanceTimersByTimeAsync(500);
      const releases = stripeStatesWritten(FakeWT.primary()).filter((s) => !s.striped);
      expect(releases.length).toBeGreaterThanOrEqual(4);
    } finally {
      vi.useRealTimers();
    }
  });

  it('refreshes the striped level at 1 Hz while engaged', async () => {
    vi.useFakeTimers();
    try {
      await connectTransport();
      transport.setStripe(2);
      await vi.runOnlyPendingTimersAsync();
      await vi.advanceTimersByTimeAsync(3100);
      const striped = stripeStatesWritten(FakeWT.primary()).filter((s) => s.striped);
      expect(striped.length).toBeGreaterThanOrEqual(4); // engage + ≥3 refreshes
    } finally {
      vi.useRealTimers();
    }
  });

  it('clamps the target to MAX_STRIPE_LEGS', async () => {
    await connectTransport();
    transport.setStripe(99);
    await flush();
    expect(FakeWT.legs()).toHaveLength(MAX_STRIPE_LEGS);
  });

  it('close() tears every leg down', async () => {
    await connectTransport();
    transport.setStripe(2);
    await flush();
    const legs = FakeWT.legs();
    transport.close();
    expect(legs.every((l) => l.closeCalled)).toBe(true);
  });

  it('legs inherit the primary dial\'s ?owner= token (docs/35 §14)', async () => {
    // The token is the relay's only handle tying one viewer's sessions
    // together (a leg dialed without it is rejected 400), and it must be the
    // SAME token on every session of the attempt: dialLeg copies the primary
    // URL, params included, so nothing here mints a second identity.
    transport = new LocalViewerTransport(
      'https://relay.test:4433/subscribe/ABC123?owner=aabbccdd00112233',
      {},
    );
    await transport.connect({
      onDatagram: (d) => received.push(d),
      onKeyframe: () => {},
      onStripeChange: (n) => stripeChanges.push(n),
      onClosed: () => {},
    });
    transport.setStripe(2);
    await flush();
    expect(FakeWT.legs()).toHaveLength(2);
    for (const l of FakeWT.legs()) {
      expect(new URL(l.url).searchParams.get('owner')).toBe('aabbccdd00112233');
    }
  });

  it('heartbeats each leg at 1 Hz while striped, and stops with the stripe (docs/35 §14)', async () => {
    // The relay reaps a leg after StripeLegLease (20 s) without any inbound
    // datagram — the cross-pod orphan backstop — so a live stripe must keep
    // every leg's lease renewed. The heartbeat is the same 0x10 the primary
    // refreshes with, sent on the legs' own sessions.
    vi.useFakeTimers();
    try {
      await connectTransport();
      transport.setStripe(2);
      await vi.runOnlyPendingTimersAsync();
      await vi.advanceTimersByTimeAsync(3100);
      for (const leg of FakeWT.legs()) {
        const beats = stripeStatesWritten(leg);
        expect(beats.length).toBeGreaterThanOrEqual(4); // engage + ≥3 refreshes
        expect(beats.every((s) => s.striped && s.stripeN === 2)).toBe(true);
      }
      // Disengage: heartbeats stop with the stripe — a torn-down attempt
      // must not keep a lease alive (the lease exists to end exactly the
      // legs nobody is renewing).
      transport.setStripe(0);
      await vi.advanceTimersByTimeAsync(100);
      const counts = FakeWT.legs().map((l) => stripeStatesWritten(l).length);
      await vi.advanceTimersByTimeAsync(3000);
      expect(FakeWT.legs().map((l) => stripeStatesWritten(l).length)).toEqual(counts);
    } finally {
      vi.useRealTimers();
    }
  });
});
