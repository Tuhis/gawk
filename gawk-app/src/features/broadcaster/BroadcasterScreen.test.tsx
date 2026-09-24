// @vitest-environment jsdom
//
// BroadcasterScreen start-failure states. createBroadcastSession is mocked so
// each test scripts the session's callback/rejection sequence; the sessions
// contract (workerBroadcastSession/BroadcastPipeline) is that a start()
// rejection fires NO onEnded — the caller owns the error surface — so the
// screen must fully reset the stage itself, including after onSourceStream
// already flipped it to LIVE.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import type { BroadcastCallbacks } from '../../transport/broadcaster';

const { created, scripts } = vi.hoisted(() => {
  interface FakeSession {
    callbacks: BroadcastCallbacks;
    broadcastId?: string;
    grant?: Promise<unknown>;
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
    grant?: Promise<unknown>,
  ) => {
    const script = scripts.shift();
    if (!script) throw new Error('test bug: no session script queued');
    const session = {
      callbacks,
      broadcastId,
      grant,
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
import { useTransportStore } from '../../state/transportStore';
import { BUNDLED_TERMS_VERSION, SITE_DOWNLOAD_URL } from '../../config';
import {
  AUDIO_NOTE,
  AUDIO_SETTINGS,
  AUDIO_TIP,
  HINT_AUDIO_MISSING_KEY,
  HINT_WINDOW_SHARE_KEY,
  NATIVE_TIP,
  tipText,
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

  // The tips render as <p> lines whose linked copy spans several nodes, so
  // assert on the assembled text, not a single text node.
  const tipLines = () =>
    Array.from(document.querySelectorAll('#sharing-tips p')).map((p) => p.textContent);
  const downloadLinks = () =>
    Array.from(document.querySelectorAll<HTMLAnchorElement>('#sharing-tips a')).filter(
      (a) => a.getAttribute('href') === SITE_DOWNLOAD_URL,
    );

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
    expect(tipLines()).toContain(tipText(AUDIO_TIP.chromium));
    expect(tipLines()).not.toContain(tipText(AUDIO_TIP.unsupported));
  });

  it('CG2.2: shows the unsupported audio tip on Firefox (no audio globals)', () => {
    render(<BroadcasterScreen />);
    expect(tipLines()).toContain(tipText(AUDIO_TIP.unsupported));
    expect(tipLines()).not.toContain(tipText(AUDIO_TIP.chromium));
  });

  // The native pointer is browser-independent: per-app audio is out of reach
  // for every browser picker, so it shows either way — and wherever the copy
  // names the native apps it links to the download page.
  it('CG2.2: shows the native per-app audio tip, linked, in either browser', () => {
    const { unmount } = render(<BroadcasterScreen />);
    expect(tipLines()).toContain(tipText(NATIVE_TIP));
    // Firefox: the audio tip names them too, so both lines carry the link.
    expect(downloadLinks()).toHaveLength(2);
    unmount();
    enableChromiumAudio();
    render(<BroadcasterScreen />);
    expect(tipLines()).toContain(tipText(NATIVE_TIP));
    expect(downloadLinks()).toHaveLength(1);
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

// Same macOS idle-dim bug as the viewer, and worse here: capturing a screen is
// not "playing media", so an idle broadcaster's display dims and then sleeps —
// and a slept display can stop delivering getDisplayMedia frames, taking the
// broadcast down rather than just dimming one desk. The hook's rules live in
// lib/useWakeLock.test.ts; this covers the wiring to the live status.
// R37 (docs/40 §4.2 F3): the publish-secret prompt is decided per resolved
// server — config.requirePublishSecret governs only the pinned default, and
// a non-default server prompts exactly when its entry holds no secret. A
// secret-less connect failure against a non-default relay is offered as
// "may require a secret" + retry, never a dead end.
describe('BroadcasterScreen per-server secret prompt (R37 F3)', () => {
  beforeEach(() => {
    const s = useTransportStore.getState();
    s.setSessionOverride(null);
    s.reloadFromStorage();
    s.selectServer('default');
  });

  // The store is a module singleton — clear the override/selection so later
  // describes start from the default server.
  afterEach(() => {
    act(() => {
      const s = useTransportStore.getState();
      s.setSessionOverride(null);
      s.selectServer('default');
    });
  });

  it('does not prompt on the default server when the deployment does not require one', () => {
    scripts.push(async () => {});
    render(<BroadcasterScreen />);
    startBroadcast();
    expect(screen.queryByRole('dialog', { name: 'Publish secret' })).toBeNull();
  });

  it('prompts on a non-default server with no stored secret', () => {
    act(() => {
      useTransportStore.getState().setSessionOverride('https://foreign.example:4433');
    });
    render(<BroadcasterScreen />);
    startBroadcast();
    expect(screen.getByRole('dialog', { name: 'Publish secret' })).toBeTruthy();
  });

  it('skips the prompt on a non-default server whose entry stores a secret', async () => {
    const store = useTransportStore.getState();
    const id = store.addServer({
      label: 'Secured',
      url: 'https://foreign.example:4433',
      publishSecret: 'stored-secret',
    })!;
    act(() => useTransportStore.getState().selectServer(id));
    scripts.push(async () => {});
    render(<BroadcasterScreen />);
    startBroadcast();
    expect(screen.queryByRole('dialog', { name: 'Publish secret' })).toBeNull();
    await waitFor(() => expect(created).toHaveLength(1));
    // Cleanup: drop the entry so later describes see a clean list.
    act(() => useTransportStore.getState().removeServer(id));
  });

  it('offers secret entry + retry when a secret-less connect to a non-default relay fails', async () => {
    const store = useTransportStore.getState();
    const id = store.addServer({ label: 'Maybe secured', url: 'https://foreign2.example:4433' })!;
    act(() => useTransportStore.getState().selectServer(id));
    // The entry has no secret — the pre-start prompt appears; submit empty to
    // attempt without one (the open-relay hope), which then 401s.
    scripts.push(async () => {
      throw new BroadcastStartError('connect', new Error('opening handshake failed'));
    });
    render(<BroadcasterScreen />);
    startBroadcast();
    // Pre-start prompt (empty secret) — continue without one.
    fireEvent.click(screen.getByRole('button', { name: 'Start broadcasting' }));
    // The connect failed; instead of a dead-end error card the secret prompt
    // returns with the may-require note.
    await waitFor(() =>
      expect(screen.getByText(/may require a publish secret/)).toBeTruthy(),
    );
    // Entering a secret retries.
    scripts.push(async () => {});
    fireEvent.change(screen.getByPlaceholderText('shared secret'), {
      target: { value: 'now-i-know' },
    });
    fireEvent.click(screen.getByRole('button', { name: 'Start broadcasting' }));
    await waitFor(() => expect(created).toHaveLength(2));
    // The prompted secret landed on the resolved entry (F3 storage rule).
    expect(
      useTransportStore.getState().servers.find((e) => e.id === id)?.publishSecret,
    ).toBe('now-i-know');
    act(() => useTransportStore.getState().removeServer(id));
  });

  // F2: the indicator renders on the broadcaster screen before capture.
  it('shows the in-session indicator pre-start on a non-default server', () => {
    act(() => {
      useTransportStore.getState().setSessionOverride('https://foreign.example:4433');
    });
    render(<BroadcasterScreen />);
    expect(screen.getByTestId('server-indicator').textContent).toContain(
      'foreign.example:4433',
    );
  });
});

describe('BroadcasterScreen screen wake lock', () => {
  const locks: Array<{ released: boolean }> = [];

  beforeEach(() => {
    locks.length = 0;
    Object.defineProperty(navigator, 'wakeLock', {
      configurable: true,
      value: {
        request: () => {
          const sentinel = {
            released: false,
            release: () => {
              sentinel.released = true;
              return Promise.resolve();
            },
          };
          locks.push(sentinel);
          return Promise.resolve(sentinel);
        },
      },
    });
  });
  afterEach(() => Reflect.deleteProperty(navigator, 'wakeLock'));

  it('takes no lock while idle on the pre-start card', async () => {
    render(<BroadcasterScreen />);
    await act(async () => {});
    expect(locks).toHaveLength(0);
  });

  it('holds the display awake while live and releases it on stop', async () => {
    scripts.push(async (cbs) => {
      cbs.onBroadcastId?.('AB2CD3');
      cbs.onSourceStream(fakeStream);
    });
    render(<BroadcasterScreen />);
    startBroadcast();

    await waitFor(() => expect(screen.getByText('LIVE')).toBeTruthy());
    await waitFor(() => expect(locks).toHaveLength(1));
    expect(locks[0].released).toBe(false);

    fireEvent.click(screen.getByRole('button', { name: /stop broadcast/i }));
    await waitFor(() => expect(locks[0].released).toBe(true));
  });

  it('keeps the lock through an auto-resume reconnect — capture never stopped', async () => {
    scripts.push(async (cbs) => {
      cbs.onBroadcastId?.('AB2CD3');
      cbs.onSourceStream(fakeStream);
    });
    render(<BroadcasterScreen />);
    startBroadcast();
    await waitFor(() => expect(locks).toHaveLength(1));

    await act(async () =>
      created[0].callbacks.onReconnecting?.({ attempt: 1, delayMs: 250, reason: 'blip' }),
    );
    await act(async () => {});
    expect(locks[0].released).toBe(false);
    expect(locks).toHaveLength(1);
  });
});

// Safari: getDisplayMedia must be called from inside the user-gesture handler.
// WebKit's activation does not survive the worker boot + relay connect that
// used to come first, so a Safari start died with "getDisplayMedia must be
// called from a user gesture handler." The prompt now opens in the click,
// concurrently with the connect, and the session consumes the grant.
describe('BroadcasterScreen screen-share prompt (user gesture)', () => {
  function stubDisplayMedia(impl: () => Promise<MediaStream>) {
    const getDisplayMedia = vi.fn(impl);
    Object.defineProperty(navigator, 'mediaDevices', {
      configurable: true,
      value: { getDisplayMedia },
    });
    return getDisplayMedia;
  }

  function stoppableStream() {
    const stop = vi.fn();
    const track = { stop, getSettings: () => ({}) } as unknown as MediaStreamTrack;
    const stream = {
      getTracks: () => [track],
      getVideoTracks: () => [track],
      getAudioTracks: () => [],
    } as unknown as MediaStream;
    return { stream, stop };
  }

  afterEach(() => {
    delete (navigator as { mediaDevices?: unknown }).mediaDevices;
  });

  it('asks for the screen synchronously inside the click and hands the grant to the session', async () => {
    const getDisplayMedia = stubDisplayMedia(() => new Promise(() => {}));
    scripts.push(() => new Promise(() => {}));
    render(<BroadcasterScreen />);
    startBroadcast();
    // No await between the click and this line: the call happened in the
    // gesture handler itself.
    expect(getDisplayMedia).toHaveBeenCalledTimes(1);
    await waitFor(() => expect(created).toHaveLength(1));
    expect(created[0]!.grant).toBeInstanceOf(Promise);
  });

  it('prompts once for a reclaim that falls back to a mint, both sessions sharing the grant', async () => {
    const getDisplayMedia = stubDisplayMedia(() => new Promise(() => {}));
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
    getDisplayMedia.mockClear();
    scripts.push(async () => {
      throw new BroadcastStartError('connect', new Error('reclaim refused'));
    });
    scripts.push(() => new Promise(() => {}));
    startBroadcast();
    await waitFor(() => expect(created).toHaveLength(3));
    expect(getDisplayMedia).toHaveBeenCalledTimes(1);
    expect(created[1]!.grant).toBe(created[2]!.grant);
  });

  // PR #373 review: leaving the page while the relay connects (Stop is
  // disabled then, so leaving is the only way out) stops a session that never
  // asked for the grant and whose start() never settles. The screen owns the
  // grant, so its unmount must release it or the share indicator stays on.
  it('stops the granted tracks when the screen unmounts mid-connect', async () => {
    const { stream, stop } = stoppableStream();
    stubDisplayMedia(async () => stream);
    scripts.push(() => new Promise(() => {}));
    const { unmount } = render(<BroadcasterScreen />);
    startBroadcast();
    await waitFor(() => expect(created).toHaveLength(1));
    unmount();
    await waitFor(() => expect(stop).toHaveBeenCalled());
  });

  it('stops the granted tracks when the start fails before a session used them', async () => {
    const { stream, stop } = stoppableStream();
    stubDisplayMedia(async () => stream);
    scripts.push(async () => {
      throw new BroadcastStartError('connect', new Error('relay unreachable'));
    });
    render(<BroadcasterScreen />);
    startBroadcast();
    await waitFor(() => expect(stop).toHaveBeenCalled());
  });
});
