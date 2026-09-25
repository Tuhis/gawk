import { create } from 'zustand';

import {
  FRAMERATE_SELECTIONS,
  RESOLUTION_SELECTIONS,
  clampBitrateOverride,
  type FramerateSelection,
  type ResolutionSelection,
} from '../media/ladder';
import type { HwPreference } from '../media/probe';
import { DEFAULT_CODEC_PREFERENCES } from '../media/types';
import { readStored, writeStored } from '../lib/storage';

// The broadcaster's encoder choices, persisted so they survive reloads.
// Stored values are validated on load; anything stale or unrecognised falls
// back to the first entry of its list ('auto' for the ladder axes).
const LS_RESOLUTION_RUNG = 'gawk.resolutionRung';
const LS_FRAMERATE_RUNG = 'gawk.framerateRung';
const LS_HW_PREFERENCE = 'gawk.hwPreference';
const LS_BITRATE_OVERRIDE = 'gawk.bitrateOverride';
const LS_CODEC_OVERRIDE = 'gawk.codecOverride';

const HW_PREFERENCES: readonly HwPreference[] = ['auto', 'hardware', 'software'];

function loadRung<T extends string | number>(key: string, rungs: readonly T[]): T {
  const raw = readStored(key);
  if (raw === null) return rungs[0];
  const list = rungs as readonly (string | number)[];
  // Try the raw string ('auto', 'native') then the numeric form ('720').
  for (const candidate of [raw, Number(raw)]) {
    if (list.includes(candidate)) return candidate as T;
  }
  return rungs[0];
}

function loadBitrateOverride(): number | null {
  const raw = readStored(LS_BITRATE_OVERRIDE);
  if (raw === null) return null;
  const n = Number(raw);
  return Number.isFinite(n) && n > 0 ? clampBitrateOverride(n) : null;
}

function loadCodecOverride(): string | null {
  const raw = readStored(LS_CODEC_OVERRIDE);
  return raw !== null && DEFAULT_CODEC_PREFERENCES.includes(raw) ? raw : null;
}

interface BroadcastSettingsState {
  resolutionSelection: ResolutionSelection;
  framerateSelection: FramerateSelection;
  hwPreference: HwPreference;
  bitrateOverride: number | null;
  codecOverride: string | null;

  setResolutionSelection: (selection: ResolutionSelection) => void;
  setFramerateSelection: (selection: FramerateSelection) => void;
  setHwPreference: (preference: HwPreference) => void;
  setBitrateOverride: (bps: number | null) => void;
  setCodecOverride: (codec: string | null) => void;
}

export const useBroadcastSettingsStore = create<BroadcastSettingsState>((set) => ({
  resolutionSelection: loadRung(LS_RESOLUTION_RUNG, RESOLUTION_SELECTIONS),
  framerateSelection: loadRung(LS_FRAMERATE_RUNG, FRAMERATE_SELECTIONS),
  hwPreference: loadRung(LS_HW_PREFERENCE, HW_PREFERENCES),
  bitrateOverride: loadBitrateOverride(),
  codecOverride: loadCodecOverride(),

  setResolutionSelection: (resolutionSelection) => {
    writeStored(LS_RESOLUTION_RUNG, String(resolutionSelection));
    set({ resolutionSelection });
  },
  setFramerateSelection: (framerateSelection) => {
    writeStored(LS_FRAMERATE_RUNG, String(framerateSelection));
    set({ framerateSelection });
  },
  setHwPreference: (hwPreference) => {
    writeStored(LS_HW_PREFERENCE, hwPreference);
    set({ hwPreference });
  },
  setBitrateOverride: (bps) => {
    const bitrateOverride = bps === null ? null : clampBitrateOverride(bps);
    writeStored(LS_BITRATE_OVERRIDE, bitrateOverride === null ? null : String(bitrateOverride));
    set({ bitrateOverride });
  },
  setCodecOverride: (codec) => {
    const codecOverride = codec !== null && DEFAULT_CODEC_PREFERENCES.includes(codec) ? codec : null;
    writeStored(LS_CODEC_OVERRIDE, codecOverride);
    set({ codecOverride });
  },
}));

// The EncoderSettings snapshot the pipeline consumes, derived in one place so
// its callers can't drift.
export function encoderSettingsFromStore(): {
  hwPreference: HwPreference;
  bitrateOverride: number | null;
  codecOverride: string | null;
} {
  const s = useBroadcastSettingsStore.getState();
  return {
    hwPreference: s.hwPreference,
    bitrateOverride: s.bitrateOverride,
    codecOverride: s.codecOverride,
  };
}
