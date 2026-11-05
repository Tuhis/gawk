import { describe, expect, it } from 'vitest';

import { readPalette } from './palette.ts';
import { buildStackOption, emptyStackOption } from './stackOption.ts';

// The option a stack member is drawn with (R36, docs/86 D4/D5).

const p = readPalette('dark');
const input = {
  series: [{ key: 'fps', label: 'fps', data: [[0, 30], [1000, null], [2000, 31]] as Array<[number, number | null]> }],
  bands: [{ fromMs: 0, toMs: 500, kind: 'veil' as const, label: 'not joined' }],
  marks: [{ atMs: 700, kind: 'note' as const, label: 'note: wifi' }],
  ticks: [1500],
  fromMs: 0,
  toMs: 2000,
};

type Opt = {
  dataZoom: Array<{ start?: number; end?: number }>;
  series: Array<{
    connectNulls: boolean;
    markArea?: { label: { show: boolean } };
    markLine?: { label: { show: boolean }; data: unknown[] };
  }>;
};

describe('buildStackOption', () => {
  // setOption replaces the option outright, so a rebuild for any reason —
  // a re-render, a live refresh, a theme switch — reset a zoomed member to
  // the whole range while its neighbours stayed zoomed (round-1 review).
  it('carries the stack’s current zoom into every rebuild', () => {
    const o = buildStackOption(p, input, { start: 20, end: 60 }) as unknown as Opt;
    expect(o.dataZoom[0]).toMatchObject({ start: 20, end: 60 });
    const e = emptyStackOption(p, 0, 2000, { start: 20, end: 60 }) as unknown as Opt;
    expect(e.dataZoom[0]).toMatchObject({ start: 20, end: 60 });
  });

  it('draws no text inside a lane, and keeps a gap a gap', () => {
    const o = buildStackOption(p, input) as unknown as Opt;
    const s = o.series[0];
    expect(s.connectNulls).toBe(false);
    expect(s.markArea?.label.show).toBe(false);
    expect(s.markLine?.label.show).toBe(false);
    // One note line plus one floor tick.
    expect(s.markLine?.data).toHaveLength(2);
  });
});
