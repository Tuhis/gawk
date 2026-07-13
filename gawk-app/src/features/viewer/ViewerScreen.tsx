import { useCallback, useEffect, useRef, useState } from 'react';
import styles from './viewer.module.css';
import { GlassPanel } from '../../ui/GlassPanel';
import { IconButton } from '../../ui/IconButton';
import { Button } from '../../ui/Button';
import { FullscreenExitIcon, FullscreenIcon, LeaveIcon, StatsIcon } from '../../ui/Icons';
import { ContextMenu, type MenuItem } from '../../ui/ContextMenu';
import { StatsOverlay } from './StatsOverlay';
import { STATS_HOTKEY } from './hotkeys';
import { useAutoHide } from '../../lib/useAutoHide';
import { useFullscreen } from '../../lib/useFullscreen';
import { useHotkey } from '../../lib/useHotkey';
import { RECONNECT_MAX_ATTEMPTS, ViewerSession } from '../../transport/viewer-session';
import type { ViewerStats } from '../../transport/viewer';
import { useTransportStore } from '../../state/transportStore';
import { HOME } from '../../routing';
import { log } from '../../lib/logger';

type Status = 'connecting' | 'watching' | 'reconnecting' | 'ended' | 'error';

const CONTROL_IDLE_MS = 3000;

const STATUS_LABEL: Record<Status, string> = {
  connecting: 'connecting',
  watching: 'live',
  reconnecting: 'reconnecting',
  ended: 'ended',
  error: 'error',
};

// The cinematic viewer (docs/10 J3/J4): the decoded stream fills the viewport
// (letterboxed, never cropped); controls auto-hide; a stats overlay opens from
// a hotkey and the right-click menu. ViewerSession is reused unchanged.
export function ViewerScreen({ broadcastId }: { broadcastId: string }) {
  const rootRef = useRef<HTMLDivElement | null>(null);
  const canvasRef = useRef<HTMLCanvasElement | null>(null);
  const ctxRef = useRef<CanvasRenderingContext2D | null>(null);
  const sessionRef = useRef<ViewerSession | null>(null);

  const [status, setStatus] = useState<Status>('connecting');
  const [stats, setStats] = useState<ViewerStats | null>(null);
  const [codec, setCodec] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [errorFatal, setErrorFatal] = useState(false);
  const [retryNote, setRetryNote] = useState<string | null>(null);
  const [showStats, setShowStats] = useState(false);
  const [menu, setMenu] = useState<{ x: number; y: number } | null>(null);
  const [copied, setCopied] = useState(false);

  const { isFullscreen, toggle: toggleFullscreen } = useFullscreen(rootRef);

  const leave = useCallback(() => {
    window.location.hash = HOME;
  }, []);

  const copyLink = useCallback(() => {
    const link = `${window.location.origin}${window.location.pathname}#/view/${broadcastId}`;
    void navigator.clipboard?.writeText(link).then(() => {
      setCopied(true);
      setTimeout(() => setCopied(false), 1800);
    });
  }, [broadcastId]);

  useHotkey(STATS_HOTKEY, () => setShowStats((s) => !s));
  useHotkey({ key: 'f' }, () => toggleFullscreen());

  // Session lifecycle, re-created whenever the broadcast id changes.
  useEffect(() => {
    let active = true;
    const { serverUrl, certHashHex } = useTransportStore.getState();

    setStatus('connecting');
    setStats(null);
    setCodec(null);
    setError(null);
    setErrorFatal(false);
    setRetryNote(null);
    setShowStats(false);
    setMenu(null);

    const session = new ViewerSession(
      serverUrl,
      broadcastId,
      { certHashHex },
      {
        onDecodedFrame: ({ frame }) => {
          const canvas = canvasRef.current;
          if (canvas) {
            const ctx = ctxRef.current ?? canvas.getContext('2d');
            ctxRef.current = ctx;
            if (ctx) {
              if (canvas.width !== frame.displayWidth || canvas.height !== frame.displayHeight) {
                canvas.width = frame.displayWidth;
                canvas.height = frame.displayHeight;
              }
              ctx.drawImage(frame, 0, 0, canvas.width, canvas.height);
            }
          }
          frame.close();
        },
        onConfig: (config) => {
          if (active) setCodec(config.codec);
        },
        onStats: (s) => {
          if (active) setStats(s);
        },
        onConnected: () => {
          if (!active) return;
          setRetryNote(null);
          setStatus('watching');
        },
        onReconnecting: ({ attempt, reason }) => {
          if (!active) return;
          setStatus('reconnecting');
          setRetryNote(`Reconnecting — attempt ${attempt}/${RECONNECT_MAX_ATTEMPTS}`);
          log.info('viewer reconnecting:', reason);
        },
        onError: (err) => {
          if (active) {
            setError(err.message);
            setErrorFatal(Boolean((err as { fatal?: boolean }).fatal));
            setStatus('error');
          }
          void session.stop();
        },
        onEnded: () => {
          // Only clear the ref if it still points at THIS session — under
          // StrictMode's mount/cleanup/mount, the discarded first session's
          // late onEnded must not null out the live second session.
          if (sessionRef.current === session) sessionRef.current = null;
          if (!active) return;
          setRetryNote(null);
          // A drop before we ever connected surfaces as an error, not a clean
          // end; code-4000 / server-side end while watching is "ended".
          setStatus((prev) => (prev === 'connecting' || prev === 'error' ? 'error' : 'ended'));
        },
      },
    );
    sessionRef.current = session;
    session.start().catch((e) => {
      const err = e instanceof Error ? e : new Error(String(e));
      log.error(err);
      if (active) {
        setError(err.message);
        setStatus('error');
      }
      sessionRef.current = null;
    });

    return () => {
      active = false;
      void session.stop();
      sessionRef.current = null;
    };
  }, [broadcastId]);

  const controlsVisible = useAutoHide(CONTROL_IDLE_MS, status === 'watching' && !menu);
  const showControls = controlsVisible || status !== 'watching' || showStats || !!menu;

  const menuItems: MenuItem[] = [
    { label: showStats ? 'Hide stats' : 'Stats', onSelect: () => setShowStats((s) => !s) },
    { label: isFullscreen ? 'Exit fullscreen' : 'Fullscreen', onSelect: () => toggleFullscreen() },
    { label: 'Copy link', onSelect: copyLink },
    { label: 'Leave', onSelect: leave },
  ];

  return (
    <div
      ref={rootRef}
      className={styles.root}
      onContextMenu={(e) => {
        e.preventDefault();
        setMenu({ x: e.clientX, y: e.clientY });
      }}
    >
      <canvas ref={canvasRef} className={styles.canvas} />

      {(status === 'connecting' || status === 'ended' || status === 'error') && (
        <div className={styles.center}>
          <GlassPanel className={styles.card}>
            {status === 'connecting' && (
              <>
                <div className={styles.spinner} aria-hidden="true" />
                <p className={styles.cardText}>Connecting to {broadcastId}…</p>
              </>
            )}
            {status === 'ended' && (
              <>
                <h2 className={styles.cardTitle}>Broadcast ended</h2>
                <p className={styles.cardText}>The stream is over.</p>
                <Button onClick={leave}>Back to home</Button>
              </>
            )}
            {status === 'error' && (
              <>
                <h2 className={styles.cardTitle}>
                  {errorFatal ? 'Can’t play this stream' : 'Can’t reach the stream'}
                </h2>
                <p className={styles.cardText}>{error ?? 'The connection was lost.'}</p>
                <div className={styles.cardActions}>
                  {/* Retry is pointless for an unplayable codec — reloading in
                      the same browser fails identically. */}
                  {!errorFatal && (
                    <Button variant="secondary" onClick={() => window.location.reload()}>
                      Retry
                    </Button>
                  )}
                  <Button onClick={leave}>Home</Button>
                </div>
              </>
            )}
          </GlassPanel>
        </div>
      )}

      {status === 'reconnecting' && retryNote && (
        <div className={styles.topPill}>
          <span className={styles.pulse} aria-hidden="true" />
          {retryNote}
        </div>
      )}

      {showStats && (
        <StatsOverlay stats={stats} codec={codec} onClose={() => setShowStats(false)} />
      )}

      {copied && <div className={styles.toast}>Link copied</div>}

      <div className={[styles.controls, showControls ? '' : styles.controlsHidden].join(' ')}>
        <div className={styles.status}>
          <span
            className={styles.dot}
            data-state={status === 'watching' ? 'live' : status}
            aria-hidden="true"
          />
          <span className={styles.statusText}>{STATUS_LABEL[status]}</span>
        </div>
        <div className={styles.actions}>
          <IconButton label={showStats ? 'Hide stats' : 'Show stats'} onClick={() => setShowStats((s) => !s)}>
            <StatsIcon />
          </IconButton>
          <IconButton
            label={isFullscreen ? 'Exit fullscreen' : 'Fullscreen'}
            onClick={() => toggleFullscreen()}
          >
            {isFullscreen ? <FullscreenExitIcon /> : <FullscreenIcon />}
          </IconButton>
          <IconButton label="Leave stream" onClick={leave}>
            <LeaveIcon />
          </IconButton>
        </div>
      </div>

      {menu && (
        <ContextMenu items={menuItems} x={menu.x} y={menu.y} onClose={() => setMenu(null)} />
      )}
    </div>
  );
}
