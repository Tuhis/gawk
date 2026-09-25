// The landing-page server chip. Quiet (muted) on the default server so it
// doesn't compete with join-by-code; prominent when a non-default server is
// selected. Hidden entirely when the deployment disallows custom relays.

import { useState } from 'react';

import styles from './servers.module.css';
import { ServerPickerPanel } from './ServerPickerPanel';
import { ServerIcon } from '../../ui/Icons';
import { allowCustomRelays } from '../../config';
import { useTransportStore } from '../../state/transportStore';
import { relayHost } from '../../lib/relayUrl';

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
        <ServerIcon className={styles.chipIcon} />
        {quiet ? 'Server' : relayHost(serverUrl)}
      </button>
      {open && <ServerPickerPanel onClose={() => setOpen(false)} />}
    </>
  );
}
