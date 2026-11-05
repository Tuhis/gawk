import { create } from 'zustand';

// R36 (docs/86 TU1): dark by default (UD14), light on request, or the OS's
// choice. The preference is one per browser — a convenience, not state anyone
// else needs — so it lives in localStorage and the page renders correctly
// without it.
//
// The theme is RESOLVED here and written to <html data-theme>, rather than
// left to a CSS media query, because the charts paint on a canvas: they cannot
// read a stylesheet, so they need the resolved theme as a value to re-render
// on, and the colours from the same tokens the CSS uses (`charts/palette.ts`).

export type ThemePref = 'dark' | 'light' | 'system';
export type Theme = 'dark' | 'light';

const KEY = 'gawk-telemetry.theme';

function readPref(): ThemePref {
  try {
    const v = window.localStorage.getItem(KEY);
    if (v === 'dark' || v === 'light' || v === 'system') return v;
  } catch {
    // Storage blocked: the default is fine.
  }
  return 'dark';
}

function systemTheme(): Theme {
  try {
    return window.matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark';
  } catch {
    return 'dark';
  }
}

export function resolveTheme(pref: ThemePref): Theme {
  return pref === 'system' ? systemTheme() : pref;
}

interface ThemeState {
  pref: ThemePref;
  theme: Theme;
  setPref: (pref: ThemePref) => void;
  /** Re-resolve after the OS preference changed (only matters for `system`). */
  refresh: () => void;
}

function apply(theme: Theme) {
  document.documentElement.dataset.theme = theme;
}

export const useThemeStore = create<ThemeState>((set, get) => {
  const pref = readPref();
  return {
    pref,
    theme: resolveTheme(pref),
    setPref: (next) => {
      try {
        window.localStorage.setItem(KEY, next);
      } catch {
        // Not persisted; still applied for this page's lifetime.
      }
      const theme = resolveTheme(next);
      apply(theme);
      set({ pref: next, theme });
    },
    refresh: () => {
      const theme = resolveTheme(get().pref);
      if (theme === get().theme) return;
      apply(theme);
      set({ theme });
    },
  };
});

/**
 * Write the resolved theme before the first paint and follow the OS while the
 * preference is `system`. Called once from main.tsx, before React renders, so
 * a light-theme reader never sees a dark flash.
 */
export function installTheme(): () => void {
  apply(useThemeStore.getState().theme);
  let mq: MediaQueryList | null = null;
  try {
    mq = window.matchMedia('(prefers-color-scheme: light)');
  } catch {
    return () => {};
  }
  const onChange = () => useThemeStore.getState().refresh();
  mq.addEventListener?.('change', onChange);
  return () => mq?.removeEventListener?.('change', onChange);
}
