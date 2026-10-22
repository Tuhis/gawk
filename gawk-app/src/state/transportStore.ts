import { create } from 'zustand';

import { getDevCertHashHex, getRelayCertHashes, getRelayUrl } from '../config';
import { normalizeRelayOrigin } from '../lib/relayUrl';
import { readStored, writeStored } from '../lib/storage';

// The relay server model: saved servers plus a selection (and a route's
// ?relay= override), resolved to the serverUrl / certHashHex / publishSecret
// trio every consumer reads.
//
// Storage:
//   gawk.servers        JSON array of RelayServerEntry — custom entries, plus
//                       at most one credentials-only record for the pinned
//                       default (id "default", no label; its `url` is a guard,
//                       not a source — see defaultCredentials()).
//   gawk.selectedServer the selected entry id; absent/unknown ⇒ "default".
//
// The pinned default's identity is never stored: its URL is recomputed from
// config every load, so a chart-side relayUrl change reaches users who have
// saved entries. Legacy single-value keys migrate on first load and are then
// removed.
const LS_SERVERS = 'gawk.servers';
const LS_SELECTED = 'gawk.selectedServer';

const LS_LEGACY_SERVER_URL = 'gawk.serverUrl';
const LS_LEGACY_CERT_HASH = 'gawk.certHashHex';
const LS_LEGACY_PUBLISH_SECRET = 'gawk.publishSecret';

export const DEFAULT_SERVER_ID = 'default';

export interface RelayServerEntry {
  id: string;
  label: string;
  // Normalized https origin (lib/relayUrl.ts) on every write path.
  url: string;
  // Per-server credentials: the stored secret for server A can never be
  // presented to server B, whatever a link says.
  publishSecret: string;
  // The first pinned hash: what a pre-R74 build reads, kept in step with
  // `certHashes[0]` on every write.
  certHashHex: string;
  // R74: every pinned hash (a PoP publishes current + next across a rolling
  // rotation). Optional on disk; read through entryCertHashes().
  certHashes?: string[];
  // Unknown fields are preserved on rewrite (forward compatibility).
  [key: string]: unknown;
}

// A hash list from a typed value ("a, b"), a stored array or a config array:
// trimmed, lowercased, empties and repeats dropped. Not validated as hex
// beyond that: a wrong hash fails the handshake, which is the one honest
// signal (the routing parser is the strict one, for links).
export function normalizeCertHashes(value: unknown): string[] {
  const parts: unknown[] = Array.isArray(value) ? value : typeof value === 'string' ? value.split(',') : [];
  const out: string[] = [];
  for (const p of parts) {
    if (typeof p !== 'string') continue;
    const clean = p.trim().toLowerCase();
    if (clean !== '' && !out.includes(clean)) out.push(clean);
  }
  return out;
}

// An entry's pinned hashes. The list is trusted only while its first hash
// still equals certHashHex: a pre-R74 build that edited the hash wrote only
// certHashHex and carried the stale list along as an unknown field.
export function entryCertHashes(entry: { certHashHex: string; certHashes?: unknown }): string[] {
  const fromHex = normalizeCertHashes(entry.certHashHex);
  const list = normalizeCertHashes(entry.certHashes);
  if (list.length > 0 && list[0] === (fromHex[0] ?? '')) return list;
  return fromHex;
}

// The two on-disk hash fields for a list.
function hashFields(hashes: string[]): { certHashHex: string; certHashes: string[] } {
  return { certHashHex: hashes[0] ?? '', certHashes: hashes };
}

function sameHashes(a: string[], b: string[]): boolean {
  return a.length === b.length && a.every((h, i) => h === b[i]);
}

// The resolved list joined with ',', the form ConnectOptions.certHashHex
// takes (webTransportInit splits it). A string, so it is a stable effect
// dependency where an array would not be.
export function selectCertHashList(s: { certHashes: string[] }): string {
  return s.certHashes.join(',');
}

// Where the resolved connection came from; drives the in-session indicator.
export type ResolvedSource = 'override' | 'selected' | 'default';

// Precedence: the deployment's configured relay (gawk-app chart
// config.relayUrl) > the reference deployment's own origin > local dev.
export function defaultServerUrl(): string {
  const configured = getRelayUrl();
  if (configured) {
    // Normalize anyway so the URL-equality guards hold; an unparseable value
    // falls through verbatim rather than silently breaking an install.
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
// servers" rather than throwing: losing a hand-added list beats a boot loop.
// Entries with an unusable id/url are dropped individually; the
// default credentials record only needs id + url.
function readStoredServers(): RelayServerEntry[] {
  const raw = readStored(LS_SERVERS);
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
  writeStored(LS_SERVERS, JSON.stringify(servers));
}

function readSelectedId(): string {
  return readStored(LS_SELECTED) ?? DEFAULT_SERVER_ID;
}

function writeSelectedId(id: string): void {
  writeStored(LS_SELECTED, id);
}

// Idempotent: runs only while legacy keys exist and removes them once the new
// ones are written.
function migrateLegacyKeys(): void {
  const legacyUrl = readStored(LS_LEGACY_SERVER_URL);
  const legacyCert = readStored(LS_LEGACY_CERT_HASH);
  const legacySecret = readStored(LS_LEGACY_PUBLISH_SECRET);
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
    // were saved against. Nothing to store when both are empty.
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
    // without the user noticing.
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

  // A failed removal just re-runs the migration next load, which is safe:
  // every branch above yields the same result for the same inputs.
  writeStored(LS_LEGACY_SERVER_URL, null);
  writeStored(LS_LEGACY_CERT_HASH, null);
  writeStored(LS_LEGACY_PUBLISH_SECRET, null);
}

// The default's credential record follows the URL it was saved against, not
// the id: a chart-side relayUrl change must not present the old relay's
// secret and cert hash to the new host. Discard on mismatch, at read time.
function pruneStaleDefaultCredentials(servers: RelayServerEntry[]): RelayServerEntry[] {
  const defaultUrl = normalizeRelayOrigin(defaultServerUrl());
  const record = servers.find((s) => s.id === DEFAULT_SERVER_ID);
  if (!record || record.url === defaultUrl) return servers;
  const pruned = servers.filter((s) => s.id !== DEFAULT_SERVER_ID);
  writeStoredServers(pruned);
  return pruned;
}

// The hash list a server patch sets, or undefined when it leaves it alone.
function patchHashes(patch: { certHashHex?: string; certHashes?: string[] }): string[] | undefined {
  if (patch.certHashes !== undefined) return normalizeCertHashes(patch.certHashes);
  if (patch.certHashHex !== undefined) return normalizeCertHashes(patch.certHashHex);
  return undefined;
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
  // certHashes[0] ?? '' (legacy single-hash consumers).
  certHashHex: string;
  // Every pinned hash for the resolved relay, in order.
  certHashes: string[];
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
  // The route's `?relay=` override: a normalized origin, held in memory only,
  // never persisted.
  sessionOverrideUrl: string | null;
  // The route's `?certHash=` pins (R74), held with the override and cleared
  // with it. They outrank a saved entry's hashes for the same relay: a link
  // from the relay's console is newer than a save.
  sessionOverrideCertHashes: string[];
  // Credentials entered while an unsaved override is active: session-only,
  // persisted only through an explicit "Save this server".
  overridePublishSecret: string;
  overrideCertHashHex: string;
  // Quiet note about this route's ?relay= handling (an invalid value, or a
  // link asking for a server on a gated deployment).
  // Route-scoped like the override itself; rendered by the in-session
  // indicator, never fatal.
  relayLinkNote: string | null;
  // True while this session reports diagnostics to a foreign relay's
  // advertised collector; the in-session indicator discloses it. Cleared with
  // the override on route change.
  foreignTelemetryActive: boolean;

  selectServer: (id: string) => void;
  // `certHashHex` may hold several comma-separated hashes; `certHashes`
  // wins when both are given.
  addServer: (entry: {
    label: string;
    url: string;
    publishSecret?: string;
    certHashHex?: string;
    certHashes?: string[];
  }) => string | null;
  updateServer: (
    id: string,
    patch: Partial<Pick<RelayServerEntry, 'label' | 'url' | 'publishSecret' | 'certHashHex'>> & {
      certHashes?: string[];
    },
  ) => boolean;
  removeServer: (id: string) => void;
  // Route-driven; null clears. Routing only ever passes normalized origins,
  // and hashes only beside a private-literal relay (routing.ts).
  setSessionOverride: (url: string | null, certHashes?: string[]) => void;
  setRelayLinkNote: (note: string | null) => void;
  setForeignTelemetryActive: (active: boolean) => void;
  // Across tabs: the picker re-reads storage when it opens; last writer wins.
  reloadFromStorage: () => void;

  // Used by the #/debug/* pages and the secret prompt. The credential setters
  // write to whatever entry the store currently resolves to.
  setServerUrl: (url: string) => void;
  setCertHashHex: (hash: string) => void;
  setPublishSecret: (secret: string) => void;
}

interface ResolutionInputs {
  servers: RelayServerEntry[];
  selectedServerId: string;
  sessionOverrideUrl: string | null;
  sessionOverrideCertHashes: string[];
  overridePublishSecret: string;
  overrideCertHashHex: string;
}

// The default's credentials-only record, if one is stored and still matches
// the current default URL (pruning happens at load; this re-checks so a
// same-session config change cannot resurrect stale credentials).
function defaultCredentials(servers: RelayServerEntry[]): { publishSecret: string; certHashes: string[] } {
  const defaultUrl = normalizeRelayOrigin(defaultServerUrl());
  const record = servers.find((s) => s.id === DEFAULT_SERVER_ID);
  if (record && record.url === defaultUrl) {
    return { publishSecret: record.publishSecret, certHashes: entryCertHashes(record) };
  }
  return { publishSecret: '', certHashes: [] };
}

// The configured hashes for this deployment's own relay, as a fallback: a
// hash the user typed or saved wins, and they are scoped by URL, not entry
// id, because they belong to this deployment's relay and must never be
// presented to another. config.relayCertHashes (R74, any origin) outranks
// the dev-only devCertHashHex. The picker's probe uses this too, so a row
// resolves its hashes exactly the way a real connection does.
export function certHashesWithFallback(url: string, certHashes: string[]): string[] {
  if (certHashes.length > 0) return certHashes;
  if (normalizeRelayOrigin(url) !== normalizeRelayOrigin(defaultServerUrl())) return certHashes;
  const pinned = getRelayCertHashes();
  if (pinned.length > 0) return pinned;
  const dev = normalizeCertHashes(getDevCertHashHex());
  return dev.length > 0 ? dev : certHashes;
}

function withDevCertFallback(resolved: ResolvedTransport): ResolvedTransport {
  const certHashes = certHashesWithFallback(resolved.serverUrl, resolved.certHashes);
  if (certHashes === resolved.certHashes) return resolved;
  return { ...resolved, certHashes, certHashHex: certHashes[0] ?? '' };
}

type EntryResolution = Omit<ResolvedTransport, 'certHashHex'>;

// Precedence, top wins: session override > selected entry > pinned default;
// then the dev-certificate fallback fills a hash the resolution left empty.
// Every consumer reads this one, never resolveEntry.
function resolve(inputs: ResolutionInputs): ResolvedTransport {
  const entry = resolveEntry(inputs);
  return withDevCertFallback({ ...entry, certHashHex: entry.certHashes[0] ?? '' });
}

// An override whose URL equals a saved entry (the default included) resolves
// to that entry, credentials attached.
function resolveEntry(inputs: ResolutionInputs): EntryResolution {
  const { servers, selectedServerId, sessionOverrideUrl, sessionOverrideCertHashes: linkHashes } = inputs;
  const orLink = (saved: string[]) => (linkHashes.length > 0 ? linkHashes : saved);
  const defaultUrl = defaultServerUrl();
  const normalizedDefault = normalizeRelayOrigin(defaultUrl);

  if (sessionOverrideUrl !== null) {
    if (sessionOverrideUrl === normalizedDefault) {
      const creds = defaultCredentials(servers);
      return {
        serverUrl: defaultUrl,
        publishSecret: creds.publishSecret,
        certHashes: orLink(creds.certHashes),
        resolvedSource: 'override',
        resolvedEntryId: DEFAULT_SERVER_ID,
      };
    }
    const match = servers.find((s) => s.id !== DEFAULT_SERVER_ID && s.url === sessionOverrideUrl);
    if (match) {
      return {
        serverUrl: match.url,
        certHashes: orLink(entryCertHashes(match)),
        publishSecret: match.publishSecret,
        resolvedSource: 'override',
        resolvedEntryId: match.id,
      };
    }
    return {
      serverUrl: sessionOverrideUrl,
      // A hash typed for this session (the secret prompt's sibling) wins
      // over the link's.
      certHashes:
        inputs.overrideCertHashHex !== '' ? normalizeCertHashes(inputs.overrideCertHashHex) : linkHashes,
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
        certHashes: entryCertHashes(entry),
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

// The stored selection, or the default when it names no saved entry.
function storedSelectionIn(servers: RelayServerEntry[]): string {
  const stored = readSelectedId();
  return servers.some((s) => s.id === stored) ? stored : DEFAULT_SERVER_ID;
}

function loadInitialState(): ResolutionInputs {
  migrateLegacyKeys();
  const servers = pruneStaleDefaultCredentials(readStoredServers());
  return {
    servers,
    selectedServerId: storedSelectionIn(servers),
    sessionOverrideUrl: null,
    sessionOverrideCertHashes: [],
    overridePublishSecret: '',
    overrideCertHashHex: '',
  };
}

// Custom entries as the picker lists them (the default's credentials record
// is internal storage, not a row).
export function customServers(servers: RelayServerEntry[]): RelayServerEntry[] {
  return servers.filter((s) => s.id !== DEFAULT_SERVER_ID);
}

// True when the RESOLVED server names the deployment's own relay — by
// normalized-URL equality, not entry id: a user who manually
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
  // The resolved hash list keeps its identity while its contents do not
  // change, so a component selecting it (or a dial effect depending on it)
  // does not see a new array on every unrelated write.
  const apply = (inputs: ResolutionInputs) => {
    const resolved = resolve(inputs);
    const prev = get().certHashes;
    if (prev !== undefined && sameHashes(prev, resolved.certHashes)) resolved.certHashes = prev;
    set({ ...inputs, ...resolved });
  };

  const inputsOf = (s: TransportSettingsState): ResolutionInputs => ({
    servers: s.servers,
    selectedServerId: s.selectedServerId,
    sessionOverrideUrl: s.sessionOverrideUrl,
    sessionOverrideCertHashes: s.sessionOverrideCertHashes,
    overridePublishSecret: s.overridePublishSecret,
    overrideCertHashHex: s.overrideCertHashHex,
  });

  // Upsert the default's credentials-only record (how its secret is rotated).
  const writeDefaultCredentials = (
    servers: RelayServerEntry[],
    patch: { publishSecret?: string; certHashes?: string[] },
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
      ...hashFields(patch.certHashes ?? current.certHashes),
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

  // Credential writes land on whatever the store currently resolves to: the
  // default's record, a custom entry, or — for an unsaved
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
    const entryPatch: { publishSecret?: string; certHashes?: string[] } = {};
    if (patch.publishSecret !== undefined) entryPatch.publishSecret = patch.publishSecret;
    if (patch.certHashHex !== undefined) entryPatch.certHashes = normalizeCertHashes(patch.certHashHex);
    if (resolved.resolvedEntryId === DEFAULT_SERVER_ID) {
      apply({ ...inputsOf(s), servers: writeDefaultCredentials(s.servers, entryPatch) });
      return;
    }
    const servers = s.servers.map((e) =>
      e.id === resolved.resolvedEntryId
        ? {
            ...e,
            ...(entryPatch.publishSecret !== undefined ? { publishSecret: entryPatch.publishSecret } : {}),
            ...(entryPatch.certHashes !== undefined ? hashFields(entryPatch.certHashes) : {}),
          }
        : e,
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
          ...hashFields(normalizeCertHashes(entry.certHashes ?? entry.certHashHex ?? '')),
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
        // are editable.
        const credPatch: { publishSecret?: string; certHashes?: string[] } = {};
        if (patch.publishSecret !== undefined) credPatch.publishSecret = patch.publishSecret;
        const hashes = patchHashes(patch);
        if (hashes !== undefined) credPatch.certHashes = hashes;
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
      const hashes = patchHashes(patch);
      const servers = s.servers.map((e) =>
        e.id === id
          ? {
              ...e,
              label: patch.label !== undefined ? patch.label.trim() || e.label : e.label,
              url,
              publishSecret: patch.publishSecret ?? e.publishSecret,
              ...(hashes !== undefined ? hashFields(hashes) : {}),
            }
          : e,
      );
      writeStoredServers(servers);
      apply({ ...inputsOf(s), servers });
      return true;
    },

    removeServer: (id) => {
      if (id === DEFAULT_SERVER_ID) return; // pinned
      const s = get();
      const servers = s.servers.filter((e) => e.id !== id);
      if (servers.length === s.servers.length) return;
      writeStoredServers(servers);
      const selectedServerId = s.selectedServerId === id ? DEFAULT_SERVER_ID : s.selectedServerId;
      if (selectedServerId !== s.selectedServerId) writeSelectedId(selectedServerId);
      apply({ ...inputsOf(s), servers, selectedServerId });
    },

    setSessionOverride: (url, certHashes = []) => {
      const s = get();
      const normalized = url === null ? null : normalizeRelayOrigin(url);
      if (url !== null && normalized === null) return; // routing pre-validates; belt and braces
      const hashes = normalized === null ? [] : normalizeCertHashes(certHashes);
      if (normalized === s.sessionOverrideUrl && sameHashes(hashes, s.sessionOverrideCertHashes)) return;
      apply({
        ...inputsOf(s),
        sessionOverrideUrl: normalized,
        sessionOverrideCertHashes: hashes,
        // A new (or cleared) override never inherits the previous override's
        // session-typed credentials.
        overridePublishSecret: '',
        overrideCertHashHex: '',
      });
    },

    reloadFromStorage: () => {
      const s = get();
      const servers = pruneStaleDefaultCredentials(readStoredServers());
      apply({ ...inputsOf(s), servers, selectedServerId: storedSelectionIn(servers) });
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
          { id, label: 'Manual (dev)', url: normalized, publishSecret: '', ...hashFields([]) },
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
