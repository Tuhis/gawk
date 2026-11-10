import { useEffect, useState, type RefObject } from 'react';

export interface ElementSize {
  width: number;
  height: number;
}

const ZERO: ElementSize = { width: 0, height: 0 };

// An element's content-box size, kept current by a ResizeObserver (R83,
// docs/87 D9). Where there is none — jsdom, which also has no layout — it
// falls back to the window's inner size, re-read on `resize`; tests that need
// a given size stub ResizeObserver. Off while `enabled` is false, so a mode
// that doesn't need the size costs nothing.
export function useElementSize(ref: RefObject<HTMLElement | null>, enabled = true): ElementSize {
  const [size, setSize] = useState<ElementSize>(ZERO);
  useEffect(() => {
    if (!enabled) return;
    const set = (width: number, height: number) =>
      setSize((prev) => (prev.width === width && prev.height === height ? prev : { width, height }));
    const el = ref.current;
    if (el && typeof ResizeObserver === 'function') {
      const ro = new ResizeObserver((entries) => {
        const box = entries[entries.length - 1]?.contentRect;
        if (box) set(box.width, box.height);
      });
      ro.observe(el);
      const r = el.getBoundingClientRect();
      if (r.width > 0 && r.height > 0) set(r.width, r.height);
      return () => ro.disconnect();
    }
    if (typeof window === 'undefined') return;
    const onResize = () => set(window.innerWidth, window.innerHeight);
    onResize();
    window.addEventListener('resize', onResize);
    return () => window.removeEventListener('resize', onResize);
  }, [ref, enabled]);
  return size;
}
