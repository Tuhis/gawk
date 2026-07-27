import { useEffect, useState } from 'react';

import { probeResolve, resolveCode } from '../api/client.ts';
import { useLiveStore } from '../state/liveStore.ts';
import { useUiStore } from '../state/uiStore.ts';
import styles from './FindStream.module.css';

/**
 * Find a stream by the six-character code you already hold.
 *
 * Rows are labelled with the OBFUSCATED key, because that is the only identity
 * telemetry is ever told — the code is a join credential and the client is
 * structurally incapable of reporting one. So the mapping runs server-side and
 * one way, and this component only ever holds the resulting digest.
 *
 * The box does not render unless the backend actually offers the lookup, so it
 * never presents an action that cannot work. That also makes it degrade cleanly
 * against a deployment predating the endpoint, which answers 404.
 */
export function FindStream() {
  const [available, setAvailable] = useState(false);
  const [code, setCode] = useState('');
  const [message, setMessage] = useState('');
  const foundKey = useUiStore((s) => s.foundKey);
  const setFoundKey = useUiStore((s) => s.setFoundKey);
  const snapshot = useLiveStore((s) => s.snapshot);

  useEffect(() => {
    void probeResolve().then(setAvailable);
  }, []);

  if (!available) return null;

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    const trimmed = code.trim();
    if (!trimmed) {
      setFoundKey(null);
      setMessage('');
      return;
    }
    try {
      const key = await resolveCode(trimmed);
      if (key === null) {
        setAvailable(false);
        return;
      }
      setFoundKey(key);
      const onPage = [...(snapshot?.live ?? []), ...(snapshot?.ended ?? [])].some(
        (b) => b.broadcastKey === key,
      );
      // A code that resolves to nothing on the page is the COMMON case, not an
      // error — the broadcast may have ended and aged out, or never existed.
      setMessage(onPage ? '' : 'not on this page');
    } catch (err) {
      setMessage(err instanceof Error ? err.message : 'lookup failed');
    }
  };

  return (
    <form className={styles.form} onSubmit={submit}>
      <input
        className={styles.input}
        value={code}
        onChange={(e) => setCode(e.target.value)}
        maxLength={6}
        placeholder="code"
        autoComplete="off"
        spellCheck={false}
        aria-label="Find a stream by its broadcast code"
      />
      <button type="submit" className={styles.button}>
        Find
      </button>
      {foundKey && (
        <button
          type="button"
          className={styles.clear}
          onClick={() => {
            setFoundKey(null);
            setCode('');
            setMessage('');
          }}
        >
          clear
        </button>
      )}
      {/* Fixed-width slot so the header does not resize as messages come and go. */}
      <span className={styles.message}>{message}</span>
    </form>
  );
}
