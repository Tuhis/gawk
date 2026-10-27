// The room voice UI (R79, docs/80 OD10, OD14, D15–D18, §6; R81, docs/82).
// Voice has its own surfaces, separate from the people panel:
//
//   - VoiceButton: the header's Voice button, with the one-time hint;
//   - VoiceInvite: the "N in voice · Join voice · Listen only" chip where
//     the dock will sit, while this browser is not in voice;
//   - VoicePanel: a flyout under the header, pinnable into a column beside
//     the stage, or the phone sheet's Voice tab;
//   - VoiceDock: the in-call dock over the stage (who is speaking, hold to
//     talk, the mic with its Microphone popover, deafen);
//   - VoiceThumbBar: the dock's phone form, thumb-sized.
//
// All render only in a room that advertises the voice capability — the room
// view decides that, so none is ever mounted otherwise.

import { useEffect, useRef, useState, type PointerEvent as ReactPointerEvent, type ReactNode } from 'react';
import styles from './voice.module.css';
import roomStyles from '../room.module.css';
import { Button } from '../../../ui/Button';
import { GlassPanel } from '../../../ui/GlassPanel';
import { IconButton } from '../../../ui/IconButton';
import { CloseIcon, HeadphonesIcon, HeadphonesOffIcon, MicIcon, MicOffIcon, PinIcon } from '../../../ui/Icons';
import { VolumeControl } from '../../../ui/VolumeControl';
import { useRoomStore } from '../../../state/roomStore';
import type { RoomState } from '../../../transport/wire';
import { inVoice, useVoiceStore, type VoiceStoreState } from './voiceStore';
import { mergeVoiceRoster, type VoiceRosterRow } from './voiceRoster';
import {
  MAX_SPEAKER_VOLUME,
  loadSpeakerVolume,
  loadTalkModeChosen,
  pttKeyLabel,
  saveTalkModeChosen,
} from './voicePrefs';
import type { VoiceActions } from './useRoomVoice';
import { voiceCaptureSupported } from './voiceCapture';
import { REMOVED_MESSAGE } from './voiceController';
import { AudioSettings, HearSelf, NoiseControl, ThresholdControl } from './VoiceAudio';
import {
  FIRST_JOIN_TITLE,
  PTT_NO_THRESHOLD,
  VAD_EXPLAIN,
  VOICE_HINT,
  VOICE_NOTICE,
  VOICE_ROOM_UNAVAILABLE,
  failureText,
  inVoiceText,
  serverStateNote,
  unavailableText,
} from './voiceCopy';

// The merged roster for the room's current snapshot.
function useVoiceRows(snapshot: RoomState): VoiceRosterRow[] {
  const roster = useRoomStore((st) => st.voiceRoster);
  const ownSession = useVoiceStore((s) => s.ownSession);
  return mergeVoiceRoster(roster ?? [], snapshot.participants, snapshot.yourId, ownSession);
}

// Who is speaking right now, as this browser hears it (OD14: only in voice).
function useSpeakingCheck(): (row: VoiceRosterRow) => boolean {
  const connected = useVoiceStore((s) => s.phase === 'connected');
  const speaking = useVoiceStore((s) => s.speaking);
  const talking = useVoiceStore((s) => s.talking);
  const set = new Set(speaking);
  return (row) => connected && (row.you ? talking : set.has(row.session));
}

// ---- Small pieces -----------------------------------------------------------

const HUES = 6;

function hueOf(name: string): number {
  let h = 0;
  for (let i = 0; i < name.length; i++) h = (h * 31 + name.charCodeAt(i)) >>> 0;
  return h % HUES;
}

function Avatar({ row, speaking = false }: { row: VoiceRosterRow; speaking?: boolean }) {
  const initial = Array.from(row.label.trim())[0]?.toUpperCase() ?? '?';
  return (
    <span
      className={styles.avatar}
      data-hue={hueOf(row.mumbleName)}
      data-on={speaking ? 'true' : 'false'}
      aria-hidden="true"
    >
      {initial}
    </span>
  );
}

// Up to three overlapping avatars, the speaker first.
function AvatarStack({ rows, isSpeaking }: { rows: VoiceRosterRow[]; isSpeaking?: (r: VoiceRosterRow) => boolean }) {
  const first = isSpeaking ? rows.findIndex(isSpeaking) : -1;
  const ordered = first > 0 ? [rows[first], ...rows.slice(0, first), ...rows.slice(first + 1)] : rows;
  return (
    <span className={styles.stack}>
      {ordered.slice(0, 3).map((r) => (
        <Avatar key={r.session} row={r} speaking={isSpeaking?.(r) ?? false} />
      ))}
    </span>
  );
}

// The hold-to-talk button: talks while pressed (pointer capture keeps a
// finger that slides off it talking until it lifts).
function HoldToTalk({
  actions,
  held,
  className,
  label,
}: {
  actions: VoiceActions;
  held: boolean;
  className?: string;
  label?: ReactNode;
}) {
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
      <MicIcon /> {label ?? (held ? 'Talking…' : 'Hold to talk')}
    </button>
  );
}

function micLabel(s: VoiceStoreState): string {
  const forced = s.serverMuted || s.suppressed || s.serverDeafened;
  return forced ? (serverStateNote(s) ?? 'Muted') : s.selfMuted ? 'Unmute microphone' : 'Mute microphone';
}

function micOff(s: VoiceStoreState): boolean {
  return s.selfMuted || s.selfDeafened || s.serverMuted || s.suppressed || s.serverDeafened;
}

function MicToggle({ actions, s, className }: { actions: VoiceActions; s: VoiceStoreState; className?: string }) {
  const forced = s.serverMuted || s.suppressed || s.serverDeafened;
  const off = micOff(s);
  return (
    <IconButton
      label={micLabel(s)}
      aria-pressed={off}
      className={[styles.toggle, className].filter(Boolean).join(' ')}
      data-off={off ? 'true' : 'false'}
      disabled={forced || s.selfDeafened}
      onClick={() => actions.setMuted(!s.selfMuted)}
    >
      {off ? <MicOffIcon /> : <MicIcon />}
    </IconButton>
  );
}

function DeafenToggle({ actions, s, className }: { actions: VoiceActions; s: VoiceStoreState; className?: string }) {
  const off = s.selfDeafened || s.serverDeafened;
  return (
    <IconButton
      label={s.serverDeafened ? 'A Mumble admin deafened you.' : s.selfDeafened ? 'Undeafen' : 'Deafen'}
      aria-pressed={off}
      className={[styles.toggle, className].filter(Boolean).join(' ')}
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

function TalkModeSwitch({ actions, s }: { actions: VoiceActions; s: VoiceStoreState }) {
  return (
    <div className={[roomStyles.segment, styles.fill].join(' ')} role="radiogroup" aria-label="Talk mode">
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
  );
}

// ---- The roster -------------------------------------------------------------

function RosterRow({
  row,
  speaking,
  canAdjust,
  open,
  onToggle,
  actions,
}: {
  row: VoiceRosterRow;
  speaking: boolean;
  canAdjust: boolean;
  open: boolean;
  onToggle: () => void;
  actions: VoiceActions;
}) {
  const [volume, setVolume] = useState(() => loadSpeakerVolume(row.mumbleName));
  // From the visit's mute set, not false: a person who reconnects to Mumble
  // gets a new session and so a new row, and must still show as muted.
  const [muted, setMuted] = useState(() => actions.isSpeakerMuted(row.mumbleName));
  const apply = (v: number, m: boolean) => actions.setSpeakerVolume(row.session, row.mumbleName, v, m);
  const adjustable = canAdjust && !row.you;
  const pct = Math.round(volume * 100);
  const main = (
    <>
      {/* OD14: speaking only for people in voice, worked out locally. */}
      <Avatar row={row} speaking={speaking} />
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
        {speaking && <span className={styles.speakingText}>speaking</span>}
        {adjustable && !open && pct !== 100 && <span className={styles.boostMark} data-boost={pct > 100 ? 'true' : 'false'}>{pct}%</span>}
      </span>
    </>
  );
  return (
    <li className={styles.rosterItem} data-open={open ? 'true' : 'false'} data-testid="voice-row">
      {adjustable ? (
        // A click, not a hover, opens the person's volume: it works on touch
        // and stays open while it is being dragged.
        <button
          type="button"
          className={styles.rosterRow}
          aria-expanded={open}
          aria-label={`${row.label}: volume`}
          onClick={onToggle}
        >
          {main}
        </button>
      ) : (
        <div className={styles.rosterRow}>{main}</div>
      )}
      {adjustable && open && (
        <div className={styles.rowVolume}>
          <VolumeControl
            compact={false}
            max={MAX_SPEAKER_VOLUME}
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
          {/* The value is the way back to 100 %; amber while boosted. */}
          <button
            type="button"
            className={styles.volumeValue}
            data-boost={pct > 100 ? 'true' : 'false'}
            aria-label={`${row.label} at ${pct}%: back to 100%`}
            title="Back to 100%"
            onClick={() => {
              setVolume(1);
              apply(1, muted);
            }}
          >
            {pct}%
          </button>
        </div>
      )}
    </li>
  );
}

function Roster({ rows, actions }: { rows: VoiceRosterRow[]; actions: VoiceActions }) {
  const connected = useVoiceStore((s) => s.phase === 'connected');
  const isSpeaking = useSpeakingCheck();
  const [openSession, setOpenSession] = useState<number | null>(null);
  return (
    <section className={styles.rosterSection} aria-label="In voice">
      <h3 className={roomStyles.sectionTitle}>{`In voice · ${rows.length}`}</h3>
      {rows.length > 0 && (
        <ul className={roomStyles.list}>
          {rows.map((row) => (
            <RosterRow
              key={row.session}
              row={row}
              speaking={isSpeaking(row)}
              canAdjust={connected}
              open={openSession === row.session}
              onToggle={() => setOpenSession((o) => (o === row.session ? null : row.session))}
              actions={actions}
            />
          ))}
        </ul>
      )}
    </section>
  );
}

// ---- The first join's card --------------------------------------------------

function FirstJoinCard({ actions, s, onDone }: { actions: VoiceActions; s: VoiceStoreState; onDone: () => void }) {
  return (
    <div className={styles.firstJoin} role="group" aria-label={FIRST_JOIN_TITLE} data-testid="voice-first-join">
      <strong className={styles.firstJoinTitle}>{FIRST_JOIN_TITLE}</strong>
      <TalkModeSwitch actions={actions} s={s} />
      {s.talkMode === 'vad' ? (
        <>
          <p className={styles.audioNote}>{VAD_EXPLAIN}</p>
          <ThresholdControl actions={actions} live />
        </>
      ) : (
        <PttKeyPicker actions={actions} pttKey={s.pttKey} />
      )}
      <div className={styles.firstJoinFoot}>
        <span className={styles.audioNote}>Noise and threshold: under Audio.</span>
        <Button onClick={onDone}>Done</Button>
      </div>
    </div>
  );
}

// ---- The panel --------------------------------------------------------------

export type VoicePanelLayout = 'flyout' | 'pinned' | 'sheet';

interface VoicePanelProps {
  snapshot: RoomState;
  actions: VoiceActions;
  // flyout: under the header, over the stage; pinned: the right column the
  // stage gives up (like the people panel); sheet: the phone's bottom sheet.
  layout: VoicePanelLayout;
  onClose: () => void;
  onPinnedChange: (pinned: boolean) => void;
  // The phone sheet's People | Voice tabs, in place of the title.
  tabs?: ReactNode;
}

export function VoicePanel({ snapshot, actions, layout, onClose, onPinnedChange, tabs }: VoicePanelProps) {
  const s = useVoiceStore();
  const roster = useRoomStore((st) => st.voiceRoster);
  const available = useRoomStore((st) => st.voiceAvailable);
  const rows = useVoiceRows(snapshot);
  const connected = s.phase === 'connected';
  const [canSpeak] = useState(voiceCaptureSupported);
  const [chosen, setChosen] = useState(loadTalkModeChosen);
  const serverNote = connected ? serverStateNote(s) : null;
  const firstJoin = inVoice(s.phase) && !s.listenOnly && !chosen;

  const head = (
    <div className={styles.panelHead}>
      {tabs ?? (
        <span className={styles.panelTitle}>
          Voice
          {connected && <span className={styles.connected}>connected</span>}
        </span>
      )}
      <span className={roomStyles.panelHeadActions}>
        {layout !== 'sheet' && (
          <IconButton
            label={layout === 'pinned' ? 'Unpin' : 'Pin beside the stage'}
            aria-pressed={layout === 'pinned'}
            className={layout === 'pinned' ? styles.pinned : undefined}
            onClick={() => onPinnedChange(layout !== 'pinned')}
          >
            <PinIcon />
          </IconButton>
        )}
        <IconButton label={inVoice(s.phase) ? 'Close (you stay in voice)' : 'Close voice'} onClick={onClose}>
          <CloseIcon />
        </IconButton>
      </span>
    </div>
  );

  const body = (
    <>
      {(s.phase === 'idle' || s.phase === 'removed') && (
        <>
          {s.phase === 'removed' && (
            <p className={styles.status} role="status" data-tone="warn">
              {REMOVED_MESSAGE}
            </p>
          )}
          {!available && roster !== null && <p className={styles.status}>{VOICE_ROOM_UNAVAILABLE}</p>}
          {!canSpeak && <p className={styles.status}>This browser can only listen.</p>}
          <div className={styles.joinRow}>
            {canSpeak && (
              <Button className={styles.joinBtn} onClick={() => actions.join(false)} disabled={!available}>
                <MicIcon /> Join voice
              </Button>
            )}
            <Button variant="secondary" onClick={() => actions.join(true)} disabled={!available}>
              <HeadphonesIcon /> {canSpeak ? 'Listen only' : 'Join listen-only'}
            </Button>
          </div>
          <p className={styles.noticeLine}>{VOICE_NOTICE}</p>
        </>
      )}

      {(s.phase === 'unavailable' || s.phase === 'failed') && (
        <>
          <p className={styles.status} role="status" data-tone="warn">
            {s.phase === 'unavailable' ? unavailableText(s.unavailableReason) : failureText(s)}
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
        <>
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
          <div className={styles.controlRow}>
            {!s.listenOnly && <MicToggle actions={actions} s={s} className={styles.boxed} />}
            <DeafenToggle actions={actions} s={s} className={styles.boxed} />
            <span className={styles.spacer} />
            <Button variant="secondary" className={styles.leaveBtn} onClick={actions.leave}>
              Leave voice
            </Button>
          </div>
          {s.listenOnly ? (
            <p className={styles.status}>Listening only — your microphone is off.</p>
          ) : firstJoin ? (
            <FirstJoinCard
              actions={actions}
              s={s}
              onDone={() => {
                saveTalkModeChosen();
                setChosen(true);
              }}
            />
          ) : (
            <>
              <TalkModeSwitch actions={actions} s={s} />
              {s.talkMode === 'ptt' && <PttKeyPicker actions={actions} pttKey={s.pttKey} />}
              <AudioSettings actions={actions} />
            </>
          )}
        </>
      )}

      <Roster rows={rows} actions={actions} />
      {roster !== null && available && rows.length === 0 && <p className={roomStyles.note}>Nobody is in voice yet.</p>}
      {inVoice(s.phase) && <p className={styles.noticeLine}>{VOICE_NOTICE}</p>}
    </>
  );

  if (layout === 'flyout') {
    return (
      <section className={styles.flyout} role="complementary" aria-label="Voice" data-testid="voice-panel" data-layout={layout}>
        {head}
        {body}
      </section>
    );
  }
  return (
    <GlassPanel
      className={[roomStyles.panel, styles.columnPanel].join(' ')}
      role="complementary"
      aria-label="Voice"
      data-testid="voice-panel"
      data-layout={layout}
    >
      {head}
      {body}
    </GlassPanel>
  );
}

// ---- The header's Voice button and its hint ---------------------------------

export function VoiceButton({
  snapshot,
  open,
  onToggle,
  hint,
  onHintDone,
}: {
  snapshot: RoomState;
  open: boolean;
  onToggle: () => void;
  // The one-time hint, while nobody is in voice (the room view decides).
  hint: boolean;
  onHintDone: () => void;
}) {
  const rows = useVoiceRows(snapshot);
  const phase = useVoiceStore((s) => s.phase);
  const live = inVoice(phase);
  const n = rows.length;
  return (
    <span className={styles.voiceBtnWrap}>
      <button
        type="button"
        className={styles.voiceBtn}
        data-live={live ? 'true' : 'false'}
        aria-expanded={open}
        aria-label={live ? `Voice: in voice, ${inVoiceText(n)}` : n > 0 ? `Voice: ${inVoiceText(n)}` : 'Voice'}
        onClick={onToggle}
      >
        {live ? (
          <>
            <span className={styles.liveDot} aria-hidden="true" />
            <span className={styles.voiceBtnText}>In voice · </span>
            {n}
          </>
        ) : (
          <>
            <HeadphonesIcon />
            <span className={styles.voiceBtnText}>Voice</span>
            {n > 0 && <span className={styles.badge}>{n}</span>}
          </>
        )}
      </button>
      {hint && (
        <span className={styles.hint} role="status" data-testid="voice-hint">
          <span>{VOICE_HINT}</span>
          <button type="button" className={styles.linkBtn} onClick={onHintDone}>
            Got it
          </button>
        </span>
      )}
    </span>
  );
}

// ---- The invite chip --------------------------------------------------------

// Where the dock will sit, while this browser is not in voice and someone is:
// voice is one click away from the stage, no panel needed.
export function VoiceInvite({
  snapshot,
  actions,
  onJoin,
  onHide,
}: {
  snapshot: RoomState;
  actions: VoiceActions;
  // After a join from the chip (the room view may open the panel).
  onJoin: (listenOnly: boolean) => void;
  onHide: () => void;
}) {
  const rows = useVoiceRows(snapshot);
  const phase = useVoiceStore((s) => s.phase);
  const available = useRoomStore((st) => st.voiceAvailable);
  const [canSpeak] = useState(voiceCaptureSupported);
  if (phase !== 'idle' || !available || rows.length === 0) return null;
  const join = (listenOnly: boolean) => {
    actions.join(listenOnly);
    onJoin(listenOnly);
  };
  return (
    <div className={styles.invite} role="group" aria-label="Voice" data-testid="voice-invite">
      <AvatarStack rows={rows} />
      <span className={styles.inviteText}>{inVoiceText(rows.length)}</span>
      {canSpeak && (
        <button type="button" className={styles.inviteJoin} onClick={() => join(false)}>
          Join voice
        </button>
      )}
      <button type="button" className={styles.inviteListen} onClick={() => join(true)}>
        {canSpeak ? 'Listen only' : 'Join listen-only'}
      </button>
      <IconButton label="Hide" className={styles.inviteHide} onClick={onHide}>
        <CloseIcon />
      </IconButton>
    </div>
  );
}

// ---- The dock and its Microphone popover -------------------------------------

function MicPopover({ actions, onOpenPanel }: { actions: VoiceActions; onOpenPanel: () => void }) {
  const s = useVoiceStore();
  return (
    <div className={styles.popover} role="dialog" aria-label="Microphone" data-testid="voice-mic-popover">
      <strong className={styles.popoverTitle}>Microphone</strong>
      <TalkModeSwitch actions={actions} s={s} />
      {s.talkMode === 'vad' ? (
        <ThresholdControl actions={actions} live />
      ) : (
        <p className={styles.audioNote}>{PTT_NO_THRESHOLD}</p>
      )}
      <NoiseControl actions={actions} />
      <div className={styles.popoverFoot}>
        <HearSelf actions={actions} />
        <button type="button" className={styles.linkBtn} onClick={onOpenPanel}>
          Voice panel
        </button>
      </div>
    </div>
  );
}

// Who is speaking, at a glance; the click opens the voice panel.
function SpeakerSummary({ snapshot, onOpenPanel }: { snapshot: RoomState; onOpenPanel: () => void }) {
  const rows = useVoiceRows(snapshot);
  const isSpeaking = useSpeakingCheck();
  const speaker = rows.find(isSpeaking) ?? null;
  const name = speaker ? (speaker.you ? 'You' : speaker.label) : null;
  return (
    <button
      type="button"
      className={styles.summary}
      aria-label={name ? `Open voice: ${name} speaking, ${inVoiceText(rows.length)}` : `Open voice: ${inVoiceText(rows.length)}`}
      onClick={onOpenPanel}
    >
      <AvatarStack rows={rows} isSpeaking={isSpeaking} />
      {name && <span className={styles.summaryName}>{name}</span>}
      <span className={styles.summaryCount}>· {rows.length}</span>
    </button>
  );
}

// The in-call dock over the stage: who is speaking, hold to talk, the mic
// (its caret opens the Microphone popover) and deafen, so the voice panel
// need not stay open while talking. Never fades; no Leave, which a stray
// click must not reach.
export function VoiceDock(props: { snapshot: RoomState; actions: VoiceActions; onOpenPanel: () => void }) {
  const live = useVoiceStore((s) => inVoice(s.phase));
  // Mounted per call, so a popover left open never reopens with the next
  // join (review round 1).
  return live ? <DockBody {...props} /> : null;
}

function DockBody({
  snapshot,
  actions,
  onOpenPanel,
}: {
  snapshot: RoomState;
  actions: VoiceActions;
  onOpenPanel: () => void;
}) {
  const s = useVoiceStore();
  const [popover, setPopover] = useState(false);
  const rootRef = useRef<HTMLDivElement | null>(null);
  const caretRef = useRef<HTMLButtonElement | null>(null);
  const canTalk = !s.listenOnly;

  // A press anywhere outside closes it; Escape closes back onto the caret.
  useEffect(() => {
    if (!popover) return;
    const onDown = (e: PointerEvent) => {
      if (!rootRef.current?.contains(e.target as Node)) setPopover(false);
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== 'Escape') return;
      setPopover(false);
      caretRef.current?.focus();
    };
    document.addEventListener('pointerdown', onDown, true);
    document.addEventListener('keydown', onKey);
    return () => {
      document.removeEventListener('pointerdown', onDown, true);
      document.removeEventListener('keydown', onKey);
    };
  }, [popover]);

  return (
    <div ref={rootRef} className={styles.dock} data-testid="voice-dock" role="group" aria-label="Voice controls">
      <SpeakerSummary snapshot={snapshot} onOpenPanel={onOpenPanel} />
      <span className={styles.dockDivider} aria-hidden="true" />
      {s.phase !== 'connected' && <span className={styles.dockLabel}>Voice…</span>}
      {s.phase === 'connected' && canTalk && s.talkMode === 'ptt' && (
        <HoldToTalk
          actions={actions}
          held={s.pttHeld}
          className={styles.dockHold}
          label={s.pttHeld ? 'Talking…' : `Hold ${pttKeyLabel(s.pttKey)}`}
        />
      )}
      {canTalk && (
        <span className={styles.split} data-open={popover ? 'true' : 'false'}>
          <MicToggle actions={actions} s={s} />
          <IconButton
            ref={caretRef}
            label="Microphone settings"
            className={styles.caret}
            aria-haspopup="dialog"
            aria-expanded={popover}
            onClick={() => setPopover((o) => !o)}
          >
            <span aria-hidden="true">▴</span>
          </IconButton>
        </span>
      )}
      {/* After the caret in the DOM, so Tab goes from the caret into it. */}
      {popover && canTalk && (
        <MicPopover
          actions={actions}
          onOpenPanel={() => {
            setPopover(false);
            onOpenPanel();
          }}
        />
      )}
      <DeafenToggle actions={actions} s={s} />
    </div>
  );
}

// ---- The phone's thumb bar ---------------------------------------------------

// The dock on a phone: deafen, and the biggest target is the one used most —
// Hold to talk in push to talk, Mute in voice activity. Leave is in the sheet.
export function VoiceThumbBar({ actions, raised }: { actions: VoiceActions; raised: boolean }) {
  const s = useVoiceStore();
  if (!inVoice(s.phase)) return null;
  const forced = s.serverMuted || s.suppressed || s.serverDeafened;
  return (
    <div className={styles.thumbBar} data-raised={raised ? 'true' : 'false'} data-testid="voice-dock" role="group" aria-label="Voice controls">
      <DeafenToggle actions={actions} s={s} className={styles.thumbRound} />
      {s.listenOnly ? (
        <span className={styles.thumbNote}>Listening only</span>
      ) : s.phase === 'connected' && s.talkMode === 'ptt' ? (
        <HoldToTalk actions={actions} held={s.pttHeld} className={styles.thumbMain} />
      ) : (
        <button
          type="button"
          className={styles.thumbMain}
          data-off={micOff(s) ? 'true' : 'false'}
          aria-pressed={micOff(s)}
          aria-label={micLabel(s)}
          disabled={forced || s.selfDeafened}
          onClick={() => actions.setMuted(!s.selfMuted)}
        >
          {micOff(s) ? <MicOffIcon /> : <MicIcon />} {micOff(s) ? 'Unmute' : 'Mute'}
        </button>
      )}
    </div>
  );
}
