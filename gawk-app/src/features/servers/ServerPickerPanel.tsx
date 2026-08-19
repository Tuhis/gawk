// R37 (docs/40 §4.3): the server picker panel. The saved-server list with
// the pinned default first (identity locked, credentials editable — F4),
// add/edit/remove for custom entries, select-on-click, and the
// "save this server" affordance for an unsaved link override (D2). The
// dev-cert-hash field is dev-gated exactly like the old panels; everything
// else is a production surface gated only by allowCustomRelays (D6) at the
// call sites that open this panel.

import { useCallback, useEffect, useState } from 'react';
import { createPortal } from 'react-dom';

import styles from './servers.module.css';
import { Button } from '../../ui/Button';
import { GlassPanel } from '../../ui/GlassPanel';
import { PlusIcon } from '../../ui/Icons';
import { getServerDirectoryUrl, isDevEnvironment } from '../../config';
import {
  DEFAULT_SERVER_ID,
  certHashWithDevFallback,
  defaultServerUrl,
  useTransportStore,
  type RelayServerEntry,
} from '../../state/transportStore';
import { fetchServerDirectory, type DirectoryOffer } from './directory';
import { sanitizeIdentityName } from './probe';
import { useServerProbe, type ProbeFn, type RowProbeState } from './useServerProbe';

interface Props {
  onClose: () => void;
  // Injectable for tests (jsdom has no WebTransport).
  probeFn?: ProbeFn;
  fetchFn?: typeof fetch;
}

// Latency buckets for the row dot. Deliberately coarse — the dot answers
// "can I play on this?" at a glance and the millisecond reading beside it
// carries the precision.
type ProbeQuality = 'good' | 'fair' | 'poor' | 'pending';

const FAIR_RTT_MS = 100;
const POOR_RTT_MS = 250;

function qualityForRtt(rttMs: number): ProbeQuality {
  if (rttMs < FAIR_RTT_MS) return 'good';
  if (rttMs < POOR_RTT_MS) return 'fair';
  return 'poor';
}

// Decorative: every dot sits beside text that already states the same thing,
// so it is aria-hidden rather than a second, redundant announcement.
function ProbeDot({ quality }: { quality: ProbeQuality }) {
  return (
    <span
      className={styles.probeDot}
      data-quality={quality}
      data-testid="probe-dot"
      aria-hidden="true"
    />
  );
}

// One probe cell (docs/40 §4.4): RTT + sanitized identity next to — never in
// place of — the host the row already shows; one honest combined failure
// state (browsers blur the causes).
function ProbeCell({ probe }: { probe: RowProbeState | undefined }) {
  if (!probe || probe.state === 'idle') return null;
  if (probe.state === 'probing') {
    return (
      <span className={styles.rowProbe}>
        <ProbeDot quality="pending" />…
      </span>
    );
  }
  if (probe.state === 'failed') {
    return (
      <span
        className={`${styles.rowProbe} ${styles.rowProbeBad}`}
        title="Unreachable — or not a gawk server, a certificate problem, or a server that does not allow this app"
      >
        <ProbeDot quality="poor" />
        unreachable
      </span>
    );
  }
  const name = probe.identity ? sanitizeIdentityName(probe.identity.name) : '';
  const detail = probe.identity
    ? ` · ${name !== '' ? `${name} · ` : ''}gawk-server ${probe.identity.serverVersion}`
    : '';
  return (
    <span className={styles.rowProbe}>
      <ProbeDot quality={qualityForRtt(probe.rttMs)} />
      {Math.round(probe.rttMs)} ms{detail}
    </span>
  );
}

function hostOf(url: string): string {
  try {
    return new URL(url).host;
  } catch {
    return url;
  }
}

type Editing =
  | { mode: 'closed' }
  | { mode: 'add' }
  | { mode: 'edit'; id: string }
  | { mode: 'default-credentials' };

interface Draft {
  label: string;
  url: string;
  publishSecret: string;
  certHashHex: string;
}

const EMPTY_DRAFT: Draft = { label: '', url: '', publishSecret: '', certHashHex: '' };

// Must match the .scrimOut / .panelOut animation durations in the stylesheet.
// A timer rather than an animationend listener: it still fires when the
// browser skips the animation entirely.
const CLOSE_ANIM_MS = 160;

function prefersReducedMotion(): boolean {
  if (typeof window === 'undefined') return false;
  return window.matchMedia?.('(prefers-reduced-motion: reduce)').matches === true;
}

export function ServerPickerPanel({ onClose, probeFn, fetchFn }: Props) {
  const servers = useTransportStore((s) => s.servers);
  const selectedServerId = useTransportStore((s) => s.selectedServerId);
  const sessionOverrideUrl = useTransportStore((s) => s.sessionOverrideUrl);
  // Directory (docs/40 §4.5): fetched when the panel opens, never at boot;
  // failure degrades to a quiet note. undefined = still loading.
  const [directory, setDirectory] = useState<DirectoryOffer[] | null | undefined>(undefined);

  const [editing, setEditing] = useState<Editing>({ mode: 'closed' });
  const [draft, setDraft] = useState<Draft>(EMPTY_DRAFT);
  const [formError, setFormError] = useState<string | null>(null);
  // Credentials live behind a disclosure: adding a server is a URL and a
  // name, and neither credential applies to the common case (a viewer joining
  // a relay that needs no secret). Opened for you when the entry you are
  // editing already carries one, so Edit never hides what you came for.
  const [advancedOpen, setAdvancedOpen] = useState(false);
  // Dismissal is deferred by one animation: every exit runs through
  // requestClose, and onClose fires once the panel has finished leaving.
  const [closing, setClosing] = useState(false);
  const [savedOverride, setSavedOverride] = useState(false);

  const showDev = isDevEnvironment();
  const custom = servers.filter((s) => s.id !== DEFAULT_SERVER_ID);
  const defaultUrl = defaultServerUrl();
  const overrideIsUnsaved =
    sessionOverrideUrl !== null &&
    sessionOverrideUrl !== defaultUrl &&
    !custom.some((s) => s.url === sessionOverrideUrl) &&
    !savedOverride;

  const requestClose = useCallback(() => {
    if (prefersReducedMotion()) {
      onClose();
      return;
    }
    setClosing(true);
  }, [onClose]);

  useEffect(() => {
    if (!closing) return;
    const timer = window.setTimeout(onClose, CLOSE_ANIM_MS);
    return () => window.clearTimeout(timer);
  }, [closing, onClose]);

  // Portal host. <body> normally, but the Fullscreen API renders only the
  // fullscreen element's own subtree, and the viewer goes fullscreen on its
  // root (`lib/useFullscreen.ts`) — a <body> child would not be painted there.
  const [portalHost, setPortalHost] = useState<Element>(
    () => document.fullscreenElement ?? document.body,
  );
  useEffect(() => {
    const sync = () => setPortalHost(document.fullscreenElement ?? document.body);
    sync();
    document.addEventListener('fullscreenchange', sync);
    return () => document.removeEventListener('fullscreenchange', sync);
  }, []);

  // Modal dismissal: Escape, alongside the backdrop click below.
  useEffect(() => {
    const onKeyDown = (e: KeyboardEvent) => {
      if (e.key === 'Escape') requestClose();
    };
    document.addEventListener('keydown', onKeyDown);
    return () => document.removeEventListener('keydown', onKeyDown);
  }, [requestClose]);

  // Cross-tab rule (F11): re-read storage when the panel opens.
  useEffect(() => {
    useTransportStore.getState().reloadFromStorage();
  }, []);

  const directoryUrl = getServerDirectoryUrl();
  useEffect(() => {
    if (directoryUrl === '') {
      setDirectory(null);
      return;
    }
    let live = true;
    void fetchServerDirectory(directoryUrl, fetchFn).then((offers) => {
      if (live) setDirectory(offers);
    });
    return () => {
      live = false;
    };
  }, [directoryUrl, fetchFn]);

  // Saved servers probe on open + demand; directory offers on demand ONLY
  // (F10 — the probe discloses the user's address to the probed host).
  const { results: probeResults, probe } = useServerProbe(
    [
      // Every row's hash goes through the same fallback a real connection
      // uses, so a probe can never report a relay the viewer is streaming
      // from as unreachable (R38, transportStore.certHashWithDevFallback).
      {
        key: DEFAULT_SERVER_ID,
        url: defaultServerUrl(),
        certHashHex: certHashWithDevFallback(defaultServerUrl(), defaultCreds().certHashHex),
        auto: true,
      },
      ...customServersOf(servers).map((e) => ({
        key: e.id,
        url: e.url,
        certHashHex: certHashWithDevFallback(e.url, e.certHashHex),
        auto: true,
      })),
      ...(Array.isArray(directory) ? directory : []).map((o, i) => ({
        key: `dir-${i}-${o.url}`,
        url: o.url,
        certHashHex: certHashWithDevFallback(o.url, ''),
        auto: false,
      })),
    ],
    probeFn,
  );

  function defaultCreds(): { certHashHex: string } {
    const record = servers.find((e) => e.id === DEFAULT_SERVER_ID);
    return { certHashHex: record?.certHashHex ?? '' };
  }
  function customServersOf(list: RelayServerEntry[]): RelayServerEntry[] {
    return list.filter((e) => e.id !== DEFAULT_SERVER_ID);
  }

  // Selecting is the panel's whole purpose, so it is also the exit (on a
  // session screen the reconnect happens behind the closing panel).
  const selectAndClose = (id: string) => {
    useTransportStore.getState().selectServer(id);
    requestClose();
  };

  const openAdd = () => {
    setDraft(EMPTY_DRAFT);
    setFormError(null);
    setAdvancedOpen(false);
    setEditing({ mode: 'add' });
  };

  const openEdit = (entry: RelayServerEntry) => {
    setDraft({
      label: entry.label,
      url: entry.url,
      publishSecret: entry.publishSecret,
      certHashHex: entry.certHashHex,
    });
    setFormError(null);
    setAdvancedOpen(entry.publishSecret !== '' || entry.certHashHex !== '');
    setEditing({ mode: 'edit', id: entry.id });
  };

  const openDefaultCredentials = () => {
    const store = useTransportStore.getState();
    const record = store.servers.find((s) => s.id === DEFAULT_SERVER_ID);
    setDraft({
      label: '',
      url: defaultUrl,
      publishSecret: record?.publishSecret ?? '',
      certHashHex: record?.certHashHex ?? '',
    });
    setFormError(null);
    setEditing({ mode: 'default-credentials' });
  };

  const submitForm = () => {
    const store = useTransportStore.getState();
    if (editing.mode === 'add') {
      const id = store.addServer({
        label: draft.label,
        url: draft.url,
        publishSecret: draft.publishSecret,
        certHashHex: draft.certHashHex,
      });
      if (id === null) {
        setFormError('Enter the server as an https address, e.g. https://relay.example.com:4433');
        return;
      }
    } else if (editing.mode === 'edit') {
      const ok = store.updateServer(editing.id, draft);
      if (!ok) {
        setFormError('Enter the server as an https address, e.g. https://relay.example.com:4433');
        return;
      }
    } else if (editing.mode === 'default-credentials') {
      store.updateServer(DEFAULT_SERVER_ID, {
        publishSecret: draft.publishSecret,
        certHashHex: draft.certHashHex,
      });
    }
    setEditing({ mode: 'closed' });
  };

  const saveOverride = () => {
    const store = useTransportStore.getState();
    if (store.sessionOverrideUrl === null) return;
    // Saving carries any session-typed credentials into the entry (F3) but
    // does NOT change the selection — selection is its own click (D2).
    store.addServer({
      label: '',
      url: store.sessionOverrideUrl,
      publishSecret: store.overridePublishSecret,
      certHashHex: store.overrideCertHashHex,
    });
    setSavedOverride(true);
  };

  const form = editing.mode !== 'closed' && (
    <form
      className={styles.form}
      onSubmit={(e) => {
        e.preventDefault();
        submitForm();
      }}
    >
      {editing.mode !== 'default-credentials' && (
        <>
          <label className={styles.field}>
            <span>Name</span>
            <input
              value={draft.label}
              onChange={(e) => setDraft((d) => ({ ...d, label: e.target.value }))}
              placeholder="My homelab"
              spellCheck={false}
            />
          </label>
          <label className={styles.field}>
            <span>Server URL</span>
            <input
              value={draft.url}
              onChange={(e) => setDraft((d) => ({ ...d, url: e.target.value }))}
              placeholder="https://relay.example.com:4433"
              spellCheck={false}
              autoCapitalize="off"
              autoCorrect="off"
            />
          </label>
        </>
      )}
      {/* The default's credential form IS these fields — a disclosure there
          would hide the entire reason the form opened. */}
      {editing.mode !== 'default-credentials' && (
        <button
          type="button"
          className={styles.disclosure}
          aria-expanded={advancedOpen}
          onClick={() => setAdvancedOpen((o) => !o)}
        >
          <span>Advanced</span>
          <span aria-hidden="true">{advancedOpen ? '⌃' : '⌄'}</span>
        </button>
      )}
      {(advancedOpen || editing.mode === 'default-credentials') && (
        <>
          <label className={styles.field}>
            <span>Publish secret (only needed to broadcast)</span>
            <input
              type="password"
              value={draft.publishSecret}
              onChange={(e) => setDraft((d) => ({ ...d, publishSecret: e.target.value }))}
              placeholder="leave empty if none"
              autoComplete="off"
              spellCheck={false}
            />
          </label>
          {showDev && (
            <label className={styles.field}>
              <span>Dev cert hash (hex; empty for a real cert)</span>
              <input
                value={draft.certHashHex}
                onChange={(e) => setDraft((d) => ({ ...d, certHashHex: e.target.value }))}
                placeholder="cert_hash_hex from the server startup log"
                spellCheck={false}
              />
            </label>
          )}
        </>
      )}
      {formError && <p className={styles.formError}>{formError}</p>}
      <div className={styles.formActions}>
        <Button type="button" variant="secondary" onClick={() => setEditing({ mode: 'closed' })}>
          Cancel
        </Button>
        <Button type="submit">Save</Button>
      </div>
    </form>
  );

  // Portalled on purpose: the landing chip lives in a `transform`ed row, and
  // a transformed ancestor becomes the containing block for `position: fixed`
  // descendants — mounted in place, the full-screen overlay collapsed to the
  // chip's own ~78px box.
  return createPortal(
    <div
      className={`${styles.scrim} ${closing ? styles.scrimOut : ''}`}
      data-testid="server-picker-backdrop"
      data-closing={closing ? 'true' : undefined}
      // Backdrop click dismisses; clicks that bubble up from the panel do not.
      onClick={(e) => {
        if (e.target === e.currentTarget) requestClose();
      }}
    >
      <GlassPanel
        className={`${styles.panel} ${closing ? styles.panelOut : ''}`}
        role="dialog"
        aria-modal="true"
        aria-label="Server picker"
      >
        <div className={styles.panelHead} data-testid="server-picker-head">
          <span>Servers</span>
        </div>

          {overrideIsUnsaved && (
            <div className={styles.form}>
              <p className={styles.note}>
                This session is using <strong>{hostOf(sessionOverrideUrl!)}</strong> from the link
                you opened. Save it to pick it again later.
              </p>
              <div className={styles.formActions}>
                <Button onClick={saveOverride}>Save this server</Button>
              </div>
            </div>
          )}

          <div className={styles.list} role="listbox" aria-label="Saved servers">
            <button
              type="button"
              className={`${styles.row} ${selectedServerId === DEFAULT_SERVER_ID ? styles.rowSelected : ''}`}
              role="option"
              aria-selected={selectedServerId === DEFAULT_SERVER_ID}
              onClick={() => selectAndClose(DEFAULT_SERVER_ID)}
            >
              <span className={styles.rowMain}>
                <span className={styles.rowLabel}>This deployment</span>
                <span className={styles.rowHost}>{hostOf(defaultUrl)}</span>
              </span>
              <ProbeCell probe={probeResults[DEFAULT_SERVER_ID]} />
              <span className={styles.rowActions}>
                <span
                  className={styles.rowActionBtn}
                  role="button"
                  tabIndex={0}
                  aria-label="Edit default server credentials"
                  onClick={(e) => {
                    e.stopPropagation();
                    openDefaultCredentials();
                  }}
                  onKeyDown={(e) => {
                    if (e.key === 'Enter' || e.key === ' ') {
                      e.stopPropagation();
                      openDefaultCredentials();
                    }
                  }}
                >
                  Credentials
                </span>
              </span>
            </button>

            {custom.map((entry) => (
              <button
                key={entry.id}
                type="button"
                className={`${styles.row} ${selectedServerId === entry.id ? styles.rowSelected : ''}`}
                role="option"
                aria-selected={selectedServerId === entry.id}
                onClick={() => selectAndClose(entry.id)}
              >
                <span className={styles.rowMain}>
                  <span className={styles.rowLabel}>{entry.label || hostOf(entry.url)}</span>
                  <span className={styles.rowHost}>{hostOf(entry.url)}</span>
                </span>
                <ProbeCell probe={probeResults[entry.id]} />
                <span className={styles.rowActions}>
                  <span
                    className={styles.rowActionBtn}
                    role="button"
                    tabIndex={0}
                    aria-label={`Edit ${entry.label || hostOf(entry.url)}`}
                    onClick={(e) => {
                      e.stopPropagation();
                      openEdit(entry);
                    }}
                    onKeyDown={(e) => {
                      if (e.key === 'Enter' || e.key === ' ') {
                        e.stopPropagation();
                        openEdit(entry);
                      }
                    }}
                  >
                    Edit
                  </span>
                  <span
                    className={styles.rowActionBtn}
                    role="button"
                    tabIndex={0}
                    aria-label={`Remove ${entry.label || hostOf(entry.url)}`}
                    onClick={(e) => {
                      e.stopPropagation();
                      useTransportStore.getState().removeServer(entry.id);
                    }}
                    onKeyDown={(e) => {
                      if (e.key === 'Enter' || e.key === ' ') {
                        e.stopPropagation();
                        useTransportStore.getState().removeServer(entry.id);
                      }
                    }}
                  >
                    Remove
                  </span>
                </span>
              </button>
            ))}
          </div>

          {directoryUrl !== '' && (
            <>
              <h3 className={styles.sectionTitle}>Directory</h3>
              {directory === undefined && <p className={styles.note}>Loading directory…</p>}
              {directory === null && <p className={styles.note}>Directory unavailable.</p>}
              {Array.isArray(directory) && directory.length === 0 && (
                <p className={styles.note}>The directory lists no servers.</p>
              )}
              {Array.isArray(directory) && directory.length > 0 && (
                <div className={styles.list}>
                  {directory.map((offer, i) => {
                    const key = `dir-${i}-${offer.url}`;
                    const alreadySaved = custom.some((e) => e.url === offer.url);
                    return (
                      <div key={key} className={styles.row} role="listitem">
                        <span className={styles.rowMain}>
                          <span className={styles.rowLabel}>
                            {offer.label}
                            {offer.managed ? ' · managed' : ''}
                          </span>
                          <span className={styles.rowHost}>{hostOf(offer.url)}</span>
                        </span>
                        <ProbeCell probe={probeResults[key]} />
                        <span className={styles.rowActions}>
                          <button
                            type="button"
                            className={styles.rowActionBtn}
                            aria-label={`Ping ${offer.label}`}
                            onClick={() => probe(key)}
                          >
                            Ping
                          </button>
                          <button
                            type="button"
                            className={styles.rowActionBtn}
                            aria-label={`Add ${offer.label}`}
                            disabled={alreadySaved}
                            onClick={() =>
                              useTransportStore.getState().addServer({ label: offer.label, url: offer.url })
                            }
                          >
                            {alreadySaved ? 'Added' : 'Add'}
                          </button>
                        </span>
                      </div>
                    );
                  })}
                </div>
              )}
            </>
          )}

          {editing.mode !== 'closed' && form}

          {/* Add on the left, dismiss on the right — the two ends of one
              footer, so neither control moves as the list grows. */}
          <div className={styles.panelFoot} data-testid="server-picker-foot">
            {editing.mode === 'closed' ? (
              <Button variant="secondary" onClick={openAdd}>
                <PlusIcon />
                Add a server
              </Button>
            ) : (
              <span />
            )}
            <Button variant="ghost" onClick={requestClose}>
              Done
            </Button>
          </div>
      </GlassPanel>
    </div>,
    portalHost,
  );
}
