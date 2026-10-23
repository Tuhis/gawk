// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

// Links stay short on the default relay and carry ?relay= on any other: that
// parameter is what makes a code minted on relay B joinable from a UI
// deployed against relay A.
async function load() {
  vi.resetModules();
  const store = (await import('../state/transportStore')).useTransportStore;
  const { buildViewLink } = await import('./shareLink');
  return { store, buildViewLink };
}

beforeEach(() => {
  localStorage.clear();
  window.__GAWK_CONFIG__ = {};
});

afterEach(() => {
  delete window.__GAWK_CONFIG__;
  localStorage.clear();
});

describe('buildViewLink', () => {
  it('stays short on the deployment default', async () => {
    const { buildViewLink } = await load();
    expect(buildViewLink('AB2CD3')).toBe(
      `${window.location.origin}${window.location.pathname}#/view/AB2CD3`,
    );
  });

  it('carries ?relay= for a non-default resolved server', async () => {
    const { store, buildViewLink } = await load();
    const id = store.getState().addServer({ label: 'x', url: 'https://relay.b.example:4433' })!;
    store.getState().selectServer(id);
    expect(buildViewLink('AB2CD3')).toBe(
      `${window.location.origin}${window.location.pathname}#/view/AB2CD3?relay=${encodeURIComponent('https://relay.b.example:4433')}`,
    );
  });

  it('carries the override relay during a link session', async () => {
    const { store, buildViewLink } = await load();
    store.getState().setSessionOverride('https://link.example:4433');
    expect(buildViewLink('AB2CD3')).toContain('?relay=');
  });

  it('stays short for a custom entry that duplicates the default URL', async () => {
    const { store, buildViewLink } = await load();
    const id = store.getState().addServer({ label: 'dup', url: 'https://localhost:4433' })!;
    store.getState().selectServer(id);
    expect(buildViewLink('AB2CD3')).not.toContain('?relay=');
  });
});

// R74 (docs/76 §6.12 item 45): on a standalone PoP, share links carry the
// pins with the LAN relay so a guest's browser can dial it.
describe('share links with certificate pins (R74)', () => {
  const H1 = '11'.repeat(32);
  const H2 = '22'.repeat(32);
  const POP = 'https://192.168.1.20:4433';
  const base = () => `${window.location.origin}${window.location.pathname}`;

  it('carries &certHash= beside a private-literal relay', async () => {
    const { store, buildViewLink } = await load();
    store.getState().setSessionOverride(POP, [H1, H2]);
    expect(buildViewLink('AB2CD3')).toBe(
      `${base()}#/view/AB2CD3?relay=${encodeURIComponent(POP)}&certHash=${H1},${H2}`,
    );
  });

  it('round-trips through the router', async () => {
    const { store, buildViewLink } = await load();
    const id = store.getState().addServer({ label: 'PoP', url: POP, certHashes: [H1, H2] })!;
    store.getState().selectServer(id);
    const link = buildViewLink('AB2CD3');
    const { parseRoute } = await import('../routing');
    expect(parseRoute(link.slice(link.indexOf('#')))).toEqual({
      view: 'viewer',
      broadcastId: 'AB2CD3',
      relay: POP,
      certHashes: [H1, H2],
      droppedParams: [],
    });
  });

  it('never carries a hash beside a public relay, where the router would drop it', async () => {
    const { store, buildViewLink } = await load();
    const id = store.getState().addServer({ label: 'x', url: 'https://relay.b.example:4433', certHashes: [H1] })!;
    store.getState().selectServer(id);
    expect(buildViewLink('AB2CD3')).not.toContain('certHash');
  });

  it('carries no certHash for a private relay with no pins', async () => {
    const { store, buildViewLink } = await load();
    store.getState().setSessionOverride(POP);
    expect(buildViewLink('AB2CD3')).toBe(`${base()}#/view/AB2CD3?relay=${encodeURIComponent(POP)}`);
  });

  it('room links and in-app hops carry the pins too', async () => {
    vi.resetModules();
    const store = (await import('../state/transportStore')).useTransportStore;
    const { buildRoomLink, relayQuerySuffix } = await import('./shareLink');
    store.getState().setSessionOverride(POP, [H1]);
    expect(relayQuerySuffix()).toBe(`?relay=${encodeURIComponent(POP)}&certHash=${H1}`);
    expect(buildRoomLink('vip-abc')).toContain(`&certHash=${H1}`);
  });
});

// R74 (docs/76 item 45): share links from a PoP's LocalRelayInfo. On a
// linked PoP they are home links with no ?relay=: a guest at the party
// switches by itself and anyone outside pulls on demand. On a standalone PoP
// they carry ?relay=<LAN literal>&certHash=, also from the host's own /app/
// on loopback, which no guest could open.
describe('share links from LocalRelayInfo (R74)', () => {
  const H1 = '11'.repeat(32);
  const POP = 'https://192.168.1.20:4433';
  const HOME_APP = 'https://gawk.ioio.fi';

  async function loadWithPop() {
    vi.resetModules();
    const store = (await import('../state/transportStore')).useTransportStore;
    const pop = (await import('../features/pop/popStore')).usePopStore;
    const links = await import('./shareLink');
    return { store, pop, ...links };
  }

  const info = (over: Partial<{ linked: boolean; homeOrigin: string }> = {}) => ({
    linked: true,
    onWall: false,
    name: 'Assembly 2026',
    homeOrigin: HOME_APP,
    wallCode: '',
    ...over,
  });

  it('a tab a followed offer moved to the PoP shares home links, and its hops add no ?relay=', async () => {
    const { store, buildViewLink, buildRoomLink, relayQuerySuffix } = await loadWithPop();
    store.getState().setOfferOverride(POP, [H1], 'pop-uid-1', 'Assembly 2026');
    expect(buildViewLink('AB2CD3')).toBe(`${window.location.origin}${window.location.pathname}#/view/AB2CD3`);
    expect(buildRoomLink('vip-abc')).toBe(`${window.location.origin}${window.location.pathname}#/room/vip-abc`);
    expect(relayQuerySuffix()).toBe('');
  });

  it('a tab a self-hosted fleet’s offer moved to its PoP keeps naming that home in hops and shares', async () => {
    const { store, buildViewLink, relayQuerySuffix } = await loadWithPop();
    const SELF = 'https://relay.self.example:4433';
    store.getState().setSessionOverride(SELF);
    store.getState().setOfferOverride(POP, [H1], 'pop-uid-1', 'Assembly 2026');
    expect(store.getState().resolvedSource).toBe('pop');
    const suffix = `?relay=${encodeURIComponent(SELF)}`;
    expect(relayQuerySuffix()).toBe(suffix);
    expect(buildViewLink('AB2CD3')).toBe(`${window.location.origin}${window.location.pathname}#/view/AB2CD3${suffix}`);
    // The hop re-applies the same override: the followed offer survives it,
    // and a later fall back from the PoP lands on the fleet that offered it.
    store.getState().setSessionOverride(SELF);
    expect(store.getState().resolvedSource).toBe('pop');
    store.getState().clearOfferOverride();
    expect(store.getState().serverUrl).toBe(SELF);
  });

  it('a linked PoP’s page shares links to the home app it names', async () => {
    window.__GAWK_CONFIG__ = { relayUrl: POP, relayCertHashes: [H1] };
    const { store, pop, buildViewLink, buildRoomLink } = await loadWithPop();
    pop.setState({ info: { relayUrl: store.getState().serverUrl, info: info() } });
    expect(buildViewLink('AB2CD3')).toBe(`${HOME_APP}/#/view/AB2CD3`);
    expect(buildRoomLink('assembly-2026')).toBe(`${HOME_APP}/#/room/assembly-2026`);
  });

  it('the host’s own /app/ on loopback, on a standalone PoP, shares a public app link naming the LAN relay with its pins', async () => {
    window.__GAWK_CONFIG__ = { relayUrl: POP, relayCertHashes: [H1] };
    const { store, pop, buildViewLink } = await loadWithPop();
    pop.setState({ info: { relayUrl: store.getState().serverUrl, info: info({ linked: false, homeOrigin: '' }) } });
    expect(buildViewLink('AB2CD3')).toBe(`${HOME_APP}/#/view/AB2CD3?relay=${encodeURIComponent(POP)}&certHash=${H1}`);
  });

  it('ignores LocalRelayInfo from another relay than the one in use, and a home origin that is not one', async () => {
    const { store, pop, buildViewLink } = await loadWithPop();
    pop.setState({ info: { relayUrl: 'https://192.168.9.9:4433', info: info() } });
    expect(buildViewLink('AB2CD3')).toBe(`${window.location.origin}${window.location.pathname}#/view/AB2CD3`);
    pop.setState({ info: { relayUrl: store.getState().serverUrl, info: info({ homeOrigin: 'javascript:alert(1)' }) } });
    expect(buildViewLink('AB2CD3')).toBe(`${window.location.origin}${window.location.pathname}#/view/AB2CD3`);
  });
});
