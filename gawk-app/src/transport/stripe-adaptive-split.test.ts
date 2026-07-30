// R30 finding 5 (docs/35): the large/small split was ABSOLUTE.
//
// STRIPE_LARGE_FRAME_CHUNKS = 8 came from one measured path (docs/34 finding
// 4), and the burst signature needs BOTH buckets — large frames losing while
// small frames stay clean is what separates a per-connection burst threshold
// from uniform loss. On a high-bitrate stream every frame clears 8 chunks: a
// live Firefox 154 session measured 2967 of 3088 video chunks in the large
// bucket, leaving the small bucket permanently under STRIPE_MIN_SMALL_CHUNKS,
// so the signature could never be *evaluated* at all — 7-9% large-frame loss
// with nothing to compare it against.
//
// The split now falls back to the stream's own median frame size when the
// fixed line cannot fill the small bucket, and the test gains the ratio form
// of what cleanliness was a proxy for: loss that RISES with burst size.
//
// Since finding 6 this signature no longer gates engagement (stripe.test.ts
// owns that policy) — it is the reported answer to "is striping earning its
// connection cost here", so getting it right still matters, and getting it
// *reportable* matters more.

import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import {
  STRIPE_LARGE_FRAME_CHUNKS,
  STRIPE_MIN_SMALL_CHUNKS,
  StripeController,
  setStripeMode,
} from './stripe';

let nowMs = 100_000;
const now = () => nowMs;

function controller(): StripeController {
  const c = new StripeController(now);
  c.noteCapable(true);
  return c;
}

// A stream in the finding-5 shape: every frame above the fixed split, sizes
// varying. `lost(size)` is the loss model under test.
function feedAllLarge(
  c: StripeController,
  seconds: number,
  lost: (size: number) => number,
): void {
  const sizes = [10, 14, 18, 22, 26];
  for (let s = 0; s < seconds; s++) {
    for (let f = 0; f < 30; f++) {
      const size = sizes[f % sizes.length];
      c.observeFrame(size, size - lost(size));
    }
    nowMs += 1000;
  }
}

function feedConstant(c: StripeController, seconds: number, size: number, lostPerFrame: number) {
  for (let s = 0; s < seconds; s++) {
    for (let f = 0; f < 30; f++) c.observeFrame(size, size - lostPerFrame);
    nowMs += 1000;
  }
}

beforeEach(() => {
  nowMs = 100_000;
  setStripeMode('auto');
});
afterEach(() => setStripeMode('auto'));

describe('adaptive large/small split', () => {
  it('sees the burst shape when every frame clears the fixed split and loss rises with size', () => {
    const c = controller();
    // Burst-threshold loss: the first ~12 chunks survive, the excess dies.
    feedAllLarge(c, 10, (size) => Math.max(0, size - 12));
    expect(c.snapshot().shapeDetected).toBe(true);
  });

  it('does not see it when loss is proportional to size — striping cannot help', () => {
    const c = controller();
    feedAllLarge(c, 10, (size) => Math.round(size * 0.1));
    expect(c.snapshot().shapeDetected).toBe(false);
  });

  it('reports both buckets and the split it actually used', () => {
    const c = controller();
    feedAllLarge(c, 10, (size) => Math.max(0, size - 12));
    const s = c.snapshot();
    // The whole point of finding 5: the small bucket now has evidence in it,
    // where the fixed split left it permanently empty.
    expect(s.smallChunks).toBeGreaterThanOrEqual(STRIPE_MIN_SMALL_CHUNKS);
    expect(s.largeChunks).toBeGreaterThan(0);
    expect(s.splitAtChunks).toBeGreaterThan(STRIPE_LARGE_FRAME_CHUNKS);
  });

  it('keeps the measured fixed split when the stream really does have small frames', () => {
    const c = controller();
    for (let s = 0; s < 10; s++) {
      for (let f = 0; f < 15; f++) {
        c.observeFrame(18, 17);
        c.observeFrame(4, 4);
      }
      nowMs += 1000;
    }
    const s = c.snapshot();
    expect(s.splitAtChunks).toBe(STRIPE_LARGE_FRAME_CHUNKS);
    expect(s.shapeDetected).toBe(true);
  });

  it('stays unprovable when every frame is the SAME size — there is no shape', () => {
    const c = controller();
    feedConstant(c, 10, 18, 1);
    expect(c.snapshot().shapeDetected).toBe(false);
  });

  it('degrades safely when there are too few frames to judge', () => {
    // The adaptive split still fires here (the small bucket cannot fill), but
    // a one-second window is nowhere near the evidence floor — the guard that
    // matters is the verdict, not the split point.
    const c = controller();
    feedConstant(c, 1, 20, 2);
    expect(c.snapshot().shapeDetected).toBe(false);
  });

  it('reports an empty window without inventing a verdict', () => {
    const c = controller();
    const s = c.snapshot();
    expect(s.splitAtChunks).toBe(STRIPE_LARGE_FRAME_CHUNKS);
    expect(s.largeLossPct).toBeNull();
    expect(s.smallLossPct).toBeNull();
    expect(s.shapeDetected).toBe(false);
  });
});
