import { describe, expect, it } from 'vitest';

import { toRate, toSpans } from './series.ts';

// UD8's behaviours, tested where they live. A counter drawn as a line and a
// bool drawn as a line are the two ways a correct number becomes a misleading
// picture, and the catalogue exists to make both avoidable.

describe('a counter is offered as a rate', () => {
  it('differences the counter per second', () => {
    const out = toRate([
      [0, 0],
      [1000, 10],
      [2000, 30],
    ]);
    // The first point has no predecessor to difference against, so it is a
    // break rather than a fabricated zero.
    expect(out[0]).toEqual([0, null]);
    expect(out[1]).toEqual([1000, 10]);
    expect(out[2]).toEqual([2000, 20]);
  });

  it('keeps a gap as a gap', () => {
    const out = toRate([
      [0, 0],
      [1000, 10],
      [2000, null],
      [3000, 40],
    ]);
    expect(out[2]).toEqual([2000, null]);
    // And the point AFTER the gap cannot be differenced across it: the counter
    // may have advanced for the whole missing interval.
    expect(out[3]).toEqual([3000, null]);
  });

  it('breaks on a counter reset instead of drawing negative traffic', () => {
    const out = toRate([
      [0, 100],
      [1000, 140],
      [2000, 5], // the producer restarted
      [3000, 25],
    ]);
    expect(out[2]).toEqual([2000, null]);
    expect(out[3]).toEqual([3000, 20]);
  });
});

describe('a bool is a band, not a line', () => {
  it('folds contiguous truthy runs into spans', () => {
    expect(
      toSpans([
        [0, 0],
        [1000, 1],
        [2000, 1],
        [3000, 0],
        [4000, 1],
      ]),
    ).toEqual([
      { fromMs: 1000, toMs: 3000 },
      // A run still open at the LAST sample closes there, not at some assumed
      // end — our knowledge stops with the samples. A single trailing sample
      // therefore yields a zero-width span, which draws nothing: honest, and
      // preferable to extending a band over time nobody observed.
      { fromMs: 4000, toMs: 4000 },
    ]);
  });

  it('closes an open run at the last sample rather than leaving it dangling', () => {
    expect(
      toSpans([
        [0, 0],
        [1000, 1],
        [2000, 1],
      ]),
    ).toEqual([{ fromMs: 1000, toMs: 2000 }]);
  });
});
