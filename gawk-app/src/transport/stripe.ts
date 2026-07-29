// R30 striped delivery — the viewer-side controller (docs/35 §5.4–§5.5).
//
// Pure decision logic, deliberately transport-free: the pipeline feeds it
// per-frame arrival accounting from the reassembler and the relay's
// capability bit, calls decide() at the stats cadence, and forwards a changed
// target to the transport's setStripe(). The module-state mode mirrors
// resilient.ts: the setting crosses the worker boundary as a command and is
// read live here.
//
// Two independent questions, two mechanisms:
//  - WHEN to stripe (auto mode): the finding-4 signature — chunks of large
//    frames (> 8 expected) dying while small frames stay clean. That shape is
//    a per-connection burst-threshold buffer; uniform loss (which striping
//    cannot help — the same fraction dies on every leg) fails the small-frame
//    cleanliness test and never engages.
//  - HOW WIDE: the measured burst size. stripeNeeded = ceil(p99 burst / 6),
//    where 6 is the per-connection target (owner decision: ~25% headroom
//    under the measured ~8 threshold) and the burst includes the parity
//    symbols that ride the same legs. Grow-only, dwelled, capped at
//    MAX_STRIPE_LEGS — and held entirely below 2, because a stripe of one
//    leg moves the burst without shortening it.

import { MAX_STRIPE_LEGS } from './wire';

export type StripeMode = 'auto' | 'on' | 'off';

// The per-connection burst target (docs/35 §4 decision 4). A constant, not a
// knob: path physics, tuned only by a new measurement.
export const STRIPE_TARGET_CHUNKS = 6;
// Frames at or below this many expected chunks measured 0.00% loss on the
// affected path (docs/34 finding 4) — the large/small discriminator.
export const STRIPE_LARGE_FRAME_CHUNKS = 8;
// Detector window and thresholds (§5.5). Loss magnitude varies session to
// session (3.8%→12.6% on one machine), so the engage floor is low and the
// SHAPE carries the decision.
export const STRIPE_DETECT_WINDOW_MS = 30_000;
export const STRIPE_ENGAGE_LOSS = 0.01;
export const STRIPE_SMALL_LOSS_CEILING = 0.001;
// The shape test's general form (finding 5): the large half must lose this
// many times more, proportionally, than the small half. 2x sits well clear of
// the ~1x uniform loss produces and well under the >5x a real burst threshold
// shows, so it separates the two without needing either half to be clean.
export const STRIPE_SHAPE_LOSS_RATIO = 2;
// Minimum evidence: engagement is a relay-state spend, so it needs a real
// sample, and the small-frame side must have enough chunks to prove
// cleanliness — without it, uniform loss and threshold loss look alike.
export const STRIPE_MIN_LARGE_CHUNKS = 500;
export const STRIPE_MIN_SMALL_CHUNKS = 200;
// Sizing window: recent burst sizes, p99 (bursts are what overflow).
export const STRIPE_SIZE_WINDOW_MS = 10_000;
export const STRIPE_MIN_SIZED_FRAMES = 30;
// Grow only after the need exceeds the current width this long (§5.4).
export const STRIPE_GROW_DWELL_MS = 5_000;
// After a leg death forced a fallback, wait before re-engaging — a flapping
// path must not burn dials at the stats cadence (§5.6).
export const STRIPE_REENGAGE_BACKOFF_MS = 5_000;
// The width a live-edge viewer starts striped at (owner decision 2026-07-29,
// docs/35 finding 6). Engagement is no longer detector-gated: on this fleet
// the per-connection burst threshold is the common case, not the exception,
// and finding 5 showed the detector could not even be *evaluated* on the
// high-bitrate streams that need striping most. Auto mode now grows from this
// floor and never returns below it — only mode 'off' releases a stripe.
//
// "Live-edge" needs no test here: reliable/DVR delivery never sets the
// capability (viewer.ts gates noteCapable on deliveryMode), so those viewers
// keep exactly today's behaviour of never dialling a leg.
export const STRIPE_START_LEGS = 4;

// --- Mode (module state, the resilient.ts pattern) --------------------------

let mode: StripeMode = 'auto';

export function getStripeMode(): StripeMode {
  return mode;
}

export function setStripeMode(m: StripeMode): void {
  mode = m;
}

// --- Detector + sizing state ------------------------------------------------

export interface StripeDetectorStats {
  // The auto gate's own inputs, so a non-engaging detector is arguable from
  // a diagnostics blob (docs/35 §7).
  largeLossPct: number | null;
  smallLossPct: number | null;
  largeChunks: number;
  smallChunks: number;
  // The frame size the large/small buckets were actually split at. Normally
  // STRIPE_LARGE_FRAME_CHUNKS; higher when the stream had no frames under that
  // line and the split fell back to the distribution's median (finding 5).
  // Without it, "the small bucket is empty" and "the small bucket is clean"
  // read identically from a diagnostics blob.
  splitAtChunks: number;
  neededNow: number;
  engaged: boolean;
  // Whether the finding-4 burst signature is present RIGHT NOW. Since
  // finding 6 this no longer gates engagement — it is the answer to "is
  // striping earning its connection cost on this path?", which is the
  // question an always-on stripe raises and the kill criteria need.
  shapeDetected: boolean;
}

// One finalized frame. Kept per-frame rather than pre-summed into large/small
// buckets because the split point is no longer a constant: it is chosen at
// DECISION time from the distribution the window actually holds (finding 5),
// which a running sum has already thrown away.
interface Sample {
  expected: number;
  arrived: number;
}

interface bucket {
  atSec: number;
  samples: Sample[];
}

export class StripeController {
  private now: () => number;
  private startLegs: number;
  private buckets: bucket[] = [];
  private capable = false;
  private parityActive = 0;
  private currentTarget = 0;
  private engaged = false;
  private backoffUntil = 0;
  private growExceedSince: number | null = null;

  // startLegs is a policy knob, not a tuning constant: production always takes
  // STRIPE_START_LEGS. It is a parameter because that default equals
  // MAX_STRIPE_LEGS today, which would leave the grow path structurally
  // untestable — a lower floor is how the sizing and dwell logic stays
  // covered, and how a narrower default could be adopted without a rewrite.
  constructor(now: () => number = () => performance.now(), startLegs = STRIPE_START_LEGS) {
    this.now = now;
    this.startLegs = Math.max(1, Math.min(MAX_STRIPE_LEGS, startLegs));
  }

  noteCapable(capable: boolean): void {
    this.capable = capable;
  }

  noteParityActive(k: number): void {
    this.parityActive = Math.max(0, k);
  }

  // The transport's report of what is actually engaged. An unexpected drop
  // to 0 while we believe we are striped is a leg-death fallback: back off
  // before re-engaging (§5.6).
  noteActive(active: number): void {
    if (active === 0 && this.engaged) {
      this.engaged = false;
      this.currentTarget = 0;
      this.growExceedSince = null;
      this.backoffUntil = this.now() + STRIPE_REENGAGE_BACKOFF_MS;
    }
  }

  // One finalized frame's arrival accounting from the reassembler: how many
  // chunks the frame-global header promised, and how many actually arrived
  // (real arrivals — parity-recovered chunks are repairs, not deliveries).
  observeFrame(expectedChunks: number, arrivedChunks: number): void {
    if (expectedChunks <= 0) return;
    const nowSec = Math.floor(this.now() / 1000);
    let b = this.buckets[this.buckets.length - 1];
    if (!b || b.atSec !== nowSec) {
      b = { atSec: nowSec, samples: [] };
      this.buckets.push(b);
      this.evict(nowSec);
    }
    b.samples.push({ expected: expectedChunks, arrived: arrivedChunks });
  }

  // The stats-cadence decision: the stripe width to request (0 = none).
  decide(): number {
    const now = this.now();
    if (getStripeMode() === 'off' || !this.capable) {
      // A live 'off' releases an engaged stripe; capability loss (reconnect
      // to an older relay) does the same.
      this.engaged = false;
      this.currentTarget = 0;
      this.growExceedSince = null;
      return 0;
    }
    const sized = this.sizedNeed(now);
    if (this.engaged) {
      // Grow-only, dwelled: a transient spike must not burn a dial set. An
      // unsized window (0) never grows — missing evidence is not a
      // measurement. Nothing here ever lowers currentTarget.
      if (sized > this.currentTarget && this.currentTarget < MAX_STRIPE_LEGS) {
        if (this.growExceedSince === null) this.growExceedSince = now;
        if (now - this.growExceedSince >= STRIPE_GROW_DWELL_MS) {
          this.currentTarget = Math.min(sized, MAX_STRIPE_LEGS);
          this.growExceedSince = null;
        }
      } else {
        this.growExceedSince = null;
      }
      return this.currentTarget;
    }
    // A leg-death fallback still backs off before re-dialling — a flapping
    // path must not burn dials at the stats cadence (§5.6). This is the one
    // thing that can put an engaged viewer back to zero, and it re-engages
    // itself the moment the backoff expires.
    if (now < this.backoffUntil) return 0;
    // Engage, unconditionally (finding 6). Not detector-gated and not gated on
    // sizing evidence: a viewer that has not measured anything yet is a viewer
    // in its first seconds, which is exactly when the burst is unabsorbed and
    // the freeze is worst. The floor is the starting width; a larger measured
    // need takes effect immediately, a smaller one never does.
    this.engaged = true;
    this.currentTarget = Math.min(MAX_STRIPE_LEGS, Math.max(this.startLegs, sized));
    this.growExceedSince = null;
    return this.currentTarget;
  }

  snapshot(): StripeDetectorStats {
    const now = this.now();
    this.evict(Math.floor(now / 1000));
    const samples = this.allSamples();
    const splitAt = this.splitPoint(samples);
    const t = this.tally(samples, splitAt);
    return {
      largeLossPct: t.largeExpected > 0 ? (100 * t.largeLost) / t.largeExpected : null,
      smallLossPct: t.smallExpected > 0 ? (100 * t.smallLost) / t.smallExpected : null,
      largeChunks: t.largeExpected,
      smallChunks: t.smallExpected,
      splitAtChunks: splitAt,
      neededNow: this.neededNow(now),
      engaged: this.engaged,
      shapeDetected: this.shapePresent(now),
    };
  }

  // The measured width, or 0 when the sizing window holds too few frames to
  // measure one. Deliberately distinct from 1: see decide().
  private sizedNeed(now: number): number {
    const sizes: number[] = [];
    const cutoffSec = Math.floor((now - STRIPE_SIZE_WINDOW_MS) / 1000);
    for (const b of this.buckets) {
      if (b.atSec >= cutoffSec) for (const s of b.samples) sizes.push(s.expected);
    }
    if (sizes.length < STRIPE_MIN_SIZED_FRAMES) return 0;
    sizes.sort((a, z) => a - z);
    const p99 = sizes[Math.min(sizes.length - 1, Math.floor(sizes.length * 0.99))];
    const burst = p99 + Math.min(this.parityActive, 2);
    return Math.max(1, Math.min(MAX_STRIPE_LEGS, Math.ceil(burst / STRIPE_TARGET_CHUNKS)));
  }

  // What the stats surface reports: 1 ("nothing to split") when unsized, the
  // value this has always shown.
  private neededNow(now: number): number {
    return this.sizedNeed(now) || 1;
  }

  private allSamples(): Sample[] {
    const out: Sample[] = [];
    for (const b of this.buckets) for (const s of b.samples) out.push(s);
    return out;
  }

  // Where to cut the large/small buckets.
  //
  // STRIPE_LARGE_FRAME_CHUNKS is the measured line (docs/34 finding 4) and
  // stays the default wherever the stream actually has frames below it. But it
  // is ABSOLUTE, and the shape test needs BOTH halves populated: a
  // high-bitrate stream clears 8 chunks on every frame, so the small bucket
  // never reaches STRIPE_MIN_SMALL_CHUNKS and the test is never evaluated at
  // all — auto striping structurally unreachable on exactly the streams whose
  // bursts overflow the queue (finding 5: 2967 of 3088 chunks large, 7-9%
  // large-frame loss, nothing to compare it against).
  //
  // So when the fixed line cannot fill the small bucket, cut the stream's own
  // distribution at its median instead. A stream whose frames are all the SAME
  // size then puts everything on one side and stays correctly unprovable —
  // no size variation, no shape to measure.
  private splitPoint(samples: Sample[]): number {
    let small = 0;
    for (const s of samples) {
      if (s.expected <= STRIPE_LARGE_FRAME_CHUNKS) small += s.expected;
    }
    if (small >= STRIPE_MIN_SMALL_CHUNKS || samples.length === 0) {
      return STRIPE_LARGE_FRAME_CHUNKS;
    }
    const sizes = samples.map((s) => s.expected).sort((a, z) => a - z);
    return Math.max(STRIPE_LARGE_FRAME_CHUNKS, sizes[Math.floor(sizes.length / 2)]);
  }

  private tally(
    samples: Sample[],
    splitAt: number,
  ): { largeExpected: number; largeLost: number; smallExpected: number; smallLost: number } {
    let largeExpected = 0;
    let largeLost = 0;
    let smallExpected = 0;
    let smallLost = 0;
    for (const s of samples) {
      const lost = Math.max(0, s.expected - s.arrived);
      if (s.expected > splitAt) {
        largeExpected += s.expected;
        largeLost += lost;
      } else {
        smallExpected += s.expected;
        smallLost += lost;
      }
    }
    return { largeExpected, largeLost, smallExpected, smallLost };
  }

  // The finding-4 burst signature, as an OBSERVATION. Until finding 6 this
  // gated engagement; it now answers "is the burst threshold actually present
  // on this path?", which is what tells an operator whether an always-on
  // stripe is earning its connection cost — and is the measurement R30's kill
  // criteria are written against. Kept intact rather than deleted so the
  // gating could be restored without re-deriving any of it.
  private shapePresent(now: number): boolean {
    this.evict(Math.floor(now / 1000));
    const samples = this.allSamples();
    const t = this.tally(samples, this.splitPoint(samples));
    if (t.largeExpected < STRIPE_MIN_LARGE_CHUNKS) return false;
    if (t.smallExpected < STRIPE_MIN_SMALL_CHUNKS) return false;
    const large = t.largeLost / t.largeExpected;
    const small = t.smallLost / t.smallExpected;
    if (large < STRIPE_ENGAGE_LOSS) return false;
    // The shape test: what separates a per-connection burst threshold from
    // uniform loss, which striping cannot help.
    //
    // Small frames CLEAN is the original, strongest form and still passes on
    // its own. Where the split had to adapt, "clean" is not on offer — a
    // median-sized frame on a burst-limited path loses too — so the general
    // form is the thing cleanliness was always a proxy for: loss that RISES
    // with burst size. Uniform loss leaves the two halves proportionally equal
    // and fails both forms.
    if (small <= STRIPE_SMALL_LOSS_CEILING) return true;
    return large >= STRIPE_SHAPE_LOSS_RATIO * small;
  }

  private evict(nowSec: number): void {
    const cutoff = nowSec - Math.ceil(STRIPE_DETECT_WINDOW_MS / 1000);
    while (this.buckets.length > 0 && this.buckets[0].atSec < cutoff) {
      this.buckets.shift();
    }
  }
}
