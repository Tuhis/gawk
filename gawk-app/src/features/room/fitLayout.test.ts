import { describe, expect, it } from 'vitest';
import {
  DEFAULT_ASPECT,
  DEFAULT_GAP,
  STICKINESS,
  placeArrangement,
  MAX_ASPECT,
  MIN_ASPECT,
  clampAspect,
  coverage,
  fitLayout,
  gridLayout,
  scoreRects,
  type Rect,
} from './fitLayout';

const R = {
  '16:9': 16 / 9,
  '21:9': 64 / 27,
  '32:9': 32 / 9,
  '16:10': 1.6,
  '4:3': 4 / 3,
  phone: 9 / 19.5,
} as const;
type Shape = keyof typeof R;
const shapes = (...s: Shape[]) => s.map((k) => R[k]);

const STAGES: [string, number, number][] = [
  ['1080p', 1920, 1080],
  ['panel', 1580, 1080],
  ['ultrawide', 2560, 1080],
  ['phone', 390, 844],
];

function expectSane(rects: Rect[], aspects: number[], W: number, H: number) {
  const eps = 0.01;
  rects.forEach((r, i) => {
    expect(r.x).toBeGreaterThanOrEqual(-eps);
    expect(r.y).toBeGreaterThanOrEqual(-eps);
    expect(r.x + r.w).toBeLessThanOrEqual(W + eps);
    expect(r.y + r.h).toBeLessThanOrEqual(H + eps);
    // Each tile has its stream's shape: no letterbox inside the box.
    expect(Math.abs(r.w / r.h / aspects[i] - 1)).toBeLessThan(0.02);
  });
  for (let i = 0; i < rects.length; i++) {
    for (let j = i + 1; j < rects.length; j++) {
      const a = rects[i];
      const b = rects[j];
      const overlap = a.x < b.x + b.w - eps && b.x < a.x + a.w - eps && a.y < b.y + b.h - eps && b.y < a.y + a.h - eps;
      expect(overlap, `tiles ${i} and ${j} overlap`).toBe(false);
    }
  }
}

describe('fitLayout', () => {
  it('stacks two 32:9 streams on a 16:9 stage and fills it', () => {
    const as = shapes('32:9', '32:9');
    const { rects } = fitLayout(as, 1920, 1080);
    expectSane(rects, as, 1920, 1080);
    expect(coverage(rects, 1920, 1080)).toBeGreaterThanOrEqual(0.95);
    expect(rects[1].y).toBeGreaterThan(rects[0].y + rects[0].h - 1);
    // Grid puts them side by side: a quarter of the stage.
    expect(coverage(gridLayout(as, 1920, 1080), 1920, 1080)).toBeLessThan(0.26);
  });

  it('keeps reading order', () => {
    const as = shapes('32:9', '16:9', '21:9', '16:10', '16:9');
    const { rects } = fitLayout(as, 1920, 1080);
    for (let i = 1; i < rects.length; i++) {
      const a = rects[i - 1];
      const b = rects[i];
      // Next in a row (to the right) or a later row/column.
      const later = b.y >= a.y + a.h - 0.01 || b.x >= a.x + a.w - 0.01;
      expect(later, `tile ${i} reads before ${i - 1}`).toBe(true);
    }
  });

  // docs/87 §12: coverage, Grid → Fit, ten mixes × four stages.
  const TABLE: [Shape[], number[], number[]][] = [
    [['32:9', '32:9'], [25, 21, 33, 6], [100, 82, 75, 26]],
    [['21:9', '21:9'], [37, 31, 50, 10], [66, 81, 50, 39]],
    [['16:9', '16:9'], [50, 41, 67, 13], [50, 61, 67, 52]],
    [['32:9', '16:9'], [37, 31, 50, 10], [75, 81, 56, 39]],
    [['16:9', '16:9', '16:9'], [75, 62, 56, 19], [75, 71, 89, 78]],
    [['32:9', '32:9', '16:9'], [50, 41, 52, 13], [83, 67, 82, 52]],
    [['16:9', 'phone'], [51, 52, 53, 31], [79, 65, 94, 79]],
    [['16:9', '16:9', '16:9', '16:9'], [100, 82, 75, 26], [100, 82, 75, 95]],
    [['32:9', '16:9', '21:9', '16:10', '16:9'], [48, 40, 64, 12], [91, 89, 85, 87]],
    [['16:9', '16:9', '16:9', 'phone'], [81, 69, 61, 44], [81, 94, 64, 77]],
  ];

  it("reproduces §12's table: above Grid in 32 cells, equal in 8", () => {
    let above = 0;
    let equal = 0;
    for (const [mix, grid, fit] of TABLE) {
      const as = shapes(...mix);
      STAGES.forEach(([, W, H], s) => {
        const g = Math.round(100 * coverage(gridLayout(as, W, H), W, H));
        const { rects } = fitLayout(as, W, H);
        expectSane(rects, as, W, H);
        const f = Math.round(100 * coverage(rects, W, H));
        expect(g, `grid ${mix.join('+')} @ ${W}×${H}`).toBe(grid[s]);
        expect(f, `fit ${mix.join('+')} @ ${W}×${H}`).toBe(fit[s]);
        if (f > g) above++;
        else if (f === g) equal++;
      });
    }
    expect([above, equal]).toEqual([32, 8]);
  });

  it('matches Grid for 2–4 plain 16:9 streams on a 16:9 stage', () => {
    for (let n = 2; n <= 4; n++) {
      const as = Array(n).fill(R['16:9']);
      expect(coverage(fitLayout(as, 1920, 1080).rects, 1920, 1080)).toBeCloseTo(coverage(gridLayout(as, 1920, 1080), 1920, 1080), 3);
    }
  });

  it('never scores below Grid, with or without a previous arrangement', () => {
    // Deterministic pseudo-random inputs.
    let seed = 7;
    const rnd = () => ((seed = (seed * 1103515245 + 12345) % 2 ** 31) / 2 ** 31);
    for (let t = 0; t < 2000; t++) {
      const n = 1 + Math.floor(rnd() * 14);
      const as = Array.from({ length: n }, () => 0.3 + rnd() * 3.5);
      const W = 300 + rnd() * 2600;
      const H = 300 + rnd() * 1400;
      const fit = fitLayout(as, W, H);
      expect(fit.score).toBeGreaterThanOrEqual(scoreRects(gridLayout(as, W, H)) - 1e-9);
      // D5: an arrangement kept from a nearby stage size never drops below
      // Grid either.
      const W2 = W * (0.85 + rnd() * 0.3);
      const H2 = H * (0.85 + rnd() * 0.3);
      const kept = fitLayout(as, W2, H2, { previous: fit });
      expect(kept.score).toBeGreaterThanOrEqual(scoreRects(gridLayout(as, W2, H2)) - 1e-9);
    }
  }, 30_000);

  it('lays a portrait stream in its own column beside a landscape one', () => {
    const as = shapes('16:9', 'phone');
    const { rects } = fitLayout(as, 1920, 1080);
    expectSane(rects, as, 1920, 1080);
    expect(rects[1].x).toBeGreaterThan(rects[0].x + rects[0].w - 1);
    expect(rects[1].h).toBeCloseTo(rects[0].h, 3);
  });

  it('keeps the arrangement on a small resize and drops it on a large one', () => {
    // 3× 16:9 on a wide stage: two arrangements score close together near
    // the boundary, so the previous one must hold within 3 %.
    const as = shapes('16:9', '16:9', '16:9');
    const first = fitLayout(as, 2200, 1080);
    // Find a nearby size where a fresh layout picks something else.
    let flipW = 0;
    for (let w = 2200; w <= 2800; w += 5) {
      if (JSON.stringify(fitLayout(as, w, 1080).arrangement) !== JSON.stringify(first.arrangement)) {
        flipW = w;
        break;
      }
    }
    expect(flipW).toBeGreaterThan(0);
    const kept = fitLayout(as, flipW, 1080, { previous: first });
    expect(kept.arrangement).toEqual(first.arrangement);
    expectSane(kept.rects, as, flipW, 1080);
    const dropped = fitLayout(as, 1080, 1920, { previous: first });
    expect(dropped.arrangement).not.toEqual(first.arrangement);
  });

  it('drops a kept arrangement that falls below Grid, for the fresh best (D5 floor)', () => {
    // A phone and a 16:9 stream: one justified row at 1580 px, still within
    // 3 % of the best at 1501 px but below Grid there.
    const as = shapes('phone', '16:9');
    const prev = fitLayout(as, 1580, 1080);
    const kept = placeArrangement(as, 1501, 1080, DEFAULT_GAP, prev.arrangement!)!;
    const fresh = fitLayout(as, 1501, 1080);
    const grid = scoreRects(gridLayout(as, 1501, 1080));
    // The premises: within the stickiness band, below the floor.
    expect(scoreRects(kept)).toBeGreaterThanOrEqual(fresh.score - STICKINESS * as.length);
    expect(scoreRects(kept)).toBeLessThan(grid);
    const next = fitLayout(as, 1501, 1080, { previous: prev });
    expect(next).toEqual(fresh);
    expect(next.score).toBeGreaterThanOrEqual(grid - 1e-9);
  });

  it('starts fresh when a tile joins or changes shape', () => {
    const as = shapes('16:9', '16:9', '16:9');
    const first = fitLayout(as, 2400, 1080);
    const joined = fitLayout([...as, R['16:9']], 2400, 1080, { previous: first });
    expect(joined).toEqual(fitLayout([...as, R['16:9']], 2400, 1080));
    const changed = fitLayout(shapes('32:9', '16:9', '16:9'), 2400, 1080, { previous: first });
    expect(changed).toEqual(fitLayout(shapes('32:9', '16:9', '16:9'), 2400, 1080));
  });

  // §1's bound is 5 ms. v8 coverage instrumentation (CI's `npm run
  // test:coverage`) slows the search several times over, so the bound is
  // 25 ms there: still far below what an exhaustive search at 40 tiles would
  // cost (2^39 splits), which is what this guards against.
  const INSTRUMENTED = process.env.npm_lifecycle_event === 'test:coverage';
  const BOUND_MS = INSTRUMENTED ? 25 : 5;

  it('lays out 10 tiles and a 40-stream wall fast', () => {
    for (const n of [10, 40]) {
      const as = Array.from({ length: n }, (_, i) => [R['16:9'], R['32:9'], R['4:3'], R.phone][i % 4]);
      fitLayout(as, 1920, 1080); // warm up
      // The best of several runs: a busy CI worker's stalls are not the
      // function's cost.
      let ms = Infinity;
      let rects: Rect[] = [];
      for (let run = 0; run < 7; run++) {
        const t0 = performance.now();
        rects = fitLayout(as, 1920, 1080).rects;
        ms = Math.min(ms, performance.now() - t0);
      }
      expect(rects).toHaveLength(n);
      expectSane(rects, as, 1920, 1080);
      expect(ms, `${n} tiles took ${ms.toFixed(2)} ms`).toBeLessThan(BOUND_MS);
    }
  });

  it('a 40-stream wall still covers at least what Grid does', () => {
    const as = Array(40).fill(R['16:9']);
    expect(coverage(fitLayout(as, 1920, 1080).rects, 1920, 1080)).toBeGreaterThanOrEqual(
      coverage(gridLayout(as, 1920, 1080), 1920, 1080) - 1e-9,
    );
  });

  it('returns nothing for no tiles or an empty stage', () => {
    expect(fitLayout([], 1920, 1080).rects).toEqual([]);
    expect(fitLayout([R['16:9']], 0, 1080).rects).toEqual([]);
    expect(fitLayout([R['16:9']], 1920, 0).rects).toEqual([]);
  });

  it('clamps degenerate shapes', () => {
    expect(clampAspect(0)).toBe(DEFAULT_ASPECT);
    expect(clampAspect(NaN)).toBe(DEFAULT_ASPECT);
    expect(clampAspect(100)).toBe(MAX_ASPECT);
    expect(clampAspect(0.01)).toBe(MIN_ASPECT);
    const { rects } = fitLayout([100, 0.01], 1920, 1080);
    expectSane(rects, [MAX_ASPECT, MIN_ASPECT], 1920, 1080);
  });

  it('one tile fills the stage at its own shape, centred', () => {
    const { rects } = fitLayout([R['4:3']], 1920, 1080);
    expect(rects[0].h).toBeCloseTo(1080);
    expect(rects[0].x).toBeCloseTo((1920 - 1440) / 2);
  });
});
