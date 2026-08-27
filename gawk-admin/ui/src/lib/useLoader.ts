import { useCallback, useEffect, useState } from 'react';

import { AuthRedirect } from '../auth/session.ts';

export interface Loaded<T> {
  data: T | null;
  /**
   * When `data` arrived, in epoch milliseconds; 0 before the first load.
   *
   * Views render relative time (uptime, time-to-expiry) against THIS rather
   * than against `Date.now()` read during render. Two reasons, and the second
   * is why it is a field rather than a convenience:
   *
   *   * Honesty — the age shown belongs to the data, not to whenever React
   *     happened to re-render. A row that says "1m 30s" is 1m 30s as of the
   *     fetch that produced it.
   *   * Purity — `Date.now()` in a render body is a render side effect (oxlint
   *     flags it), and it makes any test that asserts on a formatted duration
   *     depend on how long the render took.
   */
  loadedAt: number;
  error: string | null;
  loading: boolean;
  reload: () => void;
}

/**
 * Load once on mount, optionally on an interval, and on demand.
 *
 * `load` must be stable (wrap it in `useCallback`) — this effect *acts*, and
 * re-firing it on every render is the stale/looping-effect bug CODE-REVIEW.md
 * calls out.
 *
 * `AuthRedirect` is swallowed on purpose: it means the session has started a
 * full-page navigation to the IdP, so the page is going away and rendering an
 * error under it would only flash.
 */
export function useLoader<T>(load: () => Promise<T>, intervalMs = 0): Loaded<T> {
  // One state object rather than two setters: `data` and the instant it
  // arrived are a single fact, and splitting them lets a render see one
  // without the other.
  const [result, setResult] = useState<{ data: T | null; loadedAt: number }>({
    data: null,
    loadedAt: 0,
  });
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [tick, setTick] = useState(0);

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    load()
      .then((d) => {
        if (cancelled) return;
        setResult({ data: d, loadedAt: Date.now() });
        setError(null);
      })
      .catch((err: unknown) => {
        if (cancelled || err instanceof AuthRedirect) return;
        setError(err instanceof Error ? err.message : String(err));
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [load, tick]);

  useEffect(() => {
    if (!intervalMs) return;
    const id = setInterval(() => setTick((t) => t + 1), intervalMs);
    return () => clearInterval(id);
  }, [intervalMs]);

  const reload = useCallback(() => setTick((t) => t + 1), []);
  return { data: result.data, loadedAt: result.loadedAt, error, loading, reload };
}
