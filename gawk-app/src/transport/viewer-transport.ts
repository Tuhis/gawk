// The viewer's transport seam. ViewerPipeline consumes this interface instead
// of touching WebTransport directly, so the connection + read loops can run
// either in-process (LocalViewerTransport, below — the main-thread fallback and
// the no-nested-worker fallback) or in a dedicated transport worker
// (WorkerViewerTransport), where no decode/render pressure can ever starve the
// browser's small incoming-datagram queue.

import { getDvrBufferMs } from '../config';
import { log } from '../lib/logger';
import type { LocalRelayEvent } from './local-relay';
import {
  connectWebTransport,
  newCarrierCounters,
  openDatagramWriter,
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

// The one authoritative "session is over" signal: one event, one signal.
// closeCode carries the semantics when the server closed cleanly
// (4000 = broadcast ended); an abrupt drop has message only.
export interface TransportClosedInfo {
  closeCode?: number;
  reason?: string;
  message: string;
}

// Which connection delivered something: 0 (or absent) is the primary, a leg
// is 1 + its position. In the deep-buffer layout a leg's member index already
// starts at 1, so the connection index IS the member there (docs/91 D6).
export type ConnectionIndex = number;

export interface ViewerTransportCallbacks {
  onDatagram: (dgram: Uint8Array, conn?: ConnectionIndex) => void;
  onKeyframe: (kf: KeyframeStreamFrame, conn?: ConnectionIndex) => void;
  // This session's telemetry identity. The transport is where it arrives (it
  // rides a server uni stream), but nothing here acts on it — it is forwarded
  // to the pipeline and out to the main thread, which is the only place
  // collection happens.
  onTelemetryHello?: (hello: TelemetryHelloMessage) => void;
  onTelemetryEndpoint?: (url: string) => void;
  // The relay's capabilities. The stripe controller gates every engagement on
  // CAP_STRIPED_DELIVERY — a relay that never advertises is never dialed for
  // legs.
  onRelayCapabilities?: (caps: RelayCapabilities) => void;
  // R74: 0x18 / 0x19, forwarded to the main thread (features/pop).
  onLocalRelay?: (ev: LocalRelayEvent) => void;
  // The stripe width actually engaged (0 = unstriped). Fires on engage,
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
  // Latest relay clock-sync sample: local→relay clock offset + a
  // self-owned RTT. Null until the first ping/pong completes (or where the
  // session can't send datagrams). Lives in the transport because it owns the
  // reply timing — on the worker path a postMessage hop would add jitter.
  sampleTimeSync(): TimeSyncStats | null;
  // The reliable-carrier tallies — how the mode row tells `reliable` from
  // `requested but datagrams served`. Optional so test fakes without a carrier
  // path keep compiling; null before connect.
  sampleCarrierStats?(): CarrierCounters | null;
  // What this session's incoming datagram queue was raised to, and whether the
  // browser honoured it. Lives on the transport because only the realm holding
  // the WebTransport can set or read the attribute — on the worker path the
  // main thread has no handle on it at all. Null before connect, and on
  // transports that never touch a real session.
  sampleDatagramBuffer?(): DatagramBufferStats | null;
  // Ask for a stripe of n legs (0 disengages). The transport owns the whole
  // transition — dial-before-suppress, the 0x10 level protocol, leg-death
  // fallback — so the caller only ever states a target. Optional so fakes
  // without striping keep compiling. The layout is fixed while legs are up.
  setStripe?(n: number, layout?: StripeLayout): void;
  // The live stripe tallies for stats (null before connect).
  sampleStripe?(): StripeTransportStats | null;
  close(): void;
}

// How legs split the stream.
//   live  R30 (docs/35): legs 0 … n−1 carry live-edge delta chunks, the
//         primary is suppressed to keyframes, StripeN on the wire = n.
//   dvr   R87 (docs/91 D6): a deep-buffer session; the primary keeps GOP share
//         0, legs 1 … n carry the other shares whole (keyframe stream +
//         carrier), StripeN on the wire = n + 1, the width with the primary.
export type StripeLayout = 'live' | 'dvr';

// The DVR buffer this session asked for: the primary URL's `buffer` parameter,
// which the legs copy; the configured default when absent.
function dvrBufferMsOf(url: string): number {
  const ms = Number(new URL(url).searchParams.get('buffer'));
  return Number.isFinite(ms) && ms > 0 ? ms : getDvrBufferMs();
}

// The most legs a layout may dial. Both keep one viewer at MAX_STRIPE_LEGS
// connections beside the primary's role: in dvr the primary is one of them
// (docs/91 D12).
export function maxLegs(layout: StripeLayout): number {
  return layout === 'dvr' ? MAX_STRIPE_LEGS - 1 : MAX_STRIPE_LEGS;
}

// A leg dials the primary's URL — delivery, buffer, owner, parity and the
// client identity included — plus its place in the stripe.
export function stripeLegUrl(primaryUrl: string, member: number, stripeN: number): string {
  const u = new URL(primaryUrl);
  u.searchParams.set('stripe', String(stripeN));
  u.searchParams.set('leg', String(member));
  return u.toString();
}

// Stripe state as the overlay/controller sees it.
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

// The viewer-minted session-group token — 8 random bytes as lowercase hex,
// appended as ?owner= to the primary subscribe dial (viewer.ts) and inherited
// by every leg dial (dialLeg copies the primary URL). It is the relay's only
// handle tying one viewer's sessions together, so a primary's death reaps its
// legs. Minted per pipeline attempt: legs are per-attempt, and a reconnect's
// fresh set must not share the dead set's identity. Not a credential — the
// worst a forged token achieves is getting the forger's own sessions reaped.
export function mintStripeOwnerToken(): string {
  const bytes = new Uint8Array(8);
  const c = (globalThis as { crypto?: Crypto }).crypto;
  if (c?.getRandomValues) {
    c.getRandomValues(bytes);
  } else {
    // Non-secure-context fallback: the token only needs to be unique among
    // concurrent viewers of one broadcast, not unguessable.
    for (let i = 0; i < bytes.length; i++) bytes[i] = Math.floor(Math.random() * 256);
  }
  return Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join('');
}

// In-process transport: owns the WebTransport session and its read loops in
// this realm.
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
  // The code the relay said, in-band, it is about to close
  // this session with. Chrome never reads the close code itself, so a close
  // (or a drop) that arrives without one reports this instead.
  private noticedCloseCode: number | undefined;
  private cb: ViewerTransportCallbacks | null = null;

  // Stripe state. legs is the CURRENT set; a transition dials a whole fresh set
  // before touching it (make-before-break), so at every instant either the
  // primary or a complete leg set covers the frame.
  private legs: StripeLegSession[] = [];
  // Deep-buffer legs a transition superseded, still read and heartbeated until
  // their drain timer closes them (see drainLegs).
  private draining = new Map<StripeLegSession, ReturnType<typeof setTimeout>>();
  // The draining legs' own 1 Hz heartbeat, independent of the stripe refresh
  // so a leg-death disengage cannot stop it mid-drain.
  private drainRefresh: ReturnType<typeof setInterval> | null = null;
  private stripeTarget = 0;
  private stripeActive = 0;
  private stripeLayout: StripeLayout = 'live';
  private stripeGeneration = 0; // bumps per transition; stale dials discard
  private liveStripeGeneration = -1; // the committed leg set's generation
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
    // close() aborts a handshake still in flight, so a viewer torn down
    // before the relay answered leaves no session to open later unowned.
    const wt = await connectWebTransport(this.url, this.opts, this.abort.signal);
    this.wt = wt;
    this.cb = cb;
    this.sampler = new ConnectionStatsSampler(wt);

    // Raise the browser's incoming datagram queue before a single read happens,
    // because it is the queue — not the reader — that decides whether a frame's
    // burst survives. This is deliberately here rather than in the shared
    // connectWebTransport: a broadcaster's incoming queue carries only control
    // traffic, and this class is the one object that exists in every viewer
    // placement (main thread, viewer worker, and the nested transport worker),
    // so setting it here reaches all three with no message plumbing.
    this.datagramBuffer = applyIncomingDatagramBuffer(
      (wt as { datagrams?: unknown }).datagrams,
    );

    // Relay clock sync: ping over this session's datagrams; replies are
    // intercepted below, before the video path ever sees them. Feature-
    // detected so test fakes / odd environments without a writable datagram
    // stream simply report null. openDatagramWriter covers WebKit, which has
    // only `createWritable()`.
    const writer = openDatagramWriter(wt);
    if (writer) {
      this.timeSyncWriter = writer;
      // A ping that never leaves makes `timeSyncRttMs` read null forever,
      // with no clue as to the cause if this rejection is swallowed. Still
      // non-fatal (a failed ping must never take the pipeline down), but not
      // silent; logged once so a broken leg doesn't spam a 0.5 Hz warning for
      // the life of the session.
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
    // keyframes arrive as reliable unidirectional streams. On a joining
    // viewer the relay primes us with the last keyframe over a stream, so the
    // first picture typically appears without waiting for the next keyframe.
    void readDatagrams(
      wt,
      (dgram) => {
        if (this.timeSync?.handleDatagram(dgram)) return; // a TimeSync reply
        cb.onDatagram(dgram);
      },
      this.abort.signal,
    )
      .then(() => this.handleReadLoopEnd(cb, wt, null))
      .catch((e) => this.handleReadLoopEnd(cb, wt, e instanceof Error ? e : new Error(String(e))));

    // Server streams (keyframes + reliable carriers): failures here are not
    // fatal to the session (the next keyframe recovers, and a real drop
    // surfaces via the datagram loop / wt.closed), so they are logged, not
    // propagated. Carrier records are verbatim datagrams — they feed the same
    // handler, and the pipeline never learns which transport delivered the
    // bytes.
    void readServerStreams(
      wt,
      {
        onKeyframe: cb.onKeyframe,
        onCarrierRecord: (record) => cb.onDatagram(record),
        onTelemetryHello: (hello) => cb.onTelemetryHello?.(hello),
        onTelemetryEndpoint: (url) => cb.onTelemetryEndpoint?.(url),
        onRelayCapabilities: (caps) => cb.onRelayCapabilities?.(caps),
        onLocalRelay: (ev) => cb.onLocalRelay?.(ev),
        onSessionClosing: (code) => {
          this.noticedCloseCode = code;
        },
      },
      this.carrier,
      this.abort.signal,
    ).catch((e) => {
      if (!this.closing) log.warn('Server stream loop ended:', e);
    });
  }

  // --- Striping --------------------------------------------------------------

  setStripe(n: number, layout: StripeLayout = 'live'): void {
    // A layout belongs to the delivery mode, which is fixed per session; it
    // can only change while no legs are up.
    if (this.stripeActive === 0) this.stripeLayout = layout;
    const target = Math.max(0, Math.min(maxLegs(this.stripeLayout), Math.floor(n)));
    this.stripeTarget = target;
    this.stripeStats.target = target;
    if (this.closing) return;
    if (target === this.stripeActive) {
      // Already there: whatever is still dialing is now unwanted, and must
      // not commit later and leave the transport wider than asked.
      this.stripeGeneration++;
      return;
    }
    if (target === 0) {
      this.disengageStripe();
      return;
    }
    void this.transitionStripe(target);
  }

  sampleStripe(): StripeTransportStats | null {
    return { ...this.stripeStats, active: this.stripeActive, target: this.stripeTarget };
  }

  // Make-before-break: dial the WHOLE fresh set, and only once every leg is up
  // switch over (first engage additionally arms the primary suppression). Any
  // dial failure abandons the transition and keeps the current state — capacity
  // pressure degrades striping before it degrades the session. Duplicates
  // during the overlap are the reassembler's to drop; holes are structurally
  // impossible because the old cover (primary or old leg set) stays live until
  // the new one is complete.
  private async transitionStripe(target: number): Promise<void> {
    const generation = ++this.stripeGeneration;
    const dials: Promise<StripeLegSession>[] = [];
    // The deep-buffer primary keeps share 0, so its legs are 1 … target
    // (docs/91 D6); a relay answers 400 to leg=0 there.
    const first = this.stripeLayout === 'dvr' ? 1 : 0;
    for (let j = first; j < first + target; j++) {
      this.stripeStats.legDials++;
      dials.push(this.dialLeg(j, this.wireStripeN(target)));
    }
    const settled = await Promise.allSettled(dials);
    const fresh = settled.flatMap((r) => (r.status === 'fulfilled' ? [r.value] : []));
    const failure = settled.find((r): r is PromiseRejectedResult => r.status === 'rejected');
    if (failure) {
      this.stripeStats.legDialFailures++;
      for (const leg of fresh) leg.close();
      if (!this.closing) log.warn(`stripe transition to ${target} legs failed; staying at ${this.stripeActive}:`, failure.reason);
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
    this.liveStripeGeneration = generation;
    this.stripeActive = target;
    // Suppress only once the new set is complete; on a grow the suppression
    // is already armed and the width rides the next 1 Hz refresh.
    this.sendStripeState(true, this.wireStripeN(target));
    // First heartbeat immediately: the relay armed each leg's liveness lease at
    // subscribe, and the 1 Hz refresh below is what keeps renewing it.
    this.sendLegHeartbeats();
    if (firstEngage) this.startStripeRefresh();
    this.retireLegs(old);
    this.cb?.onStripeChange?.(target);
  }

  // A superseded live leg carries only datagrams the new set duplicates, so it
  // closes at once. A superseded deep-buffer leg may still hold a backlog of
  // its GOPs: the primary skipped them, and a new leg starts at the newest GOP
  // of its share, so closing it would lose up to the buffer's worth of video
  // (docs/91 D11). It drains instead.
  private retireLegs(old: StripeLegSession[]): void {
    if (this.stripeLayout === 'dvr') this.drainLegs(old);
    else for (const leg of old) leg.close();
  }

  // Tells each superseded leg to drain — StripeState Striped=false on the
  // leg's own session: the relay then serves it only the GOPs its ring holds
  // now and closes it once they are delivered — and keeps reading it until
  // then. The signal is idempotent, so it goes out three times 100 ms apart
  // (a datagram can be lost) and then doubles as the leg's 1 Hz heartbeat,
  // which keeps its lease while it drains. Closing is the relay's; the DVR
  // buffer plus a second (a leg is never further behind than its buffer
  // before the relay resyncs it, D3) is the backstop. Whatever a draining leg
  // delivers that a new leg also delivers is dropped by frameId (D9). Its
  // close is not a leg death: its generation is no longer the live one.
  private drainLegs(old: StripeLegSession[]): void {
    if (old.length === 0) return;
    const drainMs = dvrBufferMsOf(this.url) + 1000;
    for (const leg of old) {
      const timer = setTimeout(() => this.forgetDraining(leg), drainMs);
      this.draining.set(leg, timer);
      leg.drain();
      for (const delayMs of [100, 200]) {
        setTimeout(() => {
          if (this.draining.has(leg)) leg.drain();
        }, delayMs);
      }
    }
    if (!this.drainRefresh) {
      this.drainRefresh = setInterval(() => {
        for (const leg of this.draining.keys()) leg.drain();
        if (this.draining.size === 0) this.stopDrainRefresh();
      }, 1000);
    }
  }

  // A draining leg is done: the relay closed it, or the backstop fired.
  private forgetDraining(leg: StripeLegSession): void {
    const timer = this.draining.get(leg);
    if (timer === undefined) return;
    clearTimeout(timer);
    this.draining.delete(leg);
    leg.close();
    if (this.draining.size === 0) this.stopDrainRefresh();
  }

  private stopDrainRefresh(): void {
    if (this.drainRefresh != null) {
      clearInterval(this.drainRefresh);
      this.drainRefresh = null;
    }
  }

  private closeDraining(): void {
    for (const [leg, timer] of this.draining) {
      clearTimeout(timer);
      leg.close();
    }
    this.draining.clear();
    this.stopDrainRefresh();
  }

  // Leg death: the cover is broken, so restore the primary's full flow FIRST
  // (the unstripe burst — one datagram loss must not cost seconds of
  // keyframe-only video), then tear the rest of the set down. The controller
  // decides whether and when to re-engage.
  private handleLegDeath(generation: number): void {
    if (this.closing || generation !== this.liveStripeGeneration || this.stripeActive === 0) return;
    this.stripeStats.legDeaths++;
    this.disengageStripe(false);
  }

  // A requested disengage drains deep-buffer legs like any transition; a
  // leg-death fallback tears the set down at once, as it always has.
  private disengageStripe(drain = true): void {
    this.stripeGeneration++;
    this.liveStripeGeneration = -1;
    this.stopStripeRefresh();
    this.sendUnstripeBurst();
    const old = this.legs;
    this.legs = [];
    const wasActive = this.stripeActive;
    this.stripeActive = 0;
    if (drain) this.retireLegs(old);
    else for (const leg of old) leg.close();
    if (wasActive > 0) this.cb?.onStripeChange?.(0);
  }

  // StripeN as the relay reads it: the leg count in live mode, the width with
  // the primary in the deep-buffer layout (docs/91 D7).
  private wireStripeN(legs: number): number {
    return this.stripeLayout === 'dvr' ? legs + 1 : legs;
  }

  private async dialLeg(member: number, stripeN: number): Promise<StripeLegSession> {
    const generation = this.stripeGeneration;
    const dvr = this.stripeLayout === 'dvr';
    const conn: ConnectionIndex = dvr ? member : member + 1;
    const wt = await connectWebTransport(stripeLegUrl(this.url, member, stripeN), this.opts, this.abort.signal);
    const leg = new StripeLegSession(wt, member);
    const ended = () => {
      this.forgetDraining(leg);
      this.handleLegDeath(generation);
    };
    // A live leg carries this viewer's delta share and nothing else: same
    // datagram handler, same receive-queue raise (the buffer is the whole
    // point), no TimeSync, no server-stream reader (the relay sends live legs
    // no streams).
    applyIncomingDatagramBuffer((wt as { datagrams?: unknown }).datagrams);
    void readDatagrams(wt, (d) => this.cb?.onDatagram(d, conn), leg.abort.signal).then(ended, ended);
    // A deep-buffer leg is served its GOPs exactly as the primary is, a
    // keyframe stream and a carrier each (docs/91 D3), so it gets the same
    // stream reader feeding the same pipeline. It carries no audio, control or
    // telemetry (D5): only the media callbacks are wired. Carrier tallies are
    // shared, so the overlay's carrier rows count every connection.
    if (dvr) {
      void readServerStreams(
        wt,
        {
          onKeyframe: (kf) => this.cb?.onKeyframe(kf, conn),
          onCarrierRecord: (record) => this.cb?.onDatagram(record, conn),
        },
        this.carrier,
        leg.abort.signal,
      ).catch((e) => {
        if (!this.closing) log.warn(`stripe leg ${member} stream loop ended:`, e);
      });
    }
    void wt.closed.then(ended, ended);
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

  // Each leg gets the same 1 Hz StripeState the primary does, as a liveness
  // heartbeat: the relay reaps a leg that goes StripeLegLease (20 s) without
  // any inbound datagram — the cross-pod backstop for legs whose primary lives
  // on a different pod. The relay's leg route only renews the lease and
  // discards the bytes, so the message is semantically inert there; failures
  // are swallowed like the primary's refresh (a leg that cannot send is exactly
  // what the lease is for).
  private sendLegHeartbeats(): void {
    if (this.stripeActive === 0) return;
    for (const leg of this.legs) leg.heartbeat(this.wireStripeN(this.stripeActive));
  }

  private startStripeRefresh(): void {
    this.stopStripeRefresh();
    // Level state at 1 Hz: each send re-arms the relay's TTL, so a lost refresh
    // costs nothing and a wedged client fails open to duplicates. The legs
    // heartbeat on the same tick — one timer owns the whole stripe's liveness
    // signalling.
    this.stripeRefresh = setInterval(() => {
      if (this.stripeActive > 0) {
        this.sendStripeState(true, this.wireStripeN(this.stripeActive));
        this.sendLegHeartbeats();
      }
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
    closeCode ??= this.noticedCloseCode;
    const message = reason
      ? `WebTransport session closed: ${reason}`
      : 'WebTransport session closed by server';
    cb.onClosed({ closeCode, reason, message });
  }

  // An abrupt drop (read loop died, no close frame): message only.
  private reportDropped(cb: ViewerTransportCallbacks, err: Error): void {
    if (this.closing || this.closedReported) return;
    // The relay said why before it closed: this is that close, not a
    // drop — Chrome just never delivered its code.
    if (this.noticedCloseCode !== undefined) {
      this.reportClosed(cb, this.noticedCloseCode, err.message);
      return;
    }
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
    this.closeDraining();
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

// One stripe leg's session handle. Deliberately minimal: a leg has no
// sampler, no TimeSync, no carrier counters of its own — it is a media pipe
// with a member number (datagrams; in the deep-buffer layout also its GOP
// streams, read in dialLeg), and everything interesting about it lives on the
// primary.
// Its one outbound concern is the liveness heartbeat the relay's leg lease is
// renewed by.
class StripeLegSession {
  readonly abort = new AbortController();
  readonly member: number;
  private wt: WebTransport;
  private writer: WritableStreamDefaultWriter<BufferSource> | null = null;

  constructor(wt: WebTransport, member: number) {
    this.wt = wt;
    this.member = member;
    try {
      this.writer = openDatagramWriter(wt);
    } catch {
      this.writer = null; // a locked/odd writable — the lease will reap us
    }
  }

  // The relay's drain signal for a superseded deep-buffer leg (docs/91 D11).
  drain(): void {
    const writer = this.writer;
    if (!writer) return;
    try {
      void writer.write(encodeStripeState({ striped: false, stripeN: 0 })).catch(() => {});
    } catch {
      // a closing leg may have released the writer — the backstop closes it
    }
  }

  heartbeat(stripeN: number): void {
    const writer = this.writer;
    if (!writer) return;
    try {
      void writer.write(encodeStripeState({ striped: true, stripeN })).catch(() => {});
    } catch {
      // a closing leg may have released the writer — nothing to renew
    }
  }

  close(): void {
    this.abort.abort();
    try {
      this.writer?.releaseLock();
    } catch {
      // a pending write may hold the lock — the session close ends it anyway
    }
    this.writer = null;
    try {
      this.wt.close();
    } catch {
      // already closed — fine
    }
  }
}
