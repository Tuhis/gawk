import { useEffect, useState } from 'react';

// The app's narrow-window breakpoint. Below it the room grid degrades to
// focus and its panel becomes a bottom sheet (room.module.css), and the
// volume controls collapse into a popover (ui/VolumeControl).
export const NARROW_QUERY = '(max-width: 719px)';

export function useMediaMatch(query: string): boolean {
  const [matches, setMatches] = useState(() => {
    if (typeof window === 'undefined' || typeof window.matchMedia !== 'function') return false;
    return window.matchMedia(query).matches;
  });
  useEffect(() => {
    if (typeof window === 'undefined' || typeof window.matchMedia !== 'function') return;
    const mq = window.matchMedia(query);
    const onChange = () => setMatches(mq.matches);
    onChange();
    mq.addEventListener?.('change', onChange);
    return () => mq.removeEventListener?.('change', onChange);
  }, [query]);
  return matches;
}
