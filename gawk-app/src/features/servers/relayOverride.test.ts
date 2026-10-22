// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

// Route → session override wiring, incl. the allowCustomRelays gate.
// The store resolves its default at module-evaluation time, so each case
// re-imports both modules with the environment already in place.
async function load() {
  vi.resetModules();
  const store = (await import('../../state/transportStore')).useTransportStore;
  const mod = await import('./relayOverride');
  const routing = await import('../../routing');
  return { store, ...mod, parseRoute: routing.parseRoute };
}

beforeEach(() => {
  localStorage.clear();
  window.__GAWK_CONFIG__ = {};
});

afterEach(() => {
  delete window.__GAWK_CONFIG__;
  localStorage.clear();
});

describe('applyRouteRelay', () => {
  it('applies a link relay as the session override on both routes', async () => {
    const { store, applyRouteRelay, parseRoute } = await load();
    applyRouteRelay(parseRoute('#/view/AB2CD3?relay=https://link.example:4433'));
    expect(store.getState().serverUrl).toBe('https://link.example:4433');
    expect(store.getState().resolvedSource).toBe('override');
    expect(store.getState().relayLinkNote).toBe(null);

    applyRouteRelay(parseRoute('#/broadcast?relay=https://link2.example:4433'));
    expect(store.getState().serverUrl).toBe('https://link2.example:4433');
  });

  it('never writes the override to localStorage (D2)', async () => {
    const { store, applyRouteRelay, parseRoute } = await load();
    applyRouteRelay(parseRoute('#/view/AB2CD3?relay=https://link.example:4433'));
    expect(store.getState().sessionOverrideUrl).toBe('https://link.example:4433');
    expect(localStorage.getItem('gawk.servers') ?? '[]').not.toContain('link.example');
    expect(localStorage.getItem('gawk.selectedServer') ?? 'default').toBe('default');
  });

  it('clears the override when navigating to a route without one', async () => {
    const { store, applyRouteRelay, parseRoute } = await load();
    applyRouteRelay(parseRoute('#/view/AB2CD3?relay=https://link.example:4433'));
    applyRouteRelay(parseRoute('#/'));
    expect(store.getState().sessionOverrideUrl).toBe(null);
    expect(store.getState().serverUrl).toBe('https://localhost:4433');
  });

  it('gated deployment (allowCustomRelays: false): link joins on the own relay with a note', async () => {
    window.__GAWK_CONFIG__ = { allowCustomRelays: false };
    const { store, applyRouteRelay, parseRoute, NOTE_RELAY_NOT_ALLOWED } = await load();
    applyRouteRelay(parseRoute('#/view/AB2CD3?relay=https://link.example:4433'));
    expect(store.getState().sessionOverrideUrl).toBe(null);
    expect(store.getState().serverUrl).toBe('https://localhost:4433');
    expect(store.getState().relayLinkNote).toBe(NOTE_RELAY_NOT_ALLOWED);
  });

  it('invalid relay value: joins on the own relay with the invalid note', async () => {
    const { store, applyRouteRelay, parseRoute, NOTE_RELAY_INVALID } = await load();
    applyRouteRelay(parseRoute('#/view/AB2CD3?relay=http://insecure.example'));
    expect(store.getState().sessionOverrideUrl).toBe(null);
    expect(store.getState().relayLinkNote).toBe(NOTE_RELAY_INVALID);
    // Navigating away clears the note with the override.
    applyRouteRelay(parseRoute('#/'));
    expect(store.getState().relayLinkNote).toBe(null);
  });

  it('an override matching a saved entry attaches its credentials', async () => {
    const { store, applyRouteRelay, parseRoute } = await load();
    store.getState().addServer({
      label: 'Saved',
      url: 'https://link.example:4433',
      publishSecret: 'savedsec',
    });
    applyRouteRelay(parseRoute('#/broadcast?relay=https://link.example:4433'));
    expect(store.getState().publishSecret).toBe('savedsec');
  });
});

// R74 (docs/76 §6.1 item 5): a standalone PoP's join link is the LANDING
// route with `?relay=<lan-ip>&certHash=`. The hashes ride the override.
describe('applyRouteRelay certHash (R74)', () => {
  const H1 = '11'.repeat(32);
  const H2 = '22'.repeat(32);
  const POP = 'https://192.168.1.20:4433';

  it('applies a landing-route relay and its pins as the session override', async () => {
    const { store, applyRouteRelay, parseRoute } = await load();
    applyRouteRelay(parseRoute(`#/?relay=${encodeURIComponent(POP)}&certHash=${H1},${H2}`));
    expect(store.getState().serverUrl).toBe(POP);
    expect(store.getState().resolvedSource).toBe('override');
    expect(store.getState().certHashes).toEqual([H1, H2]);
    expect(store.getState().relayLinkNote).toBe(null);
  });

  it('a plain landing route still clears the override', async () => {
    const { store, applyRouteRelay, parseRoute } = await load();
    applyRouteRelay(parseRoute(`#/?relay=${encodeURIComponent(POP)}&certHash=${H1}`));
    applyRouteRelay(parseRoute('#/'));
    expect(store.getState().sessionOverrideUrl).toBe(null);
    expect(store.getState().certHashes).toEqual([]);
  });

  it('a dropped certHash keeps the relay and shows the pin note', async () => {
    const { store, applyRouteRelay, parseRoute, NOTE_CERT_HASH_DROPPED } = await load();
    applyRouteRelay(parseRoute(`#/view/AB2CD3?relay=https://relay.example.com:4433&certHash=${H1}`));
    expect(store.getState().serverUrl).toBe('https://relay.example.com:4433');
    expect(store.getState().certHashes).toEqual([]);
    expect(store.getState().relayLinkNote).toBe(NOTE_CERT_HASH_DROPPED);
  });

  it('an invalid relay wins the note over a dropped certHash', async () => {
    const { store, applyRouteRelay, parseRoute, NOTE_RELAY_INVALID } = await load();
    applyRouteRelay(parseRoute(`#/?relay=http://192.168.1.20&certHash=${H1}`));
    expect(store.getState().sessionOverrideUrl).toBe(null);
    expect(store.getState().relayLinkNote).toBe(NOTE_RELAY_INVALID);
  });

  it('gated deployment: pins are ignored with the relay', async () => {
    window.__GAWK_CONFIG__ = { allowCustomRelays: false };
    const { store, applyRouteRelay, parseRoute, NOTE_RELAY_NOT_ALLOWED } = await load();
    applyRouteRelay(parseRoute(`#/?relay=${encodeURIComponent(POP)}&certHash=${H1}`));
    expect(store.getState().sessionOverrideUrl).toBe(null);
    expect(store.getState().certHashes).toEqual([]);
    expect(store.getState().relayLinkNote).toBe(NOTE_RELAY_NOT_ALLOWED);
  });
});
