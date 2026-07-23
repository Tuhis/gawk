// R12 T2 (docs/17 Decision 5): playout is a three-mode setting — 'off'
// (live-edge, the default), 'fixed' (R5 Q3's constant 150 ms, preserved
// as-is), 'adaptive' (paced presentation; T3 replaces the seed offset with
// the jitter-tracked controller). Module state per JS context, read live.

import { afterEach, describe, expect, it } from 'vitest';
import { LIVE_EDGE_WINDOW_MS, QUANTILE_RANGE_MS } from './live-edge';
import {
  DECODE_LEAD_MS,
  DEFAULT_PLAYOUT_PROFILE,
  HEADROOM_MS,
  MAX_PLAYOUT_OFFSET_MS,
  MIN_PLAYOUT_OFFSET_MS,
  OFFSET_DOWN_DWELL_MS,
  OFFSET_SLEW_DOWN_MS_PER_S,
  OFFSET_SLEW_UP_MS_PER_S,
  OFFSET_WARMUP_MS,
  PLAYOUT_OFFSET_MS,
  PlayoutController,
  RESILIENT_PLAYOUT_PROFILE,
  getPlayoutMode,
  getPlayoutOffsetMs,
  getPlayoutProfile,
  getStoredPlayoutMode,
  setPlayoutMode,
  setViewerDeliveryMode,
  updatePlayoutController,
  DVR_PLAYOUT_PROFILE,
  setDvrGranted,
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

// R19 (docs/24 Decision 7): resilient mode implies adaptive pacing with a
// wider controller profile ([150, 2000] ms, seed 500, slew 100/10). The
// stored playout mode keeps its value + semantics and regains effect the
// moment resilient mode turns off.
describe('resilient playout profile (R19)', () => {
  afterEach(() => {
    setViewerDeliveryMode('live');
    setPlayoutMode('off');
  });

  function warmedResilient(jitterMs: number, seconds: number): PlayoutController {
    const c = new PlayoutController(RESILIENT_PLAYOUT_PROFILE);
    for (let t = 0; t <= seconds * 1000; t += 1000) c.update(jitterMs, t);
    return c;
  }

  it('holds the 500 ms seed through the warmup window', () => {
    const c = new PlayoutController(RESILIENT_PLAYOUT_PROFILE);
    for (let t = 0; t < OFFSET_WARMUP_MS; t += 1000) {
      c.update(1200, t);
      expect(c.offsetMs()).toBe(RESILIENT_PLAYOUT_PROFILE.seedMs);
    }
  });

  it('clamps at the widened bounds', () => {
    expect(warmedResilient(5000, 60).offsetMs()).toBe(RESILIENT_PLAYOUT_PROFILE.maxMs);
    const c = new PlayoutController(RESILIENT_PLAYOUT_PROFILE);
    for (let t = 0; t <= OFFSET_WARMUP_MS + OFFSET_DOWN_DWELL_MS + 120_000; t += 1000) c.update(0, t);
    expect(c.offsetMs()).toBe(RESILIENT_PLAYOUT_PROFILE.minMs);
  });

  it('slews up at the resilient rate (asymmetric, faster than default)', () => {
    const c = new PlayoutController(RESILIENT_PLAYOUT_PROFILE);
    let t = 0;
    for (; t <= OFFSET_WARMUP_MS; t += 1000) c.update(150, t);
    let prev = c.offsetMs();
    for (let i = 0; i < 5; i++) {
      // Each rise stays inside stepUpAboveMs so this exercises the slew
      // regime; a rise larger than that is taken in one step instead (R19
      // hardening, PLAYOUT-1 — see the suite at the end of this file).
      c.update(prev + RESILIENT_PLAYOUT_PROFILE.stepUpAboveMs - HEADROOM_MS - 1, t);
      const step = c.offsetMs() - prev;
      expect(step).toBeLessThanOrEqual(RESILIENT_PLAYOUT_PROFILE.slewUpMsPerS + 1e-9);
      prev = c.offsetMs();
      t += 1000;
    }
    expect(c.offsetMs()).toBeGreaterThan(RESILIENT_PLAYOUT_PROFILE.seedMs);
  });

  it('implies adaptive pacing while active, seeded at 500', () => {
    setPlayoutMode('off');
    setViewerDeliveryMode('resilient');
    expect(getPlayoutMode()).toBe('adaptive');
    expect(getPlayoutOffsetMs()).toBe(RESILIENT_PLAYOUT_PROFILE.seedMs);
  });

  it('the stored playout mode survives a resilient on/off round-trip', () => {
    setPlayoutMode('fixed');
    setViewerDeliveryMode('resilient');
    expect(getStoredPlayoutMode()).toBe('fixed');
    expect(getPlayoutMode()).toBe('adaptive');
    setViewerDeliveryMode('live');
    expect(getStoredPlayoutMode()).toBe('fixed');
    expect(getPlayoutMode()).toBe('fixed');
    expect(getPlayoutOffsetMs()).toBe(PLAYOUT_OFFSET_MS);
  });

  it('leaving resilient mode re-seeds the controller onto the default profile', () => {
    setViewerDeliveryMode('resilient');
    setPlayoutMode('adaptive');
    expect(getPlayoutOffsetMs()).toBe(RESILIENT_PLAYOUT_PROFILE.seedMs);
    setViewerDeliveryMode('live');
    // Still adaptive by stored mode, but back on the default seed/envelope.
    expect(getPlayoutOffsetMs()).toBe(PLAYOUT_OFFSET_MS);
  });

  it('updatePlayoutController runs under resilient mode even with stored mode off', () => {
    setPlayoutMode('off');
    setViewerDeliveryMode('resilient');
    for (let t = 0; t <= OFFSET_WARMUP_MS + 60_000; t += 1000) updatePlayoutController(1200, t);
    expect(getPlayoutOffsetMs()).toBe(1200 + HEADROOM_MS);
  });
});

// R19 hardening — PLAYOUT-1 (docs/reviews/resilient-mode-review.md). Two
// halves: the profile must be able to *measure* the envelope it clamps to
// (the histogram range lived outside the profile and capped the signal at
// 500 ms), and it must be able to *reach* it in time — slewing at 100 ms/s
// from a 500 ms seed spends 10 s stuttering on the way to 1500 ms, which is
// the stutter the mode exists to remove.
describe('playout profiles carry their own jitter measurement (R19 PLAYOUT-1)', () => {
  afterEach(() => {
    setViewerDeliveryMode('live');
    setPlayoutMode('off');
  });

  it('each profile can measure its whole clamp range', () => {
    // The invariant the bug violated: a clamp the signal cannot express is
    // dead envelope. Applies to both profiles, so neither drifts back.
    for (const p of [DEFAULT_PLAYOUT_PROFILE, RESILIENT_PLAYOUT_PROFILE]) {
      expect(p.quantileRangeMs).toBeGreaterThanOrEqual(p.maxMs);
    }
  });

  it('the resilient profile measures over a shorter window than the default', () => {
    // PLAYOUT-3: a 60 s window reacts on a minute timescale. Down-direction
    // memory lives in the dwell + slew, not in the measurement window.
    expect(RESILIENT_PLAYOUT_PROFILE.jitterWindowMs).toBeLessThan(
      DEFAULT_PLAYOUT_PROFILE.jitterWindowMs,
    );
    expect(DEFAULT_PLAYOUT_PROFILE.jitterWindowMs).toBe(LIVE_EDGE_WINDOW_MS);
    expect(DEFAULT_PLAYOUT_PROFILE.quantileRangeMs).toBe(QUANTILE_RANGE_MS);
  });

  it('getPlayoutProfile follows the resilient flag', () => {
    expect(getPlayoutProfile()).toBe(DEFAULT_PLAYOUT_PROFILE);
    setViewerDeliveryMode('resilient');
    expect(getPlayoutProfile()).toBe(RESILIENT_PLAYOUT_PROFILE);
  });

  it('steps straight to a far-above target instead of slewing for seconds', () => {
    const c = new PlayoutController(RESILIENT_PLAYOUT_PROFILE);
    let t = 0;
    for (; t <= OFFSET_WARMUP_MS; t += 1000) c.update(50, t);
    const before = c.offsetMs();
    c.update(1500, t); // a stall the seed-sized buffer cannot absorb
    expect(before).toBeLessThan(1000);
    expect(c.offsetMs()).toBe(1500 + HEADROOM_MS);
  });

  it('still slews a small rise, and never steps downward', () => {
    const c = new PlayoutController(RESILIENT_PLAYOUT_PROFILE);
    let t = 0;
    for (; t <= OFFSET_WARMUP_MS; t += 1000) c.update(400, t);
    const before = c.offsetMs();
    c.update(400 + RESILIENT_PLAYOUT_PROFILE.stepUpAboveMs - 1, t); // contiguous 1 s step
    expect(c.offsetMs() - before).toBeLessThanOrEqual(
      RESILIENT_PLAYOUT_PROFILE.slewUpMsPerS + 1e-9,
    );
    // Downward is always slewed + dwell-gated: a step down is a visible skip.
    const high = c.offsetMs();
    t += 1000;
    c.update(0, t);
    expect(c.offsetMs()).toBe(high);
  });

  it('the default profile never steps — live-edge pacing is unchanged', () => {
    const c = new PlayoutController(DEFAULT_PLAYOUT_PROFILE);
    let t = 0;
    for (; t <= OFFSET_WARMUP_MS; t += 1000) c.update(0, t);
    let prev = c.offsetMs();
    for (let i = 0; i < 3; i++) {
      c.update(10_000, t); // far above anything the default clamp allows
      expect(c.offsetMs() - prev).toBeLessThanOrEqual(OFFSET_SLEW_UP_MS_PER_S + 1e-9);
      prev = c.offsetMs();
      t += 1000;
    }
  });
});

// R21 (docs/26 Decision 15): three points on one axis, and the deep floor
// needs BOTH the user's choice and the relay's confirmation.
describe('viewer delivery mode', () => {
  afterEach(() => {
    setViewerDeliveryMode('live');
    setDvrGranted(false);
  });

  it('live edge keeps the default profile', () => {
    setViewerDeliveryMode('live');
    expect(getPlayoutProfile()).toBe(DEFAULT_PLAYOUT_PROFILE);
  });

  it('resilient uses the R19 envelope, never the deep one', () => {
    setViewerDeliveryMode('resilient');
    expect(getPlayoutProfile()).toBe(RESILIENT_PLAYOUT_PROFILE);
    // Even if a relay somehow granted a ring, a viewer that did not ask for
    // the deep buffer must not silently get its latency.
    setDvrGranted(true);
    expect(getPlayoutProfile()).toBe(RESILIENT_PLAYOUT_PROFILE);
  });

  it('deep buffer waits for the relay to confirm it can back it', () => {
    setViewerDeliveryMode('deep');
    // Asked for, not yet granted: against a relay that cannot keep it filled
    // a multi-second buffer is pure latency for no benefit.
    expect(getPlayoutProfile()).toBe(RESILIENT_PLAYOUT_PROFILE);
    setDvrGranted(true);
    expect(getPlayoutProfile()).toBe(DVR_PLAYOUT_PROFILE);
  });

  it('a mode change forgets the previous session grant', () => {
    setViewerDeliveryMode('deep');
    setDvrGranted(true);
    expect(getPlayoutProfile()).toBe(DVR_PLAYOUT_PROFILE);
    setViewerDeliveryMode('resilient');
    setViewerDeliveryMode('deep');
    // A mode change is a deliberate reconnect; the new session must re-earn it.
    expect(getPlayoutProfile()).toBe(RESILIENT_PLAYOUT_PROFILE);
  });
});
