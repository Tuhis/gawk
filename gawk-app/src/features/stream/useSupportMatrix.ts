// R12 (docs/17 L4): the UI-side probe matrix backing picker annotations.
// Probes at the pre-capture 4K upper bound (annotations are advisory —
// runtime acceleration is truth, Decision 13) and re-probes when the
// acceleration mode or codec pin changes. Scopes without WebCodecs (jsdom,
// exotic browsers) stay at null — options render unannotated.

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
