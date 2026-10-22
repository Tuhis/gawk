// @vitest-environment jsdom
//
// docs/76 item 43, the web client's half of PP14: what a fresh
// LocalRelayOffer does under each permission state, the parallel candidate
// probes, failure, the opt-out, and the broadcaster who is already live.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import type { ProbeOptions, ProbeResult } from '../servers/probe';
import type { LocalRelayOfferMessage } from '../../transport/wire';
import { useTransportStore } from '../../state/transportStore';
import { usePopStore } from './popStore';
import {
  OPT_OUT_KEY,
  POP_PROMPT_PROBE_TIMEOUT_MS,
  considerOffer,
  noteLocalRelay,
  probeHomeForOffer,
  optOut,
  resetSwitchForTest,
  setBroadcasterLive,
  setSwitchDepsForTest,
  pickLocalRelayAgain,
  primeLocalNetworkPermission,
  willFollowOffers,
  type SwitchDeps,
} from './switch';
import type { LocalNetworkPermission } from './permission';

const HOME = 'https://api.gawk.ioio.fi:4433';
const LAN = 'https://192.168.1.20:4433';
const NAMED = 'https://p-7k2m9x.pop.gawk.ioio.fi:4433';

function offer(over: Partial<LocalRelayOfferMessage> = {}): LocalRelayOfferMessage {
  return {
    handle: 'pop-uid-1',
    name: 'Assembly 2026',
    validity: 3600,
    candidates: [
      { url: NAMED, hashes: [] },
      { url: LAN, hashes: [new Uint8Array(32).fill(0x11), new Uint8Array(32).fill(0x22)] },
    ],
    ...over,
  };
}

const OK: ProbeResult = { state: 'ok', rttMs: 2, identity: null };
const FAIL: ProbeResult = { state: 'failed' };

interface Probe {
  url: string;
  hashes: string;
  timeoutMs: number | undefined;
  opts: ProbeOptions | undefined;
  settle: (r: ProbeResult) => void;
}

function harness(permission: LocalNetworkPermission) {
  const probes: Probe[] = [];
  let now = 1_000_000;
  const deps: SwitchDeps = {
    probe: (url, hashes, _now, _factory, opts) =>
      new Promise<ProbeResult>((settle) => probes.push({ url, hashes, timeoutMs: opts?.connectTimeoutMs, opts, settle })),
    queryPermission: vi.fn(async () => permission),
    now: () => now,
    isMac: () => false,
  };
  setSwitchDepsForTest(deps);
  return {
    probes,
    deps,
    advance: (ms: number) => {
      now += ms;
    },
  };
}

const flush = () => new Promise((r) => setTimeout(r, 0));

beforeEach(() => {
  localStorage.clear();
  window.__GAWK_CONFIG__ = { relayUrl: HOME };
  resetSwitchForTest();
  useTransportStore.getState().clearOfferOverride();
});

afterEach(() => {
  resetSwitchForTest();
  useTransportStore.getState().clearOfferOverride();
});

describe('considerOffer per permission state', () => {
  it('granted: probes every candidate at once and switches silently to the first that answers', async () => {
    const h = harness('granted');
    const done = considerOffer(offer(), 'viewer');
    await flush();
    // In parallel: both dials are out before either answers.
    expect(h.probes.map((p) => p.url)).toEqual([NAMED, LAN]);
    expect(h.probes[1].hashes).toBe(`${'11'.repeat(32)},${'22'.repeat(32)}`);
    h.probes[0].settle(FAIL);
    h.probes[1].settle(OK);
    await done;
    const t = useTransportStore.getState();
    expect(t.resolvedSource).toBe('pop');
    expect(t.serverUrl).toBe(LAN);
    expect(t.certHashes).toEqual(['11'.repeat(32), '22'.repeat(32)]);
    expect(usePopStore.getState().phase).toBe('switched');
    expect(usePopStore.getState().toast).toBe(null);
  });

  it('prompt: probes anyway, with the toast explaining the browser prompt, and waits for the answer', async () => {
    const h = harness('prompt');
    const done = considerOffer(offer({ candidates: [{ url: LAN, hashes: [] }] }), 'viewer');
    await flush();
    expect(usePopStore.getState().toast).toMatch(
      /^Assembly 2026 has a local relay\. Your browser will ask to let .+ connect to devices on this network\. Allow it to stream and watch over the LAN\.$/,
    );
    expect(h.probes[0].timeoutMs).toBe(POP_PROMPT_PROBE_TIMEOUT_MS);
    h.probes[0].settle(OK);
    await done;
    expect(useTransportStore.getState().resolvedSource).toBe('pop');
    expect(usePopStore.getState().toast).toBe(null);
  });

  it('denied: stays on home with the quiet note and never dials', async () => {
    const h = harness('denied');
    await considerOffer(offer(), 'viewer');
    expect(h.probes).toHaveLength(0);
    expect(useTransportStore.getState().resolvedSource).toBe('default');
    expect(usePopStore.getState().phase).toBe('denied');
    expect(usePopStore.getState().note).toMatch(/^Your browser blocked the local relay\. Streams still work through .+, over the party’s internet\./);
  });

  it('unknown permission name: just probes, with no toast', async () => {
    const h = harness('unknown');
    const done = considerOffer(offer(), 'viewer');
    await flush();
    expect(h.probes).toHaveLength(2);
    expect(usePopStore.getState().toast).toBe(null);
    h.probes[0].settle(OK);
    await done;
    expect(useTransportStore.getState().serverUrl).toBe(NAMED);
  });
});

describe('considerOffer failure and repeats', () => {
  it('every candidate failing stays on home, marks the PoP unreachable for this tab, and never retries', async () => {
    const h = harness('granted');
    const done = considerOffer(offer(), 'viewer');
    await flush();
    h.probes.forEach((p) => p.settle(FAIL));
    await done;
    expect(useTransportStore.getState().resolvedSource).toBe('default');
    expect(usePopStore.getState().phase).toBe('failed');
    await considerOffer(offer(), 'viewer');
    expect(h.probes).toHaveLength(2); // the second offer dialed nothing
  });

  it('a duplicate offer while probing dials nothing more (every session kind sends one)', async () => {
    const h = harness('granted');
    const first = considerOffer(offer(), 'viewer');
    await flush();
    await considerOffer(offer(), 'room');
    expect(h.probes).toHaveLength(2);
    h.probes[0].settle(OK);
    await first;
  });

  it('an offer that expires while its probes run is not followed', async () => {
    const h = harness('granted');
    const done = considerOffer(offer({ validity: 5 }), 'viewer');
    await flush();
    h.advance(6_000);
    h.probes[0].settle(OK);
    await done;
    expect(useTransportStore.getState().resolvedSource).toBe('default');
  });

  it('an offer on a session already at a PoP is ignored', async () => {
    const h = harness('granted');
    useTransportStore.getState().setOfferOverride(LAN, [], 'other', 'Other');
    await considerOffer(offer(), 'viewer');
    expect(h.probes).toHaveLength(0);
  });
});

describe('opt-out per PoP handle', () => {
  it('"Use … instead" goes home, is remembered per handle, and that PoP’s offers are then ignored', async () => {
    const h = harness('granted');
    const done = considerOffer(offer(), 'viewer');
    await flush();
    h.probes[0].settle(OK);
    await done;
    optOut();
    expect(useTransportStore.getState().resolvedSource).toBe('default');
    expect(JSON.parse(localStorage.getItem(OPT_OUT_KEY) ?? '[]')).toEqual(['pop-uid-1']);
    await considerOffer(offer(), 'viewer');
    expect(h.probes).toHaveLength(2);
    // The ignored offer is still named, so the user can pick it again.
    expect(usePopStore.getState().declined).toEqual({ handle: 'pop-uid-1', name: 'Assembly 2026' });
    // Another PoP is not opted out.
    const other = considerOffer(offer({ handle: 'pop-uid-2' }), 'viewer');
    await flush();
    expect(h.probes).toHaveLength(4);
    h.probes[2].settle(FAIL);
    h.probes[3].settle(FAIL);
    await other;
  });

  it('picking it again forgets the opt-out and follows the offer', async () => {
    const h = harness('granted');
    localStorage.setItem(OPT_OUT_KEY, JSON.stringify(['pop-uid-1']));
    await considerOffer(offer(), 'viewer');
    expect(h.probes).toHaveLength(0);
    const again = pickLocalRelayAgain();
    await flush();
    expect(localStorage.getItem(OPT_OUT_KEY)).toBe('[]');
    h.probes[0].settle(OK);
    await again;
    expect(useTransportStore.getState().resolvedSource).toBe('pop');
  });
});

describe('the broadcaster', () => {
  it('already live: never moved, told the next broadcast will use it, and switched once it ends', async () => {
    const h = harness('granted');
    setBroadcasterLive(true);
    await considerOffer(offer(), 'broadcaster');
    expect(h.probes).toHaveLength(0);
    expect(usePopStore.getState().note).toBe('Assembly 2026 has a local relay. Your next broadcast will use it.');
    setBroadcasterLive(false);
    await flush();
    expect(h.probes).toHaveLength(2);
    h.probes[1].settle(OK);
    await flush();
    await flush();
    expect(useTransportStore.getState().serverUrl).toBe(LAN);
  });

  it.each(['room', 'viewer'] as const)(
    'live: an offer on the tab’s %s session does not move it either (its publish stays on home)',
    async (role) => {
      const h = harness('granted');
      setBroadcasterLive(true);
      await considerOffer(offer(), role);
      expect(h.probes).toHaveLength(0);
      expect(usePopStore.getState().phase).toBe('deferred');
      expect(useTransportStore.getState().resolvedSource).not.toBe('pop');
    },
  );

  it('going live while the probe runs (the prompt is still open) defers the switch instead', async () => {
    const h = harness('prompt');
    const done = considerOffer(offer({ candidates: [{ url: LAN, hashes: [] }] }), 'broadcaster');
    await flush();
    setBroadcasterLive(true);
    h.probes[0].settle(OK);
    await done;
    expect(useTransportStore.getState().resolvedSource).not.toBe('pop');
    expect(usePopStore.getState().phase).toBe('deferred');
    expect(usePopStore.getState().note).toBe('Assembly 2026 has a local relay. Your next broadcast will use it.');
  });

  it('a viewer offer is followed even while this tab is not broadcasting', async () => {
    const h = harness('granted');
    await Promise.race([considerOffer(offer(), 'viewer'), flush()]);
    expect(h.probes).toHaveLength(2);
    h.probes[0].settle(OK);
  });
});

describe('noteLocalRelay and lr=1', () => {
  it('records a PoP’s LocalRelayInfo against the relay it came from', () => {
    harness('granted');
    const info = { linked: true, onWall: false, name: 'Assembly 2026', homeOrigin: 'https://gawk.ioio.fi', wallCode: 'assembly-2026' };
    noteLocalRelay({ kind: 'info', info }, 'viewer');
    expect(usePopStore.getState().info).toEqual({ relayUrl: useTransportStore.getState().serverUrl, info });
  });

  it('follows offers (lr=1) unless the permission is denied or the session is already on a PoP', async () => {
    harness('granted');
    expect(willFollowOffers()).toBe(true);
    usePopStore.setState({ permission: 'denied' });
    expect(willFollowOffers()).toBe(false);
    usePopStore.setState({ permission: 'granted' });
    useTransportStore.getState().setOfferOverride(LAN, [], 'h', 'n');
    expect(willFollowOffers()).toBe(false);
  });
});

// docs/76 item 43.3: the web broadcaster probes home's /echo when its screen
// opens, so an offer arrives before it goes live.
describe('probeHomeForOffer', () => {
  it('probes the resolved relay with lr=1 and follows the offer it returns', async () => {
    const h = harness('granted');
    const done = probeHomeForOffer();
    await flush();
    expect(h.probes[0].url).toBe(useTransportStore.getState().serverUrl);
    h.probes[0].settle({ state: 'ok', rttMs: 3, identity: null, offer: offer({ candidates: [{ url: LAN, hashes: [] }] }) });
    await flush();
    await flush();
    expect(h.probes[1].url).toBe(LAN);
    h.probes[1].settle(OK);
    await done;
    expect(useTransportStore.getState().serverUrl).toBe(LAN);
  });

  it('on a PoP, or with the permission denied, still reads the relay’s LocalRelayInfo but follows no offer', async () => {
    const h = harness('granted');
    const info = { linked: true, onWall: true, name: 'Assembly 2026', homeOrigin: 'https://gawk.ioio.fi', wallCode: 'assembly-2026' };
    usePopStore.setState({ permission: 'denied' });
    const first = probeHomeForOffer();
    await flush();
    expect(h.probes[0].opts?.followOffers).toBe(false);
    h.probes[0].settle({ state: 'ok', rttMs: 3, identity: null, offer: offer() });
    await first;
    expect(h.probes).toHaveLength(1);
    useTransportStore.getState().setOfferOverride(LAN, [], 'h', 'n');
    usePopStore.setState({ permission: 'granted' });
    const second = probeHomeForOffer();
    await flush();
    expect(h.probes[1].url).toBe(LAN);
    expect(h.probes[1].opts?.followOffers).toBe(false);
    h.probes[1].settle({ state: 'ok', rttMs: 3, identity: null, localRelay: info, offer: offer({ handle: 'pop-uid-2' }) });
    await second;
    expect(h.probes).toHaveLength(2);
    expect(usePopStore.getState().info).toEqual({ relayUrl: LAN, info });
  });
});

describe('primeLocalNetworkPermission', () => {
  it('asks once, so a denial is known before the first CONNECT says lr=1', async () => {
    harness('denied');
    expect(willFollowOffers()).toBe(true);
    await primeLocalNetworkPermission();
    expect(willFollowOffers()).toBe(false);
  });
});

describe('lr=1 after this tab gave up on a PoP', () => {
  it('a PoP that failed in this tab, or one opted out of, stops lr=1 so home never holds demand for nothing', async () => {
    const h = harness('granted');
    const done = considerOffer(offer(), 'viewer');
    await flush();
    h.probes.forEach((p) => p.settle(FAIL));
    await done;
    expect(willFollowOffers()).toBe(false);
    resetSwitchForTest();
    harness('granted');
    localStorage.setItem(OPT_OUT_KEY, JSON.stringify(['pop-uid-1']));
    await considerOffer(offer(), 'viewer');
    expect(willFollowOffers()).toBe(false);
    // Another PoP's offer is a fresh decision again.
    usePopStore.setState({ declined: null, offer: offer({ handle: 'pop-uid-2' }) });
    expect(willFollowOffers()).toBe(true);
  });
});
