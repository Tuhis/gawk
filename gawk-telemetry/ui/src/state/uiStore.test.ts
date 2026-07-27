import { beforeEach, describe, expect, it } from 'vitest';

import { opensByDefault, useUiStore } from './uiStore.ts';

// The card state model, which is the thing the old vanilla page got wrong: it
// rebuilt the DOM every 2 s and recomputed `open` from severity alone, so a
// card snapped shut under whoever was reading it.
//
// The fix is NOT "remember whether it is open". A card opens by itself when
// something is wrong — that is the page's premise — and remembering raw
// open-state would pin it open long after the fault cleared. What is remembered
// is the operator's DISAGREEMENT with the default.
describe('card open state', () => {
  beforeEach(() => {
    useUiStore.setState({ cardOverrides: {}, openTimelines: {}, foundKey: null });
  });

  it('follows severity when the operator has not intervened', () => {
    const { isCardOpen } = useUiStore.getState();
    expect(isCardOpen('b1', true)).toBe(true);
    expect(isCardOpen('b1', false)).toBe(false);
  });

  it('remembers a disagreement and keeps it across refreshes', () => {
    const s = useUiStore.getState();
    s.setCardOpen('b1', true, false); // expanded a healthy card
    expect(useUiStore.getState().isCardOpen('b1', false)).toBe(true);
    expect(useUiStore.getState().isCardOpen('b1', false)).toBe(true);
  });

  it('collapses a warning card and keeps it collapsed', () => {
    useUiStore.getState().setCardOpen('b1', false, true);
    expect(useUiStore.getState().isCardOpen('b1', true)).toBe(false);
  });

  // Once the default agrees with the choice, the override dissolves, so a
  // recovered broadcast follows severity again instead of being muted forever.
  it('dissolves the override when the default catches up', () => {
    const s = useUiStore.getState();
    s.setCardOpen('b1', false, true); // collapsed a bad card
    expect(useUiStore.getState().cardOverrides.b1).toBe(false);
    // It recovers: the default is now also collapsed.
    s.setCardOpen('b1', false, false);
    expect(useUiStore.getState().cardOverrides.b1).toBeUndefined();
    // ...and a new fault gets to open it again.
    expect(useUiStore.getState().isCardOpen('b1', true)).toBe(true);
  });

  it('prunes overrides for broadcasts that have left the page', () => {
    const s = useUiStore.getState();
    s.setCardOpen('gone', true, false);
    s.setCardOpen('here', true, false);
    useUiStore.getState().prune(new Set(['here']));
    expect(useUiStore.getState().cardOverrides).toEqual({ here: true });
  });

  it('opens by default only when something is wrong', () => {
    expect(opensByDefault('bad')).toBe(true);
    expect(opensByDefault('warn')).toBe(true);
    expect(opensByDefault('ok')).toBe(false);
    // `unknown` deliberately does NOT auto-open: it is the state of a fleet
    // that has not reported yet, and auto-opening every row on a cold start
    // would bury the one that matters.
    expect(opensByDefault('unknown')).toBe(false);
  });
});
