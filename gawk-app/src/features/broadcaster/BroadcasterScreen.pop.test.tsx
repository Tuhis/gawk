// @vitest-environment jsdom
//
// R74 (docs/76 items 34 and 43.3, §7.3): the web broadcaster and local
// relays. It probes the resolved relay's /echo when the screen opens, so
// home's offer arrives before it goes live; its /publish says lr=1 and
// carries a remembered party-wall Hide as wall=hidden; a live broadcast is
// never moved; and on a PoP its live bar says where it is live and whether
// it is on the party wall.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { BroadcastStartError, type BroadcastCallbacks } from '../../transport/broadcaster';
import type { ConnectOptions } from '../../transport/connection';

const { created, scripts } = vi.hoisted(() => {
  interface FakeSession {
    callbacks: BroadcastCallbacks;
    url: string;
    opts: ConnectOptions;
  }
  const created: FakeSession[] = [];
  const scripts: Array<(cbs: BroadcastCallbacks) => Promise<void>> = [];
  return { created, scripts };
});

vi.mock('./workerBroadcastSession', () => ({
  createBroadcastSession: async (_config: unknown, url: string, opts: ConnectOptions, callbacks: BroadcastCallbacks) => {
    const script = scripts.shift();
    if (!script) throw new Error('test bug: no session script queued');
    const session = {
      callbacks,
      url,
      opts,
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
import { acceptCurrentTerms } from '../terms/acceptance';
import { useTransportStore } from '../../state/transportStore';
import { usePopStore } from '../pop/popStore';
import { resetSwitchForTest, setSwitchDepsForTest } from '../pop/switch';
import { setWallHidden } from '../pop/wall';
import type { ProbeOptions, ProbeResult } from '../servers/probe';
import type { LocalRelayOfferMessage } from '../../transport/wire';

const HOME = 'https://api.gawk.ioio.fi:4433';
const LAN = 'https://192.168.1.20:4433';
const INFO = { linked: true, onWall: true, name: 'Assembly 2026', homeOrigin: 'https://gawk.ioio.fi', wallCode: 'assembly-2026' };
const OFFER: LocalRelayOfferMessage = {
  handle: 'pop-uid-1',
  name: 'Assembly 2026',
  validity: 3600,
  candidates: [{ url: LAN, hashes: [new Uint8Array(32).fill(0x11)] }],
};

const fakeStream = { getTracks: () => [] } as unknown as MediaStream;

interface Probe {
  url: string;
  opts: ProbeOptions | undefined;
  settle: (r: ProbeResult) => void;
}
let probes: Probe[] = [];

beforeEach(() => {
  created.length = 0;
  scripts.length = 0;
  probes = [];
  localStorage.clear();
  window.__GAWK_CONFIG__ = { requirePublishSecret: false, relayUrl: HOME };
  acceptCurrentTerms();
  resetSwitchForTest();
  useTransportStore.getState().clearOfferOverride();
  setSwitchDepsForTest({
    probe: (url, _hashes, _now, _factory, opts) => new Promise<ProbeResult>((settle) => probes.push({ url, opts, settle })),
    queryPermission: async () => 'granted',
    isMac: () => false,
  });
  Object.defineProperty(HTMLMediaElement.prototype, 'srcObject', { configurable: true, get: () => null, set: () => {} });
  HTMLMediaElement.prototype.play = () => Promise.resolve();
});

afterEach(() => {
  cleanup();
  resetSwitchForTest();
  useTransportStore.getState().clearOfferOverride();
  delete window.__GAWK_CONFIG__;
  localStorage.clear();
});

function startBroadcast() {
  fireEvent.click(screen.getByRole('button', { name: /start a stream/i }));
}

describe('the web broadcaster and local relays', () => {
  it('probes the resolved relay on open, and follows home’s offer before going live', async () => {
    render(<BroadcasterScreen />);
    await waitFor(() => expect(probes).toHaveLength(1));
    expect(probes[0].url).toBe(useTransportStore.getState().serverUrl);
    expect(probes[0].opts?.followOffers).toBe(true);
    await act(async () => probes[0].settle({ state: 'ok', rttMs: 3, identity: null, offer: OFFER }));
    await waitFor(() => expect(probes).toHaveLength(2));
    expect(probes[1].url).toBe(LAN);
    await act(async () => probes[1].settle({ state: 'ok', rttMs: 1, identity: null }));
    await waitFor(() => expect(useTransportStore.getState().resolvedSource).toBe('pop'));

    scripts.push(async (cbs) => cbs.onSourceStream(fakeStream));
    startBroadcast();
    await waitFor(() => expect(created).toHaveLength(1));
    expect(created[0].url).toBe(LAN);
    expect(created[0].opts.certHashHex).toBe('11'.repeat(32));
    // On the PoP it follows no further offer, so it says no lr=1.
    expect(created[0].opts.followOffers).toBeFalsy();
  });

  it('says lr=1 on a /publish to home', async () => {
    render(<BroadcasterScreen />);
    scripts.push(async (cbs) => cbs.onSourceStream(fakeStream));
    startBroadcast();
    await waitFor(() => expect(created).toHaveLength(1));
    expect(created[0].opts.followOffers).toBe(true);
    expect(created[0].opts.wallHidden).toBeFalsy();
  });

  it('a remembered Hide for this PoP’s wall goes out as wall=hidden on the first /publish', async () => {
    act(() => useTransportStore.getState().setOfferOverride(LAN, [], 'pop-uid-1', 'Assembly 2026'));
    setWallHidden('assembly-2026', true);
    render(<BroadcasterScreen />);
    await waitFor(() => expect(probes).toHaveLength(1));
    await act(async () => probes[0].settle({ state: 'ok', rttMs: 1, identity: null, localRelay: INFO }));
    scripts.push(async (cbs) => cbs.onSourceStream(fakeStream));
    startBroadcast();
    await waitFor(() => expect(created).toHaveLength(1));
    expect(created[0].opts.wallHidden).toBe(true);
  });

  it('live on a PoP: names it and shows the party wall line from the publish session’s 0x18', async () => {
    act(() => useTransportStore.getState().setOfferOverride(LAN, [], 'pop-uid-1', 'Assembly 2026'));
    render(<BroadcasterScreen />);
    scripts.push(async (cbs) => {
      cbs.onBroadcastId?.('K7XQ2M');
      cbs.onResumeToken?.('ab'.repeat(16));
      cbs.onSourceStream(fakeStream);
      cbs.onLocalRelay?.({ kind: 'info', info: INFO });
    });
    startBroadcast();
    await waitFor(() => expect(screen.getByText('Live on Assembly 2026’s local relay')).toBeTruthy());
    expect(screen.getByText(/On the party wall/)).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Hide' })).toBeTruthy();
  });

  it('an offer while live never moves the broadcast; it says the next broadcast will use it', async () => {
    render(<BroadcasterScreen />);
    await waitFor(() => expect(probes).toHaveLength(1));
    await act(async () => probes[0].settle({ state: 'failed' }));
    scripts.push(async (cbs) => cbs.onSourceStream(fakeStream));
    startBroadcast();
    await waitFor(() => expect(screen.getByText('LIVE')).toBeTruthy());
    act(() => created[0].callbacks.onLocalRelay?.({ kind: 'offer', offer: OFFER }));
    await waitFor(() =>
      expect(usePopStore.getState().note).toBe('Assembly 2026 has a local relay. Your next broadcast will use it.'),
    );
    expect(probes).toHaveLength(1);
    expect(useTransportStore.getState().resolvedSource).not.toBe('pop');
    expect(screen.getByText('Assembly 2026 has a local relay. Your next broadcast will use it.')).toBeTruthy();
  });

  it('a followed PoP that refuses the /publish sends the tab home, with no secret prompt and no loop back', async () => {
    act(() => useTransportStore.getState().setOfferOverride(LAN, [], 'pop-uid-1', 'Assembly 2026'));
    render(<BroadcasterScreen />);
    scripts.push(async () => {
      throw new BroadcastStartError('connect', new Error('refused'));
    });
    startBroadcast();
    await waitFor(() => expect(screen.getByText('Couldn’t start')).toBeTruthy());
    expect(screen.queryByText(/publish secret/)).toBeNull();
    expect(useTransportStore.getState().resolvedSource).not.toBe('pop');
    // Home's next probe offers the same PoP: this tab gave up on it.
    await waitFor(() => expect(probes.some((p) => p.url !== LAN)).toBe(true));
    const home = probes.find((p) => p.url !== LAN)!;
    expect(home.opts?.followOffers).toBe(false);
    await act(async () => home.settle({ state: 'ok', rttMs: 3, identity: null, offer: OFFER }));
    expect(probes.filter((p) => p.url === LAN)).toHaveLength(1);
  });
});
