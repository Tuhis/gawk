import { useCallback, useEffect, useMemo, useRef, useState } from 'react';

// R36 item 1 (docs/86 TU2): columns that fit their content, and that the
// operator can resize.
//
// R31 declared every table's widths before it had data (`15ch 7ch …`,
// percentage colgroups) and truncated whatever did not fit, and nothing could
// be widened. The rule this replaces it with keeps the two guarantees the
// fixed layouts existed for — the table fits its container, and NO width
// changes while values tick (TH11) — and adds the one they lacked:
//
//   * **Measured once per data load, then frozen.** Widths come from the
//     content, measured with a canvas against the bundled fonts — so a
//     virtualized grid measures every loaded row, not just the ones on
//     screen, and nothing reflows as you scroll. A ticking value never
//     re-measures; a new LOAD (another range, another filter) does.
//   * **One free-text column takes the remainder** and is the only one that
//     ellipsises (with the whole value in its title).
//   * **The operator overrides**: drag a header's grip, double-click it to fit
//     that column to its content, "reset widths" to drop every override.
//     Overrides persist per table in localStorage — not in the URL, where
//     eight widths would make a link unpasteable.

export interface ColumnSpec<T> {
  key: string;
  /** The header text; measured too, so a header is never cut ("DURA…"). */
  label: string;
  /** The text this column shows for a row — what is measured. */
  text?: (row: T) => string;
  /**
   * Strings the column must always fit, whatever the rows hold: the widest
   * form a ticking value can take ('60.0 → 60.0 / 60'), or a fixed badge.
   */
  samples?: string[];
  /** Pixels beyond the text: an icon, a badge's padding, a tag. */
  extra?: number;
  mono?: boolean;
  /** Bounds on the MEASURED width. An operator's drag ignores `max`. */
  min?: number;
  max?: number;
  /** The free-text column that takes the remainder. At most one per table. */
  flex?: boolean;
  /** Smallest width the flexible column may be squeezed to. */
  flexMin?: number;
  /**
   * Free text that may give up width when the table would not fit its
   * container (a client string, a delivery mode) — down to `squeezeMin`. Its
   * cells ellipsise and carry the whole value in their title.
   */
  squeeze?: boolean;
  squeezeMin?: number;
}

/** Horizontal padding of a cell (12px a side), matched to tables.module.css. */
export const CELL_PAD = 24;

const SANS_CELL = "12px 'IBM Plex Sans', system-ui, sans-serif";
const MONO_CELL = "12px 'IBM Plex Mono', ui-monospace, monospace";
const HEAD = "500 11px 'IBM Plex Sans', system-ui, sans-serif";

let ctx: CanvasRenderingContext2D | null | undefined;

function context(): CanvasRenderingContext2D | null {
  if (ctx !== undefined) return ctx;
  try {
    // jsdom has no canvas; it logs a complaint per call, so do not ask it.
    ctx = /jsdom/.test(navigator.userAgent) ? null : document.createElement('canvas').getContext('2d');
  } catch {
    ctx = null;
  }
  return ctx;
}

/**
 * Width of `text` in `font`. Falls back to a per-character estimate where
 * there is no canvas (tests), which errs wide rather than narrow.
 */
export function textWidth(text: string, font: string): number {
  const c = context();
  if (!c) return Math.ceil(text.length * (font.includes('Mono') ? 7.3 : 6.9));
  c.font = font;
  return Math.ceil(c.measureText(text).width);
}

/** Measure every column against `rows`. Pure, so it is testable on its own. */
export function measureColumns<T>(cols: ColumnSpec<T>[], rows: T[]): Record<string, number> {
  const out: Record<string, number> = {};
  for (const col of cols) {
    // Headers render at 11px with a little tracking; +4 covers the sort mark.
    let w = textWidth(col.label, HEAD) + Math.ceil(col.label.length * 0.35) + 4;
    const font = col.mono ? MONO_CELL : SANS_CELL;
    for (const s of col.samples ?? []) w = Math.max(w, textWidth(s, font));
    if (col.text && !col.flex) {
      // Bounded work: a 5 000-row SQL result is measured on its first 2 000
      // rows. Beyond that the operator's grip is the answer.
      const n = Math.min(rows.length, 2000);
      for (let i = 0; i < n; i++) {
        const t = col.text(rows[i]);
        if (t) w = Math.max(w, textWidth(t, font));
      }
    }
    w += CELL_PAD + (col.extra ?? 0);
    if (col.min !== undefined) w = Math.max(w, col.min);
    if (col.max !== undefined) w = Math.min(w, col.max);
    out[col.key] = Math.ceil(w);
  }
  return out;
}

/** The un-capped content width of one column: what a double-click fits to. */
export function fitColumn<T>(col: ColumnSpec<T>, rows: T[]): number {
  return measureColumns([{ ...col, max: undefined, flex: false }], rows)[col.key];
}

const STORE_PREFIX = 'gawk-telemetry.widths.';

function loadOverrides(tableId: string): Record<string, number> {
  try {
    const raw = window.localStorage.getItem(STORE_PREFIX + tableId);
    if (!raw) return {};
    const parsed = JSON.parse(raw) as unknown;
    if (!parsed || typeof parsed !== 'object') return {};
    const out: Record<string, number> = {};
    for (const [k, v] of Object.entries(parsed as Record<string, unknown>)) {
      if (typeof v === 'number' && Number.isFinite(v) && v >= 24 && v <= 2000) out[k] = Math.round(v);
    }
    return out;
  } catch {
    return {};
  }
}

function saveOverrides(tableId: string, o: Record<string, number>) {
  try {
    if (Object.keys(o).length === 0) window.localStorage.removeItem(STORE_PREFIX + tableId);
    else window.localStorage.setItem(STORE_PREFIX + tableId, JSON.stringify(o));
  } catch {
    // Storage blocked: the override still holds for this page's lifetime.
  }
}

/**
 * A load key for "who is on the table": the same set of ids in any order is
 * the same load. A live table re-sorts by severity as values tick, and a key
 * that followed the order would re-measure on a tick (TH11).
 */
export function membershipKey(ids: string[]): string {
  return [...ids].sort().join(',');
}

const loadIds = new WeakMap<object, number>();
let nextLoadId = 0;

/** A load key for one result object: a new object is a new load, whatever it holds. */
export function objectLoadKey(o: object): string {
  let id = loadIds.get(o);
  if (id === undefined) {
    id = ++nextLoadId;
    loadIds.set(o, id);
  }
  return `load-${id}`;
}

/** A container's width, following resizes — what `useColumns` fits into. */
export function useWidth(): [(el: HTMLElement | null) => void, number] {
  const [width, setWidth] = useState(0);
  const observer = useRef<ResizeObserver | null>(null);
  const ref = useCallback((el: HTMLElement | null) => {
    observer.current?.disconnect();
    observer.current = null;
    if (!el) return;
    setWidth(el.clientWidth);
    if (typeof ResizeObserver === 'undefined') return;
    const ro = new ResizeObserver(() => setWidth(el.clientWidth));
    ro.observe(el);
    observer.current = ro;
  }, []);
  return [ref, width];
}

let fontsReady = false;
const fontListeners = new Set<() => void>();
if (typeof document !== 'undefined' && document.fonts?.ready) {
  void document.fonts.ready.then(() => {
    fontsReady = true;
    for (const l of fontListeners) l();
  });
}

export interface Columns {
  /** Final width per column key. The flexible column's entry is its minimum. */
  widths: Record<string, number>;
  /** Keys the operator has widened or narrowed. */
  overridden: string[];
  /** Sum of every fixed column plus the flexible column's minimum. */
  minTotal: number;
  /** A CSS grid track list: fixed px, the flexible column as minmax(min, 1fr). */
  template: string;
  /** The column being dragged right now, and its width — for the drag line. */
  drag: { key: string; px: number } | null;
  setWidth: (key: string, px: number, commit: boolean) => void;
  fit: (key: string) => void;
  reset: () => void;
}

/**
 * Fit measured widths into `available` px by taking width from the
 * squeezable columns, in proportion to what each can give. The operator's
 * own widths are never taken from; if the table still does not fit, it
 * scrolls sideways rather than cutting a column off. Pure, for the tests.
 */
export function squeezeColumns<T>(
  cols: ColumnSpec<T>[],
  widths: Record<string, number>,
  overridden: Record<string, number>,
  available: number,
): Record<string, number> {
  const total = cols.reduce((n, c) => n + (widths[c.key] ?? 0), 0);
  const excess = total - available;
  if (!(available > 0) || excess <= 0) return widths;
  const give = cols
    .filter((c) => c.squeeze && !c.flex && overridden[c.key] === undefined)
    .map((c) => ({ key: c.key, can: Math.max(0, (widths[c.key] ?? 0) - (c.squeezeMin ?? 96)) }))
    .filter((g) => g.can > 0);
  const pool = give.reduce((n, g) => n + g.can, 0);
  if (pool <= 0) return widths;
  const out = { ...widths };
  const take = Math.min(excess, pool);
  for (const g of give) out[g.key] = Math.floor(out[g.key] - (take * g.can) / pool);
  return out;
}

/**
 * Widths for one table. `loadKey` names the data load: widths are measured
 * when it changes (and once the bundled fonts are in), and never as rows tick.
 * `available` is the container's width, when the table should fit it: a
 * resize is not a tick, so following it does not break TH11.
 */
export function useColumns<T>(
  tableId: string,
  cols: ColumnSpec<T>[],
  rows: T[],
  loadKey: string,
  available = 0,
): Columns {
  const [measured, setMeasured] = useState<Record<string, number>>({});
  const [overrides, setOverrides] = useState<Record<string, number>>(() => loadOverrides(tableId));
  const [fontEpoch, setFontEpoch] = useState(fontsReady ? 1 : 0);
  const [drag, setDrag] = useState<{ key: string; px: number } | null>(null);
  const rowsRef = useRef(rows);
  const colsRef = useRef(cols);
  // Kept current for the measuring effect and the callbacks, which must not
  // re-run (and re-measure) just because a row ticked.
  useEffect(() => {
    rowsRef.current = rows;
    colsRef.current = cols;
  });
  // Measured against whichever load first had rows: an empty first render
  // must not freeze a header-only layout.
  const hasRows = rows.length > 0;

  useEffect(() => {
    if (fontsReady) return;
    const l = () => setFontEpoch((e) => e + 1);
    fontListeners.add(l);
    return () => {
      fontListeners.delete(l);
    };
  }, []);

  useEffect(() => {
    setMeasured(measureColumns(colsRef.current, rowsRef.current));
  }, [loadKey, hasRows, fontEpoch]);

  useEffect(() => {
    setOverrides(loadOverrides(tableId));
  }, [tableId]);

  const setWidth = useCallback(
    (key: string, px: number, commit: boolean) => {
      const w = Math.max(32, Math.round(px));
      setDrag(commit ? null : { key, px: w });
      setOverrides((prev) => {
        const next = { ...prev, [key]: w };
        if (commit) saveOverrides(tableId, next);
        return next;
      });
    },
    [tableId],
  );

  const fit = useCallback(
    (key: string) => {
      const col = colsRef.current.find((c) => c.key === key);
      if (!col) return;
      const w = fitColumn(col, rowsRef.current);
      setOverrides((prev) => {
        const next = { ...prev, [key]: w };
        saveOverrides(tableId, next);
        return next;
      });
    },
    [tableId],
  );

  const reset = useCallback(() => {
    saveOverrides(tableId, {});
    setOverrides({});
  }, [tableId]);

  return useMemo(() => {
    let widths: Record<string, number> = {};
    for (const col of cols) {
      const o = overrides[col.key];
      widths[col.key] = col.flex ? (o ?? col.flexMin ?? 160) : (o ?? measured[col.key] ?? (col.min ?? 64));
    }
    widths = squeezeColumns(cols, widths, overrides, available);
    let minTotal = 0;
    const tracks: string[] = [];
    for (const col of cols) {
      const w = widths[col.key];
      minTotal += w;
      tracks.push(col.flex ? `minmax(${w}px, 1fr)` : `${w}px`);
    }
    return {
      widths,
      overridden: Object.keys(overrides).filter((k) => cols.some((c) => c.key === k)),
      minTotal,
      template: tracks.join(' '),
      drag,
      setWidth,
      fit,
      reset,
    };
  }, [cols, measured, overrides, available, drag, setWidth, fit, reset]);
}
