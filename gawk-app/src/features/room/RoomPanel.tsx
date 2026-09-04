import { useState } from 'react';
import styles from './room.module.css';
import { Button } from '../../ui/Button';
import { GlassPanel } from '../../ui/GlassPanel';
import { IconButton } from '../../ui/IconButton';
import { CloseIcon, CopyIcon, EditIcon, EyeIcon, OpenIcon, PinIcon } from '../../ui/Icons';
import {
  ROOM_CAP_CHAT,
  ROOM_CLIENT_NATIVE,
  ROOM_CLIENT_WEB_BROADCASTER,
  ROOM_PARTICIPANT_FLAG_SPEAKING,
  ROOM_PARTICIPANT_FLAG_STREAMING,
  type RoomState,
} from '../../transport/wire';
import { fmtWatching } from '../../lib/format';
import { buildViewLink } from '../../lib/shareLink';
import { isDynamicRoom, isRoomCreator } from '../../state/roomStore';
import { SHARE_CODE_NOTE } from './roomCopy';
import { sanitizeNickname } from './roomPrefs';

interface Props {
  snapshot: RoomState;
  nickname: string;
  pinned: boolean;
  onPin: () => void;
  onClose: () => void;
  onDetach: (broadcastId: string) => void;
  onSetNickname: (nickname: string) => void;
  onCopyLink: () => void;
  onCopyCode: () => void;
  onEndRoom: () => void;
  // The broadcaster's own attached broadcast, if any (its row gets Detach
  // even for a non-creator — the attacher may detach its own).
  ownBroadcastId: string | null;
  // A participant without a broadcast may start one (docs/44 §4.8).
  onStartStreaming: (() => void) | null;
}

function kindLabel(kind: number, flags: number): string {
  if ((flags & ROOM_PARTICIPANT_FLAG_STREAMING) !== 0) return 'streaming';
  if (kind === ROOM_CLIENT_NATIVE) return 'native';
  if (kind === ROOM_CLIENT_WEB_BROADCASTER) return 'broadcaster';
  return 'viewer';
}

// R42 (docs/44 §4.9 revision): the people-and-chat panel. Streams (label,
// live/away, viewer count, open full-screen, the creator's detach), the
// roster with its reserved speaking slot and the nickname edit, the reserved
// chat area (rendered only when the room advertises the capability), and the
// share actions. Pinnable so it stays while the overlays fade; a bottom
// sheet on narrow screens (CSS).
export function RoomPanel({
  snapshot,
  nickname,
  pinned,
  onPin,
  onClose,
  onDetach,
  onSetNickname,
  onCopyLink,
  onCopyCode,
  onEndRoom,
  ownBroadcastId,
  onStartStreaming,
}: Props) {
  const creator = isRoomCreator(snapshot);
  const dynamic = isDynamicRoom(snapshot);
  const chat = (snapshot.caps & ROOM_CAP_CHAT) !== 0;
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState(nickname);

  return (
    <GlassPanel className={styles.panel} role="complementary" aria-label="People and chat">
      <div className={styles.panelHead}>
        <span>{snapshot.displayName || 'People'}</span>
        <span className={styles.panelHeadActions}>
          <IconButton
            label={pinned ? 'Unpin panel' : 'Pin panel'}
            aria-pressed={pinned}
            className={pinned ? styles.pinned : undefined}
            onClick={onPin}
          >
            <PinIcon />
          </IconButton>
          <IconButton label="Close panel" onClick={onClose}>
            <CloseIcon />
          </IconButton>
        </span>
      </div>

      <section className={styles.section}>
        <h3 className={styles.sectionTitle}>Streaming · {snapshot.attachments.length}</h3>
        {snapshot.attachments.length === 0 ? (
          <p className={styles.note}>Nobody is streaming yet.</p>
        ) : (
          <ul className={styles.list}>
            {snapshot.attachments.map((a, i) => (
              <li key={a.broadcastId} className={styles.row} data-testid="stream-row">
                <span className={a.live ? styles.liveDot : styles.awayDot} aria-hidden="true" />
                <span className={styles.rowMain}>
                  <span className={styles.rowLabel}>
                    {i + 1}. {a.label || a.broadcastId}
                    {a.broadcastId === ownBroadcastId ? ' (you)' : ''}
                  </span>
                  <span className={styles.rowSub}>
                    {a.live ? 'live' : 'away'} · <EyeIcon /> {fmtWatching(a.viewerCount)}
                  </span>
                </span>
                <span className={styles.rowActions}>
                  <a
                    className={styles.openLink}
                    href={buildViewLink(a.broadcastId)}
                    target="_blank"
                    rel="noopener noreferrer"
                    aria-label={`Open ${a.label || a.broadcastId} full-screen`}
                    title="Open full-screen"
                  >
                    <OpenIcon />
                  </a>
                  {(creator || a.broadcastId === ownBroadcastId) && (
                    <IconButton label={`Detach ${a.label || a.broadcastId}`} onClick={() => onDetach(a.broadcastId)}>
                      <CloseIcon />
                    </IconButton>
                  )}
                </span>
              </li>
            ))}
          </ul>
        )}
      </section>

      <section className={styles.section}>
        <h3 className={styles.sectionTitle}>People · {snapshot.participants.length}</h3>
        <ul className={styles.list}>
          {snapshot.participants.map((p) => {
            const you = p.id === snapshot.yourId;
            return (
              <li key={p.id} className={styles.row} data-testid="person-row">
                {/* Reserved: the voice speaking indicator (docs/44 §4.11). */}
                <span
                  className={styles.speaking}
                  data-on={(p.flags & ROOM_PARTICIPANT_FLAG_SPEAKING) !== 0 ? 'true' : 'false'}
                  aria-hidden="true"
                />
                <span className={styles.rowMain}>
                  <span className={styles.rowLabel}>
                    {p.nickname}
                    {you ? ' (you)' : ''}
                  </span>
                  <span
                    className={styles.kind}
                    data-streaming={(p.flags & ROOM_PARTICIPANT_FLAG_STREAMING) !== 0 ? 'true' : 'false'}
                  >
                    {kindLabel(p.kind, p.flags)}
                  </span>
                </span>
                {you && (
                  <span className={styles.rowActions}>
                    <IconButton
                      label="Edit nickname"
                      onClick={() => {
                        setDraft(nickname);
                        setEditing((e) => !e);
                      }}
                    >
                      <EditIcon />
                    </IconButton>
                  </span>
                )}
              </li>
            );
          })}
        </ul>
        {editing && (
          <form
            className={styles.nickForm}
            onSubmit={(e) => {
              e.preventDefault();
              const clean = sanitizeNickname(draft);
              if (clean !== '') onSetNickname(clean);
              setEditing(false);
            }}
          >
            <input
              className={styles.nickInput}
              value={draft}
              onChange={(e) => setDraft(e.target.value)}
              aria-label="New nickname"
              // eslint-disable-next-line jsx-a11y/no-autofocus -- opened by the edit button
              autoFocus
            />
            <Button type="submit" variant="secondary">
              Save
            </Button>
          </form>
        )}
      </section>

      {/* Reserved (docs/44 §4.11): the chat slot renders only when the room
          advertises the capability — a v1 relay never does. */}
      {chat && (
        <section className={styles.section}>
          <h3 className={styles.sectionTitle}>Chat</h3>
          <div className={styles.chatArea}>Chat is coming to this room.</div>
          <input className={styles.nickInput} disabled placeholder="Message" aria-label="Chat message" />
        </section>
      )}

      <div className={styles.panelFoot}>
        <div className={styles.panelActions}>
          <Button variant="secondary" onClick={onCopyLink}>
            <CopyIcon /> Copy room link
          </Button>
          {dynamic && (
            <Button variant="secondary" onClick={onCopyCode}>
              <CopyIcon /> Copy room code
            </Button>
          )}
          {onStartStreaming && (
            <Button variant="secondary" onClick={onStartStreaming}>
              Start streaming here
            </Button>
          )}
          {creator && dynamic && (
            <Button variant="danger" onClick={onEndRoom}>
              End room
            </Button>
          )}
        </div>
        <p className={styles.note}>{SHARE_CODE_NOTE}</p>
      </div>
    </GlassPanel>
  );
}
