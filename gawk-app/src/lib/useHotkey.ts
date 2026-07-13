import { useEffect, useRef } from 'react';

export interface Hotkey {
  // Matched case-insensitively against KeyboardEvent.key.
  key: string;
  ctrl?: boolean;
  alt?: boolean;
  shift?: boolean;
  meta?: boolean;
}

export function formatHotkey(h: Hotkey): string {
  const parts: string[] = [];
  if (h.ctrl) parts.push('Ctrl');
  if (h.alt) parts.push('Alt');
  if (h.shift) parts.push('Shift');
  if (h.meta) parts.push('Meta');
  parts.push(h.key.length === 1 ? h.key.toUpperCase() : h.key);
  return parts.join('+');
}

function isEditable(target: EventTarget | null): boolean {
  const el = target as HTMLElement | null;
  if (!el || typeof el.tagName !== 'string') return false;
  return (
    el.tagName === 'INPUT' ||
    el.tagName === 'TEXTAREA' ||
    el.tagName === 'SELECT' ||
    el.isContentEditable
  );
}

// A global keyboard shortcut (docs/10 J4). Exact modifier match, ignores key
// repeat, and never fires while a text field is focused. The handler is kept
// in a ref so passing an inline closure doesn't re-subscribe every render.
export function useHotkey(hotkey: Hotkey, handler: () => void): void {
  const handlerRef = useRef(handler);
  useEffect(() => {
    handlerRef.current = handler;
  });

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.repeat) return;
      if (isEditable(e.target)) return;
      if (e.key.toLowerCase() !== hotkey.key.toLowerCase()) return;
      if (!!hotkey.ctrl !== e.ctrlKey) return;
      if (!!hotkey.alt !== e.altKey) return;
      if (!!hotkey.shift !== e.shiftKey) return;
      if (!!hotkey.meta !== e.metaKey) return;
      e.preventDefault();
      handlerRef.current();
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [hotkey.key, hotkey.ctrl, hotkey.alt, hotkey.shift, hotkey.meta]);
}
