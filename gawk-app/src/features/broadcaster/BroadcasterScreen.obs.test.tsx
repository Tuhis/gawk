// @vitest-environment jsdom
//
// R33 (docs/83 §5.3 D, OD9): the way in to the OBS page on the broadcaster
// page. U1 (the card is unchanged), U2 (visible without opening anything),
// U3 (none where it can't work), U6 (a returning user), U7 (one status read
// on load, only with a saved key) and U5 (the key never reaches the
// diagnostics copy).
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import type { BroadcastCallbacks, BroadcastStats } from '../../transport/broadcaster';

const { scripts } = vi.hoisted(() => ({ scripts: [] as Array<(cbs: BroadcastCallbacks) => Promise<void>> }));

vi.mock('./workerBroadcastSession', () => ({
  createBroadcastSession: async (
    _config: unknown,
    _url: string,
    _opts: unknown,
    callbacks: BroadcastCallbacks,
  ) => {
    const script = scripts.shift();
    if (!script) throw new Error('test bug: no broadcast expected');
    return {
      start: () => script(callbacks),
      stop: async () => callbacks.onEnded(),
      setLadder: () => {},
      setEncoderSettings: () => {},
      setDelivery: () => {},
    };
  },
}));

import { BroadcasterScreen } from './BroadcasterScreen';
import { useTransportStore } from '../../state/transportStore';
import { acceptCurrentTerms } from '../terms/acceptance';
import { parseObsKey, saveObsKey } from '../obs/key';

const WHIP = 'https://whip.example';
const TOKEN = '0123456789abcdef0123456789abcdef';
const KEY = parseObsKey(`gawk1_K7Q2XM_${TOKEN}`)!;

const UA = {
  windows:
    'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36',
  mac: 'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/26.0 Safari/605.1.15',
  linux: 'Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36',
  android:
    'Mozilla/5.0 (Linux; Android 16; Pixel 9) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Mobile Safari/537.36',
  iphone:
    'Mozilla/5.0 (iPhone; CPU iPhone OS 26_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/26.0 Mobile/15E148 Safari/604.1',
  chromeos:
    'Mozilla/5.0 (X11; CrOS x86_64 16000.0.0) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36',
};

function asDevice(ua: string, touchPoints = 0) {
  vi.spyOn(navigator, 'userAgent', 'get').mockReturnValue(ua);
  Object.defineProperty(navigator, 'maxTouchPoints', { value: touchPoints, configurable: true });
}

let statusReads: Array<{ url: string; auth: string | null }> = [];
let statusState = 'idle';

const wayIn = () => screen.queryByTestId('obs-way-in');
// The pre-start card: the GlassPanel holding the Start button.
const card = () => screen.getByRole('heading', { name: 'Start a stream' }).parentElement!;

beforeEach(() => {
  window.__GAWK_CONFIG__ = { whipGatewayUrl: WHIP, requirePublishSecret: false };
  localStorage.clear();
  sessionStorage.clear();
  acceptCurrentTerms();
  scripts.length = 0;
  useTransportStore.getState().setSessionOverride(null);
  useTransportStore.getState().setRelayLinkNote(null);
  window.location.hash = '#/broadcast';
  statusReads = [];
  statusState = 'idle';
  vi.stubGlobal('fetch', async (url: string, init?: RequestInit) => {
    const headers = (init?.headers ?? {}) as Record<string, string>;
    statusReads.push({ url: String(url), auth: headers.Authorization ?? null });
    return { ok: true, status: 200, json: async () => ({ state: statusState, warnings: [] }) } as Response;
  });
  asDevice(UA.windows);
  Object.defineProperty(HTMLMediaElement.prototype, 'srcObject', { configurable: true, get: () => null, set: () => {} });
  HTMLMediaElement.prototype.play = () => Promise.resolve();
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  Reflect.deleteProperty(navigator, 'maxTouchPoints');
  delete window.__GAWK_CONFIG__;
  localStorage.clear();
  window.location.hash = '';
});

// Every button and link inside the card, by accessible name.
function cardControls(): string[] {
  return [...card().querySelectorAll('button, a')].map((el) => `${el.tagName.toLowerCase()}:${el.textContent?.trim()}`);
}

describe('U1: new users aren’t drawn in', () => {
  it('leaves the card’s buttons and links as they are, and puts the way in outside the GlassPanel with the chip', () => {
    window.__GAWK_CONFIG__ = { requirePublishSecret: false };
    const { unmount } = render(<BroadcasterScreen />);
    const before = cardControls();
    expect(wayIn()).toBeNull();
    unmount();

    window.__GAWK_CONFIG__ = { whipGatewayUrl: WHIP, requirePublishSecret: false };
    render(<BroadcasterScreen />);
    expect(cardControls()).toEqual(before);
    expect(before).toEqual([
      'button:Start a stream',
      'a:Open in the desktop app',
      'button:Sharing tips',
      // The collapsed tips' download links.
      'a:native broadcaster',
      'a:native broadcaster',
      'button:Settings',
      'button:Room',
      'button:Home',
    ]);
    const line = wayIn()!;
    expect(card().contains(line)).toBe(false);
    expect(card().className).toMatch(/glass/);
    expect(within(line).getByTestId('advanced-chip').textContent).toBe('Advanced');
    // Start a stream is still the only primary action.
    expect(within(card()).getAllByRole('button').filter((b) => /primary/.test(b.className))).toHaveLength(1);
  });
});

describe('U2: OBS isn’t hidden knowledge', () => {
  it('shows the way in on a desktop without opening anything, linking #/broadcast/obs with the query', () => {
    window.location.hash = '#/broadcast?relay=https%3A%2F%2Flocalhost%3A4433';
    render(<BroadcasterScreen />);
    expect(wayIn()!.textContent).toContain('Already stream with OBS Studio?');
    expect(screen.getByRole('link', { name: 'Stream from OBS' }).getAttribute('href')).toBe(
      '#/broadcast/obs?relay=https%3A%2F%2Flocalhost%3A4433',
    );
  });

  it.each([
    ['macOS', UA.mac, 0],
    ['Linux', UA.linux, 0],
  ])('on %s too', (_name, ua, touch) => {
    asDevice(ua, touch);
    render(<BroadcasterScreen />);
    expect(wayIn()).not.toBeNull();
  });
});

describe('U3: no way in where it can’t work', () => {
  it('none without whipGatewayUrl', () => {
    window.__GAWK_CONFIG__ = { requirePublishSecret: false };
    render(<BroadcasterScreen />);
    expect(wayIn()).toBeNull();
  });

  it('none when the selected server isn’t the deployment’s default', () => {
    useTransportStore.getState().setSessionOverride('https://relay.friend.example');
    render(<BroadcasterScreen />);
    expect(wayIn()).toBeNull();
  });

  it.each([
    ['Android', UA.android, 5],
    ['iOS', UA.iphone, 5],
    ['iPadOS (desktop UA, touch points)', UA.mac, 5],
    ['ChromeOS', UA.chromeos, 0],
  ])('none on %s, where R67 hides its offer', (_name, ua, touch) => {
    asDevice(ua, touch);
    render(<BroadcasterScreen />);
    expect(wayIn()).toBeNull();
  });

  it('none while a start is in flight', async () => {
    let release: () => void = () => {};
    scripts.push(() => new Promise<void>((r) => (release = r)));
    render(<BroadcasterScreen />);
    fireEvent.click(screen.getByRole('button', { name: /start a stream/i }));
    await screen.findByText('Starting…');
    expect(wayIn()).toBeNull();
    release();
  });
});

describe('U6 and U7: a returning OBS user', () => {
  it('reads no status without a saved key', async () => {
    render(<BroadcasterScreen />);
    await act(async () => {});
    expect(statusReads).toEqual([]);
  });

  it('names the saved key’s code, after one status read on load', async () => {
    saveObsKey(useTransportStore.getState().serverUrl, KEY);
    render(<BroadcasterScreen />);
    await waitFor(() => expect(statusReads).toHaveLength(1));
    expect(statusReads[0]).toEqual({ url: `${WHIP}/whip/status`, auth: `Bearer ${KEY.key}` });
    expect(wayIn()!.textContent).toBe('Your OBS key for K7Q2XM is saved · OpenAdvanced');
    expect(screen.getByRole('link', { name: 'Open' }).getAttribute('href')).toBe('#/broadcast/obs');
  });

  it('becomes the LIVE glass pill when OBS is streaming on it, and reads only once', async () => {
    vi.useFakeTimers();
    try {
      statusState = 'live';
      saveObsKey(useTransportStore.getState().serverUrl, KEY);
      render(<BroadcasterScreen />);
      await act(async () => {
        await vi.advanceTimersByTimeAsync(0);
      });
      const line = wayIn()!;
      expect(line.textContent).toBe('LIVEOBS is streaming on K7Q2XM · Open');
      expect(card().contains(line)).toBe(false);
      await act(async () => {
        await vi.advanceTimersByTimeAsync(30_000);
      });
      expect(statusReads).toHaveLength(1);
    } finally {
      vi.useRealTimers();
    }
  });
});

describe('U5: the key never reaches the diagnostics copy', () => {
  it('Copy diagnostics on a live broadcast carries no part of the saved token', async () => {
    saveObsKey(useTransportStore.getState().serverUrl, KEY);
    const writeText = vi.fn(() => Promise.resolve());
    Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText } });
    scripts.push(async (cbs) => {
      cbs.onBroadcastId?.('K7Q2XM');
      cbs.onSourceStream({ getTracks: () => [] } as unknown as MediaStream);
      cbs.onStats({ audioState: 'off' } as BroadcastStats);
    });
    render(<BroadcasterScreen />);
    fireEvent.click(screen.getByRole('button', { name: /start a stream/i }));
    await screen.findByText('LIVE');
    fireEvent.click(screen.getByRole('button', { name: 'Show stats' }));
    fireEvent.click(screen.getByRole('button', { name: 'Copy diagnostics' }));
    await waitFor(() => expect(writeText).toHaveBeenCalled());
    const text = (writeText.mock.calls[0] as unknown as [string])[0];
    expect(text).toContain('"surface": "broadcaster"');
    expect(text).not.toContain(TOKEN);
    expect(text).not.toContain('gawk1_');
    expect(window.location.href).not.toContain(TOKEN);
  });
});
