// The two prompts that stand between a broadcaster and a publish: the
// one-time terms acknowledgment and the relay's publish secret. Shared by
// the broadcaster page and the OBS page (R33, docs/83 §5.4: "asks for the
// secret with the broadcaster's existing modal"), so both read the same.
import { Button } from '../../ui/Button';
import { GlassPanel } from '../../ui/GlassPanel';
import styles from './broadcaster.module.css';

export function TermsPromptModal({ onCancel, onAccept }: { onCancel: () => void; onAccept: () => void }) {
  return (
    <>
      <div className={styles.scrim} onClick={onCancel} />
      <div className={styles.modalCenter}>
        <GlassPanel className={styles.modal} role="dialog" aria-label="Terms of use">
          <h2 className={styles.modalTitle}>Before you broadcast</h2>
          <p className={styles.cardText}>
            You are about to publish your screen to anyone holding your join code. You are
            responsible for the content you stream, which must be lawful where you are. The
            service is provided as is, with no warranty.
          </p>
          <p className={styles.cardText}>
            By continuing you agree to the{' '}
            <a href="#/terms" target="_blank" rel="noopener noreferrer">
              Terms of use
            </a>
            .
          </p>
          <div className={styles.modalActions}>
            <Button variant="secondary" onClick={onCancel}>
              Cancel
            </Button>
            <Button onClick={onAccept}>Agree &amp; continue</Button>
          </div>
        </GlassPanel>
      </div>
    </>
  );
}

export function PublishSecretModal({
  note,
  draft,
  onDraftChange,
  onSubmit,
  onCancel,
  submitLabel = 'Start broadcasting',
}: {
  // Null: the plain "this relay requires a secret" line.
  note: string | null;
  draft: string;
  onDraftChange: (value: string) => void;
  onSubmit: () => void;
  onCancel: () => void;
  submitLabel?: string;
}) {
  return (
    <>
      <div className={styles.scrim} onClick={onCancel} />
      <div className={styles.modalCenter}>
        <GlassPanel className={styles.modal} role="dialog" aria-label="Publish secret">
          <h2 className={styles.modalTitle}>Publish secret</h2>
          <p className={styles.cardText}>{note ?? 'This relay requires a secret to broadcast.'}</p>
          <form
            className={styles.modalForm}
            onSubmit={(e) => {
              e.preventDefault();
              onSubmit();
            }}
          >
            <input
              type="password"
              className={styles.modalInput}
              value={draft}
              onChange={(e) => onDraftChange(e.target.value)}
              placeholder="shared secret"
              autoComplete="current-password"
              spellCheck={false}
              // eslint-disable-next-line jsx-a11y/no-autofocus -- the field is the modal's sole purpose
              autoFocus
            />
            <div className={styles.modalActions}>
              <Button type="button" variant="secondary" onClick={onCancel}>
                Cancel
              </Button>
              <Button type="submit">{submitLabel}</Button>
            </div>
          </form>
        </GlassPanel>
      </div>
    </>
  );
}

// The note offered when a secret-less publish was refused: the relay answers
// a missing secret with a 401 the browser surfaces opaquely.
export const SECRET_MAYBE_REQUIRED_NOTE =
  'The server refused the connection. It may require a publish secret — enter one to retry, or cancel.';
