// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest';
import { renderHook } from '@testing-library/react';
import { formatHotkey, useHotkey, type Hotkey } from './useHotkey';

const STATS: Hotkey = { key: 'd', ctrl: true, alt: true, shift: true };

function press(target: EventTarget, over: Partial<KeyboardEventInit> = {}) {
  target.dispatchEvent(
    new KeyboardEvent('keydown', {
      key: 'd',
      ctrlKey: true,
      altKey: true,
      shiftKey: true,
      bubbles: true,
      ...over,
    }),
  );
}

afterEach(() => {
  document.body.innerHTML = '';
});

describe('useHotkey', () => {
  it('fires on an exact modifier match', () => {
    const handler = vi.fn();
    renderHook(() => useHotkey(STATS, handler));
    press(window);
    expect(handler).toHaveBeenCalledTimes(1);
  });

  it('fires when Option has changed the reported character (macOS)', () => {
    const handler = vi.fn();
    renderHook(() => useHotkey(STATS, handler));
    press(window, { key: 'Î', code: 'KeyD' });
    expect(handler).toHaveBeenCalledTimes(1);
  });

  it('does not match a different physical key through code', () => {
    const handler = vi.fn();
    renderHook(() => useHotkey(STATS, handler));
    press(window, { key: 'ß', code: 'KeyS' });
    expect(handler).not.toHaveBeenCalled();
  });

  it('matches a shortcut without Alt by character only', () => {
    const handler = vi.fn();
    renderHook(() => useHotkey({ key: 'f' }, handler));
    // Dvorak: the physical F key types 'u'.
    press(window, { key: 'u', code: 'KeyF', ctrlKey: false, altKey: false, shiftKey: false });
    expect(handler).not.toHaveBeenCalled();
  });

  it('ignores a partial modifier match', () => {
    const handler = vi.fn();
    renderHook(() => useHotkey(STATS, handler));
    press(window, { shiftKey: false });
    expect(handler).not.toHaveBeenCalled();
  });

  it('ignores auto-repeat', () => {
    const handler = vi.fn();
    renderHook(() => useHotkey(STATS, handler));
    press(window, { repeat: true });
    expect(handler).not.toHaveBeenCalled();
  });

  it('does not fire while a text field is focused', () => {
    const handler = vi.fn();
    const input = document.createElement('input');
    document.body.appendChild(input);
    renderHook(() => useHotkey(STATS, handler));
    press(input);
    expect(handler).not.toHaveBeenCalled();
  });

  it('formats a readable label', () => {
    expect(formatHotkey(STATS)).toBe('Ctrl+Alt+Shift+D');
  });
});
