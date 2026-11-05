import { useEffect } from 'react';

import { usePinStore } from './pinStore.ts';

/**
 * Publish a stack's crosshair to the pin store whenever it is on the stack,
 * and clear it when the page goes, so the next page starts from its own
 * default instant.
 */
export function usePinFollow(cursorMs: number | null) {
  const set = usePinStore((s) => s.set);
  useEffect(() => {
    if (cursorMs !== null) set(cursorMs);
  }, [cursorMs, set]);
  useEffect(() => () => set(null), [set]);
}
