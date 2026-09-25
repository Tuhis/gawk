import type { Hotkey } from './useHotkey';

// The stats overlay shortcut (Netflix's Ctrl+Shift+Alt+D). The stats button
// and the viewer's right-click menu are the discoverable way in.
export const STATS_HOTKEY: Hotkey = { key: 'd', ctrl: true, alt: true, shift: true };
