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
import { PLAYOUT_OFFSET_MS, getPlayoutOffsetMs, setSmoothedPlayout } from '../../transport/playout';

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

// R5 Q3: the smoothed-playout toggle — right-click menu item, persisted, and
// applied to the (main-thread, in these tests) pipeline context.
describe('ViewerScreen smoothed playout', () => {
  it('toggles via the context menu, persists, and sets the playout module', async () => {
    localStorage.removeItem('gawk:smoothed-playout');
    render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(sessions).toHaveLength(1));
    expect(getPlayoutOffsetMs()).toBe(0); // default: live-edge

    fireEvent.contextMenu(screen.getByText('connecting').closest('div')!.parentElement!);
    fireEvent.click(screen.getByText('Smooth playback'));
    expect(getPlayoutOffsetMs()).toBe(PLAYOUT_OFFSET_MS);
    expect(localStorage.getItem('gawk:smoothed-playout')).toBe('1');

    // Toggle back off: the menu item shows the checked label.
    fireEvent.contextMenu(screen.getByText('connecting').closest('div')!.parentElement!);
    fireEvent.click(screen.getByText('Smooth playback ✓'));
    expect(getPlayoutOffsetMs()).toBe(0);
    expect(localStorage.getItem('gawk:smoothed-playout')).toBe('0');
  });

  it('applies a persisted preference on mount', async () => {
    localStorage.setItem('gawk:smoothed-playout', '1');
    render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(sessions).toHaveLength(1));
    expect(getPlayoutOffsetMs()).toBe(PLAYOUT_OFFSET_MS);
    // Reset the module + storage so nothing leaks past this file's tests.
    setSmoothedPlayout(false);
    localStorage.removeItem('gawk:smoothed-playout');
  });
});
