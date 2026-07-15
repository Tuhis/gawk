import styles from './stream.module.css';
import {
  FRAMERATE_SELECTIONS,
  RESOLUTION_RUNGS,
  RESOLUTION_SELECTIONS,
  resolveAutoFps,
  type FramerateSelection,
  type ResolutionSelection,
} from '../../media/ladder';
import type { ProbeAcceleration, SupportMatrix } from '../../media/probe';
import { useBroadcastSettingsStore } from '../../state/broadcastSettingsStore';

interface Props {
  // Invoked after the store updates so a live pipeline can apply the change.
  onChange?: (resolution: ResolutionSelection, framerate: FramerateSelection) => void;
  // R13 (docs/18 Decision 9): probe matrix backing the option annotations.
  // null (probe unavailable / not yet landed) renders options unannotated.
  matrix?: SupportMatrix | null;
}

function resolutionLabel(selection: ResolutionSelection): string {
  if (selection === 'auto') return 'auto';
  return selection === 'native' ? 'native' : `${selection}p`;
}

function framerateLabel(selection: FramerateSelection): string {
  if (selection === 'auto') return 'auto';
  return selection === 'native' ? 'native' : `${selection} fps`;
}

// docs/18 Decision 9: options are annotated, never removed — an explicit
// software rung is allowed (R4's "explicit choices are honored"); only
// genuinely unsupported combos disable.
function annotate(label: string, acceleration: ProbeAcceleration | null): {
  label: string;
  disabled: boolean;
} {
  if (acceleration === 'software') return { label: `${label} · software`, disabled: false };
  if (acceleration === 'unsupported') return { label: `${label} · unsupported`, disabled: true };
  return { label, disabled: false };
}

// The fps a resolution option is annotated at: explicit rungs speak for
// themselves, 'auto' uses the matrix's framerate-first resolution, 'native'
// probes at the 60 upper bound (no matrix column for arbitrary refresh
// rates).
function annotationFps(matrix: SupportMatrix, fpsSelection: FramerateSelection): number {
  if (fpsSelection === 'auto') return resolveAutoFps(matrix.get);
  return fpsSelection === 'native' ? 60 : fpsSelection;
}

function resolutionAcceleration(
  matrix: SupportMatrix | null | undefined,
  selection: ResolutionSelection,
  fpsSelection: FramerateSelection,
): ProbeAcceleration | null {
  if (!matrix || selection === 'auto') return null; // auto always works — it adapts
  return matrix.get(selection, annotationFps(matrix, fpsSelection)).acceleration;
}

function framerateAcceleration(
  matrix: SupportMatrix | null | undefined,
  selection: FramerateSelection,
  resolutionSelection: ResolutionSelection,
): ProbeAcceleration | null {
  if (!matrix || selection === 'auto') return null;
  const fps = selection === 'native' ? 60 : selection;
  if (resolutionSelection !== 'auto') return matrix.get(resolutionSelection, fps).acceleration;
  // Auto resolution adapts the rung — annotate with the best any rung offers
  // at this fps (that's what auto would pick).
  let best: ProbeAcceleration = 'unsupported';
  for (const rung of RESOLUTION_RUNGS) {
    const acc = matrix.get(rung, fps).acceleration;
    if (acc === 'hardware') return 'hardware';
    if (acc === 'software') best = 'software';
  }
  return best;
}

// Deliberately never disabled as a whole: changing rungs mid-broadcast is a
// supported, designed-for operation (docs/08) — and the mechanism R4
// automates behind the "auto" selection (docs/09).
export function LadderPicker({ onChange, matrix }: Props) {
  const resolutionSelection = useBroadcastSettingsStore((s) => s.resolutionSelection);
  const framerateSelection = useBroadcastSettingsStore((s) => s.framerateSelection);
  const setResolutionSelection = useBroadcastSettingsStore((s) => s.setResolutionSelection);
  const setFramerateSelection = useBroadcastSettingsStore((s) => s.setFramerateSelection);

  const parseRes = (v: string): ResolutionSelection =>
    v === 'auto' || v === 'native' ? v : (Number(v) as ResolutionSelection);
  const parseFps = (v: string): FramerateSelection =>
    v === 'auto' || v === 'native' ? v : (Number(v) as FramerateSelection);

  return (
    <div className={styles.ladderPicker}>
      <div className={styles.field}>
        <label htmlFor="resolution-rung">Resolution</label>
        <select
          id="resolution-rung"
          value={String(resolutionSelection)}
          onChange={(e) => {
            const selection = parseRes(e.target.value);
            setResolutionSelection(selection);
            onChange?.(selection, framerateSelection);
          }}
        >
          {RESOLUTION_SELECTIONS.map((selection) => {
            const { label, disabled } = annotate(
              resolutionLabel(selection),
              resolutionAcceleration(matrix, selection, framerateSelection),
            );
            return (
              <option key={String(selection)} value={String(selection)} disabled={disabled}>
                {label}
              </option>
            );
          })}
        </select>
      </div>
      <div className={styles.field}>
        <label htmlFor="framerate-rung">Framerate</label>
        <select
          id="framerate-rung"
          value={String(framerateSelection)}
          onChange={(e) => {
            const selection = parseFps(e.target.value);
            setFramerateSelection(selection);
            onChange?.(resolutionSelection, selection);
          }}
        >
          {FRAMERATE_SELECTIONS.map((selection) => {
            const { label, disabled } = annotate(
              framerateLabel(selection),
              framerateAcceleration(matrix, selection, resolutionSelection),
            );
            return (
              <option key={String(selection)} value={String(selection)} disabled={disabled}>
                {label}
              </option>
            );
          })}
        </select>
      </div>
    </div>
  );
}
