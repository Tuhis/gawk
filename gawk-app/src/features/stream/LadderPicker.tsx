import styles from './stream.module.css';
import {
  FRAMERATE_RUNGS,
  RESOLUTION_SELECTIONS,
  type FramerateRung,
  type ResolutionSelection,
} from '../../media/ladder';
import { useBroadcastSettingsStore } from '../../state/broadcastSettingsStore';

interface Props {
  // Invoked after the store updates so a live pipeline can apply the change.
  onChange?: (resolution: ResolutionSelection, framerate: FramerateRung) => void;
}

function resolutionLabel(selection: ResolutionSelection): string {
  if (selection === 'auto') return 'auto';
  return selection === 'native' ? 'native' : `${selection}p`;
}

function framerateLabel(rung: FramerateRung): string {
  return rung === 'native' ? 'native' : `${rung} fps`;
}

// Deliberately never disabled: changing rungs mid-broadcast is a supported,
// designed-for operation (docs/08) — and the mechanism R4 automates behind
// the "auto" selection (docs/09).
export function LadderPicker({ onChange }: Props) {
  const resolutionSelection = useBroadcastSettingsStore((s) => s.resolutionSelection);
  const framerateRung = useBroadcastSettingsStore((s) => s.framerateRung);
  const setResolutionSelection = useBroadcastSettingsStore((s) => s.setResolutionSelection);
  const setFramerateRung = useBroadcastSettingsStore((s) => s.setFramerateRung);

  const parseRes = (v: string): ResolutionSelection =>
    v === 'auto' || v === 'native' ? v : (Number(v) as ResolutionSelection);
  const parseFps = (v: string): FramerateRung => (v === 'native' ? v : (Number(v) as FramerateRung));

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
            onChange?.(selection, framerateRung);
          }}
        >
          {RESOLUTION_SELECTIONS.map((selection) => (
            <option key={String(selection)} value={String(selection)}>
              {resolutionLabel(selection)}
            </option>
          ))}
        </select>
      </div>
      <div className={styles.field}>
        <label htmlFor="framerate-rung">Framerate</label>
        <select
          id="framerate-rung"
          value={String(framerateRung)}
          onChange={(e) => {
            const rung = parseFps(e.target.value);
            setFramerateRung(rung);
            onChange?.(resolutionSelection, rung);
          }}
        >
          {FRAMERATE_RUNGS.map((rung) => (
            <option key={String(rung)} value={String(rung)}>
              {framerateLabel(rung)}
            </option>
          ))}
        </select>
      </div>
    </div>
  );
}
