// The room voice UI (R79, docs/80 OD10, OD14, D15–D18, §6): the Voice
// section of the people panel, and the small in-call dock over the stage.
// Both render only in a room that advertises the voice capability — the room
// view decides that, so neither is ever mounted otherwise.

import { useState, type PointerEvent as ReactPointerEvent } from 'react';
import styles from './voice.module.css';
import roomStyles from '../room.module.css';
import { Button } from '../../../ui/Button';
import { IconButton } from '../../../ui/IconButton';
import { HeadphonesIcon, HeadphonesOffIcon, MicIcon, MicOffIcon } from '../../../ui/Icons';
import { VolumeControl } from '../../../ui/VolumeControl';
import { useRoomStore } from '../../../state/roomStore';
import type { RoomState } from '../../../transport/wire';
import { useVoiceStore, type VoiceStoreState } from './voiceStore';
import { mergeVoiceRoster, type VoiceRosterRow } from './voiceRoster';
import { loadSpeakerVolume, pttKeyLabel } from './voicePrefs';
import type { VoiceActions } from './useRoomVoice';
import { voiceCaptureSupported } from './voiceCapture';
import { REMOVED_MESSAGE } from './voiceController';
import { VOICE_NOTICE, VOICE_ROOM_UNAVAILABLE, failureText, serverStateNote, unavailableText } from './voiceCopy';

function inVoice(phase: VoiceStoreState['phase']): boolean {
  return phase === 'connected' || phase === 'joining' || phase === 'rejoining';
}

// The hold-to-talk button: talks while pressed (pointer capture keeps a
// finger that slides off it talking until it lifts).
function HoldToTalk({ actions, held, className }: { actions: VoiceActions; held: boolean; className?: string }) {
  const down = (e: ReactPointerEvent<HTMLButtonElement>) => {
    if (e.button !== 0) return;
    e.preventDefault();
    e.currentTarget.setPointerCapture?.(e.pointerId);
    actions.setPttHeld(true);
  };
  const up = () => actions.setPttHeld(false);
  return (
    <button
      type="button"
      className={[styles.holdBtn, className].filter(Boolean).join(' ')}
      data-held={held ? 'true' : 'false'}
      aria-pressed={held}
      onPointerDown={down}
      onPointerUp={up}
      onPointerCancel={up}
      onLostPointerCapture={up}
      onContextMenu={(e) => e.preventDefault()}
      onKeyDown={(e) => {
        if ((e.key === ' ' || e.key === 'Enter') && !e.repeat) {
          e.preventDefault();
          actions.setPttHeld(true);
        }
      }}
      onKeyUp={(e) => {
        if (e.key === ' ' || e.key === 'Enter') actions.setPttHeld(false);
      }}
      onBlur={up}
    >
      <MicIcon /> {held ? 'Talking…' : 'Hold to talk'}
    </button>
  );
}

function MicToggle({ actions, s }: { actions: VoiceActions; s: VoiceStoreState }) {
  const forced = s.serverMuted || s.suppressed || s.serverDeafened;
  const off = s.selfMuted || s.selfDeafened || forced;
  return (
    <IconButton
      label={forced ? (serverStateNote(s) ?? 'Muted') : s.selfMuted ? 'Unmute microphone' : 'Mute microphone'}
      aria-pressed={off}
      className={styles.toggle}
      data-off={off ? 'true' : 'false'}
      disabled={forced || s.selfDeafened}
      onClick={() => actions.setMuted(!s.selfMuted)}
    >
      {off ? <MicOffIcon /> : <MicIcon />}
    </IconButton>
  );
}

function DeafenToggle({ actions, s }: { actions: VoiceActions; s: VoiceStoreState }) {
  const off = s.selfDeafened || s.serverDeafened;
  return (
    <IconButton
      label={s.serverDeafened ? 'A Mumble admin deafened you.' : s.selfDeafened ? 'Undeafen' : 'Deafen'}
      aria-pressed={off}
      className={styles.toggle}
      data-off={off ? 'true' : 'false'}
      disabled={s.serverDeafened}
      onClick={() => actions.setDeafened(!s.selfDeafened)}
    >
      {off ? <HeadphonesOffIcon /> : <HeadphonesIcon />}
    </IconButton>
  );
}

function PttKeyPicker({ actions, pttKey }: { actions: VoiceActions; pttKey: string }) {
  const [picking, setPicking] = useState(false);
  return (
    <span className={styles.pttKey}>
      Hold <kbd className={styles.kbd}>{pttKeyLabel(pttKey)}</kbd> to talk ·{' '}
      {picking ? (
        <button
          type="button"
          className={styles.linkBtn}
          // eslint-disable-next-line jsx-a11y/no-autofocus -- opened by the Change button
          autoFocus
          onKeyDown={(e) => {
            e.preventDefault();
            e.stopPropagation();
            if (e.key !== 'Escape') actions.setPttKey(e.code);
            setPicking(false);
          }}
          onBlur={() => setPicking(false)}
        >
          Press a key…
        </button>
      ) : (
        <button type="button" className={styles.linkBtn} onClick={() => setPicking(true)} aria-label="Change the push-to-talk key">
          Change
        </button>
      )}
    </span>
  );
}

function RosterRow({
  row,
  speaking,
  showSpeaking,
  canAdjust,
  actions,
}: {
  row: VoiceRosterRow;
  speaking: boolean;
  showSpeaking: boolean;
  canAdjust: boolean;
  actions: VoiceActions;
}) {
  const [volume, setVolume] = useState(() => loadSpeakerVolume(row.mumbleName));
  const [muted, setMuted] = useState(false);
  const apply = (v: number, m: boolean) => actions.setSpeakerVolume(row.session, row.mumbleName, m ? 0 : v);
  return (
    <li className={roomStyles.row} data-testid="voice-row">
      {/* OD14: speaking only for people in voice, worked out locally. */}
      <span
        className={roomStyles.speaking}
        data-on={showSpeaking && speaking ? 'true' : 'false'}
        aria-hidden="true"
      />
      <span className={roomStyles.rowMain}>
        <span className={roomStyles.rowLabel}>
          {row.label}
          {row.you ? ' (you)' : ''}
        </span>
        <span className={styles.rowState}>
          {row.mumble && <span className={styles.mumbleMark}>Mumble</span>}
          {row.deafened ? (
            <span className={styles.stateIcon} title={row.serverDeafened ? 'Deafened by the server' : 'Deafened'}>
              <HeadphonesOffIcon aria-hidden="false" aria-label={row.serverDeafened ? 'Deafened by the server' : 'Deafened'} />
            </span>
          ) : row.muted ? (
            <span className={styles.stateIcon} title={row.serverMuted ? 'Muted by the server' : 'Muted'}>
              <MicOffIcon aria-hidden="false" aria-label={row.serverMuted ? 'Muted by the server' : 'Muted'} />
            </span>
          ) : null}
          {showSpeaking && speaking && <span className={styles.speakingText}>speaking</span>}
        </span>
      </span>
      {canAdjust && !row.you && (
        <VolumeControl
          className={styles.rowVolume}
          compact={false}
          label={`Volume of ${row.label}`}
          muteLabel={`Mute ${row.label} for you`}
          unmuteLabel={`Unmute ${row.label}`}
          volume={volume}
          muted={muted}
          onVolume={(v) => {
            setVolume(v);
            apply(v, muted);
          }}
          onMutedChange={(m) => {
            setMuted(m);
            apply(volume, m);
          }}
        />
      )}
    </li>
  );
}

// The Voice section of the people panel.
export function VoiceSection({ snapshot, actions }: { snapshot: RoomState; actions: VoiceActions }) {
  const s = useVoiceStore();
  const roster = useRoomStore((st) => st.voiceRoster);
  const available = useRoomStore((st) => st.voiceAvailable);
  const rows = mergeVoiceRoster(roster ?? [], snapshot.participants, snapshot.yourId, s.ownSession);
  const connected = s.phase === 'connected';
  const speaking = new Set(s.speaking);
  const [canSpeak] = useState(voiceCaptureSupported);
  const serverNote = connected ? serverStateNote(s) : null;

  const joinButtons = (
    <div className={styles.actions}>
      {canSpeak && (
        <Button variant="secondary" onClick={() => actions.join(false)} disabled={!available}>
          <MicIcon /> Join voice
        </Button>
      )}
      <Button variant="secondary" onClick={() => actions.join(true)} disabled={!available}>
        <HeadphonesIcon /> {canSpeak ? 'Listen only' : 'Join listen-only'}
      </Button>
    </div>
  );

  return (
    <section className={roomStyles.section} aria-label="Voice" data-testid="voice-section">
      <h3 className={roomStyles.sectionTitle}>Voice · {rows.length}</h3>
      <p className={styles.notice}>{VOICE_NOTICE}</p>
      {!available && roster !== null && <p className={styles.status}>{VOICE_ROOM_UNAVAILABLE}</p>}

      {(s.phase === 'idle' || s.phase === 'removed') && (
        <>
          {s.phase === 'removed' && (
            <p className={styles.status} role="status" data-tone="warn">
              {REMOVED_MESSAGE}
            </p>
          )}
          {!canSpeak && <p className={styles.status}>This browser can only listen.</p>}
          {joinButtons}
        </>
      )}

      {s.phase === 'unavailable' && (
        <>
          <p className={styles.status} role="status" data-tone="warn">
            {unavailableText(s.unavailableReason)}
          </p>
          <div className={styles.actions}>
            <Button variant="secondary" onClick={actions.retry}>
              Try again
            </Button>
            <Button variant="secondary" onClick={actions.leave}>
              Dismiss
            </Button>
          </div>
        </>
      )}

      {s.phase === 'failed' && (
        <>
          <p className={styles.status} role="status" data-tone="warn">
            {failureText(s)}
          </p>
          <div className={styles.actions}>
            <Button variant="secondary" onClick={actions.retry}>
              Try again
            </Button>
            <Button variant="secondary" onClick={actions.leave}>
              Dismiss
            </Button>
          </div>
        </>
      )}

      {inVoice(s.phase) && (
        <div className={styles.controls}>
          {s.phase !== 'connected' && (
            <p className={styles.status} role="status">
              <span className={roomStyles.pulse} aria-hidden="true" />
              {s.phase === 'joining' ? 'Joining voice…' : 'Reconnecting voice…'}
            </p>
          )}
          {connected && !s.mumbleConnected && <p className={styles.status}>Connecting to Mumble…</p>}
          {serverNote && (
            <p className={styles.status} data-tone="warn">
              {serverNote}
            </p>
          )}
          <div className={styles.actions}>
            {!s.listenOnly && <MicToggle actions={actions} s={s} />}
            <DeafenToggle actions={actions} s={s} />
            <Button variant="secondary" onClick={actions.leave}>
              Leave voice
            </Button>
          </div>
          {s.listenOnly ? (
            <p className={styles.status}>Listening only — your microphone is off.</p>
          ) : (
            <>
              <div className={roomStyles.segment} role="radiogroup" aria-label="Talk mode">
                <button
                  type="button"
                  role="radio"
                  aria-checked={s.talkMode === 'vad'}
                  className={roomStyles.segmentBtn}
                  onClick={() => actions.setTalkMode('vad')}
                >
                  Voice activity
                </button>
                <button
                  type="button"
                  role="radio"
                  aria-checked={s.talkMode === 'ptt'}
                  className={roomStyles.segmentBtn}
                  onClick={() => actions.setTalkMode('ptt')}
                >
                  Push to talk
                </button>
              </div>
              {s.talkMode === 'ptt' && (
                <>
                  <PttKeyPicker actions={actions} pttKey={s.pttKey} />
                  <HoldToTalk actions={actions} held={s.pttHeld} />
                </>
              )}
            </>
          )}
        </div>
      )}

      {rows.length > 0 && (
        <ul className={roomStyles.list}>
          {rows.map((row) => (
            <RosterRow
              key={row.session}
              row={row}
              speaking={row.you ? s.talking : speaking.has(row.session)}
              showSpeaking={connected}
              canAdjust={connected}
              actions={actions}
            />
          ))}
        </ul>
      )}
      {roster !== null && available && rows.length === 0 && <p className={roomStyles.note}>Nobody is in voice yet.</p>}
    </section>
  );
}

// The in-call dock over the stage: mute, deafen and (push-to-talk) the hold
// button, so the people panel need not stay open while talking. Never fades.
export function VoiceDock({ actions }: { actions: VoiceActions }) {
  const s = useVoiceStore();
  if (!inVoice(s.phase)) return null;
  return (
    <div className={styles.dock} data-testid="voice-dock" role="group" aria-label="Voice controls">
      <span className={styles.dockState} data-talking={s.talking ? 'true' : 'false'} aria-hidden="true" />
      <span className={styles.dockLabel}>{s.phase === 'connected' ? 'Voice' : 'Voice…'}</span>
      {!s.listenOnly && <MicToggle actions={actions} s={s} />}
      <DeafenToggle actions={actions} s={s} />
      {s.phase === 'connected' && !s.listenOnly && s.talkMode === 'ptt' && (
        <HoldToTalk actions={actions} held={s.pttHeld} className={styles.dockHold} />
      )}
    </div>
  );
}
