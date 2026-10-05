import { useEffect, useRef } from 'react';
import styles from './broadcaster.module.css';
import buttonStyles from '../../ui/Button.module.css';
import { Button } from '../../ui/Button';
import { GlassPanel } from '../../ui/GlassPanel';
import { SITE_DOWNLOAD_URL } from '../../config';

// R67 (docs/69 D4, D5, D8): what the broadcaster page shows once the desktop
// app has been asked to open. 'opening' follows a click on the offer (or a
// `?desktop=1` link, D9); 'opened' follows an automatic launch. The page
// never learns whether the launch worked, so both keep the browser path one
// click away, and the card underneath is untouched.
export type HandoffModalKind = 'opening' | 'opened';

interface Props {
  kind: HandoffModalKind;
  // The `gawk://broadcast?…` link, for "Open again".
  href: string;
  // The remembered "always" (D5).
  auto: boolean;
  onAutoChange: (on: boolean) => void;
  onClose: () => void;
}

export function DesktopHandoffModal({ kind, href, auto, onAutoChange, onClose }: Props) {
  const continueRef = useRef<HTMLButtonElement>(null);
  // Focus lands on the primary action, so Enter continues in the browser and
  // Escape (below) reaches the dialog.
  useEffect(() => continueRef.current?.focus(), []);

  const getTheApp = (
    <a href={SITE_DOWNLOAD_URL} target="_blank" rel="noopener noreferrer" className={styles.linkBtn}>
      Get the app
    </a>
  );

  return (
    <>
      <div className={styles.scrim} onClick={onClose} />
      <div className={styles.modalCenter}>
        <GlassPanel
          className={`${styles.modal} ${styles.handoffModal}`}
          role="dialog"
          aria-modal="true"
          aria-label={kind === 'opening' ? 'Opening the desktop app' : 'Opened the desktop app'}
          data-testid="desktop-handoff-modal"
          onKeyDown={(e) => {
            if (e.key === 'Escape') onClose();
          }}
        >
          {kind === 'opening' ? (
            <>
              <h2 className={styles.modalTitle}>Opening the desktop app…</h2>
              <p className={styles.cardText}>Didn’t open? Get the app, or continue in the browser.</p>
              <label className={styles.handoffCheck}>
                <input type="checkbox" checked={auto} onChange={(e) => onAutoChange(e.target.checked)} />
                Always open broadcast links in the desktop app
              </label>
              <div className={`${styles.modalActions} ${styles.handoffActions}`}>
                <a
                  href={SITE_DOWNLOAD_URL}
                  target="_blank"
                  rel="noopener noreferrer"
                  className={`${buttonStyles.btn} ${buttonStyles.secondary}`}
                >
                  Get the app
                </a>
                <Button ref={continueRef} onClick={onClose}>
                  Continue in the browser
                </Button>
              </div>
            </>
          ) : (
            <>
              <h2 className={styles.modalTitle}>Opened the desktop app</h2>
              <p className={styles.cardText}>Didn’t open? {getTheApp}, or continue in the browser.</p>
              <div className={styles.handoffAuto}>
                <span>Broadcast links open in the desktop app automatically.</span>
                <button
                  type="button"
                  className={styles.linkBtn}
                  onClick={() => {
                    onAutoChange(false);
                    onClose();
                  }}
                >
                  Stop doing this
                </button>
              </div>
              <div className={`${styles.modalActions} ${styles.handoffActions}`}>
                <a href={href} className={`${buttonStyles.btn} ${buttonStyles.secondary}`}>
                  Open again
                </a>
                <Button ref={continueRef} onClick={onClose}>
                  Continue in the browser
                </Button>
              </div>
            </>
          )}
        </GlassPanel>
      </div>
    </>
  );
}
