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
