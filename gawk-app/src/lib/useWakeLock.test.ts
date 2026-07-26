// @vitest-environment jsdom
//
// Screen Wake Lock (the macOS idle-dim bug): neither surface plays a media
// element — the viewer paints decoded VideoFrames onto a canvas, the
// broadcaster's preview is a muted <video> nobody is watching — so the browser
// holds no display power-save blocker and the OS dims, then sleeps, the screen
// mid-stream. These tests pin the two rules the API's shape imposes: the UA
// drops the lock whenever the document hides and never takes it back, and
// request() is async, so the surface can go away while one is in flight.

import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { act, cleanup, renderHook } from '@testing-library/react';
import { useWakeLock } from './useWakeLock';

interface FakeSentinel {
  released: boolean;
  release: () => Promise<void>;
}

const state = {
  requests: 0,
  types: [] as string[],
  sentinels: [] as FakeSentinel[],
  rejectWith: null as Error | null,
  // When set, request() hangs until releasePending() runs — the seam for the
  // in-flight race.
  hold: false,
  pending: [] as Array<() => void>,
};

function fakeWakeLock() {
  return {
    request: (type: string): Promise<FakeSentinel> => {
      state.requests++;
      state.types.push(type);
      if (state.rejectWith) return Promise.reject(state.rejectWith);
      const sentinel: FakeSentinel = {
        released: false,
        release: () => {
          sentinel.released = true;
          return Promise.resolve();
        },
      };
      if (state.hold) {
        return new Promise((resolve) => {
          state.pending.push(() => {
            state.sentinels.push(sentinel);
            resolve(sentinel);
          });
        });
      }
      state.sentinels.push(sentinel);
      return Promise.resolve(sentinel);
    },
  };
}

function setVisibility(value: 'visible' | 'hidden'): void {
  Object.defineProperty(document, 'visibilityState', { configurable: true, value });
}

// Model the real browser: hiding the document auto-releases every sentinel,
// and nothing re-acquires them on its own.
function flipVisibility(value: 'visible' | 'hidden'): void {
  setVisibility(value);
  if (value === 'hidden') for (const s of state.sentinels) s.released = true;
  document.dispatchEvent(new Event('visibilitychange'));
}

const flush = () => act(async () => {});

beforeEach(() => {
  state.requests = 0;
  state.types = [];
  state.sentinels = [];
  state.rejectWith = null;
  state.hold = false;
  state.pending = [];
  setVisibility('visible');
  Object.defineProperty(navigator, 'wakeLock', { configurable: true, value: fakeWakeLock() });
});

afterEach(() => {
  // Explicit: vitest runs without globals here, so RTL's auto-cleanup is off
  // and a leaked mount would keep its own visibilitychange listener alive into
  // the next test (which reads as the hook stacking locks).
  cleanup();
  Reflect.deleteProperty(navigator, 'wakeLock');
  setVisibility('visible');
});

describe('useWakeLock', () => {
  it('holds a screen wake lock while enabled', async () => {
    renderHook(() => useWakeLock(true));
    await flush();
    expect(state.requests).toBe(1);
    expect(state.types).toEqual(['screen']);
    expect(state.sentinels[0].released).toBe(false);
  });

  it('requests nothing while disabled', async () => {
    renderHook(() => useWakeLock(false));
    await flush();
    expect(state.requests).toBe(0);
  });

  it('releases when it stops being enabled, and re-acquires when it resumes', async () => {
    const { rerender } = renderHook(({ on }) => useWakeLock(on), {
      initialProps: { on: true },
    });
    await flush();
    expect(state.requests).toBe(1);

    rerender({ on: false });
    await flush();
    expect(state.sentinels[0].released).toBe(true);

    rerender({ on: true });
    await flush();
    expect(state.requests).toBe(2);
    expect(state.sentinels[1].released).toBe(false);
  });

  it('releases on unmount', async () => {
    const { unmount } = renderHook(() => useWakeLock(true));
    await flush();
    unmount();
    await flush();
    expect(state.sentinels[0].released).toBe(true);
  });

  // The regression this hook exists to prevent: the UA releases the lock on
  // hide and does NOT restore it, so without a visibilitychange re-request the
  // first tab switch loses the lock for the rest of the stream — and the
  // display starts dimming again with the stream still running.
  it('re-acquires the lock the browser dropped when the document was hidden', async () => {
    renderHook(() => useWakeLock(true));
    await flush();
    expect(state.requests).toBe(1);

    await act(async () => flipVisibility('hidden'));
    expect(state.sentinels[0].released).toBe(true);
    expect(state.requests).toBe(1);

    await act(async () => flipVisibility('visible'));
    await flush();
    expect(state.requests).toBe(2);
    expect(state.sentinels[1].released).toBe(false);
  });

  it('does not request while the document is hidden, and takes the lock once it shows', async () => {
    setVisibility('hidden');
    renderHook(() => useWakeLock(true));
    await flush();
    expect(state.requests).toBe(0);

    await act(async () => flipVisibility('visible'));
    await flush();
    expect(state.requests).toBe(1);
  });

  // request() rejects on battery saver and other UA discretion. That is a
  // degraded stream, never a broken screen: no throw, and the next chance to
  // ask is still taken.
  it('survives a refused request and retries on the next visibility change', async () => {
    state.rejectWith = new Error('NotAllowedError');
    renderHook(() => useWakeLock(true));
    await flush();
    expect(state.requests).toBe(1);
    expect(state.sentinels).toHaveLength(0);

    state.rejectWith = null;
    await act(async () => flipVisibility('hidden'));
    await act(async () => flipVisibility('visible'));
    await flush();
    expect(state.requests).toBe(2);
    expect(state.sentinels[0].released).toBe(false);
  });

  it('is inert where the API does not exist', async () => {
    Reflect.deleteProperty(navigator, 'wakeLock');
    const { unmount } = renderHook(() => useWakeLock(true));
    await flush();
    unmount();
    await flush();
    expect(state.requests).toBe(0);
  });

  // request() is async: the stream can end (or the tab hide) before the
  // sentinel arrives. A lock that lands after its owner is gone must not keep
  // the display awake for the rest of the browsing session.
  it('releases a lock that arrives after it was already torn down', async () => {
    state.hold = true;
    const { unmount } = renderHook(() => useWakeLock(true));
    await flush();
    expect(state.requests).toBe(1);
    expect(state.sentinels).toHaveLength(0);

    unmount();
    await act(async () => {
      for (const resolve of state.pending) resolve();
    });
    expect(state.sentinels).toHaveLength(1);
    expect(state.sentinels[0].released).toBe(true);
  });

  it('does not stack locks when re-enabled while a request is in flight', async () => {
    state.hold = true;
    renderHook(() => useWakeLock(true));
    await flush();
    await act(async () => flipVisibility('visible'));
    await flush();
    expect(state.requests).toBe(1);
  });
});
