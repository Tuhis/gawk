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
import { DEFAULT_DELIVERY, DELIVERIES, type Delivery } from '../transport/carrier-sender';

// The broadcaster's encoder choices, persisted so they survive reloads.
// Stored values are validated on load; anything stale or unrecognised falls
// back to the first entry of its list ('auto' for the ladder axes).
const LS_RESOLUTION_RUNG = 'gawk.resolutionRung';
const LS_FRAMERATE_RUNG = 'gawk.framerateRung';
const LS_HW_PREFERENCE = 'gawk.hwPreference';
const LS_BITRATE_OVERRIDE = 'gawk.bitrateOverride';
const LS_CODEC_OVERRIDE = 'gawk.codecOverride';
// docs/88 D6: the Delivery setting, under the native broadcast.json's name.
const LS_UPLINK_DELIVERY = 'gawk.uplinkDelivery';

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

// A stored Delivery word, or null (blank: follows DEFAULT_DELIVERY).
function loadDelivery(): Delivery | null {
  const raw = readStored(LS_UPLINK_DELIVERY);
  return raw !== null && (DELIVERIES as readonly string[]).includes(raw) ? (raw as Delivery) : null;
}

interface BroadcastSettingsState {
  resolutionSelection: ResolutionSelection;
  framerateSelection: FramerateSelection;
  hwPreference: HwPreference;
  bitrateOverride: number | null;
  codecOverride: string | null;
  // null = blank: the delivery follows DEFAULT_DELIVERY, so a change of the
  // default reaches everyone who never chose (docs/88 D6, as natively).
  uplinkDelivery: Delivery | null;

  setResolutionSelection: (selection: ResolutionSelection) => void;
  setFramerateSelection: (selection: FramerateSelection) => void;
  setHwPreference: (preference: HwPreference) => void;
  setBitrateOverride: (bps: number | null) => void;
  setCodecOverride: (codec: string | null) => void;
  setUplinkDelivery: (delivery: Delivery) => void;
}

export const useBroadcastSettingsStore = create<BroadcastSettingsState>((set) => ({
  resolutionSelection: loadRung(LS_RESOLUTION_RUNG, RESOLUTION_SELECTIONS),
  framerateSelection: loadRung(LS_FRAMERATE_RUNG, FRAMERATE_SELECTIONS),
  hwPreference: loadRung(LS_HW_PREFERENCE, HW_PREFERENCES),
  bitrateOverride: loadBitrateOverride(),
  codecOverride: loadCodecOverride(),
  uplinkDelivery: loadDelivery(),

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
  // A word is stored only once the broadcaster picks something other than
  // what a blank shows (the native Quality page's rule).
  setUplinkDelivery: (delivery) =>
    set((s) => {
      if (s.uplinkDelivery === null && delivery === DEFAULT_DELIVERY) return {};
      writeStored(LS_UPLINK_DELIVERY, delivery);
      return { uplinkDelivery: delivery };
    }),
}));

// The delivery the pipeline is asked for: the stored word, or the default.
export function deliveryFromStore(): Delivery {
  return useBroadcastSettingsStore.getState().uplinkDelivery ?? DEFAULT_DELIVERY;
}

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
