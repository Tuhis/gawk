// R12 T4 (docs/17 Decision 7): the pure slot logic behind experimental frame
// interpolation, and its per-context toggle.

import { afterEach, describe, expect, it } from 'vitest';
import {
  MAX_INTERPOLATION_GAP_MS,
  getInterpolationEnabled,
  midSlotMs,
  setInterpolationEnabled,
} from './interpolation';

afterEach(() => setInterpolationEnabled(false));

describe('interpolation toggle', () => {
  it('defaults off and round-trips', () => {
    expect(getInterpolationEnabled()).toBe(false);
    setInterpolationEnabled(true);
    expect(getInterpolationEnabled()).toBe(true);
  });
});

describe('midSlotMs', () => {
  it('is the midpoint of two consecutive display targets', () => {
    expect(midSlotMs(1000, 1033)).toBeCloseTo(1016.5);
  });

  it('is null without a previous presented frame', () => {
    expect(midSlotMs(null, 1033)).toBeNull();
  });

  it('is null across a stall or resync (gap too wide to blend)', () => {
    expect(midSlotMs(1000, 1000 + MAX_INTERPOLATION_GAP_MS + 1)).toBeNull();
    expect(midSlotMs(1000, 1000 + MAX_INTERPOLATION_GAP_MS)).not.toBeNull();
  });

  it('is null for non-monotonic targets', () => {
    expect(midSlotMs(1033, 1000)).toBeNull();
    expect(midSlotMs(1000, 1000)).toBeNull();
  });
});
