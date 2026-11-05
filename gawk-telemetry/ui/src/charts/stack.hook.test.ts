// @vitest-environment jsdom
import { act, renderHook } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import type { EChartsType } from './echarts.ts';
import { useStack } from './stack.ts';

// The stack's zoom relay (R36, docs/86 D4), against charts that record what
// they are told. `echarts.connect` relays an inside-zoom but not a slider's,
// so the stack relays it itself — and must stop on its own echo.

class FakeChart {
  handlers: Record<string, Array<(e?: unknown) => void>> = {};
  zr: Record<string, Array<() => void>> = {};
  zoom = { start: 0, end: 100 };
  dispatched: Array<{ start: number; end: number }> = [];

  on(name: string, fn: (e?: unknown) => void) {
    (this.handlers[name] ??= []).push(fn);
  }
  getZr() {
    return { on: (name: string, fn: () => void) => (this.zr[name] ??= []).push(fn) };
  }
  getOption() {
    return { dataZoom: [this.zoom] };
  }
  isDisposed() {
    return false;
  }
  dispatchAction(a: { start: number; end: number }) {
    this.dispatched.push({ start: a.start, end: a.end });
    this.zoom = { start: a.start, end: a.end };
    // A real chart reports the zoom it was given: the echo.
    for (const h of this.handlers.datazoom ?? []) h();
  }
  /** The operator zooming THIS chart. */
  userZoom(start: number, end: number) {
    this.zoom = { start, end };
    for (const h of this.handlers.datazoom ?? []) h();
  }
}

describe('useStack', () => {
  it('relays a zoom to every other member once, and remembers it', () => {
    const { result } = renderHook(() => useStack(0, 10_000));
    const a = new FakeChart();
    const b = new FakeChart();
    const c = new FakeChart();
    act(() => {
      for (const ch of [a, b, c]) result.current.register(ch as unknown as EChartsType);
    });

    act(() => a.userZoom(20, 60));

    // Each other member is told once; the echoes stop the relay.
    expect(b.dispatched).toEqual([{ start: 20, end: 60 }]);
    expect(c.dispatched).toEqual([{ start: 20, end: 60 }]);
    expect(a.dispatched).toEqual([]);
    expect(result.current.zoomOf()).toEqual({ start: 20, end: 60 });
    expect(result.current.viewFrom).toBe(2000);
    expect(result.current.viewTo).toBe(6000);
  });

  it('ignores an empty pointer report and clears only on leaving', () => {
    const { result } = renderHook(() => useStack(0, 10_000));
    const a = new FakeChart();
    act(() => result.current.register(a as unknown as EChartsType));
    // Members the mouse is not over report an empty axesInfo; that is not
    // "the cursor left".
    act(() => {
      for (const h of a.handlers.updateAxisPointer) h({ axesInfo: [] });
    });
    expect(result.current.cursorMs).toBeNull();
  });
});
