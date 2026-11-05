import { useState } from 'react';

import { createAnnotation, deleteAnnotation } from '../api/client.ts';
import type { Annotation } from '../api/types.ts';
import { absoluteTime, compactTime } from '../lib/format.ts';
import { useMetaStore } from '../state/metaStore.ts';
import { usePinStore } from '../state/pinStore.ts';
import styles from './Annotations.module.css';
import view from '../views/view.module.css';

// TH8 / UD16: the ONE write path.
//
// A note pinned to a session, a broadcast, or a moment on a timeline —
// "switched to WiFi here", "this is the R30 regression". Three properties are
// worth knowing while reading this component:
//
//   * **Permanent.** An annotation outliving the samples it describes is the
//     normal case and the point. It is stored beside rollups, which the prune
//     loop does not walk.
//   * **Never mixed into a session file.** A raw partition stays exactly what a
//     client sent.
//   * **Free text the operator typed**, on the operator's own PVC. R28's
//     zero-PII rule governs COLLECTED data and is untouched by this.
//
// The affordance is hidden entirely where the deployment has no store, rather
// than offered and then 501-ing.

interface Props {
  sessionId?: string;
  broadcastKey?: string;
  /** Default moment for a new note — usually where the operator clicked. */
  atMs?: number;
  notes: Annotation[];
  onChange: () => void;
  /** Lets a page's "add note" button focus the box. */
  inputId?: string;
  /** Pin to the page's crosshair (where it last was) instead of `atMs`. */
  followPin?: boolean;
}

export function AnnotationPanel({ sessionId, broadcastKey, atMs: fallbackAt, notes, onChange, inputId, followPin }: Props) {
  const pinned = usePinStore((s) => s.atMs);
  const atMs = followPin && pinned !== null ? pinned : fallbackAt;
  const enabled = useMetaStore((s) => s.meta?.annotations ?? false);
  const [text, setText] = useState('');
  const [pinAt, setPinAt] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  if (!enabled) return null;

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!text.trim()) return;
    setBusy(true);
    setError(null);
    try {
      await createAnnotation({
        sessionId,
        broadcastKey,
        atMs: pinAt ? atMs : undefined,
        text: text.trim(),
      });
      setText('');
      onChange();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <section className={view.panel} aria-label="Notes">
      <div className={view.panelH}>
        <h2 className={view.panelT}>Notes</h2>
        <span className={view.note}>operator prose, kept permanently beside the rollups</span>
      </div>
      <div className={`${view.panelB} ${styles.body}`}>
        {notes.length === 0 ? (
          <p className={view.note}>No notes yet.</p>
        ) : (
          <ul className={styles.list}>
            {notes.map((n) => (
              <li key={n.id} className={styles.note}>
                <span className={styles.noteWhen} title={absoluteTime(n.atMs ?? n.createdAtMs)}>
                  {compactTime(n.atMs ?? n.createdAtMs)}
                  {n.atMs ? '' : ' (written)'}
                </span>
                <span className={styles.noteText}>{n.text}</span>
                <button
                  type="button"
                  className={styles.delete}
                  title="Delete this note"
                  aria-label="Delete this note"
                  onClick={() => {
                    void deleteAnnotation(n.id).then(onChange, (e: unknown) =>
                      setError(e instanceof Error ? e.message : String(e)),
                    );
                  }}
                >
                  ×
                </button>
              </li>
            ))}
          </ul>
        )}
        <form className={styles.form} onSubmit={submit}>
          <input
            id={inputId}
            className={`${view.text} ${styles.input}`}
            value={text}
            placeholder="What happened here…"
            aria-label="Note text"
            maxLength={4096}
            onChange={(e) => setText(e.target.value)}
          />
          {atMs !== undefined && (
            <label className={view.control} title="Pin to this moment, so it marks every chart covering it">
              <input type="checkbox" checked={pinAt} onChange={(e) => setPinAt(e.target.checked)} />
              pin to {compactTime(atMs)}
            </label>
          )}
          <button type="submit" className={view.button} disabled={busy || !text.trim()}>
            add
          </button>
        </form>
        {error && <p className={view.error}>{error}</p>}
      </div>
    </section>
  );
}
