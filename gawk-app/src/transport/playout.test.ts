// R12 T2 (docs/17 Decision 5): playout is a three-mode setting — 'off'
// (live-edge, the default), 'fixed' (R5 Q3's constant 150 ms, preserved
// as-is), 'adaptive' (paced presentation; T3 replaces the seed offset with
// the jitter-tracked controller). Module state per JS context, read live.

import { afterEach, describe, expect, it } from 'vitest';
import {
  DECODE_LEAD_MS,
  PLAYOUT_OFFSET_MS,
  getPlayoutMode,
  getPlayoutOffsetMs,
  setPlayoutMode,
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
