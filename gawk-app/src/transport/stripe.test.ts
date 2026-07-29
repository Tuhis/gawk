// R30 ST5 (docs/35 §5.4–§5.5): the stripe controller. The detector must fire
// on the finding-4 signature and ONLY on it; sizing must key on burst length,
// never on a loss rate; engagement is sticky, growth dwelled, fallback backed
// off.

import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import {
  STRIPE_DETECT_WINDOW_MS,
  STRIPE_GROW_DWELL_MS,
  STRIPE_REENGAGE_BACKOFF_MS,
  StripeController,
  getStripeMode,
  setStripeMode,
} from './stripe';
import { MAX_STRIPE_LEGS } from './wire';

let nowMs = 100_000;
const now = () => nowMs;

function controller(): StripeController {
  const c = new StripeController(now);
  c.noteCapable(true);
  return c;
}

// Feed `seconds` of a synthetic stream: 30 fps, alternating large (18-chunk)
// and small (4-chunk) frames, with the given per-chunk loss applied to large
// and small frames respectively (loss expressed as lost chunks per frame).
function feed(
  c: StripeController,
  seconds: number,
  opts: { largeChunks?: number; largeLostPerFrame?: number; smallLostPerFrame?: number } = {},
): void {
  const largeChunks = opts.largeChunks ?? 18;
  for (let s = 0; s < seconds; s++) {
    for (let f = 0; f < 15; f++) {
      c.observeFrame(largeChunks, largeChunks - (opts.largeLostPerFrame ?? 0));
      c.observeFrame(4, 4 - (opts.smallLostPerFrame ?? 0));
    }
    nowMs += 1000;
  }
}

beforeEach(() => {
  nowMs = 100_000;
  setStripeMode('auto');
});

afterEach(() => {
  setStripeMode('auto');
});

describe('stripe mode module state', () => {
  it('defaults to auto and round-trips', () => {
    expect(getStripeMode()).toBe('auto');
    setStripeMode('on');
    expect(getStripeMode()).toBe('on');
    setStripeMode('off');
    expect(getStripeMode()).toBe('off');
  });
});

describe('StripeController — auto detector (finding-4 signature)', () => {
  it('fires on threshold-shaped loss: large frames lossy, small frames clean', () => {
    const c = controller();
    // ~5.5% large-frame chunk loss (1 of 18), zero small-frame loss.
    feed(c, 10, { largeLostPerFrame: 1 });
    expect(c.decide()).toBeGreaterThanOrEqual(3); // 18+2 parity? no parity noted: ceil(18/6)=3
    expect(c.snapshot().engaged).toBe(true);
  });

  it('does NOT fire on uniform loss (small frames lossy too — striping cannot help)', () => {
    const c = controller();
    // ~5.5% loss on large frames AND ~25% on small — uniform-loss shape.
    feed(c, 10, { largeLostPerFrame: 1, smallLostPerFrame: 1 });
    expect(c.decide()).toBe(0);
    expect(c.snapshot().engaged).toBe(false);
  });

  it('does NOT fire on a clean link', () => {
    const c = controller();
    feed(c, 10);
    expect(c.decide()).toBe(0);
  });

  it('does NOT fire below the evidence floor', () => {
    const c = controller();
    // One second of traffic: lossy shape, but nowhere near 500 large chunks…
    // actually 15 frames × 18 = 270 < 500.
    feed(c, 1, { largeLostPerFrame: 2 });
    expect(c.decide()).toBe(0);
  });

  it('does NOT fire without small-frame evidence (shape unprovable)', () => {
    const c = controller();
    for (let s = 0; s < 10; s++) {
      for (let f = 0; f < 30; f++) c.observeFrame(18, 17); // large only
      nowMs += 1000;
    }
    expect(c.decide()).toBe(0);
  });

  it('forgets loss outside the detector window', () => {
    const c = controller();
    feed(c, 10, { largeLostPerFrame: 1 });
    expect(c.decide()).toBeGreaterThanOrEqual(2);
    // Fresh controller state via fallback, then a long clean stretch.
    c.noteActive(2);
    c.noteActive(0); // fallback clears engagement
    nowMs += STRIPE_REENGAGE_BACKOFF_MS + STRIPE_DETECT_WINDOW_MS + 1000;
    feed(c, 31); // clean, and long enough to evict every lossy bucket
    expect(c.decide()).toBe(0);
  });
});

describe('StripeController — manual on', () => {
  it('engages from size alone once enough frames are seen', () => {
    setStripeMode('on');
    const c = controller();
    feed(c, 3);
    expect(c.decide()).toBe(3); // ceil(18/6)
  });

  it('holds while frames fit one share (nothing to split)', () => {
    setStripeMode('on');
    const c = controller();
    feed(c, 3, { largeChunks: 9 }); // p99 = 9 → ceil(9/6) = 2… so use 6-chunk frames
    const small = new StripeController(now);
    small.noteCapable(true);
    for (let s = 0; s < 3; s++) {
      for (let f = 0; f < 30; f++) small.observeFrame(5, 5);
      nowMs += 1000;
    }
    expect(small.decide()).toBe(0);
  });

  it('never engages without the capability bit', () => {
    setStripeMode('on');
    const c = new StripeController(now);
    c.noteCapable(false);
    feed(c, 5);
    expect(c.decide()).toBe(0);
  });

  it('never engages in off mode, and a live off releases an engaged stripe', () => {
    setStripeMode('on');
    const c = controller();
    feed(c, 3);
    expect(c.decide()).toBe(3);
    setStripeMode('off');
    expect(c.decide()).toBe(0);
  });
});

describe('StripeController — sizing and growth', () => {
  it('sizes from the p99 burst plus the active parity level', () => {
    setStripeMode('on');
    const c = controller();
    c.noteParityActive(2);
    // 16-chunk frames + 2 parity = 18 → ceil(18/6) = 3.
    feed(c, 3, { largeChunks: 16 });
    expect(c.decide()).toBe(3);
  });

  it('grows only after the dwell, and never shrinks in-session', () => {
    setStripeMode('on');
    const c = controller();
    feed(c, 3, { largeChunks: 12 }); // ceil(12/6) = 2
    expect(c.decide()).toBe(2);
    c.noteActive(2);
    // Frames grow to 20 chunks → needed 4; the first decides stay at 2…
    feed(c, 2, { largeChunks: 20 });
    expect(c.decide()).toBe(2);
    // …until the dwell elapses with the need sustained.
    nowMs += STRIPE_GROW_DWELL_MS;
    feed(c, 1, { largeChunks: 20 });
    expect(c.decide()).toBe(4);
    // Frames shrink again: the width stays (grow-only; reconnect resets).
    feed(c, 11, { largeChunks: 6 });
    expect(c.decide()).toBe(4);
  });

  it('caps at MAX_STRIPE_LEGS however large the frames', () => {
    setStripeMode('on');
    const c = controller();
    feed(c, 3, { largeChunks: 60 });
    expect(c.decide()).toBe(MAX_STRIPE_LEGS);
  });
});

describe('StripeController — fallback backoff', () => {
  it('backs off after a leg-death fallback, then re-engages', () => {
    setStripeMode('on');
    const c = controller();
    feed(c, 3);
    expect(c.decide()).toBe(3);
    c.noteActive(3);
    c.noteActive(0); // leg death: transport fell back
    expect(c.decide()).toBe(0); // inside the backoff
    nowMs += STRIPE_REENGAGE_BACKOFF_MS + 1;
    feed(c, 1);
    expect(c.decide()).toBe(3); // re-engaged after the backoff
  });
});
