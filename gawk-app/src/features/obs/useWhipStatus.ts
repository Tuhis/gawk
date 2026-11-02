// R33 (docs/83 U7, D9): the OBS page's status poll. Every 2 s while the tab
// is visible, never while it is hidden: hiding stops the timer and aborts a
// read in flight, showing it again reads at once and resumes. A null key
// (Forget key) stops it. The subscription is keyed on the key itself, which
// only changes on an explicit action (create, paste, reset, forget).
import { useCallback, useEffect, useRef, useState } from 'react';
import { readWhipStatus, type StatusRead } from './status';

export const POLL_INTERVAL_MS = 2000;

function tabHidden(): boolean {
  return typeof document !== 'undefined' && document.visibilityState === 'hidden';
}

export function useWhipStatus(
  statusUrl: string,
  key: string | null,
): { read: StatusRead | null; readAt: number; refresh: () => void } {
  // Tagged with the key it answers for, so a reset or a forget never shows
  // the previous key's status for a render.
  // `at` (Date.now() on arrival) bases the S4 countdown.
  const [state, setState] = useState<{ key: string; read: StatusRead; at: number } | null>(null);
  const refreshRef = useRef<() => void>(() => {});

  useEffect(() => {
    if (statusUrl === '' || key === null) {
      refreshRef.current = () => {};
      return;
    }
    let cancelled = false;
    let timer: ReturnType<typeof setTimeout> | null = null;
    let ctrl: AbortController | null = null;

    const stop = () => {
      if (timer !== null) clearTimeout(timer);
      timer = null;
      ctrl?.abort();
      ctrl = null;
    };
    const schedule = () => {
      if (cancelled || timer !== null || tabHidden()) return;
      timer = setTimeout(() => void poll(), POLL_INTERVAL_MS);
    };
    const poll = async () => {
      stop();
      if (cancelled || tabHidden()) return;
      const mine = new AbortController();
      ctrl = mine;
      const read = await readWhipStatus(statusUrl, key, mine.signal);
      if (cancelled || mine.signal.aborted) return;
      ctrl = null;
      // A failed read (network, CORS, 5xx) keeps what the page showed.
      if (read.kind !== 'error') setState({ key, read, at: Date.now() });
      schedule();
    };
    const onVisibility = () => {
      if (tabHidden()) stop();
      else void poll();
    };

    refreshRef.current = () => void poll();
    document.addEventListener('visibilitychange', onVisibility);
    void poll();
    return () => {
      cancelled = true;
      stop();
      document.removeEventListener('visibilitychange', onVisibility);
    };
  }, [statusUrl, key]);

  const refresh = useCallback(() => refreshRef.current(), []);
  const current = state !== null && state.key === key ? state : null;
  return { read: current?.read ?? null, readAt: current?.at ?? 0, refresh };
}
