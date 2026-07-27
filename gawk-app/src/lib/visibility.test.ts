// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { __resetVisibilityForTests, readVisibility } from './visibility.ts';

// jsdom reports `visibilityState` from a getter, so the state is faked by
// redefining it and dispatching the event the real browser would.
function setVisibility(state: 'visible' | 'hidden'): void {
  Object.defineProperty(document, 'visibilityState', {
    configurable: true,
    get: () => state,
  });
  document.dispatchEvent(new Event('visibilitychange'));
}

describe('visibility tracker', () => {
  beforeEach(() => {
    vi.spyOn(performance, 'now').mockReturnValue(0);
    setVisibility('visible');
    __resetVisibilityForTests();
  });

  afterEach(() => {
    __resetVisibilityForTests();
    vi.restoreAllMocks();
  });

  it('reports a visible tab as not hidden, with a zero counter', () => {
    expect(readVisibility()).toEqual({ documentHidden: false, documentHiddenMs: 0 });
  });

  it('reports hidden while hidden', () => {
    readVisibility(); // start the tracker
    setVisibility('hidden');
    expect(readVisibility().documentHidden).toBe(true);
  });

  // The counter has to include the interval still in progress. One that only
  // advanced on the way back to visible would read 0 for exactly as long as the
  // condition it describes was true — which is the whole window a reader needs
  // to discount.
  it('counts the in-progress hidden interval', () => {
    readVisibility();
    vi.spyOn(performance, 'now').mockReturnValue(1_000);
    setVisibility('hidden');
    vi.spyOn(performance, 'now').mockReturnValue(4_000);
    expect(readVisibility().documentHiddenMs).toBe(3_000);
  });

  it('accumulates across several hidden intervals', () => {
    readVisibility();
    vi.spyOn(performance, 'now').mockReturnValue(1_000);
    setVisibility('hidden');
    vi.spyOn(performance, 'now').mockReturnValue(3_000);
    setVisibility('visible');
    expect(readVisibility().documentHiddenMs).toBe(2_000);

    vi.spyOn(performance, 'now').mockReturnValue(10_000);
    setVisibility('hidden');
    vi.spyOn(performance, 'now').mockReturnValue(10_500);
    setVisibility('visible');
    expect(readVisibility().documentHiddenMs).toBe(2_500);
  });

  // It only ever goes up, which is what lets a reader take a delta between two
  // samples and get the hidden share of that interval.
  it('never goes backwards', () => {
    readVisibility();
    vi.spyOn(performance, 'now').mockReturnValue(1_000);
    setVisibility('hidden');
    vi.spyOn(performance, 'now').mockReturnValue(5_000);
    const during = readVisibility().documentHiddenMs;
    setVisibility('visible');
    expect(readVisibility().documentHiddenMs).toBeGreaterThanOrEqual(during);
  });

  // Some browsers fire 'hidden' twice around bfcache. Starting a second
  // interval on the duplicate would double-count the first.
  it('ignores a duplicate hidden event', () => {
    readVisibility();
    vi.spyOn(performance, 'now').mockReturnValue(1_000);
    setVisibility('hidden');
    vi.spyOn(performance, 'now').mockReturnValue(2_000);
    setVisibility('hidden'); // duplicate
    vi.spyOn(performance, 'now').mockReturnValue(4_000);
    setVisibility('visible');
    expect(readVisibility().documentHiddenMs).toBe(3_000);
  });

  // A stream started in a background tab, or restored from bfcache, is hidden
  // before anything reads it. Missing that interval entirely would understate
  // exactly the case most worth catching.
  it('counts time already spent hidden when collection starts', () => {
    __resetVisibilityForTests();
    vi.spyOn(performance, 'now').mockReturnValue(500);
    Object.defineProperty(document, 'visibilityState', {
      configurable: true,
      get: () => 'hidden',
    });
    readVisibility(); // first read starts the tracker, already hidden
    vi.spyOn(performance, 'now').mockReturnValue(2_500);
    expect(readVisibility().documentHiddenMs).toBe(2_000);
  });
});
