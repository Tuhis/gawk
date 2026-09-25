// The UI-side probe matrix backing picker annotations. Probes at the
// pre-capture 4K upper bound (annotations are advisory; runtime acceleration
// is the truth) and re-probes when the acceleration mode or codec pin changes.
// Scopes without WebCodecs (jsdom, exotic browsers) stay at null, so options
// render unannotated.

import { useEffect, useState } from 'react';

import {
  EncoderSupportProber,
  probeSupportMatrix,
  probeSupported,
  type SupportMatrix,
} from '../../media/probe';
import { DEFAULT_CODEC_PREFERENCES } from '../../media/types';
import { useBroadcastSettingsStore } from '../../state/broadcastSettingsStore';

// Module singleton: the prober memoizes per combo, so mode flips re-use
// everything already probed this page load.
const prober = new EncoderSupportProber();

export function useSupportMatrix(): SupportMatrix | null {
  const hwPreference = useBroadcastSettingsStore((s) => s.hwPreference);
  const codecOverride = useBroadcastSettingsStore((s) => s.codecOverride);
  const [matrix, setMatrix] = useState<SupportMatrix | null>(null);

  useEffect(() => {
    if (!probeSupported()) return;
    let cancelled = false;
    void probeSupportMatrix(prober, {
      codecs: codecOverride ? [codecOverride] : DEFAULT_CODEC_PREFERENCES,
      hwPreference,
    }).then((m) => {
      if (!cancelled) setMatrix(m);
    });
    return () => {
      cancelled = true;
    };
  }, [hwPreference, codecOverride]);

  return matrix;
}

// Per-codec matrices for the advanced codec-pin annotations: each codec gets
// its own single-codec matrix, so 'auto' axes resolve *per codec* (one codec
// may do HW at 60 where another only manages 30). This is the expensive
// probe set (every default codec × every combo), so it is **lazy**: `enabled`
// should be the settings panel's open state, and nothing is probed until the
// codec list can actually be seen. The whole set firing at surface load can
// run a tab out of memory; the prober's slot pool bounds that, but not
// probing at all is better. Probed combos stay memoized in the prober, so
// reopening the panel is free.
export function useCodecMatrices(enabled: boolean): Map<string, SupportMatrix> | null {
  const hwPreference = useBroadcastSettingsStore((s) => s.hwPreference);
  const [matrices, setMatrices] = useState<Map<string, SupportMatrix> | null>(null);

  useEffect(() => {
    if (!enabled || !probeSupported()) return;
    let cancelled = false;
    void Promise.all(
      DEFAULT_CODEC_PREFERENCES.map(
        async (codec) =>
          [codec, await probeSupportMatrix(prober, { codecs: [codec], hwPreference })] as const,
      ),
    ).then((entries) => {
      if (!cancelled) setMatrices(new Map(entries));
    });
    return () => {
      cancelled = true;
    };
  }, [enabled, hwPreference]);

  return matrices;
}
