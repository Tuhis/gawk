import { useCallback, useEffect, useMemo, useRef, useState, type CSSProperties, type ReactNode } from 'react';
import styles from './room.module.css';
import { Button } from '../../ui/Button';
import { GlassPanel } from '../../ui/GlassPanel';
import { IconButton } from '../../ui/IconButton';
import { ContextMenu, type MenuItem } from '../../ui/ContextMenu';
import { Toast } from '../../ui/Toast';
import {
  CopyIcon,
  FocusIcon,
  FullscreenExitIcon,
  FullscreenIcon,
  GridIcon,
  LeaveIcon,
  MoreIcon,
  PeopleIcon,
  SpeakerIcon,
  SpeakerMutedIcon,
  VideoOffIcon,
  CloseIcon,
} from '../../ui/Icons';
import { PRESETS, presetConfig, presetLabel, type PresetId } from '../viewer/playbackPresets';
import { useAutoHide } from '../../lib/useAutoHide';
import { useFullscreen } from '../../lib/useFullscreen';
import { useHotkey } from '../../lib/useHotkey';
import { useWakeLock } from '../../lib/useWakeLock';
import { buildRoomLink } from '../../lib/shareLink';
import { HOME } from '../../routing';
import { ROOM_CLIENT_WEB_BROADCASTER, ROOM_CLIENT_WEB_VIEWER, type RoomAttachment } from '../../transport/wire';
import type { RoomTarget } from '../../transport/room-session';
import { isDynamicRoom, isRoomCreator, mayAttach, useRoomStore } from '../../state/roomStore';
import { ServerIndicator } from '../servers/ServerIndicator';
import { NicknamePrompt } from './NicknamePrompt';
import { RoomPanel } from './RoomPanel';
import { OwnPreviewTile, RoomTile } from './RoomTile';
import { RoomAudioMixer } from './roomAudio';
import { EMPTY_ROOM_CARD, HIDDEN_CARD, endedCard, errorCard, rejectionToast, removalToast } from './roomCopy';
import { loadNickname, loadRoomMode, loadRoomPreset, saveNickname, saveRoomMode, saveRoomPreset, type RoomMode } from './roomPrefs';
import { readGrant, type RoomGrant } from './grantHandoff';
import { stashRoomReturn } from './roomReturn';
import { useRoomSession } from './useRoomSession';

// The viewer's idle period (ViewerScreen CONTROL_IDLE_MS): the dock fades
// after the same 3 s.
const CONTROL_IDLE_MS = 3000;
const TOAST_MS = 4000;
const NO_ATTACHMENTS: RoomAttachment[] = [];
// Below this width the grid degrades to focus (docs/44 §4.9 "mobile"); the
// same breakpoint room.module.css uses for the bottom-sheet panel.
export const NARROW_QUERY = '(max-width: 719px)';

// RM5 (docs/44 §4.8): what the web broadcaster brings into the room — its
// running broadcast (attached with the resume token as proof), the local
// preview for its own tile, and the controls that ride on that tile.
export interface OwnBroadcast {
  broadcastId: string;
  resumeTokenHex: string;
  label: string;
  // Bumped after a publish auto-resume: the relay's grace GC may have seen
  // the publisher die, and the attach is re-sent (idempotent) to be sure.
  attachEpoch: number;
  preview: MediaStream | null;
  controls: ReactNode;
  onDetach: () => void;
}

export interface RoomViewProps {
  target: RoomTarget;
  grant?: RoomGrant | null;
  own?: OwnBroadcast | null;
  // Leaving the room. The route version goes home; the broadcaster returns
  // to its live page with the broadcast still running.
  onLeave?: () => void;
  // Viewer only: "start streaming here" (docs/44 §4.8).
  onStartStreaming?: () => void;
}

function useMediaMatch(query: string): boolean {
  const [matches, setMatches] = useState(() => {
    if (typeof window === 'undefined' || typeof window.matchMedia !== 'function') return false;
    return window.matchMedia(query).matches;
  });
  useEffect(() => {
    if (typeof window === 'undefined' || typeof window.matchMedia !== 'function') return;
    const mq = window.matchMedia(query);
    const onChange = () => setMatches(mq.matches);
    onChange();
    mq.addEventListener?.('change', onChange);
    return () => mq.removeEventListener?.('change', onChange);
  }, [query]);
  return matches;
}

function bytesToHex(bytes: Uint8Array): string {
  let out = '';
  for (const b of bytes) out += b.toString(16).padStart(2, '0');
  return out;
}

// R42 (docs/44 §4.9 revision): the cinematic dock. Video edge to edge, a
// header and a footer overlay that fade after the viewer's idle period, an
// optional pinnable people-and-chat panel. Three modes: grid (every POV, all
// mixed), focus (one large + the rest small in a glass strip, focused audio
// only), hide videos (no media sessions at all — the control session stays).
export function RoomView({ target, grant = null, own = null, onLeave, onStartStreaming }: RoomViewProps) {
  const rootRef = useRef<HTMLDivElement | null>(null);
  const status = useRoomStore((s) => s.status);
  const snapshot = useRoomStore((s) => s.snapshot);
  const retryNote = useRoomStore((s) => s.retryNote);
  const endReason = useRoomStore((s) => s.endReason);
  const errorKind = useRoomStore((s) => s.errorKind);
  const lastRejection = useRoomStore((s) => s.lastRejection);
  const lastRemoval = useRoomStore((s) => s.lastRemoval);
  const clearRejection = useRoomStore((s) => s.clearRejection);

  // D10: the nickname, asked once before the first dial and remembered.
  const [nickname, setNicknameState] = useState<string | null>(loadNickname);
  const [guest, setGuest] = useState(false);
  const [editingNick, setEditingNick] = useState(false);
  const ready = nickname !== null || guest;

  const commands = useRoomSession({
    target: ready ? target : null,
    nickname: nickname ?? '',
    clientKind: own ? ROOM_CLIENT_WEB_BROADCASTER : ROOM_CLIENT_WEB_VIEWER,
    grant,
  });

  // RM5: attach (and re-attach) the broadcaster's own broadcast. Idempotent
  // on the relay — a mint's first snapshot already carries it, and a
  // reconnected broadcaster must re-attach before it can detach (RM2).
  const joined = status === 'joined';
  const attachOk = mayAttach(snapshot);
  const ownId = own?.broadcastId ?? null;
  const ownToken = own?.resumeTokenHex ?? null;
  const ownLabel = own?.label ?? '';
  const ownEpoch = own?.attachEpoch ?? 0;
  useEffect(() => {
    if (!joined || !attachOk || ownId === null || ownToken === null) return;
    commands.attach(ownId, ownToken, ownLabel);
  }, [joined, attachOk, ownId, ownToken, ownLabel, ownEpoch, commands]);

  // Layout mode, persisted; the grid degrades to focus on a narrow screen.
  const [mode, setModeState] = useState<RoomMode>(loadRoomMode);
  const narrow = useMediaMatch(NARROW_QUERY);
  const effectiveMode: RoomMode = mode === 'grid' && narrow ? 'focus' : mode;
  const setMode = useCallback((next: RoomMode) => {
    setModeState(next);
    saveRoomMode(next);
  }, []);

  const attachments: RoomAttachment[] = snapshot?.attachments ?? NO_ATTACHMENTS;
  const [focusedId, setFocusedId] = useState<string | null>(null);
  const focused = attachments.find((a) => a.broadcastId === focusedId) ?? attachments[0] ?? null;
  const focus = useCallback(
    (id: string) => {
      setFocusedId(id);
      setMode('focus');
    },
    [setMode],
  );
  const focusIndex = useCallback(
    (i: number) => {
      const a = attachments[i];
      if (a) focus(a.broadcastId);
    },
    [attachments, focus],
  );
  useHotkey({ key: '1' }, () => focusIndex(0));
  useHotkey({ key: '2' }, () => focusIndex(1));
  useHotkey({ key: '3' }, () => focusIndex(2));
  useHotkey({ key: '4' }, () => focusIndex(3));
  useHotkey({ key: '0' }, () => setMode('grid'));

  // R32 presets per tile: the focused / grid tiles on the user's choice, the
  // small tiles on the cheapest (docs/44 §4.7).
  const [preset, setPresetState] = useState<PresetId>(loadRoomPreset);
  const setPreset = useCallback((id: PresetId) => {
    setPresetState(id);
    saveRoomPreset(id);
  }, []);
  const mainConfig = useMemo(() => presetConfig(preset), [preset]);
  const smallConfig = useMemo(() => presetConfig('lowest'), []);

  // The mixer (docs/44 §4.7): one AudioContext, one master gain, opened on
  // the first tile shown and closed with the screen.
  const mixerRef = useRef<RoomAudioMixer | null>(null);
  mixerRef.current ??= new RoomAudioMixer();
  useEffect(() => {
    const mixer = mixerRef.current;
    return () => {
      mixer?.dispose();
      mixerRef.current = null;
    };
  }, []);
  const tilesShown = joined && effectiveMode !== 'hidden' && attachments.length > 0;
  const audioOutput = useMemo(() => (tilesShown ? (mixerRef.current?.output() ?? null) : null), [tilesShown]);
  const [masterMuted, setMasterMuted] = useState(false);
  const [masterVolume, setMasterVolume] = useState(1);
  const setMaster = useCallback((v: number) => {
    setMasterVolume(v);
    setMasterMuted(false);
    mixerRef.current?.setMasterVolume(v);
    mixerRef.current?.setMasterMuted(false);
  }, []);
  const toggleMasterMute = useCallback(() => {
    setMasterMuted((m) => {
      mixerRef.current?.setMasterMuted(!m);
      return !m;
    });
  }, []);
  // The shared context is gesture-gated once for every tile; the mixer's
  // state is not reactive, so it is sampled while tiles are shown.
  const [needsGesture, setNeedsGesture] = useState(false);
  useEffect(() => {
    if (!tilesShown) {
      setNeedsGesture(false);
      return;
    }
    const tick = () => setNeedsGesture(mixerRef.current?.needsGesture ?? false);
    tick();
    const t = setInterval(tick, 1000);
    return () => clearInterval(t);
  }, [tilesShown]);

  // The people-and-chat panel: open from the header, pinnable so it stays
  // while the chrome fades; a bottom sheet on a phone (CSS).
  const [panelOpen, setPanelOpen] = useState(false);
  const [pinned, setPinned] = useState(false);

  const [menu, setMenu] = useState<{ x: number; y: number } | null>(null);
  const [presetMenu, setPresetMenu] = useState<{ x: number; y: number } | null>(null);
  const menuButtonRef = useRef<HTMLButtonElement | null>(null);
  const presetButtonRef = useRef<HTMLButtonElement | null>(null);

  const [toast, setToast] = useState<string | null>(null);
  const toastTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  const showToast = useCallback((text: string) => {
    setToast(text);
    if (toastTimer.current !== null) clearTimeout(toastTimer.current);
    toastTimer.current = setTimeout(() => setToast(null), TOAST_MS);
  }, []);
  useEffect(() => () => {
    if (toastTimer.current !== null) clearTimeout(toastTimer.current);
  }, []);

  // Every relay state has a visible form (docs/44 §4.9): removals and
  // rejections are toasts, the rest are cards and pills below.
  useEffect(() => {
    if (!lastRemoval) return;
    showToast(removalToast(lastRemoval.label, lastRemoval.reason));
  }, [lastRemoval, showToast]);
  useEffect(() => {
    if (!lastRejection) return;
    showToast(rejectionToast(lastRejection.command, lastRejection.reason, lastRejection.message));
    clearRejection();
  }, [lastRejection, clearRejection, showToast]);

  const { isFullscreen, tier, toggle: toggleFullscreen } = useFullscreen(rootRef);
  useHotkey({ key: 'f' }, () => toggleFullscreen());
  useWakeLock(tilesShown);

  const anyOverlayOpen = !!menu || !!presetMenu || editingNick;
  const chromeVisible = useAutoHide(CONTROL_IDLE_MS, tilesShown && !anyOverlayOpen);
  const showChrome = chromeVisible || !tilesShown || anyOverlayOpen;

  const code = snapshot?.code ?? (target.kind === 'join' ? target.code : '');
  const roomKey = snapshot && snapshot.key.length > 0 ? bytesToHex(snapshot.key) : null;
  const you = snapshot?.participants.find((p) => p.id === snapshot.yourId) ?? null;
  const shownNickname = you?.nickname ?? nickname ?? '';

  const leave = useCallback(() => {
    if (onLeave) onLeave();
    else window.location.hash = HOME;
  }, [onLeave]);

  const copyLink = useCallback(() => {
    if (code === '') return;
    void navigator.clipboard?.writeText(buildRoomLink(code)).then(() => showToast('Room link copied'));
  }, [code, showToast]);
  const copyCode = useCallback(() => {
    if (code === '') return;
    void navigator.clipboard?.writeText(code).then(() => showToast('Room code copied'));
  }, [code, showToast]);

  const submitNickname = useCallback(
    (n: string) => {
      saveNickname(n);
      setNicknameState(n);
      setEditingNick(false);
      if (status !== 'idle' && status !== 'connecting') commands.setNickname(n);
    },
    [commands, status],
  );

  const startStreaming = useCallback(() => {
    if (code !== '') stashRoomReturn(code);
    onStartStreaming?.();
  }, [code, onStartStreaming]);

  const creator = isRoomCreator(snapshot);
  const dynamic = isDynamicRoom(snapshot);
  const streaming = attachments.length;
  const people = snapshot?.participants.length ?? 0;

  const presetItems: MenuItem[] = PRESETS.map((p) => ({
    label: p.label,
    note: p.sub,
    checked: preset === p.id,
    onSelect: () => setPreset(p.id),
  }));
  const menuItems: MenuItem[] = [
    { label: 'Copy room link', onSelect: copyLink },
    ...(dynamic ? [{ label: 'Copy room code', onSelect: copyCode }] : []),
    { label: 'Change nickname…', onSelect: () => setEditingNick(true) },
    { label: isFullscreen ? 'Exit fullscreen' : 'Fullscreen', onSelect: () => toggleFullscreen() },
    ...(creator && dynamic ? [{ label: 'End room', onSelect: () => commands.endRoom() }] : []),
    {
      label: 'Terms of use',
      onSelect: () => window.open(`${window.location.origin}${window.location.pathname}#/terms`, '_blank', 'noopener'),
    },
    { label: 'Leave room', onSelect: leave },
  ];

  // Detaching the own broadcast: the command goes out here (the session is
  // this screen's), the owner then drops its `own` so no re-attach follows.
  const detachOwn = useCallback(() => {
    if (!own) return;
    commands.detach(own.broadcastId);
    own.onDetach();
  }, [own, commands]);
  const ownControls = own ? (
    <>
      <span className={styles.ownBarLabel}>You</span>
      {own.controls}
      <IconButton label="Detach from room" onClick={detachOwn}>
        <CloseIcon />
      </IconButton>
    </>
  ) : null;

  // The stage: one flat list keyed by broadcast ID so a mode switch moves
  // tiles instead of remounting their media sessions. Hidden ⇒ no tiles.
  const cols = Math.max(1, Math.ceil(Math.sqrt(attachments.length)));
  let strip = 0;
  const tiles =
    !tilesShown
      ? null
      : attachments.map((a, i) => {
          const isFocused = effectiveMode === 'focus' && focused?.broadcastId === a.broadcastId;
          const variant = effectiveMode === 'grid' ? 'grid' : isFocused ? 'focus' : 'small';
          const stripIndex = variant === 'small' ? strip++ : undefined;
          if (own && own.preview && a.broadcastId === own.broadcastId) {
            return (
              <OwnPreviewTile
                key={a.broadcastId}
                attachment={a}
                index={i + 1}
                stripIndex={stripIndex}
                variant={variant}
                preview={own.preview}
                ownControls={ownControls}
                showChrome={showChrome}
                onFocus={focus}
              />
            );
          }
          return (
            <RoomTile
              key={a.broadcastId}
              attachment={a}
              index={i + 1}
              stripIndex={stripIndex}
              variant={variant}
              config={variant === 'small' ? smallConfig : mainConfig}
              audioOutput={audioOutput}
              suppressed={effectiveMode === 'focus' && !isFocused}
              own={own?.broadcastId === a.broadcastId}
              ownControls={ownControls}
              showChrome={showChrome}
              roomKey={roomKey}
              onFocus={focus}
            />
          );
        });

  const card = (title: string, body: string, actions?: ReactNode) => (
    <div className={styles.center} data-panel={panelOpen ? 'true' : 'false'}>
      <GlassPanel className={styles.card}>
        <h2 className={styles.cardTitle}>{title}</h2>
        <p className={styles.cardText}>{body}</p>
        {actions && <div className={styles.cardActions}>{actions}</div>}
      </GlassPanel>
    </div>
  );

  const panelVisible = panelOpen && (pinned || showChrome);

  return (
    <div
      ref={rootRef}
      className={[styles.root, isFullscreen && tier === 'pseudo' ? styles.pseudoFullscreen : ''].join(' ')}
      data-mode={effectiveMode}
      data-status={status}
      onContextMenu={(e) => {
        e.preventDefault();
        setMenu({ x: e.clientX, y: e.clientY });
      }}
    >
      <div
        className={styles.stage}
        data-mode={effectiveMode}
        data-panel={panelOpen ? 'true' : 'false'}
        style={{ '--cols': cols } as CSSProperties}
      >
        {tiles}
      </div>

      {(status === 'idle' || status === 'connecting') && ready && (
        <div className={styles.center}>
          <GlassPanel className={styles.card}>
            <div className={styles.spinner} aria-hidden="true" />
            <p className={styles.cardText}>{target.kind === 'mint' ? 'Creating the room…' : 'Joining the room…'}</p>
          </GlassPanel>
        </div>
      )}
      {status === 'error' &&
        errorKind &&
        card(
          errorCard(errorKind).title,
          errorCard(errorKind).body,
          <>
            <Button variant="secondary" onClick={() => window.location.reload()}>
              Retry
            </Button>
            <Button onClick={leave}>Home</Button>
          </>,
        )}
      {status === 'ended' && card(endedCard(endReason).title, endedCard(endReason).body, <Button onClick={leave}>Leave</Button>)}
      {(joined || status === 'reconnecting') &&
        effectiveMode === 'hidden' &&
        card(
          HIDDEN_CARD.title,
          HIDDEN_CARD.body,
          <>
            <Button variant="secondary" onClick={() => setMode('grid')}>
              <GridIcon /> Grid
            </Button>
            <Button variant="secondary" onClick={() => setMode('focus')}>
              <FocusIcon /> Focus
            </Button>
          </>,
        )}
      {(joined || status === 'reconnecting') &&
        effectiveMode !== 'hidden' &&
        attachments.length === 0 &&
        card(
          EMPTY_ROOM_CARD.title,
          EMPTY_ROOM_CARD.body,
          onStartStreaming ? <Button onClick={startStreaming}>Start streaming here</Button> : undefined,
        )}

      {status === 'reconnecting' && retryNote && (
        <div className={styles.topPill}>
          <span className={styles.pulse} aria-hidden="true" />
          {retryNote}
        </div>
      )}

      {needsGesture && tilesShown && (
        <button
          className={styles.unmutePrompt}
          onClick={() => {
            void mixerRef.current?.resume().then(() => setNeedsGesture(mixerRef.current?.needsGesture ?? false));
          }}
        >
          <SpeakerIcon /> Tap for sound
        </button>
      )}

      {toast && <Toast>{toast}</Toast>}

      <div className={[styles.header, showChrome ? '' : styles.headerHidden].join(' ')}>
        <div className={styles.headerLeft}>
          <span className={styles.code} title="Room code">
            {code}
          </span>
          <span className={styles.count} data-live={streaming > 0 ? 'true' : 'false'}>
            {streaming} streaming
          </span>
          <span className={styles.count}>
            <PeopleIcon /> {people}
          </span>
        </div>
        <div className={styles.headerRight}>
          <IconButton label="Copy room link" onClick={copyLink}>
            <CopyIcon />
          </IconButton>
          <IconButton
            label={panelOpen ? 'Hide people and chat' : 'People and chat'}
            aria-pressed={panelOpen}
            onClick={() => setPanelOpen((o) => !o)}
          >
            <PeopleIcon />
          </IconButton>
          <IconButton label={isFullscreen ? 'Exit fullscreen' : 'Fullscreen'} onClick={() => toggleFullscreen()}>
            {isFullscreen ? <FullscreenExitIcon /> : <FullscreenIcon />}
          </IconButton>
        </div>
      </div>

      <div className={[styles.footer, showChrome ? '' : styles.footerHidden].join(' ')}>
        <div className={styles.footerLeft}>
          <div className={styles.segment} role="radiogroup" aria-label="Layout">
            <button
              type="button"
              role="radio"
              aria-checked={mode === 'grid'}
              className={styles.segmentBtn}
              onClick={() => setMode('grid')}
              title={narrow ? 'Grid shows as focus on a narrow screen' : undefined}
            >
              <GridIcon /> Grid
            </button>
            <button
              type="button"
              role="radio"
              aria-checked={mode === 'focus'}
              className={styles.segmentBtn}
              onClick={() => setMode('focus')}
            >
              <FocusIcon /> Focus
            </button>
            <button
              type="button"
              role="radio"
              aria-checked={mode === 'hidden'}
              className={styles.segmentBtn}
              onClick={() => setMode('hidden')}
            >
              <VideoOffIcon /> Hide videos
            </button>
          </div>
          <button
            ref={presetButtonRef}
            type="button"
            className={styles.presetPill}
            aria-haspopup="menu"
            aria-expanded={presetMenu != null}
            aria-label={`Playback quality: ${presetLabel(preset)}`}
            onClick={(e) => {
              if (presetMenu) {
                setPresetMenu(null);
                return;
              }
              const r = e.currentTarget.getBoundingClientRect();
              setMenu(null);
              setPresetMenu({ x: r.right, y: r.top - 6 });
            }}
          >
            {presetLabel(preset)}
            <span className={styles.presetCaret} aria-hidden="true">
              ▾
            </span>
          </button>
        </div>
        <div className={styles.footerRight}>
          <span className={styles.master}>
            <IconButton label={masterMuted ? 'Unmute all' : 'Mute all'} onClick={toggleMasterMute}>
              {masterMuted ? <SpeakerMutedIcon /> : <SpeakerIcon />}
            </IconButton>
            <input
              className={styles.masterVolume}
              type="range"
              min={0}
              max={1}
              step={0.01}
              value={masterMuted ? 0 : masterVolume}
              aria-label="Master volume"
              onChange={(e) => setMaster(Number(e.target.value))}
            />
          </span>
          <IconButton
            ref={menuButtonRef}
            label="More options"
            aria-haspopup="menu"
            aria-expanded={menu != null}
            onClick={(e) => {
              if (menu) {
                setMenu(null);
                return;
              }
              const r = e.currentTarget.getBoundingClientRect();
              setPresetMenu(null);
              setMenu({ x: r.right, y: r.top - 6 });
            }}
          >
            <MoreIcon />
          </IconButton>
          <IconButton label="Leave room" className={styles.leaveBtn} onClick={leave}>
            <LeaveIcon />
          </IconButton>
        </div>
      </div>

      {panelOpen && snapshot && (
        <div className={panelVisible ? '' : styles.chromeHidden} data-testid="room-panel-wrap">
          <RoomPanel
            snapshot={snapshot}
            nickname={shownNickname}
            pinned={pinned}
            onPin={() => setPinned((p) => !p)}
            onClose={() => setPanelOpen(false)}
            onDetach={(id) => (own && id === own.broadcastId ? detachOwn() : commands.detach(id))}
            onSetNickname={submitNickname}
            onCopyLink={copyLink}
            onCopyCode={copyCode}
            onEndRoom={() => commands.endRoom()}
            ownBroadcastId={own?.broadcastId ?? null}
            onStartStreaming={onStartStreaming && !own ? startStreaming : null}
          />
        </div>
      )}

      {menu && (
        <ContextMenu items={menuItems} x={menu.x} y={menu.y} anchor="bottom-right" anchorRef={menuButtonRef} onClose={() => setMenu(null)} />
      )}
      {presetMenu && (
        <ContextMenu
          items={presetItems}
          x={presetMenu.x}
          y={presetMenu.y}
          anchor="bottom-right"
          anchorRef={presetButtonRef}
          onClose={() => setPresetMenu(null)}
        />
      )}

      {!ready && (
        <NicknamePrompt
          onSubmit={(n) => {
            saveNickname(n);
            setNicknameState(n);
          }}
          onSkip={() => setGuest(true)}
        />
      )}
      {editingNick && (
        <NicknamePrompt initial={shownNickname} editing onSubmit={submitNickname} onCancel={() => setEditingNick(false)} />
      )}

      <ServerIndicator />
    </div>
  );
}

// The `#/room/<code>` route (docs/44 D19): a participant joining by link.
// The grant, if the link carried one, was moved into session storage by
// App.tsx before this mounted (grantHandoff.ts).
export function RoomScreen({ code }: { code: string }) {
  const target = useMemo<RoomTarget>(() => ({ kind: 'join', code }), [code]);
  const [grant] = useState(() => readGrant(code));
  return (
    <RoomView
      target={target}
      grant={grant}
      onStartStreaming={() => {
        window.location.hash = '#/broadcast';
      }}
    />
  );
}
