// R83 (docs/87): the Fit room layout. A pure function from the tiles' stream
// shapes and the stage's size to one rectangle per tile, so that as much of
// the stage as the shapes allow is video.
//
// Candidates (D3), in two families:
//   rows    — order-preserving groups of consecutive tiles laid out as rows
//             (or, transposed, as columns), each sized "justified" (a row's
//             tiles share a height and fill the width, then the block scales
//             to fit the height) or "balanced" (each row capped at an equal
//             share of the height). Every split up to EXHAUSTIVE_MAX tiles,
//             the even splits above it.
//   grid    — Grid's own geometry for every column count: equal cells, each
//             tile the video's rectangle centred in its cell. It contains
//             today's Grid, so Fit never scores below it.
// The score is Σ ln(tile area) (D4); the block is centred on the stage (D6).

export interface Rect {
  x: number;
  y: number;
  w: number;
  h: number;
}

export type Arrangement =
  | { family: 'rows'; transpose: boolean; groups: number[]; sizing: 'justified' | 'balanced' }
  | { family: 'grid'; cols: number };

export interface FitResult {
  rects: Rect[];
  arrangement: Arrangement | null;
  score: number;
  // What the arrangement was chosen for: D5 keeps it only while these match.
  aspects: number[];
}

export interface FitOptions {
  gap?: number;
  // The last result; kept when only the stage size changed and it is within
  // STICKINESS of the best (D5).
  previous?: FitResult | null;
}

export const DEFAULT_ASPECT = 16 / 9;
export const MIN_ASPECT = 1 / 4;
export const MAX_ASPECT = 4;
export const DEFAULT_GAP = 2;
// Every order-preserving split is tried up to this many tiles (2^(n−1)).
export const EXHAUSTIVE_MAX = 10;
// D5: a new best must beat the arrangement in use by more than 3 % in
// geometric-mean tile area.
export const STICKINESS = Math.log(1.03);

const EMPTY: FitResult = { rects: [], arrangement: null, score: -Infinity, aspects: [] };

// docs/87 D7: a tile's shape is updated only when it moves by more than this.
export const ASPECT_DEADBAND = 0.02;

export function aspectChanged(current: number | undefined, next: number): boolean {
  return current === undefined || Math.abs(next / current - 1) > ASPECT_DEADBAND;
}

export function clampAspect(a: number): number {
  if (!Number.isFinite(a) || a <= 0) return DEFAULT_ASPECT;
  return Math.min(MAX_ASPECT, Math.max(MIN_ASPECT, a));
}

export function scoreRects(rects: Rect[]): number {
  let s = 0;
  for (const r of rects) {
    const area = r.w * r.h;
    if (!(area > 0)) return -Infinity;
    s += Math.log(area);
  }
  return s;
}

export function coverage(rects: Rect[], width: number, height: number): number {
  if (width <= 0 || height <= 0) return 0;
  return rects.reduce((s, r) => s + r.w * r.h, 0) / (width * height);
}

// Today's Grid: ceil(√n) columns of equal cells, each video letterboxed in
// its cell (RoomScreen's CSS grid). The comparison baseline, and the grid
// family's geometry.
export function gridColumns(n: number): number {
  return Math.max(1, Math.ceil(Math.sqrt(n)));
}

function placeGrid(aspects: number[], width: number, height: number, gap: number, cols: number): Rect[] | null {
  const n = aspects.length;
  const rows = Math.ceil(n / cols);
  const cw = (width - (cols - 1) * gap) / cols;
  const ch = (height - (rows - 1) * gap) / rows;
  if (cw <= 0 || ch <= 0) return null;
  return aspects.map((a, i) => {
    const col = i % cols;
    const row = Math.floor(i / cols);
    const w = cw / ch > a ? ch * a : cw;
    const h = cw / ch > a ? ch : cw / a;
    const x = col * (cw + gap) + (cw - w) / 2;
    const y = row * (ch + gap) + (ch - h) / 2;
    return { x, y, w, h };
  });
}

export function gridLayout(aspects: number[], width: number, height: number, gap = DEFAULT_GAP): Rect[] {
  if (aspects.length === 0) return [];
  return placeGrid(aspects.map(clampAspect), width, height, gap, gridColumns(aspects.length)) ?? [];
}

// Row heights for rows (or, transposed, columns) of consecutive tiles, or
// null when the stage can't hold them. `span[r]` is the sum of the row's
// shapes in the (possibly transposed) frame.
function rowHeights(
  spans: number[],
  groups: number[],
  W: number,
  H: number,
  gap: number,
  sizing: 'justified' | 'balanced',
): number[] | null {
  const R = groups.length;
  const avail = H - (R - 1) * gap;
  if (avail <= 0) return null;
  const heights: number[] = new Array(R);
  let sum = 0;
  for (let r = 0; r < R; r++) {
    const free = W - (groups[r] - 1) * gap;
    if (free <= 0) return null;
    heights[r] = free / spans[r];
    sum += heights[r];
  }
  if (sizing === 'justified') {
    const scale = Math.min(1, avail / sum);
    for (let r = 0; r < R; r++) heights[r] *= scale;
  } else {
    const budget = avail / R;
    for (let r = 0; r < R; r++) heights[r] = Math.min(heights[r], budget);
  }
  return heights;
}

// Rows (or, transposed, columns) of consecutive tiles.
function placeRows(
  aspects: number[],
  width: number,
  height: number,
  gap: number,
  groups: number[],
  transpose: boolean,
  sizing: 'justified' | 'balanced',
): Rect[] | null {
  // In the transposed frame a "row" is a column: swap the stage and invert
  // the shapes, then swap each rectangle back.
  const W = transpose ? height : width;
  const H = transpose ? width : height;
  const A = transpose ? aspects.map((a) => 1 / a) : aspects;
  const spans: number[] = [];
  let start = 0;
  for (const m of groups) {
    let span = 0;
    for (let i = start; i < start + m; i++) span += A[i];
    spans.push(span);
    start += m;
  }
  const rowH = rowHeights(spans, groups, W, H, gap, sizing);
  if (!rowH) return null;
  const total = rowH.reduce((s, h) => s + h, 0) + (groups.length - 1) * gap;
  const rects: Rect[] = [];
  let y = (H - total) / 2;
  start = 0;
  groups.forEach((m, r) => {
    const h = rowH[r];
    let x = (W - (spans[r] * h + (m - 1) * gap)) / 2;
    for (let i = start; i < start + m; i++) {
      const w = A[i] * h;
      rects.push(transpose ? { x: y, y: x, w: h, h: w } : { x, y, w, h });
      x += w + gap;
    }
    y += h + gap;
    start += m;
  });
  return rects;
}

// One arrangement on a given stage (exported for the D5 tests).
export function placeArrangement(aspects: number[], width: number, height: number, gap: number, arr: Arrangement): Rect[] | null {
  return arr.family === 'grid'
    ? placeGrid(aspects, width, height, gap, arr.cols)
    : placeRows(aspects, width, height, gap, arr.groups, arr.transpose, arr.sizing);
}

// Group sizes for every order-preserving split of n tiles (n ≤ EXHAUSTIVE_MAX),
// or only the even splits into k groups for each k above it.
function splits(n: number): number[][] {
  const out: number[][] = [];
  if (n <= EXHAUSTIVE_MAX) {
    for (let mask = 0; mask < 1 << (n - 1); mask++) {
      const groups: number[] = [];
      let run = 1;
      for (let i = 1; i < n; i++) {
        if (mask & (1 << (i - 1))) {
          groups.push(run);
          run = 1;
        } else {
          run++;
        }
      }
      groups.push(run);
      out.push(groups);
    }
    return out;
  }
  for (let k = 1; k <= n; k++) {
    const base = Math.floor(n / k);
    const extra = n % k;
    out.push(Array.from({ length: k }, (_, i) => base + (i < extra ? 1 : 0)));
  }
  return out;
}

// The rows family scored without building rectangles: a tile's area is
// A·h² in either frame, so Σ ln(area) = Σ ln A + Σ_r m_r · 2 ln h_r.
function bestRows(aspects: number[], width: number, height: number, gap: number): { arrangement: Arrangement; score: number } | null {
  const n = aspects.length;
  let best: { arrangement: Arrangement; score: number } | null = null;
  const all = splits(n);
  for (const transpose of [false, true]) {
    const W = transpose ? height : width;
    const H = transpose ? width : height;
    const A = transpose ? aspects.map((a) => 1 / a) : aspects;
    const prefix = [0];
    let lnSum = 0;
    for (const a of A) {
      prefix.push(prefix[prefix.length - 1] + a);
      lnSum += Math.log(a);
    }
    for (const groups of all) {
      const spans: number[] = [];
      let start = 0;
      for (const m of groups) {
        spans.push(prefix[start + m] - prefix[start]);
        start += m;
      }
      for (const sizing of ['justified', 'balanced'] as const) {
        const heights = rowHeights(spans, groups, W, H, gap, sizing);
        if (!heights) continue;
        let score = lnSum;
        for (let r = 0; r < groups.length; r++) score += groups[r] * 2 * Math.log(heights[r]);
        if (!best || score > best.score) best = { arrangement: { family: 'rows', transpose, groups, sizing }, score };
      }
    }
  }
  return best;
}

function sameAspects(a: number[], b: number[]): boolean {
  return a.length === b.length && a.every((v, i) => v === b[i]);
}

export function fitLayout(rawAspects: number[], width: number, height: number, opts: FitOptions = {}): FitResult {
  const gap = opts.gap ?? DEFAULT_GAP;
  const n = rawAspects.length;
  if (n === 0 || !(width > 0) || !(height > 0)) return EMPTY;
  const aspects = rawAspects.map(clampAspect);

  let best: FitResult | null = null;
  const rows = bestRows(aspects, width, height, gap);
  if (rows) {
    const rects = placeArrangement(aspects, width, height, gap, rows.arrangement);
    if (rects) best = { rects, arrangement: rows.arrangement, score: scoreRects(rects), aspects };
  }
  for (let cols = 1; cols <= n; cols++) {
    const arrangement: Arrangement = { family: 'grid', cols };
    const rects = placeGrid(aspects, width, height, gap, cols);
    if (!rects) continue;
    const score = scoreRects(rects);
    if (!best || score > best.score) best = { rects, arrangement, score, aspects };
  }
  if (!best || best.score === -Infinity) return { ...EMPTY, aspects };

  // D5: same tiles and shapes, only the stage changed — keep the arrangement
  // in use unless the best beats it by more than STICKINESS per tile, or it
  // has fallen below today's Grid on this stage.
  const prev = opts.previous;
  if (prev?.arrangement && sameAspects(prev.aspects, aspects)) {
    const rects = placeArrangement(aspects, width, height, gap, prev.arrangement);
    if (rects) {
      const score = scoreRects(rects);
      const floor = scoreRects(placeGrid(aspects, width, height, gap, gridColumns(n)) ?? []);
      if (score >= best.score - STICKINESS * n && score >= floor) {
        return { rects, arrangement: prev.arrangement, score, aspects };
      }
    }
  }
  return best;
}
