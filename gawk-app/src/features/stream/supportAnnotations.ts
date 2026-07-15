// R13 (docs/18 Decision 9): shared option-annotation logic for the quality
// pickers and the advanced codec pin. Options are annotated, never removed —
// an explicit software choice is allowed (R4's "explicit choices are
// honored"); only genuinely unsupported combos disable. The probe is
// advisory: the overlay's Encode mode row shows the runtime truth.

import {
  RESOLUTION_RUNGS,
  resolveAutoFps,
  type FramerateSelection,
  type ResolutionSelection,
} from '../../media/ladder';
import type { ProbeAcceleration, SupportMatrix } from '../../media/probe';

export function annotate(
  label: string,
  acceleration: ProbeAcceleration | null,
): { label: string; disabled: boolean } {
  if (acceleration === 'software') return { label: `${label} · software`, disabled: false };
  if (acceleration === 'unsupported') return { label: `${label} · unsupported`, disabled: true };
  return { label, disabled: false };
}

// The fps an option is annotated at: explicit rungs speak for themselves,
// 'auto' uses the matrix's framerate-first resolution, 'native' probes at
// the 60 upper bound (no matrix column for arbitrary refresh rates).
export function annotationFps(matrix: SupportMatrix, fpsSelection: FramerateSelection): number {
  if (fpsSelection === 'auto') return resolveAutoFps(matrix.get);
  return fpsSelection === 'native' ? 60 : fpsSelection;
}

// The best acceleration any rung offers at this fps — what auto resolution
// would pick, since it adapts the rung.
export function bestAtFps(matrix: SupportMatrix, fps: number): ProbeAcceleration {
  let best: ProbeAcceleration = 'unsupported';
  for (const rung of RESOLUTION_RUNGS) {
    const acc = matrix.get(rung, fps).acceleration;
    if (acc === 'hardware') return 'hardware';
    if (acc === 'software') best = 'software';
  }
  return best;
}

export function resolutionAcceleration(
  matrix: SupportMatrix | null | undefined,
  selection: ResolutionSelection,
  fpsSelection: FramerateSelection,
): ProbeAcceleration | null {
  if (!matrix || selection === 'auto') return null; // auto always works — it adapts
  return matrix.get(selection, annotationFps(matrix, fpsSelection)).acceleration;
}

export function framerateAcceleration(
  matrix: SupportMatrix | null | undefined,
  selection: FramerateSelection,
  resolutionSelection: ResolutionSelection,
): ProbeAcceleration | null {
  if (!matrix || selection === 'auto') return null;
  const fps = selection === 'native' ? 60 : selection;
  if (resolutionSelection !== 'auto') return matrix.get(resolutionSelection, fps).acceleration;
  return bestAtFps(matrix, fps);
}

// Codec-pin annotation against that codec's own matrix: what pinning this
// codec would get at the current resolution/fps selections. 'auto' axes
// resolve per codec (a codec may do HW at 60 where another only manages 30).
export function codecAcceleration(
  matrix: SupportMatrix | null | undefined,
  resolutionSelection: ResolutionSelection,
  fpsSelection: FramerateSelection,
): ProbeAcceleration | null {
  if (!matrix) return null;
  const fps = annotationFps(matrix, fpsSelection);
  if (resolutionSelection !== 'auto') {
    return matrix.get(resolutionSelection, fps).acceleration;
  }
  return bestAtFps(matrix, fps);
}
