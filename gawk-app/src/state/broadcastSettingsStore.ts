import { create } from 'zustand';

import {
  FRAMERATE_RUNGS,
  RESOLUTION_RUNGS,
  type FramerateRung,
  type ResolutionRung,
} from '../media/ladder';

// R3 ladder selection for the broadcast page, persisted like transportStore
// so the choice survives reloads. Values are validated against the rung
// lists on load — a stale/garbage localStorage entry falls back to native.
const LS_RESOLUTION_RUNG = 'gawk.resolutionRung';
const LS_FRAMERATE_RUNG = 'gawk.framerateRung';

function loadRung<T extends string | number>(key: string, rungs: readonly T[]): T {
  const raw = localStorage.getItem(key);
  if (raw === null) return rungs[0];
  const parsed: string | number = raw === 'native' ? raw : Number(raw);
  return (rungs as readonly (string | number)[]).includes(parsed) ? (parsed as T) : rungs[0];
}

interface BroadcastSettingsState {
  resolutionRung: ResolutionRung;
  framerateRung: FramerateRung;

  setResolutionRung: (rung: ResolutionRung) => void;
  setFramerateRung: (rung: FramerateRung) => void;
}

export const useBroadcastSettingsStore = create<BroadcastSettingsState>((set) => ({
  resolutionRung: loadRung(LS_RESOLUTION_RUNG, RESOLUTION_RUNGS),
  framerateRung: loadRung(LS_FRAMERATE_RUNG, FRAMERATE_RUNGS),

  setResolutionRung: (resolutionRung) => {
    localStorage.setItem(LS_RESOLUTION_RUNG, String(resolutionRung));
    set({ resolutionRung });
  },
  setFramerateRung: (framerateRung) => {
    localStorage.setItem(LS_FRAMERATE_RUNG, String(framerateRung));
    set({ framerateRung });
  },
}));
