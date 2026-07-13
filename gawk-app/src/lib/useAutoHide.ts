import { useEffect, useRef, useState } from 'react';

// Auto-hiding UI chrome (docs/10 J3). Returns whether controls should be
// visible: true on mount and after any pointer/key activity, false after
// `idleMs` of inactivity. When `enabled` is false (e.g. an overlay is up, or
// the viewer isn't actively watching) it stays visible and no timer runs.
export function useAutoHide(idleMs: number, enabled = true): boolean {
  const [visible, setVisible] = useState(true);
  const timerRef = useRef<number | null>(null);

  useEffect(() => {
    if (!enabled) {
      setVisible(true);
      if (timerRef.current !== null) {
        clearTimeout(timerRef.current);
        timerRef.current = null;
      }
      return;
    }

    const reveal = () => {
      setVisible(true);
      if (timerRef.current !== null) clearTimeout(timerRef.current);
      timerRef.current = window.setTimeout(() => setVisible(false), idleMs);
    };

    reveal();
    window.addEventListener('pointermove', reveal);
    window.addEventListener('pointerdown', reveal);
    window.addEventListener('keydown', reveal);
    return () => {
      window.removeEventListener('pointermove', reveal);
      window.removeEventListener('pointerdown', reveal);
      window.removeEventListener('keydown', reveal);
      if (timerRef.current !== null) {
        clearTimeout(timerRef.current);
        timerRef.current = null;
      }
    };
  }, [enabled, idleMs]);

  return visible;
}
