import { describe, expect, it } from 'vitest';

import { ago, bitrate, dur, EMPTY, fps, ms, num } from './format.ts';

// The formatters exist to stop the layout MOVING, and the rule they enforce is
// that an ABSENT value never renders as a number. A missing `capToRenderMs`
// means "not measured", not "0 ms", and the difference is the whole health
// model in miniature.
describe('formatters', () => {
  it('renders absent values as the em dash, never as zero', () => {
    for (const f of [num, fps, ms, bitrate]) {
      expect(f(undefined)).toBe(EMPTY);
      expect(f(null)).toBe(EMPTY);
    }
    expect(num(NaN)).toBe(EMPTY);
    expect(fps(Infinity)).toBe(EMPTY);
  });

  it('renders a real zero as zero', () => {
    expect(num(0)).toBe('0');
    expect(fps(0)).toBe('0.0');
    expect(ms(0)).toBe('0 ms');
  });

  // Fixed decimal counts per bucket, so values in a column share a width and
  // the column cannot shuffle as they tick.
  it('keeps width stable within a bucket', () => {
    expect(fps(9.9)).toHaveLength(fps(1.2).length);
    expect(fps(30)).toHaveLength(fps(60).length);
    expect(num(1.234, 2)).toBe('1.23');
  });

  // A negative age is how the backend spells "this side has produced no
  // evidence at all". It must never read as "0s ago".
  it('spells a never-seen side as never', () => {
    expect(ago(-1)).toBe('never');
    expect(ago(undefined)).toBe('never');
    expect(ago(500)).toBe('now');
    expect(ago(12_000)).toBe('12s ago');
    expect(ago(120_000)).toBe('2m ago');
  });

  it('formats durations without losing the unit', () => {
    expect(dur(5_000)).toBe('5s');
    expect(dur(65_000)).toBe('1m 05s');
    expect(dur(3_725_000)).toBe('1h 02m');
    expect(dur(-1)).toBe(EMPTY);
  });

  it('scales bitrates', () => {
    expect(bitrate(5_500_000)).toBe('5.5 Mbps');
    expect(bitrate(64_000)).toBe('64 kbps');
    expect(bitrate(900)).toBe('900 bps');
  });
});
