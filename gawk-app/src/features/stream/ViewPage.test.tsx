// @vitest-environment jsdom
//
// ViewPage auto-join wiring. ViewerSession is mocked; each construction is
// recorded so the tests can assert exactly when the page dials out.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';

const { sessions, FakeViewerSession } = vi.hoisted(() => {
  interface FakeCbs {
    onConnected: () => void;
    onEnded: () => void;
  }
  class FakeViewerSession {
    broadcastId: string;
    cbs: FakeCbs;
    stopped = false;

    constructor(_url: string, broadcastId: string, _opts: unknown, cbs: FakeCbs) {
      this.broadcastId = broadcastId;
      this.cbs = cbs;
      sessions.push(this);
    }

    async start(): Promise<void> {
      this.cbs.onConnected();
    }

    async stop(): Promise<void> {
      if (this.stopped) return;
      this.stopped = true;
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

import { ViewPage } from './ViewPage';

beforeEach(() => {
  sessions.length = 0;
  window.location.hash = '';
});

afterEach(() => {
  cleanup();
});

describe('ViewPage auto-join', () => {
  it('auto-joins the broadcast ID from the URL hash on mount', async () => {
    window.location.hash = '#/view/AB2CD3';
    render(<ViewPage />);
    await waitFor(() => expect(sessions).toHaveLength(1));
    expect(sessions[0].broadcastId).toBe('AB2CD3');
  });

  it('does not rejoin the stale hash ID when typing a new code after stop', async () => {
    window.location.hash = '#/view/AB2CD3';
    render(<ViewPage />);
    await waitFor(() => expect(sessions).toHaveLength(1));

    // Stop watching; the hash still carries the old ID.
    fireEvent.click(screen.getByText('Stop'));
    await waitFor(() => expect(screen.getByText('Watch')).toBeTruthy());

    // Typing a different code must not silently rejoin AB2CD3.
    fireEvent.change(screen.getByPlaceholderText('Enter 6-char code'), {
      target: { value: 'X' },
    });
    await new Promise((r) => setTimeout(r, 50));
    expect(sessions).toHaveLength(1);
  });

  it('does not auto-join without an ID in the hash', async () => {
    window.location.hash = '#/view';
    render(<ViewPage />);
    await new Promise((r) => setTimeout(r, 50));
    expect(sessions).toHaveLength(0);
  });
});
