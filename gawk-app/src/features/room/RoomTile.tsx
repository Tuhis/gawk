import { useCallback, useEffect, useRef, useState, type CSSProperties, type ReactNode } from 'react';
import styles from './room.module.css';
import { IconButton } from '../../ui/IconButton';
import { EyeIcon, OpenIcon, SpeakerIcon, SpeakerMutedIcon } from '../../ui/Icons';
import { useViewerConnection } from '../viewer/useViewerConnection';
import type { AudioOutput } from '../viewer/audioSink';
import type { PlaybackConfig } from '../viewer/playbackPresets';
import type { RoomAttachment } from '../../transport/wire';
import { fmtWatching } from '../../lib/format';
import { buildViewLink } from '../../lib/shareLink';
import { loadTileVolume, saveTileVolume } from './roomPrefs';

export type TileVariant = 'grid' | 'focus' | 'small';

interface FrameProps {
  attachment: RoomAttachment;
  // 1-based: the number key that focuses this POV (docs/44 §4.9).
  index: number;
  // Position in the focus strip (small tiles only).
  stripIndex?: number;
  variant: TileVariant;
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
}

// The tile shell shared by a remote POV and the broadcaster's own preview:
// the number badge + label, the state overlay, the bottom-right controls,
// the "open full-screen" #/view/ link, and the own glass bar.
function TileFrame({
  attachment,
  index,
  stripIndex,
  variant,
  own,
  ownControls,
  showChrome,
  status,
  frames,
  onFocus,
  audioControls,
  surface,
  state,
}: FrameProps) {
  const { broadcastId } = attachment;
  const away = !attachment.live;
  const chromeCls = showChrome ? '' : styles.chromeHidden;
  const small = variant === 'small';
  return (
    <div
      className={styles.tile}
      style={stripIndex === undefined ? undefined : ({ '--i': stripIndex } as CSSProperties)}
      data-testid="room-tile"
      data-broadcast-index={index}
      data-variant={variant}
      data-own={own ? 'true' : undefined}
      data-focused={variant === 'focus' ? 'true' : undefined}
      data-status={status}
      data-frames={frames}
      role={small ? 'button' : undefined}
      tabIndex={small ? 0 : undefined}
      aria-label={small ? `Focus ${attachment.label || `stream ${index}`}` : undefined}
      onClick={small ? () => onFocus(broadcastId) : undefined}
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
        <span className={styles.tileLabel} data-live={away ? 'false' : 'true'}>
          {attachment.label || broadcastId}
          {own && <span>· you</span>}
          {away && <span>· away</span>}
          {!small && (
            <span className={styles.tileWatching}>
              <EyeIcon /> {fmtWatching(attachment.viewerCount)}
            </span>
          )}
        </span>
      </div>

      {state && <div className={styles.tileState}>{state}</div>}

      {!small && (
        <div className={`${styles.tileFoot} ${chromeCls}`}>
          {audioControls}
          {/* D1: a plain #/view/ link, in a new tab so the room stays. */}
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
  config: PlaybackConfig;
  audioOutput: AudioOutput | null;
  // Focus mode: every non-focused tile is silenced through its sink, never
  // torn down (docs/44 §4.7 "audio follows the mode").
  suppressed: boolean;
  own: boolean;
  ownControls?: ReactNode;
  showChrome: boolean;
  roomKey: string | null;
  onFocus: (broadcastId: string) => void;
}

// One attached POV: one ordinary /subscribe session (useViewerConnection,
// keyed by the parent on the broadcast ID so a mode switch never remounts
// it), its own volume pill in grid mode, the number-key badge, and the
// broadcaster's own glass bar when it is theirs. presentationMux stays off —
// the room offers no native (iPhone) fullscreen per tile; the "open
// full-screen" link is a plain #/view/ link for that.
export function RoomTile({
  attachment,
  index,
  stripIndex,
  variant,
  config,
  audioOutput,
  suppressed,
  own,
  ownControls,
  showChrome,
  roomKey,
  onFocus,
}: Props) {
  const canvasRef = useRef<HTMLCanvasElement | null>(null);
  const { broadcastId } = attachment;
  // Read once per mount: the tile's level is its own persisted preference.
  const [initialVolume] = useState(() => loadTileVolume(broadcastId));
  const { status, stats, audio } = useViewerConnection(
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
      // The broadcaster hears their own game already (docs/44 §4.7).
      initialMuted: own,
      initialVolume,
      roomKey,
    },
  );

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
  const small = variant === 'small';

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
    state = <span>{small ? 'Offline' : 'This stream is offline right now.'}</span>;
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

  return (
    <TileFrame
      attachment={attachment}
      index={index}
      stripIndex={stripIndex}
      variant={variant}
      own={own}
      ownControls={ownControls}
      showChrome={showChrome}
      status={status}
      frames={stats?.framesCompleted ?? 0}
      onFocus={onFocus}
      audioControls={audioControls}
      surface={<canvas ref={canvasRef} className={styles.tileCanvas} />}
      state={state}
    />
  );
}

interface OwnProps {
  attachment: RoomAttachment;
  index: number;
  stripIndex?: number;
  variant: TileVariant;
  preview: MediaStream;
  ownControls?: ReactNode;
  showChrome: boolean;
  onFocus: (broadcastId: string) => void;
}

// RM5 (docs/44 §4.8): the web broadcaster's own tile paints the LOCAL
// capture preview — no /subscribe session to their own broadcast, so the
// room costs them no extra uplink and shows no relay round-trip; and it is
// muted by construction (they hear their game already, §4.7).
export function OwnPreviewTile({
  attachment,
  index,
  stripIndex,
  variant,
  preview,
  ownControls,
  showChrome,
  onFocus,
}: OwnProps) {
  const videoRef = useRef<HTMLVideoElement | null>(null);
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
