// @vitest-environment jsdom
import { act, renderHook } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';

import { CELL_PAD, measureColumns, membershipKey, objectLoadKey, squeezeColumns, useColumns, type ColumnSpec } from './columns.ts';

// R36 item 1 (docs/86 TU2). jsdom has no canvas, so widths come from the
// per-character estimate — the RULES are what is under test here: what is
// measured, when, what bounds it, and what an operator's override does.

interface Row {
  name: string;
  n: number;
}

const COLS: ColumnSpec<Row>[] = [
  { key: 'name', label: 'name', text: (r) => r.name },
  { key: 'n', label: 'n', text: (r) => String(r.n), samples: ['99999'], min: 40, max: 120 },
  { key: 'note', label: 'note', flex: true, flexMin: 180 },
];

afterEach(() => localStorage.clear());

describe('measureColumns', () => {
  it('fits the widest value, never less than its header', () => {
    const narrow = measureColumns(COLS, [{ name: 'a', n: 1 }]);
    const wide = measureColumns(COLS, [{ name: 'a much longer name than the header', n: 1 }]);
    expect(wide.name).toBeGreaterThan(narrow.name);
    // An empty table still fits its header ("DURA…" was R31's failure).
    expect(measureColumns(COLS, []).name).toBeGreaterThanOrEqual(CELL_PAD + 4);
  });

  it('reserves the widest form a ticking value can take', () => {
    const w = measureColumns(COLS, [{ name: 'x', n: 1 }]);
    // '99999' is the sample, so a 1 and a 99999 get the same width.
    expect(w.n).toBe(measureColumns(COLS, [{ name: 'x', n: 99999 }]).n);
  });

  it('applies the bounds to a measured width', () => {
    const tiny: ColumnSpec<Row>[] = [{ key: 'n', label: 'n', text: () => '', min: 64 }];
    expect(measureColumns(tiny, [{ name: '', n: 0 }]).n).toBe(64);
    const huge: ColumnSpec<Row>[] = [{ key: 'name', label: 'name', text: (r) => r.name, max: 100 }];
    expect(measureColumns(huge, [{ name: 'x'.repeat(200), n: 0 }]).name).toBe(100);
  });
});

describe('load keys', () => {
  // A live table re-sorts by severity as values tick; a key that followed
  // the order re-measured on a tick, which TH11 forbids (round-1 review).
  it('treats the same sessions in another order as the same load', () => {
    expect(membershipKey(['b', 'a', 'c'])).toBe(membershipKey(['c', 'b', 'a']));
    expect(membershipKey(['a', 'b'])).not.toBe(membershipKey(['a', 'b', 'c']));
  });

  it('gives every result object its own load, whatever it holds', () => {
    // "1 row in 23 ms" and "12 rows in 3 ms" collided under the old key.
    const one = { rows: 1, ms: 23 };
    const other = { rows: 12, ms: 3 };
    expect(objectLoadKey(one)).not.toBe(objectLoadKey(other));
    expect(objectLoadKey(one)).toBe(objectLoadKey(one));
  });
});

describe('squeezeColumns', () => {
  const cols: ColumnSpec<Row>[] = [
    { key: 'a', label: 'a' },
    { key: 'client', label: 'client', squeeze: true, squeezeMin: 100 },
    { key: 'app', label: 'app', squeeze: true, squeezeMin: 50 },
    { key: 'v', label: 'v', flex: true },
  ];
  const widths = { a: 100, client: 300, app: 100, v: 200 };

  it('leaves a table that fits alone', () => {
    expect(squeezeColumns(cols, widths, {}, 900)).toEqual(widths);
  });

  it('takes the overflow from the free-text columns, in proportion, never below their floor', () => {
    const out = squeezeColumns(cols, widths, {}, 600);
    // 100 over: client can give 200, app 50, so 80 and 20.
    expect(out.client).toBe(220);
    expect(out.app).toBe(80);
    expect(out.a).toBe(100);
    expect(out.v).toBe(200);
    const tight = squeezeColumns(cols, widths, {}, 100);
    expect(tight.client).toBe(100);
    expect(tight.app).toBe(50);
  });

  it('never takes from a width the operator set', () => {
    const out = squeezeColumns(cols, widths, { client: 300 }, 600);
    expect(out.client).toBe(300);
    expect(out.app).toBe(50);
  });
});

describe('useColumns', () => {
  const rows: Row[] = [{ name: 'alpha', n: 3 }];

  it('gives the free-text column the remainder, and only that one', () => {
    const { result } = renderHook(() => useColumns('t1', COLS, rows, 'load-1'));
    const tracks = result.current.template.split(' ');
    expect(tracks[0]).toMatch(/^\d+px$/);
    expect(result.current.template).toMatch(/minmax\(180px, 1fr\)$/);
    expect(result.current.minTotal).toBe(result.current.widths.name + result.current.widths.n + 180);
  });

  it('does not re-measure as values tick, only on a new load', () => {
    const { result, rerender } = renderHook(({ r, k }) => useColumns('t2', COLS, r, k), {
      initialProps: { r: rows, k: 'load-1' },
    });
    const before = result.current.widths.name;
    rerender({ r: [{ name: 'a value far wider than alpha ever was', n: 3 }], k: 'load-1' });
    expect(result.current.widths.name).toBe(before);
    rerender({ r: [{ name: 'a value far wider than alpha ever was', n: 3 }], k: 'load-2' });
    expect(result.current.widths.name).toBeGreaterThan(before);
  });

  it('keeps an override across a reload, and reset drops it', () => {
    const first = renderHook(() => useColumns('t3', COLS, rows, 'load-1'));
    act(() => first.result.current.setWidth('name', 333, true));
    expect(first.result.current.widths.name).toBe(333);
    expect(first.result.current.overridden).toEqual(['name']);
    first.unmount();

    const second = renderHook(() => useColumns('t3', COLS, rows, 'load-1'));
    expect(second.result.current.widths.name).toBe(333);
    act(() => second.result.current.reset());
    expect(second.result.current.overridden).toEqual([]);
    expect(localStorage.getItem('gawk-telemetry.widths.t3')).toBeNull();
  });

  it('shows a drag without saving it, and saves only on release', () => {
    const { result } = renderHook(() => useColumns('t4', COLS, rows, 'load-1'));
    act(() => result.current.setWidth('name', 210, false));
    expect(result.current.drag).toEqual({ key: 'name', px: 210 });
    expect(localStorage.getItem('gawk-telemetry.widths.t4')).toBeNull();
    act(() => result.current.setWidth('name', 220, true));
    expect(result.current.drag).toBeNull();
    expect(JSON.parse(localStorage.getItem('gawk-telemetry.widths.t4')!)).toEqual({ name: 220 });
  });

  it('fits a column to its whole content on double-click, past the measured cap', () => {
    const long: Row[] = [{ name: 'x', n: 123456789012345 }];
    const { result } = renderHook(() => useColumns('t5', COLS, long, 'load-1'));
    expect(result.current.widths.n).toBe(120);
    act(() => result.current.fit('n'));
    expect(result.current.widths.n).toBeGreaterThan(120);
  });

  it('ignores a stored width that is not a sane number', () => {
    localStorage.setItem('gawk-telemetry.widths.t6', JSON.stringify({ name: 'wide', n: 1e9 }));
    const { result } = renderHook(() => useColumns('t6', COLS, rows, 'load-1'));
    expect(result.current.overridden).toEqual([]);
  });
});
