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
import { encodeDeliveryAck, type DeliveryServedMode } from './wire';
import type { ViewerDeliveryMode } from './resilient';

interface Harness {
  pipeline: ViewerPipeline;
  stripeCalls: [number, StripeLayout | undefined][];
  cb: ViewerTransportCallbacks;
  stats: ViewerStats[];
}

async function startPipeline(delivery: ViewerDeliveryMode): Promise<Harness> {
  const stripeCalls: Harness['stripeCalls'] = [];
  const stats: ViewerStats[] = [];
  let cb: ViewerTransportCallbacks | null = null;
  const transport: ViewerTransport = {
    kind: 'in-process',
    connect: async (c) => {
      cb = c;
    },
    sampleConnectionStats: () => null,
    sampleTimeSync: () => null,
    setStripe: (n, layout) => {
      stripeCalls.push([n, layout]);
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
  return { pipeline, stripeCalls, cb: cb!, stats };
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
});
