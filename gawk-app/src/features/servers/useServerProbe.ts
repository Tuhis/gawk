// Probe state for the picker's rows. Saved servers (the pinned default +
// custom entries) are probed once when the panel opens and on demand;
// directory offers are probed ON DEMAND ONLY, because opening the picker must
// not disclose the user's address to every third-party host an operator
// listed. No background probing exists anywhere: this hook lives inside the
// panel, so an idle landing page generates zero traffic.

import { useEffect, useRef, useState } from 'react';

import { probeRelay, type ProbeResult } from './probe';

export type RowProbeState = { state: 'idle' } | { state: 'probing' } | ProbeResult;

export interface ProbeTarget {
  key: string;
  url: string;
  certHashHex: string;
  // false ⇒ never probed without an explicit request (directory offers).
  auto: boolean;
}

export type ProbeFn = typeof probeRelay;

// A row's id survives an edit of its URL or cert hash, so a verdict belongs
// to the whole target, not the row key: results are stored per target and a
// probe of a superseded target can only ever write its own slot.
function targetId(t: ProbeTarget): string {
  return JSON.stringify([t.key, t.url, t.certHashHex]);
}

export function useServerProbe(
  targets: ProbeTarget[],
  probeFn: ProbeFn = probeRelay,
): { results: Record<string, RowProbeState>; probe: (key: string) => void } {
  const [byTarget, setByTarget] = useState<Record<string, RowProbeState>>({});
  const startedRef = useRef(new Set<string>());
  const targetsRef = useRef(targets);
  targetsRef.current = targets;
  const probeFnRef = useRef(probeFn);
  probeFnRef.current = probeFn;

  const start = (key: string) => {
    const target = targetsRef.current.find((t) => t.key === key);
    if (!target) return;
    const id = targetId(target);
    startedRef.current.add(id);
    setByTarget((r) => ({ ...r, [id]: { state: 'probing' } }));
    void probeFnRef.current(target.url, target.certHashHex).then((result) => {
      setByTarget((r) => ({ ...r, [id]: result }));
    });
  };

  // Auto targets probe once per panel lifetime; a target added later (a new
  // saved entry, or an edit to one) is picked up on the render that
  // introduces it.
  const autoIds = JSON.stringify(targets.filter((t) => t.auto).map(targetId));
  useEffect(() => {
    for (const t of targetsRef.current) {
      if (t.auto && !startedRef.current.has(targetId(t))) start(t.key);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps -- keyed on the auto set only
  }, [autoIds]);

  const results: Record<string, RowProbeState> = {};
  for (const t of targets) {
    const r = byTarget[targetId(t)];
    if (r) results[t.key] = r;
  }
  return { results, probe: start };
}
