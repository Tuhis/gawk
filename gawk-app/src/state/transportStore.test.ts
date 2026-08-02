// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

// The store resolves its default at module-evaluation time, so every case has
// to re-import it with the window/localStorage of that case already in place.
async function freshStore() {
  vi.resetModules();
  const mod = await import('./transportStore');
  return mod.useTransportStore.getState();
}

function setHostname(hostname: string) {
  Object.defineProperty(window, 'location', {
    configurable: true,
    value: { ...window.location, hostname },
  });
}

describe('transportStore default relay URL', () => {
  const realLocation = window.location;

  beforeEach(() => {
    localStorage.clear();
    window.__GAWK_CONFIG__ = {};
  });

  afterEach(() => {
    Object.defineProperty(window, 'location', {
      configurable: true,
      value: realLocation,
    });
    delete window.__GAWK_CONFIG__;
    localStorage.clear();
  });

  // The self-hosting case, and the reason config.relayUrl exists: before it,
  // any origin that was not the reference deployment fell through to
  // localhost, so a self-hoster's viewers each had to paste the relay URL
  // into settings before a join link would work.
  it('uses the deployment-configured relay on any host', async () => {
    setHostname('gawk.example.com');
    window.__GAWK_CONFIG__ = { relayUrl: 'https://relay.example.com:4433' };
    expect((await freshStore()).serverUrl).toBe('https://relay.example.com:4433');
  });

  it('prefers the configured relay over the reference deployment default', async () => {
    setHostname('gawk.ioio.fi');
    window.__GAWK_CONFIG__ = { relayUrl: 'https://relay.example.com:4433' };
    expect((await freshStore()).serverUrl).toBe('https://relay.example.com:4433');
  });

  it('keeps the reference deployment default when nothing is configured', async () => {
    setHostname('gawk.ioio.fi');
    expect((await freshStore()).serverUrl).toBe('https://api.gawk.ioio.fi:4433');
  });

  // `npm run dev` must keep working with no config at all.
  it('falls back to localhost for local dev', async () => {
    setHostname('localhost');
    expect((await freshStore()).serverUrl).toBe('https://localhost:4433');
  });

  // A URL pasted into settings is the user's own override and outranks both.
  it('lets a persisted setting win over the configured relay', async () => {
    setHostname('gawk.example.com');
    window.__GAWK_CONFIG__ = { relayUrl: 'https://relay.example.com:4433' };
    localStorage.setItem('gawk.serverUrl', 'https://other.example.com:4433');
    expect((await freshStore()).serverUrl).toBe('https://other.example.com:4433');
  });
});
