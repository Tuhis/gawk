// @vitest-environment jsdom
//
// The hidden-tab watchdog on the real page (docs/44 §4.8 revision
// 2026-09-06): five minutes hidden with no frame encoded ends the broadcast
// and explains it on the card; frames still flowing while hidden do not.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import type { BroadcastCallbacks, BroadcastStats } from '../../transport/broadcaster';

const { created, scripts } = vi.hoisted(() => {
  interface FakeSession {
    callbacks: BroadcastCallbacks;
    stopped: number;
    start(): Promise<void>;
    stop(): Promise<void>;
    setLadder(): void;
    setEncoderSettings(): void;
  }
  const created: FakeSession[] = [];
  const scripts: Array<(cbs: BroadcastCallbacks) => Promise<void>> = [];
  return { created, scripts };
});

vi.mock('./workerBroadcastSession', () => ({
  createBroadcastSession: async (_config: unknown, _url: string, _opts: unknown, callbacks: BroadcastCallbacks) => {
    const script = scripts.shift();
    if (!script) throw new Error('test bug: no session script queued');
    const session = {
      callbacks,
      stopped: 0,
      start: () => script(callbacks),
      stop: async () => {
        session.stopped++;
        callbacks.onEnded();
      },
      setLadder: () => {},
      setEncoderSettings: () => {},
    };
    created.push(session);
    return session;
  },
}));

import { BroadcasterScreen } from './BroadcasterScreen';
import { acceptCurrentTerms } from '../terms/acceptance';
import { BACKGROUND_STOP_MS, BACKGROUND_STOP_NOTE } from './backgroundWatchdog';

const fakeStream = { getTracks: () => [], getVideoTracks: () => [] } as unknown as MediaStream;
const stats = (encodedFrames: number) => ({ encodedFrames, audioState: 'off' }) as unknown as BroadcastStats;

function setHidden(hidden: boolean) {
  Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => (hidden ? 'hidden' : 'visible') });
  Object.defineProperty(document, 'hidden', { configurable: true, get: () => hidden });
}

async function goLive() {
  scripts.push(async (cbs) => {
    cbs.onBroadcastId?.('AB2CD3');
    cbs.onSourceStream(fakeStream);
    cbs.onStats(stats(100));
  });
  render(<BroadcasterScreen />);
  fireEvent.click(screen.getByRole('button', { name: /start a stream/i }));
  await waitFor(() => expect(screen.getByText('LIVE')).toBeTruthy());
  return created[0];
}

beforeEach(() => {
  created.length = 0;
  scripts.length = 0;
  window.__GAWK_CONFIG__ = { requirePublishSecret: false };
  localStorage.clear();
  sessionStorage.clear();
  acceptCurrentTerms();
  setHidden(false);
  vi.useFakeTimers({ shouldAdvanceTime: true });
  vi.setSystemTime(new Date('2026-09-06T12:00:00Z'));
  Object.defineProperty(HTMLMediaElement.prototype, 'srcObject', { configurable: true, get: () => null, set: () => {} });
  HTMLMediaElement.prototype.play = () => Promise.resolve();
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  setHidden(false);
  delete window.__GAWK_CONFIG__;
});

describe('BroadcasterScreen hidden-tab watchdog', () => {
  it('stops the broadcast after five minutes hidden with no frame encoded, and says so on the card', async () => {
    const session = await goLive();
    setHidden(true);
    act(() => session.callbacks.onStats(stats(100))); // the silent span starts
    vi.setSystemTime(new Date('2026-09-06T12:04:00Z'));
    act(() => session.callbacks.onStats(stats(100)));
    expect(session.stopped).toBe(0);
    expect(screen.getByText('LIVE')).toBeTruthy();

    vi.setSystemTime(new Date(Date.parse('2026-09-06T12:00:00Z') + BACKGROUND_STOP_MS + 1000));
    await act(async () => {
      session.callbacks.onStats(stats(100));
    });
    await waitFor(() => expect(session.stopped).toBe(1));
    // Back on the pre-start card, with the reason.
    await waitFor(() => expect(screen.getByRole('button', { name: /start a stream/i })).toBeTruthy());
    expect(screen.getByTestId('background-stop-note').textContent).toBe(BACKGROUND_STOP_NOTE);
  });

  it('a hidden tab whose frames keep flowing is left alone', async () => {
    const session = await goLive();
    setHidden(true);
    act(() => session.callbacks.onStats(stats(100)));
    for (let m = 1; m <= 12; m++) {
      vi.setSystemTime(new Date(Date.parse('2026-09-06T12:00:00Z') + m * 60_000));
      act(() => session.callbacks.onStats(stats(100 + m * 60))); // ~1 fps in the background
    }
    expect(session.stopped).toBe(0);
    expect(screen.getByText('LIVE')).toBeTruthy();
  });

  it('a visible static screen is never stopped', async () => {
    const session = await goLive();
    for (let m = 1; m <= 12; m++) {
      vi.setSystemTime(new Date(Date.parse('2026-09-06T12:00:00Z') + m * 60_000));
      act(() => session.callbacks.onStats(stats(100)));
    }
    expect(session.stopped).toBe(0);
  });

  it('the note clears on the next start', async () => {
    const session = await goLive();
    setHidden(true);
    act(() => session.callbacks.onStats(stats(100)));
    vi.setSystemTime(new Date(Date.parse('2026-09-06T12:00:00Z') + BACKGROUND_STOP_MS + 1000));
    await act(async () => {
      session.callbacks.onStats(stats(100));
    });
    await waitFor(() => expect(screen.getByTestId('background-stop-note')).toBeTruthy());
    setHidden(false);
    scripts.push(async (cbs) => {
      cbs.onBroadcastId?.('AB2CD3');
      cbs.onSourceStream(fakeStream);
    });
    fireEvent.click(screen.getByRole('button', { name: /start a stream/i }));
    await waitFor(() => expect(screen.getByText('LIVE')).toBeTruthy());
    expect(screen.queryByTestId('background-stop-note')).toBeNull();
  });
});
