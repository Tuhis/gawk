import { useEffect, useState } from 'react';

/** The phone breakpoint (UD17): read-only triage below it, the console above. */
export const NARROW = '(max-width: 720px)';

export function useNarrow(): boolean {
  const [narrow, setNarrow] = useState(() => {
    try {
      return window.matchMedia(NARROW).matches;
    } catch {
      return false;
    }
  });
  useEffect(() => {
    let mq: MediaQueryList;
    try {
      mq = window.matchMedia(NARROW);
    } catch {
      return;
    }
    const update = () => setNarrow(mq.matches);
    mq.addEventListener?.('change', update);
    return () => mq.removeEventListener?.('change', update);
  }, []);
  return narrow;
}
