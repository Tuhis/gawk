import { create } from 'zustand';

/**
 * Where a chart stack's crosshair LAST was (R36): the instant a new note pins
 * to. Moving the mouse from the charts down to the note box must not lose the
 * moment being described, and only the note box subscribes, so the page does
 * not re-render on every mouse move.
 */
interface PinState {
  atMs: number | null;
  set: (atMs: number | null) => void;
}

export const usePinStore = create<PinState>((set) => ({
  atMs: null,
  set: (atMs) => set({ atMs }),
}));
