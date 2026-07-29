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
  neededNow: number;
  engaged: boolean;
}

interface bucket {
  atSec: number;
  largeExpected: number;
  largeLost: number;
  smallExpected: number;
  smallLost: number;
  frames: number;
  burstSum: number[]; // burst sizes observed this second
}

export class StripeController {
  private now: () => number;
  private buckets: bucket[] = [];
  private capable = false;
  private parityActive = 0;
  private currentTarget = 0;
  private engaged = false;
  private backoffUntil = 0;
  private growExceedSince: number | null = null;

  constructor(now: () => number = () => performance.now()) {
    this.now = now;
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
      b = { atSec: nowSec, largeExpected: 0, largeLost: 0, smallExpected: 0, smallLost: 0, frames: 0, burstSum: [] };
      this.buckets.push(b);
      this.evict(nowSec);
    }
    const lost = Math.max(0, expectedChunks - arrivedChunks);
    if (expectedChunks > STRIPE_LARGE_FRAME_CHUNKS) {
      b.largeExpected += expectedChunks;
      b.largeLost += lost;
    } else {
      b.smallExpected += expectedChunks;
      b.smallLost += lost;
    }
    b.frames++;
    b.burstSum.push(expectedChunks);
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
    const needed = this.neededNow(now);
    if (this.engaged) {
      // Grow-only, dwelled: a transient spike must not burn a dial set.
      if (needed > this.currentTarget && this.currentTarget < MAX_STRIPE_LEGS) {
        if (this.growExceedSince === null) this.growExceedSince = now;
        if (now - this.growExceedSince >= STRIPE_GROW_DWELL_MS) {
          this.currentTarget = Math.min(needed, MAX_STRIPE_LEGS);
          this.growExceedSince = null;
        }
      } else {
        this.growExceedSince = null;
      }
      return this.currentTarget;
    }
    if (now < this.backoffUntil) return 0;
    // A stripe below 2 legs moves the burst without shortening it (§5.4).
    if (needed < 2) return 0;
    if (getStripeMode() === 'on' || this.detectorFires(now)) {
      this.engaged = true;
      this.currentTarget = needed;
      return needed;
    }
    return 0;
  }

  snapshot(): StripeDetectorStats {
    const now = this.now();
    this.evict(Math.floor(now / 1000));
    const { largeExpected, largeLost, smallExpected, smallLost } = this.sums();
    return {
      largeLossPct: largeExpected > 0 ? (100 * largeLost) / largeExpected : null,
      smallLossPct: smallExpected > 0 ? (100 * smallLost) / smallExpected : null,
      largeChunks: largeExpected,
      smallChunks: smallExpected,
      neededNow: this.neededNow(now),
      engaged: this.engaged,
    };
  }

  // clamp(ceil((p99 burst + parity)/target), 1, max) once enough frames are
  // sized; 1 (i.e. "nothing to split") before that.
  private neededNow(now: number): number {
    const sizes: number[] = [];
    const cutoffSec = Math.floor((now - STRIPE_SIZE_WINDOW_MS) / 1000);
    for (const b of this.buckets) {
      if (b.atSec >= cutoffSec) sizes.push(...b.burstSum);
    }
    if (sizes.length < STRIPE_MIN_SIZED_FRAMES) return 1;
    sizes.sort((a, z) => a - z);
    const p99 = sizes[Math.min(sizes.length - 1, Math.floor(sizes.length * 0.99))];
    const burst = p99 + Math.min(this.parityActive, 2);
    return Math.max(1, Math.min(MAX_STRIPE_LEGS, Math.ceil(burst / STRIPE_TARGET_CHUNKS)));
  }

  private detectorFires(now: number): boolean {
    this.evict(Math.floor(now / 1000));
    const { largeExpected, largeLost, smallExpected, smallLost } = this.sums();
    if (largeExpected < STRIPE_MIN_LARGE_CHUNKS) return false;
    if (smallExpected < STRIPE_MIN_SMALL_CHUNKS) return false;
    if (largeLost / largeExpected < STRIPE_ENGAGE_LOSS) return false;
    // The shape test: small frames clean is what separates the threshold
    // buffer from uniform loss, which striping cannot help.
    return smallLost / smallExpected <= STRIPE_SMALL_LOSS_CEILING;
  }

  private sums(): { largeExpected: number; largeLost: number; smallExpected: number; smallLost: number } {
    let largeExpected = 0;
    let largeLost = 0;
    let smallExpected = 0;
    let smallLost = 0;
    for (const b of this.buckets) {
      largeExpected += b.largeExpected;
      largeLost += b.largeLost;
      smallExpected += b.smallExpected;
      smallLost += b.smallLost;
    }
    return { largeExpected, largeLost, smallExpected, smallLost };
  }

  private evict(nowSec: number): void {
    const cutoff = nowSec - Math.ceil(STRIPE_DETECT_WINDOW_MS / 1000);
    while (this.buckets.length > 0 && this.buckets[0].atSec < cutoff) {
      this.buckets.shift();
    }
  }
}
