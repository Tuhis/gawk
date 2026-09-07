import { describe, expect, it } from 'vitest';
import { BACKGROUND_STOP_MS, BackgroundWatchdog } from './backgroundWatchdog';

const MIN = 60_000;

describe('BackgroundWatchdog', () => {
  it('fires once after the threshold hidden with no frame encoded', () => {
    const w = new BackgroundWatchdog();
    expect(w.sample(true, 100, 0)).toBe(false); // hidden span starts
    expect(w.sample(true, 100, 4 * MIN)).toBe(false);
    expect(w.sample(true, 100, BACKGROUND_STOP_MS)).toBe(true);
    // Not again on the next tick.
    expect(w.sample(true, 100, BACKGROUND_STOP_MS + 1000)).toBe(false);
  });

  it('frames flowing while hidden keep restarting the span (the worker path)', () => {
    const w = new BackgroundWatchdog();
    w.sample(true, 100, 0);
    expect(w.sample(true, 160, 4 * MIN)).toBe(false); // frames advanced → span restarts at 4 min
    expect(w.sample(true, 160, 8 * MIN)).toBe(false); // 4 min into the new span
    expect(w.sample(true, 160, 9 * MIN)).toBe(true); // 5 min silent from 4 min
  });

  it('becoming visible resets everything; a static VISIBLE screen never fires', () => {
    const w = new BackgroundWatchdog();
    w.sample(true, 100, 0);
    w.sample(true, 100, 4 * MIN);
    expect(w.sample(false, 100, 4 * MIN + 1)).toBe(false);
    // Visible and static for an hour: nothing.
    expect(w.sample(false, 100, 70 * MIN)).toBe(false);
    // Hidden again: a fresh span, not the old one.
    expect(w.sample(true, 100, 70 * MIN)).toBe(false);
    expect(w.sample(true, 100, 74 * MIN)).toBe(false);
    expect(w.sample(true, 100, 75 * MIN)).toBe(true);
  });

  it('reset() forgets the span so a restarted broadcast starts clean', () => {
    const w = new BackgroundWatchdog();
    w.sample(true, 100, 0);
    w.sample(true, 100, 4 * MIN);
    w.reset();
    expect(w.sample(true, 100, 5 * MIN)).toBe(false); // a new span begins here
    expect(w.sample(true, 100, 10 * MIN)).toBe(true);
  });
});
