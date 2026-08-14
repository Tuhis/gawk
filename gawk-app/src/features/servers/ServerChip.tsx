// R37 (docs/40 §4.3): the landing-page server chip — the only new element on
// the front door. Quiet (muted) on the default server so join-by-code reads
// exactly as before; prominent when a non-default server is selected. Hidden
// entirely when the deployment disallows custom relays (D6). Opens the
// picker panel.

import { useState } from 'react';

import styles from './servers.module.css';
import { ServerPickerPanel } from './ServerPickerPanel';
import { allowCustomRelays } from '../../config';
import { useTransportStore } from '../../state/transportStore';

function hostOf(url: string): string {
  try {
    return new URL(url).host;
  } catch {
    return url;
  }
}

export function ServerChip() {
  const serverUrl = useTransportStore((s) => s.serverUrl);
  const resolvedSource = useTransportStore((s) => s.resolvedSource);
  const [open, setOpen] = useState(false);

  if (!allowCustomRelays()) return null;

  const quiet = resolvedSource === 'default';
  return (
    <>
      <button
        type="button"
        className={`${styles.chip} ${quiet ? styles.chipQuiet : styles.chipProminent}`}
        onClick={() => setOpen(true)}
        aria-label="Choose server"
        data-testid="server-chip"
      >
        <span className={styles.chipDot} aria-hidden="true" />
        {quiet ? 'Server' : hostOf(serverUrl)}
      </button>
      {open && <ServerPickerPanel onClose={() => setOpen(false)} />}
    </>
  );
}
