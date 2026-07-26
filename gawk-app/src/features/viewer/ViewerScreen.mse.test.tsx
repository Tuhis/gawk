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
    arm: vi.fn(),
    setSegmentSink: vi.fn(),
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
      arm: conn.state.arm,
      setSegmentSink: conn.state.setSegmentSink,
    },
    audio: {
      present: false,
      muted: false,
      volume: 1,
      needsGesture: false,
      setMuted: () => {},
      setVolume: () => {},
      resume: () => {},
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

afterEach(() => {
  cleanup();
  conn.state.arm.mockClear();
  conn.state.setSegmentSink.mockClear();
  conn.state.status = 'watching';
  conn.state.probe = null;
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
