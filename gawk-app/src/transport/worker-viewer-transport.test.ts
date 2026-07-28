// R10 P3: WorkerViewerTransport proxies a ViewerTransport over a (fake)
// transport worker: connect resolves/rejects on the worker's events, data
// events dispatch to the pipeline callbacks, close() asks for a graceful
// close then reaps the worker. The final describe wires the proxy to a real
// TransportWorkerCore through an in-process message channel, pinning the two
// sides of the protocol against each other.

import { afterEach, describe, expect, it, vi } from 'vitest';
import type { ConnectOptions } from './connection';
import {
  TransportWorkerCore,
  type TransportWorkerCommand,
  type TransportWorkerEvent,
} from './transport-worker-core';
import type { ViewerTransport, ViewerTransportCallbacks } from './viewer-transport';
import {
  CLOSE_REAP_DELAY_MS,
  WorkerViewerTransport,
  type TransportWorkerLike,
} from './worker-viewer-transport';

class FakeWorker implements TransportWorkerLike {
  onmessage: ((e: MessageEvent) => void) | null = null;
  onerror: ((e: ErrorEvent) => void) | null = null;
  sent: TransportWorkerCommand[] = [];
  terminate = vi.fn();

  postMessage(message: TransportWorkerCommand): void {
    this.sent.push(message);
  }

  emit(event: TransportWorkerEvent): void {
    this.onmessage?.({ data: event } as MessageEvent);
  }
}

function makeCallbacks() {
  return {
    onDatagram: vi.fn(),
    onKeyframe: vi.fn(),
    onClosed: vi.fn(),
  } satisfies ViewerTransportCallbacks;
}

afterEach(() => {
  vi.useRealTimers();
});

describe('WorkerViewerTransport', () => {
  it('posts the connect command and resolves on connected', async () => {
    const worker = new FakeWorker();
    const transport = new WorkerViewerTransport(() => worker, 'https://r/subscribe/AB', {
      certHashHex: 'aa',
    });
    const connected = transport.connect(makeCallbacks());
    expect(worker.sent).toEqual([
      { type: 'connect', url: 'https://r/subscribe/AB', connectOpts: { certHashHex: 'aa' } },
    ]);
    worker.emit({ type: 'connected' });
    await expect(connected).resolves.toBeUndefined();
  });

  it('rejects on connect-error and reaps the worker', async () => {
    const worker = new FakeWorker();
    const transport = new WorkerViewerTransport(() => worker, 'https://r/subscribe/AB', {});
    const connected = transport.connect(makeCallbacks());
    worker.emit({ type: 'connect-error', message: 'no route' });
    await expect(connected).rejects.toThrow('no route');
    expect(worker.terminate).toHaveBeenCalledTimes(1);
  });

  it('rejects with the close code when the session dies before connected relays', async () => {
    const worker = new FakeWorker();
    const transport = new WorkerViewerTransport(() => worker, 'https://r/subscribe/AB', {});
    const connected = transport.connect(makeCallbacks());
    worker.emit({ type: 'closed', closeCode: 4000, message: 'broadcast ended' });
    await expect(connected).rejects.toMatchObject({ closeCode: 4000 });
  });

  it('dispatches datagrams, keyframes and the close to the callbacks', async () => {
    const worker = new FakeWorker();
    const cbs = makeCallbacks();
    const transport = new WorkerViewerTransport(() => worker, 'https://r/subscribe/AB', {});
    const connected = transport.connect(cbs);
    worker.emit({ type: 'connected' });
    await connected;

    const dgram = new Uint8Array([1, 2, 3]);
    worker.emit({ type: 'datagram', data: dgram });
    expect(cbs.onDatagram).toHaveBeenCalledWith(dgram);

    worker.emit({
      type: 'keyframe',
      frameId: 7,
      timestampUs: 99n,
      config: { codec: 'vp8', extradata: new Uint8Array(0) },
      data: new Uint8Array([4]),
      streamBytes: 25,
    });
    expect(cbs.onKeyframe).toHaveBeenCalledWith(
      expect.objectContaining({ frameId: 7, timestampUs: 99n, streamBytes: 25 }),
    );

    worker.emit({ type: 'closed', closeCode: 4000, reason: 'ended', message: 'closed: ended' });
    expect(cbs.onClosed).toHaveBeenCalledWith({
      closeCode: 4000,
      reason: 'ended',
      message: 'closed: ended',
    });
    expect(worker.terminate).toHaveBeenCalledTimes(1);
  });

  it('caches pushed connection stats for synchronous sampling', async () => {
    const worker = new FakeWorker();
    const transport = new WorkerViewerTransport(() => worker, 'https://r/subscribe/AB', {});
    const connected = transport.connect(makeCallbacks());
    worker.emit({ type: 'connected' });
    await connected;

    expect(transport.sampleConnectionStats()).toBeNull();
    expect(transport.sampleTimeSync()).toBeNull();
    expect(transport.sampleCarrierStats()).toBeNull();
    expect(transport.sampleDatagramBuffer()).toBeNull();
    const stats = { rttMs: 12 } as never;
    // timeOriginMs is the transport worker's own clock anchor — it must cross
    // untouched so the pipeline can rebase onto the sample's clock domain.
    const timeSync = { offsetUs: 5_000n, rttMs: 3, timeOriginMs: 1_234.5 };
    const carrier = { streamsOpened: 2, recordsReceived: 40, streamsAborted: 0, malformed: 0 };
    // R29 finding 2: the buffer is set in whichever realm owns the
    // WebTransport — here the transport worker — so its verdict has to travel
    // back out the same way the carrier tallies do, or the gate on the main
    // thread can only ever report "unknown" on the path that actually matters.
    const datagramBuffer = {
      property: 'incomingHighWaterMark' as const,
      requested: 256,
      defaultDepth: 1,
      effective: 256,
      applied: true,
      governsDrops: false,
    };
    worker.emit({ type: 'connStats', stats, timeSync, carrier, datagramBuffer });
    expect(transport.sampleConnectionStats()).toBe(stats);
    // R5 Q2: the clock-sync sample rides the same push (bigint survives the
    // structured-clone boundary in the real pair).
    expect(transport.sampleTimeSync()).toBe(timeSync);
    // R19: the carrier tallies ride the same push.
    expect(transport.sampleCarrierStats()).toBe(carrier);
    expect(transport.sampleDatagramBuffer()).toBe(datagramBuffer);
  });

  it('close() requests a graceful close, suppresses further events, then reaps', async () => {
    vi.useFakeTimers();
    const worker = new FakeWorker();
    const cbs = makeCallbacks();
    const transport = new WorkerViewerTransport(() => worker, 'https://r/subscribe/AB', {});
    const connected = transport.connect(cbs);
    worker.emit({ type: 'connected' });
    await connected;

    transport.close();
    expect(worker.sent.at(-1)).toEqual({ type: 'close' });
    expect(worker.terminate).not.toHaveBeenCalled(); // graceful window first

    // Events after a local close never reach the pipeline.
    worker.emit({ type: 'datagram', data: new Uint8Array([1]) });
    worker.emit({ type: 'closed', message: 'gone' });
    expect(cbs.onDatagram).not.toHaveBeenCalled();
    expect(cbs.onClosed).not.toHaveBeenCalled();

    await vi.advanceTimersByTimeAsync(CLOSE_REAP_DELAY_MS);
    expect(worker.terminate).toHaveBeenCalledTimes(1);
  });

  it('a worker-level error before connect rejects; after connect it reports a drop', async () => {
    const worker = new FakeWorker();
    const transport = new WorkerViewerTransport(() => worker, 'https://r/subscribe/AB', {});
    const connected = transport.connect(makeCallbacks());
    worker.onerror?.(new Event('error') as ErrorEvent);
    await expect(connected).rejects.toThrow('transport worker failed');

    const worker2 = new FakeWorker();
    const cbs = makeCallbacks();
    const transport2 = new WorkerViewerTransport(() => worker2, 'https://r/subscribe/AB', {});
    const connected2 = transport2.connect(cbs);
    worker2.emit({ type: 'connected' });
    await connected2;
    worker2.onerror?.(new Event('error') as ErrorEvent);
    expect(cbs.onClosed).toHaveBeenCalledWith({ message: 'transport worker failed' });
  });
});

// Proxy ↔ core, joined by an in-process channel: what the real worker pair
// does minus structured clone. Pins that the two sides speak the same
// protocol without either being mocked.
describe('WorkerViewerTransport + TransportWorkerCore end-to-end', () => {
  function pair(inner: ViewerTransport, url: string, opts: ConnectOptions) {
    const workerLike: TransportWorkerLike = {
      onmessage: null,
      onerror: null,
      terminate: vi.fn(),
      postMessage: (cmd: TransportWorkerCommand) => {
        if (cmd.type === 'connect') core.connect(cmd.url, cmd.connectOpts);
        else core.close();
      },
    };
    const core = new TransportWorkerCore(
      { post: (ev) => workerLike.onmessage?.({ data: ev } as MessageEvent) },
      () => inner,
    );
    return new WorkerViewerTransport(() => workerLike, url, opts);
  }

  it('relays connect, keyframes and the session close across the channel', async () => {
    let innerCbs: ViewerTransportCallbacks | null = null;
    const inner: ViewerTransport = {
      kind: 'in-process',
      connect: async (cb) => {
        innerCbs = cb;
      },
      sampleConnectionStats: () => null,
      sampleTimeSync: () => null,
      close: vi.fn(),
    };
    const proxy = pair(inner, 'https://r/subscribe/AB', {});
    const cbs = makeCallbacks();
    await proxy.connect(cbs);

    innerCbs!.onKeyframe({
      frameId: 3,
      timestampUs: 1000n,
      config: { codec: 'vp8', extradata: new Uint8Array(0) },
      data: new Uint8Array([1, 2]),
      streamBytes: 26,
    });
    expect(cbs.onKeyframe).toHaveBeenCalledWith(
      expect.objectContaining({ frameId: 3, timestampUs: 1000n, streamBytes: 26 }),
    );

    innerCbs!.onClosed({ closeCode: 4000, message: 'broadcast ended' });
    expect(cbs.onClosed).toHaveBeenCalledWith(
      expect.objectContaining({ closeCode: 4000, message: 'broadcast ended' }),
    );
  });
});
