import { useState } from 'react';
import styles from './room.module.css';
import { Button } from '../../ui/Button';
import { GlassPanel } from '../../ui/GlassPanel';
import { MAX_ROOM_NICKNAME_LEN } from '../../transport/wire';
import { sanitizeNickname } from './roomPrefs';

interface Props {
  initial?: string;
  // The first join asks before dialing; a later edit (from the panel) changes
  // a live session. Both use this dialog; the copy differs.
  editing?: boolean;
  onSubmit: (nickname: string) => void;
  // First join only: continue with a relay-assigned guest name.
  onSkip?: () => void;
  onCancel?: () => void;
}

// R42 (docs/44 D10): the first-join nickname prompt — scrim + GlassPanel
// dialog, remembered per browser (roomPrefs.ts) and editable from the roster.
export function NicknamePrompt({ initial = '', editing = false, onSubmit, onSkip, onCancel }: Props) {
  const [draft, setDraft] = useState(initial);
  const clean = sanitizeNickname(draft);
  return (
    <>
      <div className={styles.scrim} onClick={onCancel ?? onSkip} />
      <div className={styles.modalCenter}>
        <GlassPanel className={styles.modal} role="dialog" aria-label="Nickname">
          <h2 className={styles.modalTitle}>{editing ? 'Change your nickname' : 'What should we call you?'}</h2>
          <p className={styles.cardText}>
            {editing
              ? 'Everyone in the room sees this name.'
              : 'Shown to the others in the room. Remembered in this browser; no account needed.'}
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
              value={draft}
              onChange={(e) => setDraft(e.target.value)}
              placeholder="nickname"
              maxLength={MAX_ROOM_NICKNAME_LEN}
              autoComplete="nickname"
              spellCheck={false}
              aria-label="Nickname"
              // eslint-disable-next-line jsx-a11y/no-autofocus -- the field is the dialog's sole purpose
              autoFocus
            />
            <div className={styles.modalActions}>
              {editing ? (
                <Button type="button" variant="secondary" onClick={onCancel}>
                  Cancel
                </Button>
              ) : (
                <Button type="button" variant="secondary" onClick={onSkip}>
                  Join as a guest
                </Button>
              )}
              <Button type="submit" disabled={clean === ''}>
                {editing ? 'Save' : 'Join'}
              </Button>
            </div>
          </form>
        </GlassPanel>
      </div>
    </>
  );
}
