// R10 P3: the viewer-worker side of the transport split — a ViewerTransport
// that proxies to a dedicated transport worker (transport.worker.ts) over
// postMessage. One worker per pipeline attempt: connect() spawns it, close()
// reaps it, so a reconnect gets a fresh session and nothing outlives the
// pipeline that owns it.

import type { CarrierCounters, ConnectOptions } from './connection';
import type { DatagramBufferStats } from './datagram-buffer';
import type { TransportConnectionStats } from './net-stats';
import type { TimeSyncStats } from './time-sync';
import type { TransportWorkerCommand, TransportWorkerEvent } from './transport-worker-core';
import type {
  StripeTransportStats,
  ViewerTransport,
  ViewerTransportCallbacks,
  ViewerTransportKind,
} from './viewer-transport';

// The subset of Worker the proxy drives; injectable for tests.
export interface TransportWorkerLike {
  postMessage(message: TransportWorkerCommand, transfer?: Transferable[]): void;
  terminate(): void;
  onmessage: ((e: MessageEvent) => void) | null;
  onerror: ((e: ErrorEvent) => void) | null;
}

// How long a closing worker gets to flush the graceful session close before
// it is reaped. The shell also self-close()s after handling 'close'.
export const CLOSE_REAP_DELAY_MS = 250;

export class WorkerViewerTransport implements ViewerTransport {
  readonly kind: ViewerTransportKind = 'worker';
  private createWorker: () => TransportWorkerLike;
  private url: string;
  private opts: ConnectOptions;
  private worker: TransportWorkerLike | null = null;
  private latestStats: TransportConnectionStats | null = null;
  private latestTimeSync: TimeSyncStats | null = null;
  private latestCarrier: CarrierCounters | null = null;
  private latestDatagramBuffer: DatagramBufferStats | null = null;
  private latestStripe: StripeTransportStats | null = null;
  private closing = false;

  constructor(createWorker: () => TransportWorkerLike, url: string, opts: ConnectOptions) {
    this.createWorker = createWorker;
    this.url = url;
    this.opts = opts;
  }

  connect(cb: ViewerTransportCallbacks): Promise<void> {
    const worker = this.createWorker();
    this.worker = worker;
    return new Promise<void>((resolve, reject) => {
      let settled = false;
      worker.onmessage = (e: MessageEvent) => {
        const ev = e.data as TransportWorkerEvent;
        switch (ev.type) {
          case 'connected':
            settled = true;
            resolve();
            break;
          case 'connect-error': {
            settled = true;
            this.reap();
            reject(new Error(ev.message));
            break;
          }
          case 'datagram':
            if (!this.closing) cb.onDatagram(ev.data);
            break;
          case 'keyframe':
            if (!this.closing) {
              cb.onKeyframe({
                frameId: ev.frameId,
                timestampUs: ev.timestampUs,
                config: ev.config,
                data: ev.data,
                streamBytes: ev.streamBytes,
              });
            }
            break;
          case 'closed': {
            const wasSettled = settled;
            settled = true;
            this.reap();
            if (!wasSettled) {
              // Connected-then-dropped before 'connected' was relayed: surface
              // it as a connect failure, keeping the close code's semantics.
              const err = new Error(ev.message) as Error & { closeCode?: number };
              if (ev.closeCode !== undefined) err.closeCode = ev.closeCode;
              reject(err);
            } else if (!this.closing) {
              cb.onClosed({ closeCode: ev.closeCode, reason: ev.reason, message: ev.message });
            }
            break;
          }
          case 'telemetryHello':
            if (!this.closing) cb.onTelemetryHello?.(ev.hello);
            break;
          case 'relayCapabilities':
            if (!this.closing) cb.onRelayCapabilities?.(ev.caps);
            break;
          case 'stripeChange':
            if (!this.closing) cb.onStripeChange?.(ev.active);
            break;
          case 'connStats':
            this.latestStats = ev.stats;
            this.latestTimeSync = ev.timeSync;
            this.latestCarrier = ev.carrier;
            this.latestDatagramBuffer = ev.datagramBuffer;
            this.latestStripe = ev.stripe ?? null;
            break;
        }
      };
      // Worker-level failure (script load error, crash): same shape as a
      // session drop — reconnectable, no close code.
      worker.onerror = () => {
        const err = new Error('transport worker failed');
        const wasSettled = settled;
        settled = true;
        this.reap();
        if (!wasSettled) reject(err);
        else if (!this.closing) cb.onClosed({ message: err.message });
      };
      worker.postMessage({ type: 'connect', url: this.url, connectOpts: this.opts });
    });
  }

  sampleConnectionStats(): TransportConnectionStats | null {
    return this.latestStats;
  }

  sampleTimeSync(): TimeSyncStats | null {
    return this.latestTimeSync;
  }

  sampleCarrierStats(): CarrierCounters | null {
    return this.latestCarrier;
  }

  sampleDatagramBuffer(): DatagramBufferStats | null {
    return this.latestDatagramBuffer;
  }

  setStripe(n: number): void {
    if (!this.closing) this.worker?.postMessage({ type: 'stripe', n });
  }

  sampleStripe(): StripeTransportStats | null {
    return this.latestStripe;
  }

  close(): void {
    this.closing = true;
    const worker = this.worker;
    this.worker = null;
    if (!worker) return;
    // Ask for a graceful session close, then reap; the shell self-close()s
    // too, so the timer is only the backstop for a wedged worker.
    worker.postMessage({ type: 'close' });
    setTimeout(() => worker.terminate(), CLOSE_REAP_DELAY_MS);
  }

  // Terminal event or failure: the worker has nothing more to say.
  private reap(): void {
    const worker = this.worker;
    this.worker = null;
    worker?.terminate();
  }
}
