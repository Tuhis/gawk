// @vitest-environment jsdom
//
// R33 (docs/83 §5.4, WG8): the OBS page. The mint is mocked here (mint.test.ts
// covers the transport); the gateway's status endpoint is a stubbed fetch.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';

const { mintCalls, mintScript } = vi.hoisted(() => ({
  mintCalls: [] as Array<{ serverUrl: string; opts: { publishSecret?: string; certHashHex?: string } }>,
  // One result per mint, consumed in order.
  mintScript: [] as Array<() => Promise<unknown>>,
}));

vi.mock('./mint', async (importActual) => ({
  ...(await importActual<typeof import('./mint')>()),
  mintObsKey: async (serverUrl: string, opts: { publishSecret?: string; certHashHex?: string }) => {
    mintCalls.push({ serverUrl, opts });
    const next = mintScript.shift();
    if (!next) throw new Error('test bug: no mint scripted');
    return next();
  },
}));

import { ObsPage, OBS_GUIDE_URL } from './ObsPage';
import { MintError } from './mint';
import { loadObsKey, parseObsKey, saveObsKey, type ObsKey } from './key';
import { POLL_INTERVAL_MS } from './useWhipStatus';
import { acceptCurrentTerms } from '../terms/acceptance';
import { useTransportStore } from '../../state/transportStore';
import { SECRET_MAYBE_REQUIRED_NOTE } from '../broadcaster/PublishPrompts';
import { log } from '../../lib/logger';

const WHIP = 'https://whip.example';
const TOKEN = '0123456789abcdef0123456789abcdef';
const TOKEN2 = 'fedcba9876543210fedcba9876543210';
const KEY: ObsKey = parseObsKey(`gawk1_K7Q2XM_${TOKEN}`)!;
const KEY2: ObsKey = parseObsKey(`gawk1_ZZ3YXW_${TOKEN2}`)!;
const ANDROID_UA =
  'Mozilla/5.0 (Linux; Android 16; Pixel 9) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Mobile Safari/537.36';

interface FetchCall {
  url: string;
  method: string;
  auth: string | null;
}
let fetchCalls: FetchCall[] = [];
// What GET /whip/status answers next; a function so a test can change it.
let statusBody: () => { status: number; body?: unknown } = () => ({ status: 200, body: { state: 'idle', warnings: [] } });
let writeText: ReturnType<typeof vi.fn>;

function serverUrl(): string {
  return useTransportStore.getState().serverUrl;
}

function setVisibility(state: 'visible' | 'hidden'): void {
  Object.defineProperty(document, 'visibilityState', { value: state, configurable: true });
  document.dispatchEvent(new Event('visibilitychange'));
}

const gets = () => fetchCalls.filter((c) => c.method === 'GET');

beforeEach(() => {
  window.__GAWK_CONFIG__ = { whipGatewayUrl: WHIP, requirePublishSecret: false };
  localStorage.clear();
  acceptCurrentTerms();
  useTransportStore.getState().setSessionOverride(null);
  useTransportStore.getState().setPublishSecret('');
  window.location.hash = '#/broadcast/obs';
  mintCalls.length = 0;
  mintScript.length = 0;
  fetchCalls = [];
  statusBody = () => ({ status: 200, body: { state: 'idle', warnings: [] } });
  vi.stubGlobal('fetch', async (url: string, init?: RequestInit) => {
    const headers = (init?.headers ?? {}) as Record<string, string>;
    fetchCalls.push({ url: String(url), method: init?.method ?? 'GET', auth: headers.Authorization ?? null });
    if (init?.method === 'DELETE') return { ok: true, status: 204, json: async () => ({}) } as Response;
    const { status, body } = statusBody();
    return { ok: status >= 200 && status < 300, status, json: async () => body } as Response;
  });
  writeText = vi.fn(() => Promise.resolve());
  Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText } });
  setVisibility('visible');
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  delete window.__GAWK_CONFIG__;
  localStorage.clear();
  window.location.hash = '';
});

describe('U3: no OBS where it can’t work', () => {
  it('says so without whipGatewayUrl, with a link back keeping the query', () => {
    window.__GAWK_CONFIG__ = { requirePublishSecret: false };
    window.location.hash = '#/broadcast/obs?nick=x';
    render(<ObsPage />);
    expect(screen.getByRole('heading', { name: 'This server doesn’t take streams from OBS' })).toBeTruthy();
    expect(screen.getByRole('link', { name: '← Back' }).getAttribute('href')).toBe('#/broadcast?nick=x');
    expect(screen.queryByRole('button', { name: 'Create stream key' })).toBeNull();
    expect(fetchCalls).toEqual([]);
  });

  it('says so when another server is selected (the gateway publishes only to its own relay, OD4)', () => {
    window.location.hash = '#/broadcast/obs?relay=https%3A%2F%2Frelay.friend.example';
    useTransportStore.getState().setSessionOverride('https://relay.friend.example');
    saveObsKey(serverUrl(), KEY);
    render(<ObsPage />);
    expect(screen.getByRole('heading', { name: 'This server doesn’t take streams from OBS' })).toBeTruthy();
    expect(screen.getByRole('link', { name: '← Back' }).getAttribute('href')).toBe(
      '#/broadcast?relay=https%3A%2F%2Frelay.friend.example',
    );
    expect(fetchCalls).toEqual([]);
  });

  it('gives a phone opening the route directly the full page', () => {
    vi.spyOn(navigator, 'userAgent', 'get').mockReturnValue(ANDROID_UA);
    render(<ObsPage />);
    expect(screen.getByRole('heading', { name: 'Stream from OBS Studio' })).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Create stream key' })).toBeTruthy();
  });
});

describe('S1: first visit, no key', () => {
  it('opens with the browser alternative and what you need, before Create stream key', () => {
    render(<ObsPage />);
    expect(screen.getByText('broadcast · OBS Studio')).toBeTruthy();
    expect(screen.getByTestId('advanced-chip').textContent).toBe('Advanced');
    expect(screen.getByText('New to streaming? The browser takes one click and needs no other app.')).toBeTruthy();
    expect(screen.getByRole('link', { name: 'Start a stream in the browser' }).getAttribute('href')).toBe('#/broadcast');
    const needs = screen.getByRole('region', { name: 'You need' });
    expect(within(needs).getAllByRole('listitem').map((li) => li.textContent)).toEqual([
      'OBS Studio 30 or later (or another WHIP app, such as FFmpeg or GStreamer)',
      'An H.264 video encoder (NVENC, AMF, QuickSync or x264)',
      'A few minutes in OBS’s Settings',
    ]);
    // The way back comes before the primary action in the document.
    const strip = screen.getByRole('link', { name: 'Start a stream in the browser' });
    const create = screen.getByRole('button', { name: 'Create stream key' });
    expect(strip.compareDocumentPosition(create) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Already have a key? Paste it' })).toBeTruthy();
  });

  it('has the footer: Back keeping the query and the OBS setup guide, no key actions yet', () => {
    window.location.hash = '#/broadcast/obs?relay=https%3A%2F%2Flocalhost%3A4433';
    render(<ObsPage />);
    expect(screen.getByRole('link', { name: '← Back' }).getAttribute('href')).toBe(
      '#/broadcast?relay=https%3A%2F%2Flocalhost%3A4433',
    );
    const guide = screen.getByRole('link', { name: 'OBS setup guide ↗' });
    expect(guide.getAttribute('href')).toBe('https://github.com/Tuhis/gawk/blob/main/docs/obs-setup.md');
    expect(guide.getAttribute('href')).toBe(OBS_GUIDE_URL);
    expect(screen.queryByRole('button', { name: 'Reset key' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Forget key' })).toBeNull();
  });
});

describe('mint (D3)', () => {
  it('Create stream key mints on the selected server, saves the key and shows the steps', async () => {
    mintScript.push(async () => KEY);
    render(<ObsPage />);
    fireEvent.click(screen.getByRole('button', { name: 'Create stream key' }));
    await screen.findByText('Bearer Token');
    expect(mintCalls).toEqual([{ serverUrl: serverUrl(), opts: { publishSecret: '', certHashHex: expect.any(String) } }]);
    expect(loadObsKey(serverUrl())).toEqual(KEY);
    expect(screen.getByTestId('obs-whip-url').textContent).toBe(`${WHIP}/whip`);
    expect(screen.getByTestId('obs-key').textContent).toBe('gawk1_K7Q2XM_••••');
  });

  it('asks for the publish secret first with the broadcaster’s modal when the deployment requires one', async () => {
    window.__GAWK_CONFIG__ = { whipGatewayUrl: WHIP, requirePublishSecret: true };
    useTransportStore.getState().setPublishSecret('saved-secret');
    mintScript.push(async () => KEY);
    render(<ObsPage />);
    fireEvent.click(screen.getByRole('button', { name: 'Create stream key' }));
    const dialog = screen.getByRole('dialog', { name: 'Publish secret' });
    expect(within(dialog).getByText('This relay requires a secret to broadcast.')).toBeTruthy();
    const field = within(dialog).getByPlaceholderText('shared secret') as HTMLInputElement;
    expect(field.value).toBe('saved-secret');
    expect(mintCalls).toEqual([]);
    fireEvent.change(field, { target: { value: ' new-secret ' } });
    fireEvent.click(within(dialog).getByRole('button', { name: 'Create stream key' }));
    await screen.findByText('Bearer Token');
    expect(mintCalls[0].opts.publishSecret).toBe('new-secret');
    expect(useTransportStore.getState().publishSecret).toBe('new-secret');
  });

  it('offers the secret modal when the relay refuses the dial (secret missing or wrong)', async () => {
    mintScript.push(async () => {
      throw new MintError('connect', new Error('Opening handshake failed.'));
    });
    render(<ObsPage />);
    fireEvent.click(screen.getByRole('button', { name: 'Create stream key' }));
    const dialog = await screen.findByRole('dialog', { name: 'Publish secret' });
    expect(within(dialog).getByText(SECRET_MAYBE_REQUIRED_NOTE)).toBeTruthy();
    expect(screen.getByRole('alert').textContent).toBe('Couldn’t create a key: Opening handshake failed.');
    // Retrying with a secret mints.
    mintScript.push(async () => KEY);
    fireEvent.change(within(dialog).getByPlaceholderText('shared secret'), { target: { value: 's3cret' } });
    fireEvent.click(within(dialog).getByRole('button', { name: 'Create stream key' }));
    await screen.findByText('Bearer Token');
    expect(mintCalls.map((c) => c.opts.publishSecret)).toEqual(['', 's3cret']);
  });

  it('shows a session-phase failure without asking for a secret', async () => {
    mintScript.push(async () => {
      throw new MintError('session', new Error('the server sent no stream key'));
    });
    render(<ObsPage />);
    fireEvent.click(screen.getByRole('button', { name: 'Create stream key' }));
    expect((await screen.findByRole('alert')).textContent).toBe('Couldn’t create a key: the server sent no stream key');
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(loadObsKey(serverUrl())).toBeNull();
  });

  it('carries the broadcaster’s one-time terms acknowledgment', async () => {
    localStorage.clear();
    mintScript.push(async () => KEY);
    render(<ObsPage />);
    fireEvent.click(screen.getByRole('button', { name: 'Create stream key' }));
    const terms = screen.getByRole('dialog', { name: 'Terms of use' });
    expect(mintCalls).toEqual([]);
    fireEvent.click(within(terms).getByRole('button', { name: 'Agree & continue' }));
    await screen.findByText('Bearer Token');
    expect(mintCalls).toHaveLength(1);
  });
});

describe('paste a key (§5.7)', () => {
  it('checks the format only, then saves the key and starts polling', async () => {
    render(<ObsPage />);
    fireEvent.click(screen.getByRole('button', { name: 'Already have a key? Paste it' }));
    const field = screen.getByLabelText('Stream key');
    fireEvent.change(field, { target: { value: 'gawk1_nope' } });
    fireEvent.click(screen.getByRole('button', { name: 'Save key' }));
    expect(screen.getByRole('alert').textContent).toBe('That isn’t a gawk stream key. It starts with gawk1_.');
    expect(loadObsKey(serverUrl())).toBeNull();
    fireEvent.change(field, { target: { value: `  ${KEY.key} ` } });
    fireEvent.click(screen.getByRole('button', { name: 'Save key' }));
    await screen.findByText('Waiting for OBS…');
    expect(loadObsKey(serverUrl())).toEqual(KEY);
    expect(gets()[0]).toEqual({ url: `${WHIP}/whip/status`, method: 'GET', auth: `Bearer ${KEY.key}` });
  });
});

describe('S2: the key, masked, with Show and Copy (U5)', () => {
  it('copies the whole key while it stays masked, and Show reveals it', async () => {
    saveObsKey(serverUrl(), KEY);
    render(<ObsPage />);
    const shown = screen.getByTestId('obs-key');
    expect(shown.textContent).toBe('gawk1_K7Q2XM_••••');
    expect(document.body.textContent).not.toContain(TOKEN);
    fireEvent.click(screen.getByRole('button', { name: 'Copy key' }));
    await waitFor(() => expect(writeText).toHaveBeenCalledWith(KEY.key));
    expect(shown.textContent).toBe('gawk1_K7Q2XM_••••');
    const show = screen.getByRole('button', { name: 'Show' });
    expect(show.getAttribute('aria-pressed')).toBe('false');
    fireEvent.click(show);
    expect(shown.textContent).toBe(KEY.key);
    fireEvent.click(screen.getByRole('button', { name: 'Hide' }));
    expect(shown.textContent).toBe('gawk1_K7Q2XM_••••');
  });

  it('shows OBS’s settings in OBS’s words, the WHIP URL with Copy, and the join code with Copy link', async () => {
    saveObsKey(serverUrl(), KEY);
    render(<ObsPage />);
    for (const label of ['Service', 'Server', 'Bearer Token', 'Simulcast', 'Output Mode', 'Video Encoder', 'Keyframe Interval', 'Audio Encoder']) {
      expect(screen.getByText(label).tagName).toBe('DT');
    }
    expect(screen.getByText('not 0 (auto), which is often 4 s or more')).toBeTruthy();
    expect(
      screen.getByText('The token works like a password: anyone who has it can stream on your code. Keep it off screen.'),
    ).toBeTruthy();
    fireEvent.click(screen.getAllByRole('button', { name: 'Copy' })[0]);
    await waitFor(() => expect(writeText).toHaveBeenCalledWith(`${WHIP}/whip`));
    const joinOn = screen.getByRole('region', { name: 'Your viewers join on' });
    expect(within(joinOn).getByRole('img', { name: 'Code K7Q2XM' }).textContent).toBe('K7Q2XM');
    fireEvent.click(within(joinOn).getByRole('button', { name: /Copy link/ }));
    await waitFor(() => expect(writeText).toHaveBeenLastCalledWith(expect.stringMatching(/#\/view\/K7Q2XM$/)));
    expect((await screen.findByRole('status')).textContent).toContain('Waiting for OBS…');
    expect(screen.getByText('This page updates on its own when OBS connects.')).toBeTruthy();
  });
});

describe('the states (§5.4, §5.6)', () => {
  it('S3: live, with the topbar pieces, the measured grid and Watch as a viewer', async () => {
    saveObsKey(serverUrl(), KEY);
    statusBody = () => ({
      status: 200,
      body: {
        state: 'live',
        codec: 'H.264',
        resolution: { width: 1920, height: 1080 },
        fps: 60,
        gopMs: 1000,
        bitrateKbps: 6000,
        lossPct: 0.1,
        viewers: 3,
        warnings: [],
      },
    });
    render(<ObsPage />);
    await screen.findByRole('heading', { name: 'You’re live from OBS' });
    expect(screen.getByText('LIVE')).toBeTruthy();
    expect(screen.getByText('3 watching')).toBeTruthy();
    expect(screen.getByText('1920×1080 · 60 fps')).toBeTruthy();
    expect(screen.getByText('every 1.0 s')).toBeTruthy();
    expect(screen.getByText('6.0 Mb/s · 0.1 % lost')).toBeTruthy();
    expect(screen.getByRole('link', { name: 'Watch as a viewer ↗' }).getAttribute('href')).toMatch(/#\/view\/K7Q2XM$/);
    expect(screen.getByText('Stop Streaming in OBS ends the broadcast. Next time, your code is K7Q2XM again.')).toBeTruthy();
    // The steps fold into a closed disclosure.
    const details = screen.getByText('OBS settings for this key').closest('details')!;
    expect(details.open).toBe(false);
    expect(screen.queryByTestId('obs-warning')).toBeNull();
  });

  it('S3 with a warning: the amber banner, and the keyframes cell turns amber', async () => {
    saveObsKey(serverUrl(), KEY);
    statusBody = () => ({ status: 200, body: { state: 'live', gopMs: 2000, warnings: [{ reason: 'gop_long', gopMs: 2000 }] } });
    render(<ObsPage />);
    const banner = await screen.findByTestId('obs-warning');
    expect(banner.textContent).toContain('OBS sends a keyframe every 2.0 s.');
    expect(banner.closest('[role="status"]')).toBeTruthy();
    expect(screen.getByTestId('obs-cell-keyframes').className).toMatch(/cellWarn/);
  });

  it('S4: gawk ended the stream, the countdown, and Retry now sends DELETE and turns into S5', async () => {
    saveObsKey(serverUrl(), KEY);
    let cleared = false;
    statusBody = () =>
      cleared
        ? { status: 200, body: { state: 'ended', warnings: [] } }
        : { status: 200, body: { state: 'ended', warnings: [], lastError: { reason: 'bframes' }, retryAfterMs: 48_000 } };
    render(<ObsPage />);
    await screen.findByRole('heading', { name: 'gawk ended your stream' });
    expect(screen.getByText('Viewers on K7Q2XM are waiting; they pick the stream up again when you’re back.')).toBeTruthy();
    const alert = screen.getByRole('alert');
    expect(alert.textContent).toContain('Your encoder sends B-frames, which gawk can’t play live.');
    expect(screen.getByText(/that is gawk turning it away for 0:4\d more/)).toBeTruthy();
    cleared = true;
    fireEvent.click(screen.getByRole('button', { name: 'Retry now' }));
    await screen.findByRole('heading', { name: 'Your stream has ended' });
    expect(fetchCalls.find((c) => c.method === 'DELETE')).toEqual({
      url: `${WHIP}/whip/status`,
      method: 'DELETE',
      auth: `Bearer ${KEY.key}`,
    });
    expect(screen.getByText('Press Start Streaming in OBS to go live again on K7Q2XM.').closest('[role="status"]')).toBeTruthy();
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('a refusal without a backoff: the red box, no countdown, no Retry now', async () => {
    saveObsKey(serverUrl(), KEY);
    statusBody = () => ({ status: 200, body: { state: 'idle', warnings: [], lastError: { reason: 'gateway_full' } } });
    render(<ObsPage />);
    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toBe(
      'This server is streaming as many OBS broadcasts as it can.Wait a minute, then press Start Streaming in OBS again.',
    );
    expect(screen.queryByRole('button', { name: 'Retry now' })).toBeNull();
    expect(screen.queryByText(/turning it away/)).toBeNull();
  });

  it('S5 after OBS vanished: "OBS disconnected", not red', async () => {
    saveObsKey(serverUrl(), KEY);
    statusBody = () => ({ status: 200, body: { state: 'ended', warnings: [], lastError: { reason: 'lost' } } });
    render(<ObsPage />);
    await screen.findByRole('heading', { name: 'OBS disconnected' });
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('401 (a malformed key) gets a sentence, never the status code', async () => {
    saveObsKey(serverUrl(), KEY);
    statusBody = () => ({ status: 401 });
    render(<ObsPage />);
    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toContain('The server can’t read this key.');
    expect(alert.textContent).not.toContain('401');
  });
});

describe('Forget key and Reset key (§5.7)', () => {
  it('Forget asks first, then removes the key and stops polling', async () => {
    saveObsKey(serverUrl(), KEY);
    render(<ObsPage />);
    await screen.findByText('Waiting for OBS…');
    fireEvent.click(screen.getByRole('button', { name: 'Forget key' }));
    const dialog = screen.getByRole('alertdialog', { name: 'Forget key' });
    expect(within(dialog).getByText('Forget this key?')).toBeTruthy();
    expect(dialog.textContent).toContain(
      'OBS keeps streaming with it; this browser just stops showing its status. Copy it from OBS (Settings → Stream) to see it here again.',
    );
    fireEvent.click(within(dialog).getByRole('button', { name: 'Cancel' }));
    expect(loadObsKey(serverUrl())).toEqual(KEY);
    fireEvent.click(screen.getByRole('button', { name: 'Forget key' }));
    fireEvent.click(within(screen.getByRole('alertdialog')).getByRole('button', { name: 'Forget key' }));
    expect(loadObsKey(serverUrl())).toBeNull();
    expect(screen.getByRole('button', { name: 'Create stream key' })).toBeTruthy();
  });

  it('Reset asks first with the danger button, then mints a new key and code', async () => {
    saveObsKey(serverUrl(), KEY);
    mintScript.push(async () => KEY2);
    render(<ObsPage />);
    fireEvent.click(screen.getByRole('button', { name: 'Reset key' }));
    const dialog = screen.getByRole('alertdialog', { name: 'Reset key' });
    expect(dialog.textContent).toContain(
      'You get a new key and a new code. Paste the new key into OBS before you stream again, and share the new link.',
    );
    expect(dialog.textContent).toContain(
      'The old key keeps working on K7Q2XM; gawk can’t switch it off. If it leaked, ask whoever runs this server to ban K7Q2XM.',
    );
    const reset = within(dialog).getByRole('button', { name: 'Reset key' });
    expect(reset.className).toMatch(/danger/);
    expect(within(dialog).getByRole('button', { name: 'Cancel' }).className).not.toMatch(/danger/);
    fireEvent.click(within(dialog).getByRole('button', { name: 'Cancel' }));
    expect(mintCalls).toEqual([]);
    fireEvent.click(screen.getByRole('button', { name: 'Reset key' }));
    fireEvent.click(within(screen.getByRole('alertdialog')).getByRole('button', { name: 'Reset key' }));
    await screen.findByRole('img', { name: 'Code ZZ3YXW' });
    expect(mintCalls).toHaveLength(1);
    expect(loadObsKey(serverUrl())).toEqual(KEY2);
  });

  it('a failed Reset keeps the old key', async () => {
    saveObsKey(serverUrl(), KEY);
    mintScript.push(async () => {
      throw new MintError('session', new Error('the server sent no stream key'));
    });
    render(<ObsPage />);
    fireEvent.click(screen.getByRole('button', { name: 'Reset key' }));
    fireEvent.click(within(screen.getByRole('alertdialog')).getByRole('button', { name: 'Reset key' }));
    await screen.findByText('Couldn’t create a key: the server sent no stream key');
    expect(loadObsKey(serverUrl())).toEqual(KEY);
  });
});

describe('U7: polling stays cheap', () => {
  it('polls every 2 s while visible, never while hidden, reads at once on return, and stops on Forget', async () => {
    vi.useFakeTimers();
    saveObsKey(serverUrl(), KEY);
    render(<ObsPage />);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(gets()).toHaveLength(1);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(POLL_INTERVAL_MS);
    });
    expect(gets()).toHaveLength(2);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(POLL_INTERVAL_MS);
    });
    expect(gets()).toHaveLength(3);

    act(() => setVisibility('hidden'));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(POLL_INTERVAL_MS * 5);
    });
    expect(gets()).toHaveLength(3);

    act(() => setVisibility('visible'));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(gets()).toHaveLength(4);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(POLL_INTERVAL_MS);
    });
    expect(gets()).toHaveLength(5);

    fireEvent.click(screen.getByRole('button', { name: 'Forget key' }));
    fireEvent.click(within(screen.getByRole('alertdialog')).getByRole('button', { name: 'Forget key' }));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(POLL_INTERVAL_MS * 5);
    });
    expect(gets()).toHaveLength(5);
  });

  it('does not poll without a key, nor start while the tab is hidden', async () => {
    vi.useFakeTimers();
    render(<ObsPage />);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(POLL_INTERVAL_MS * 3);
    });
    expect(fetchCalls).toEqual([]);
    cleanup();
    saveObsKey(serverUrl(), KEY);
    setVisibility('hidden');
    render(<ObsPage />);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(POLL_INTERVAL_MS * 3);
    });
    expect(fetchCalls).toEqual([]);
  });
});

describe('U5: the key is treated as a password', () => {
  it('never enters the URL, a log line or a fetch URL, through mint, show, copy and polling', async () => {
    const logged: unknown[] = [];
    for (const m of ['info', 'warn', 'error'] as const) {
      vi.spyOn(log, m).mockImplementation((...args: unknown[]) => void logged.push(...args));
    }
    const consoleSpies = (['log', 'info', 'warn', 'error', 'debug'] as const).map((m) =>
      vi.spyOn(console, m).mockImplementation(() => {}),
    );
    mintScript.push(async () => KEY);
    render(<ObsPage />);
    fireEvent.click(screen.getByRole('button', { name: 'Create stream key' }));
    await screen.findByText('Bearer Token');
    fireEvent.click(screen.getByRole('button', { name: 'Show' }));
    fireEvent.click(screen.getByRole('button', { name: 'Copy key' }));
    await screen.findByText('Waiting for OBS…');

    expect(window.location.href).not.toContain(TOKEN);
    expect(window.location.hash).toBe('#/broadcast/obs');
    for (const c of fetchCalls) expect(c.url).not.toContain(TOKEN);
    expect(JSON.stringify(logged)).not.toContain(TOKEN);
    for (const spy of consoleSpies) expect(JSON.stringify(spy.mock.calls)).not.toContain(TOKEN);
    // Nothing but the key's own slot in storage holds it.
    for (let i = 0; i < localStorage.length; i++) {
      const k = localStorage.key(i)!;
      expect(k).not.toContain(TOKEN);
      if (!k.startsWith('gawk.obsKey:')) expect(localStorage.getItem(k) ?? '').not.toContain(TOKEN);
    }
  });
});
