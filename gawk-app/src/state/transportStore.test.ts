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
  vi.unstubAllEnvs();
});

describe('transportStore default relay URL', () => {
  // The self-hosting case, and the reason config.relayUrl exists: without it,
  // any origin that is not the reference deployment falls through to
  // localhost, so a self-hoster's viewers would each have to paste the relay
  // URL into settings before a join link works.
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

// The legacy three-key model migrates on first load, both shapes,
// idempotently, and the legacy keys are removed afterwards.
describe('legacy key migration', () => {
  it('migrates a custom legacy URL into a selected entry (custom shape)', async () => {
    setHostname('gawk.example.com');
    window.__GAWK_CONFIG__ = { relayUrl: 'https://relay.example.com:4433' };
    localStorage.setItem('gawk.serverUrl', 'https://other.example.com:4433');
    localStorage.setItem('gawk.publishSecret', 'hunter2');
    localStorage.setItem('gawk.certHashHex', 'abcd');

    const s = await freshStore();
    // A persisted setting wins over the configured relay, via migration +
    // selection.
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
    // Unsaved override carries no credentials.
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

// The local stack renders its relay's dev certificate hash into /config.js
// so the chrome-free viewer route works in a fresh profile. A fallback, and
// scoped to this deployment's own relay.
describe('devCertHashHex fallback', () => {
  it('fills in the pinned default’s empty hash', async () => {
    setHostname('localhost');
    window.__GAWK_CONFIG__ = { devCertHashHex: 'beef00' };
    const s = await freshStore();
    expect(s.serverUrl).toBe('https://localhost:4433');
    expect(s.certHashHex).toBe('beef00');
  });

  it('does not override a hash the developer typed', async () => {
    setHostname('localhost');
    window.__GAWK_CONFIG__ = { devCertHashHex: 'beef00' };
    const mod = await freshModule();
    const store = mod.useTransportStore;
    store.getState().setCertHashHex('cafe11');
    expect(store.getState().certHashHex).toBe('cafe11');

    // Clearing it hands the resolution back to the configured value rather
    // than to an empty string — the stack's hash is still the right one.
    store.getState().setCertHashHex('');
    expect(store.getState().certHashHex).toBe('beef00');
  });

  it('does not override an entry-stored hash', async () => {
    setHostname('localhost');
    window.__GAWK_CONFIG__ = { devCertHashHex: 'beef00' };
    const mod = await freshModule();
    const store = mod.useTransportStore;
    const id = store.getState().addServer({
      label: 'Other',
      url: 'https://localhost:4433',
      certHashHex: 'aa11',
    });
    store.getState().selectServer(id!);
    expect(store.getState().certHashHex).toBe('aa11');
  });

  // The scoping rule: it belongs to THIS deployment's relay. Handing it to a
  // link-supplied relay would present one relay's identity to another.
  it('is not applied to a foreign ?relay= override', async () => {
    setHostname('localhost');
    window.__GAWK_CONFIG__ = { devCertHashHex: 'beef00' };
    const mod = await freshModule();
    const store = mod.useTransportStore;
    store.getState().setSessionOverride('https://relay.elsewhere.example:4433');
    expect(store.getState().certHashHex).toBe('');

    // …but an override naming the deployment's own relay is not foreign.
    store.getState().setSessionOverride('https://localhost:4433');
    expect(store.getState().certHashHex).toBe('beef00');
  });

  // A saved custom entry that happens to name the same relay is the same
  // relay: relay identity is URL equality.
  it('applies to a custom entry whose URL equals the default', async () => {
    setHostname('localhost');
    window.__GAWK_CONFIG__ = { devCertHashHex: 'beef00' };
    const mod = await freshModule();
    const store = mod.useTransportStore;
    const id = store.getState().addServer({ label: 'Same relay', url: 'https://localhost:4433' });
    store.getState().selectServer(id!);
    expect(store.getState().certHashHex).toBe('beef00');
  });

  // The gate lives in config.ts; assert the store inherits it rather than
  // re-deriving "is this local" for itself. The runner is Vite dev mode, so
  // the other half of isDevEnvironment() has to be turned off explicitly.
  it('is inert on a non-dev origin', async () => {
    vi.stubEnv('DEV', false);
    setHostname('gawk.example.com');
    window.__GAWK_CONFIG__ = {
      relayUrl: 'https://relay.example.com:4433',
      devCertHashHex: 'beef00',
    };
    const s = await freshStore();
    expect(s.serverUrl).toBe('https://relay.example.com:4433');
    expect(s.certHashHex).toBe('');
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

  // The default's credential record is keyed to the URL it was saved against;
  // a chart-side relayUrl change discards it rather than presenting the old
  // relay's secret to the new host.
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

  // Re-read on panel open, last-writer-wins.
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

// Credential writes land on whatever the store currently resolves to.
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

// R74 (docs/76 §6.1 items 5–6): a PoP publishes two pinned hashes (current +
// next, across the rolling rotation's overlap), so every resolution carries a
// LIST. The legacy `certHashHex` stays the first one.
describe('certificate hash lists (R74)', () => {
  const H1 = '11'.repeat(32);
  const H2 = '22'.repeat(32);
  const H3 = '33'.repeat(32);
  const POP = 'https://192.168.1.20:4433';

  it('an unsaved link override resolves to the link’s hashes', async () => {
    setHostname('localhost');
    const store = (await freshModule()).useTransportStore;
    store.getState().setSessionOverride(POP, [H1, H2]);
    expect(store.getState().serverUrl).toBe(POP);
    expect(store.getState().certHashes).toEqual([H1, H2]);
    expect(store.getState().certHashHex).toBe(H1);
    expect(store.getState().resolvedEntryId).toBe(null);
    // Clearing drops them with the override.
    store.getState().setSessionOverride(null);
    expect(store.getState().certHashes).toEqual([]);
  });

  it('a new link for the same relay with different hashes re-resolves', async () => {
    setHostname('localhost');
    const store = (await freshModule()).useTransportStore;
    store.getState().setSessionOverride(POP, [H1]);
    store.getState().setSessionOverride(POP, [H2, H3]);
    expect(store.getState().certHashes).toEqual([H2, H3]);
    store.getState().setSessionOverride(POP);
    expect(store.getState().certHashes).toEqual([]);
  });

  it('keeps the resolved list referentially stable across unrelated writes', async () => {
    setHostname('localhost');
    const store = (await freshModule()).useTransportStore;
    store.getState().setSessionOverride(POP, [H1, H2]);
    const before = store.getState().certHashes;
    store.getState().setPublishSecret('x');
    store.getState().addServer({ label: 'other', url: 'https://other.example:4433' });
    store.getState().setSessionOverride(POP, [H1, H2]);
    expect(store.getState().certHashes).toBe(before);
  });

  it('a link’s hashes outrank a saved entry’s for the same relay, the secret still attaches', async () => {
    setHostname('localhost');
    const store = (await freshModule()).useTransportStore;
    store.getState().addServer({ label: 'PoP', url: POP, publishSecret: 'sec', certHashes: [H3] });
    store.getState().setSessionOverride(POP, [H1, H2]);
    expect(store.getState().certHashes).toEqual([H1, H2]);
    expect(store.getState().publishSecret).toBe('sec');
    // A link without hashes uses the entry's.
    store.getState().setSessionOverride(POP);
    expect(store.getState().certHashes).toEqual([H3]);
  });

  it('saves and restores an entry’s list, keeping certHashHex as the first hash on disk', async () => {
    setHostname('localhost');
    let store = (await freshModule()).useTransportStore;
    const id = store.getState().addServer({ label: 'PoP', url: POP, certHashes: [H1, H2] })!;
    store.getState().selectServer(id);
    const stored = JSON.parse(localStorage.getItem('gawk.servers')!);
    expect(stored[0].certHashHex).toBe(H1);
    expect(stored[0].certHashes).toEqual([H1, H2]);

    store = (await freshModule()).useTransportStore;
    expect(store.getState().certHashes).toEqual([H1, H2]);
    expect(store.getState().certHashHex).toBe(H1);
  });

  it('a legacy single-hash entry resolves to a one-element list', async () => {
    setHostname('localhost');
    localStorage.setItem(
      'gawk.servers',
      JSON.stringify([{ id: 'srv-1', label: 'Old', url: POP, publishSecret: '', certHashHex: H1 }]),
    );
    localStorage.setItem('gawk.selectedServer', 'srv-1');
    const s = await freshStore();
    expect(s.certHashes).toEqual([H1]);
  });

  it('a typed comma-separated hash is stored as a list', async () => {
    setHostname('localhost');
    const store = (await freshModule()).useTransportStore;
    const id = store.getState().addServer({ label: 'PoP', url: POP })!;
    store.getState().selectServer(id);
    store.getState().updateServer(id, { certHashHex: ` ${H1.toUpperCase()} , ${H2} ` });
    expect(store.getState().certHashes).toEqual([H1, H2]);
    expect(store.getState().certHashHex).toBe(H1);
    // Replacing the text replaces the list.
    store.getState().setCertHashHex(H3);
    expect(store.getState().certHashes).toEqual([H3]);
    store.getState().setCertHashHex('');
    expect(store.getState().certHashes).toEqual([]);
  });

  it('a session-typed hash on an unsaved override wins over the link’s', async () => {
    setHostname('localhost');
    const store = (await freshModule()).useTransportStore;
    store.getState().setSessionOverride(POP, [H1]);
    store.getState().setCertHashHex(H2);
    expect(store.getState().certHashes).toEqual([H2]);
  });

  it('certHashList joins the resolved list for a dial', async () => {
    setHostname('localhost');
    const mod = await freshModule();
    mod.useTransportStore.getState().setSessionOverride(POP, [H1, H2]);
    expect(mod.selectCertHashList(mod.useTransportStore.getState())).toBe(`${H1},${H2}`);
  });
});

// config.relayCertHashes (docs/76 §6.1 item 6): the PoP's own /app/ names its
// LAN relay with both hashes. Scoped to the default URL like devCertHashHex,
// but NOT dev-gated: the named tier serves /app/ from a real hostname.
describe('relayCertHashes config (R74)', () => {
  const H1 = '11'.repeat(32);
  const H2 = '22'.repeat(32);

  it('pins the deployment’s own relay, even on a non-dev origin', async () => {
    vi.stubEnv('DEV', false);
    setHostname('p-7k2m9x.pop.gawk.ioio.fi');
    window.__GAWK_CONFIG__ = { relayUrl: 'https://192.168.1.20:4433', relayCertHashes: [H1, H2] };
    const s = await freshStore();
    expect(s.serverUrl).toBe('https://192.168.1.20:4433');
    expect(s.certHashes).toEqual([H1, H2]);
    expect(s.certHashHex).toBe(H1);
  });

  it('outranks devCertHashHex, and is never handed to another relay', async () => {
    setHostname('127.0.0.1');
    window.__GAWK_CONFIG__ = {
      relayUrl: 'https://192.168.1.20:4433',
      relayCertHashes: [H1, H2],
      devCertHashHex: 'beef00',
    };
    const store = (await freshModule()).useTransportStore;
    expect(store.getState().certHashes).toEqual([H1, H2]);
    store.getState().setSessionOverride('https://192.168.1.99:4433');
    expect(store.getState().certHashes).toEqual([]);
  });

  it('a stored hash for the default still wins', async () => {
    setHostname('127.0.0.1');
    window.__GAWK_CONFIG__ = { relayUrl: 'https://192.168.1.20:4433', relayCertHashes: [H1] };
    const store = (await freshModule()).useTransportStore;
    store.getState().setCertHashHex(H2);
    expect(store.getState().certHashes).toEqual([H2]);
  });

  it('ignores junk values', async () => {
    window.__GAWK_CONFIG__ = { relayCertHashes: ['', 42, ' AA '] as unknown as string[] };
    const mod = await freshModule();
    expect(mod.useTransportStore.getState().certHashes).toEqual(['aa']);
  });
});
