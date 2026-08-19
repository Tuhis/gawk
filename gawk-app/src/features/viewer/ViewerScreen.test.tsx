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
    // R28: wire 0x0D, the session identity the overlay names.
    onTelemetryHello?: (hello: {
      enabled: boolean;
      reportIntervalMs: number;
      token: string;
      broadcastKey: string;
    }) => void;
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
    // The relay address this session was started against — the seam where the
    // dev server-URL override has to land.
    url: string;
    constructor(url: string, _id: string, opts: unknown, cbs: Cbs) {
      this.cbs = cbs;
      this.opts = opts;
      this.url = url;
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

// isDevEnvironment() is unconditionally true under vitest
// (import.meta.env.DEV), so a real viewer's behaviour — the relay-override
// entry's absence, and the production menu's row ceiling — is only assertable
// through a seam. Everything else in config.ts stays real: playout.ts reads
// getDvrBufferMs() from this same module.
const devEnv = vi.hoisted(() => ({ value: true }));
vi.mock('../../config', async (importActual) => ({
  ...(await importActual<typeof import('../../config')>()),
  isDevEnvironment: () => devEnv.value,
}));

import { ViewerScreen } from './ViewerScreen';
import { useTransportStore } from '../../state/transportStore';

// ── R32 shared surface helpers ───────────────────────────────────────────────
// The tuning controls moved from one flat menu to a pill + a settings panel,
// so every test that used to click a menu row now walks one of these two
// paths. Selectors changed; the behaviour each test asserts did not.

/** The two-tap path an average viewer takes to the playback presets. */
const openPresetMenu = () => fireEvent.click(screen.getByLabelText(/^Playback quality:/));

/** Open the settings panel (via the preset popover's "More settings…"). */
const openSettings = () => {
  openPresetMenu();
  fireEvent.click(screen.getByText('More settings…'));
};

/** Expand the panel's Advanced disclosure — collapsed by default (UX3.2). */
const openAdvanced = () => fireEvent.click(screen.getByRole('button', { name: /Advanced/ }));

const interpolationBox = () =>
  screen.getByRole('checkbox', { name: /Frame interpolation/ });

/** A labelled <select> in the Advanced section. */
const advancedSelect = (name: RegExp) => screen.getByRole('combobox', { name });
import {
  getPlayoutMode,
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
// (main-thread, in these tests) pipeline context. R32 removed the retired
// fixed 150 ms mode outright, so pacing is now purely a property of the chosen
// preset. Since the default flip (user decision 2026-07-15), a fresh browser
// defaults to adaptive + interpolation.
describe('ViewerScreen playout modes', () => {
  function cleanupPlayout() {
    setPlayoutMode('off');
    devEnv.value = true;
    // All five R32 keys, not just the playout ones: the preset is derived from
    // every one of them (docs/37 decision 1), so a delivery value left behind
    // by an earlier test now changes what this one renders. The leakage
    // predates R32 — the R19 migration test used to clear `viewer-delivery` by
    // hand — but a derived pill makes it bite everywhere instead of once.
    localStorage.removeItem('gawk:playout-mode');
    localStorage.removeItem('gawk:smoothed-playout');
    localStorage.removeItem('gawk:interpolation');
    localStorage.removeItem('gawk:viewer-delivery');
    localStorage.removeItem('gawk:resilient-mode');
    localStorage.removeItem('gawk:parity-level');
    localStorage.removeItem('gawk:stripe-mode');
    setViewerDeliveryMode('live');
  }

  const openMenu = () =>
    fireEvent.contextMenu(screen.getByText('connecting').closest('div')!.parentElement!);

  // R32 UX4: pacing is now a property of the preset, reached from the
  // control-bar pill. `openPresets` is the two-tap path an average viewer
  // takes; `checkedPreset` reads the state off ARIA rather than off a '✓'
  // glued into the label (UX1.2).
  const openPresets = () => fireEvent.click(screen.getByLabelText(/^Playback quality:/));
  const pickPreset = (label: string) =>
    fireEvent.click(screen.getByRole('menuitemradio', { name: label }));
  // The label alone, not the whole row: the sub-label rides an
  // aria-describedby span inside the same button.
  const checkedPreset = () => {
    const row = screen
      .getAllByRole('menuitemradio')
      .find((el) => el.getAttribute('aria-checked') === 'true');
    if (!row) return null;
    const labelId = row.getAttribute('aria-labelledby');
    return labelId ? (document.getElementById(labelId)?.textContent ?? null) : row.textContent;
  };

  it('defaults to adaptive paced playback when nothing is stored', async () => {
    cleanupPlayout();
    render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(sessions).toHaveLength(1));
    expect(getPlayoutMode()).toBe('adaptive');
    // The default state IS a preset — "Balanced" is defined to be today's
    // shipping configuration, so R32 changes no behaviour on a fresh install.
    expect(screen.getByLabelText('Playback quality: Balanced')).toBeTruthy();
    openPresets();
    expect(checkedPreset()).toBe('Balanced');
    cleanupPlayout();
  });

  // docs/17 Decision 10 retired the fixed 150 ms mode from the production
  // menu; R32 removed it outright (owner decision 2026-07-29), so there is no
  // build in which a pacing row exists in the menu at all. Pacing is a
  // property of the preset and nothing else.
  it('offers no fixed-playout entry in any build', async () => {
    cleanupPlayout();
    render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(sessions).toHaveLength(1));

    openMenu();
    expect(screen.queryByText(/Smooth playback/)).toBeNull();
    fireEvent.keyDown(screen.getByRole('menu'), { key: 'Escape' });

    // The pacing binary is now the step between the two live-edge presets.
    openPresets();
    pickPreset('Lowest latency');
    expect(getPlayoutMode()).toBe('off');
    expect(localStorage.getItem('gawk:playout-mode')).toBe('off');
    openPresets();
    pickPreset('Balanced');
    expect(getPlayoutMode()).toBe('adaptive');
    cleanupPlayout();
  });

  // A viewer carrying 'fixed' — from before docs/17 Decision 10 retired it, or
  // from a dev build that could still select it until R32 — lands on adaptive,
  // the mode fixed was a worse approximation of. Unconditional now: there is no
  // build left that can honour the stored value.
  it('migrates a stored fixed mode to adaptive in every build', async () => {
    cleanupPlayout();
    localStorage.setItem('gawk:playout-mode', 'fixed');
    render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(sessions).toHaveLength(1));
    expect(getPlayoutMode()).toBe('adaptive');
    expect(screen.getByLabelText('Playback quality: Balanced')).toBeTruthy();
    cleanup();

    // Same in a dev build — the value has nowhere left to be honoured.
    cleanupPlayout();
    devEnv.value = true;
    localStorage.setItem('gawk:playout-mode', 'fixed');
    render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(sessions).toHaveLength(2));
    expect(getPlayoutMode()).toBe('adaptive');
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

  // R12 T4 + default flip: interpolation defaults ON and the surface is the
  // disable path. R32 UX5.3 changes *when* it renders: it is present from the
  // first paint, disabled with a reason, rather than materialising a row a
  // second into the session when the first stats sample lands.
  it('interpolation defaults on and toggles off through the settings panel', async () => {
    cleanupPlayout();
    render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(sessions).toHaveLength(1));

    openSettings();
    openAdvanced();
    const before = interpolationBox();
    // Present from first paint — but honest about not knowing yet, which is
    // not the same claim as "this browser can't".
    expect(before.getAttribute('disabled')).not.toBeNull();
    expect(screen.getByText('Available once the stream is running.')).toBeTruthy();

    act(() => sessions[0].cbs.onStats({ interpolation: 'on' }));
    const live = interpolationBox();
    expect(live.getAttribute('disabled')).toBeNull();
    expect((live as HTMLInputElement).checked).toBe(true);

    fireEvent.click(live);
    expect(localStorage.getItem('gawk:interpolation')).toBe('0');
    expect((interpolationBox() as HTMLInputElement).checked).toBe(false);
    cleanupPlayout();
  });

  // The other end of the three states: the pipeline has reported and the
  // answer is no. Distinct copy, because "not yet" and "never" are different
  // facts about the viewer's browser.
  it('says interpolation is unavailable once the pipeline has reported it is', async () => {
    cleanupPlayout();
    render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(sessions).toHaveLength(1));
    act(() => sessions[0].cbs.onStats({ interpolation: null }));

    openSettings();
    openAdvanced();
    expect(interpolationBox().getAttribute('disabled')).not.toBeNull();
    expect(screen.getByText(/this viewer pipeline can’t interpolate/)).toBeTruthy();
    cleanupPlayout();
  });

  // Review finding LIFECYCLE-2 (docs/reviews/resilient-mode-review.md): the
  // entry was gated on the *stored* mode, but resilient mode overrides the
  // *effective* one to adaptive — so a resilient viewer whose stored playout
  // is 'off' had interpolation running with no way to turn it off.
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
    openSettings();
    openAdvanced();
    const box = interpolationBox();
    expect(box.getAttribute('disabled')).toBeNull();
    expect((box as HTMLInputElement).checked).toBe(true);

    // …and it actually turns it off, leaving the stored playout mode alone.
    fireEvent.click(box);
    expect(localStorage.getItem('gawk:interpolation')).toBe('0');
    expect(localStorage.getItem('gawk:playout-mode')).toBe('off');

    localStorage.removeItem('gawk:resilient-mode');
    setViewerDeliveryMode('live');
    cleanupPlayout();
  });

  // The other half of "effective mode": with a carrier delivery mode off and
  // the stored mode not adaptive, nothing is interpolating. R32 UX5.1 keeps
  // the control *present* and says why, instead of removing it — a row that
  // vanishes teaches nothing.
  it('disables interpolation with a reason when the effective mode is not adaptive', async () => {
    cleanupPlayout();
    localStorage.setItem('gawk:playout-mode', 'off');
    render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(sessions).toHaveLength(1));

    act(() => sessions[0].cbs.onStats({ interpolation: 'on' }));
    openSettings();
    openAdvanced();
    expect(interpolationBox().getAttribute('disabled')).not.toBeNull();
    expect(screen.getByText(/Needs paced playback/)).toBeTruthy();
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
    // Every key the preset derives from — see cleanupPlayout above.
    localStorage.removeItem('gawk:resilient-mode');
    localStorage.removeItem('gawk:playout-mode');
    localStorage.removeItem('gawk:interpolation');
    localStorage.removeItem('gawk:viewer-delivery');
    localStorage.removeItem('gawk:parity-level');
    localStorage.removeItem('gawk:stripe-mode');
    setViewerDeliveryMode('live');
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
    expect(screen.getByText('Copy link')).toBeTruthy();
    expect(screen.getByText('Playback settings…')).toBeTruthy();
  });

  // R32 UX4.4/UX4.5: the menu is actions-only. This is the assertion that
  // stops the next milestone quietly appending its knob here — which is
  // exactly how it reached seventeen rows, eleven of them tuning (docs/37 §1).
  it('carries no tuning rows and stays short', async () => {
    // A production build: the dev-only relay override is what a real viewer
    // never sees, and the ceiling that matters is theirs.
    devEnv.value = false;
    render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(sessions).toHaveLength(1));
    act(() => sessions[0].cbs.onConnected());
    tap(screen.getByLabelText('More options'));

    const rows = screen.getByRole('menu').querySelectorAll('button');
    for (const pattern of [
      /Live edge/,
      /Resilient mode/,
      /Deep buffer/,
      /Loss protection/,
      /Striping/,
      /Paced playback/,
      /Frame interpolation/,
      /Smooth playback/,
    ]) {
      expect(screen.queryByText(pattern)).toBeNull();
    }
    // Stats, Fullscreen, Playback settings…, Copy link, Terms of use, Leave —
    // plus Mute on a stream that carries audio. Seven, against the seventeen
    // this menu had grown to (docs/37 §1).
    expect(rows.length).toBeLessThanOrEqual(7);
    devEnv.value = true;
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

  // The point of the fix, carried over to R32's pill: a phone viewer on a
  // lossy link can reach the mode in two taps, and reaching it negotiates
  // reliable delivery.
  it('picks a delivery mode from the preset pill and reconnects with reliable delivery', async () => {
    render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(sessions).toHaveLength(1));
    act(() => sessions[0].cbs.onConnected());
    expect(sessions[0].opts).not.toMatchObject({ deliveryMode: 'reliable' });

    tap(screen.getByLabelText(/^Playback quality:/));
    fireEvent.click(screen.getByRole('menuitemradio', { name: 'Smoother' }));

    await waitFor(() => expect(sessions).toHaveLength(2));
    expect(sessions[1].opts).toMatchObject({ deliveryMode: 'reliable' });
    expect(localStorage.getItem('gawk:viewer-delivery')).toBe('resilient');

    // R21 (docs/26 Decision 15): one axis, now four points — a radio group, so
    // the active one is checked and the others are reachable.
    tap(screen.getByLabelText(/^Playback quality:/));
    expect(
      screen.getByRole('menuitemradio', { name: 'Smoother' }).getAttribute('aria-checked'),
    ).toBe('true');
    expect(screen.getByRole('menuitemradio', { name: 'Balanced' })).toBeTruthy();
    fireEvent.click(screen.getByRole('menuitemradio', { name: 'Most stable' }));
    await waitFor(() => expect(sessions).toHaveLength(3));
    // Deep buffer is still reliable delivery; what differs is the buffer the
    // viewer asks the relay to back.
    expect(sessions[2].opts).toMatchObject({ deliveryMode: 'reliable' });
    expect(localStorage.getItem('gawk:viewer-delivery')).toBe('deep');
  });

  // R32 UX5.5: the pacing-only step must not re-dial. Delivery and parity are
  // in useViewerConnection's session-effect deps; pacing is not, and the
  // "· switching reconnects" annotation would be a lie if this changed.
  it('does not reconnect when a preset changes only pacing', async () => {
    render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(sessions).toHaveLength(1));
    act(() => sessions[0].cbs.onConnected());

    tap(screen.getByLabelText(/^Playback quality:/));
    fireEvent.click(screen.getByRole('menuitemradio', { name: 'Lowest latency' }));

    expect(localStorage.getItem('gawk:playout-mode')).toBe('off');
    expect(localStorage.getItem('gawk:viewer-delivery')).toBe('live');
    expect(sessions).toHaveLength(1);
  });

  it('migrates an R19 resilient viewer to resilient, never to deep', async () => {
    // A 10x latency change nobody asked for would be the worst possible way
    // to introduce this (docs/26 Decision 15).
    localStorage.removeItem('gawk:viewer-delivery');
    localStorage.setItem('gawk:resilient-mode', '1');
    render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(sessions).toHaveLength(1));
    act(() => sessions[0].cbs.onConnected());
    tap(screen.getByLabelText(/^Playback quality:/));
    expect(
      screen.getByRole('menuitemradio', { name: 'Smoother' }).getAttribute('aria-checked'),
    ).toBe('true');
  });

  // R30 (docs/35 §5.5): striping. Live-edge only, persisted, and — unlike
  // delivery/parity — applied LIVE: no session teardown. R32 moves it into the
  // panel's Advanced section and, per UX5.1, keeps it *present and disabled*
  // off live-edge instead of removing it.
  it('picks a stripe mode from the panel without reconnecting, live-edge only', async () => {
    render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(sessions).toHaveLength(1));
    act(() => sessions[0].cbs.onConnected());

    openSettings();
    openAdvanced();
    const striping = advancedSelect(/Striping/) as HTMLSelectElement;
    expect(striping.value).toBe('auto');
    expect(striping.disabled).toBe(false);

    fireEvent.change(striping, { target: { value: 'on' } });
    expect(localStorage.getItem('gawk:stripe-mode')).toBe('on');
    // A live flip: the session is untouched.
    expect(sessions).toHaveLength(1);

    expect((advancedSelect(/Striping/) as HTMLSelectElement).value).toBe('on');
    fireEvent.change(advancedSelect(/Striping/), { target: { value: 'auto' } });
    expect(localStorage.getItem('gawk:stripe-mode')).toBeNull();
    expect(sessions).toHaveLength(1);
  });

  // R32 UX5.1: off live-edge, parity and striping stay on screen, disabled,
  // each carrying its own reason. Before R32 they were filtered out of the
  // array, so the menu changed length with the delivery mode and a viewer who
  // had seen "Loss protection" once could not find it again (docs/37 §1.2).
  it('grays parity and striping with reasons under a carrier delivery mode', async () => {
    render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(sessions).toHaveLength(1));
    act(() => sessions[0].cbs.onConnected());

    tap(screen.getByLabelText(/^Playback quality:/));
    fireEvent.click(screen.getByRole('menuitemradio', { name: 'Smoother' }));
    await waitFor(() => expect(sessions).toHaveLength(2));

    openSettings();
    openAdvanced();
    expect((advancedSelect(/Loss protection/) as HTMLSelectElement).disabled).toBe(true);
    expect((advancedSelect(/Striping/) as HTMLSelectElement).disabled).toBe(true);
    expect(screen.getByText(/already recovers lost packets/)).toBeTruthy();
    expect(screen.getByText(/already handles bursts/)).toBeTruthy();
  });

  // R32 UX3.1 + UX4.1/UX4.2 + UX3.3: the Custom rule the owner asked for —
  // Custom appears only once something advanced is off its default, applying a
  // preset is a complete configuration, and Reset advanced undoes the
  // deviation without touching the preset.
  it('shows Custom only after an advanced change, and resets back', async () => {
    render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(sessions).toHaveLength(1));
    act(() => sessions[0].cbs.onConnected());

    // A clean install is never Custom.
    expect(screen.getByLabelText('Playback quality: Balanced')).toBeTruthy();

    openSettings();
    openAdvanced();
    fireEvent.change(advancedSelect(/Striping/), { target: { value: 'off' } });
    expect(screen.getByLabelText('Playback quality: Custom')).toBeTruthy();
    expect(screen.getByText('· 1 changed')).toBeTruthy();

    fireEvent.click(screen.getByRole('button', { name: 'Reset advanced' }));
    expect(localStorage.getItem('gawk:stripe-mode')).toBeNull();
    expect(screen.getByLabelText('Playback quality: Balanced')).toBeTruthy();
  });

  // Decision 2, and the risk it carries: a preset is a *complete*
  // configuration, so picking one puts the advanced knobs back. The pill would
  // otherwise read "Balanced" over a forced-off striping setting.
  it('picking a preset resets the advanced knobs to their defaults', async () => {
    localStorage.setItem('gawk:stripe-mode', 'off');
    localStorage.setItem('gawk:parity-level', '0');
    localStorage.setItem('gawk:interpolation', '0');
    render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(sessions).toHaveLength(1));
    act(() => sessions[0].cbs.onConnected());
    expect(screen.getByLabelText('Playback quality: Custom')).toBeTruthy();

    tap(screen.getByLabelText(/^Playback quality:/));
    fireEvent.click(screen.getByRole('menuitemradio', { name: 'Balanced' }));

    expect(localStorage.getItem('gawk:stripe-mode')).toBeNull();
    expect(localStorage.getItem('gawk:parity-level')).toBeNull();
    expect(localStorage.getItem('gawk:interpolation')).toBe('1');
    expect(screen.getByLabelText('Playback quality: Balanced')).toBeTruthy();

    localStorage.removeItem('gawk:stripe-mode');
    localStorage.removeItem('gawk:parity-level');
    localStorage.removeItem('gawk:interpolation');
  });
});

// R16 gate + R22 MSE surface (docs/21 Decision 1, docs/27): the device gate,
// the hidden presentation video, and the Feature Gates overlay section. jsdom
// has no Element Fullscreen API, so a bare render is a *gated* device on the
// main-thread pipeline (worker unavailable in jsdom ⇒ tier 3 only, probe
// verdict false — docs/27 Decision 11); the non-gated cases install
// document.documentElement.requestFullscreen first.
describe('ViewerScreen presentation surface (R16 gate + R22 MSE)', () => {
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

  it('gated without worker support: no video element, gate row reads the failed verdict', async () => {
    const { container } = render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(sessions).toHaveLength(1));

    expect(container.querySelector('video')).toBeNull();

    openStats();
    const value = screen.getByText('NativeVideoFullscreen').nextSibling as HTMLElement;
    expect(value.textContent).toBe('✗');
    expect(value.getAttribute('title')).toBe('main-thread pipeline → pseudo');
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
      { name: 'NativeVideoFullscreen', active: false, detail: 'main-thread pipeline → pseudo' },
      // R29 finding 2: this sample carries no datagramBuffer, and the gate says
      // so rather than guessing — the distinction Copy diagnostics has to
      // preserve for a remote read to mean anything.
      { name: 'DatagramReceiveBuffer', active: false, detail: 'unknown' },
    ]);
    expect(blob.samples[0].stats.presentationSurface).toEqual({
      tier: null,
      armed: false,
      muxInitSegments: 0,
      muxMediaSegments: 0,
      muxErrors: 0,
      segmentsAppended: 0,
      appendErrors: 0,
      // docs/27 finding 7: no presenter on this path, so nothing was received,
      // queued or dropped, and there is no MediaSource to report a `streaming`
      // state for.
      segmentsReceived: 0,
      segmentsQueued: 0,
      segmentsDroppedNoInit: 0,
      mmsStreaming: null,
      // docs/27 finding 6: null until an append actually fails.
      lastAppendError: null,
      bufferedMs: null,
      bufferedAheadMs: null,
      // R22: no MediaSource on this path, so no live-duration verdict; and with
      // no audio in the fixture stream there is nothing to mux or probe.
      liveDuration: null,
      audioMode: 'none',
      audioTranscode: null,
      audioSegmentsAppended: 0,
      audioTrackActive: false,
      elementAudioTracks: null,
      muxAudioSegments: 0,
      muxAudioHoles: 0,
      bufferedRanges: null,
      // Element-side fields are null while no presentation <video> exists
      // (main-thread fallback ⇒ never armed ⇒ no element).
      elementReadyState: null,
      elementPaused: null,
      elementWidth: null,
      elementHeight: null,
      elementFrames: null,
    });
  });

  // R29 finding 2 (docs/34): the gate has to distinguish three states, because
  // the failure it exists for is a SILENT one — a browser that accepts the
  // assignment and ignores it looks identical to success from the call site,
  // and that is exactly how a fleet-wide no-op would ship unnoticed.
  it('DatagramReceiveBuffer gate reports applied, ignored and unsupported apart', async () => {
    render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(sessions).toHaveLength(1));

    // Nothing reported yet: not green. An absence of evidence is never "ok".
    openStats();
    let value = screen.getByText('DatagramReceiveBuffer').nextSibling as HTMLElement;
    expect(value.textContent).toBe('✗');
    expect(value.getAttribute('title')).toBe('unknown');

    act(() =>
      sessions[0].cbs.onStats({
        datagramBuffer: {
          property: 'incomingMaxBufferedDatagrams',
          requested: 256,
          defaultDepth: 8,
          effective: 256,
          applied: true,
          governsDrops: true,
        },
      }),
    );
    value = screen.getByText('DatagramReceiveBuffer').nextSibling as HTMLElement;
    expect(value.textContent).toBe('✓');
    expect(value.getAttribute('title')).toBe('256 datagrams (was 8)');

    // R29 finding 3: the write landed on the LEGACY attribute, which is not
    // the drop threshold. This must NOT read green — it is exactly the state
    // that shipped a confident gate over a fix that changed nothing.
    act(() =>
      sessions[0].cbs.onStats({
        datagramBuffer: {
          property: 'incomingHighWaterMark',
          requested: 256,
          defaultDepth: 1,
          effective: 256,
          applied: true,
          governsDrops: false,
        },
      }),
    );
    value = screen.getByText('DatagramReceiveBuffer').nextSibling as HTMLElement;
    expect(value.textContent).toBe('✗');
    expect(value.getAttribute('title')).toBe(
      'set 256 on incomingHighWaterMark (was 1), which does not govern drops',
    );

    // Set and ignored — the case the whole gate exists to make visible.
    act(() =>
      sessions[0].cbs.onStats({
        datagramBuffer: {
          property: 'incomingMaxBufferedDatagrams',
          requested: 256,
          defaultDepth: 4,
          effective: 4,
          applied: false,
          governsDrops: true,
        },
      }),
    );
    value = screen.getByText('DatagramReceiveBuffer').nextSibling as HTMLElement;
    expect(value.textContent).toBe('✗');
    expect(value.getAttribute('title')).toBe('requested 256, browser kept 4');

    // No attribute at all: the browser buffers whatever it buffers.
    act(() =>
      sessions[0].cbs.onStats({
        datagramBuffer: {
          property: null,
          requested: 256,
          defaultDepth: null,
          effective: null,
          applied: false,
          governsDrops: false,
        },
      }),
    );
    value = screen.getByText('DatagramReceiveBuffer').nextSibling as HTMLElement;
    expect(value.textContent).toBe('✗');
    expect(value.getAttribute('title')).toBe('unsupported → browser default');
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

// R28 (docs/33 §4.13): the operator's end of a phone call — "open stats and
// read me the session id" has to work end to end, from wire 0x0D to a row on
// screen. The token below is the shared golden one; its middle 12 bytes are
// the sessionId both the relay and the ingest name this session by.
describe('ViewerScreen telemetry session id (R28)', () => {
  const HELLO = {
    enabled: true,
    reportIntervalMs: 2000,
    token: '00012345000102030405060708090a0ba1a2a3a4a5a6a7a8',
    broadcastKey: '1a2b3c4d5e6f',
  };
  const SESSION_ID = '000102030405060708090a0b';

  const openStats = () => {
    fireEvent.contextMenu(screen.getByText('connecting').closest('div')!.parentElement!);
    fireEvent.click(screen.getByText('Stats'));
  };

  it('shows the id the relay handed this session', async () => {
    render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(sessions).toHaveLength(1));

    openStats();
    // Nothing has arrived yet: the overlay says so rather than guessing.
    expect((screen.getByText('Session id').nextSibling as HTMLElement).textContent).toBe('—');

    act(() => sessions[0].cbs.onTelemetryHello!(HELLO));
    const value = screen.getByText('Session id').nextSibling as HTMLElement;
    expect(value.textContent).toBe('00010203…');
    expect(value.getAttribute('title')).toBe(SESSION_ID);
  });

  it('names no session when the fleet collects nothing', async () => {
    render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(sessions).toHaveLength(1));

    act(() => sessions[0].cbs.onTelemetryHello!({ ...HELLO, enabled: false }));
    openStats();
    expect((screen.getByText('Session id').nextSibling as HTMLElement).textContent).toBe('—');
  });

  it('carries the whole id in Copy diagnostics, and never the token', async () => {
    const writeText = vi.fn(() => Promise.resolve());
    Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText } });
    render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(sessions).toHaveLength(1));
    act(() => sessions[0].cbs.onTelemetryHello!(HELLO));
    act(() => sessions[0].cbs.onStats({ interpolation: null }));

    openStats();
    fireEvent.click(screen.getByText('Copy diagnostics'));
    const body = (writeText.mock.calls[0] as unknown as [string])[0];
    expect(JSON.parse(body).telemetrySessionId).toBe(SESSION_ID);
    // The blob gets pasted into chats; the bearer half of the token must not
    // ride along (lib/telemetry.ts, docs/33 §4.2).
    expect(body).not.toContain(HELLO.token);
    expect(body).not.toContain('a1a2a3a4a5a6a7a8');
  });
});

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

// R37 (docs/40 §4.3): the server picker replaced the dev-only relay panel.
// The viewer needs it reachable in-session because a viewer is usually opened
// straight from a share link (an iPhone joining a code against a laptop's
// relay never passes through #/broadcast). Selecting a server is a deliberate
// teardown + reconnect: useViewerConnection depends on the store's resolved
// serverUrl.
describe('viewer server picker', () => {
  const openMenu = () =>
    fireEvent.contextMenu(screen.getByText('connecting').closest('div')!.parentElement!);

  beforeEach(() => {
    devEnv.value = true;
    delete window.__GAWK_CONFIG__;
    localStorage.removeItem('gawk.servers');
    localStorage.removeItem('gawk.selectedServer');
    const s = useTransportStore.getState();
    s.setSessionOverride(null);
    s.reloadFromStorage();
    s.selectServer('default');
  });

  afterEach(() => {
    delete window.__GAWK_CONFIG__;
  });

  // D6: the deployment-level gate removes the whole surface.
  it('is not offered when the deployment disallows custom relays', async () => {
    window.__GAWK_CONFIG__ = { allowCustomRelays: false };
    render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(sessions).toHaveLength(1));
    openMenu();
    expect(screen.queryByText('Server…')).toBeNull();
  });

  // F1: the picker is a production surface — offered outside dev builds too;
  // only the cert-hash field stays dev-gated.
  it('is offered outside a dev build, without the cert-hash field', async () => {
    devEnv.value = false;
    render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(sessions).toHaveLength(1));
    openMenu();
    fireEvent.click(screen.getByText('Server…'));
    expect(screen.getByRole('dialog', { name: 'Server picker' })).toBeTruthy();
    fireEvent.click(screen.getByText('Add a server'));
    expect(screen.getByLabelText('Server URL')).toBeTruthy();
    // Expand Advanced first — otherwise the field is merely collapsed, and
    // this assertion would pass in a dev build too.
    fireEvent.click(screen.getByRole('button', { name: /Advanced/ }));
    expect(screen.getByLabelText(/Publish secret/)).toBeTruthy();
    expect(screen.queryByLabelText(/Dev cert hash/)).toBeNull();
  });

  it('reconnects to a newly added, selected server, and persists it', async () => {
    render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(sessions).toHaveLength(1));
    expect(sessions[0].url).toBe('https://localhost:4433');

    openMenu();
    fireEvent.click(screen.getByText('Server…'));
    fireEvent.click(screen.getByText('Add a server'));
    fireEvent.change(screen.getByLabelText('Name'), { target: { value: 'Test relay' } });
    fireEvent.change(screen.getByLabelText('Server URL'), {
      target: { value: 'https://api.gawk.example:4433' },
    });
    // The credential fields sit behind the form's Advanced disclosure.
    fireEvent.click(screen.getByRole('button', { name: /Advanced/ }));
    fireEvent.change(screen.getByLabelText(/Dev cert hash/), {
      target: { value: 'abc123' },
    });
    fireEvent.click(screen.getByText('Save'));

    // Adding never selects (D2's explicit-act rule) — the select is its own
    // click, and THAT is the deliberate teardown + reconnect.
    expect(sessions).toHaveLength(1);
    fireEvent.click(screen.getByRole('option', { name: /Test relay/ }));

    await waitFor(() => expect(sessions).toHaveLength(2));
    expect(sessions[1].url).toBe('https://api.gawk.example:4433');
    expect((sessions[1].opts as { certHashHex: string }).certHashHex).toBe('abc123');
    // Persisted through the same store the broadcaster reads, as a saved
    // entry + selection (R37 replaced the single gawk.serverUrl key).
    const stored = JSON.parse(localStorage.getItem('gawk.servers')!) as Array<{
      id: string;
      url: string;
      certHashHex: string;
    }>;
    const added = stored.find((e) => e.url === 'https://api.gawk.example:4433');
    expect(added).toBeTruthy();
    expect(added!.certHashHex).toBe('abc123');
    expect(localStorage.getItem('gawk.selectedServer')).toBe(added!.id);
  });

  // The situation the picker must cover: a viewer aimed at a relay that will
  // not answer. It must be reachable *while the error card is up* — the old
  // dev panel's first implementation was covered by the error card exactly
  // here. jsdom cannot see stacking; this pins the requirement behaviourally,
  // and the layering itself is verified in a real browser.
  it('is reachable while the connection-failed card is showing', async () => {
    sessionState.failStartWith = new Error('WebTransportError: Opening handshake failed.');
    render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(screen.getByText('Streamer offline')).toBeTruthy());

    fireEvent.contextMenu(screen.getByText('error').closest('div')!.parentElement!);
    fireEvent.click(screen.getByText('Server…'));

    expect(screen.getByRole('dialog', { name: 'Server picker' })).toBeTruthy();
    // The error card is still mounted underneath — the panel is layered over
    // it, not swapped for it.
    expect(screen.getByText('Streamer offline')).toBeTruthy();
  });

  it('closes without reconnecting when dismissed', async () => {
    render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(sessions).toHaveLength(1));

    openMenu();
    fireEvent.click(screen.getByText('Server…'));
    fireEvent.click(screen.getByText('Done'));

    // Dismissal plays an exit animation before it unmounts.
    await waitFor(() =>
      expect(screen.queryByRole('dialog', { name: 'Server picker' })).toBeNull(),
    );
    expect(sessions).toHaveLength(1);
    expect(useTransportStore.getState().serverUrl).toBe('https://localhost:4433');
  });

  // F2: the in-session indicator renders on this screen for a non-default
  // resolution — and not at all on the default server.
  it('shows the in-session indicator only on a non-default server', async () => {
    render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(sessions).toHaveLength(1));
    expect(screen.queryByTestId('server-indicator')).toBeNull();

    act(() => {
      useTransportStore.getState().setSessionOverride('https://link.example:4433');
    });
    expect(screen.getByTestId('server-indicator').textContent).toContain('link.example:4433');
  });
});

// The macOS idle-dim bug: the viewer paints a canvas, so the browser holds no
// display power-save blocker and the OS dims/sleeps the screen mid-stream
// (fullscreen included). The hook's own rules are pinned in
// lib/useWakeLock.test.ts; what this covers is the wiring — that the lock
// tracks the *watching* status and is given back when the stream is not on
// screen.
describe('ViewerScreen screen wake lock', () => {
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

  it('holds the display awake while watching and releases when the broadcast ends', async () => {
    render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(sessions).toHaveLength(1));
    // Still connecting — nothing to keep awake for yet.
    expect(locks).toHaveLength(0);

    await act(async () => sessions[0].cbs.onConnected());
    await waitFor(() => expect(locks).toHaveLength(1));
    expect(locks[0].released).toBe(false);

    await act(async () => sessions[0].cbs.onEnded());
    await waitFor(() => expect(locks[0].released).toBe(true));
  });

  it('keeps the lock across a reconnect — a blip is still someone watching', async () => {
    render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(sessions).toHaveLength(1));
    await act(async () => sessions[0].cbs.onConnected());
    await waitFor(() => expect(locks).toHaveLength(1));

    await act(async () => sessions[0].cbs.onReconnecting({ attempt: 1, reason: 'blip' }));
    await act(async () => {});
    expect(locks[0].released).toBe(false);
    expect(locks).toHaveLength(1);
  });

  it('releases the lock on unmount', async () => {
    const { unmount } = render(<ViewerScreen broadcastId="AB2CD3" />);
    await waitFor(() => expect(sessions).toHaveLength(1));
    await act(async () => sessions[0].cbs.onConnected());
    await waitFor(() => expect(locks).toHaveLength(1));

    unmount();
    await act(async () => {});
    expect(locks[0].released).toBe(true);
  });
});
