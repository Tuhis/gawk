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

  // Shift changes a digit's character with the layout ('!' on US, '"' on
  // Nordic), so Shift+digit shortcuts match the number row by code (R86).
  it('matches Shift+digit by its number-row key, and the plain digit only without Shift', () => {
    const shifted = vi.fn();
    const plain = vi.fn();
    renderHook(() => useHotkey({ key: '2', shift: true }, shifted));
    renderHook(() => useHotkey({ key: '2' }, plain));
    press(window, { key: '"', code: 'Digit2', ctrlKey: false, altKey: false, shiftKey: true });
    press(window, { key: '@', code: 'Digit2', ctrlKey: false, altKey: false, shiftKey: true });
    expect(shifted).toHaveBeenCalledTimes(2);
    expect(plain).not.toHaveBeenCalled();
    press(window, { key: '2', code: 'Digit2', ctrlKey: false, altKey: false, shiftKey: false });
    expect(plain).toHaveBeenCalledTimes(1);
    expect(shifted).toHaveBeenCalledTimes(2);
    // Another digit's key is not this one.
    press(window, { key: '#', code: 'Digit3', ctrlKey: false, altKey: false, shiftKey: true });
    expect(shifted).toHaveBeenCalledTimes(2);
  });

  // AZERTY types a digit with Shift: that is the digit, not Shift+digit.
  it('does not take a digit Shift typed (AZERTY) for the Shift shortcut', () => {
    const shifted = vi.fn();
    renderHook(() => useHotkey({ key: '1', shift: true }, shifted));
    press(window, { key: '1', code: 'Digit1', ctrlKey: false, altKey: false, shiftKey: true });
    expect(shifted).not.toHaveBeenCalled();
  });

  // A checkbox or radio takes no typing, so keys stay commands there (R86).
  it('fires with a checkbox, radio or range focused, not with a text field', () => {
    const handler = vi.fn();
    renderHook(() => useHotkey({ key: '2', shift: true }, handler));
    for (const type of ['checkbox', 'radio', 'range', 'text', 'search', 'number']) {
      const input = document.createElement('input');
      input.type = type;
      document.body.appendChild(input);
      press(input, { key: '@', code: 'Digit2', ctrlKey: false, altKey: false, shiftKey: true });
    }
    expect(handler).toHaveBeenCalledTimes(3);
  });

  it('formats a readable label', () => {
    expect(formatHotkey(STATS)).toBe('Ctrl+Alt+Shift+D');
  });
});
