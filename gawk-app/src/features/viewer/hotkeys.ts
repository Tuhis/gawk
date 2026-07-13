import type { Hotkey } from '../../lib/useHotkey';

// The stats overlay shortcut (docs/10 Decision 5). Mirrors Netflix's
// Ctrl+Shift+Alt+D; one constant, trivially re-bindable. The right-click menu
// is the discoverable path to the same overlay.
export const STATS_HOTKEY: Hotkey = { key: 'd', ctrl: true, alt: true, shift: true };
