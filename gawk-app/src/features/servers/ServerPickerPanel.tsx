// R37 (docs/40 §4.3): the server picker panel. The saved-server list with
// the pinned default first (identity locked, credentials editable — F4),
// add/edit/remove for custom entries, select-on-click, and the
// "save this server" affordance for an unsaved link override (D2). The
// dev-cert-hash field is dev-gated exactly like the old panels; everything
// else is a production surface gated only by allowCustomRelays (D6) at the
// call sites that open this panel.

import { useEffect, useState } from 'react';

import styles from './servers.module.css';
import { Button } from '../../ui/Button';
import { GlassPanel } from '../../ui/GlassPanel';
import { isDevEnvironment } from '../../config';
import {
  DEFAULT_SERVER_ID,
  defaultServerUrl,
  useTransportStore,
  type RelayServerEntry,
} from '../../state/transportStore';

interface Props {
  onClose: () => void;
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

export function ServerPickerPanel({ onClose }: Props) {
  const servers = useTransportStore((s) => s.servers);
  const selectedServerId = useTransportStore((s) => s.selectedServerId);
  const sessionOverrideUrl = useTransportStore((s) => s.sessionOverrideUrl);

  const [editing, setEditing] = useState<Editing>({ mode: 'closed' });
  const [draft, setDraft] = useState<Draft>(EMPTY_DRAFT);
  const [formError, setFormError] = useState<string | null>(null);
  const [savedOverride, setSavedOverride] = useState(false);

  const showDev = isDevEnvironment();
  const custom = servers.filter((s) => s.id !== DEFAULT_SERVER_ID);
  const defaultUrl = defaultServerUrl();
  const overrideIsUnsaved =
    sessionOverrideUrl !== null &&
    sessionOverrideUrl !== defaultUrl &&
    !custom.some((s) => s.url === sessionOverrideUrl) &&
    !savedOverride;

  // Cross-tab rule (F11): re-read storage when the panel opens.
  useEffect(() => {
    useTransportStore.getState().reloadFromStorage();
  }, []);

  const openAdd = () => {
    setDraft(EMPTY_DRAFT);
    setFormError(null);
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
      {formError && <p className={styles.formError}>{formError}</p>}
      <div className={styles.formActions}>
        <Button type="button" variant="secondary" onClick={() => setEditing({ mode: 'closed' })}>
          Cancel
        </Button>
        <Button type="submit">Save</Button>
      </div>
    </form>
  );

  return (
    <>
      <div className={styles.scrim} onClick={onClose} />
      <div className={styles.panelCenter}>
        <GlassPanel className={styles.panel} role="dialog" aria-label="Server picker">
          <div className={styles.panelHead}>
            <span>Servers</span>
            <Button variant="ghost" onClick={onClose}>
              Done
            </Button>
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
              onClick={() => useTransportStore.getState().selectServer(DEFAULT_SERVER_ID)}
            >
              <span className={styles.rowMain}>
                <span className={styles.rowLabel}>This deployment</span>
                <span className={styles.rowHost}>{hostOf(defaultUrl)}</span>
              </span>
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
                onClick={() => useTransportStore.getState().selectServer(entry.id)}
              >
                <span className={styles.rowMain}>
                  <span className={styles.rowLabel}>{entry.label || hostOf(entry.url)}</span>
                  <span className={styles.rowHost}>{hostOf(entry.url)}</span>
                </span>
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

          {editing.mode === 'closed' ? (
            <div className={styles.formActions}>
              <Button variant="secondary" onClick={openAdd}>
                Add a server
              </Button>
            </div>
          ) : (
            form
          )}
        </GlassPanel>
      </div>
    </>
  );
}
