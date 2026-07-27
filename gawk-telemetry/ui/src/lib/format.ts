// Formatting rules exist to stop the layout MOVING. A dashboard that reflows
// every 2 s cannot be read, and a number that changes width drags its whole row
// with it. Two rules follow, and they are enforced here rather than left to each
// caller:
//
//   1. Every numeric cell is `font-variant-numeric: tabular-nums` (in CSS) AND
//      rendered at a fixed decimal count, so 9.9 and 10.0 occupy one width.
//   2. A value that is absent renders as an em dash of the same width, never as
//      an empty string that collapses the cell.

export const EMPTY = '—';

/** Fixed-decimal number, or the em dash when there is nothing to show. */
export function num(v: number | undefined | null, digits = 0): string {
  if (v === undefined || v === null || !Number.isFinite(v)) return EMPTY;
  return v.toFixed(digits);
}

/** Integer-ish rate: one decimal below 10, none above — still fixed per bucket. */
export function fps(v: number | undefined | null): string {
  if (v === undefined || v === null || !Number.isFinite(v)) return EMPTY;
  return v < 10 ? v.toFixed(1) : v.toFixed(0);
}

export function ms(v: number | undefined | null): string {
  if (v === undefined || v === null || !Number.isFinite(v)) return EMPTY;
  return `${Math.round(v)} ms`;
}

/** Compact byte-rate for bitrates. */
export function bitrate(bps: number | undefined | null): string {
  if (bps === undefined || bps === null || !Number.isFinite(bps)) return EMPTY;
  if (bps >= 1e6) return `${(bps / 1e6).toFixed(1)} Mbps`;
  if (bps >= 1e3) return `${(bps / 1e3).toFixed(0)} kbps`;
  return `${Math.round(bps)} bps`;
}

/** Elapsed duration, widest form first so the width is stable within a bucket. */
export function dur(msValue: number | undefined | null): string {
  if (msValue === undefined || msValue === null || msValue < 0) return EMPTY;
  const s = Math.round(msValue / 1000);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ${String(s % 60).padStart(2, '0')}s`;
  const h = Math.floor(m / 60);
  return `${h}h ${String(m % 60).padStart(2, '0')}m`;
}

/**
 * Age, in words. `never` is deliberate and load-bearing: a negative age is how
 * the backend spells "this side has produced no evidence at all", and it must
 * not read as "0 s ago".
 */
export function ago(msValue: number | undefined | null): string {
  if (msValue === undefined || msValue === null || msValue < 0) return 'never';
  if (msValue < 1500) return 'now';
  const s = Math.round(msValue / 1000);
  if (s < 60) return `${s}s ago`;
  const m = Math.round(s / 60);
  if (m < 60) return `${m}m ago`;
  return `${Math.round(m / 60)}h ago`;
}

/** Wall-clock, for the "updated" stamp. Fixed width by construction. */
export function clockTime(atMs: number): string {
  return new Date(atMs).toLocaleTimeString(undefined, {
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
  });
}

/** Short display form of an id — enough to recognise, never the whole thing. */
export function shortId(id: string, len = 8): string {
  return id.slice(0, len);
}
