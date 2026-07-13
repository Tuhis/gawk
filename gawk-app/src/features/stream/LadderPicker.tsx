import styles from './stream.module.css';
import {
  FRAMERATE_RUNGS,
  RESOLUTION_RUNGS,
  type FramerateRung,
  type ResolutionRung,
} from '../../media/ladder';
import { useBroadcastSettingsStore } from '../../state/broadcastSettingsStore';

interface Props {
  // Invoked after the store updates so a live pipeline can apply the change.
  onChange?: (resolution: ResolutionRung, framerate: FramerateRung) => void;
}

function resolutionLabel(rung: ResolutionRung): string {
  return rung === 'native' ? 'native' : `${rung}p`;
}

function framerateLabel(rung: FramerateRung): string {
  return rung === 'native' ? 'native' : `${rung} fps`;
}

// Deliberately never disabled: changing rungs mid-broadcast is a supported,
// designed-for operation (docs/08) — and the mechanism R4 will automate.
export function LadderPicker({ onChange }: Props) {
  const resolutionRung = useBroadcastSettingsStore((s) => s.resolutionRung);
  const framerateRung = useBroadcastSettingsStore((s) => s.framerateRung);
  const setResolutionRung = useBroadcastSettingsStore((s) => s.setResolutionRung);
  const setFramerateRung = useBroadcastSettingsStore((s) => s.setFramerateRung);

  const parseRes = (v: string): ResolutionRung => (v === 'native' ? v : (Number(v) as ResolutionRung));
  const parseFps = (v: string): FramerateRung => (v === 'native' ? v : (Number(v) as FramerateRung));

  return (
    <div className={styles.ladderPicker}>
      <div className={styles.field}>
        <label htmlFor="resolution-rung">Resolution</label>
        <select
          id="resolution-rung"
          value={String(resolutionRung)}
          onChange={(e) => {
            const rung = parseRes(e.target.value);
            setResolutionRung(rung);
            onChange?.(rung, framerateRung);
          }}
        >
          {RESOLUTION_RUNGS.map((rung) => (
            <option key={String(rung)} value={String(rung)}>
              {resolutionLabel(rung)}
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
            onChange?.(resolutionRung, rung);
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
