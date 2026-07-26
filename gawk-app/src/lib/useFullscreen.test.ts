// @vitest-environment jsdom
//
// jsdom has no Fullscreen API, so we mock requestFullscreen/exitFullscreen and
// a mutable document.fullscreenElement, firing `fullscreenchange` the way the
// real API does. R16 (docs/21): the hook is tiered — tier 1 (element
// fullscreen, unchanged) where the API exists, tier 2 (webkitEnterFullscreen
// on the presentation video) / tier 3 (CSS pseudo-fullscreen) on gated
// devices. jsdom's documentElement has no requestFullscreen, so the gated
// tiers are the jsdom default; tier 1 tests install it explicitly.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act, renderHook } from '@testing-library/react';
import { elementFullscreenAvailable, useFullscreen } from './useFullscreen';

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

// The R16 device gate: present ⇒ tier 1 is the entire feature.
function installElementFullscreen() {
  (document.documentElement as { requestFullscreen?: unknown }).requestFullscreen = vi.fn();
}

// A stub of the iPhone presentation <video> — jsdom's video element plus the
// WebKit-prefixed fullscreen API, a settable readyState/paused, play()/pause()
// mocks (jsdom's own are unimplemented), and R22's buffered/currentTime for
// the seek-to-live path.
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
    // A paused MediaStream video is exactly a black native player; the toggle
    // runs in the user gesture, so play() succeeds even where muted autoplay
    // was blocked (e.g. iOS Low Power Mode).
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

  // R22 (docs/27 Decision 5): the armed MSE video sits paused while its
  // buffer follows the live edge — entering must jump to the newest buffered
  // range or the native player would resume minutes behind live.
  it('seeks a lagging video to the live edge inside the entry gesture', () => {
    const video = fakeVideo({ paused: true, buffered: [[0, 30]], currentTime: 2 });
    const { result } = renderHook(() => useFullscreen({ current: document.createElement('div') }, video));
    act(() => result.current.toggle());
    expect(video.currentTime).toBeCloseTo(29.9, 5);
    expect(video.play).toHaveBeenCalledTimes(1);
    expect(video.webkitEnterFullscreen).toHaveBeenCalledTimes(1);
    expect(result.current.tier).toBe('video');
  });

  it('leaves a near-live playhead alone on entry', () => {
    const video = fakeVideo({ paused: true, buffered: [[0, 30]], currentTime: 29.8 });
    const { result } = renderHook(() => useFullscreen({ current: document.createElement('div') }, video));
    act(() => result.current.toggle());
    expect(video.currentTime).toBeCloseTo(29.8, 5);
    expect(result.current.tier).toBe('video');
  });

  // R22: playback exists only for the native player — exiting (system UI or
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
