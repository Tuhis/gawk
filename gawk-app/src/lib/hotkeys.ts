import type { Hotkey } from './useHotkey';

// The stats overlay shortcut (docs/10 Decision 5). Mirrors Netflix's
// Ctrl+Shift+Alt+D; one constant, trivially re-bindable. Shared by the viewer
// and (R9 M7) broadcaster overlays; the right-click menu / topbar button is
// the discoverable path to the same overlay.
export const STATS_HOTKEY: Hotkey = { key: 'd', ctrl: true, alt: true, shift: true };
