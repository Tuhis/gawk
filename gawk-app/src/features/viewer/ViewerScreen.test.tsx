// @vitest-environment jsdom
//
// Viewer status → overlay mapping (docs/10 J3). ViewerSession is mocked so the
// test can drive its callbacks and assert the cinematic state each produces.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';

const { sessions, sessionState, FakeViewerSession } = vi.hoisted(() => {
  interface Cbs {
    onConnected: () => void;
    onReconnecting: (i: { attempt: number; reason: string }) => void;
    onConfig: (c: { codec: string; extradata: Uint8Array }) => void;
    onStats: (s: unknown) => void;
    onError: (e: Error) => void;
    onEnded: () => void;
    // R15: the audio crossing (optional — a session consumer without audio
    // never sets them).
    onAudioChunk?: (chunk: {
      timestampUs: number;
      sampleRate: number;
      channels: Float32Array[];
      frameCount: number;
    }) => void;
    onAudioReset?: () => void;
  }
  // failStartWith: makes the next sessions' start() reject — the
  // never-connected path (fatal by ViewerSession policy, no callbacks fire).
  const sessionState = { failStartWith: null as Error | null };
  class FakeViewerSession {
    cbs: Cbs;
    // R19: the connect options carry the delivery negotiation
    // (`deliveryMode: 'reliable'` ⇔ resilient mode) — recorded so a toggle
    // can be asserted at the seam that actually reaches the relay.
    opts: unknown;
    constructor(_url: string, _id: string, opts: unknown, cbs: Cbs) {
      this.cbs = cbs;
      this.opts = opts;
      sessions.push(this);
    }
    async start(): Promise<void> {
      if (sessionState.failStartWith) throw sessionState.failStartWith;
    }
    async stop(): Promise<void> {
      this.cbs.onEnded();
    }
  }
  const sessions: FakeViewerSession[] = [];
  return { sessions, sessionState, FakeViewerSession };
});

vi.mock('../../transport/viewer-session', () => ({
  ViewerSession: FakeViewerSession,
  RECONNECT_MAX_ATTEMPTS: 10,
}));

// docs/17 Decision 10: the retired 'fixed' playout entry is gated on
// isDevEnvironment(), which vitest makes unconditionally true
// (import.meta.env.DEV) — so a real viewer's behaviour is only assertable
// through a seam. Everything else in config.ts stays real: playout.ts reads
// getDvrBufferMs() from this same module.
const devEnv = vi.hoisted(() => ({ value: true }));
vi.mock('../../config', async (importActual) => ({
  ...(await importActual<typeof import('../../config')>()),
  isDevEnvironment: () => devEnv.value,
}));

import { ViewerScreen } from './ViewerScreen';
import {
  PLAYOUT_OFFSET_MS,
  getPlayoutMode,
  getPlayoutOffsetMs,
  getStoredPlayoutMode,
  setPlayoutMode,
  setViewerDeliveryMode,
} from '../../transport/playout';

beforeEach(() => {
  sessions.length = 0;
  sessionState.failStartWith = null;
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

  // Error-card copy is keyed on the structured kind, never the raw transport
  // message — that goes to the console only (users found "handshake failed"
  // and friends meaningless).

  it('shows the streamer-offline card when the first connect fails', async () => {
    sessionState.failStartWith = new Error('WebTransportError: Opening handshake failed.');
    render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(screen.getByText('Streamer offline')).toBeTruthy());
    expect(screen.getByText(/No one is streaming at “AB2CD3”/)).toBeTruthy();
    expect(screen.queryByText(/handshake/i)).toBeNull();
    expect(screen.getByText('Retry')).toBeTruthy();
  });

  it('shows the lost-stream card when reconnects are exhausted', async () => {
    render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(sessions).toHaveLength(1));
    act(() => sessions[0].cbs.onConnected());
    act(() => sessions[0].cbs.onError(new Error('reconnect failed after 10 attempts: boom')));
    expect(screen.getByText('Lost the stream')).toBeTruthy();
    expect(screen.getByText(/couldn’t reconnect/)).toBeTruthy();
    expect(screen.queryByText(/boom/)).toBeNull();
    expect(screen.getByText('Retry')).toBeTruthy();
  });

  // R18 (docs/23 Decision 8): the live audience badge beside the status. The
  // wire carries the honest total, so a lone viewer reads "1 watching".
  it('renders the watching badge from the pushed viewer count', async () => {
    render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(sessions).toHaveLength(1));
    act(() => sessions[0].cbs.onConnected());
    expect(screen.queryByText(/watching/)).toBeNull();

    act(() => sessions[0].cbs.onStats({ viewerCount: 1 }));
    expect(screen.getByText(/1 watching/)).toBeTruthy();

    act(() => sessions[0].cbs.onStats({ viewerCount: 5 }));
    expect(screen.getByText(/5 watching/)).toBeTruthy();
  });

  it('shows the unplayable card (with codec, without Retry) on a fatal error', async () => {
    render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(sessions).toHaveLength(1));
    act(() => sessions[0].cbs.onConnected());
    act(() => sessions[0].cbs.onConfig({ codec: 'avc1.42E02A', extradata: new Uint8Array() }));
    act(() => sessions[0].cbs.onError(Object.assign(new Error('decoder exploded'), { fatal: true })));
    expect(screen.getByText('Can’t play this stream')).toBeTruthy();
    expect(screen.getByText(/can’t decode this stream’s video format \(codec avc1\.42E02A\)/)).toBeTruthy();
    expect(screen.queryByText(/decoder exploded/)).toBeNull();
    // Retry is pointless for an unplayable codec.
    expect(screen.queryByText('Retry')).toBeNull();
  });
});

// R5 Q3 + R12 T2, revised by docs/17 Decision 10 (2026-07-23): the production
// viewer has ONE playout toggle — "Paced playback" (the R12 adaptive
// paced-presentation mode) — persisted as one mode and applied to the
// (main-thread, in these tests) pipeline context. The retired fixed 150 ms
// mode keeps its "Smooth playback (fixed 150 ms)" entry in dev builds only,
// as the measurement-free control for pacing diagnosis. Since the default
// flip (user decision 2026-07-15), a fresh browser defaults to adaptive +
// interpolation; the menu is the disable path.
describe('ViewerScreen playout modes', () => {
  function cleanupPlayout() {
    setPlayoutMode('off');
    devEnv.value = true;
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
    expect(screen.getByText('Paced playback ✓')).toBeTruthy();
    cleanupPlayout();
  });

  // docs/17 Decision 10: the production menu offers pacing as one binary. A
  // real viewer never sees the fixed entry, so it can never reach 'fixed'.
  it('offers no fixed-playout entry outside a dev build', async () => {
    cleanupPlayout();
    devEnv.value = false;
    render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(sessions).toHaveLength(1));

    openMenu();
    expect(screen.queryByText(/Smooth playback/)).toBeNull();
    expect(screen.getByText('Paced playback ✓')).toBeTruthy();

    // The one remaining toggle is a plain binary: adaptive ⇄ live-edge.
    fireEvent.click(screen.getByText('Paced playback ✓'));
    expect(getPlayoutMode()).toBe('off');
    expect(localStorage.getItem('gawk:playout-mode')).toBe('off');
    openMenu();
    fireEvent.click(screen.getByText('Paced playback'));
    expect(getPlayoutMode()).toBe('adaptive');
    cleanupPlayout();
  });

  // The mode survives as a dev-only diagnostic — a measurement-free offset,
  // which is what separates a pacing bug from a jitter-estimator bug.
  it('toggles fixed smoothing via the context menu in a dev build', async () => {
    cleanupPlayout();
    render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(sessions).toHaveLength(1));
    expect(getPlayoutMode()).toBe('adaptive'); // the default

    openMenu();
    fireEvent.click(screen.getByText('Smooth playback (fixed 150 ms)'));
    expect(getPlayoutMode()).toBe('fixed');
    expect(getPlayoutOffsetMs()).toBe(PLAYOUT_OFFSET_MS);
    expect(localStorage.getItem('gawk:playout-mode')).toBe('fixed');

    // Toggle back off: live-edge, persisted so the default flip won't undo it.
    openMenu();
    fireEvent.click(screen.getByText('Smooth playback (fixed 150 ms) ✓'));
    expect(getPlayoutMode()).toBe('off');
    expect(getPlayoutOffsetMs()).toBe(0);
    expect(localStorage.getItem('gawk:playout-mode')).toBe('off');
    cleanupPlayout();
  });

  it('the two smoothing modes exclude each other in a dev build', async () => {
    cleanupPlayout();
    render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(sessions).toHaveLength(1));

    // Checking the other mode unchecks the default adaptive one.
    openMenu();
    expect(screen.queryByText('Paced playback ✓')).toBeTruthy();
    fireEvent.click(screen.getByText('Smooth playback (fixed 150 ms)'));
    expect(getPlayoutMode()).toBe('fixed');
    openMenu();
    expect(screen.queryByText('Paced playback ✓')).toBeNull();
    expect(screen.queryByText('Smooth playback (fixed 150 ms) ✓')).toBeTruthy();

    // Re-checking adaptive flips back.
    fireEvent.click(screen.getByText('Paced playback'));
    expect(getPlayoutMode()).toBe('adaptive');

    // And unchecking the checked one returns to live-edge.
    openMenu();
    fireEvent.click(screen.getByText('Paced playback ✓'));
    expect(getPlayoutMode()).toBe('off');
    cleanupPlayout();
  });

  // docs/17 Decision 10: a viewer carrying 'fixed' from before the retirement
  // lands on adaptive — the mode fixed was a worse approximation of — rather
  // than on a mode with no control in their menu.
  it('migrates a stored fixed mode to adaptive outside a dev build', async () => {
    cleanupPlayout();
    devEnv.value = false;
    localStorage.setItem('gawk:playout-mode', 'fixed');
    render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(sessions).toHaveLength(1));
    expect(getPlayoutMode()).toBe('adaptive');
    openMenu();
    expect(screen.getByText('Paced playback ✓')).toBeTruthy();
    cleanup();

    // ...and is honoured where the menu can still reach it.
    cleanupPlayout();
    localStorage.setItem('gawk:playout-mode', 'fixed');
    render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(sessions).toHaveLength(2));
    expect(getPlayoutMode()).toBe('fixed');
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

  // docs/17 Decision 10 re-pointed the legacy hop: an R5 viewer who opted into
  // "Smooth playback" was asking for smoothing, and adaptive is the mode that
  // now delivers it. (It also lands them where the menu has a control.)
  it('migrates the legacy smoothed-playout preference: on → adaptive, explicit off → off', async () => {
    cleanupPlayout();
    localStorage.setItem('gawk:smoothed-playout', '1');
    render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(sessions).toHaveLength(1));
    expect(getPlayoutMode()).toBe('adaptive');
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

  // Review finding LIFECYCLE-2 (docs/reviews/resilient-mode-review.md): the
  // entry was gated on the *stored* mode, but resilient mode overrides the
  // *effective* one to adaptive — so a resilient viewer whose stored playout
  // is 'off'/'fixed' had interpolation running with no way to turn it off.
  it('offers interpolation under resilient mode even when the stored mode is not adaptive', async () => {
    cleanupPlayout();
    localStorage.removeItem('gawk:resilient-mode');
    localStorage.setItem('gawk:playout-mode', 'off');
    localStorage.setItem('gawk:resilient-mode', '1');
    render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(sessions).toHaveLength(1));

    // Stored 'off', effective 'adaptive' — the pipeline reports interpolation
    // as live, which is precisely why the control has to be reachable.
    expect(getStoredPlayoutMode()).toBe('off');
    expect(getPlayoutMode()).toBe('adaptive');

    act(() => sessions[0].cbs.onStats({ interpolation: 'on' }));
    openMenu();
    expect(screen.getByText('Frame interpolation (experimental) ✓')).toBeTruthy();

    // …and it actually turns it off, leaving the stored playout mode alone.
    fireEvent.click(screen.getByText('Frame interpolation (experimental) ✓'));
    expect(localStorage.getItem('gawk:interpolation')).toBe('0');
    expect(localStorage.getItem('gawk:playout-mode')).toBe('off');

    localStorage.removeItem('gawk:resilient-mode');
    setViewerDeliveryMode('live');
    cleanupPlayout();
  });

  // The other half of "effective mode": with resilient mode off and the
  // stored mode not adaptive, nothing is interpolating, so the entry stays
  // absent (a stale stats sample must not resurrect it).
  it('hides interpolation when neither the stored nor the effective mode is adaptive', async () => {
    cleanupPlayout();
    localStorage.setItem('gawk:playout-mode', 'fixed');
    render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(sessions).toHaveLength(1));

    act(() => sessions[0].cbs.onStats({ interpolation: 'on' }));
    openMenu();
    expect(screen.queryByText(/Frame interpolation/)).toBeNull();
    cleanupPlayout();
  });
});

// Review finding PRODUCT-2 (docs/reviews/resilient-mode-review.md): every
// menu-only setting — above all R19's "Resilient mode (mobile networks)",
// which exists *for* phones — was reachable through a right-click alone, an
// affordance touch devices do not have. The control bar carries a visible
// overflow button that opens the same menu.
describe('ViewerScreen menu button (touch reachability)', () => {
  function clearPrefs() {
    localStorage.removeItem('gawk:resilient-mode');
    localStorage.removeItem('gawk:playout-mode');
    localStorage.removeItem('gawk:interpolation');
  }
  beforeEach(clearPrefs);
  afterEach(clearPrefs);

  // A tap: pointerdown (which also dismisses an open menu) then click.
  const tap = (el: Element) => {
    fireEvent.pointerDown(el);
    fireEvent.click(el);
  };

  it('opens the same menu from a visible control-bar button', async () => {
    render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(sessions).toHaveLength(1));
    act(() => sessions[0].cbs.onConnected());

    expect(screen.queryByRole('menu')).toBeNull();
    tap(screen.getByLabelText('More options'));

    expect(screen.getByRole('menu')).toBeTruthy();
    expect(screen.getByText(/^Resilient mode —/)).toBeTruthy();
    expect(screen.getByText('Copy link')).toBeTruthy();
  });

  it('tapping the button again dismisses the menu', async () => {
    render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(sessions).toHaveLength(1));
    act(() => sessions[0].cbs.onConnected());

    const button = screen.getByLabelText('More options');
    tap(button);
    expect(screen.getByRole('menu')).toBeTruthy();
    tap(button);
    expect(screen.queryByRole('menu')).toBeNull();
  });

  // The point of the fix: a phone viewer on a lossy link can actually reach
  // the mode, and reaching it negotiates reliable delivery.
  it('picks a delivery mode from the button and reconnects with reliable delivery', async () => {
    render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(sessions).toHaveLength(1));
    act(() => sessions[0].cbs.onConnected());
    expect(sessions[0].opts).not.toMatchObject({ deliveryMode: 'reliable' });

    tap(screen.getByLabelText('More options'));
    fireEvent.click(screen.getByText(/^Resilient mode —/));

    await waitFor(() => expect(sessions).toHaveLength(2));
    expect(sessions[1].opts).toMatchObject({ deliveryMode: 'reliable' });
    expect(localStorage.getItem('gawk:viewer-delivery')).toBe('resilient');

    // R21 (docs/26 Decision 15): one axis, three points — the menu is a radio
    // group, so the active one is ticked and the others are reachable.
    tap(screen.getByLabelText('More options'));
    expect(screen.getByText(/^Resilient mode ✓/)).toBeTruthy();
    expect(screen.getByText(/^Live edge —/)).toBeTruthy();
    fireEvent.click(screen.getByText(/^Deep buffer —/));
    await waitFor(() => expect(sessions).toHaveLength(3));
    // Deep buffer is still reliable delivery; what differs is the buffer the
    // viewer asks the relay to back.
    expect(sessions[2].opts).toMatchObject({ deliveryMode: 'reliable' });
    expect(localStorage.getItem('gawk:viewer-delivery')).toBe('deep');
  });

  it('migrates an R19 resilient viewer to resilient, never to deep', async () => {
    // A 10x latency change nobody asked for would be the worst possible way
    // to introduce this (docs/26 Decision 15).
    localStorage.removeItem('gawk:viewer-delivery');
    localStorage.setItem('gawk:resilient-mode', '1');
    render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(sessions).toHaveLength(1));
    act(() => sessions[0].cbs.onConnected());
    tap(screen.getByLabelText('More options'));
    expect(screen.getByText(/^Resilient mode ✓/)).toBeTruthy();
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
    const value = screen.getByText('NativeVideoFullscreen').nextSibling as HTMLElement;
    expect(value.textContent).toBe('✗');
    expect(value.getAttribute('title')).toBe('element fullscreen available');
  });

  it('gated without worker support: no video element, gate row reads probe failed → pseudo', async () => {
    const { container } = render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(sessions).toHaveLength(1));

    expect(container.querySelector('video')).toBeNull();

    openStats();
    const value = screen.getByText('NativeVideoFullscreen').nextSibling as HTMLElement;
    expect(value.textContent).toBe('✗');
    expect(value.getAttribute('title')).toBe('probe failed → pseudo');
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
      // U4: element-side fields are null while no presentation <video> exists
      // (main-thread fallback ⇒ no track ⇒ no element).
      elementReadyState: null,
      elementPaused: null,
      elementWidth: null,
      elementHeight: null,
      elementFrames: null,
      elementContentPeak: null,
    });
  });
});

// R15 N4 (docs/20 Decision 9): the conditional-audio-UI criterion — audio
// controls exist only when the stream actually carries audio.
//
// jsdom has no Web Audio, so these stub the minimum the sink touches. That is
// deliberate: the UI must appear only where audio can actually *play*, so the
// tests exercise the real ensureSink path rather than bypassing it.
function stubWebAudio() {
  class FakeAudioWorkletNode {
    port = { postMessage: vi.fn(), onmessage: null as unknown };
    connect = vi.fn();
    disconnect = vi.fn();
  }
  class FakeAudioContext {
    state = 'running';
    destination = {};
    audioWorklet = { addModule: vi.fn(() => Promise.resolve()) };
    createGain = vi.fn(() => ({ gain: { value: 1 }, connect: vi.fn(), disconnect: vi.fn() }));
    resume = vi.fn(() => Promise.resolve());
    close = vi.fn(() => Promise.resolve());
  }
  vi.stubGlobal('AudioContext', FakeAudioContext);
  vi.stubGlobal('AudioWorkletNode', FakeAudioWorkletNode);
  vi.stubGlobal('URL', {
    ...URL,
    createObjectURL: vi.fn(() => 'blob:stub'),
    revokeObjectURL: vi.fn(),
  });
}

describe('ViewerScreen audio UI (R15)', () => {
  beforeEach(() => {
    localStorage.clear();
    stubWebAudio();
  });
  afterEach(() => vi.unstubAllGlobals());

  it('renders no audio UI for a video-only stream', async () => {
    render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(sessions).toHaveLength(1));
    act(() => sessions[0].cbs.onConnected());

    expect(screen.queryByLabelText('Volume')).toBeNull();
    expect(screen.queryByLabelText('Mute')).toBeNull();
    expect(screen.queryByLabelText('Unmute')).toBeNull();
    expect(screen.queryByText(/Tap for sound/)).toBeNull();

    // …and the right-click menu carries no audio entry either.
    fireEvent.contextMenu(document.body.querySelector('div')!);
    expect(screen.queryByText('Mute')).toBeNull();
  });

  it('reveals mute/volume reactively once audio is received mid-view', async () => {
    render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(sessions).toHaveLength(1));
    act(() => sessions[0].cbs.onConnected());
    expect(screen.queryByLabelText('Volume')).toBeNull();

    // The pipeline decodes its first audio chunk (jsdom has no AudioContext,
    // so the sink can't build — but `present` is driven by receipt, and the
    // controls must appear the moment audio exists in the stream).
    act(() => {
      sessions[0].cbs.onAudioChunk?.({
        timestampUs: 0,
        sampleRate: 48000,
        channels: [new Float32Array(960), new Float32Array(960)],
        frameCount: 960,
      });
    });

    await waitFor(() => expect(screen.queryByLabelText('Volume')).not.toBeNull());
    expect(screen.getByLabelText('Mute')).toBeTruthy();
  });

  it('mute persists and flips the control label', async () => {
    render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(sessions).toHaveLength(1));
    act(() => sessions[0].cbs.onConnected());
    act(() => {
      sessions[0].cbs.onAudioChunk?.({
        timestampUs: 0,
        sampleRate: 48000,
        channels: [new Float32Array(960)],
        frameCount: 960,
      });
    });
    await waitFor(() => expect(screen.queryByLabelText('Mute')).not.toBeNull());

    fireEvent.click(screen.getByLabelText('Mute'));
    await waitFor(() => expect(screen.queryByLabelText('Unmute')).not.toBeNull());
    expect(localStorage.getItem('gawk:muted')).toBe('1');

    fireEvent.click(screen.getByLabelText('Unmute'));
    await waitFor(() => expect(screen.queryByLabelText('Mute')).not.toBeNull());
    expect(localStorage.getItem('gawk:muted')).toBe('0');
  });
});
