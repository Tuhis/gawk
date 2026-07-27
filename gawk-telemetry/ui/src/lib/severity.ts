import type { Severity } from '../api/types.ts';

// Severity is carried by THREE channels — glyph, word and hue — and never by
// hue alone. The page has to survive a colour-blind reader, a greyscale
// screenshot pasted into a chat, and the CSS failing to load entirely.

export const GLYPH: Record<Severity, string> = {
  ok: '○', // ○
  warn: '△', // △
  bad: '●', // ●
  unknown: '?',
};

const ORDER: Record<Severity, number> = { bad: 3, warn: 2, unknown: 1, ok: 0 };

export function rank(s: Severity | undefined): number {
  return s ? (ORDER[s] ?? 0) : 0;
}

/**
 * The severity a broadcast row shows: its own, or its worst viewer's if that is
 * worse. A healthy relay carrying a stuttering viewer is not a healthy
 * broadcast, and the fleet scan is the place that has to say so.
 */
export function effectiveSeverity(own: Severity, worstViewer: Severity | undefined): Severity {
  return worstViewer && rank(worstViewer) > rank(own) ? worstViewer : own;
}

export function isSeverity(v: string): v is Severity {
  return v === 'ok' || v === 'warn' || v === 'bad' || v === 'unknown';
}

/**
 * Freshness -> the severity-ish tone a staleness label wears. Absence of
 * evidence is never `ok`: a side that has never reported is `unknown`, one that
 * has gone quiet is `warn`-toned. This is the page's cardinal rule.
 */
export function freshnessTone(state: string): Severity {
  if (state === 'reporting' || state === 'observed') return 'ok';
  if (state === 'stale') return 'warn';
  return 'unknown';
}
