// Striped deep-buffer delivery, the viewer's decision half (R87, docs/91).
// Pure and transport-free, like stripe.ts: the pipeline feeds it complete
// frames and the reorder buffer's hole skips, calls decide() at the stats
// cadence, and forwards a changed width to the transport's setStripe().
//
// Width counts connections, the primary included: the primary carries GOP
// share 0 and legs j = 1 … width−1 carry the rest (D6). Width 1 is unstriped.
// Live-edge striping (stripe.ts) is a different mechanism with a different
// unit and is untouched by anything here.

import { CAP_STRIPED_DVR } from './parity';
import type { StripeMode } from './stripe';
import { MAX_STRIPE_LEGS, nextFrameId } from './wire';

// D12: MaxStripeLegs bounds the connections one viewer opens, so the width,
// which counts the primary, tops out at it.
export const DVR_STRIPE_MAX_WIDTH = MAX_STRIPE_LEGS;
// D10: Auto's first step, and the width On engages at join.
export const DVR_STRIPE_START_WIDTH = 2;
// D10: undersupply is complete-frame goodput below this share of the stream's
// own live rate, or a relay resync, held for DVR_STRIPE_ENGAGE_MS.
export const DVR_STRIPE_UNDERSUPPLY_RATIO = 0.9;
export const DVR_STRIPE_ENGAGE_MS = 10_000;
// A resync keeps the undersupply condition true for this long after it.
export const DVR_STRIPE_RESYNC_WINDOW_MS = 10_000;
// D10: a step helped when it raised aggregate goodput by at least 20 %.
export const DVR_STRIPE_HELP_RATIO = 1.2;
// After a step's legs are up: time for the new connections to ramp before
// goodput is measured, then the window it is measured over.
export const DVR_STRIPE_SETTLE_MS = 3_000;
export const DVR_STRIPE_EVAL_MS = 10_000;
// A step whose legs never come up (caps pressure, dial errors) did not help.
export const DVR_STRIPE_DIAL_TIMEOUT_MS = 10_000;
// After a leg death forced a fallback, as stripe.ts: a flapping path must not
// burn dials at the stats cadence.
export const DVR_STRIPE_REENGAGE_BACKOFF_MS = 5_000;
// Goodput is measured over a short window so a step's effect shows quickly;
// the live rate over a long one so it spans several GOPs.
export const DVR_GOODPUT_WINDOW_MS = 2_000;
export const DVR_LIVE_RATE_WINDOW_MS = 10_000;

// D8, D10, OD4: a deep-buffer session may stripe only against a relay that
// advertises CAP_STRIPED_DVR and actually granted the ring, and only when the
// setting is not Off. Plain reliable (Smoother) never stripes: it is not deep.
export function dvrStripeAllowed(g: {
  deep: boolean;
  dvrGranted: boolean;
  capFlags: number;
  mode: StripeMode;
}): boolean {
  return g.deep && g.dvrGranted && (g.capFlags & CAP_STRIPED_DVR) !== 0 && g.mode !== 'off';
}

// --- Goodput and the stream's live rate ---------------------------------------

interface FrameSample {
  frameId: number;
  timestampUs: number;
  bytes: number;
  keyframe: boolean;
  atMs: number;
}

// Measures what the connections together deliver against what the stream
// needs. Both come from complete frames, deduplicated by frameId, so a GOP
// served twice during a transition (D11) counts once.
//
// Goodput is complete-frame bytes per second of wall time. The live rate is
// bytes per second of MEDIA time, taken only over runs of consecutive frames
// from one keyframe to a later one: a span with a skipped GOP in it would
// count the skipped GOP's time but not its bytes, and read an undersupplied
// stream's rate as low as its goodput — which would hide exactly the
// undersupply this exists to see.
export class DvrGoodputMeter {
  private now: () => number;
  private frames = new Map<number, FrameSample>(); // insertion = arrival order
  private firstAtMs: number | null = null;
  private lastLiveRateBps: number | null = null;

  constructor(now: () => number = () => performance.now()) {
    this.now = now;
  }

  observeFrame(frameId: number, timestampUs: bigint, bytes: number, keyframe: boolean): void {
    if (this.frames.has(frameId)) return;
    const atMs = this.now();
    this.firstAtMs ??= atMs;
    this.frames.set(frameId, { frameId, timestampUs: Number(timestampUs), bytes, keyframe, atMs });
    this.evict(atMs);
  }

  // Broadcaster restart: a new timeline, so nothing measured on the old one
  // describes it.
  reset(): void {
    this.frames.clear();
    this.firstAtMs = null;
    this.lastLiveRateBps = null;
  }

  goodputBps(): number | null {
    const now = this.now();
    this.evict(now);
    if (this.firstAtMs === null) return null;
    const windowMs = Math.min(DVR_GOODPUT_WINDOW_MS, now - this.firstAtMs);
    if (windowMs <= 0) return null;
    let bytes = 0;
    for (const f of this.frames.values()) if (f.atMs > now - DVR_GOODPUT_WINDOW_MS) bytes += f.bytes;
    return (bytes * 8 * 1000) / windowMs;
  }

  // The last measurable live rate: a window with no keyframe-to-keyframe run
  // keeps the previous estimate rather than reporting none.
  liveRateBps(): number | null {
    this.evict(this.now());
    const sorted = [...this.frames.values()].sort((a, b) => a.timestampUs - b.timestampUs);
    let bytes = 0;
    let spanUs = 0;
    let runStart = 0;
    for (let i = 1; i <= sorted.length; i++) {
      const contiguous = i < sorted.length && sorted[i].frameId === nextFrameId(sorted[i - 1].frameId);
      if (contiguous) continue;
      const r = keyframeSpan(sorted, runStart, i);
      bytes += r.bytes;
      spanUs += r.spanUs;
      runStart = i;
    }
    if (spanUs > 0) this.lastLiveRateBps = (bytes * 8 * 1e6) / spanUs;
    return this.lastLiveRateBps;
  }

  private evict(now: number): void {
    const cutoff = now - DVR_LIVE_RATE_WINDOW_MS;
    for (const [id, f] of this.frames) {
      if (f.atMs >= cutoff) break;
      this.frames.delete(id);
    }
  }
}

// Within one contiguous run [from, to): the frames from its first keyframe up
// to (not including) its last, and the media time between those two keyframes.
function keyframeSpan(run: FrameSample[], from: number, to: number): { bytes: number; spanUs: number } {
  let first = -1;
  let last = -1;
  for (let i = from; i < to; i++) {
    if (!run[i].keyframe) continue;
    if (first < 0) first = i;
    last = i;
  }
  if (first < 0 || last <= first) return { bytes: 0, spanUs: 0 };
  let bytes = 0;
  for (let i = first; i < last; i++) bytes += run[i].bytes;
  return { bytes, spanUs: Math.max(0, run[last].timestampUs - run[first].timestampUs) };
}

// --- The engagement policy (D10) ----------------------------------------------

export interface DvrStripeInput {
  // dvrStripeAllowed() without the mode, which is passed on its own so Off
  // can be told apart from "not possible here".
  allowed: boolean;
  mode: StripeMode;
  goodputBps: number | null;
  liveRateBps: number | null;
  // Cumulative missing-frame skips to a newer keyframe (ReorderStats
  // holeSkips): the client's view of a relay resync.
  resyncs: number;
}

export type DvrStripePhase = 'idle' | 'stepping' | 'engaged' | 'held';

export interface DvrStripeSnapshot {
  target: number;
  phase: DvrStripePhase;
  undersupplied: boolean;
}

interface GoodputSample {
  atMs: number;
  bps: number;
}

// Auto engages ×2 once undersupply has held for DVR_STRIPE_ENGAGE_MS. Each
// step is measured: goodput over DVR_STRIPE_EVAL_MS after its legs settle,
// against goodput over the DVR_STRIPE_ENGAGE_MS before it. A step that raised
// it by DVR_STRIPE_HELP_RATIO stays, and if the stream is still undersupplied
// the next step follows, up to DVR_STRIPE_MAX_WIDTH; a step that did not
// returns to the previous width and holds it for the session — on a full
// link extra connections take nothing more, so Auto ends at ×1 or ×2 (G4).
// On engages ×2 at join without evidence and grows by the same rule; it never
// returns below ×2. Off never stripes.
export class DvrStripePolicy {
  private now: () => number;
  private target = 1;
  private previous = 1;
  private phase: DvrStripePhase = 'idle';
  private stepAt = 0;
  private activeAt: number | null = null;
  private baselineBps = 0;
  private active = 1;
  private backoffUntil = 0;
  private undersuppliedSince: number | null = null;
  private undersupplied = false;
  private lastResyncs = 0;
  private lastResyncAt: number | null = null;
  private liveRateBps: number | null = null;
  private samples: GoodputSample[] = [];

  constructor(now: () => number = () => performance.now()) {
    this.now = now;
  }

  // The width the transport actually has up (1 = unstriped). A drop to 1 while
  // striped is a leg-death fallback; anything else just times the step.
  noteActive(width: number): void {
    const now = this.now();
    if (width <= 1 && this.active > 1 && this.target > 1) {
      this.backoffUntil = now + DVR_STRIPE_REENGAGE_BACKOFF_MS;
      if (this.phase === 'stepping') {
        this.activeAt = null;
        this.stepAt = this.backoffUntil;
      }
    }
    this.active = width;
    if (this.phase === 'stepping' && this.activeAt === null && width >= this.target) {
      this.activeAt = now;
    }
  }

  decide(input: DvrStripeInput): number {
    const now = this.now();
    this.observe(now, input);
    if (!input.allowed || input.mode === 'off') {
      // A held width survives an Off flip: the session already learned that
      // more connections don't help here.
      if (this.phase !== 'held') {
        this.phase = 'idle';
        this.target = 1;
        this.previous = 1;
      }
      return 1;
    }
    if (now < this.backoffUntil) return 1;

    switch (this.phase) {
      case 'idle':
        if (input.mode === 'on') {
          this.target = DVR_STRIPE_START_WIDTH;
          this.phase = 'engaged';
        } else if (this.sustainedUndersupply(now)) {
          this.step(now, DVR_STRIPE_START_WIDTH);
        }
        break;
      case 'stepping':
        this.evaluate(now);
        break;
      case 'engaged':
        if (input.mode === 'on' && this.target < DVR_STRIPE_START_WIDTH) {
          this.target = DVR_STRIPE_START_WIDTH;
        }
        if (this.target < DVR_STRIPE_MAX_WIDTH && this.sustainedUndersupply(now)) {
          this.step(now, this.target + 1);
        }
        break;
      case 'held':
        // On's floor holds even over a session that learned ×1 under Auto.
        if (input.mode === 'on' && this.target < DVR_STRIPE_START_WIDTH) {
          this.target = DVR_STRIPE_START_WIDTH;
        }
        break;
    }
    return this.target;
  }

  snapshot(): DvrStripeSnapshot {
    return { target: this.target, phase: this.phase, undersupplied: this.undersupplied };
  }

  private observe(now: number, input: DvrStripeInput): void {
    if (input.liveRateBps !== null) this.liveRateBps = input.liveRateBps;
    if (input.goodputBps !== null) this.samples.push({ atMs: now, bps: input.goodputBps });
    const keepMs = DVR_STRIPE_ENGAGE_MS + DVR_STRIPE_SETTLE_MS + DVR_STRIPE_EVAL_MS + DVR_STRIPE_DIAL_TIMEOUT_MS;
    while (this.samples.length > 0 && this.samples[0].atMs < now - keepMs) this.samples.shift();
    if (input.resyncs > this.lastResyncs) this.lastResyncAt = now;
    this.lastResyncs = input.resyncs;

    const starved =
      this.liveRateBps !== null &&
      input.goodputBps !== null &&
      input.goodputBps < DVR_STRIPE_UNDERSUPPLY_RATIO * this.liveRateBps;
    const resynced = this.lastResyncAt !== null && now - this.lastResyncAt < DVR_STRIPE_RESYNC_WINDOW_MS;
    this.undersupplied = starved || resynced;
    if (!this.undersupplied) this.undersuppliedSince = null;
    else this.undersuppliedSince ??= now;
  }

  private sustainedUndersupply(now: number): boolean {
    return this.undersuppliedSince !== null && now - this.undersuppliedSince >= DVR_STRIPE_ENGAGE_MS;
  }

  private step(now: number, width: number): void {
    this.baselineBps = this.meanGoodput(now - DVR_STRIPE_ENGAGE_MS, now);
    this.previous = this.target;
    this.target = Math.min(DVR_STRIPE_MAX_WIDTH, width);
    this.phase = 'stepping';
    this.stepAt = now;
    this.activeAt = this.active >= this.target ? now : null;
    this.undersuppliedSince = null;
  }

  private evaluate(now: number): void {
    if (this.activeAt === null) {
      if (now - this.stepAt >= DVR_STRIPE_DIAL_TIMEOUT_MS) this.stepBack();
      return;
    }
    const from = this.activeAt + DVR_STRIPE_SETTLE_MS;
    if (now < from + DVR_STRIPE_EVAL_MS) return;
    const after = this.meanGoodput(from, now);
    const helped = after >= DVR_STRIPE_HELP_RATIO * this.baselineBps && after > 0;
    if (!helped) {
      this.stepBack();
      return;
    }
    this.phase = 'engaged';
    // Still short of the stream after a step that helped: the next one is
    // already justified by the evaluation window itself.
    const stillShort =
      this.liveRateBps !== null && after < DVR_STRIPE_UNDERSUPPLY_RATIO * this.liveRateBps;
    if (stillShort && this.target < DVR_STRIPE_MAX_WIDTH) {
      this.step(now, this.target + 1);
      this.baselineBps = after;
    }
  }

  private stepBack(): void {
    this.target = this.previous;
    this.phase = 'held';
    this.activeAt = null;
  }

  private meanGoodput(fromMs: number, toMs: number): number {
    let sum = 0;
    let n = 0;
    for (const s of this.samples) {
      if (s.atMs < fromMs || s.atMs > toMs) continue;
      sum += s.bps;
      n++;
    }
    return n > 0 ? sum / n : 0;
  }
}
