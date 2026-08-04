import type { BrowserSupport } from '../lib/browserSupport';
import { Button } from './Button';
import { GlassPanel } from './GlassPanel';
import styles from './UnsupportedBrowserModal.module.css';

type Unsupported = Extract<BrowserSupport, { supported: false }>;

interface Props {
  support: Unsupported;
  onContinue: () => void;
}

// Shown when the client's engine cannot carry a gawk stream (BUGS.md: WebKit
// since the quic-go bump). Deliberately an acknowledgment, not a block: the
// user is told what will happen and then allowed through, because the diagnosis
// is a strong inference about their browser and not a certainty about their
// session. The scrim is inert for the same reason a toast would be wrong — a
// stray backdrop click must not stand in for "I understand".
export function UnsupportedBrowserModal({ support, onContinue }: Props) {
  const webkit = support.reason === 'webkit';
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
          {webkit ? (
            <>
              <p className={styles.text}>
                {support.browserLabel} uses Apple’s WebKit engine, which currently refuses the
                WebTransport connection every gawk stream rides on. It fails before reaching the
                relay, so live broadcasts look offline.
              </p>
              <p className={styles.text}>
                On iPhone and iPad every browser uses WebKit, so switching browsers there won’t
                help. To watch, use Chrome, Edge or Firefox on a computer.
              </p>
            </>
          ) : (
            <>
              <p className={styles.text}>
                This browser doesn’t support WebTransport, the protocol every gawk stream rides on.
              </p>
              <p className={styles.text}>
                To watch, use a current version of Chrome, Edge or Firefox.
              </p>
            </>
          )}
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
