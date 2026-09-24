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

// True when the event target is a text field. Exported for the R42 room
// view's number keys, which must never fire while a nickname is being typed.
export function isEditable(target: EventTarget | null): boolean {
  const el = target as HTMLElement | null;
  if (!el || typeof el.tagName !== 'string') return false;
  return (
    el.tagName === 'INPUT' ||
    el.tagName === 'TEXTAREA' ||
    el.tagName === 'SELECT' ||
    el.isContentEditable
  );
}

// On macOS, Option changes the character `key` reports (Option+Shift+D is
// 'Î'), so an Alt shortcut's letter also matches by its physical key. Only
// then: elsewhere `code` would misfire on non-QWERTY layouts.
function matchesKey(e: KeyboardEvent, key: string, alt: boolean): boolean {
  const want = key.toLowerCase();
  if (e.key.toLowerCase() === want) return true;
  return alt && /^[a-z]$/.test(want) && e.code === `Key${want.toUpperCase()}`;
}

// A global keyboard shortcut. Exact modifier match, ignores key repeat, and
// never fires while a text field is focused. The handler is kept in a ref so
// passing an inline closure doesn't re-subscribe every render.
export function useHotkey(hotkey: Hotkey, handler: () => void): void {
  const handlerRef = useRef(handler);
  useEffect(() => {
    handlerRef.current = handler;
  });

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.repeat) return;
      if (isEditable(e.target)) return;
      if (!matchesKey(e, hotkey.key, !!hotkey.alt)) return;
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
