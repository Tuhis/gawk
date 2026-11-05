import { describe, expect, it } from 'vitest';

import { lastValue, niceMax, readoutValue, stepOf, timeTicks, valueAt } from './stack.ts';

// The readout rail's arithmetic (R36 item 2, docs/86 TU3). The rule that
// matters most: a reading too old to describe the crosshair's instant is
// ABSENT there (UD9), never carried forward as if it were current.

const pts: Array<[number, number | null]> = [
  [1000, 10],
  [3000, 20],
  [5000, null],
  [7000, 40],
];

describe('valueAt', () => {
  it('takes the last point at or before the instant', () => {
    expect(valueAt(pts, 3000, 4000)).toBe(20);
    expect(valueAt(pts, 3999, 4000)).toBe(20);
    expect(valueAt(pts, 7500, 4000)).toBe(40);
  });

  it('is absent before the first point and after the reading goes stale', () => {
    expect(valueAt(pts, 500, 4000)).toBeNull();
    expect(valueAt(pts, 7000 + 4001, 4000)).toBeNull();
  });

  it('keeps a gap a gap', () => {
    // The point at 5000 is a null: the series said nothing then, and the
    // readout says nothing too rather than reaching back to 20.
    expect(valueAt(pts, 5500, 4000)).toBeNull();
  });
});

describe('lastValue', () => {
  it('skips trailing gaps', () => {
    expect(lastValue([[1, 5], [2, null]])).toBe(5);
    expect(lastValue([])).toBeNull();
  });
});

describe('stepOf', () => {
  it('is the median gap, so one long gap does not stretch it', () => {
    expect(stepOf([[0, 1], [2000, 1], [4000, 1], [60_000, 1], [62_000, 1]])).toBe(2000);
    expect(stepOf([[0, 1]])).toBe(0);
  });
});

describe('timeTicks', () => {
  it('lands on round instants, at most the asked-for number of them', () => {
    const from = new Date(2026, 10, 4, 20, 12, 6).getTime();
    const to = new Date(2026, 10, 4, 21, 4, 0).getTime();
    const ticks = timeTicks(from, to, 8);
    expect(ticks.length).toBeGreaterThan(2);
    expect(ticks.length).toBeLessThanOrEqual(9);
    for (const t of ticks) {
      expect(new Date(t.atMs).getSeconds()).toBe(0);
      expect(t.pct).toBeGreaterThanOrEqual(0);
      expect(t.pct).toBeLessThanOrEqual(100);
    }
  });

  it('labels a multi-day range by day', () => {
    const from = new Date(2026, 9, 29, 18, 37).getTime();
    const to = new Date(2026, 10, 5, 18, 37).getTime();
    const ticks = timeTicks(from, to, 8);
    expect(ticks.length).toBe(7);
    expect(ticks.every((t) => new Date(t.atMs).getHours() === 0)).toBe(true);
  });

  it('has nothing to say about an empty window', () => {
    expect(timeTicks(5, 5)).toEqual([]);
  });
});

describe('niceMax and readoutValue', () => {
  it('rounds a scale up to a number a person reads', () => {
    expect(niceMax(61)).toBe(80);
    expect(niceMax(30.2)).toBe(40);
    expect(niceMax(0)).toBe(1);
  });

  it('prints an absence as a dash and gives each magnitude its digits', () => {
    expect(readoutValue(null)).toBe('—');
    expect(readoutValue(3.14159)).toBe('3.14');
    expect(readoutValue(31.25)).toBe('31.3');
    expect(readoutValue(142.4)).toBe('142');
  });
});
