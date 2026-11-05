import { useMemo } from 'react';

import { useThemeStore, type Theme } from '../state/theme.ts';

// The chart layer's colours, read from the SAME tokens the stylesheet uses.
//
// R31's charts carried their own dark-tuned literals (`#58a6ff` crosshairs,
// `rgba(11,12,14,0.55)` veils), which painted a black veil on a white chart
// the moment anyone used light mode. A canvas cannot read CSS, so this reads
// the computed custom properties once per theme and hands the chart options
// plain strings. Nothing below may hardcode a colour.

export interface Palette {
  theme: Theme;
  bg: string;
  surface1: string;
  surface2: string;
  surface3: string;
  line: string;
  lineStrong: string;
  text: string;
  text2: string;
  text3: string;
  ok: string;
  warn: string;
  bad: string;
  okBg: string;
  warnBg: string;
  badBg: string;
  accent: string;
  accentBg: string;
  veil: string;
  hatch: string;
  hatchLine: string;
  /** Categorical series colours. Never a severity hue (UD14). */
  series: string[];
  sans: string;
  mono: string;
}

// Values used when there is no DOM to read (tests) — the dark set, verbatim.
const FALLBACK: Record<string, string> = {
  '--bg': '#0c0e11',
  '--surface-1': '#13161b',
  '--surface-2': '#191d23',
  '--surface-3': '#20252d',
  '--line': '#242932',
  '--line-strong': '#323946',
  '--text': '#e7e9ed',
  '--text-2': '#a7adb8',
  '--text-3': '#8a919d',
  '--ok': '#46c25a',
  '--warn': '#e9ad45',
  '--bad': '#f2645d',
  '--ok-bg': 'rgba(70, 194, 90, 0.12)',
  '--warn-bg': 'rgba(233, 173, 69, 0.13)',
  '--bad-bg': 'rgba(242, 100, 93, 0.14)',
  '--accent': '#6ea8ff',
  '--accent-bg': 'rgba(110, 168, 255, 0.13)',
  '--veil': 'rgba(12, 14, 17, 0.62)',
  '--hatch': 'rgba(167, 173, 184, 0.1)',
  '--hatch-line': 'rgba(167, 173, 184, 0.2)',
  '--s1': '#6ea8ff',
  '--s2': '#b69cf5',
  '--s3': '#4cc4c4',
  '--s4': '#ea8fc5',
  '--s5': '#9fb3cc',
  '--sans': "'IBM Plex Sans', system-ui, sans-serif",
  '--mono': "'IBM Plex Mono', ui-monospace, monospace",
};

export function readPalette(theme: Theme): Palette {
  let cs: CSSStyleDeclaration | null = null;
  try {
    cs = getComputedStyle(document.documentElement);
  } catch {
    cs = null;
  }
  const v = (name: string) => cs?.getPropertyValue(name).trim() || FALLBACK[name] || '';
  return {
    theme,
    bg: v('--bg'),
    surface1: v('--surface-1'),
    surface2: v('--surface-2'),
    surface3: v('--surface-3'),
    line: v('--line'),
    lineStrong: v('--line-strong'),
    text: v('--text'),
    text2: v('--text-2'),
    text3: v('--text-3'),
    ok: v('--ok'),
    warn: v('--warn'),
    bad: v('--bad'),
    okBg: v('--ok-bg'),
    warnBg: v('--warn-bg'),
    badBg: v('--bad-bg'),
    accent: v('--accent'),
    accentBg: v('--accent-bg'),
    veil: v('--veil'),
    hatch: v('--hatch'),
    hatchLine: v('--hatch-line'),
    series: [v('--s1'), v('--s2'), v('--s3'), v('--s4'), v('--s5')],
    sans: v('--sans'),
    mono: v('--mono'),
  };
}

/** The palette for the current theme; a new object only when the theme changes. */
export function usePalette(): Palette {
  const theme = useThemeStore((s) => s.theme);
  return useMemo(() => readPalette(theme), [theme]);
}

/** A severity's band colour: the token's own tint, so it matches the pills. */
export function severityTint(p: Palette, sev: string): string {
  if (sev === 'bad') return p.badBg;
  if (sev === 'warn') return p.warnBg;
  if (sev === 'ok') return p.okBg;
  return p.hatch;
}

/** A severity's solid colour. `unknown` has no hue, deliberately. */
export function severityColor(p: Palette, sev: string): string {
  if (sev === 'bad') return p.bad;
  if (sev === 'warn') return p.warn;
  if (sev === 'ok') return p.ok;
  return p.text3;
}
