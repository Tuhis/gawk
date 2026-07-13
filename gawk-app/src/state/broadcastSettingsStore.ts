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
// default (the first list entry). R4: the resolution axis is a
// ResolutionSelection whose default is 'auto'; a previously persisted
// explicit rung (including 'native') keeps its exact meaning.
const LS_RESOLUTION_RUNG = 'gawk.resolutionRung';
const LS_FRAMERATE_RUNG = 'gawk.framerateRung';

function loadRung<T extends string | number>(key: string, rungs: readonly T[]): T {
  const raw = localStorage.getItem(key);
  if (raw === null) return rungs[0];
  const list = rungs as readonly (string | number)[];
  // Try the raw string ('auto', 'native') then the numeric form ('720').
  for (const candidate of [raw, Number(raw)]) {
    if (list.includes(candidate)) return candidate as T;
  }
  return rungs[0];
}

interface BroadcastSettingsState {
  resolutionSelection: ResolutionSelection;
  framerateRung: FramerateRung;

  setResolutionSelection: (selection: ResolutionSelection) => void;
  setFramerateRung: (rung: FramerateRung) => void;
}

export const useBroadcastSettingsStore = create<BroadcastSettingsState>((set) => ({
  resolutionSelection: loadRung(LS_RESOLUTION_RUNG, RESOLUTION_SELECTIONS),
  framerateRung: loadRung(LS_FRAMERATE_RUNG, FRAMERATE_RUNGS),

  setResolutionSelection: (resolutionSelection) => {
    localStorage.setItem(LS_RESOLUTION_RUNG, String(resolutionSelection));
    set({ resolutionSelection });
  },
  setFramerateRung: (framerateRung) => {
    localStorage.setItem(LS_FRAMERATE_RUNG, String(framerateRung));
    set({ framerateRung });
  },
}));
