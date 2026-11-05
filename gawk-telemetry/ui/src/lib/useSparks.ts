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

  useEffect(() => {
    if (!enabled || paused) return;
    const ac = new AbortController();
    const load = () =>
      fetchSparks(broadcastKey, ac.signal)
        .then(setSparks)
        // A miss (the projection no longer holds the broadcast, or a server
        // predating the endpoint) leaves the column on "—" and the poll
        // carries on: the card it serves may still be there next time.
        .catch(() => setSparks(null));
    void load();
    const id = setInterval(load, POLL_MS);
    return () => {
      clearInterval(id);
      ac.abort();
    };
  }, [broadcastKey, enabled, paused]);

  return sparks;
}
