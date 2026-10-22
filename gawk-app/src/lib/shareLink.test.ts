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
