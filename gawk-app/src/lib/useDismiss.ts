import { useEffect, type RefObject } from 'react';

// Closes an anchored popover while it is open: on a press anywhere outside
// `ref` (the popover and the control that opened it), or on Escape. The press
// is taken in the capture phase, so a handler inside that stops propagation
// can't keep the popover open behind the user's back. Escape counts only from
// inside `ref` or with nothing focused (Safari doesn't focus a clicked
// button): one pressed in another layer, a nickname field the user tabbed
// to, is that layer's.
export function useDismiss(
  ref: RefObject<HTMLElement | null>,
  open: boolean,
  onClose: (via: 'outside' | 'escape') => void,
): void {
  useEffect(() => {
    if (!open) return;
    const onDown = (e: PointerEvent) => {
      if (!ref.current?.contains(e.target as Node)) onClose('outside');
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== 'Escape' || e.defaultPrevented) return;
      const t = e.target as Node | null;
      const nowhere = t === document || t === document.body || t === document.documentElement;
      if (nowhere || ref.current?.contains(t)) onClose('escape');
    };
    document.addEventListener('pointerdown', onDown, true);
    document.addEventListener('keydown', onKey);
    return () => {
      document.removeEventListener('pointerdown', onDown, true);
      document.removeEventListener('keydown', onKey);
    };
  }, [ref, open, onClose]);
}
