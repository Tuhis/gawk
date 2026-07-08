import styles from './CaptureControls.module.css';
import { usePipelineStore } from '../../../state/pipelineStore';

interface Props {
  onStart: () => void;
  onStop: () => void;
}

export function CaptureControls({ onStart, onStop }: Props) {
  const status = usePipelineStore((s) => s.status);
  const running = status === 'capturing' || status === 'starting';
  const busy = status === 'starting' || status === 'stopping';

  return (
    <div className={styles.controls}>
      {!running ? (
        <button onClick={onStart} disabled={busy}>
          Start Capture
        </button>
      ) : (
        <button className="danger" onClick={onStop} disabled={busy}>
          Stop
        </button>
      )}
      <span className={styles.statusPill}>{status}</span>
    </div>
  );
}
