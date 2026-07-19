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

// R3 ladder selection for the broadcast page, persisted like transportStore
// so the choice survives reloads. Values are validated against the rung
// lists on load — a stale/garbage localStorage entry falls back to the
// default (the first list entry, unless an explicit fallback is passed).
// R4: the resolution axis is a ResolutionSelection whose default is 'auto';
// a previously persisted explicit rung (including 'native') keeps its exact
// meaning.
// R13 (docs/18): the framerate axis widens to FramerateSelection with
// default 'auto' (probe-resolved: 60 when hardware supports it, else 30 —
// consciously revising the old fixed-30 fan-out default); a previously
// persisted explicit fps rung keeps its exact meaning. New advanced axes:
// acceleration tri-state, bitrate override (bps), codec pin — all persisted
// and validated the same way.
const LS_RESOLUTION_RUNG = 'gawk.resolutionRung';
const LS_FRAMERATE_RUNG = 'gawk.framerateRung';
const LS_HW_PREFERENCE = 'gawk.hwPreference';
const LS_BITRATE_OVERRIDE = 'gawk.bitrateOverride';
const LS_CODEC_OVERRIDE = 'gawk.codecOverride';
// R15 (docs/20): "Enable audio (experimental)" — default off; applies on the
// next broadcast start (the one R13 live-apply exception, forced by
// getDisplayMedia).
const LS_AUDIO_ENABLED = 'gawk.audioEnabled';

const HW_PREFERENCES: readonly HwPreference[] = ['auto', 'hardware', 'software'];

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

function loadBitrateOverride(): number | null {
  const raw = localStorage.getItem(LS_BITRATE_OVERRIDE);
  if (raw === null) return null;
  const n = Number(raw);
  return Number.isFinite(n) && n > 0 ? clampBitrateOverride(n) : null;
}

function loadCodecOverride(): string | null {
  const raw = localStorage.getItem(LS_CODEC_OVERRIDE);
  return raw !== null && DEFAULT_CODEC_PREFERENCES.includes(raw) ? raw : null;
}

function setOrClear(key: string, value: string | null): void {
  if (value === null) localStorage.removeItem(key);
  else localStorage.setItem(key, value);
}

interface BroadcastSettingsState {
  resolutionSelection: ResolutionSelection;
  framerateSelection: FramerateSelection;
  hwPreference: HwPreference;
  bitrateOverride: number | null;
  codecOverride: string | null;
  audioEnabled: boolean;

  setResolutionSelection: (selection: ResolutionSelection) => void;
  setFramerateSelection: (selection: FramerateSelection) => void;
  setHwPreference: (preference: HwPreference) => void;
  setBitrateOverride: (bps: number | null) => void;
  setCodecOverride: (codec: string | null) => void;
  setAudioEnabled: (enabled: boolean) => void;
}

export const useBroadcastSettingsStore = create<BroadcastSettingsState>((set) => ({
  resolutionSelection: loadRung(LS_RESOLUTION_RUNG, RESOLUTION_SELECTIONS),
  framerateSelection: loadRung(LS_FRAMERATE_RUNG, FRAMERATE_SELECTIONS),
  hwPreference: loadRung(LS_HW_PREFERENCE, HW_PREFERENCES),
  bitrateOverride: loadBitrateOverride(),
  codecOverride: loadCodecOverride(),
  audioEnabled: localStorage.getItem(LS_AUDIO_ENABLED) === 'true',

  setResolutionSelection: (resolutionSelection) => {
    localStorage.setItem(LS_RESOLUTION_RUNG, String(resolutionSelection));
    set({ resolutionSelection });
  },
  setFramerateSelection: (framerateSelection) => {
    localStorage.setItem(LS_FRAMERATE_RUNG, String(framerateSelection));
    set({ framerateSelection });
  },
  setHwPreference: (hwPreference) => {
    localStorage.setItem(LS_HW_PREFERENCE, hwPreference);
    set({ hwPreference });
  },
  setBitrateOverride: (bps) => {
    const bitrateOverride = bps === null ? null : clampBitrateOverride(bps);
    setOrClear(LS_BITRATE_OVERRIDE, bitrateOverride === null ? null : String(bitrateOverride));
    set({ bitrateOverride });
  },
  setCodecOverride: (codec) => {
    const codecOverride = codec !== null && DEFAULT_CODEC_PREFERENCES.includes(codec) ? codec : null;
    setOrClear(LS_CODEC_OVERRIDE, codecOverride);
    set({ codecOverride });
  },
  setAudioEnabled: (audioEnabled) => {
    localStorage.setItem(LS_AUDIO_ENABLED, String(audioEnabled));
    set({ audioEnabled });
  },
}));

// The EncoderSettings snapshot the pipeline consumes (docs/18 L4): one
// place derives it so the screen's two call sites and the live-change
// handler can't drift.
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
