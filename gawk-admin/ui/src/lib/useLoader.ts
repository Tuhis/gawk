import { useCallback, useEffect, useState } from 'react';

import { AuthRedirect } from '../auth/session.ts';

export interface Loaded<T> {
  data: T | null;
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
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [tick, setTick] = useState(0);

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    load()
      .then((d) => {
        if (cancelled) return;
        setData(d);
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
  return { data, error, loading, reload };
}
