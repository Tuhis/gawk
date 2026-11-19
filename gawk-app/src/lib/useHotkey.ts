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

// Inputs that take no typing: with one focused, a key is still a command
// (R86: a focused checkbox in the room's What to show popover must not
// swallow the Shift+number keys the popover itself names).
const NON_TEXT_INPUTS = new Set(['checkbox', 'radio', 'range', 'button', 'submit', 'reset', 'image', 'color', 'file']);

// True when the event target is a text field, where keys are typing.
export function isEditable(target: EventTarget | null): boolean {
  const el = target as HTMLElement | null;
  if (!el || typeof el.tagName !== 'string') return false;
  if (el.tagName === 'INPUT') return !NON_TEXT_INPUTS.has((el as HTMLInputElement).type);
  return el.tagName === 'TEXTAREA' || el.tagName === 'SELECT' || el.isContentEditable;
}

// On macOS, Option changes the character `key` reports (Option+Shift+D is
// 'Î'), so an Alt shortcut's letter also matches by its physical key. Only
// then: elsewhere `code` would misfire on non-QWERTY layouts.
//
// Shift changes a digit's `key` the same way (Shift+1 is '!' on US layouts,
// '"' on Nordic ones), so a Shift shortcut's digit matches by its physical
// key in the number row instead — unless Shift typed the digit itself, as on
// AZERTY, where that is the user typing "1", not asking for Shift+1.
function matchesKey(e: KeyboardEvent, key: string, alt: boolean, shift: boolean): boolean {
  const want = key.toLowerCase();
  if (shift && /^[0-9]$/.test(want)) return e.code === `Digit${want}` && !/^[0-9]$/.test(e.key);
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
      if (!matchesKey(e, hotkey.key, !!hotkey.alt, !!hotkey.shift)) return;
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
