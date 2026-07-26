// @vitest-environment jsdom
//
// R22 (docs/27): the gated arm/video lifecycle that ViewerScreen.test.tsx
// cannot reach — jsdom always falls back to the main-thread pipeline, whose
// probe verdict is false by design, so the armed states below are driven
// through a mocked useViewerConnection instead (the seam the screen actually
// consumes). jsdom has no Element Fullscreen API, so a bare render is gated.

import { afterEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, waitFor } from '@testing-library/react';
import type { MseProbeResult } from './msePresentation';

const conn = vi.hoisted(() => ({
  state: {
    status: 'watching' as string,
    probe: null as MseProbeResult | null,
    audioProbe: null as MseProbeResult | null,
    audioPresent: false,
    muted: false,
    arm: vi.fn(),
    setSegmentSink: vi.fn(),
    setSuppressed: vi.fn(),
  },
}));

vi.mock('./useViewerConnection', () => ({
  useViewerConnection: () => ({
    status: conn.state.status,
    stats: null,
    codec: 'avc1.42C00D',
    error: null,
    errorKind: null,
    errorFatal: false,
    retryNote: null,
    presentation: {
      probe: conn.state.probe,
      audioProbe: conn.state.audioProbe,
      arm: conn.state.arm,
      setSegmentSink: conn.state.setSegmentSink,
    },
    audio: {
      present: conn.state.audioPresent,
      muted: conn.state.muted,
      volume: 1,
      needsGesture: false,
      setMuted: () => {},
      setVolume: () => {},
      resume: () => {},
      setSuppressed: conn.state.setSuppressed,
    },
  }),
}));

import { ViewerScreen } from './ViewerScreen';

const SUPPORTED: MseProbeResult = {
  supported: true,
  mime: 'video/mp4; codecs="avc1.42C00D"',
  reason: 'MSE available',
};
const UNSUPPORTED: MseProbeResult = {
  supported: false,
  mime: null,
  reason: 'codec vp8 is not H.264',
};

const AUDIO_SUPPORTED: MseProbeResult = {
  supported: true,
  mime: 'audio/mp4; codecs="opus"',
  reason: 'Opus in MP4 available',
};
const AUDIO_UNSUPPORTED: MseProbeResult = {
  supported: false,
  mime: null,
  reason: 'unsupported: audio/mp4; codecs="opus"',
};

afterEach(() => {
  cleanup();
  conn.state.arm.mockClear();
  conn.state.setSegmentSink.mockClear();
  conn.state.setSuppressed.mockClear();
  conn.state.status = 'watching';
  conn.state.probe = null;
  conn.state.audioProbe = null;
  conn.state.audioPresent = false;
  conn.state.muted = false;
});

describe('ViewerScreen R22 arm lifecycle (gated, mocked connection)', () => {
  it('arms at watching once the probe passes, mounting the hidden paused video', async () => {
    conn.state.probe = SUPPORTED;
    const { container } = render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(conn.state.arm).toHaveBeenCalled());
    expect(conn.state.setSegmentSink).toHaveBeenCalled();
    const video = container.querySelector('video');
    expect(video).not.toBeNull();
    // Decision 5: loaded-but-paused — no autoplay attribute.
    expect(video!.hasAttribute('autoplay')).toBe(false);
    expect(video!.muted !== undefined).toBe(true);
  });

  it('never arms before watching', () => {
    conn.state.probe = SUPPORTED;
    conn.state.status = 'connecting';
    const { container } = render(<ViewerScreen broadcastId="AB2CD3" />);
    expect(conn.state.arm).not.toHaveBeenCalled();
    expect(container.querySelector('video')).toBeNull();
  });

  it('never arms when the probe fails (VP codec ⇒ tier-3 pseudo)', () => {
    conn.state.probe = UNSUPPORTED;
    const { container } = render(<ViewerScreen broadcastId="AB2CD3" />);
    expect(conn.state.arm).not.toHaveBeenCalled();
    expect(container.querySelector('video')).toBeNull();
  });

  // docs/27 finding 7: the worker muxer emits its init segment exactly ONCE
  // per session and survives reconnects, so any window with no sink registered
  // costs the presentation every segment after it — permanently and silently
  // (the presenter drops media it has no init for). A reconnect flips status
  // away from 'watching', which is exactly such a window, so the sink must
  // outlive it: it is cleared with the presenter, on unmount.
  it('keeps the segment sink registered across a reconnect', async () => {
    conn.state.probe = SUPPORTED;
    const { rerender, unmount } = render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(conn.state.setSegmentSink).toHaveBeenCalled());
    expect(conn.state.setSegmentSink.mock.calls.at(-1)?.[0]).toBeTypeOf('function');

    conn.state.status = 'reconnecting';
    rerender(<ViewerScreen broadcastId="AB2CD3" />);
    expect(conn.state.setSegmentSink.mock.calls.at(-1)?.[0]).toBeTypeOf('function');

    conn.state.status = 'watching';
    rerender(<ViewerScreen broadcastId="AB2CD3" />);
    expect(conn.state.setSegmentSink.mock.calls.at(-1)?.[0]).toBeTypeOf('function');

    unmount();
    expect(conn.state.setSegmentSink).toHaveBeenLastCalledWith(null);
  });

  // A broadcaster restart can change the codec mid-view (R13 pin, or a
  // different broadcaster reclaiming the ID). If the new codec probes false,
  // the armed surface is stale: keeping the ready <video> mounted would let
  // the next fullscreen tap native-present frozen content. The video must
  // unmount so tier 2 disappears and the tap falls to pseudo.
  it('unmounts the hidden video when the probe flips unsupported mid-view', async () => {
    conn.state.probe = SUPPORTED;
    const { container, rerender } = render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(container.querySelector('video')).not.toBeNull());

    conn.state.probe = UNSUPPORTED;
    rerender(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(container.querySelector('video')).toBeNull());
  });
});

// R22 audio (docs/27 finding 2): only one output may be audible. The muxed
// track plays through the native player, which is independently clocked from the
// inline AudioWorklet sink — both at once is an echo.
describe('ViewerScreen R22 audio handoff (gated, mocked connection)', () => {
  async function armed() {
    conn.state.probe = SUPPORTED;
    const rendered = render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(rendered.container.querySelector('video')).not.toBeNull());
    return rendered.container.querySelector('video')!;
  }

  it('keeps the hidden video muted while the audio lane is not muxed', async () => {
    conn.state.audioProbe = AUDIO_UNSUPPORTED;
    const video = await armed();
    // Nothing to hear from the native player, so it must not be unmuted (and the
    // inline sink must keep playing — see the suppression test below).
    expect(video.muted).toBe(true);
  });

  it('unmutes the hidden video once the audio track is muxed, honoring the mute toggle', async () => {
    conn.state.audioProbe = AUDIO_SUPPORTED;
    const video = await armed();
    expect(video.muted).toBe(false);

    cleanup();
    conn.state.muted = true;
    const mutedVideo = await armed();
    // The viewer's own mute preference travels into the native player.
    expect(mutedVideo.muted).toBe(true);
  });

  it('silences the inline sink for the native player, and only when it has audio', async () => {
    conn.state.audioProbe = AUDIO_SUPPORTED;
    const video = await armed();
    // webkitEnterFullscreen is absent in jsdom; drive the hook's tier-2 path by
    // giving the element the API and enough readiness to be chosen.
    (video as HTMLVideoElement & { webkitEnterFullscreen?: () => void }).webkitEnterFullscreen =
      vi.fn();
    Object.defineProperty(video, 'readyState', { configurable: true, get: () => 1 });
    video.play = vi.fn().mockResolvedValue(undefined) as unknown as typeof video.play;

    // The fullscreen control is the only tier-2 entry point on a gated device.
    const button = document.querySelector('[aria-label="Fullscreen"]') as HTMLButtonElement;
    button.click();
    expect(conn.state.setSuppressed).toHaveBeenCalledWith(true);

    // Leaving hands the audio back.
    video.dispatchEvent(new Event('webkitendfullscreen'));
    expect(conn.state.setSuppressed).toHaveBeenLastCalledWith(false);
  });

  it('never silences the inline sink when the native player has no audio track', async () => {
    conn.state.audioProbe = AUDIO_UNSUPPORTED;
    const video = await armed();
    (video as HTMLVideoElement & { webkitEnterFullscreen?: () => void }).webkitEnterFullscreen =
      vi.fn();
    Object.defineProperty(video, 'readyState', { configurable: true, get: () => 1 });
    video.play = vi.fn().mockResolvedValue(undefined) as unknown as typeof video.play;

    (document.querySelector('[aria-label="Fullscreen"]') as HTMLButtonElement).click();
    expect(conn.state.setSuppressed).not.toHaveBeenCalledWith(true);
  });
});
