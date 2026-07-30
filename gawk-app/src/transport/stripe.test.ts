// R30 ST5 (docs/35 §5.4–§5.5), as revised by finding 6.
//
// Engagement is no longer detector-gated: a live-edge viewer starts striped at
// STRIPE_START_LEGS and only ever grows, and ONLY mode 'off' releases a
// stripe. The finding-4 burst signature survives as an OBSERVATION
// (`snapshot().shapeDetected`) — the answer to "is striping earning its
// connection cost here", which is what the kill criteria are written against —
// and this file pins both halves separately: the signature's logic, and the
// engagement policy that no longer consults it.
//
// Sizing must still key on burst length, never on a loss rate; growth stays
// dwelled; a leg-death fallback still backs off before re-dialling.

import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import {
  STRIPE_DETECT_WINDOW_MS,
  STRIPE_GROW_DWELL_MS,
  STRIPE_REENGAGE_BACKOFF_MS,
  STRIPE_START_LEGS,
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

// STRIPE_START_LEGS equals MAX_STRIPE_LEGS today, so the grow path is only
// reachable from a lower floor. Production always uses the default.
function growable(startLegs = 2): StripeController {
  const c = new StripeController(now, startLegs);
  c.noteCapable(true);
  return c;
}

// Feed `seconds` of a synthetic stream: 30 fps, alternating large (18-chunk)
// and small (4-chunk) frames, with the given per-frame lost-chunk counts.
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

describe('burst signature (observation, not a gate)', () => {
  it('reports the shape when large frames are lossy and small frames are clean', () => {
    const c = controller();
    // ~5.5% large-frame chunk loss (1 of 18), zero small-frame loss.
    feed(c, 10, { largeLostPerFrame: 1 });
    expect(c.snapshot().shapeDetected).toBe(true);
  });

  it('does not report it on uniform loss — striping cannot help that', () => {
    const c = controller();
    feed(c, 10, { largeLostPerFrame: 1, smallLostPerFrame: 1 });
    expect(c.snapshot().shapeDetected).toBe(false);
  });

  it('does not report it on a clean link', () => {
    const c = controller();
    feed(c, 10);
    expect(c.snapshot().shapeDetected).toBe(false);
  });

  it('does not report it below the evidence floor', () => {
    const c = controller();
    feed(c, 1, { largeLostPerFrame: 2 }); // 15 × 18 = 270 large chunks < 500
    expect(c.snapshot().shapeDetected).toBe(false);
  });

  it('does not report it when every frame is the same size — no shape to measure', () => {
    const c = controller();
    for (let s = 0; s < 10; s++) {
      for (let f = 0; f < 30; f++) c.observeFrame(18, 17);
      nowMs += 1000;
    }
    expect(c.snapshot().shapeDetected).toBe(false);
  });

  it('forgets loss outside the detector window', () => {
    const c = controller();
    feed(c, 10, { largeLostPerFrame: 1 });
    expect(c.snapshot().shapeDetected).toBe(true);
    nowMs += STRIPE_DETECT_WINDOW_MS + 1000;
    feed(c, 31); // clean, long enough to evict every lossy bucket
    expect(c.snapshot().shapeDetected).toBe(false);
  });
});

describe('engagement policy (finding 6)', () => {
  it('starts striped at the floor with no evidence at all', () => {
    const c = controller();
    expect(c.decide()).toBe(STRIPE_START_LEGS);
    expect(c.snapshot().engaged).toBe(true);
  });

  it('starts striped on a perfectly clean link — the shape does not gate it', () => {
    const c = controller();
    feed(c, 10);
    expect(c.snapshot().shapeDetected).toBe(false);
    expect(c.decide()).toBe(STRIPE_START_LEGS);
  });

  it('never engages without the relay capability bit (covers reliable/DVR too)', () => {
    // viewer.ts withholds the capability for reliable/DVR delivery, so this is
    // the same code path as "not in live-edge mode".
    const c = new StripeController(now);
    c.noteCapable(false);
    feed(c, 5, { largeLostPerFrame: 1 });
    expect(c.decide()).toBe(0);
    expect(c.snapshot().engaged).toBe(false);
  });

  it('never engages in off mode, and a live off releases an engaged stripe', () => {
    const c = controller();
    expect(c.decide()).toBe(STRIPE_START_LEGS);
    setStripeMode('off');
    expect(c.decide()).toBe(0);
    expect(c.snapshot().engaged).toBe(false);
  });

  it('re-engages after an off→auto flip: only the user can keep it off', () => {
    const c = controller();
    setStripeMode('off');
    expect(c.decide()).toBe(0);
    setStripeMode('auto');
    expect(c.decide()).toBe(STRIPE_START_LEGS);
  });

  it('never drops below the floor, however clean the link becomes', () => {
    const c = controller();
    expect(c.decide()).toBe(STRIPE_START_LEGS);
    feed(c, 30, { largeChunks: 3 }); // tiny frames: sized need is 1
    expect(c.snapshot().neededNow).toBe(1);
    expect(c.decide()).toBe(STRIPE_START_LEGS);
  });
});

describe('sizing and growth above the floor', () => {
  it('sizes from the p99 burst plus the active parity level', () => {
    const c = growable(1);
    c.noteParityActive(2);
    // 16-chunk frames + 2 parity = 18 → ceil(18/6) = 3.
    feed(c, 3, { largeChunks: 16 });
    expect(c.decide()).toBe(3);
  });

  it('grows only after the dwell, and never shrinks in-session', () => {
    const c = growable(2);
    feed(c, 3, { largeChunks: 12 }); // ceil(12/6) = 2 — at the floor
    expect(c.decide()).toBe(2);
    // Frames grow to 20 chunks → needed 4; the first decides stay at 2…
    feed(c, 2, { largeChunks: 20 });
    expect(c.decide()).toBe(2);
    // …until the dwell elapses with the need sustained.
    nowMs += STRIPE_GROW_DWELL_MS;
    feed(c, 1, { largeChunks: 20 });
    expect(c.decide()).toBe(4);
    // Frames shrink again: the width stays (grow-only).
    feed(c, 11, { largeChunks: 6 });
    expect(c.decide()).toBe(4);
  });

  it('takes a larger measured need immediately at first engagement', () => {
    const c = growable(2);
    feed(c, 3, { largeChunks: 20 }); // ceil(20/6) = 4 > floor 2
    expect(c.decide()).toBe(4);
  });

  it('caps at MAX_STRIPE_LEGS however large the frames', () => {
    const c = growable(1);
    feed(c, 3, { largeChunks: 60 });
    expect(c.decide()).toBe(MAX_STRIPE_LEGS);
  });

  it('does not grow on an unsized window — missing evidence is not a measurement', () => {
    const c = growable(2);
    expect(c.decide()).toBe(2);
    nowMs += STRIPE_GROW_DWELL_MS + 1000; // time passes, no frames observed
    expect(c.decide()).toBe(2);
  });
});

describe('leg-death fallback', () => {
  it('backs off after a fallback, then re-engages at the floor on its own', () => {
    const c = controller();
    expect(c.decide()).toBe(STRIPE_START_LEGS);
    c.noteActive(STRIPE_START_LEGS);
    c.noteActive(0); // leg death: the transport fell back
    expect(c.decide()).toBe(0); // inside the backoff
    nowMs += STRIPE_REENGAGE_BACKOFF_MS + 1;
    expect(c.decide()).toBe(STRIPE_START_LEGS);
  });
});
