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
import { BroadcastStartError, type BroadcastStats } from '../../transport/broadcaster';
import { acceptCurrentTerms } from '../terms/acceptance';
import { BUNDLED_TERMS_VERSION } from '../../config';
import {
  AUDIO_NOTE,
  AUDIO_SETTINGS,
  AUDIO_TIP,
  HINT_AUDIO_MISSING_KEY,
  HINT_WINDOW_SHARE_KEY,
  WHOLE_SCREEN_TIP,
  WINDOW_NOTE,
} from './captureGuidance';

const fakeStream = { getTracks: () => [] } as unknown as MediaStream;

// R24: a display stream whose single video track reports a capture surface, so
// the window-share note (CG3) can be exercised. Fully shaped for the
// optional-chained read in BroadcasterScreen.
function streamWithSurface(surface?: string): MediaStream {
  const track = { getSettings: () => ({ displaySurface: surface }) } as unknown as MediaStreamTrack;
  return {
    getTracks: () => [track],
    getVideoTracks: () => [track],
  } as unknown as MediaStream;
}

// R24: make audioLaneSupported() report true (jsdom lacks both globals, so the
// default is Firefox-like / unsupported).
function enableChromiumAudio(): void {
  (globalThis as Record<string, unknown>).AudioEncoder = function () {};
  (globalThis as Record<string, unknown>).MediaStreamTrackProcessor = function () {};
}
function disableChromiumAudio(): void {
  delete (globalThis as Record<string, unknown>).AudioEncoder;
  delete (globalThis as Record<string, unknown>).MediaStreamTrackProcessor;
}

// R24: drive a session straight to a live broadcast with the given capture
// surface + audio state, so the reactive notes can be asserted.
function goLive(surface: string, audioState: BroadcastStats['audioState']) {
  scripts.push(async (cbs) => {
    cbs.onSourceStream(streamWithSurface(surface));
    cbs.onStats({ audioState } as BroadcastStats);
  });
  render(<BroadcasterScreen />);
  startBroadcast();
}

beforeEach(() => {
  created.length = 0;
  scripts.length = 0;
  // Skip the publish-secret modal (vitest runs with import.meta.env.DEV, and
  // requiresPublishSecret() falls back to isDevEnvironment()).
  window.__GAWK_CONFIG__ = { requirePublishSecret: false };
  // R23: pre-accept the terms so these behaviour tests exercise the start
  // flow, not the gate. The gate has its own describe below (which clears it).
  localStorage.clear();
  acceptCurrentTerms();
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
  localStorage.clear();
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

// R23 (docs/29 D5): the terms acknowledgment gate. It sits ahead of connect —
// nothing touches the transport until the broadcaster has agreed (once per
// terms version). Viewers are never gated (covered elsewhere); this is the
// broadcaster gate.
describe('BroadcasterScreen terms acknowledgment gate', () => {
  beforeEach(() => {
    // Undo the outer pre-accept so these tests see the gate.
    localStorage.clear();
  });

  it('first start shows the terms modal and connects nothing', () => {
    render(<BroadcasterScreen />);
    startBroadcast();
    expect(screen.getByText('Before you broadcast')).toBeTruthy();
    // The strong guarantee: no session was created (no connect happened).
    expect(created.length).toBe(0);
  });

  it('Agree persists acceptance and proceeds to start', async () => {
    scripts.push(async (cbs) => {
      cbs.onBroadcastId?.('AB2CD3');
      cbs.onSourceStream(fakeStream);
    });
    render(<BroadcasterScreen />);
    startBroadcast();
    fireEvent.click(screen.getByRole('button', { name: /agree/i }));
    await waitFor(() => expect(screen.getByText('LIVE')).toBeTruthy());
    expect(created.length).toBe(1);
    expect(localStorage.getItem('gawk:terms-accepted')).toBe(BUNDLED_TERMS_VERSION);
  });

  it('Cancel connects nothing and stays idle', () => {
    render(<BroadcasterScreen />);
    startBroadcast();
    fireEvent.click(screen.getByRole('button', { name: /cancel/i }));
    expect(screen.queryByText('Before you broadcast')).toBeNull();
    expect(created.length).toBe(0);
    expect(screen.getByRole('button', { name: /start a stream/i })).toBeTruthy();
  });

  it('does not re-prompt once the current version is accepted', async () => {
    acceptCurrentTerms();
    scripts.push(async (cbs) => {
      cbs.onSourceStream(fakeStream);
    });
    render(<BroadcasterScreen />);
    startBroadcast();
    await waitFor(() => expect(screen.getByText('LIVE')).toBeTruthy());
    expect(screen.queryByText('Before you broadcast')).toBeNull();
  });

  it('re-prompts when the terms version has been bumped', () => {
    acceptCurrentTerms(); // accepts the bundled version
    window.__GAWK_CONFIG__ = { requirePublishSecret: false, termsVersion: '2099-01-01' };
    render(<BroadcasterScreen />);
    startBroadcast();
    expect(screen.getByText('Before you broadcast')).toBeTruthy();
    expect(created.length).toBe(0);
  });
});

// R24 (docs/30): browser-aware capture & audio guidance. The two hard UX
// constraints are the load-bearing assertions here — it must not add a step to
// Start, and it must not fire on a healthy broadcast.
describe('BroadcasterScreen capture & audio guidance (R24)', () => {
  afterEach(disableChromiumAudio);

  // ── CG2: pre-start "Sharing tips" ──
  it('CG2.1: the tips disclosure is present and collapsed by default', () => {
    render(<BroadcasterScreen />);
    const toggle = screen.getByRole('button', { name: /sharing tips/i });
    expect(toggle.getAttribute('aria-expanded')).toBe('false');
  });

  it('CG2.2: shows the whole-screen tip and the Chromium audio tip on Chromium', () => {
    enableChromiumAudio();
    render(<BroadcasterScreen />);
    expect(screen.getByText(WHOLE_SCREEN_TIP)).toBeTruthy();
    expect(screen.getByText(AUDIO_TIP.chromium)).toBeTruthy();
    expect(screen.queryByText(AUDIO_TIP.unsupported)).toBeNull();
  });

  it('CG2.2: shows the unsupported audio tip on Firefox (no audio globals)', () => {
    render(<BroadcasterScreen />);
    expect(screen.getByText(AUDIO_TIP.unsupported)).toBeTruthy();
    expect(screen.queryByText(AUDIO_TIP.chromium)).toBeNull();
  });

  it('CG2.3: toggling the tips fires no session (the Start path is untouched)', () => {
    render(<BroadcasterScreen />);
    const toggle = screen.getByRole('button', { name: /sharing tips/i });
    fireEvent.click(toggle);
    expect(toggle.getAttribute('aria-expanded')).toBe('true');
    expect(created.length).toBe(0);
    // The Start button is still the one, unchanged entry point.
    expect(screen.getByRole('button', { name: /start a stream/i })).toBeTruthy();
  });

  // ── CG3: reactive live notes ──
  it('CG3.1: renders and dismisses the audio note on Chromium no-track', async () => {
    enableChromiumAudio();
    goLive('monitor', 'no-track');
    await waitFor(() => expect(screen.getByText('LIVE')).toBeTruthy());

    expect(screen.getByText(AUDIO_NOTE.noTrack)).toBeTruthy();
    fireEvent.click(screen.getByLabelText('Dismiss audio note'));
    expect(screen.queryByText(AUDIO_NOTE.noTrack)).toBeNull();
    expect(localStorage.getItem(HINT_AUDIO_MISSING_KEY)).toBe('1');
  });

  it('CG3.2: never renders an audio note on Firefox (no audio capability)', async () => {
    // No audio globals → audioLaneSupported() false → no nag, any state.
    goLive('monitor', 'no-track');
    await waitFor(() => expect(screen.getByText('LIVE')).toBeTruthy());
    expect(screen.queryByText(AUDIO_NOTE.noTrack)).toBeNull();
  });

  it('CG3.3: the window note tracks the capture surface', async () => {
    enableChromiumAudio();
    goLive('window', 'active'); // active audio isolates the window note
    await waitFor(() => expect(screen.getByText('LIVE')).toBeTruthy());
    expect(screen.getByText(WINDOW_NOTE)).toBeTruthy();

    cleanup();
    scripts.length = 0;
    created.length = 0;
    goLive('monitor', 'active');
    await waitFor(() => expect(screen.getByText('LIVE')).toBeTruthy());
    expect(screen.queryByText(WINDOW_NOTE)).toBeNull();
  });

  it('CG3.4: a previously-dismissed note does not re-render', async () => {
    enableChromiumAudio();
    localStorage.setItem(HINT_AUDIO_MISSING_KEY, '1');
    goLive('monitor', 'no-track');
    await waitFor(() => expect(screen.getByText('LIVE')).toBeTruthy());
    expect(screen.queryByText(AUDIO_NOTE.noTrack)).toBeNull();
  });

  it('CG3.5: the happy path renders ZERO notes (active audio, whole screen)', async () => {
    enableChromiumAudio();
    goLive('monitor', 'active');
    await waitFor(() => expect(screen.getByText('LIVE')).toBeTruthy());
    expect(screen.queryByText(AUDIO_NOTE.noTrack)).toBeNull();
    expect(screen.queryByText(AUDIO_NOTE.unavailable)).toBeNull();
    expect(screen.queryByText(WINDOW_NOTE)).toBeNull();
  });

  it('CG3.6: the two notes dismiss independently', async () => {
    enableChromiumAudio();
    goLive('window', 'no-track'); // both notes present
    await waitFor(() => expect(screen.getByText('LIVE')).toBeTruthy());
    expect(screen.getByText(AUDIO_NOTE.noTrack)).toBeTruthy();
    expect(screen.getByText(WINDOW_NOTE)).toBeTruthy();

    fireEvent.click(screen.getByLabelText('Dismiss audio note'));
    expect(screen.queryByText(AUDIO_NOTE.noTrack)).toBeNull();
    // The window note survives — and its own key was not written.
    expect(screen.getByText(WINDOW_NOTE)).toBeTruthy();
    expect(localStorage.getItem(HINT_WINDOW_SHARE_KEY)).toBeNull();
  });

  // ── CG4: settings echo ──
  it('CG4.1: the settings panel shows the browser-correct audio line', () => {
    enableChromiumAudio();
    render(<BroadcasterScreen />);
    fireEvent.click(screen.getByRole('button', { name: /settings/i }));
    expect(screen.getByText(AUDIO_SETTINGS.chromium)).toBeTruthy();
    expect(screen.queryByText(AUDIO_SETTINGS.unsupported)).toBeNull();
  });
});
