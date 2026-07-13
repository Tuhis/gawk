// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act, renderHook } from '@testing-library/react';
import { useAutoHide } from './useAutoHide';

beforeEach(() => vi.useFakeTimers());
afterEach(() => vi.useRealTimers());

describe('useAutoHide', () => {
  it('is visible on mount and hides after the idle window', () => {
    const { result } = renderHook(() => useAutoHide(3000, true));
    expect(result.current).toBe(true);
    act(() => vi.advanceTimersByTime(3000));
    expect(result.current).toBe(false);
  });

  it('reveals again on pointer activity, then re-hides', () => {
    const { result } = renderHook(() => useAutoHide(3000, true));
    act(() => vi.advanceTimersByTime(3000));
    expect(result.current).toBe(false);

    act(() => window.dispatchEvent(new Event('pointermove')));
    expect(result.current).toBe(true);

    act(() => vi.advanceTimersByTime(3000));
    expect(result.current).toBe(false);
  });

  it('stays visible while disabled', () => {
    const { result } = renderHook(() => useAutoHide(3000, false));
    expect(result.current).toBe(true);
    act(() => vi.advanceTimersByTime(10_000));
    expect(result.current).toBe(true);
  });
});
