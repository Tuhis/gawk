// @vitest-environment jsdom
//
// BroadcasterScreen start-failure states. createBroadcastSession is mocked so
// each test scripts the session's callback/rejection sequence; the sessions
// contract (workerBroadcastSession/BroadcastPipeline) is that a start()
// rejection fires NO onEnded — the caller owns the error surface — so the
// screen must fully reset the stage itself, including after onSourceStream
// already flipped it to LIVE.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import type { BroadcastCallbacks } from '../../transport/broadcaster';

const { created, scripts } = vi.hoisted(() => {
  interface FakeSession {
    callbacks: BroadcastCallbacks;
    broadcastId?: string;
    start(): Promise<void>;
    stop(): Promise<void>;
    setLadder(): void;
    setEncoderSettings(): void;
  }
  const created: FakeSession[] = [];
  // One script per createBroadcastSession call, consumed in order; a script
  // drives the callbacks and resolves/rejects like the real session's start().
  const scripts: Array<(cbs: BroadcastCallbacks) => Promise<void>> = [];
  return { created, scripts };
});

vi.mock('./workerBroadcastSession', () => ({
  createBroadcastSession: async (
    _config: unknown,
    _url: string,
    _opts: unknown,
    callbacks: BroadcastCallbacks,
    broadcastId?: string,
  ) => {
    const script = scripts.shift();
    if (!script) throw new Error('test bug: no session script queued');
    const session = {
      callbacks,
      broadcastId,
      start: () => script(callbacks),
      stop: async () => callbacks.onEnded(),
      setLadder: () => {},
      setEncoderSettings: () => {},
    };
    created.push(session);
    return session;
  },
}));

import { BroadcasterScreen } from './BroadcasterScreen';
import { BroadcastStartError } from '../../transport/broadcaster';

const fakeStream = { getTracks: () => [] } as unknown as MediaStream;

beforeEach(() => {
  created.length = 0;
  scripts.length = 0;
  // Skip the publish-secret modal (vitest runs with import.meta.env.DEV, and
  // requiresPublishSecret() falls back to isDevEnvironment()).
  window.__GAWK_CONFIG__ = { requirePublishSecret: false };
  // jsdom's HTMLMediaElement implements neither srcObject nor play(); the
  // preview effect touches both.
  Object.defineProperty(HTMLMediaElement.prototype, 'srcObject', {
    configurable: true,
    get: () => null,
    set: () => {},
  });
  HTMLMediaElement.prototype.play = () => Promise.resolve();
});

afterEach(() => {
  cleanup();
  delete window.__GAWK_CONFIG__;
});

function startBroadcast() {
  fireEvent.click(screen.getByRole('button', { name: /start a stream/i }));
}

describe('BroadcasterScreen start failure after capture', () => {
  // The dark-screen-claiming-LIVE bug: onSourceStream flips the stage to
  // LIVE, then start() rejects (phase 'capture' — e.g. the worker-side frame
  // pump failed after the share picker). No onEnded follows by contract, so
  // the screen itself must drop the stage and show the error card.
  it('mint path: shows the error card, not a dead LIVE stage', async () => {
    scripts.push(async (cbs) => {
      cbs.onSourceStream(fakeStream);
      throw new BroadcastStartError('capture', new Error('frame pump failed'));
    });

    render(<BroadcasterScreen />);
    startBroadcast();

    await waitFor(() => expect(screen.getByText('Couldn’t start')).toBeTruthy());
    expect(screen.getByText('frame pump failed')).toBeTruthy();
    expect(screen.getByText('Try again')).toBeTruthy();
    expect(screen.queryByText('LIVE')).toBeNull();
  });

  it('reclaim path: shows the error card, not a dead LIVE stage', async () => {
    // First broadcast succeeds and announces an ID, so the next start
    // reclaims (activeId set) and its failure lands in the reclaim catch.
    scripts.push(async (cbs) => {
      cbs.onBroadcastId?.('AB2CD3');
      cbs.onSourceStream(fakeStream);
    });

    render(<BroadcasterScreen />);
    startBroadcast();
    await waitFor(() => expect(screen.getByText('LIVE')).toBeTruthy());

    fireEvent.click(screen.getByRole('button', { name: /stop broadcast/i }));
    await waitFor(() =>
      expect(screen.getByRole('button', { name: /start a stream/i })).toBeTruthy(),
    );

    scripts.push(async (cbs) => {
      cbs.onSourceStream(fakeStream);
      throw new BroadcastStartError('capture', new Error('frame pump failed'));
    });
    startBroadcast();

    await waitFor(() => expect(screen.getByText('Couldn’t start')).toBeTruthy());
    expect(screen.queryByText('LIVE')).toBeNull();
  });
});
