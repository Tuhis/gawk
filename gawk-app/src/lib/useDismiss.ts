import { useEffect, type RefObject } from 'react';
import { isEditable } from './useHotkey';

// Closes an anchored popover while it is open: on a press anywhere outside
// `ref` (the popover and the control that opened it), or on Escape. The press
// is taken in the capture phase, so a handler inside that stops propagation
// can't keep the popover open behind the user's back. An Escape typed in a
// text field (a nickname the user tabbed to) is the field's, and one another
// layer already handled (defaultPrevented) is that layer's.
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
      if (e.key !== 'Escape' || e.defaultPrevented || isEditable(e.target)) return;
      onClose('escape');
    };
    document.addEventListener('pointerdown', onDown, true);
    document.addEventListener('keydown', onKey);
    return () => {
      document.removeEventListener('pointerdown', onDown, true);
      document.removeEventListener('keydown', onKey);
    };
  }, [ref, open, onClose]);
}
