import { create } from 'zustand';

import {
  FRAMERATE_RUNGS,
  RESOLUTION_SELECTIONS,
  type FramerateRung,
  type ResolutionSelection,
} from '../media/ladder';

// R3 ladder selection for the broadcast page, persisted like transportStore
// so the choice survives reloads. Values are validated against the rung
// lists on load — a stale/garbage localStorage entry falls back to the
// default (the first list entry, unless an explicit fallback is passed).
// R4: the resolution axis is a ResolutionSelection whose default is 'auto';
// a previously persisted explicit rung (including 'native') keeps its exact
// meaning. Framerate defaults to 30 (an explicit fallback, not the first
// entry) to cap the fan-out.
const LS_RESOLUTION_RUNG = 'gawk.resolutionRung';
const LS_FRAMERATE_RUNG = 'gawk.framerateRung';

function loadRung<T extends string | number>(
  key: string,
  rungs: readonly T[],
  fallback: T = rungs[0],
): T {
  const raw = localStorage.getItem(key);
  if (raw === null) return fallback;
  const list = rungs as readonly (string | number)[];
  // Try the raw string ('auto', 'native') then the numeric form ('720').
  for (const candidate of [raw, Number(raw)]) {
    if (list.includes(candidate)) return candidate as T;
  }
  return fallback;
}

interface BroadcastSettingsState {
  resolutionSelection: ResolutionSelection;
  framerateRung: FramerateRung;

  setResolutionSelection: (selection: ResolutionSelection) => void;
  setFramerateRung: (rung: FramerateRung) => void;
}

export const useBroadcastSettingsStore = create<BroadcastSettingsState>((set) => ({
  resolutionSelection: loadRung(LS_RESOLUTION_RUNG, RESOLUTION_SELECTIONS),
  // Default 30fps caps the fan-out (halves datagram rate + viewer decode load);
  // a broadcaster can still pick 60/native in the picker.
  framerateRung: loadRung(LS_FRAMERATE_RUNG, FRAMERATE_RUNGS, 30),

  setResolutionSelection: (resolutionSelection) => {
    localStorage.setItem(LS_RESOLUTION_RUNG, String(resolutionSelection));
    set({ resolutionSelection });
  },
  setFramerateRung: (framerateRung) => {
    localStorage.setItem(LS_FRAMERATE_RUNG, String(framerateRung));
    set({ framerateRung });
  },
}));
