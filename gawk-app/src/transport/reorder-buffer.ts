// Viewer reorder / playout buffer (R8, docs/12 Decision 7).
//
// R8 splits video across two transports: keyframes arrive reliably over
// unidirectional streams, deltas arrive fast-but-lossy over datagrams. That
// makes the channels race — a delta N+1 can arrive before its keyframe N
// finishes on the stream. This buffer merges the two by frameId and releases
// frames to the decoder in decode order.
//
// By default this is NOT a fixed-offset de-jitter buffer: the project favors
// the latest frame over smooth-but-late playback, so there is no constant
// playout delay and frames are released as soon as they are decodable. R5 Q3
// adds an *opt-in* smoothed mode (playout.ts, default off): when the viewer
// enables it, a decodable frame is additionally held until
// `now >= timestampMs + arrivalBaseline + offset` — a constant offset from
// the source clock, anchored by the windowed-min arrival baseline. Smoothing
// adds delay, never patience: every drop/resync policy below fires unchanged.
// The two bounded waits exist only to disambiguate the two-channel race:
//
//   - KEYFRAME_WAIT_MS: while waiting for a keyframe (initial sync, or after a
//     declared gap), decodable-pending frames are held at most this long.
//     Keyframes are reliable so one WILL arrive; this is only a safety cap on
//     undecodable buffering.
//   - DELTA_GAP_GRACE_MS: when the next contiguous delta is missing but later
//     frames have arrived, wait this long for the straggler before declaring a
//     gap. A lost delta never retransmits, so this is short — a couple of frame
//     intervals — after which we freeze and resync at the next keyframe.
//
// Pure and timer-free: the pipeline injects the clock and calls tick()
// periodically (e.g. once per rendered frame). Fully unit-testable in node.

import {
  LIVE_EDGE_BUCKET_MS,
  QUANTILE_BIN_MS,
  WindowedMinTracker,
  WindowedQuantileTracker,
} from './live-edge';
import {
  DECODE_LEAD_MS,
  OFFSET_SLEW_DOWN_MS_PER_S,
  OFFSET_SLEW_UP_MS_PER_S,
  PlayoutController,
  getPlayoutMode,
  getPlayoutOffsetMs,
  getPlayoutProfile,
  type SlewEnvelope,
} from './playout';
import {
  RESILIENT_DELTA_GAP_GRACE_MS,
  RESILIENT_KEYFRAME_WAIT_MS,
  getLossAllowanceFrames,
  RESILIENT_MAX_BUFFERED_FRAMES,
  getResilientMode,
} from './resilient';
import { frameIdAhead, nextFrameId, type DecoderConfigMessage } from './wire';

// Tunables, named in one place (à la media/fallback.ts) for later real-world
// tuning. Times are milliseconds.
//
// KEYFRAME_WAIT_MS must cover the real-world latency gap between the two
// channels, not just reordering jitter: a keyframe is store-and-forwarded as
// a single large stream (~236 KB at native ultrawide) and was measured (R10
// field finding, docs/14) landing > 500 ms behind its trailing datagram
// deltas on a congested peer. At 200 ms those deltas expired before their
// keyframe arrived, so every GOP degenerated into keyframe-only playback.
// 1000 ms covers the measured worst case with margin and stays within
// MAX_BUFFERED_FRAMES (~1.07 s at 60 fps), which remains the memory bound.
export const KEYFRAME_WAIT_MS = 1000;
export const DELTA_GAP_GRACE_MS = 60;
// Ceiling for the adaptive grace below. 250 ms is not a fresh guess: it is
// RESILIENT_DELTA_GAP_GRACE_MS, already shipped and measured as the patience a
// deliberately-buffered viewer wants, and it stays far under KEYFRAME_WAIT_MS
// (so a widened grace can never age deltas out before their keyframe — docs/14's
// keyframe-only 2 fps failure) and under MAX_BUFFERED_FRAMES (~8 frames at
// 30 fps against a 64-frame cap).
export const MAX_DELTA_GAP_GRACE_MS = 250;
// Hard cap on buffered frames; guards against a lingering stale frame (e.g. a
// straggler above the decode position after a broadcaster restart) growing the
// buffer without bound. Oldest-received entries are dropped past this.
export const MAX_BUFFERED_FRAMES = 64;

// How far the keyframe wait must outlast the playout offset (R21). One GOP at
// the broadcaster default, so a delta held for its keyframe always survives at
// least until the following keyframe is due.
export const KEYFRAME_WAIT_PLAYOUT_HEADROOM_MS = 500;

// R19 (docs/24 Decision 7): the three bounds widen while resilient mode is
// on — read live per use so the defaults stay byte-identical when it's off.
function keyframeWaitMs(): number {
  const base = getResilientMode() ? RESILIENT_KEYFRAME_WAIT_MS : KEYFRAME_WAIT_MS;
  // The wait must outlast the delay the playout offset deliberately imposes
  // (R21). While waiting for a keyframe, a held delta is dropped once it is
  // older than this — but its keyframe does not become *due* until the offset
  // has elapsed, so a wait shorter than the offset ages out every delta before
  // its keyframe can release, leaving keyframe-only playback: 2 fps at a
  // 500 ms GOP. R19's 2 s wait covered its <=2 s clamp; R21's 3 s buffer
  // exceeded it. Deriving from the live offset keeps the two in step whatever
  // the profile is tuned to. No effect below the base: live-edge (offset 0)
  // and resilient (~0.5 s) keep exactly the values they had.
  return Math.max(base, getPlayoutOffsetMs() + KEYFRAME_WAIT_PLAYOUT_HEADROOM_MS);
}
// R30 finding 4 (docs/35): the adaptive live-edge grace.
//
// DELTA_GAP_GRACE_MS was sized per CONNECTION — "a couple of frame intervals",
// correct while a frame's datagrams arrived back-to-back on one QUIC
// connection, where a chunk outstanding past the grace really was lost.
// Striping spreads each frame over N legs whose mutual skew becomes per-frame
// completion jitter, and a live Firefox 154 session measured that jitter at
// 101 ms median / 268 ms p95 against a 60 ms grace: ~1 gap resync per second,
// against 0.5 % of frames actually lost. Almost every freeze was a frame that
// was merely LATE.
//
// The lever is the jitter the buffer already measures for the playout offset,
// and what makes it the right signal is that it separates the two failure
// modes with no extra input: a late frame ARRIVES, so it inflates p95 and buys
// itself patience; a lost frame never arrives, contributes nothing, and leaves
// the grace at its floor to freeze fast. A larger constant could not do that —
// it would buy the striped path patience by making a genuinely lossy path
// slower to resync.
//
// Seed and floor are both DELTA_GAP_GRACE_MS, so a viewer that has not yet
// measured anything — and any link whose jitter fits inside the shipped
// constant — behaves exactly as before.
export const GRACE_ENVELOPE: SlewEnvelope = {
  seedMs: DELTA_GAP_GRACE_MS,
  minMs: DELTA_GAP_GRACE_MS,
  maxMs: MAX_DELTA_GAP_GRACE_MS,
  slewUpMsPerS: OFFSET_SLEW_UP_MS_PER_S,
  slewDownMsPerS: OFFSET_SLEW_DOWN_MS_PER_S,
  // Unlike the playout offset, grace is patience rather than delay: raising it
  // changes nothing about when an on-time frame is presented, only how long a
  // hole is tolerated before freezing. A step up is therefore invisible where
  // a stepped offset would be a skip, and under-patience costs a visible
  // freeze NOW — so a large rise is taken at once (the reasoning
  // RESILIENT_PLAYOUT_PROFILE uses, with none of its cost). Small wobble still
  // slews, which keeps freeze behaviour stable and reproducible, and DOWN is
  // never stepped.
  stepUpAboveMs: 50,
};

// Module state, like the playout mode and the R29 loss allowance: the buffer
// reads it live per advance, and it is driven from the pipeline's stats tick
// (viewer.ts) because taking a windowed quantile is far too expensive to do
// per arrival.
const graceController = new PlayoutController(GRACE_ENVELOPE);

// Driven by the pipeline's stats tick with the same arrival-jitter estimate
// that feeds the playout offset and the overlay.
export function updateGraceController(jitterMs: number | null, nowMs: number): void {
  graceController.update(jitterMs, nowMs);
}

// Broadcaster restart: frame timestamps move to a new timeline, so the
// arrival-jitter window resets — and so must anything reading it.
export function resetGraceController(): void {
  graceController.reset();
}

export function deltaGapGraceMs(): number {
  // Resilient/DVR delivery keeps its deliberate constant: those deltas ride
  // reliable carriers, so a hole means something other than skew and the
  // envelope was chosen for the buffer depth, not for the link.
  return getResilientMode() ? RESILIENT_DELTA_GAP_GRACE_MS : graceController.offsetMs();
}
function maxBufferedFrames(): number {
  return getResilientMode() ? RESILIENT_MAX_BUFFERED_FRAMES : MAX_BUFFERED_FRAMES;
}

// R19 hardening (PLAYOUT-1): the arrival-jitter histogram's geometry belongs
// to the playout profile that consumes it — a controller clamped to 2000 ms
// off a histogram that saturates at 500 ms can never buffer more than ~534 ms.
// Unlike the bounds above, geometry can't be read per use (the histogram is a
// fixed-shape structure), so the tracker is rebuilt when the profile changes.
function newQuantileTracker(): WindowedQuantileTracker {
  const p = getPlayoutProfile();
  return new WindowedQuantileTracker(
    p.jitterWindowMs,
    LIVE_EDGE_BUCKET_MS,
    QUANTILE_BIN_MS,
    p.quantileRangeMs,
  );
}

export interface ReorderKeyframe {
  frameId: number;
  timestampUs: bigint;
  // Embedded decoder config (present on stream keyframes); null if none.
  config: DecoderConfigMessage | null;
  data: Uint8Array; // encoded keyframe payload
}

export interface ReorderDelta {
  frameId: number;
  timestampUs: bigint;
  data: Uint8Array; // reassembled encoded delta payload
}

export interface ReleasedFrame {
  frameId: number;
  keyframe: boolean;
  timestampUs: bigint;
  data: Uint8Array;
  // Non-null only for keyframes carrying a config; the pipeline dedups + applies.
  config: DecoderConfigMessage | null;
}

export interface ReorderStats {
  released: number;
  keyframesReleased: number;
  // Deltas dropped as stale (frameId <= decode position) or evicted by the cap.
  deltasDropped: number;
  // Times a missing delta was declared a gap and we froze to await a keyframe.
  gapResyncs: number;
  // R29 FP6 (docs/34 §6): unrecovered frames skipped within the GOP's budget
  // instead of forfeiting the rest of the GOP. Deliberately NOT folded into
  // gapResyncs — docs/13's playbook reads that as "delta loss is eating GOPs",
  // and a skip is the opposite outcome: the GOP survived. One counter meaning
  // two things would retire a working signal.
  framesSkippedWithinAllowance: number;
  // Frames dropped while waiting for a keyframe (undecodable, aged out).
  keyframeWaitDrops: number;
  // Held (buffered, not yet released) frames right now.
  buffered: number;
}

interface Entry {
  frameId: number;
  keyframe: boolean;
  timestampUs: bigint;
  data: Uint8Array;
  config: DecoderConfigMessage | null;
  receivedAtMs: number;
}

export interface ReorderBufferOptions {
  // Invoked when a keyframe arrives serially behind the decode position — the
  // broadcaster-restart signal (R10 field finding; R5 Q1, docs/15). Frame
  // timestamps move to a new timeline across a restart, so live-edge baselines
  // built against the old one must reset. The rare other cause (two keyframe
  // streams read out of order) costs one harmless baseline rebuild.
  onRestart?: () => void;
  // Playout offset in ms, read on every advance so a live toggle re-paces
  // (R5 Q3). 0 = live-edge (the default, via playout.ts); injectable for
  // tests.
  playoutOffsetMs?: () => number;
  // R12 T2 (docs/17 Decision 4): how much earlier than the presentation
  // target a frame releases to the decoder. Non-zero only in adaptive mode —
  // the sink holds the decoded frame for its display slot, so releasing at
  // target − lead keeps the decoder frame pool bounded while the paint stays
  // on time. Fixed mode keeps its R5 Q3 semantics (lead 0). Injectable.
  decodeLeadMs?: () => number;
}

export class ReorderBuffer {
  private onFrame: (frame: ReleasedFrame) => void;
  private now: () => number;
  private onRestart: (() => void) | undefined;
  private playoutOffsetMs: () => number;
  private decodeLeadMs: () => number;
  // Windowed min of (arrivalMs − timestampMs): the pacing anchor (R5 Q3).
  // Reset with the restart signal — new session, new timestamp timeline.
  // Its window is profile-independent on purpose: `releasableAt` anchors the
  // release schedule on this min, so the offset (≈ p95 − this min) and the
  // anchor have to be measured against the same baseline.
  private arrivalBaseline = new WindowedMinTracker();
  // Windowed quantile of the same delta (R12 T1): p95 − min is the arrival
  // jitter, and the same estimator feeds the adaptive playout offset (T3).
  // Geometry comes from the active playout profile (R19 PLAYOUT-1).
  private arrivalQuantile = newQuantileTracker();
  private quantileProfile = getPlayoutProfile();

  private buffer = new Map<number, Entry>();
  // frameId of the last frame released to the decoder; null before the first.
  private decodePosition: number | null = null;
  // True when we need a keyframe to (re)sync: before the first frame, after a
  // declared delta gap, or on an explicit resync request (decoder backpressure).
  private waitingForKeyframe = true;

  // R29 FP6: unrecovered frames skipped in the CURRENT GOP. Reset at every
  // released keyframe, which is what makes the budget per-GOP rather than
  // per-session.
  private gopSkips = 0;

  private stats: ReorderStats = {
    released: 0,
    keyframesReleased: 0,
    deltasDropped: 0,
    gapResyncs: 0,
    framesSkippedWithinAllowance: 0,
    keyframeWaitDrops: 0,
    buffered: 0,
  };

  constructor(
    onFrame: (frame: ReleasedFrame) => void,
    now: () => number = () => performance.now(),
    opts: ReorderBufferOptions = {},
  ) {
    this.onFrame = onFrame;
    this.now = now;
    this.onRestart = opts.onRestart;
    this.playoutOffsetMs = opts.playoutOffsetMs ?? getPlayoutOffsetMs;
    this.decodeLeadMs =
      opts.decodeLeadMs ?? (() => (getPlayoutMode() === 'adaptive' ? DECODE_LEAD_MS : 0));
  }

  pushKeyframe(kf: ReorderKeyframe): void {
    // A keyframe is self-contained. If it's not newer than what we've already
    // decoded within this session it's a stale duplicate — but a frameId reset
    // (broadcaster restart) also looks "older", so only drop an exact duplicate
    // of the current position; anything else is buffered and the resync logic
    // sorts out ordering (jumping backwards on restart is intentional).
    if (this.decodePosition !== null && kf.frameId === this.decodePosition) {
      return; // exact duplicate of the just-decoded frame
    }
    // A keyframe BEHIND the decode position (serially) is the restart signal:
    // frameIds reset while we were mid-session. Resync to it immediately —
    // waiting out the delta-gap grace would stale-drop the new session's
    // first deltas against the old position, costing an extra GOP of freeze
    // after every restart (R10 field finding, docs/14). The rare other cause
    // (two keyframe streams read out of order) costs one brief jump back and
    // self-heals at the next keyframe.
    const backwards =
      this.decodePosition !== null && !frameIdAhead(kf.frameId, this.decodePosition);
    if (backwards) {
      this.arrivalBaseline.reset();
      this.arrivalQuantile.reset();
      this.onRestart?.();
    }
    if (backwards && !this.waitingForKeyframe) {
      this.stats.gapResyncs++;
      this.waitingForKeyframe = true;
    }
    this.insert({
      frameId: kf.frameId,
      keyframe: true,
      timestampUs: kf.timestampUs,
      data: kf.data,
      config: kf.config,
      receivedAtMs: this.now(),
    });
    this.advance();
  }

  pushDelta(d: ReorderDelta): void {
    // A delta at or behind the decode position is stale (already superseded).
    // Serial comparison: a delta just past the uint32 rollover is ahead.
    if (this.decodePosition !== null && !frameIdAhead(d.frameId, this.decodePosition)) {
      this.stats.deltasDropped++;
      return;
    }
    this.insert({
      frameId: d.frameId,
      keyframe: false,
      timestampUs: d.timestampUs,
      data: d.data,
      config: null,
      receivedAtMs: this.now(),
    });
    this.advance();
  }

  // Called periodically by the pipeline so the time-bounded waits elapse even
  // without new arrivals.
  tick(): void {
    this.advance();
  }

  // Forces a resync at the next keyframe — used when the decoder queue is deep
  // (drop-to-keyframe) so the viewer catches up to live.
  requestResync(): void {
    if (!this.waitingForKeyframe) this.stats.gapResyncs++;
    this.waitingForKeyframe = true;
    this.advance();
  }

  getStats(): ReorderStats {
    return { ...this.stats, buffered: this.buffer.size };
  }

  // R12 T1: how much later than the session-best delta the slow tail arrives
  // (windowed p95 − windowed min, ms). Null before any frame.
  arrivalJitterMs(): number | null {
    const nowMs = this.now();
    const p95 = this.arrivalQuantile.quantile(0.95, nowMs);
    const min = this.arrivalBaseline.min(nowMs);
    if (p95 === null || min === null) return null;
    return Math.max(0, p95 - min);
  }

  reset(): void {
    this.buffer.clear();
    this.decodePosition = null;
    this.waitingForKeyframe = true;
    this.arrivalBaseline.reset();
    this.arrivalQuantile.reset();
  }

  private insert(e: Entry): void {
    // A resilient-mode flip is a deliberate reconnect (docs/24 Decision 9), so
    // in production this rebuilds nothing; it keeps the estimator honest if a
    // context ever does flip mid-session (jitter reads null until it refills,
    // which the controller treats as "no data" and holds its offset).
    const profile = getPlayoutProfile();
    if (profile !== this.quantileProfile) {
      this.quantileProfile = profile;
      this.arrivalQuantile = newQuantileTracker();
    }
    const arrivalDelta = e.receivedAtMs - Number(e.timestampUs) / 1000;
    this.arrivalBaseline.observe(arrivalDelta, e.receivedAtMs);
    this.arrivalQuantile.observe(arrivalDelta, e.receivedAtMs);
    const existing = this.buffer.get(e.frameId);
    // Keep a keyframe over a duplicate delta for the same id; otherwise ignore
    // a duplicate.
    if (existing && !(e.keyframe && !existing.keyframe)) return;
    this.buffer.set(e.frameId, e);
    this.evictIfOverCap();
  }

  private evictIfOverCap(): void {
    while (this.buffer.size > maxBufferedFrames()) {
      // Drop the oldest-received entry (Map preserves insertion order, which is
      // arrival order).
      const oldest = this.buffer.keys().next();
      if (oldest.done) return;
      this.buffer.delete(oldest.value);
      this.stats.deltasDropped++;
    }
  }

  private advance(): void {
    // Repeat until no further progress can be made.
    for (;;) {
      if (this.decodePosition === null || this.waitingForKeyframe) {
        if (this.jumpToKeyframe()) continue;
        // No keyframe available yet: drop undecodable held frames that have
        // aged past the wait cap, then stop.
        this.dropStaleWhileWaiting();
        return;
      }

      const next = nextFrameId(this.decodePosition);
      const entry = this.buffer.get(next);
      if (entry) {
        if (this.now() < this.releasableAt(entry)) return; // paced (Q3): tick re-drives
        this.release(entry);
        continue;
      }

      // The contiguous next frame is missing.
      // If a keyframe is already buffered ahead, it is a definitive, reliable
      // resync point — jump to it now (latest-first) rather than waiting on
      // deltas that clearly lost the race to it.
      if (this.hasBufferedKeyframeAbove(this.decodePosition)) {
        this.stats.gapResyncs++;
        this.waitingForKeyframe = true;
        continue;
      }
      // No keyframe yet: wait briefly for a straggler delta; past the grace,
      // either spend one of the GOP's loss allowance (R29 FP6) and carry on,
      // or declare a gap and freeze until the next (reliable) keyframe.
      if (this.shouldDeclareGap()) {
        // The allowance is live-edge only: resilient/DVR deltas ride reliable
        // carriers, so a hole there means something else went wrong and
        // freezing is still the correct response.
        const allowance = getResilientMode() ? 0 : getLossAllowanceFrames();
        if (this.gopSkips < allowance) {
          this.gopSkips++;
          this.stats.framesSkippedWithinAllowance++;
          // Step over the hole and keep decoding. Frames after it reference
          // data that never arrived, so they carry artifacts until the next
          // keyframe — the trade docs/34 §6 makes explicit, and the reason
          // the budget is bounded and operator-set rather than unlimited.
          this.decodePosition = next;
          continue;
        }
        this.stats.gapResyncs++;
        this.waitingForKeyframe = true;
        continue;
      }
      return;
    }
  }

  // Jumps to the freshest buffered keyframe that is actually DUE (most
  // recently arrived among those the playout schedule allows — the newest
  // within a session and, across a broadcaster restart, the new one). Drops
  // buffered frames below it by frameId (undecodable / superseded). Returns
  // true if it released a keyframe.
  //
  // "Due" is part of the selection, not a check applied afterwards, and R21
  // is why. Picking the freshest keyframe overall and then rejecting it as
  // not-yet-due livelocks whenever the playout offset exceeds the GOP
  // interval: at a 3 s offset with a 500 ms GOP, a newer keyframe always
  // arrives before the current pick comes due, so the pick keeps moving
  // forward and NOTHING is ever released. The buffer fills for ever, decode
  // never starts, and every arrival counter keeps climbing while the screen
  // stays black. R19's <=500 ms offsets hid it: a keyframe came due inside
  // one GOP interval. Live-edge is unaffected — at offset 0 everything
  // buffered is due, so this picks exactly what it always did.
  private jumpToKeyframe(): boolean {
    const now = this.now();
    let best: Entry | null = null;
    for (const e of this.buffer.values()) {
      if (!e.keyframe) continue;
      if (now < this.releasableAt(e)) continue; // paced (Q3): not due yet
      if (best === null || e.receivedAtMs > best.receivedAtMs) best = e;
    }
    if (!best) return false;

    for (const [id, e] of this.buffer) {
      const behindBest = id !== best.frameId && !frameIdAhead(id, best.frameId);
      if (behindBest || (id === best.frameId && !e.keyframe)) {
        this.buffer.delete(id);
        if (!e.keyframe) this.stats.deltasDropped++;
      }
    }
    this.waitingForKeyframe = false;
    this.release(best);
    return true;
  }

  // When a frame becomes releasable (R5 Q3). Live-edge (offset 0, the
  // default) means "now" — the smoothed schedule is
  // timestampMs + arrivalBaseline + offset, i.e. a constant offset from the
  // source clock anchored at the best-observed arrival delta. In adaptive
  // mode (R12 T2) release happens DECODE_LEAD_MS early: the presentation
  // sink holds the decoded frame for its actual display slot.
  private releasableAt(e: Entry): number {
    const offset = this.playoutOffsetMs();
    if (offset <= 0) return 0;
    const base = this.arrivalBaseline.min(this.now());
    if (base === null) return 0;
    return Number(e.timestampUs) / 1000 + base + offset - this.decodeLeadMs();
  }

  // The pacing anchor (windowed min of arrival − timestamp), exposed so the
  // pipeline computes each decoded frame's display target from the same
  // baseline the release gate uses (R12 T2). Null before any frame.
  arrivalBaselineMs(): number | null {
    return this.arrivalBaseline.min(this.now());
  }

  private hasBufferedKeyframeAbove(position: number): boolean {
    for (const e of this.buffer.values()) {
      if (e.keyframe && frameIdAhead(e.frameId, position)) return true;
    }
    return false;
  }

  private shouldDeclareGap(): boolean {
    // Declare a gap once the oldest frame waiting above the missing one has sat
    // in the buffer longer than the grace — the straggler isn't coming.
    let oldest = Infinity;
    for (const e of this.buffer.values()) {
      if (e.receivedAtMs < oldest) oldest = e.receivedAtMs;
    }
    if (oldest === Infinity) return false; // nothing buffered; just wait
    return this.now() - oldest >= deltaGapGraceMs();
  }

  private dropStaleWhileWaiting(): void {
    const cutoff = this.now() - keyframeWaitMs();
    for (const [id, e] of this.buffer) {
      if (e.keyframe) continue; // keyframes are always useful to resync on
      if (e.receivedAtMs < cutoff) {
        this.buffer.delete(id);
        this.stats.keyframeWaitDrops++;
      }
    }
  }

  private release(entry: Entry): void {
    this.buffer.delete(entry.frameId);
    this.decodePosition = entry.frameId;
    this.stats.released++;
    if (entry.keyframe) {
      this.stats.keyframesReleased++;
      // R29 FP6: a keyframe is a clean reference, so the GOP's loss budget
      // starts over. This is what makes the allowance per-GOP — a per-session
      // budget would be spent in the first minute and never help again.
      this.gopSkips = 0;
    }
    this.onFrame({
      frameId: entry.frameId,
      keyframe: entry.keyframe,
      timestampUs: entry.timestampUs,
      data: entry.data,
      config: entry.config,
    });
  }
}
