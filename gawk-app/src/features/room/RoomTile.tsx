import { useCallback, useEffect, useRef, useState, type CSSProperties, type MouseEvent, type ReactNode } from 'react';
import { createPortal } from 'react-dom';
import styles from './room.module.css';
import { Button } from '../../ui/Button';
import { IconButton } from '../../ui/IconButton';
import { OpenIcon, SpeakerIcon, SpeakerMutedIcon } from '../../ui/Icons';
import { useViewerConnection } from '../viewer/useViewerConnection';
import { isFullErrorKind } from '../viewer/fullRefusal';
import { StatsOverlay } from '../viewer/StatsOverlay';
import { DiagnosticsBuffer } from '../../lib/diagnostics';
import type { ViewerStats } from '../../transport/viewer';
import type { AudioOutput } from '../viewer/audioSink';
import type { PlaybackConfig } from '../viewer/playbackPresets';
import type { RoomAttachment } from '../../transport/wire';
import { buildViewLink } from '../../lib/shareLink';
import { loadTileVolume, saveTileVolume } from './roomPrefs';
import { TILE_AUTO_RETRIES, TILE_RETRY_DELAYS_MS } from './tileRetry';
import type { Rect } from './fitLayout';

export type TileVariant = 'grid' | 'focus' | 'small';

// Grid only: which stage edges the tile touches (RoomScreen computes it from
// the tile's row). The label steps below the header band on a top-row tile
// and the controls above the footer band on a bottom-row tile; the video
// itself stays edge to edge.
export interface TileEdges {
  top: boolean;
  bottom: boolean;
  // Fit only (docs/87 D10): how far, in px, the label and the controls step
  // in from the tile's own top and bottom to clear the room's chrome bands.
  topOffset?: number;
  bottomOffset?: number;
}

// Reports the stream's decoded size (docs/87 D7); the room keeps the shape.
export type AspectReport = (broadcastId: string, width: number, height: number) => void;

interface FrameProps {
  attachment: RoomAttachment;
  // 1-based: the number key that focuses this POV.
  index: number;
  // Position in the focus strip (small tiles only).
  stripIndex?: number;
  variant: TileVariant;
  edges?: TileEdges;
  // Fit only: the tile's box on the stage, in px.
  box?: Rect;
  // The decoded frame size, for the e2e (data-frame-w/h).
  frameSize?: { width: number; height: number } | null;
  own: boolean;
  ownControls?: ReactNode;
  showChrome: boolean;
  status: string;
  frames: number;
  onFocus: (broadcastId: string) => void;
  // The tile's own audio controls (grid: a volume pill, focus: a mute), or
  // nothing when the stream carries no audio.
  audioControls?: ReactNode;
  // What is painted: the remote canvas or the broadcaster's local preview.
  surface: ReactNode;
  // Centred state text, if any.
  state?: ReactNode;
  // A right-click on the tile; absent ⇒ the room's own menu handles it.
  onContextMenu?: (e: MouseEvent<HTMLDivElement>) => void;
}

// The tile shell shared by a remote POV and the broadcaster's own preview:
// the number badge + label, the state overlay, the bottom-right controls,
// the "open full-screen" #/view/ link, and the own glass bar.
function TileFrame({
  attachment,
  index,
  stripIndex,
  variant,
  edges,
  box,
  frameSize,
  own,
  ownControls,
  showChrome,
  status,
  frames,
  onFocus,
  audioControls,
  surface,
  state,
  onContextMenu,
}: FrameProps) {
  const { broadcastId } = attachment;
  const away = !attachment.live;
  const chromeCls = showChrome ? '' : styles.chromeHidden;
  const small = variant === 'small';
  const style: Record<string, string | number> = {};
  if (stripIndex !== undefined) style['--i'] = stripIndex;
  if (box) {
    style.left = `${box.x}px`;
    style.top = `${box.y}px`;
    style.width = `${box.w}px`;
    style.height = `${box.h}px`;
  }
  if (edges?.topOffset !== undefined) style['--edge-top'] = `${edges.topOffset}px`;
  if (edges?.bottomOffset !== undefined) style['--edge-bottom'] = `${edges.bottomOffset}px`;
  return (
    <div
      className={styles.tile}
      style={Object.keys(style).length > 0 ? (style as CSSProperties) : undefined}
      data-testid="room-tile"
      data-broadcast-index={index}
      data-variant={variant}
      data-edge-top={edges?.top ? 'true' : undefined}
      data-edge-bottom={edges?.bottom ? 'true' : undefined}
      data-frame-w={frameSize ? frameSize.width : undefined}
      data-frame-h={frameSize ? frameSize.height : undefined}
      data-own={own ? 'true' : undefined}
      data-focused={variant === 'focus' ? 'true' : undefined}
      data-status={status}
      data-frames={frames}
      role={small ? 'button' : undefined}
      tabIndex={small ? 0 : undefined}
      aria-label={small ? `Focus ${attachment.label || `stream ${index}`}` : undefined}
      onClick={small ? () => onFocus(broadcastId) : undefined}
      onContextMenu={onContextMenu}
      onKeyDown={
        small
          ? (e) => {
              if (e.key === 'Enter' || e.key === ' ') {
                e.preventDefault();
                onFocus(broadcastId);
              }
            }
          : undefined
      }
    >
      {surface}

      <div className={`${styles.tileHead} ${chromeCls}`}>
        <span className={styles.tileKey} aria-hidden="true">
          {index}
        </span>
        {/* No per-POV viewer count here: the header carries the room's
            totals and the panel the per-stream figure. */}
        <span className={styles.tileLabel} data-live={away ? 'false' : 'true'}>
          {attachment.label || broadcastId}
          {own && <span>· you</span>}
          {away && <span>· away</span>}
        </span>
      </div>

      {state && <div className={styles.tileState}>{state}</div>}

      {!small && (
        <div className={`${styles.tileFoot} ${chromeCls}`}>
          {audioControls}
          {/* A plain #/view/ link, in a new tab so the room stays. */}
          <a
            className={styles.openLink}
            href={buildViewLink(broadcastId)}
            target="_blank"
            rel="noopener noreferrer"
            aria-label={`Open ${attachment.label || broadcastId} full-screen`}
            title="Open full-screen"
          >
            <OpenIcon />
          </a>
        </div>
      )}

      {own && ownControls && !small && (
        <div className={`${styles.ownBar} ${chromeCls}`} data-testid="own-bar">
          {ownControls}
        </div>
      )}
    </div>
  );
}

interface Props {
  attachment: RoomAttachment;
  index: number;
  stripIndex?: number;
  variant: TileVariant;
  edges?: TileEdges;
  config: PlaybackConfig;
  audioOutput: AudioOutput | null;
  // Focus mode: every non-focused tile is silenced through its sink, never
  // torn down.
  suppressed: boolean;
  own: boolean;
  ownControls?: ReactNode;
  showChrome: boolean;
  roomKey: string | null;
  onFocus: (broadcastId: string) => void;
  box?: Rect;
  onAspect?: AspectReport;
  // The tile's stats: the room owns which tile's panel is open (one at a
  // time); the tile owns the stats and draws the panel into `statsHost`,
  // the room root, because a tile clips its content.
  statsOpen?: boolean;
  statsHost?: HTMLElement | null;
  onTileMenu?: (broadcastId: string, x: number, y: number) => void;
  onCloseStats?: () => void;
}

// What the current attempt's session is doing, as far as retrying goes.
// 'fatal' is an unplayable stream: a re-dial in the same browser fails the
// same way, so it is never retried.
type SessionPhase = 'pending' | 'watching' | 'dead' | 'fatal';

// One attached POV. The session lives in RoomTileSession; this keeps the
// attempt counter that keys it. The canvas transfers to the worker only
// once, so a retry is a remount with a new canvas, never a re-run of the
// connection effect.
export function RoomTile(props: Props) {
  const live = props.attachment.live;
  const [attempt, setAttempt] = useState(0);
  const [phase, setPhase] = useState<SessionPhase>('pending');
  // Attempts in a row that never reached 'watching' (sets the delay), and
  // the automatic retries left before only a tap re-dials.
  const streakRef = useRef(0);
  const budgetRef = useRef(TILE_AUTO_RETRIES);
  const wasLiveRef = useRef(live);

  const retry = useCallback(() => {
    streakRef.current += 1;
    setPhase('pending');
    setAttempt((a) => a + 1);
  }, []);

  const onPhase = useCallback((p: SessionPhase) => {
    if (p === 'watching') {
      streakRef.current = 0;
      budgetRef.current = TILE_AUTO_RETRIES;
    }
    setPhase(p);
  }, []);

  // Declared before the scheduler so a flip refills the budget first.
  useEffect(() => {
    if (live && !wasLiveRef.current) budgetRef.current = TILE_AUTO_RETRIES;
    wasLiveRef.current = live;
  }, [live]);

  useEffect(() => {
    if (phase !== 'dead' || !live || budgetRef.current <= 0) return;
    const delay = TILE_RETRY_DELAYS_MS[Math.min(streakRef.current, TILE_RETRY_DELAYS_MS.length - 1)];
    const t = setTimeout(() => {
      budgetRef.current -= 1;
      retry();
    }, delay);
    return () => clearTimeout(t);
  }, [phase, live, retry]);

  return <RoomTileSession key={attempt} {...props} onPhase={onPhase} onRetry={retry} />;
}

interface SessionProps extends Props {
  onPhase: (p: SessionPhase) => void;
  onRetry: () => void;
}

// One attempt's /subscribe session (useViewerConnection; the parent keys
// the tile on the broadcast ID so a mode switch never remounts it), its own
// volume pill in grid mode, the number-key badge, and the broadcaster's own
// glass bar when it is theirs. presentationMux stays off — the room offers
// no native (iPhone) fullscreen per tile; the "open full-screen" link is a
// plain #/view/ link for that.
function RoomTileSession({
  attachment,
  index,
  stripIndex,
  variant,
  edges,
  config,
  audioOutput,
  suppressed,
  own,
  ownControls,
  showChrome,
  roomKey,
  onFocus,
  box,
  onAspect,
  onPhase,
  onRetry,
  statsOpen = false,
  statsHost = null,
  onTileMenu,
  onCloseStats,
}: SessionProps) {
  const canvasRef = useRef<HTMLCanvasElement | null>(null);
  const { broadcastId } = attachment;
  // Read once per mount: the tile's level is its own persisted preference.
  const [initialVolume] = useState(() => loadTileVolume(broadcastId));
  const { status, stats, audio, popFull, endReason, errorFatal, errorKind, codec, telemetrySessionId } = useViewerConnection(
    broadcastId,
    canvasRef,
    config.playout,
    config.interpolation,
    false,
    config.delivery,
    config.parity === 'auto' ? undefined : config.parity,
    config.striping,
    {
      audioOutput: audioOutput ?? undefined,
      audioPrefs: 'session',
      // The broadcaster hears their own game already.
      initialMuted: own,
      initialVolume,
      roomKey,
    },
  );

  // docs/87 D7: the stream's shape, from the decoded frames themselves.
  const frameW = stats?.frameWidth ?? 0;
  const frameH = stats?.frameHeight ?? 0;
  useEffect(() => {
    if (frameW > 0 && frameH > 0) onAspect?.(broadcastId, frameW, frameH);
  }, [onAspect, broadcastId, frameW, frameH]);

  // The stats panel's sample window, as the standalone viewer keeps it: the
  // received bitrate needs two samples, so it fills whether or not the
  // panel is open. A retry's new session starts a new one.
  const [diag] = useState(() => new DiagnosticsBuffer<ViewerStats>());
  useEffect(() => {
    if (stats) diag.push(stats);
  }, [diag, stats]);
  const [statsCopied, setStatsCopied] = useState(false);
  const copyDiagnostics = useCallback(() => {
    const json = diag.build({ surface: 'viewer', broadcastId, codec, telemetrySessionId });
    void navigator.clipboard?.writeText(json).then(() => {
      setStatsCopied(true);
      setTimeout(() => setStatsCopied(false), 1800);
    });
  }, [diag, broadcastId, codec, telemetrySessionId]);

  const { setSuppressed } = audio;
  useEffect(() => {
    setSuppressed(suppressed);
  }, [suppressed, setSuppressed]);

  const setVolume = useCallback(
    (v: number) => {
      if (audio.muted && v > 0) audio.setMuted(false);
      audio.setVolume(v);
      saveTileVolume(broadcastId, v);
    },
    [audio, broadcastId],
  );

  const away = !attachment.live;
  const offline = status === 'error' || status === 'ended';
  // A 'stopped' end keeps redialling the code underneath (docs/79 D7), so
  // only an error or any other end leaves the session dead.
  const dead = status === 'error' || (status === 'ended' && endReason !== 'stopped');
  const fatal = status === 'error' && errorFatal;
  const small = variant === 'small';

  useEffect(() => {
    onPhase(fatal ? 'fatal' : dead ? 'dead' : status === 'watching' ? 'watching' : 'pending');
  }, [onPhase, fatal, dead, status]);

  let state: ReactNode = null;
  if (away) {
    state = <span>{small ? 'Away' : 'The streamer is away — their stream comes back when they do.'}</span>;
  } else if (status === 'connecting') {
    state = (
      <>
        <div className={`${styles.spinner} ${styles.spinnerSmall}`} aria-hidden="true" />
        {!small && <span>Connecting…</span>}
      </>
    );
  } else if (offline) {
    state = (
      <>
        <span>{small ? 'Offline' : 'This stream is offline right now.'}</span>
        {/* A small tile is itself the focus button; Retry is on the
            focused one. */}
        {dead && !fatal && !small && (
          <Button variant="secondary" className={styles.tileRetry} onClick={onRetry}>
            Retry
          </Button>
        )}
      </>
    );
  } else if (popFull) {
    // docs/76 item 35: past the party's internet budget. The session keeps
    // retrying every 20 s and the tile joins when a slot frees; the label is
    // on the tile's own bar.
    state = <span>Party-only right now</span>;
  } else if (status === 'reconnecting' && isFullErrorKind(errorKind)) {
    // 4018/4019: the broadcast or the relay is at its viewer limit. The
    // session retries every 20 s and the tile joins when a slot frees.
    state = <span>Full right now</span>;
  } else if (status === 'reconnecting') {
    state = <span>Reconnecting…</span>;
  }

  const muteButton = (
    <IconButton
      label={audio.muted ? `Unmute ${attachment.label || broadcastId}` : `Mute ${attachment.label || broadcastId}`}
      onClick={() => audio.setMuted(!audio.muted)}
    >
      {audio.muted ? <SpeakerMutedIcon /> : <SpeakerIcon />}
    </IconButton>
  );
  const audioControls = !audio.present ? null : variant === 'grid' ? (
    <span className={styles.volumePill}>
      {muteButton}
      <input
        type="range"
        min={0}
        max={1}
        step={0.01}
        value={audio.muted ? 0 : audio.volume}
        aria-label={`Volume for ${attachment.label || broadcastId}`}
        onChange={(e) => setVolume(Number(e.target.value))}
      />
    </span>
  ) : (
    muteButton
  );

  const onContextMenu = onTileMenu
    ? (e: MouseEvent<HTMLDivElement>) => {
        e.preventDefault();
        e.stopPropagation();
        onTileMenu(broadcastId, e.clientX, e.clientY);
      }
    : undefined;

  return (
    <>
      <TileFrame
        attachment={attachment}
        index={index}
        stripIndex={stripIndex}
        variant={variant}
        edges={edges}
        box={box}
        frameSize={frameW > 0 && frameH > 0 ? { width: frameW, height: frameH } : null}
        own={own}
        ownControls={ownControls}
        showChrome={showChrome}
        status={status}
        frames={stats?.framesCompleted ?? 0}
        onFocus={onFocus}
        audioControls={audioControls}
        surface={<canvas ref={canvasRef} className={styles.tileCanvas} />}
        state={state}
        onContextMenu={onContextMenu}
      />
      {statsOpen &&
        statsHost &&
        createPortal(
          <StatsOverlay
            stats={stats}
            codec={codec}
            bitrateBps={(() => {
              const bytesRate = diag.rate((s) => s.videoBytesReceived);
              return bytesRate == null ? null : bytesRate * 8;
            })()}
            telemetrySessionId={telemetrySessionId}
            streamName={attachment.label || broadcastId}
            footer="Right-click the tile to toggle"
            onClose={() => onCloseStats?.()}
            onCopy={copyDiagnostics}
            copied={statsCopied}
          />,
          statsHost,
        )}
    </>
  );
}

interface OwnProps {
  attachment: RoomAttachment;
  index: number;
  stripIndex?: number;
  variant: TileVariant;
  edges?: TileEdges;
  box?: Rect;
  preview: MediaStream;
  ownControls?: ReactNode;
  showChrome: boolean;
  onFocus: (broadcastId: string) => void;
  onAspect?: AspectReport;
}

// The web broadcaster's own tile paints the LOCAL
// capture preview — no /subscribe session to their own broadcast, so the
// room costs them no extra uplink and shows no relay round-trip; and it is
// muted by construction (they hear their game already).
export function OwnPreviewTile({
  attachment,
  index,
  stripIndex,
  variant,
  edges,
  box,
  preview,
  ownControls,
  showChrome,
  onFocus,
  onAspect,
}: OwnProps) {
  const videoRef = useRef<HTMLVideoElement | null>(null);
  const [frameSize, setFrameSize] = useState<{ width: number; height: number } | null>(null);
  // docs/87 D7: the local preview's shape is the video's own size.
  const { broadcastId } = attachment;
  useEffect(() => {
    const el = videoRef.current;
    if (!el) return;
    const read = () => {
      const width = el.videoWidth;
      const height = el.videoHeight;
      if (!(width > 0 && height > 0)) return;
      setFrameSize((prev) => (prev && prev.width === width && prev.height === height ? prev : { width, height }));
      onAspect?.(broadcastId, width, height);
    };
    read();
    el.addEventListener('loadedmetadata', read);
    el.addEventListener('resize', read);
    return () => {
      el.removeEventListener('loadedmetadata', read);
      el.removeEventListener('resize', read);
    };
  }, [onAspect, broadcastId]);
  useEffect(() => {
    const el = videoRef.current;
    if (!el) return;
    el.srcObject = preview;
    void el.play?.()?.catch?.(() => {
      /* autoplay races on mount */
    });
    return () => {
      el.srcObject = null;
    };
  }, [preview]);
  return (
    <TileFrame
      attachment={attachment}
      index={index}
      stripIndex={stripIndex}
      variant={variant}
      edges={edges}
      box={box}
      frameSize={frameSize}
      own
      ownControls={ownControls}
      showChrome={showChrome}
      status="preview"
      frames={0}
      onFocus={onFocus}
      surface={<video ref={videoRef} className={styles.tileCanvas} muted playsInline data-testid="own-preview" />}
      state={attachment.live ? null : <span>Your stream is reconnecting…</span>}
    />
  );
}
