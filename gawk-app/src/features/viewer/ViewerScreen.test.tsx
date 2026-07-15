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
// applied to the (main-thread, in these tests) pipeline context.
describe('ViewerScreen playout modes', () => {
  function cleanupPlayout() {
    setPlayoutMode('off');
    localStorage.removeItem('gawk:playout-mode');
    localStorage.removeItem('gawk:smoothed-playout');
  }

  const openMenu = () =>
    fireEvent.contextMenu(screen.getByText('connecting').closest('div')!.parentElement!);

  it('toggles fixed smoothing via the context menu, persists, and sets the module', async () => {
    cleanupPlayout();
    render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(sessions).toHaveLength(1));
    expect(getPlayoutOffsetMs()).toBe(0); // default: live-edge

    openMenu();
    fireEvent.click(screen.getByText('Smooth playback'));
    expect(getPlayoutMode()).toBe('fixed');
    expect(getPlayoutOffsetMs()).toBe(PLAYOUT_OFFSET_MS);
    expect(localStorage.getItem('gawk:playout-mode')).toBe('fixed');

    // Toggle back off: the menu item shows the checked label.
    openMenu();
    fireEvent.click(screen.getByText('Smooth playback ✓'));
    expect(getPlayoutMode()).toBe('off');
    expect(getPlayoutOffsetMs()).toBe(0);
    expect(localStorage.getItem('gawk:playout-mode')).toBe('off');
    cleanupPlayout();
  });

  it('toggles adaptive paced playback and the two modes exclude each other', async () => {
    cleanupPlayout();
    render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(sessions).toHaveLength(1));

    openMenu();
    fireEvent.click(screen.getByText('Paced playback (adaptive)'));
    expect(getPlayoutMode()).toBe('adaptive');
    expect(localStorage.getItem('gawk:playout-mode')).toBe('adaptive');

    // Checking the other mode unchecks this one.
    openMenu();
    expect(screen.queryByText('Paced playback (adaptive) ✓')).toBeTruthy();
    fireEvent.click(screen.getByText('Smooth playback'));
    expect(getPlayoutMode()).toBe('fixed');
    openMenu();
    expect(screen.queryByText('Paced playback (adaptive) ✓')).toBeNull();
    expect(screen.queryByText('Smooth playback ✓')).toBeTruthy();

    // And unchecking the checked one returns to live-edge.
    fireEvent.click(screen.getByText('Smooth playback ✓'));
    expect(getPlayoutMode()).toBe('off');
    cleanupPlayout();
  });

  it('applies a persisted mode on mount', async () => {
    cleanupPlayout();
    localStorage.setItem('gawk:playout-mode', 'adaptive');
    render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(sessions).toHaveLength(1));
    expect(getPlayoutMode()).toBe('adaptive');
    cleanupPlayout();
  });

  it('migrates the legacy smoothed-playout preference to fixed mode', async () => {
    cleanupPlayout();
    localStorage.setItem('gawk:smoothed-playout', '1');
    render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(sessions).toHaveLength(1));
    expect(getPlayoutMode()).toBe('fixed');
    expect(getPlayoutOffsetMs()).toBe(PLAYOUT_OFFSET_MS);
    cleanupPlayout();
  });
});
