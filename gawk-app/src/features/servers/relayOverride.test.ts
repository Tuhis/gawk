// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

// R37 (docs/40 §4.2): route → session override wiring, incl. the D6 gate.
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
