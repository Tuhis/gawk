// The guest-facing local relay surfaces (docs/76 §7.3), rendered inside the
// session screens' in-session indicator:
// - the chip on a session switched to a PoP, "● Assembly 2026 · local relay",
//   with its menu item "Use gawk.ioio.fi instead" (the opt-out, item 43.6);
// - the toast beside the browser's local-network prompt;
// - the quiet note: denied, failed on a Mac, or "your next broadcast";
// - an opted-out PoP offered back ("Use it").
// Copy is provisional until PP1's design pass.

import { useState } from 'react';

import styles from '../servers/servers.module.css';
import { useTransportStore } from '../../state/transportStore';
import { usePopStore } from './popStore';
import { dismissPopToast, homeAppHost, offerName, optOut, pickLocalRelayAgain } from './switch';

export function LocalRelayChip() {
  const offer = useTransportStore((s) => s.offerOverride);
  const [open, setOpen] = useState(false);
  if (offer === null) return null;
  const name = offerName(offer);
  return (
    <span className={styles.popChip} data-testid="local-relay-chip">
      <button
        type="button"
        className={styles.popChipButton}
        aria-haspopup="menu"
        aria-expanded={open}
        onClick={() => setOpen((o) => !o)}
      >
        <span className={styles.popDot} aria-hidden="true">
          ●
        </span>{' '}
        {name} · local relay
      </button>
      {open && (
        <span role="menu" className={styles.popMenu}>
          <button
            type="button"
            role="menuitem"
            onClick={() => {
              setOpen(false);
              optOut();
            }}
          >
            Use {homeAppHost()} instead
          </button>
        </span>
      )}
    </span>
  );
}

export function LocalRelaySurfaces() {
  const toast = usePopStore((s) => s.toast);
  const note = usePopStore((s) => s.note);
  const declined = usePopStore((s) => s.declined);
  return (
    <>
      <LocalRelayChip />
      {toast !== null && (
        <span className={styles.indicatorDetail}>
          {toast}{' '}
          <button type="button" className={styles.popLinkButton} onClick={dismissPopToast}>
            OK
          </button>
        </span>
      )}
      {note !== null && <span className={styles.indicatorDetail}>{note}</span>}
      {declined !== null && (
        <span className={styles.indicatorDetail}>
          {offerName(declined)} has a local relay.{' '}
          <button type="button" className={styles.popLinkButton} onClick={() => void pickLocalRelayAgain()}>
            Use it
          </button>
        </span>
      )}
    </>
  );
}
