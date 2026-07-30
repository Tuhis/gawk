import { useCallback, useEffect, useState } from 'react';

import { fetchAnnotations } from '../api/client.ts';
import type { Annotation } from '../api/types.ts';
import { useMetaStore } from '../state/metaStore.ts';

/**
 * Load the notes for one scope (TH8).
 *
 * Lives beside the component rather than inside it so the module stays
 * component-only — and so a view that wants markers without the editing panel
 * (a chart in a report, say) can have them.
 */
export function useAnnotations(scope: { sessionId?: string; broadcastKey?: string }) {
  const enabled = useMetaStore((s) => s.meta?.annotations ?? false);
  const [notes, setNotes] = useState<Annotation[]>([]);
  const { sessionId, broadcastKey } = scope;

  const reload = useCallback(() => {
    if (!enabled) return;
    fetchAnnotations({ sessionId, broadcastKey })
      .then(setNotes)
      // A failed annotation fetch must never take the page down with it: the
      // measurements are the point and the notes are beside them.
      .catch(() => setNotes([]));
  }, [enabled, sessionId, broadcastKey]);

  useEffect(reload, [reload]);
  return { enabled, notes, reload };
}
