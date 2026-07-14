// R10 P3: TransportWorkerCore marshals ViewerTransport callbacks into posted
// events with transferred buffers, pushes connection stats on an interval,
// and reports connect failures / session closes as typed events. No real
// worker or WebTransport — a fake transport + fake host pin the protocol.

import { afterEach, describe, expect, it, vi } from 'vitest';
import type { KeyframeStreamFrame } from './connection';
import {
  CONN_STATS_INTERVAL_MS,
  TransportWorkerCore,
  keyframeTransferables,
  type TransportWorkerEvent,
} from './transport-worker-core';
import type { ViewerTransport, ViewerTransportCallbacks } from './viewer-transport';

function fakeTransport(connectImpl?: (cb: ViewerTransportCallbacks) => Promise<void>) {
  let callbacks: ViewerTransportCallbacks | null = null;
  const transport: ViewerTransport = {
    kind: 'in-process',
    connect: vi.fn(async (cb: ViewerTransportCallbacks) => {
      callbacks = cb;
      if (connectImpl) await connectImpl(cb);
    }),
    sampleConnectionStats: vi.fn(() => null),
    sampleTimeSync: vi.fn(() => null),
    close: vi.fn(),
  };
  return { transport, cb: () => callbacks! };
}

function makeCore(transport: ViewerTransport) {
  const posted: { event: TransportWorkerEvent; transfer?: Transferable[] }[] = [];
  const core = new TransportWorkerCore(
    { post: (event, transfer) => posted.push({ event, transfer }) },
    () => transport,
  );
  return { core, posted };
}

const flush = () => new Promise((r) => setTimeout(r, 0));

afterEach(() => {
  vi.useRealTimers();
});

describe('TransportWorkerCore', () => {
  it('posts connected after the transport connects', async () => {
    const { transport } = fakeTransport();
    const { core, posted } = makeCore(transport);
    core.connect('https://relay.test/subscribe/AB', {});
    await flush();
    expect(posted.map((p) => p.event.type)).toContain('connected');
  });

  it('posts connect-error when the transport cannot connect', async () => {
    const { transport } = fakeTransport(() => Promise.reject(new Error('no route')));
    const { core, posted } = makeCore(transport);
    core.connect('https://relay.test/subscribe/AB', {});
    await flush();
    expect(posted.map((p) => p.event.type)).not.toContain('connected');
    const err = posted.find((p) => p.event.type === 'connect-error');
    expect(err && err.event.type === 'connect-error' && err.event.message).toBe('no route');
  });

  it('forwards datagrams with their buffer in the transfer list', async () => {
    const { transport, cb } = fakeTransport();
    const { core, posted } = makeCore(transport);
    core.connect('https://relay.test/subscribe/AB', {});
    await flush();

    const data = new Uint8Array([1, 2, 3]);
    cb().onDatagram(data);
    const msg = posted.find((p) => p.event.type === 'datagram');
    expect(msg && msg.event.type === 'datagram' && msg.event.data).toBe(data);
    expect(msg?.transfer).toEqual([data.buffer]);
  });

  it('forwards keyframes with frameId/timestamp/config intact', async () => {
    const { transport, cb } = fakeTransport();
    const { core, posted } = makeCore(transport);
    core.connect('https://relay.test/subscribe/AB', {});
    await flush();

    const kf: KeyframeStreamFrame = {
      frameId: 42,
      timestampUs: 123_456n,
      config: { codec: 'avc1.42E01F', extradata: new Uint8Array([1, 2]) },
      data: new Uint8Array([9, 8, 7]),
    };
    cb().onKeyframe(kf);
    const msg = posted.find((p) => p.event.type === 'keyframe');
    expect(msg).toBeDefined();
    if (msg && msg.event.type === 'keyframe') {
      expect(msg.event.frameId).toBe(42);
      expect(msg.event.timestampUs).toBe(123_456n);
      expect(msg.event.config?.codec).toBe('avc1.42E01F');
      expect(msg.event.data).toBe(kf.data);
    }
  });

  it('forwards the session close with its code and stops the stats ticker', async () => {
    vi.useFakeTimers();
    const { transport, cb } = fakeTransport();
    const { core, posted } = makeCore(transport);
    core.connect('https://relay.test/subscribe/AB', {});
    await vi.advanceTimersByTimeAsync(0);

    cb().onClosed({ closeCode: 4000, reason: 'broadcast ended', message: 'closed: broadcast ended' });
    const msg = posted.find((p) => p.event.type === 'closed');
    expect(msg && msg.event.type === 'closed' && msg.event.closeCode).toBe(4000);

    // No further connStats after the session ended.
    const statsBefore = posted.filter((p) => p.event.type === 'connStats').length;
    await vi.advanceTimersByTimeAsync(CONN_STATS_INTERVAL_MS * 3);
    expect(posted.filter((p) => p.event.type === 'connStats')).toHaveLength(statsBefore);
  });

  it('pushes connection stats on the sampling interval', async () => {
    vi.useFakeTimers();
    const { transport } = fakeTransport();
    const { core, posted } = makeCore(transport);
    core.connect('https://relay.test/subscribe/AB', {});
    await vi.advanceTimersByTimeAsync(0);

    await vi.advanceTimersByTimeAsync(CONN_STATS_INTERVAL_MS * 2);
    const connStats = posted.filter((p) => p.event.type === 'connStats');
    expect(connStats.length).toBeGreaterThanOrEqual(2);
    expect(transport.sampleConnectionStats).toHaveBeenCalled();
    // R5 Q2: the clock-sync sample rides the same push (null from this fake).
    expect(transport.sampleTimeSync).toHaveBeenCalled();
    expect(connStats[0].event.type === 'connStats' && connStats[0].event.timeSync).toBeNull();
  });

  it('close() closes the transport and stops sampling', async () => {
    vi.useFakeTimers();
    const { transport } = fakeTransport();
    const { core, posted } = makeCore(transport);
    core.connect('https://relay.test/subscribe/AB', {});
    await vi.advanceTimersByTimeAsync(0);

    core.close();
    expect(transport.close).toHaveBeenCalledTimes(1);
    const statsBefore = posted.filter((p) => p.event.type === 'connStats').length;
    await vi.advanceTimersByTimeAsync(CONN_STATS_INTERVAL_MS * 3);
    expect(posted.filter((p) => p.event.type === 'connStats')).toHaveLength(statsBefore);
  });
});

describe('keyframeTransferables', () => {
  it('dedupes when payload and extradata share one buffer', () => {
    const backing = new Uint8Array([0, 1, 2, 3, 4, 5, 6, 7]);
    const kf: KeyframeStreamFrame = {
      frameId: 1,
      timestampUs: 0n,
      config: { codec: 'vp8', extradata: backing.subarray(0, 2) },
      data: backing.subarray(2),
    };
    expect(keyframeTransferables(kf)).toEqual([backing.buffer]);
  });

  it('lists both buffers when they differ, one when config is absent', () => {
    const kf: KeyframeStreamFrame = {
      frameId: 1,
      timestampUs: 0n,
      config: { codec: 'vp8', extradata: new Uint8Array([1]) },
      data: new Uint8Array([2]),
    };
    expect(keyframeTransferables(kf)).toHaveLength(2);
    expect(keyframeTransferables({ ...kf, config: null })).toEqual([kf.data.buffer]);
  });
});
