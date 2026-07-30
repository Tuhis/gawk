import { useEffect, useMemo, useRef, useState } from 'react';

import { resolveCode } from '../api/client.ts';
import { href, isBroadcastKey, isSessionId, navigate } from '../router/router.ts';
import { useMetaStore } from '../state/metaStore.ts';
import { useLiveStore } from '../state/liveStore.ts';
import styles from './CommandPalette.module.css';

// TH11's command palette: ⌘K reaches every view and every addressable object.
//
// It is the keyboard path through everything TH1 made addressable, and it is
// the reason TH1 comes first: a palette over a page with no routes could only
// offer scrolling.
//
// What it accepts, in the order a human would try them: a view name, a session
// id, a broadcast key, a join code, or a field name. The code path goes through
// `POST /v1/resolve` — the code is a join credential and must never travel in a
// query string or land in browser history.

interface Item {
  label: string;
  hint: string;
  go: () => void | Promise<void>;
}

export function CommandPalette() {
  const [open, setOpen] = useState(false);
  const [q, setQ] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const inputRef = useRef<HTMLInputElement | null>(null);
  const fields = useMetaStore((s) => s.fields);
  const resolveEnabled = useMetaStore((s) => s.meta?.resolve ?? false);
  const snapshot = useLiveStore((s) => s.snapshot);

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') {
        e.preventDefault();
        setOpen((v) => !v);
        return;
      }
      if (e.key === 'Escape') setOpen(false);
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, []);

  useEffect(() => {
    if (open) {
      setError(null);
      inputRef.current?.focus();
      inputRef.current?.select();
    }
  }, [open]);

  const items = useMemo<Item[]>(() => {
    const term = q.trim();
    const lower = term.toLowerCase();
    const out: Item[] = [];

    if (isSessionId(term)) {
      out.push({
        label: `Session ${term}`,
        hint: 'open the session detail',
        go: () => navigate(href('session', term)),
      });
    }
    if (isBroadcastKey(term)) {
      out.push({
        label: `Broadcast ${term}`,
        hint: 'open the multi-lane timeline',
        go: () => navigate(href('broadcast', term)),
      });
    }
    // A join code is 6 characters of the relay's alphabet. Resolving it is a
    // server round-trip, so it is offered as an action rather than run on every
    // keystroke.
    if (resolveEnabled && /^[0-9A-Za-z]{4,8}$/.test(term) && !isBroadcastKey(term)) {
      out.push({
        label: `Find stream “${term.toUpperCase()}”`,
        hint: 'resolve the join code to its obfuscated key',
        go: async () => {
          setBusy(true);
          setError(null);
          try {
            const key = await resolveCode(term.toUpperCase());
            if (!key) {
              setError('this deployment cannot resolve codes');
              return;
            }
            navigate(href('broadcast', key));
            setOpen(false);
          } catch {
            setError('no broadcast matches that code');
          } finally {
            setBusy(false);
          }
        },
      });
    }

    for (const v of ['live', 'history', 'fleet', 'explore', 'rules', 'sql'] as const) {
      if (!lower || v.startsWith(lower)) {
        out.push({ label: v, hint: 'go to section', go: () => navigate(href(v)) });
      }
    }

    // Live broadcasts by key prefix, so the thing on screen is reachable
    // without typing twelve hex characters.
    for (const b of snapshot?.live ?? []) {
      if (lower && !b.broadcastKey.startsWith(lower)) continue;
      out.push({
        label: `live ${b.broadcastKey}`,
        hint: `${b.viewers} viewer(s)`,
        go: () => navigate(href('broadcast', b.broadcastKey)),
      });
    }

    if (lower.length >= 2) {
      for (const f of fields) {
        if (!f.name.toLowerCase().includes(lower)) continue;
        out.push({
          label: f.name,
          hint: `plot this ${f.semantic}`,
          go: () => navigate(href('explore', undefined, { fields: f.name })),
        });
        if (out.length > 24) break;
      }
    }
    return out.slice(0, 25);
  }, [q, fields, snapshot, resolveEnabled]);

  if (!open) return null;

  return (
    <div
      className={styles.backdrop}
      role="dialog"
      aria-modal="true"
      aria-label="Command palette"
      onClick={(e) => {
        if (e.target === e.currentTarget) setOpen(false);
      }}
    >
      <div className={styles.panel}>
        <input
          ref={inputRef}
          className={styles.input}
          value={q}
          placeholder="session id, broadcast key, code, field, or a section"
          onChange={(e) => setQ(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter' && items[0]) {
              void items[0].go();
              if (!/^[0-9A-Za-z]{4,8}$/.test(q.trim()) || isBroadcastKey(q.trim())) setOpen(false);
            }
          }}
        />
        {error && <p className={styles.error}>{error}</p>}
        <ul className={styles.list}>
          {items.map((it, i) => (
            <li key={`${it.label}-${i}`}>
              <button
                type="button"
                className={styles.item}
                disabled={busy}
                onClick={() => {
                  void it.go();
                  setOpen(false);
                }}
              >
                <span className={styles.itemLabel}>{it.label}</span>
                <span className={styles.itemHint}>{it.hint}</span>
              </button>
            </li>
          ))}
          {items.length === 0 && <li className={styles.none}>Nothing matches.</li>}
        </ul>
      </div>
    </div>
  );
}
