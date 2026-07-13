// @vitest-environment jsdom
//
// jsdom has no Fullscreen API, so we mock requestFullscreen/exitFullscreen and
// a mutable document.fullscreenElement, firing `fullscreenchange` the way the
// real API does — enough to prove toggle() enters/exits and the hook tracks it.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act, renderHook } from '@testing-library/react';
import { useFullscreen } from './useFullscreen';

let fsElement: Element | null = null;

beforeEach(() => {
  fsElement = null;
  Object.defineProperty(document, 'fullscreenElement', {
    configurable: true,
    get: () => fsElement,
  });
  document.exitFullscreen = vi.fn(() => {
    fsElement = null;
    document.dispatchEvent(new Event('fullscreenchange'));
    return Promise.resolve();
  });
});

afterEach(() => vi.restoreAllMocks());

describe('useFullscreen', () => {
  it('enters and exits fullscreen for the target element', () => {
    const el = document.createElement('div');
    el.requestFullscreen = vi.fn(() => {
      fsElement = el;
      document.dispatchEvent(new Event('fullscreenchange'));
      return Promise.resolve();
    });
    const ref = { current: el };

    const { result } = renderHook(() => useFullscreen(ref));
    expect(result.current.isFullscreen).toBe(false);

    act(() => result.current.toggle());
    expect(el.requestFullscreen).toHaveBeenCalledTimes(1);
    expect(result.current.isFullscreen).toBe(true);

    act(() => result.current.toggle());
    expect(document.exitFullscreen).toHaveBeenCalledTimes(1);
    expect(result.current.isFullscreen).toBe(false);
  });
});
