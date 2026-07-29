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

// --- R31: absolute time is the primary axis for anything historical (UD5) ---
//
// The vocabulary above — `ago()`, `dur()` — is right for the live page, where
// "now" is the anchor. It is wrong everywhere else: correlating with a relay
// log, with Prometheus, with a release timestamp or with a friend's "it broke
// around nine" needs a wall clock. So relative time becomes the ANNOTATION and
// absolute time becomes the axis.
//
// **The timezone is displayed, never assumed.** A dashboard that renders
// 21:04 without saying whose 21:04 is a dashboard that will eventually cost
// someone an hour.

/** The viewer's IANA zone, e.g. "Europe/Helsinki". */
export function timeZone(): string {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || 'local';
  } catch {
    return 'local';
  }
}

/** The zone's short name at a given instant, e.g. "EEST". */
export function timeZoneLabel(atMs: number = Date.now()): string {
  try {
    const parts = new Intl.DateTimeFormat(undefined, { timeZoneName: 'short' }).formatToParts(
      new Date(atMs),
    );
    return parts.find((p) => p.type === 'timeZoneName')?.value ?? timeZone();
  } catch {
    return timeZone();
  }
}

/** Date + time to the second. The form an operator pastes into a log search. */
export function absoluteTime(atMs: number | undefined | null): string {
  if (atMs === undefined || atMs === null || !Number.isFinite(atMs) || atMs <= 0) return EMPTY;
  const d = new Date(atMs);
  return `${d.toLocaleDateString(undefined, {
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
  })} ${d.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit', second: '2-digit' })}`;
}

/** Time only, for a row in a table whose date is already stated. */
export function timeOfDay(atMs: number | undefined | null): string {
  if (atMs === undefined || atMs === null || !Number.isFinite(atMs) || atMs <= 0) return EMPTY;
  return new Date(atMs).toLocaleTimeString(undefined, {
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
  });
}

/**
 * A chart axis label. Coarser than `absoluteTime` because an axis has to stay
 * readable at any zoom: the date appears only when the tick IS midnight, so a
 * multi-day range still says which day without repeating it on every tick.
 */
export function axisTime(value: number): string {
  const d = new Date(value);
  if (d.getHours() === 0 && d.getMinutes() === 0) {
    return d.toLocaleDateString(undefined, { month: 'short', day: 'numeric' });
  }
  if (d.getSeconds() === 0) {
    return d.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' });
  }
  return d.toLocaleTimeString(undefined, {
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
  });
}

/** A tooltip's timestamp: absolute, to the second, with the zone named. */
export function tooltipTime(atMs: number): string {
  return `${absoluteTime(atMs)} ${timeZoneLabel(atMs)}`;
}

/** An absolute range, collapsing the date when both ends share one. */
export function rangeLabel(fromMs: number, toMs: number): string {
  if (!fromMs || !toMs) return EMPTY;
  const a = new Date(fromMs);
  const b = new Date(toMs);
  if (a.toDateString() === b.toDateString()) {
    return `${absoluteTime(fromMs)} → ${timeOfDay(toMs)}`;
  }
  return `${absoluteTime(fromMs)} → ${absoluteTime(toMs)}`;
}

/**
 * A value with its unit, or the em dash. Used for evidence rows and dip movers.
 *
 * Unlike `num()` this does NOT pad to a fixed decimal count, and the difference
 * is deliberate: those are static values in prose, not a ticking column, so the
 * fixed-width rule buys nothing and "412.0 drops" reads as a measurement
 * precision nobody claimed. A whole number stays whole; a fraction gets enough
 * digits to be worth showing.
 */
export function withUnit(v: number | undefined, unit?: string, digits = 2): string {
  if (v === undefined || !Number.isFinite(v)) return EMPTY;
  const n = Number.isInteger(v)
    ? String(v)
    : Math.abs(v) >= 100
      ? v.toFixed(0)
      : Math.abs(v) >= 10
        ? v.toFixed(1)
        : v.toFixed(digits);
  return unit ? `${n} ${unit}` : n;
}

/** Confidence as a percentage, because 0.6 reads as a score and 60 % as a claim. */
export function confidence(c: number | undefined): string {
  if (c === undefined || !Number.isFinite(c)) return EMPTY;
  return `${Math.round(c * 100)}%`;
}
