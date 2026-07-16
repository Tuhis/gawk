// @vitest-environment jsdom
//
// Viewer status → overlay mapping (docs/10 J3). ViewerSession is mocked so the
// test can drive its callbacks and assert the cinematic state each produces.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';

const { sessions, FakeViewerSession } = vi.hoisted(() => {
  interface Cbs {
    onConnected: () => void;
    onReconnecting: (i: { attempt: number; reason: string }) => void;
    onStats: (s: unknown) => void;
    onError: (e: Error) => void;
    onEnded: () => void;
  }
  class FakeViewerSession {
    cbs: Cbs;
    constructor(_url: string, _id: string, _opts: unknown, cbs: Cbs) {
      this.cbs = cbs;
      sessions.push(this);
    }
    async start(): Promise<void> {}
    async stop(): Promise<void> {
      this.cbs.onEnded();
    }
  }
  const sessions: FakeViewerSession[] = [];
  return { sessions, FakeViewerSession };
});

vi.mock('../../transport/viewer-session', () => ({
  ViewerSession: FakeViewerSession,
  RECONNECT_MAX_ATTEMPTS: 10,
}));

import { ViewerScreen } from './ViewerScreen';
import {
  PLAYOUT_OFFSET_MS,
  getPlayoutMode,
  getPlayoutOffsetMs,
  setPlayoutMode,
} from '../../transport/playout';

beforeEach(() => {
  sessions.length = 0;
  window.location.hash = '';
});
afterEach(cleanup);

describe('ViewerScreen states', () => {
  it('shows a connecting card until connected, then live', async () => {
    render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(sessions).toHaveLength(1));
    expect(screen.getByText(/Connecting to AB2CD3/)).toBeTruthy();

    act(() => sessions[0].cbs.onConnected());
    expect(screen.queryByText(/Connecting to AB2CD3/)).toBeNull();
    expect(screen.getByText('live')).toBeTruthy();
  });

  it('shows the ended card when the broadcast ends after connecting', async () => {
    render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(sessions).toHaveLength(1));
    act(() => sessions[0].cbs.onConnected());
    act(() => sessions[0].cbs.onEnded());
    expect(screen.getByText('Broadcast ended')).toBeTruthy();
  });

  it('shows the error card on a fatal error', async () => {
    render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(sessions).toHaveLength(1));
    act(() => sessions[0].cbs.onError(new Error('boom')));
    expect(screen.getByText('Can’t reach the stream')).toBeTruthy();
    expect(screen.getByText('boom')).toBeTruthy();
  });
});

// R5 Q3 + R12 T2: the playout toggles — two mutually exclusive right-click
// menu items ("Smooth playback" = fixed 150 ms, unchanged; "Paced playback
// (adaptive)" = the R12 paced-presentation mode), persisted as one mode and
// applied to the (main-thread, in these tests) pipeline context. Since the
// default flip (user decision 2026-07-15), a fresh browser defaults to
// adaptive + interpolation; the menu is the disable path.
describe('ViewerScreen playout modes', () => {
  function cleanupPlayout() {
    setPlayoutMode('off');
    localStorage.removeItem('gawk:playout-mode');
    localStorage.removeItem('gawk:smoothed-playout');
    localStorage.removeItem('gawk:interpolation');
  }

  const openMenu = () =>
    fireEvent.contextMenu(screen.getByText('connecting').closest('div')!.parentElement!);

  it('defaults to adaptive paced playback when nothing is stored', async () => {
    cleanupPlayout();
    render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(sessions).toHaveLength(1));
    expect(getPlayoutMode()).toBe('adaptive');
    openMenu();
    expect(screen.getByText('Paced playback (adaptive) ✓')).toBeTruthy();
    cleanupPlayout();
  });

  it('toggles fixed smoothing via the context menu, persists, and sets the module', async () => {
    cleanupPlayout();
    render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(sessions).toHaveLength(1));
    expect(getPlayoutMode()).toBe('adaptive'); // the default

    openMenu();
    fireEvent.click(screen.getByText('Smooth playback'));
    expect(getPlayoutMode()).toBe('fixed');
    expect(getPlayoutOffsetMs()).toBe(PLAYOUT_OFFSET_MS);
    expect(localStorage.getItem('gawk:playout-mode')).toBe('fixed');

    // Toggle back off: live-edge, persisted so the default flip won't undo it.
    openMenu();
    fireEvent.click(screen.getByText('Smooth playback ✓'));
    expect(getPlayoutMode()).toBe('off');
    expect(getPlayoutOffsetMs()).toBe(0);
    expect(localStorage.getItem('gawk:playout-mode')).toBe('off');
    cleanupPlayout();
  });

  it('the two smoothing modes exclude each other and uncheck back to live-edge', async () => {
    cleanupPlayout();
    render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(sessions).toHaveLength(1));

    // Checking the other mode unchecks the default adaptive one.
    openMenu();
    expect(screen.queryByText('Paced playback (adaptive) ✓')).toBeTruthy();
    fireEvent.click(screen.getByText('Smooth playback'));
    expect(getPlayoutMode()).toBe('fixed');
    openMenu();
    expect(screen.queryByText('Paced playback (adaptive) ✓')).toBeNull();
    expect(screen.queryByText('Smooth playback ✓')).toBeTruthy();

    // Re-checking adaptive flips back.
    fireEvent.click(screen.getByText('Paced playback (adaptive)'));
    expect(getPlayoutMode()).toBe('adaptive');

    // And unchecking the checked one returns to live-edge.
    openMenu();
    fireEvent.click(screen.getByText('Paced playback (adaptive) ✓'));
    expect(getPlayoutMode()).toBe('off');
    cleanupPlayout();
  });

  it('applies a persisted mode on mount — an explicit off is respected', async () => {
    cleanupPlayout();
    localStorage.setItem('gawk:playout-mode', 'off');
    render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(sessions).toHaveLength(1));
    expect(getPlayoutMode()).toBe('off');
    cleanupPlayout();
  });

  it('migrates the legacy smoothed-playout preference: on → fixed, explicit off → off', async () => {
    cleanupPlayout();
    localStorage.setItem('gawk:smoothed-playout', '1');
    render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(sessions).toHaveLength(1));
    expect(getPlayoutMode()).toBe('fixed');
    expect(getPlayoutOffsetMs()).toBe(PLAYOUT_OFFSET_MS);
    cleanup();

    // A viewer who explicitly turned the old smoothing off chose live-edge;
    // the default flip must not overrule them.
    cleanupPlayout();
    localStorage.setItem('gawk:smoothed-playout', '0');
    render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(sessions).toHaveLength(2));
    expect(getPlayoutMode()).toBe('off');
    cleanupPlayout();
  });

  // R12 T4 + default flip: interpolation defaults ON, shown (checked) when
  // the pipeline reports it available, and the menu is the disable path.
  it('interpolation defaults on and toggles off through the menu', async () => {
    cleanupPlayout();
    render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(sessions).toHaveLength(1));

    openMenu();
    expect(screen.queryByText(/Frame interpolation/)).toBeNull(); // no stats yet

    act(() => sessions[0].cbs.onStats({ interpolation: 'on' }));
    expect(screen.getByText('Frame interpolation (experimental) ✓')).toBeTruthy();

    fireEvent.click(screen.getByText('Frame interpolation (experimental) ✓'));
    expect(localStorage.getItem('gawk:interpolation')).toBe('0');
    openMenu();
    expect(screen.getByText('Frame interpolation (experimental)')).toBeTruthy();
    cleanupPlayout();
  });
});

// R16 (docs/21): the device gate, the hidden presentation video, and the
// Feature Gates overlay section. jsdom has no Element Fullscreen API, so a
// bare render is a *gated* device on the main-thread pipeline (worker
// unavailable in jsdom ⇒ probe false, tier 3 only); the non-gated cases
// install document.documentElement.requestFullscreen first.
describe('ViewerScreen R16 presentation surface', () => {
  const installElementFullscreen = () => {
    (document.documentElement as { requestFullscreen?: unknown }).requestFullscreen = vi.fn();
  };
  afterEach(() => {
    delete (document.documentElement as { requestFullscreen?: unknown }).requestFullscreen;
  });

  const openStats = () => {
    fireEvent.contextMenu(screen.getByText('connecting').closest('div')!.parentElement!);
    fireEvent.click(screen.getByText('Stats'));
  };

  it('non-gated: no video element in the DOM, gate row reads element fullscreen available', async () => {
    installElementFullscreen();
    const { container } = render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(sessions).toHaveLength(1));

    expect(container.querySelector('video')).toBeNull();

    openStats();
    expect(screen.getByText('Feature Gates')).toBeTruthy();
    expect(screen.getByText('NativeVideoFullscreen').nextSibling?.textContent).toBe(
      '✗ — element fullscreen available',
    );
  });

  it('gated without worker support: no video element, gate row reads probe failed → pseudo', async () => {
    const { container } = render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(sessions).toHaveLength(1));

    expect(container.querySelector('video')).toBeNull();

    openStats();
    expect(screen.getByText('NativeVideoFullscreen').nextSibling?.textContent).toBe(
      '✗ — probe failed → pseudo',
    );
  });

  it('gated: the fullscreen button falls back to pseudo-fullscreen (never a dead tap)', async () => {
    render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(sessions).toHaveLength(1));

    fireEvent.click(screen.getByLabelText('Fullscreen'));
    expect(screen.getByLabelText('Exit fullscreen')).toBeTruthy();
    fireEvent.click(screen.getByLabelText('Exit fullscreen'));
    expect(screen.getByLabelText('Fullscreen')).toBeTruthy();
  });

  it('Copy diagnostics includes featureGates and presentationSurface', async () => {
    const writeText = vi.fn(() => Promise.resolve());
    Object.defineProperty(navigator, 'clipboard', {
      configurable: true,
      value: { writeText },
    });
    render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(sessions).toHaveLength(1));
    act(() => sessions[0].cbs.onStats({ interpolation: null }));

    openStats();
    fireEvent.click(screen.getByText('Copy diagnostics'));
    expect(writeText).toHaveBeenCalledTimes(1);
    const blob = JSON.parse((writeText.mock.calls[0] as unknown as [string])[0]);
    expect(blob.samples).toHaveLength(1);
    expect(blob.samples[0].stats.featureGates).toEqual([
      { name: 'NativeVideoFullscreen', active: false, detail: 'probe failed → pseudo' },
    ]);
    expect(blob.samples[0].stats.presentationSurface).toEqual({
      tier: null,
      armed: false,
      teedFrames: 0,
      teeErrors: 0,
    });
  });
});
