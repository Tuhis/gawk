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
import { MAX_STRIPE_LEGS, encodeStripeState, type TelemetryHelloMessage } from './wire';
import type { RelayCapabilities } from './parity';
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
  // R29/R30: the relay's capabilities (docs/35 §5.3). The stripe controller
  // gates every engagement on CAP_STRIPED_DELIVERY — a relay that never
  // advertises is never dialed for legs, which is the whole skew story.
  onRelayCapabilities?: (caps: RelayCapabilities) => void;
  // R30: the stripe width actually engaged (0 = unstriped). Fires on engage,
  // grow and fallback — the requested-vs-active distinction's active half.
  onStripeChange?: (active: number) => void;
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
  // R30 (docs/35 §5.6): ask for a stripe of n legs (0 disengages). The
  // transport owns the whole transition — dial-before-suppress, the 0x10
  // level protocol, leg-death fallback — so the caller only ever states a
  // target. Optional so pre-R30 fakes keep compiling.
  setStripe?(n: number): void;
  // R30: the live stripe tallies for stats (null before connect).
  sampleStripe?(): StripeTransportStats | null;
  close(): void;
}

// R30 stripe state as the overlay/controller sees it (docs/35 §7).
export interface StripeTransportStats {
  // Legs currently carrying deltas (0 = unstriped).
  active: number;
  // The last setStripe target — active < target means a transition is in
  // flight or failed (caps pressure, dial errors).
  target: number;
  legDials: number;
  legDialFailures: number;
  legDeaths: number;
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
  private cb: ViewerTransportCallbacks | null = null;

  // R30 stripe state (docs/35 §5.6). legs is the CURRENT set; a transition
  // dials a whole fresh set before touching it (make-before-break), so at
  // every instant either the primary or a complete leg set covers the frame.
  private legs: StripeLegSession[] = [];
  private stripeTarget = 0;
  private stripeActive = 0;
  private stripeGeneration = 0; // bumps per transition; stale dials discard
  private stripeRefresh: ReturnType<typeof setInterval> | null = null;
  private stripeStats: StripeTransportStats = {
    active: 0,
    target: 0,
    legDials: 0,
    legDialFailures: 0,
    legDeaths: 0,
  };

  constructor(url: string, opts: ConnectOptions) {
    this.url = url;
    this.opts = opts;
  }

  async connect(cb: ViewerTransportCallbacks): Promise<void> {
    const wt = await connectWebTransport(this.url, this.opts);
    this.wt = wt;
    this.cb = cb;
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
        onRelayCapabilities: (caps) => cb.onRelayCapabilities?.(caps),
      },
      this.carrier,
      this.abort.signal,
    ).catch((e) => {
      if (!this.closing) log.warn('Server stream loop ended:', e);
    });
  }

  // --- R30 striping (docs/35 §5.6) -----------------------------------------

  setStripe(n: number): void {
    const target = Math.max(0, Math.min(MAX_STRIPE_LEGS, Math.floor(n)));
    this.stripeTarget = target;
    this.stripeStats.target = target;
    if (this.closing || target === this.stripeActive) return;
    if (target === 0) {
      this.disengageStripe();
      return;
    }
    void this.transitionStripe(target);
  }

  sampleStripe(): StripeTransportStats | null {
    return { ...this.stripeStats, active: this.stripeActive, target: this.stripeTarget };
  }

  // Make-before-break: dial the WHOLE fresh set, and only once every leg is
  // up switch over (first engage additionally arms the primary suppression).
  // Any dial failure abandons the transition and keeps the current state —
  // capacity pressure degrades striping before it degrades the session
  // (docs/35 §5.8). Duplicates during the overlap are the reassembler's to
  // drop; holes are structurally impossible because the old cover (primary
  // or old leg set) stays live until the new one is complete.
  private async transitionStripe(target: number): Promise<void> {
    const generation = ++this.stripeGeneration;
    const fresh: StripeLegSession[] = [];
    try {
      const dials: Promise<StripeLegSession>[] = [];
      for (let j = 0; j < target; j++) {
        this.stripeStats.legDials++;
        dials.push(this.dialLeg(j, target));
      }
      fresh.push(...(await Promise.all(dials)));
    } catch (e) {
      this.stripeStats.legDialFailures++;
      for (const leg of fresh) leg.close();
      if (!this.closing) log.warn(`stripe transition to ${target} legs failed; staying at ${this.stripeActive}:`, e);
      return;
    }
    if (this.closing || generation !== this.stripeGeneration) {
      // A newer transition (or close) superseded this dial set.
      for (const leg of fresh) leg.close();
      return;
    }
    const old = this.legs;
    const firstEngage = this.stripeActive === 0;
    this.legs = fresh;
    this.stripeActive = target;
    // Suppress only once the new set is complete; on a grow the suppression
    // is already armed and the width rides the next 1 Hz refresh.
    this.sendStripeState(true, target);
    if (firstEngage) this.startStripeRefresh();
    for (const leg of old) leg.close();
    this.cb?.onStripeChange?.(target);
  }

  // Leg death (docs/35 §5.6): the cover is broken, so restore the primary's
  // full flow FIRST (the unstripe burst — one datagram loss must not cost
  // seconds of keyframe-only video), then tear the rest of the set down. The
  // controller decides whether and when to re-engage.
  private handleLegDeath(generation: number): void {
    if (this.closing || generation !== this.stripeGeneration || this.stripeActive === 0) return;
    this.stripeStats.legDeaths++;
    this.disengageStripe();
  }

  private disengageStripe(): void {
    this.stripeGeneration++;
    this.stopStripeRefresh();
    this.sendUnstripeBurst();
    const old = this.legs;
    this.legs = [];
    const wasActive = this.stripeActive;
    this.stripeActive = 0;
    for (const leg of old) leg.close();
    if (wasActive > 0) this.cb?.onStripeChange?.(0);
  }

  private async dialLeg(member: number, stripeN: number): Promise<StripeLegSession> {
    const u = new URL(this.url);
    u.searchParams.set('stripe', String(stripeN));
    u.searchParams.set('leg', String(member));
    const generation = this.stripeGeneration;
    const wt = await connectWebTransport(u.toString(), this.opts);
    const leg = new StripeLegSession(wt, member);
    // A leg carries this viewer's delta share and nothing else: same datagram
    // handler, same receive-queue raise (the buffer is the whole point), no
    // TimeSync, no server-stream reader (the relay sends legs no streams).
    applyIncomingDatagramBuffer((wt as { datagrams?: unknown }).datagrams);
    void readDatagrams(wt, (d) => this.cb?.onDatagram(d), leg.abort.signal)
      .then(() => this.handleLegDeath(generation))
      .catch(() => this.handleLegDeath(generation));
    void wt.closed.then(
      () => this.handleLegDeath(generation),
      () => this.handleLegDeath(generation),
    );
    return leg;
  }

  private sendStripeState(striped: boolean, n: number): void {
    const writer = this.timeSyncWriter;
    if (!writer) return; // no datagram writer ⇒ striping cannot engage safely
    try {
      void writer.write(encodeStripeState({ striped, stripeN: striped ? n : 0 })).catch(() => {});
    } catch {
      // a closing session may have released the writer — the TTL covers us
    }
  }

  // The unstripe transition is the one message whose loss costs frames: the
  // 1 Hz refresh runs only WHILE striped, so a single lost release would
  // leave the primary suppressed until the relay's TTL expired — seconds of
  // keyframe-only video. Send it as a short burst (the DeliveryAck
  // re-announce shape); the TTL stays the last-resort backstop.
  private sendUnstripeBurst(): void {
    this.sendStripeState(false, 0);
    for (const delayMs of [150, 300, 450]) {
      setTimeout(() => {
        if (!this.closing && this.stripeActive === 0) this.sendStripeState(false, 0);
      }, delayMs);
    }
  }

  private startStripeRefresh(): void {
    this.stopStripeRefresh();
    // Level state at 1 Hz (the R15 audio-config cadence): each send re-arms
    // the relay's TTL, so a lost refresh costs nothing and a wedged client
    // fails open to duplicates.
    this.stripeRefresh = setInterval(() => {
      if (this.stripeActive > 0) this.sendStripeState(true, this.stripeActive);
    }, 1000);
  }

  private stopStripeRefresh(): void {
    if (this.stripeRefresh != null) {
      clearInterval(this.stripeRefresh);
      this.stripeRefresh = null;
    }
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
    this.stopStripeRefresh();
    for (const leg of this.legs) leg.close();
    this.legs = [];
    this.stripeActive = 0;
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

// One stripe leg's session handle (R30). Deliberately minimal: a leg has no
// sampler, no TimeSync, no carrier counters — it is a datagram pipe with a
// member number, and everything interesting about it lives on the primary.
class StripeLegSession {
  readonly abort = new AbortController();
  readonly member: number;
  private wt: WebTransport;

  constructor(wt: WebTransport, member: number) {
    this.wt = wt;
    this.member = member;
  }

  close(): void {
    this.abort.abort();
    try {
      this.wt.close();
    } catch {
      // already closed — fine
    }
  }
}
