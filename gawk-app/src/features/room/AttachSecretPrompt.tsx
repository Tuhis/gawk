import { useState } from 'react';
import styles from './room.module.css';
import { Button } from '../../ui/Button';
import { GlassPanel } from '../../ui/GlassPanel';

interface Props {
  onSubmit: (secret: string) => void;
  onCancel: () => void;
}

// R42 (docs/44 D8): the secret a gated static room wants before it will carry
// our stream. The grant rides RoomHello, not a command, so submitting this
// re-dials the control session — see RoomView's attachSecret state.
export function AttachSecretPrompt({ onSubmit, onCancel }: Props) {
  const [draft, setDraft] = useState('');
  const clean = draft.trim();
  return (
    <>
      <div className={styles.scrim} onClick={onCancel} />
      <div className={styles.modalCenter}>
        {/* The dialog's name differs from the field's so each is
            unambiguous to a screen reader (and to a test query). */}
        <GlassPanel className={styles.modal} role="dialog" aria-label="Attach secret for this room">
          <h2 className={styles.modalTitle}>Attach secret</h2>
          <p className={styles.cardText}>
            Whoever set this room up has it. Your stream keeps running either way — this only decides whether the room
            carries it.
          </p>
          <form
            className={styles.modalForm}
            onSubmit={(e) => {
              e.preventDefault();
              if (clean !== '') onSubmit(clean);
            }}
          >
            <input
              className={styles.modalInput}
              type="password"
              value={draft}
              onChange={(e) => setDraft(e.target.value)}
              placeholder="attach secret"
              autoComplete="off"
              spellCheck={false}
              aria-label="Attach secret"
              // eslint-disable-next-line jsx-a11y/no-autofocus -- the field is the dialog's sole purpose
              autoFocus
            />
            <div className={styles.modalActions}>
              <Button type="button" variant="secondary" onClick={onCancel}>
                Cancel
              </Button>
              <Button type="submit" disabled={clean === ''}>
                Attach
              </Button>
            </div>
          </form>
        </GlassPanel>
      </div>
    </>
  );
}
