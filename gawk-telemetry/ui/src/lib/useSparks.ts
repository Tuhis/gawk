import { useEffect, useState } from 'react';

import { fetchSparks } from '../api/client.ts';
import type { Sparks } from '../api/types.ts';
import { useLiveStore } from '../state/liveStore.ts';

// One broadcast's 2-minute tails (R36, docs/86 TU7), served per broadcast by
// `/live/sparks` rather than carried in the SSE snapshot, so the live feed
// stays as small as UD1 bounds it. Polled only while the card is open.

const POLL_MS = 2000;

/** Poll one broadcast's tails while `enabled` (the card is open, not paused). */
export function useSparks(broadcastKey: string, enabled: boolean): Sparks | null {
  const [sparks, setSparks] = useState<Sparks | null>(null);
  const paused = useLiveStore((s) => s.paused);
  const [gone, setGone] = useState(false);

  useEffect(() => {
    if (!enabled || paused || gone) return;
    const ac = new AbortController();
    const load = () =>
      fetchSparks(broadcastKey, ac.signal)
        .then(setSparks)
        .catch((e: unknown) => {
          // A server without the endpoint answers 404: stop asking, and the
          // column says so rather than spinning.
          if ((e as { status?: number }).status === 404) setGone(true);
        });
    void load();
    const id = setInterval(load, POLL_MS);
    return () => {
      clearInterval(id);
      ac.abort();
    };
  }, [broadcastKey, enabled, paused, gone]);

  return sparks;
}
