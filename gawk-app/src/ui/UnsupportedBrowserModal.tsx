import type { BrowserSupport } from '../lib/browserSupport';
import { Button } from './Button';
import { GlassPanel } from './GlassPanel';
import styles from './UnsupportedBrowserModal.module.css';

type Unsupported = Extract<BrowserSupport, { supported: false }>;

interface Props {
  support: Unsupported;
  onContinue: () => void;
}

// Shown when the browser lacks WebTransport, the one API every gawk stream
// rides on. Deliberately an acknowledgment, not a block: the user is told what
// will happen and then allowed through, because a capability probe is a strong
// inference about their browser and not a certainty about their session. The
// scrim is inert for the same reason a toast would be wrong — a stray backdrop
// click must not stand in for "I understand".
export function UnsupportedBrowserModal({ support, onContinue }: Props) {
  return (
    <>
      <div className={styles.scrim} data-testid="scrim" />
      <div className={styles.center}>
        <GlassPanel
          className={styles.modal}
          role="dialog"
          aria-modal="true"
          aria-label="Unsupported browser"
        >
          <p className={styles.eyebrow}>Unsupported browser</p>
          <h2 className={styles.title}>{support.browserLabel} can’t play gawk streams</h2>
          <p className={styles.text}>
            This browser doesn’t support WebTransport, the protocol every gawk stream rides on.
          </p>
          <p className={styles.text}>
            To watch, use a current version of Chrome, Edge, Firefox or Safari.
          </p>
          <div className={styles.actions}>
            {/* eslint-disable-next-line jsx-a11y/no-autofocus -- the modal's sole action; keyboard and screen-reader users must land on it */}
            <Button autoFocus onClick={onContinue}>
              Continue anyway
            </Button>
          </div>
        </GlassPanel>
      </div>
    </>
  );
}
