// The pipeline's striped deep-buffer gate (docs/91 SG2): legs are asked for
// only on a deep-buffer session, against a relay advertising CAP_STRIPED_DVR
// that granted the ring, with Striping not Off — and in the deep-buffer
// layout. Live-edge striping keeps asking exactly as before. The transport is
// a fake; what is under test is what the pipeline tells it.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

vi.mock('../media/decoder', () => ({
  Decoder: class {
    queueSize = 0;
    configure = vi.fn();
    decode = vi.fn();
    close = vi.fn(() => Promise.resolve());
  },
}));

import { ViewerPipeline, type ViewerStats } from './viewer';
import { setViewerDeliveryMode } from './playout';
import { setStripeMode, type StripeMode } from './stripe';
import { CAP_PARITY_CHUNKS, CAP_STRIPED_DELIVERY, CAP_STRIPED_DVR } from './parity';
import type { StripeLayout, ViewerTransport, ViewerTransportCallbacks } from './viewer-transport';
import { encodeDeliveryAck, encodeVideoChunk, type DeliveryServedMode } from './wire';
import { DVR_STRIPE_DIAL_TIMEOUT_MS, DVR_STRIPE_REENGAGE_BACKOFF_MS } from './dvr-stripe';
import type { ViewerDeliveryMode } from './resilient';

interface Harness {
  pipeline: ViewerPipeline;
  // What the fake transport reports from sampleStripe().
  stripeSample: { active: number; target: number; legDials: number; legDialFailures: number; legDeaths: number };
  stripeCalls: [number, StripeLayout | undefined][];
  cb: ViewerTransportCallbacks;
  stats: ViewerStats[];
}

async function startPipeline(delivery: ViewerDeliveryMode): Promise<Harness> {
  const stripeCalls: Harness['stripeCalls'] = [];
  const stats: ViewerStats[] = [];
  let cb: ViewerTransportCallbacks | null = null;
  const stripeSample = { active: 0, target: 0, legDials: 0, legDialFailures: 0, legDeaths: 0 };
  const transport: ViewerTransport = {
    sampleStripe: () => ({ ...stripeSample }),
    kind: 'in-process',
    connect: async (c) => {
      cb = c;
    },
    sampleConnectionStats: () => null,
    sampleTimeSync: () => null,
    setStripe: (n, layout) => {
      stripeCalls.push([n, layout]);
      // Like the real transport: the sample's target is the last request.
      stripeSample.target = n;
    },
    close: () => {},
  };
  setViewerDeliveryMode(delivery);
  const pipeline = new ViewerPipeline(
    'https://relay.test:4433',
    'K7XQ2M',
    delivery === 'live' ? {} : { deliveryMode: 'reliable' },
    {
      onDecodedFrame: () => {},
      onConfig: () => {},
      onStats: (s) => stats.push(s),
      onError: () => {},
      onEnded: () => {},
    },
    null,
    () => transport,
  );
  await pipeline.start();
  return { pipeline, stripeSample, stripeCalls, cb: cb!, stats };
}

beforeEach(() => {
  vi.useFakeTimers({
    toFake: ['setTimeout', 'clearTimeout', 'setInterval', 'clearInterval', 'performance'],
  });
});

afterEach(() => {
  vi.useRealTimers();
  setViewerDeliveryMode('live');
  setStripeMode('auto');
});

describe('ViewerPipeline striped deep-buffer gate (docs/91 SG2)', () => {
  const cases: {
    name: string;
    delivery: ViewerDeliveryMode;
    flags: number;
    ack: DeliveryServedMode | null;
    mode: StripeMode;
    want: [number, StripeLayout | undefined][];
  }[] = [
    {
      name: 'deep + CAP_STRIPED_DVR + granted + On: one leg, deep-buffer layout',
      delivery: 'deep',
      flags: CAP_STRIPED_DVR,
      ack: 'dvr',
      mode: 'on',
      want: [[1, 'dvr']],
    },
    {
      name: 'deep without CAP_STRIPED_DVR: never',
      delivery: 'deep',
      flags: CAP_PARITY_CHUNKS | CAP_STRIPED_DELIVERY,
      ack: 'dvr',
      mode: 'on',
      want: [],
    },
    {
      name: 'deep with Striping Off: never',
      delivery: 'deep',
      flags: CAP_STRIPED_DVR,
      ack: 'dvr',
      mode: 'off',
      want: [],
    },
    {
      name: 'deep whose ring the relay denied: never',
      delivery: 'deep',
      flags: CAP_STRIPED_DVR,
      ack: 'reliable',
      mode: 'on',
      want: [],
    },
    {
      name: 'deep before the DeliveryAck: never',
      delivery: 'deep',
      flags: CAP_STRIPED_DVR,
      ack: null,
      mode: 'on',
      want: [],
    },
    {
      name: 'Smoother (reliable, no ring) with every bit: never (OD4)',
      delivery: 'resilient',
      flags: CAP_STRIPED_DELIVERY | CAP_STRIPED_DVR,
      ack: 'reliable',
      mode: 'on',
      want: [],
    },
    {
      name: 'live edge keeps R30: chunk legs, no layout argument',
      delivery: 'live',
      flags: CAP_STRIPED_DELIVERY | CAP_STRIPED_DVR,
      ack: null,
      mode: 'on',
      want: [[4, undefined]],
    },
  ];

  for (const c of cases) {
    it(c.name, async () => {
      setStripeMode(c.mode);
      const h = await startPipeline(c.delivery);
      h.cb.onRelayCapabilities?.({ flags: c.flags, parityLevel: 0 });
      if (c.ack) h.cb.onDatagram(encodeDeliveryAck(c.ack, c.ack === 'dvr' ? 3000 : 0));
      await vi.advanceTimersByTimeAsync(1_500);
      expect(h.stripeCalls).toEqual(c.want);
      await h.pipeline.stop();
    });
  }

  it('reports the width and per-connection bytes on a deep-buffer session only', async () => {
    setStripeMode('on');
    const h = await startPipeline('deep');
    h.cb.onRelayCapabilities?.({ flags: CAP_STRIPED_DVR, parityLevel: 0 });
    h.cb.onDatagram(encodeDeliveryAck('dvr', 3000));
    await vi.advanceTimersByTimeAsync(600);
    expect(h.stats.at(-1)).toMatchObject({
      stripeDvrCapable: true,
      dvrStripeWidth: 1,
      connectionBytesReceived: [0],
    });

    h.cb.onStripeChange?.(1);
    const kf = (frameId: number, bytes: number) => ({
      frameId,
      timestampUs: BigInt(frameId) * 16_667n,
      config: null,
      data: new Uint8Array(bytes),
      streamBytes: bytes + 20,
    });
    h.cb.onKeyframe(kf(30, 100));
    h.cb.onKeyframe(kf(60, 200), 1);
    await vi.advanceTimersByTimeAsync(500);
    expect(h.stats.at(-1)).toMatchObject({
      dvrStripeWidth: 2,
      connectionBytesReceived: [120, 220],
    });

    // Back to one connection: the leg's slot leaves the report.
    h.cb.onStripeChange?.(0);
    await vi.advanceTimersByTimeAsync(500);
    expect(h.stats.at(-1)).toMatchObject({ dvrStripeWidth: 1, connectionBytesReceived: [120] });
    await h.pipeline.stop();
  });

  it('leaves the deep-buffer fields off a live-edge session', async () => {
    const h = await startPipeline('live');
    h.cb.onRelayCapabilities?.({ flags: CAP_STRIPED_DVR, parityLevel: 0 });
    await vi.advanceTimersByTimeAsync(600);
    const s = h.stats.at(-1)!;
    expect(s.stripeDvrCapable).toBe(true);
    expect(s.dvrStripeWidth).toBeUndefined();
    expect(s.connectionBytesReceived).toBeUndefined();
    await h.pipeline.stop();
  });

  // Review round 1, finding 4: a failed DVR leg dial (a 429 at capacity) is
  // retried after the leg-death backoff, because the policy's width never
  // changed and nothing else would ask again.
  it('retries a failed deep-buffer leg dial after the backoff, and stops once the legs are up', async () => {
    setStripeMode('on');
    const h = await startPipeline('deep');
    h.cb.onRelayCapabilities?.({ flags: CAP_STRIPED_DVR, parityLevel: 0 });
    h.cb.onDatagram(encodeDeliveryAck('dvr', 3000));
    await vi.advanceTimersByTimeAsync(500);
    expect(h.stripeCalls).toEqual([[1, 'dvr']]);

    // The transport reports the transition failed: still no legs.
    h.stripeSample.target = 1;
    h.stripeSample.legDialFailures = 1;
    await vi.advanceTimersByTimeAsync(DVR_STRIPE_REENGAGE_BACKOFF_MS - 1000);
    expect(h.stripeCalls).toEqual([[1, 'dvr']]);
    await vi.advanceTimersByTimeAsync(1500);
    expect(h.stripeCalls).toEqual([[1, 'dvr'], [1, 'dvr']]);

    // This one works.
    h.stripeSample.active = 1;
    h.cb.onStripeChange?.(1);
    await vi.advanceTimersByTimeAsync(20_000);
    expect(h.stripeCalls).toHaveLength(2);
    await h.pipeline.stop();
  });

  // Review round 1, item 6: telemetry compares goodput against the client's
  // own live rate (video only), not the relay's ingress.
  it('reports the live rate and goodput on a deep-buffer session once measurable', async () => {
    const h = await startPipeline('deep');
    await vi.advanceTimersByTimeAsync(500);
    expect(h.stats.at(-1)).not.toHaveProperty('dvrLiveRateBps');
    expect(h.stats.at(-1)).not.toHaveProperty('dvrGoodputBps');
    // Eleven 1000-byte frames, 100 ms of media apart: 10 frames' bytes over
    // 1 s of media from the first keyframe to the last.
    for (let id = 0; id <= 10; id++) {
      h.cb.onKeyframe({
        frameId: id,
        timestampUs: BigInt(id) * 100_000n,
        config: null,
        data: new Uint8Array(1000),
        streamBytes: 1020,
      });
      await vi.advanceTimersByTimeAsync(100);
    }
    await vi.advanceTimersByTimeAsync(500);
    const s = h.stats.at(-1)!;
    expect(s.dvrLiveRateBps).toBeCloseTo(80_000, -2);
    expect(s.dvrGoodputBps).toBeGreaterThan(0);
    await h.pipeline.stop();
  });

  // Review round 2, finding 2: the viewer reconciles the transport's actual
  // legs against the desired width whenever they differ — not only after a
  // dial failure — without restarting a transition that is still dialing.
  it('re-requests when the transport ends up at a width the policy did not ask for', async () => {
    setStripeMode('on');
    const h = await startPipeline('deep');
    h.cb.onRelayCapabilities?.({ flags: CAP_STRIPED_DVR, parityLevel: 0 });
    h.cb.onDatagram(encodeDeliveryAck('dvr', 3000));
    await vi.advanceTimersByTimeAsync(500);
    expect(h.stripeCalls).toEqual([[1, 'dvr']]);

    // A stale transition commits two legs.
    Object.assign(h.stripeSample, { active: 2, target: 2 });
    h.cb.onStripeChange?.(2);
    await vi.advanceTimersByTimeAsync(2000);
    expect(h.stripeCalls).toEqual([[1, 'dvr'], [1, 'dvr']]);
    await h.pipeline.stop();
  });

  it('leaves a transition that is still dialing alone, until the dial timeout', async () => {
    setStripeMode('on');
    const h = await startPipeline('deep');
    h.cb.onRelayCapabilities?.({ flags: CAP_STRIPED_DVR, parityLevel: 0 });
    h.cb.onDatagram(encodeDeliveryAck('dvr', 3000));
    await vi.advanceTimersByTimeAsync(500);
    // Working toward the right width, nothing up yet, nothing failed.
    Object.assign(h.stripeSample, { active: 0, target: 1 });
    await vi.advanceTimersByTimeAsync(DVR_STRIPE_DIAL_TIMEOUT_MS - 1000);
    expect(h.stripeCalls).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(1500);
    expect(h.stripeCalls).toEqual([[1, 'dvr'], [1, 'dvr']]);
    await h.pipeline.stop();
  });

  // Review round 3, item 4: dvrIdle tells a broadcaster pause (no video bytes
  // on any connection) from starvation (bytes, no complete frame).
  it('reports dvrIdle: true with no video bytes, false while bytes arrive without a frame', async () => {
    const h = await startPipeline('deep');
    await vi.advanceTimersByTimeAsync(500);
    expect(h.stats.at(-1)!.dvrIdle).toBe(true);
    // One chunk of a two-chunk frame, every 200 ms: bytes, never a frame.
    for (let i = 0; i < 10; i++) {
      h.cb.onDatagram(
        encodeVideoChunk(
          { keyframe: false, frameId: 10 + i, chunkIndex: 0, chunkCount: 2, timestampUs: BigInt(i) * 1000n },
          new Uint8Array(500),
        ),
      );
      await vi.advanceTimersByTimeAsync(200);
    }
    const s = h.stats.at(-1)!;
    expect(s.dvrIdle).toBe(false);
    expect(s.dvrGoodputBps ?? 0).toBe(0);
    await h.pipeline.stop();
  });

  it('leaves dvrIdle off a live-edge session', async () => {
    const h = await startPipeline('live');
    await vi.advanceTimersByTimeAsync(600);
    expect(h.stats.at(-1)!.dvrIdle).toBeUndefined();
    await h.pipeline.stop();
  });
});
