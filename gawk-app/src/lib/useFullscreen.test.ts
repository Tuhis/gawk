// @vitest-environment jsdom
//
// jsdom has no Fullscreen API, so we mock requestFullscreen/exitFullscreen and
// a mutable document.fullscreenElement, firing `fullscreenchange` the way the
// real API does. The hook is tiered: tier 1 (element fullscreen) where the
// API exists, tier 2 (webkitEnterFullscreen on the presentation video) /
// tier 3 (CSS pseudo-fullscreen) on gated devices. jsdom's documentElement
// has no requestFullscreen, so the gated tiers are the jsdom default; tier 1
// tests install it explicitly.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act, renderHook } from '@testing-library/react';
import { elementFullscreenAvailable, useFullscreen } from './useFullscreen';
import { ENTRY_SEEK_SLACK_S, liveEdgePolicy } from './liveEdge';

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

afterEach(() => {
  delete (document.documentElement as { requestFullscreen?: unknown }).requestFullscreen;
  vi.restoreAllMocks();
});

// The device gate: present ⇒ tier 1 is the entire feature.
function installElementFullscreen() {
  (document.documentElement as { requestFullscreen?: unknown }).requestFullscreen = vi.fn();
}

// A stub of the iPhone presentation <video> — jsdom's video element plus the
// WebKit-prefixed fullscreen API, a settable readyState/paused, play()/pause()
// mocks (jsdom's own are unimplemented), and buffered/currentTime for the
// seek-to-live path.
function fakeVideo({
  readyState = 1,
  enterThrows = false,
  paused = false,
  buffered = [] as Array<[number, number]>,
  currentTime = 0,
} = {}) {
  const video = document.createElement('video') as HTMLVideoElement & {
    webkitEnterFullscreen: () => void;
    webkitExitFullscreen: () => void;
  };
  Object.defineProperty(video, 'readyState', { configurable: true, get: () => readyState });
  Object.defineProperty(video, 'paused', { configurable: true, get: () => paused });
  Object.defineProperty(video, 'buffered', {
    configurable: true,
    get: () => ({
      length: buffered.length,
      start: (i: number) => buffered[i][0],
      end: (i: number) => buffered[i][1],
    }),
  });
  let ct = currentTime;
  Object.defineProperty(video, 'currentTime', {
    configurable: true,
    get: () => ct,
    set: (v: number) => {
      ct = v;
    },
  });
  video.play = vi.fn(() => Promise.resolve());
  video.pause = vi.fn();
  video.webkitEnterFullscreen = vi.fn(() => {
    if (enterThrows) throw new DOMException('not ready', 'InvalidStateError');
    video.dispatchEvent(new Event('webkitbeginfullscreen'));
  });
  video.webkitExitFullscreen = vi.fn(() => {
    video.dispatchEvent(new Event('webkitendfullscreen'));
  });
  return video;
}

describe('elementFullscreenAvailable', () => {
  it('reflects the presence of Element.requestFullscreen', () => {
    expect(elementFullscreenAvailable()).toBe(false); // jsdom = gated
    installElementFullscreen();
    expect(elementFullscreenAvailable()).toBe(true);
  });
});

describe('useFullscreen tier 1 (element fullscreen available)', () => {
  it('enters and exits fullscreen for the target element, exactly as before R16', () => {
    installElementFullscreen();
    const el = document.createElement('div');
    el.requestFullscreen = vi.fn(() => {
      fsElement = el;
      document.dispatchEvent(new Event('fullscreenchange'));
      return Promise.resolve();
    });
    const ref = { current: el };

    const { result } = renderHook(() => useFullscreen(ref));
    expect(result.current.isFullscreen).toBe(false);
    expect(result.current.tier).toBeNull();

    act(() => result.current.toggle());
    expect(el.requestFullscreen).toHaveBeenCalledTimes(1);
    expect(result.current.isFullscreen).toBe(true);
    expect(result.current.tier).toBe('element');

    act(() => result.current.toggle());
    expect(document.exitFullscreen).toHaveBeenCalledTimes(1);
    expect(result.current.isFullscreen).toBe(false);
    expect(result.current.tier).toBeNull();
  });

  it('never touches a presentation video when element fullscreen exists', () => {
    installElementFullscreen();
    const el = document.createElement('div');
    el.requestFullscreen = vi.fn(() => {
      fsElement = el;
      document.dispatchEvent(new Event('fullscreenchange'));
      return Promise.resolve();
    });
    const video = fakeVideo();

    const { result } = renderHook(() => useFullscreen({ current: el }, video));
    act(() => result.current.toggle());
    expect(el.requestFullscreen).toHaveBeenCalledTimes(1);
    expect(video.webkitEnterFullscreen).not.toHaveBeenCalled();
    expect(result.current.tier).toBe('element');
  });
});

describe('useFullscreen tier 2 (gated, native video fullscreen)', () => {
  it('calls webkitEnterFullscreen on a ready video and tracks the WebKit events', () => {
    const video = fakeVideo({ readyState: 1 });
    const { result } = renderHook(() => useFullscreen({ current: document.createElement('div') }, video));

    act(() => result.current.toggle());
    expect(video.webkitEnterFullscreen).toHaveBeenCalledTimes(1);
    expect(result.current.isFullscreen).toBe(true);
    expect(result.current.tier).toBe('video');

    // The system UI's own exit path fires webkitendfullscreen.
    act(() => void video.dispatchEvent(new Event('webkitendfullscreen')));
    expect(result.current.isFullscreen).toBe(false);
    expect(result.current.tier).toBeNull();
  });

  it('plays a paused video inside the gesture before entering (U4 black-screen finding)', () => {
    // A paused video is exactly a black native player; the toggle runs in the
    // user gesture, so play() succeeds even where muted autoplay was blocked
    // (e.g. iOS Low Power Mode).
    const video = fakeVideo({ paused: true });
    const { result } = renderHook(() => useFullscreen({ current: document.createElement('div') }, video));
    act(() => result.current.toggle());
    expect(video.play).toHaveBeenCalledTimes(1);
    expect(video.webkitEnterFullscreen).toHaveBeenCalledTimes(1);
    expect(result.current.tier).toBe('video');
  });

  it('does not call play() when the video is already playing', () => {
    const video = fakeVideo({ paused: false });
    const { result } = renderHook(() => useFullscreen({ current: document.createElement('div') }, video));
    act(() => result.current.toggle());
    expect(video.play).not.toHaveBeenCalled();
    expect(result.current.tier).toBe('video');
  });

  it('exits native fullscreen from the toggle', () => {
    const video = fakeVideo();
    const { result } = renderHook(() => useFullscreen({ current: document.createElement('div') }, video));
    act(() => result.current.toggle());
    expect(result.current.tier).toBe('video');
    act(() => result.current.toggle());
    expect(video.webkitExitFullscreen).toHaveBeenCalledTimes(1);
    expect(result.current.isFullscreen).toBe(false);
  });

  // The armed MSE video sits paused while its buffer follows the live edge:
  // entering must jump to the newest buffered range or the native player
  // would resume minutes behind live.
  it('seeks a lagging video to the live edge inside the entry gesture', () => {
    const video = fakeVideo({ paused: true, buffered: [[0, 30]], currentTime: 2 });
    const { result } = renderHook(() => useFullscreen({ current: document.createElement('div') }, video));
    act(() => result.current.toggle());
    // The landing leaves the shared live target lag as cushion (docs/27
    // finding 3) — appends arrive at 1×, so this IS the session's budget.
    expect(30 - video.currentTime).toBeCloseTo(liveEdgePolicy('live').targetLagS, 5);
    expect(video.play).toHaveBeenCalledTimes(1);
    expect(video.webkitEnterFullscreen).toHaveBeenCalledTimes(1);
    expect(result.current.tier).toBe('video');
  });

  it('leaves a playhead already near the target lag alone on entry', () => {
    const target = liveEdgePolicy('live').targetLagS;
    const t = 30 - target - ENTRY_SEEK_SLACK_S / 2;
    const video = fakeVideo({ paused: true, buffered: [[0, 30]], currentTime: t });
    const { result } = renderHook(() => useFullscreen({ current: document.createElement('div') }, video));
    act(() => result.current.toggle());
    expect(video.currentTime).toBeCloseTo(t, 5);
    expect(result.current.tier).toBe('video');
  });

  // A playhead pinned at the very end has no cushion at all: the first jitter
  // excursion underruns, and WebKit pauses on underrun. Step back into range.
  it('backs a playhead at the very end off to the target lag on entry', () => {
    const video = fakeVideo({ paused: true, buffered: [[0, 30]], currentTime: 29.95 });
    const { result } = renderHook(() => useFullscreen({ current: document.createElement('div') }, video));
    act(() => result.current.toggle());
    expect(30 - video.currentTime).toBeCloseTo(liveEdgePolicy('live').targetLagS, 5);
  });

  it("takes the delivery mode's target lag", () => {
    const video = fakeVideo({ paused: true, buffered: [[0, 30]], currentTime: 2 });
    const { result } = renderHook(() =>
      useFullscreen({ current: document.createElement('div') }, video, {}, 'deep'),
    );
    act(() => result.current.toggle());
    expect(30 - video.currentTime).toBeCloseTo(liveEdgePolicy('deep').targetLagS, 5);
  });

  // Playback exists only for the native player: exiting (system UI or
  // toggle) pauses the hidden video so a second decode doesn't keep burning
  // battery under the inline canvas.
  it('pauses the hidden video when the system UI exits fullscreen', () => {
    const video = fakeVideo();
    const { result } = renderHook(() => useFullscreen({ current: document.createElement('div') }, video));
    act(() => result.current.toggle());
    expect(result.current.tier).toBe('video');
    act(() => void video.dispatchEvent(new Event('webkitendfullscreen')));
    expect(video.pause).toHaveBeenCalled();
    expect(result.current.isFullscreen).toBe(false);
  });

  it('pauses the hidden video when the toggle exits fullscreen', () => {
    const video = fakeVideo();
    const { result } = renderHook(() => useFullscreen({ current: document.createElement('div') }, video));
    act(() => result.current.toggle());
    act(() => result.current.toggle());
    expect(video.pause).toHaveBeenCalled();
  });

  it('falls through to pseudo when the video is not ready (readyState < metadata)', () => {
    const video = fakeVideo({ readyState: 0 });
    const { result } = renderHook(() => useFullscreen({ current: document.createElement('div') }, video));
    act(() => result.current.toggle());
    expect(video.webkitEnterFullscreen).not.toHaveBeenCalled();
    expect(result.current.isFullscreen).toBe(true);
    expect(result.current.tier).toBe('pseudo');
  });

  it('falls through to pseudo when webkitEnterFullscreen throws', () => {
    const video = fakeVideo({ enterThrows: true });
    const { result } = renderHook(() => useFullscreen({ current: document.createElement('div') }, video));
    act(() => result.current.toggle());
    expect(video.webkitEnterFullscreen).toHaveBeenCalledTimes(1);
    expect(result.current.isFullscreen).toBe(true);
    expect(result.current.tier).toBe('pseudo');
  });
});

describe('useFullscreen tier 3 (gated, no video)', () => {
  it('toggles CSS pseudo-fullscreen — the button always does something', () => {
    const { result } = renderHook(() => useFullscreen({ current: document.createElement('div') }));
    act(() => result.current.toggle());
    expect(result.current.isFullscreen).toBe(true);
    expect(result.current.tier).toBe('pseudo');
    act(() => result.current.toggle());
    expect(result.current.isFullscreen).toBe(false);
    expect(result.current.tier).toBeNull();
  });
});

// The stall watchdog is tier-2 only: it runs between webkitbeginfullscreen and
// webkitendfullscreen and nowhere else, so the armed-but-paused element
// (docs/27 Decision 5) is never resumed behind the user's back.
describe('useFullscreen tier 2 stall watchdog', () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  // Mutable media state on top of fakeVideo's stub, for WebKit's underrun:
  // paused + ended + starved, with newer data buffered ahead.
  function starvable() {
    const video = fakeVideo({ paused: true, buffered: [[0, 10]], currentTime: 9.95 });
    const s = { paused: true, ended: false, readyState: 4, ranges: [[0, 10]] as Array<[number, number]> };
    Object.defineProperty(video, 'paused', { configurable: true, get: () => s.paused });
    Object.defineProperty(video, 'ended', { configurable: true, get: () => s.ended });
    Object.defineProperty(video, 'readyState', { configurable: true, get: () => s.readyState });
    Object.defineProperty(video, 'buffered', {
      configurable: true,
      get: () => ({ length: s.ranges.length, start: (i: number) => s.ranges[i][0], end: (i: number) => s.ranges[i][1] }),
    });
    video.play = vi.fn(() => {
      s.paused = false;
      return Promise.resolve();
    });
    const underrun = () => {
      s.paused = true;
      s.ended = true;
      s.readyState = 2;
      s.ranges = [
        [0, 10],
        [10.4, 13],
      ];
      video.dispatchEvent(new Event('pause'));
      video.dispatchEvent(new Event('ended'));
    };
    return { video, underrun };
  }

  it('re-plays an underrun while native fullscreen is up, and counts it', async () => {
    const { video, underrun } = starvable();
    const { result } = renderHook(() => useFullscreen({ current: document.createElement('div') }, video));
    act(() => result.current.toggle());
    expect(video.play).toHaveBeenCalledTimes(1); // the entry gesture's own play()

    act(() => underrun());
    await act(() => vi.advanceTimersByTimeAsync(500));
    expect(video.play).toHaveBeenCalledTimes(2);
    expect(result.current.stallRecoveries()?.recoveries).toBe(1);
  });

  it('lands recoveries at the delivery mode\'s cushion, following a mode change', async () => {
    const { video, underrun } = starvable();
    const { result, rerender } = renderHook(
      ({ mode }: { mode: 'live' | 'deep' }) =>
        useFullscreen({ current: document.createElement('div') }, video, {}, mode),
      { initialProps: { mode: 'live' as 'live' | 'deep' } },
    );
    rerender({ mode: 'deep' });
    act(() => result.current.toggle());
    video.currentTime = 9.95; // wherever the entry seek put it: before the hole
    act(() => underrun()); // newest range [10.4, 13]
    await act(() => vi.advanceTimersByTimeAsync(500));
    // Hole skip into the newest range, 1.5 s (deep) inside its end.
    expect(video.currentTime).toBeCloseTo(13 - 1.5, 6);
  });

  it('leaves the armed (not fullscreen) element paused', async () => {
    const { video, underrun } = starvable();
    const { result } = renderHook(() => useFullscreen({ current: document.createElement('div') }, video));
    act(() => underrun());
    await act(() => vi.advanceTimersByTimeAsync(2000));
    expect(video.play).not.toHaveBeenCalled();
    expect(result.current.stallRecoveries()?.recoveries ?? 0).toBe(0);
  });

  it('stands down when native fullscreen exits (system UI and toggle)', async () => {
    for (const exit of ['system', 'toggle'] as const) {
      const { video, underrun } = starvable();
      const { result } = renderHook(() => useFullscreen({ current: document.createElement('div') }, video));
      act(() => result.current.toggle());
      if (exit === 'system') act(() => void video.dispatchEvent(new Event('webkitendfullscreen')));
      else act(() => result.current.toggle());
      act(() => underrun());
      await act(() => vi.advanceTimersByTimeAsync(2000));
      expect(video.play).toHaveBeenCalledTimes(1); // only the entry's
    }
  });
});
