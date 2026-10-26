// Speaking indicators (docs/80 D16): held 250 ms after the last frame,
// cleared at once on the end-of-transmission flag.
import { describe, expect, it } from 'vitest';
import { SPEAKING_HOLD_MS, SpeakingTracker } from './speaking';

describe('SpeakingTracker', () => {
  it('marks on the first frame and holds for SPEAKING_HOLD_MS after the last', () => {
    const t = new SpeakingTracker();
    expect(t.frame(3, false, 0)).toBe(true);
    expect(t.frame(3, false, 10)).toBe(false); // already speaking
    expect(t.speaking()).toEqual([3]);
    expect(t.nextExpiryMs()).toBe(10 + SPEAKING_HOLD_MS);
    expect(t.expire(10 + SPEAKING_HOLD_MS - 1)).toBe(false);
    expect(t.speaking()).toEqual([3]);
    expect(t.expire(10 + SPEAKING_HOLD_MS)).toBe(true);
    expect(t.speaking()).toEqual([]);
    expect(t.nextExpiryMs()).toBeNull();
  });

  it('clears at once on the end flag', () => {
    const t = new SpeakingTracker();
    t.frame(3, false, 0);
    t.frame(9, false, 5);
    expect(t.speaking()).toEqual([3, 9]);
    expect(t.frame(3, true, 6)).toBe(true);
    expect(t.speaking()).toEqual([9]);
    expect(t.frame(3, true, 7)).toBe(false); // nothing to clear
  });

  it('expires each speaker on their own hold, and remove / clear drop them', () => {
    const t = new SpeakingTracker(100);
    t.frame(1, false, 0);
    t.frame(2, false, 50);
    expect(t.nextExpiryMs()).toBe(100);
    t.expire(100);
    expect(t.speaking()).toEqual([2]);
    expect(t.remove(2)).toBe(true);
    t.frame(5, false, 0);
    t.clear();
    expect(t.speaking()).toEqual([]);
  });
});
