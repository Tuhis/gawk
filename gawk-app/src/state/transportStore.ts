import { create } from 'zustand';

import { getDevCertHashHex, getRelayUrl } from '../config';
import { normalizeRelayOrigin } from '../lib/relayUrl';

// R37 (docs/40 §4.1): the server model. What was three global values
// (serverUrl / certHashHex / publishSecret) is now a list of saved servers
// plus a selection, resolved to the same three values every existing
// consumer keeps reading from this store — the transport layer is untouched.
//
// Storage:
//   gawk.servers        JSON array of RelayServerEntry — custom entries, plus
//                       at most one credentials-only record for the pinned
//                       default (id "default", no label; its `url` is a guard,
//                       not a source — see defaultCredentials()).
//   gawk.selectedServer the selected entry id; absent/unknown ⇒ "default".
//
// The pinned default's identity is never stored (D5): its URL is recomputed
// from config every load, so a chart-side relayUrl change reaches users who
// have saved entries. Legacy keys (gawk.serverUrl / certHashHex /
// publishSecret) migrate on first load (§4.1.2) and are then removed.
const LS_SERVERS = 'gawk.servers';
const LS_SELECTED = 'gawk.selectedServer';

// Legacy single-value keys, pre-R37. Read once by the migration, then removed.
const LS_LEGACY_SERVER_URL = 'gawk.serverUrl';
const LS_LEGACY_CERT_HASH = 'gawk.certHashHex';
const LS_LEGACY_PUBLISH_SECRET = 'gawk.publishSecret';

// The pinned default's reserved id (docs/40 §4.1.1).
export const DEFAULT_SERVER_ID = 'default';

export interface RelayServerEntry {
  id: string;
  label: string;
  // Normalized https origin (lib/relayUrl.ts) on every write path (F8).
  url: string;
  // Per-server credentials (D4): the stored secret for server A can never be
  // presented to server B, whatever a link says.
  publishSecret: string;
  certHashHex: string;
  // Unknown fields are preserved on rewrite (forward compatibility for the
  // §4.9 managed-relay extension points).
  [key: string]: unknown;
}

// Where the resolved connection came from — drives the in-session indicator
// (§4.3) and phase E's telemetry guard.
export type ResolvedSource = 'override' | 'selected' | 'default';

// Precedence (docs/40 §4.1.1): the deployment's configured relay (gawk-app
// chart config.relayUrl, rendered into /config.js) > the reference
// deployment's own origin > local dev.
export function defaultServerUrl(): string {
  const configured = getRelayUrl();
  if (configured) {
    // Operators write clean origins, but normalize anyway so the F8/F9
    // equality guards hold; an unparseable configured value falls through
    // verbatim rather than silently breaking an existing install.
    return normalizeRelayOrigin(configured) ?? configured;
  }
  if (typeof window !== 'undefined' && window.location.hostname === 'gawk.ioio.fi') {
    return 'https://api.gawk.ioio.fi:4433';
  }
  return 'https://localhost:4433';
}

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

function str(v: unknown): string {
  return typeof v === 'string' ? v : '';
}

// Parse gawk.servers. Corrupt JSON or a non-array degrades to "no custom
// servers" rather than throwing — losing a hand-added list beats a boot loop
// (§4.1.1). Entries with an unusable id/url are dropped individually; the
// default credentials record only needs id + url.
function readStoredServers(): RelayServerEntry[] {
  let raw: string | null = null;
  try {
    raw = localStorage.getItem(LS_SERVERS);
  } catch {
    return [];
  }
  if (!raw) return [];
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    return [];
  }
  if (!Array.isArray(parsed)) return [];
  const out: RelayServerEntry[] = [];
  for (const item of parsed) {
    if (!isRecord(item)) continue;
    const id = str(item.id);
    const url = normalizeRelayOrigin(str(item.url));
    if (id === '' || url === null) continue;
    out.push({
      ...item,
      id,
      url,
      label: str(item.label),
      publishSecret: str(item.publishSecret),
      certHashHex: str(item.certHashHex),
    });
  }
  return out;
}

function writeStoredServers(servers: RelayServerEntry[]): void {
  try {
    localStorage.setItem(LS_SERVERS, JSON.stringify(servers));
  } catch {
    // Quota/privacy-mode failures degrade to session-only state.
  }
}

function readSelectedId(): string {
  try {
    return localStorage.getItem(LS_SELECTED) ?? DEFAULT_SERVER_ID;
  } catch {
    return DEFAULT_SERVER_ID;
  }
}

function writeSelectedId(id: string): void {
  try {
    localStorage.setItem(LS_SELECTED, id);
  } catch {
    // Same degradation as writeStoredServers.
  }
}

// §4.1.2 migration, idempotent: runs only while legacy keys exist, removes
// them after a successful write of the new ones.
function migrateLegacyKeys(): void {
  let legacyUrl: string | null;
  let legacyCert: string | null;
  let legacySecret: string | null;
  try {
    legacyUrl = localStorage.getItem(LS_LEGACY_SERVER_URL);
    legacyCert = localStorage.getItem(LS_LEGACY_CERT_HASH);
    legacySecret = localStorage.getItem(LS_LEGACY_PUBLISH_SECRET);
  } catch {
    return;
  }
  if (legacyUrl === null && legacyCert === null && legacySecret === null) return;

  const servers = readStoredServers();
  const defaultUrl = normalizeRelayOrigin(defaultServerUrl());
  const normalizedLegacy = legacyUrl === null ? null : normalizeRelayOrigin(legacyUrl);
  const credentials = {
    publishSecret: (legacySecret ?? '').trim(),
    certHashHex: (legacyCert ?? '').trim(),
  };

  const isDefaultShaped =
    legacyUrl === null ||
    legacyUrl.trim() === '' ||
    normalizedLegacy === null || // unparseable legacy junk carries no identity
    normalizedLegacy === defaultUrl;

  if (isDefaultShaped) {
    // Credentials-only record for the pinned default, keyed to the URL they
    // were saved against (F9). Nothing to store when both are empty.
    if ((credentials.publishSecret !== '' || credentials.certHashHex !== '') && defaultUrl !== null) {
      const rest = servers.filter((s) => s.id !== DEFAULT_SERVER_ID);
      rest.unshift({
        id: DEFAULT_SERVER_ID,
        label: '',
        url: defaultUrl,
        ...credentials,
      });
      writeStoredServers(rest);
    }
    writeSelectedId(DEFAULT_SERVER_ID);
  } else {
    // A custom relay pointed at by the old global setting keeps working
    // without the user noticing (§4.1.2).
    const id = generateEntryId(servers);
    servers.push({
      id,
      label: 'Migrated server',
      url: normalizedLegacy,
      ...credentials,
    });
    writeStoredServers(servers);
    writeSelectedId(id);
  }

  try {
    localStorage.removeItem(LS_LEGACY_SERVER_URL);
    localStorage.removeItem(LS_LEGACY_CERT_HASH);
    localStorage.removeItem(LS_LEGACY_PUBLISH_SECRET);
  } catch {
    // If removal fails the migration re-runs next load; every branch above is
    // idempotent under a re-run (same inputs, same outputs).
  }
}

// F9: the default's credential record follows the URL it was saved against,
// not the id — a chart-side relayUrl change must not present the old relay's
// secret and cert hash to the new host. Discard on mismatch, at read time.
function pruneStaleDefaultCredentials(servers: RelayServerEntry[]): RelayServerEntry[] {
  const defaultUrl = normalizeRelayOrigin(defaultServerUrl());
  const record = servers.find((s) => s.id === DEFAULT_SERVER_ID);
  if (!record || record.url === defaultUrl) return servers;
  const pruned = servers.filter((s) => s.id !== DEFAULT_SERVER_ID);
  writeStoredServers(pruned);
  return pruned;
}

function generateEntryId(existing: RelayServerEntry[]): string {
  // Collision-proof against the current list without needing crypto:
  // "srv-" + monotonic suffix.
  let n = existing.length + 1;
  // eslint-disable-next-line no-constant-condition
  while (true) {
    const id = `srv-${n}`;
    if (id !== DEFAULT_SERVER_ID && !existing.some((s) => s.id === id)) return id;
    n += 1;
  }
}

interface ResolvedTransport {
  serverUrl: string;
  certHashHex: string;
  publishSecret: string;
  resolvedSource: ResolvedSource;
  // The entry the resolution landed on, or null for an unsaved override.
  resolvedEntryId: string | null;
}

interface TransportSettingsState extends ResolvedTransport {
  // Custom entries only — the pinned default renders from defaultServerUrl()
  // + defaultCredentials, not from this list.
  servers: RelayServerEntry[];
  selectedServerId: string;
  // `?relay=` session override (docs/40 §4.2): normalized origin, held in
  // memory only, never persisted (D2).
  sessionOverrideUrl: string | null;
  // Credentials entered while an unsaved override is active — session-only
  // (F3); they persist only through an explicit "Save this server".
  overridePublishSecret: string;
  overrideCertHashHex: string;
  // Quiet note about this route's ?relay= handling (docs/40 §4.2 — an
  // invalid value, or a link asking for a server on a gated deployment).
  // Route-scoped like the override itself; rendered by the in-session
  // indicator, never fatal.
  relayLinkNote: string | null;
  // R37 (docs/40 D16): true while this session reports diagnostics to a
  // foreign relay's advertised collector — the in-session indicator carries
  // the disclosure. Session-scoped, set by the screens when a 0x12 lands on
  // a non-default resolution, cleared with the override on route change.
  foreignTelemetryActive: boolean;

  // Picker actions (SP3).
  selectServer: (id: string) => void;
  addServer: (entry: { label: string; url: string; publishSecret?: string; certHashHex?: string }) => string | null;
  updateServer: (
    id: string,
    patch: Partial<Pick<RelayServerEntry, 'label' | 'url' | 'publishSecret' | 'certHashHex'>>,
  ) => boolean;
  removeServer: (id: string) => void;
  // Route-driven (SP2): null clears. An invalid value is the caller's problem
  // — routing only ever passes normalized origins.
  setSessionOverride: (url: string | null) => void;
  setRelayLinkNote: (note: string | null) => void;
  setForeignTelemetryActive: (active: boolean) => void;
  // Cross-tab rule (F11): the panel re-reads storage when it opens;
  // last-writer-wins; no storage-event reactivity.
  reloadFromStorage: () => void;

  // Legacy surface, kept for the frozen #/debug/* tree and the secret prompt.
  // Semantics are per-resolved-server now: the credential setters write to
  // whatever entry the store currently resolves to (docs/40 §4.2 F3).
  setServerUrl: (url: string) => void;
  setCertHashHex: (hash: string) => void;
  setPublishSecret: (secret: string) => void;
}

interface ResolutionInputs {
  servers: RelayServerEntry[];
  selectedServerId: string;
  sessionOverrideUrl: string | null;
  overridePublishSecret: string;
  overrideCertHashHex: string;
}

// The default's credentials-only record, if one is stored and still matches
// the current default URL (pruning happens at load; this re-checks so a
// same-session config change cannot resurrect stale credentials).
function defaultCredentials(servers: RelayServerEntry[]): { publishSecret: string; certHashHex: string } {
  const defaultUrl = normalizeRelayOrigin(defaultServerUrl());
  const record = servers.find((s) => s.id === DEFAULT_SERVER_ID);
  if (record && record.url === defaultUrl) {
    return { publishSecret: record.publishSecret, certHashHex: record.certHashHex };
  }
  return { publishSecret: '', certHashHex: '' };
}

// R38 (docs/41 §4.2.3): a local stack renders its relay's dev-certificate
// hash into /config.js, which is what makes the chrome-free `#/view/{id}`
// route work in a fresh profile instead of only where the broadcaster page
// had already written the hash to localStorage.
//
// A FALLBACK, never an override: a hash the developer typed wins, because
// they typed it while pointing somewhere else. And it is scoped by URL, not
// by entry id — the configured hash belongs to THIS deployment's relay, so
// presenting it to a foreign `?relay=` target would be handing one relay's
// identity to another.
function withDevCertFallback(resolved: ResolvedTransport): ResolvedTransport {
  if (resolved.certHashHex !== '') return resolved;
  const configured = getDevCertHashHex();
  if (configured === '') return resolved;
  if (normalizeRelayOrigin(resolved.serverUrl) !== normalizeRelayOrigin(defaultServerUrl())) {
    return resolved;
  }
  return { ...resolved, certHashHex: configured };
}

// Precedence, top wins: session override > selected entry > pinned default
// (docs/40 §4.1.1) — then R38's fallback fills a hash the resolution left
// empty. Every consumer reads this one, never resolveEntry.
function resolve(inputs: ResolutionInputs): ResolvedTransport {
  return withDevCertFallback(resolveEntry(inputs));
}

// An override whose URL equals a saved entry (the default included) resolves
// to that entry, credentials attached.
function resolveEntry(inputs: ResolutionInputs): ResolvedTransport {
  const { servers, selectedServerId, sessionOverrideUrl } = inputs;
  const defaultUrl = defaultServerUrl();
  const normalizedDefault = normalizeRelayOrigin(defaultUrl);

  if (sessionOverrideUrl !== null) {
    if (sessionOverrideUrl === normalizedDefault) {
      const creds = defaultCredentials(servers);
      return {
        serverUrl: defaultUrl,
        ...creds,
        resolvedSource: 'override',
        resolvedEntryId: DEFAULT_SERVER_ID,
      };
    }
    const match = servers.find((s) => s.id !== DEFAULT_SERVER_ID && s.url === sessionOverrideUrl);
    if (match) {
      return {
        serverUrl: match.url,
        certHashHex: match.certHashHex,
        publishSecret: match.publishSecret,
        resolvedSource: 'override',
        resolvedEntryId: match.id,
      };
    }
    return {
      serverUrl: sessionOverrideUrl,
      certHashHex: inputs.overrideCertHashHex,
      publishSecret: inputs.overridePublishSecret,
      resolvedSource: 'override',
      resolvedEntryId: null,
    };
  }

  if (selectedServerId !== DEFAULT_SERVER_ID) {
    const entry = servers.find((s) => s.id === selectedServerId);
    if (entry) {
      return {
        serverUrl: entry.url,
        certHashHex: entry.certHashHex,
        publishSecret: entry.publishSecret,
        resolvedSource: 'selected',
        resolvedEntryId: entry.id,
      };
    }
    // Unknown selection (deleted in another tab, corrupt value) ⇒ default.
  }

  const creds = defaultCredentials(servers);
  return {
    serverUrl: defaultUrl,
    ...creds,
    resolvedSource: 'default',
    resolvedEntryId: DEFAULT_SERVER_ID,
  };
}

function loadInitialState(): ResolutionInputs {
  migrateLegacyKeys();
  const servers = pruneStaleDefaultCredentials(readStoredServers());
  const storedSelection = readSelectedId();
  const selectedServerId =
    storedSelection === DEFAULT_SERVER_ID || servers.some((s) => s.id === storedSelection)
      ? storedSelection
      : DEFAULT_SERVER_ID;
  return {
    servers,
    selectedServerId,
    sessionOverrideUrl: null,
    overridePublishSecret: '',
    overrideCertHashHex: '',
  };
}

// relayLinkNote is not a resolution input — it rides beside the override.

// Custom entries as the picker lists them (the default's credentials record
// is internal storage, not a row).
export function customServers(servers: RelayServerEntry[]): RelayServerEntry[] {
  return servers.filter((s) => s.id !== DEFAULT_SERVER_ID);
}

// True when the RESOLVED server names the deployment's own relay — by
// normalized-URL equality, not entry id (docs/40 §5 G3): a user who manually
// saved the deployment's relay as a custom entry is id-non-default but
// URL-default, and anything keyed on identity semantics (the foreign-
// telemetry disclosure) must not treat them as foreign.
export function resolvedUrlIsDefault(): boolean {
  const { serverUrl } = useTransportStore.getState();
  return normalizeRelayOrigin(serverUrl) === normalizeRelayOrigin(defaultServerUrl());
}

export const useTransportStore = create<TransportSettingsState>((set, get) => {
  const initial = loadInitialState();

  // Every mutation recomputes the resolved trio so consumers keep reading
  // plain state fields (no selector changes anywhere in the transport layer).
  const apply = (inputs: ResolutionInputs) => {
    set({ ...inputs, ...resolve(inputs) });
  };

  const inputsOf = (s: TransportSettingsState): ResolutionInputs => ({
    servers: s.servers,
    selectedServerId: s.selectedServerId,
    sessionOverrideUrl: s.sessionOverrideUrl,
    overridePublishSecret: s.overridePublishSecret,
    overrideCertHashHex: s.overrideCertHashHex,
  });

  // Upsert the default's credentials-only record (F4: the rotation path).
  const writeDefaultCredentials = (
    servers: RelayServerEntry[],
    patch: Partial<Pick<RelayServerEntry, 'publishSecret' | 'certHashHex'>>,
  ): RelayServerEntry[] => {
    const defaultUrl = normalizeRelayOrigin(defaultServerUrl());
    if (defaultUrl === null) return servers;
    const existing = servers.find((s) => s.id === DEFAULT_SERVER_ID);
    const current = defaultCredentials(servers);
    const next: RelayServerEntry = {
      ...(existing && existing.url === defaultUrl ? existing : {}),
      id: DEFAULT_SERVER_ID,
      label: '',
      url: defaultUrl,
      publishSecret: patch.publishSecret ?? current.publishSecret,
      certHashHex: patch.certHashHex ?? current.certHashHex,
    };
    const rest = servers.filter((s) => s.id !== DEFAULT_SERVER_ID);
    if (next.publishSecret === '' && next.certHashHex === '') {
      writeStoredServers(rest);
      return rest;
    }
    const out = [next, ...rest];
    writeStoredServers(out);
    return out;
  };

  // Credential writes land on whatever the store currently resolves to
  // (F3/F4): the default's record, a custom entry, or — for an unsaved
  // override — session-only memory.
  const setResolvedCredential = (patch: {
    publishSecret?: string;
    certHashHex?: string;
  }) => {
    const s = get();
    const resolved = resolve(inputsOf(s));
    if (resolved.resolvedEntryId === null) {
      apply({
        ...inputsOf(s),
        overridePublishSecret: patch.publishSecret ?? s.overridePublishSecret,
        overrideCertHashHex: patch.certHashHex ?? s.overrideCertHashHex,
      });
      return;
    }
    if (resolved.resolvedEntryId === DEFAULT_SERVER_ID) {
      apply({ ...inputsOf(s), servers: writeDefaultCredentials(s.servers, patch) });
      return;
    }
    const servers = s.servers.map((e) =>
      e.id === resolved.resolvedEntryId ? { ...e, ...patch } : e,
    );
    writeStoredServers(servers);
    apply({ ...inputsOf(s), servers });
  };

  return {
    ...initial,
    ...resolve(initial),
    relayLinkNote: null,
    foreignTelemetryActive: false,

    setRelayLinkNote: (relayLinkNote) => set({ relayLinkNote }),
    setForeignTelemetryActive: (foreignTelemetryActive) => set({ foreignTelemetryActive }),

    selectServer: (id) => {
      const s = get();
      const valid = id === DEFAULT_SERVER_ID || s.servers.some((e) => e.id === id);
      if (!valid) return;
      writeSelectedId(id);
      apply({ ...inputsOf(s), selectedServerId: id });
    },

    addServer: (entry) => {
      const url = normalizeRelayOrigin(entry.url);
      if (url === null) return null;
      const s = get();
      const id = generateEntryId(s.servers);
      const label = entry.label.trim() || new URL(url).host;
      const servers = [
        ...s.servers,
        {
          id,
          label,
          url,
          publishSecret: entry.publishSecret ?? '',
          certHashHex: entry.certHashHex ?? '',
        },
      ];
      writeStoredServers(servers);
      apply({ ...inputsOf(s), servers });
      return id;
    },

    updateServer: (id, patch) => {
      const s = get();
      if (id === DEFAULT_SERVER_ID) {
        // The pinned default's identity is locked; only its credential slots
        // are editable (F4 — the rotation path).
        const credPatch: Partial<Pick<RelayServerEntry, 'publishSecret' | 'certHashHex'>> = {};
        if (patch.publishSecret !== undefined) credPatch.publishSecret = patch.publishSecret;
        if (patch.certHashHex !== undefined) credPatch.certHashHex = patch.certHashHex;
        if (Object.keys(credPatch).length === 0) return false;
        apply({ ...inputsOf(s), servers: writeDefaultCredentials(s.servers, credPatch) });
        return true;
      }
      const entry = s.servers.find((e) => e.id === id);
      if (!entry) return false;
      let url = entry.url;
      if (patch.url !== undefined) {
        const normalized = normalizeRelayOrigin(patch.url);
        if (normalized === null) return false;
        url = normalized;
      }
      const servers = s.servers.map((e) =>
        e.id === id
          ? {
              ...e,
              label: patch.label !== undefined ? patch.label.trim() || e.label : e.label,
              url,
              publishSecret: patch.publishSecret ?? e.publishSecret,
              certHashHex: patch.certHashHex ?? e.certHashHex,
            }
          : e,
      );
      writeStoredServers(servers);
      apply({ ...inputsOf(s), servers });
      return true;
    },

    removeServer: (id) => {
      if (id === DEFAULT_SERVER_ID) return; // pinned (D5)
      const s = get();
      const servers = s.servers.filter((e) => e.id !== id);
      if (servers.length === s.servers.length) return;
      writeStoredServers(servers);
      const selectedServerId = s.selectedServerId === id ? DEFAULT_SERVER_ID : s.selectedServerId;
      if (selectedServerId !== s.selectedServerId) writeSelectedId(selectedServerId);
      apply({ ...inputsOf(s), servers, selectedServerId });
    },

    setSessionOverride: (url) => {
      const s = get();
      const normalized = url === null ? null : normalizeRelayOrigin(url);
      if (url !== null && normalized === null) return; // routing pre-validates; belt and braces
      if (normalized === s.sessionOverrideUrl) return;
      apply({
        ...inputsOf(s),
        sessionOverrideUrl: normalized,
        // A new (or cleared) override never inherits the previous override's
        // session-typed credentials.
        overridePublishSecret: '',
        overrideCertHashHex: '',
      });
    },

    reloadFromStorage: () => {
      const s = get();
      const servers = pruneStaleDefaultCredentials(readStoredServers());
      const storedSelection = readSelectedId();
      const selectedServerId =
        storedSelection === DEFAULT_SERVER_ID || servers.some((e) => e.id === storedSelection)
          ? storedSelection
          : DEFAULT_SERVER_ID;
      apply({ ...inputsOf(s), servers, selectedServerId });
    },

    // Legacy debug-tree surface: point the store at a raw URL. Equal to the
    // default ⇒ plain default selection; anything else lands in a dedicated
    // "Manual" entry so the frozen #/debug/* pages keep behaving like the
    // old single-value field without growing a second storage model.
    setServerUrl: (url) => {
      const s = get();
      const normalized = normalizeRelayOrigin(url);
      if (normalized === null || normalized === normalizeRelayOrigin(defaultServerUrl())) {
        writeSelectedId(DEFAULT_SERVER_ID);
        apply({ ...inputsOf(s), selectedServerId: DEFAULT_SERVER_ID });
        return;
      }
      const existing = s.servers.find((e) => e.label === 'Manual (dev)');
      let servers = s.servers;
      let id: string;
      if (existing) {
        id = existing.id;
        servers = s.servers.map((e) => (e.id === id ? { ...e, url: normalized } : e));
      } else {
        id = generateEntryId(s.servers);
        servers = [
          ...s.servers,
          { id, label: 'Manual (dev)', url: normalized, publishSecret: '', certHashHex: '' },
        ];
      }
      writeStoredServers(servers);
      writeSelectedId(id);
      apply({ ...inputsOf(s), servers, selectedServerId: id });
    },

    setCertHashHex: (certHashHex) => setResolvedCredential({ certHashHex: certHashHex.trim() }),
    setPublishSecret: (publishSecret) => setResolvedCredential({ publishSecret: publishSecret.trim() }),
  };
});
