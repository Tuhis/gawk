// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

// R37 (docs/40 §4.7): links stay short on the default relay and carry
// ?relay= on any other — that parameter is what makes a code minted on
// relay B joinable from a UI deployed against relay A.
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
