// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

// The store resolves its default at module-evaluation time, so every case has
// to re-import it with the window/localStorage of that case already in place.
async function freshStore() {
  vi.resetModules();
  const mod = await import('./transportStore');
  return mod.useTransportStore.getState();
}

async function freshModule() {
  vi.resetModules();
  return import('./transportStore');
}

function setHostname(hostname: string) {
  Object.defineProperty(window, 'location', {
    configurable: true,
    value: { ...window.location, hostname },
  });
}

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

describe('transportStore default relay URL', () => {
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
});

// R37 §4.1.2: the legacy three-key model migrates on first load, both
// shapes, idempotently, and the legacy keys are removed afterwards.
describe('legacy key migration', () => {
  it('migrates a custom legacy URL into a selected entry (custom shape)', async () => {
    setHostname('gawk.example.com');
    window.__GAWK_CONFIG__ = { relayUrl: 'https://relay.example.com:4433' };
    localStorage.setItem('gawk.serverUrl', 'https://other.example.com:4433');
    localStorage.setItem('gawk.publishSecret', 'hunter2');
    localStorage.setItem('gawk.certHashHex', 'abcd');

    const s = await freshStore();
    // The pre-R37 test "lets a persisted setting win over the configured
    // relay" — same behaviour, now via migration + selection.
    expect(s.serverUrl).toBe('https://other.example.com:4433');
    expect(s.publishSecret).toBe('hunter2');
    expect(s.certHashHex).toBe('abcd');
    expect(s.resolvedSource).toBe('selected');
    // Legacy keys are gone.
    expect(localStorage.getItem('gawk.serverUrl')).toBe(null);
    expect(localStorage.getItem('gawk.publishSecret')).toBe(null);
    expect(localStorage.getItem('gawk.certHashHex')).toBe(null);
  });

  it('attaches default-shaped legacy credentials to the pinned default', async () => {
    setHostname('localhost');
    localStorage.setItem('gawk.serverUrl', 'https://localhost:4433');
    localStorage.setItem('gawk.publishSecret', 'dev-secret');
    localStorage.setItem('gawk.certHashHex', 'ff00');

    const s = await freshStore();
    expect(s.serverUrl).toBe('https://localhost:4433');
    expect(s.resolvedSource).toBe('default');
    expect(s.publishSecret).toBe('dev-secret');
    expect(s.certHashHex).toBe('ff00');
    expect(s.servers.find((e) => e.id === 'default')?.url).toBe('https://localhost:4433');
  });

  it('treats unparseable legacy junk as default-shaped (no identity to keep)', async () => {
    setHostname('localhost');
    localStorage.setItem('gawk.serverUrl', 'not a url');
    localStorage.setItem('gawk.publishSecret', 'sec');
    const s = await freshStore();
    expect(s.serverUrl).toBe('https://localhost:4433');
    expect(s.publishSecret).toBe('sec');
  });

  it('is idempotent: a second load changes nothing', async () => {
    setHostname('gawk.example.com');
    window.__GAWK_CONFIG__ = { relayUrl: 'https://relay.example.com:4433' };
    localStorage.setItem('gawk.serverUrl', 'https://other.example.com:4433');
    await freshStore();
    const serversAfterFirst = localStorage.getItem('gawk.servers');
    const selectedAfterFirst = localStorage.getItem('gawk.selectedServer');
    const s = await freshStore();
    expect(localStorage.getItem('gawk.servers')).toBe(serversAfterFirst);
    expect(localStorage.getItem('gawk.selectedServer')).toBe(selectedAfterFirst);
    expect(s.serverUrl).toBe('https://other.example.com:4433');
  });
});

describe('resolution precedence (override > selected > default)', () => {
  it('selected entry outranks the default; override outranks both', async () => {
    setHostname('localhost');
    const mod = await freshModule();
    const store = mod.useTransportStore;

    const id = store.getState().addServer({
      label: 'Homelab',
      url: 'https://relay.home.example:4433',
      publishSecret: 'homesec',
    });
    expect(id).not.toBe(null);
    expect(store.getState().serverUrl).toBe('https://localhost:4433');

    store.getState().selectServer(id!);
    expect(store.getState().serverUrl).toBe('https://relay.home.example:4433');
    expect(store.getState().publishSecret).toBe('homesec');
    expect(store.getState().resolvedSource).toBe('selected');

    store.getState().setSessionOverride('https://link.example.com:4433');
    expect(store.getState().serverUrl).toBe('https://link.example.com:4433');
    expect(store.getState().resolvedSource).toBe('override');
    // Unsaved override carries no credentials (D4).
    expect(store.getState().publishSecret).toBe('');
    expect(store.getState().certHashHex).toBe('');

    store.getState().setSessionOverride(null);
    expect(store.getState().serverUrl).toBe('https://relay.home.example:4433');
  });

  it('an override matching a saved entry attaches that entry (normalized, F8)', async () => {
    setHostname('localhost');
    const mod = await freshModule();
    const store = mod.useTransportStore;
    // Mixed-case, trailing-slash save still credential-matches its link.
    store.getState().addServer({
      label: 'Homelab',
      url: 'https://Relay.Home.Example:4433/',
      publishSecret: 'homesec',
      certHashHex: 'aa11',
    });
    store.getState().setSessionOverride('https://relay.home.example:4433');
    expect(store.getState().publishSecret).toBe('homesec');
    expect(store.getState().certHashHex).toBe('aa11');
    expect(store.getState().resolvedEntryId).not.toBe(null);
  });

  it('an override equal to the default resolves to the default, credentials included', async () => {
    setHostname('localhost');
    localStorage.setItem('gawk.publishSecret', 'devsec');
    localStorage.setItem('gawk.serverUrl', 'https://localhost:4433');
    const mod = await freshModule();
    const store = mod.useTransportStore;
    store.getState().setSessionOverride('https://localhost:4433');
    expect(store.getState().publishSecret).toBe('devsec');
    expect(store.getState().resolvedEntryId).toBe('default');
  });

  it('an unknown selection falls back to the default', async () => {
    setHostname('localhost');
    localStorage.setItem('gawk.selectedServer', 'srv-gone');
    const s = await freshStore();
    expect(s.serverUrl).toBe('https://localhost:4433');
    expect(s.selectedServerId).toBe('default');
  });
});

describe('storage semantics', () => {
  it('degrades corrupt gawk.servers to an empty list', async () => {
    setHostname('localhost');
    localStorage.setItem('gawk.servers', '{not json');
    const s = await freshStore();
    expect(s.servers).toEqual([]);
    expect(s.serverUrl).toBe('https://localhost:4433');
  });

  it('drops individual entries with unusable urls, keeps the rest', async () => {
    setHostname('localhost');
    localStorage.setItem(
      'gawk.servers',
      JSON.stringify([
        { id: 'srv-1', label: 'ok', url: 'https://a.example:4433', publishSecret: '', certHashHex: '' },
        { id: 'srv-2', label: 'bad', url: 'http://insecure.example', publishSecret: '', certHashHex: '' },
      ]),
    );
    const s = await freshStore();
    expect(s.servers.map((e) => e.id)).toEqual(['srv-1']);
  });

  it('preserves unknown entry fields across a rewrite (forward compat, §4.9)', async () => {
    setHostname('localhost');
    localStorage.setItem(
      'gawk.servers',
      JSON.stringify([
        {
          id: 'srv-1',
          label: 'ok',
          url: 'https://a.example:4433',
          publishSecret: '',
          certHashHex: '',
          auth: { mode: 'future' },
        },
      ]),
    );
    const mod = await freshModule();
    mod.useTransportStore.getState().updateServer('srv-1', { label: 'renamed' });
    const stored = JSON.parse(localStorage.getItem('gawk.servers')!);
    expect(stored[0].auth).toEqual({ mode: 'future' });
    expect(stored[0].label).toBe('renamed');
  });

  // F9: the default's credential record is keyed to the URL it was saved
  // against; a chart-side relayUrl change discards it rather than presenting
  // the old relay's secret to the new host.
  it('discards default credentials when the recomputed default URL changes', async () => {
    setHostname('localhost');
    localStorage.setItem(
      'gawk.servers',
      JSON.stringify([
        {
          id: 'default',
          label: '',
          url: 'https://old-relay.example.com:4433',
          publishSecret: 'oldsec',
          certHashHex: 'dead',
        },
      ]),
    );
    window.__GAWK_CONFIG__ = { relayUrl: 'https://new-relay.example.com:4433' };
    const s = await freshStore();
    expect(s.serverUrl).toBe('https://new-relay.example.com:4433');
    expect(s.publishSecret).toBe('');
    expect(s.certHashHex).toBe('');
    expect(JSON.parse(localStorage.getItem('gawk.servers')!)).toEqual([]);
  });

  // F11: re-read on panel open, last-writer-wins.
  it('reloadFromStorage picks up another tab’s writes', async () => {
    setHostname('localhost');
    const mod = await freshModule();
    const store = mod.useTransportStore;
    expect(store.getState().servers).toEqual([]);
    localStorage.setItem(
      'gawk.servers',
      JSON.stringify([
        { id: 'srv-9', label: 'From other tab', url: 'https://b.example:4433', publishSecret: '', certHashHex: '' },
      ]),
    );
    localStorage.setItem('gawk.selectedServer', 'srv-9');
    store.getState().reloadFromStorage();
    expect(store.getState().servers.map((e) => e.id)).toEqual(['srv-9']);
    expect(store.getState().serverUrl).toBe('https://b.example:4433');
  });
});

describe('entry management', () => {
  it('normalizes URLs on every write path (F8)', async () => {
    setHostname('localhost');
    const mod = await freshModule();
    const store = mod.useTransportStore;
    const id = store.getState().addServer({ label: '', url: 'https://Mixed.Case.Example:4433/' });
    expect(store.getState().servers.find((e) => e.id === id)?.url).toBe(
      'https://mixed.case.example:4433',
    );
    // Label defaults to the host when empty.
    expect(store.getState().servers.find((e) => e.id === id)?.label).toBe('mixed.case.example:4433');
    store.getState().updateServer(id!, { url: 'https://Other.Example:443/' });
    expect(store.getState().servers.find((e) => e.id === id)?.url).toBe('https://other.example');
  });

  it('rejects invalid URLs on add and update', async () => {
    setHostname('localhost');
    const mod = await freshModule();
    const store = mod.useTransportStore;
    expect(store.getState().addServer({ label: 'x', url: 'http://nope.example' })).toBe(null);
    const id = store.getState().addServer({ label: 'x', url: 'https://ok.example:4433' })!;
    expect(store.getState().updateServer(id, { url: 'not a url' })).toBe(false);
    expect(store.getState().servers.find((e) => e.id === id)?.url).toBe('https://ok.example:4433');
  });

  it('locks the pinned default’s identity but allows credential rotation (F4)', async () => {
    setHostname('localhost');
    const mod = await freshModule();
    const store = mod.useTransportStore;
    expect(store.getState().updateServer('default', { label: 'nope', url: 'https://x.example' })).toBe(false);
    expect(store.getState().updateServer('default', { publishSecret: 'rotated' })).toBe(true);
    expect(store.getState().publishSecret).toBe('rotated');
    store.getState().removeServer('default');
    expect(store.getState().serverUrl).toBe('https://localhost:4433'); // still there
  });

  it('removing the selected entry falls back to the default', async () => {
    setHostname('localhost');
    const mod = await freshModule();
    const store = mod.useTransportStore;
    const id = store.getState().addServer({ label: 'x', url: 'https://ok.example:4433' })!;
    store.getState().selectServer(id);
    store.getState().removeServer(id);
    expect(store.getState().selectedServerId).toBe('default');
    expect(store.getState().serverUrl).toBe('https://localhost:4433');
  });
});

// F3: credential writes land on whatever the store currently resolves to.
describe('per-resolved-server credential writes', () => {
  it('writes the secret to the selected custom entry', async () => {
    setHostname('localhost');
    const mod = await freshModule();
    const store = mod.useTransportStore;
    const id = store.getState().addServer({ label: 'x', url: 'https://ok.example:4433' })!;
    store.getState().selectServer(id);
    store.getState().setPublishSecret('persec');
    expect(store.getState().servers.find((e) => e.id === id)?.publishSecret).toBe('persec');
    // The default's credentials are untouched.
    store.getState().selectServer('default');
    expect(store.getState().publishSecret).toBe('');
  });

  it('holds an unsaved override’s secret in session memory only', async () => {
    setHostname('localhost');
    const mod = await freshModule();
    const store = mod.useTransportStore;
    store.getState().setSessionOverride('https://link.example.com:4433');
    store.getState().setPublishSecret('linksec');
    expect(store.getState().publishSecret).toBe('linksec');
    expect(localStorage.getItem('gawk.servers') ?? '[]').not.toContain('linksec');
    // Clearing the override drops the session credential.
    store.getState().setSessionOverride(null);
    store.getState().setSessionOverride('https://link.example.com:4433');
    expect(store.getState().publishSecret).toBe('');
  });
});
