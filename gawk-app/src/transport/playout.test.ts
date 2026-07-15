// R12 T2 (docs/17 Decision 5): playout is a three-mode setting — 'off'
// (live-edge, the default), 'fixed' (R5 Q3's constant 150 ms, preserved
// as-is), 'adaptive' (paced presentation; T3 replaces the seed offset with
// the jitter-tracked controller). Module state per JS context, read live.

import { afterEach, describe, expect, it } from 'vitest';
import {
  DECODE_LEAD_MS,
  HEADROOM_MS,
  MAX_PLAYOUT_OFFSET_MS,
  MIN_PLAYOUT_OFFSET_MS,
  OFFSET_DOWN_DWELL_MS,
  OFFSET_SLEW_DOWN_MS_PER_S,
  OFFSET_SLEW_UP_MS_PER_S,
  OFFSET_WARMUP_MS,
  PLAYOUT_OFFSET_MS,
  PlayoutController,
  getPlayoutMode,
  getPlayoutOffsetMs,
  setPlayoutMode,
  updatePlayoutController,
} from './playout';

afterEach(() => setPlayoutMode('off'));

describe('playout modes (R12 T2)', () => {
  it('defaults to off — live-edge, zero offset', () => {
    expect(getPlayoutMode()).toBe('off');
    expect(getPlayoutOffsetMs()).toBe(0);
  });

  it('fixed mode keeps the R5 Q3 constant offset', () => {
    setPlayoutMode('fixed');
    expect(getPlayoutMode()).toBe('fixed');
    expect(getPlayoutOffsetMs()).toBe(PLAYOUT_OFFSET_MS);
  });

  it('adaptive mode starts at the seed offset (T3 makes it dynamic)', () => {
    setPlayoutMode('adaptive');
    expect(getPlayoutMode()).toBe('adaptive');
    expect(getPlayoutOffsetMs()).toBe(PLAYOUT_OFFSET_MS);
  });

  it('switching back off zeroes the offset immediately', () => {
    setPlayoutMode('adaptive');
    setPlayoutMode('off');
    expect(getPlayoutOffsetMs()).toBe(0);
  });

  it('names the decode lead beside the offsets', () => {
    expect(DECODE_LEAD_MS).toBeGreaterThan(0);
    expect(DECODE_LEAD_MS).toBeLessThan(PLAYOUT_OFFSET_MS);
  });
});

// R12 T3 (docs/17 Decision 6): the adaptive offset controller —
// clamp(p95−min jitter + headroom, [MIN, MAX]), seeded at 150 until the
// jitter window has warmed up, slewed asymmetrically: up fast
// (under-buffering drops frames NOW), down slowly and only after the target
// has sat well below the current offset for a dwell period.
describe('PlayoutController (R12 T3)', () => {
  // Drives update() once per second from t=0; returns the controller.
  function warmedController(jitterMs: number, seconds: number): PlayoutController {
    const c = new PlayoutController();
    for (let t = 0; t <= seconds * 1000; t += 1000) c.update(jitterMs, t);
    return c;
  }

  it('holds the seed through the warmup window', () => {
    const c = new PlayoutController();
    for (let t = 0; t < OFFSET_WARMUP_MS; t += 1000) {
      c.update(300, t);
      expect(c.offsetMs()).toBe(PLAYOUT_OFFSET_MS);
    }
  });

  it('converges to jitter + headroom on a jittery link', () => {
    const c = warmedController(300, 20);
    expect(c.offsetMs()).toBe(300 + HEADROOM_MS);
  });

  it('clamps at both bounds', () => {
    expect(warmedController(1000, 30).offsetMs()).toBe(MAX_PLAYOUT_OFFSET_MS);
    // A clean link wants ~headroom only, but never below the floor. Reaching
    // the floor takes the dwell plus a long slew-down; drive it far enough.
    const c = new PlayoutController();
    let t = 0;
    for (; t <= OFFSET_WARMUP_MS + (OFFSET_DOWN_DWELL_MS + 60_000); t += 1000) c.update(0, t);
    expect(c.offsetMs()).toBe(MIN_PLAYOUT_OFFSET_MS);
  });

  it('rises fast but never faster than the up-slew rate', () => {
    const c = new PlayoutController();
    let t = 0;
    for (; t <= OFFSET_WARMUP_MS; t += 1000) c.update(0, t);
    let prev = c.offsetMs();
    for (let i = 0; i < 5; i++) {
      c.update(300, t); // contiguous 1 s updates: slew bound is rate × 1 s
      const step = c.offsetMs() - prev;
      expect(step).toBeLessThanOrEqual(OFFSET_SLEW_UP_MS_PER_S + 1e-9);
      prev = c.offsetMs();
      t += 1000;
    }
    expect(c.offsetMs()).toBeGreaterThan(PLAYOUT_OFFSET_MS);
  });

  it('decreases only after the dwell period, then slowly', () => {
    // Converge high first.
    const c = warmedController(300, 20);
    const high = c.offsetMs();
    let t = 21_000;
    // Jitter vanishes: the offset must hold through the dwell…
    const dwellEnd = t + OFFSET_DOWN_DWELL_MS;
    for (; t < dwellEnd; t += 1000) {
      c.update(0, t);
      expect(c.offsetMs()).toBe(high);
    }
    // …then descend, bounded by the down-slew rate.
    let prev = c.offsetMs();
    for (let i = 0; i < 5; i++) {
      c.update(0, t);
      const step = prev - c.offsetMs();
      expect(step).toBeGreaterThan(0);
      expect(step).toBeLessThanOrEqual(OFFSET_SLEW_DOWN_MS_PER_S + 1e-9);
      prev = c.offsetMs();
      t += 1000;
    }
  });

  it('a jitter spike mid-descent flips straight back to rising', () => {
    const c = warmedController(300, 20);
    let t = 21_000;
    for (; t < 21_000 + OFFSET_DOWN_DWELL_MS + 3000; t += 1000) c.update(0, t);
    const midDescent = c.offsetMs();
    c.update(300, t);
    expect(c.offsetMs()).toBeGreaterThanOrEqual(midDescent);
  });

  it('reset() re-seeds and re-arms the warmup', () => {
    const c = warmedController(300, 20);
    c.reset();
    expect(c.offsetMs()).toBe(PLAYOUT_OFFSET_MS);
    c.update(300, 100_000);
    expect(c.offsetMs()).toBe(PLAYOUT_OFFSET_MS); // warming up again
  });

  it('null jitter (no data yet) keeps the current offset', () => {
    const c = warmedController(300, 20);
    const v = c.offsetMs();
    c.update(null, 30_000);
    expect(c.offsetMs()).toBe(v);
  });
});

describe('adaptive mode wiring (R12 T3)', () => {
  it('getPlayoutOffsetMs tracks the controller in adaptive mode only', () => {
    setPlayoutMode('adaptive');
    // Warm past the window with steady high jitter, once per second.
    for (let t = 0; t <= 30_000; t += 1000) updatePlayoutController(250, t);
    expect(getPlayoutOffsetMs()).toBe(250 + HEADROOM_MS);
    // Leaving adaptive mode returns the static offsets immediately.
    setPlayoutMode('fixed');
    expect(getPlayoutOffsetMs()).toBe(PLAYOUT_OFFSET_MS);
    setPlayoutMode('off');
    expect(getPlayoutOffsetMs()).toBe(0);
  });

  it('updatePlayoutController is a no-op outside adaptive mode', () => {
    setPlayoutMode('fixed');
    for (let t = 0; t <= 30_000; t += 1000) updatePlayoutController(250, t);
    setPlayoutMode('adaptive'); // entering adaptive re-seeds
    expect(getPlayoutOffsetMs()).toBe(PLAYOUT_OFFSET_MS);
  });
});
