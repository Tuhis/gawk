// R10 P3: host-agnostic core of the dedicated transport worker.
// `transport.worker.ts` is a thin `onmessage` shell around this; the core owns
// one WebTransport session (via LocalViewerTransport — the same connection
// code the in-process path runs) and marshals its callbacks into postMessage
// events with transferred buffers, so the decode/render worker receives
// datagrams and keyframes without the transport thread ever doing decode or
// render work. DOM-free and unit-testable with a fake host + fake transport.

import type { CarrierCounters, ConnectOptions, KeyframeStreamFrame } from './connection';
import type { DatagramBufferStats } from './datagram-buffer';
import type { TransportConnectionStats } from './net-stats';
import type { RelayCapabilities } from './parity';
import type { TimeSyncStats } from './time-sync';
import type { DecoderConfigMessage, TelemetryHelloMessage } from './wire';
import {
  LocalViewerTransport,
  type StripeTransportStats,
  type TransportClosedInfo,
  type ViewerTransportFactory,
} from './viewer-transport';

// Decode worker → transport worker.
export type TransportWorkerCommand =
  | { type: 'connect'; url: string; connectOpts: ConnectOptions }
  // R30 (docs/35 §5.6): stripe target. The transport worker owns the legs —
  // they must live beside the primary's session so their datagrams ride the
  // same posting path with no extra hop.
  | { type: 'stripe'; n: number }
  | { type: 'close' };

// Transport worker → decode worker. datagram/keyframe buffers are transferred
// (zero-copy), never cloned.
export type TransportWorkerEvent =
  | { type: 'connected' }
  | { type: 'connect-error'; message: string }
  | { type: 'datagram'; data: Uint8Array }
  | {
      type: 'keyframe';
      frameId: number;
      timestampUs: bigint;
      config: DecoderConfigMessage | null;
      data: Uint8Array;
      streamBytes: number;
    }
  | { type: 'closed'; closeCode?: number; reason?: string; message: string }
  // R28 (docs/33 D2): the session's telemetry identity, forwarded once. It
  // gets its own message rather than riding the stats push because it is a
  // bearer credential — keeping it out of ViewerStats is what keeps it out of
  // the Copy-diagnostics blob a user pastes into a chat.
  | { type: 'telemetryHello'; hello: TelemetryHelloMessage }
  | { type: 'telemetryEndpoint'; url: string }
  // R29/R30: the relay's capabilities — the stripe controller's gate.
  | { type: 'relayCapabilities'; caps: RelayCapabilities }
  // R30: the stripe width actually engaged (0 = unstriped).
  | { type: 'stripeChange'; active: number }
  // Pushed at the stats cadence: connection health + the relay clock-sync
  // sample (R5 Q2 — measured in this worker, where the reply timing is jitter-
  // free; bigint crosses postMessage via structured clone) + the R19 carrier
  // tallies.
  | {
      type: 'connStats';
      stats: TransportConnectionStats | null;
      timeSync: TimeSyncStats | null;
      carrier: CarrierCounters | null;
      // R29 finding 2 (docs/34): the receive-buffer verdict. It can only be
      // read where the WebTransport lives, which on this path is here — so
      // without this field the main thread's gate could never report the
      // placement the loss was actually measured on.
      datagramBuffer: DatagramBufferStats | null;
      // R30: the stripe tallies (docs/35 §7); null on pre-R30 cores.
      stripe?: StripeTransportStats | null;
    };

export interface TransportWorkerHost {
  post(event: TransportWorkerEvent, transfer?: Transferable[]): void;
}

// The proxy can't pull samples across the boundary, so the core pushes them
// at the stats cadence (matches the pipeline's 500 ms publishStats tick).
export const CONN_STATS_INTERVAL_MS = 500;

export class TransportWorkerCore {
  private host: TransportWorkerHost;
  private createTransport: ViewerTransportFactory;
  private transport: ReturnType<ViewerTransportFactory> | null = null;
  private statsTimer: number | null = null;

  constructor(
    host: TransportWorkerHost,
    createTransport: ViewerTransportFactory = (url, opts) => new LocalViewerTransport(url, opts),
  ) {
    this.host = host;
    this.createTransport = createTransport;
  }

  connect(url: string, connectOpts: ConnectOptions): void {
    const transport = this.createTransport(url, connectOpts);
    this.transport = transport;
    transport
      .connect({
        onDatagram: (data) => this.host.post({ type: 'datagram', data }, [data.buffer]),
        onKeyframe: (kf) =>
          this.host.post(
            {
              type: 'keyframe',
              frameId: kf.frameId,
              timestampUs: kf.timestampUs,
              config: kf.config,
              data: kf.data,
              streamBytes: kf.streamBytes,
            },
            keyframeTransferables(kf),
          ),
        onTelemetryHello: (hello) => this.host.post({ type: 'telemetryHello', hello }),
        onTelemetryEndpoint: (url) => this.host.post({ type: 'telemetryEndpoint', url }),
        onRelayCapabilities: (caps) => this.host.post({ type: 'relayCapabilities', caps }),
        onStripeChange: (active) => this.host.post({ type: 'stripeChange', active }),
        onClosed: (info: TransportClosedInfo) => {
          this.stopStats();
          this.host.post({ type: 'closed', ...info });
        },
      })
      .then(() => {
        this.host.post({ type: 'connected' });
        this.statsTimer = setInterval(() => {
          this.host.post({
            type: 'connStats',
            stats: transport.sampleConnectionStats(),
            timeSync: transport.sampleTimeSync(),
            carrier: transport.sampleCarrierStats?.() ?? null,
            datagramBuffer: transport.sampleDatagramBuffer?.() ?? null,
            stripe: transport.sampleStripe?.() ?? null,
          });
        }, CONN_STATS_INTERVAL_MS) as unknown as number;
      })
      .catch((e) => {
        this.host.post({
          type: 'connect-error',
          message: e instanceof Error ? e.message : String(e),
        });
      });
  }

  setStripe(n: number): void {
    this.transport?.setStripe?.(n);
  }

  close(): void {
    this.stopStats();
    this.transport?.close();
    this.transport = null;
  }

  private stopStats(): void {
    if (this.statsTimer !== null) {
      clearInterval(this.statsTimer);
      this.statsTimer = null;
    }
  }
}

// The keyframe payload and its embedded config extradata are usually views of
// one underlying buffer (readOneKeyframe slices a single read) — dedupe so the
// transfer list never names a buffer twice.
export function keyframeTransferables(kf: KeyframeStreamFrame): Transferable[] {
  const buffers = new Set<ArrayBuffer>([kf.data.buffer as ArrayBuffer]);
  if (kf.config) buffers.add(kf.config.extradata.buffer as ArrayBuffer);
  return [...buffers];
}
