// The viewer's transport seam (R10 P3, docs/14). ViewerPipeline consumes this
// interface instead of touching WebTransport directly, so the connection +
// read loops can run either in-process (LocalViewerTransport, below — the
// main-thread fallback and the no-nested-worker fallback) or in a dedicated
// transport worker (WorkerViewerTransport), where no decode/render pressure
// can ever starve the browser's small incoming-datagram queue.

import { log } from '../lib/logger';
import {
  connectWebTransport,
  newCarrierCounters,
  readDatagrams,
  readServerStreams,
  type CarrierCounters,
  type ConnectOptions,
  type KeyframeStreamFrame,
} from './connection';
import { applyIncomingDatagramBuffer, type DatagramBufferStats } from './datagram-buffer';
import { ConnectionStatsSampler, type TransportConnectionStats } from './net-stats';
import type { TelemetryHelloMessage } from './wire';
import { TimeSyncClient, type TimeSyncStats } from './time-sync';

// The one authoritative "session is over" signal (see CODE-REVIEW: one event,
// one signal). closeCode carries the semantics when the server closed cleanly
// (4000 = broadcast ended); an abrupt drop has message only.
export interface TransportClosedInfo {
  closeCode?: number;
  reason?: string;
  message: string;
}

export interface ViewerTransportCallbacks {
  onDatagram: (dgram: Uint8Array) => void;
  onKeyframe: (kf: KeyframeStreamFrame) => void;
  // R28 (docs/33 D2): this session's telemetry identity. The transport is
  // where it arrives (it rides a server uni stream), but nothing here acts on
  // it — it is forwarded to the pipeline and out to the main thread, which is
  // the only place collection happens (D13).
  onTelemetryHello?: (hello: TelemetryHelloMessage) => void;
  // Fires at most once, and never after close() was called locally.
  onClosed: (info: TransportClosedInfo) => void;
}

// Where the connection + read loops actually run — surfaced in
// ViewerStats.transport so "did the split actually engage?" is answerable
// from the overlay / Copy diagnostics.
export type ViewerTransportKind = 'in-process' | 'worker';

export interface ViewerTransport {
  readonly kind: ViewerTransportKind;
  // Resolves once the session is up and the read loops are running; rejects
  // on a never-connected failure (ViewerSession treats that as fatal).
  connect(cb: ViewerTransportCallbacks): Promise<void>;
  // Latest connection-health sample (null where getStats() is unsupported).
  // Calling it also schedules a refresh where the impl samples on demand.
  sampleConnectionStats(): TransportConnectionStats | null;
  // Latest relay clock-sync sample (R5 Q2): local→relay clock offset + a
  // self-owned RTT. Null until the first ping/pong completes (or where the
  // session can't send datagrams). Lives in the transport because it owns the
  // reply timing — on the worker path a postMessage hop would add jitter.
  sampleTimeSync(): TimeSyncStats | null;
  // R19: the reliable-carrier tallies (docs/24 Decision 10) — how the mode
  // row tells `reliable` from `requested but datagrams served`. Optional so
  // test fakes without a carrier path keep compiling; null before connect.
  sampleCarrierStats?(): CarrierCounters | null;
  // R29 finding 2 (docs/34): what this session's incoming datagram queue was
  // raised to, and whether the browser honoured it. Lives on the transport
  // because only the realm holding the WebTransport can set or read the
  // attribute — on the worker path the main thread has no handle on it at all.
  // Null before connect, and on transports that never touch a real session.
  sampleDatagramBuffer?(): DatagramBufferStats | null;
  close(): void;
}

export type ViewerTransportFactory = (url: string, opts: ConnectOptions) => ViewerTransport;

// In-process transport: exactly the connection handling ViewerPipeline had
// before the seam (extracted verbatim, including the close-code race below).
export class LocalViewerTransport implements ViewerTransport {
  readonly kind: ViewerTransportKind = 'in-process';
  private url: string;
  private opts: ConnectOptions;
  private wt: WebTransport | null = null;
  private sampler: ConnectionStatsSampler | null = null;
  private timeSync: TimeSyncClient | null = null;
  private timeSyncWriter: WritableStreamDefaultWriter<BufferSource> | null = null;
  private carrier = newCarrierCounters();
  private datagramBuffer: DatagramBufferStats | null = null;
  private abort = new AbortController();
  private closing = false; // close() called — suppress onClosed
  private closedReported = false;

  constructor(url: string, opts: ConnectOptions) {
    this.url = url;
    this.opts = opts;
  }

  async connect(cb: ViewerTransportCallbacks): Promise<void> {
    const wt = await connectWebTransport(this.url, this.opts);
    this.wt = wt;
    this.sampler = new ConnectionStatsSampler(wt);

    // R29 finding 2 (docs/34): raise the browser's incoming datagram queue
    // before a single read happens, because it is the queue — not the reader —
    // that decides whether a frame's burst survives. This is deliberately here
    // rather than in the shared connectWebTransport: a broadcaster's incoming
    // queue carries only control traffic, and this class is the one object
    // that exists in every viewer placement (main thread, viewer worker, and
    // the nested transport worker), so setting it here reaches all three with
    // no message plumbing.
    this.datagramBuffer = applyIncomingDatagramBuffer(
      (wt as { datagrams?: unknown }).datagrams,
    );

    // Relay clock sync (R5 Q2): ping over this session's datagrams; replies
    // are intercepted below, before the video path ever sees them. Feature-
    // detected so test fakes / odd environments without a writable datagram
    // stream simply report null.
    const datagrams = (wt as { datagrams?: { writable?: WritableStream<BufferSource> } }).datagrams;
    if (datagrams?.writable) {
      const writer = datagrams.writable.getWriter();
      this.timeSyncWriter = writer;
      // A ping that never leaves is why `timeSyncRttMs` reads null forever —
      // which is how both 2026-07-22 Safari captures looked, with no clue as
      // to the cause because this rejection used to be swallowed outright
      // (BUGS.md). Still non-fatal (a failed ping must never take the
      // pipeline down), but no longer silent; logged once so a broken leg
      // doesn't spam a 0.5 Hz warning for the life of the session.
      let pingSendLogged = false;
      this.timeSync = new TimeSyncClient(
        (d) =>
          void writer.write(d).catch((e) => {
            if (pingSendLogged) return;
            pingSendLogged = true;
            log.warn('TimeSync ping could not be sent; clock sync and RTT stay unavailable:', e);
          }),
      );
      this.timeSync.start();
    }

    void wt.closed
      .then((closeInfo) => {
        const info = closeInfo as { closeCode?: number; reason?: string } | undefined;
        this.reportClosed(cb, info?.closeCode, info?.reason);
      })
      .catch((err) => {
        this.reportClosed(cb, err?.closeCode, err?.reason || err?.message);
      });

    // Read loops run for the life of the session. Deltas arrive as datagrams;
    // keyframes arrive as reliable unidirectional streams (R8). On a joining
    // viewer the relay primes us with the last keyframe over a stream, so the
    // first picture typically appears without waiting for the next keyframe.
    void readDatagrams(
      wt,
      (dgram) => {
        if (this.timeSync?.handleDatagram(dgram)) return; // consumed (R5 Q2)
        cb.onDatagram(dgram);
      },
      this.abort.signal,
    )
      .then(() => this.handleReadLoopEnd(cb, wt, null))
      .catch((e) => this.handleReadLoopEnd(cb, wt, e instanceof Error ? e : new Error(String(e))));

    // Server streams (keyframes + R19 carriers): failures here are not fatal
    // to the session (the next keyframe recovers, and a real drop surfaces
    // via the datagram loop / wt.closed), so they are logged, not propagated.
    // Carrier records are verbatim datagrams — they feed the same handler,
    // and the pipeline never learns which transport delivered the bytes.
    void readServerStreams(
      wt,
      {
        onKeyframe: cb.onKeyframe,
        onCarrierRecord: (record) => cb.onDatagram(record),
        onTelemetryHello: (hello) => cb.onTelemetryHello?.(hello),
      },
      this.carrier,
      this.abort.signal,
    ).catch((e) => {
      if (!this.closing) log.warn('Server stream loop ended:', e);
    });
  }

  // On a server close, the datagram read loop and wt.closed settle in
  // unspecified, browser-dependent order — and only wt.closed carries the
  // close code (4000 = broadcast ended, the one signal that must stop
  // reconnecting). Give wt.closed a short window to settle before treating
  // the read-loop end as an anonymous drop.
  private async handleReadLoopEnd(
    cb: ViewerTransportCallbacks,
    wt: WebTransport,
    err: Error | null,
  ): Promise<void> {
    if (this.closing || this.closedReported) return;
    const closeInfo = await Promise.race([
      wt.closed.then(
        (info) => info ?? {},
        (e) => e ?? {},
      ),
      new Promise<null>((r) => setTimeout(() => r(null), 100)),
    ]);
    if (this.closing || this.closedReported) return; // the wt.closed handler acted first
    if (closeInfo !== null) {
      const info = closeInfo as { closeCode?: number; reason?: string; message?: string };
      this.reportClosed(cb, info.closeCode, info.reason || info.message);
      return;
    }
    this.reportDropped(cb, err ?? new Error('WebTransport session closed by server'));
  }

  private reportClosed(cb: ViewerTransportCallbacks, closeCode?: number, reason?: string): void {
    if (this.closing || this.closedReported) return;
    this.closedReported = true;
    const message = reason
      ? `WebTransport session closed: ${reason}`
      : 'WebTransport session closed by server';
    cb.onClosed({ closeCode, reason, message });
  }

  // An abrupt drop (read loop died, no close frame): message only.
  private reportDropped(cb: ViewerTransportCallbacks, err: Error): void {
    if (this.closing || this.closedReported) return;
    this.closedReported = true;
    cb.onClosed({ message: err.message });
  }

  sampleConnectionStats(): TransportConnectionStats | null {
    this.sampler?.tick();
    return this.sampler?.latest() ?? null;
  }

  sampleTimeSync(): TimeSyncStats | null {
    return this.timeSync?.sample() ?? null;
  }

  sampleCarrierStats(): CarrierCounters | null {
    return { ...this.carrier };
  }

  sampleDatagramBuffer(): DatagramBufferStats | null {
    return this.datagramBuffer;
  }

  close(): void {
    this.closing = true;
    this.timeSync?.stop();
    this.timeSync = null;
    try {
      this.timeSyncWriter?.releaseLock();
    } catch {
      // a pending write may hold the lock — the session close ends it anyway
    }
    this.timeSyncWriter = null;
    this.abort.abort();
    try {
      this.wt?.close();
    } catch {
      // already closed by the server — fine
    }
    this.wt = null;
  }
}
