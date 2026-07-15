import styles from './stream.module.css';
import {
  FRAMERATE_SELECTIONS,
  RESOLUTION_SELECTIONS,
  type FramerateSelection,
  type ResolutionSelection,
} from '../../media/ladder';
import type { SupportMatrix } from '../../media/probe';
import { useBroadcastSettingsStore } from '../../state/broadcastSettingsStore';
import { annotate, framerateAcceleration, resolutionAcceleration } from './supportAnnotations';

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

// Annotation logic (badge/disable per docs/18 Decision 9) is shared with the
// advanced codec pin — see ./supportAnnotations.ts.

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
