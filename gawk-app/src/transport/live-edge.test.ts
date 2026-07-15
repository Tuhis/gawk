// Live-edge drift estimator (R5 Q1, docs/15). Pure module, fake clock.

import { describe, expect, it } from 'vitest';
import {
  LIVE_EDGE_WINDOW_MS,
  LiveEdgeTracker,
  WindowedMinTracker,
  WindowedQuantileTracker,
  QUANTILE_BIN_MS,
  QUANTILE_RANGE_MS,
} from './live-edge';

describe('WindowedMinTracker', () => {
  it('tracks the minimum across observations', () => {
    const t = new WindowedMinTracker();
    t.observe(50, 1000);
    t.observe(30, 2000);
    t.observe(80, 3000);
    expect(t.min(3000)).toBe(30);
  });

  it('ages a stale minimum out of the window', () => {
    const t = new WindowedMinTracker(10_000, 1000);
    t.observe(10, 0);
    t.observe(40, 5000);
    expect(t.min(5000)).toBe(10);
    // The bucket holding 10 ends at 1000; past 1000 + window it is gone.
    expect(t.min(11_500)).toBe(40);
  });

  it('returns null when empty and after reset', () => {
    const t = new WindowedMinTracker();
    expect(t.min(0)).toBeNull();
    t.observe(5, 100);
    t.reset();
    expect(t.min(100)).toBeNull();
  });
});

describe('WindowedQuantileTracker (R12 T1)', () => {
  it('returns the requested quantile on a known distribution', () => {
    // 1 ms bins make the quantile exact for integer values.
    const t = new WindowedQuantileTracker(60_000, 1000, 1, 500);
    for (let v = 1; v <= 100; v++) t.observe(v, 500);
    expect(t.quantile(0.95, 500)).toBe(95);
    expect(t.quantile(0.5, 500)).toBe(50);
    // q=0 reads the smallest observed bin.
    expect(t.quantile(0, 500)).toBe(1);
  });

  it('is null when empty and after reset', () => {
    const t = new WindowedQuantileTracker();
    expect(t.quantile(0.95, 0)).toBeNull();
    t.observe(10, 100);
    t.reset();
    expect(t.quantile(0.95, 100)).toBeNull();
  });

  it('ages old observations out of the window', () => {
    const t = new WindowedQuantileTracker(10_000, 1000, 1, 500);
    t.observe(10, 0);
    t.observe(10, 0);
    t.observe(40, 5000);
    expect(t.quantile(0, 5000)).toBe(10);
    // The bucket holding 10s ends at 1000; past 1000 + window it is gone.
    expect(t.quantile(0, 11_500)).toBe(40);
    expect(t.quantile(1, 11_500)).toBe(40);
  });

  it('rebases a bucket when a smaller value arrives after its first', () => {
    const t = new WindowedQuantileTracker(60_000, 1000, QUANTILE_BIN_MS, QUANTILE_RANGE_MS);
    t.observe(50, 100);
    t.observe(30, 200); // same bucket, below the initial min → rebase
    expect(t.quantile(0, 200)).toBe(30);
    expect(t.quantile(1, 200)).toBeGreaterThanOrEqual(46); // 50 stays counted (bin lower edge)
  });

  it('clamps far outliers into the top bin instead of growing memory', () => {
    const t = new WindowedQuantileTracker(60_000, 1000, QUANTILE_BIN_MS, QUANTILE_RANGE_MS);
    t.observe(0, 100);
    t.observe(10_000, 200); // way past the range → top bin
    expect(t.quantile(1, 200)).toBe(QUANTILE_RANGE_MS);
  });
});

describe('LiveEdgeTracker', () => {
  it('is null before the first frame', () => {
    const t = new LiveEdgeTracker(() => 1000);
    expect(t.driftMs()).toBeNull();
  });

  it('reads zero drift at the session-best delta and grows as frames lag', () => {
    let now = 10_000;
    const t = new LiveEdgeTracker(() => now);
    // Frame captured (broadcaster clock) at 9_800ms => delta 200ms.
    t.observe(9_800_000);
    expect(t.driftMs()).toBe(0); // the only sample is the baseline

    // Next frame arrives 120ms later but was captured only 20ms after the
    // previous one: delta 300ms, drift 100ms over the baseline.
    now = 10_120;
    t.observe(9_820_000);
    expect(t.driftMs()).toBeCloseTo(100);

    // Catch back up to the baseline delta: drift returns to 0.
    now = 10_220;
    t.observe(10_020_000);
    expect(t.driftMs()).toBeCloseTo(0);
  });

  it('clamps drift at zero when a new best delta appears', () => {
    let now = 1000;
    const t = new LiveEdgeTracker(() => now);
    t.observe(500_000); // delta 500ms
    now = 1100;
    t.observe(700_000); // delta 400ms — new best, better than baseline
    expect(t.driftMs()).toBe(0);
  });

  it('absorbs slow clock skew: an old minimum ages out of the window', () => {
    let now = 0;
    const t = new LiveEdgeTracker(() => now);
    t.observe(-200_000); // delta 200ms at t=0
    // Much later (past the window), steady frames at delta 260ms — e.g. pure
    // crystal skew. Drift must not read a permanent 60ms.
    now = LIVE_EDGE_WINDOW_MS + 61_000;
    t.observe((now - 260) * 1000);
    expect(t.driftMs()).toBe(0);
  });

  it('reset clears the baseline (broadcaster restart: new timestamp timeline)', () => {
    let now = 5000;
    const t = new LiveEdgeTracker(() => now);
    t.observe(4_000_000); // delta 1000ms
    t.reset();
    expect(t.driftMs()).toBeNull();
    // New session with a fresh (tiny) timestamp origin: huge delta, but it is
    // the new baseline, so drift restarts at 0 instead of reading seconds.
    now = 5100;
    t.observe(100_000);
    expect(t.driftMs()).toBe(0);
  });
});
