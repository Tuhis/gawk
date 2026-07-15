import { useCallback, useEffect, useRef, useState } from 'react';
import styles from './viewer.module.css';
import { GlassPanel } from '../../ui/GlassPanel';
import { IconButton } from '../../ui/IconButton';
import { Button } from '../../ui/Button';
import { FullscreenExitIcon, FullscreenIcon, LeaveIcon, StatsIcon } from '../../ui/Icons';
import { ContextMenu, type MenuItem } from '../../ui/ContextMenu';
import { StatsOverlay } from './StatsOverlay';
import { STATS_HOTKEY } from '../../lib/hotkeys';
import { DiagnosticsBuffer } from '../../lib/diagnostics';
import { useAutoHide } from '../../lib/useAutoHide';
import { useFullscreen } from '../../lib/useFullscreen';
import { useHotkey } from '../../lib/useHotkey';
import { useViewerConnection, type ViewerStatus } from './useViewerConnection';
import type { PlayoutMode } from '../../transport/playout';
import type { ViewerStats } from '../../transport/viewer';
import { HOME } from '../../routing';

const CONTROL_IDLE_MS = 3000;

// R5 Q3 + R12 T2: the opt-in playout preference, persisted per browser as
// one mode ('off' | 'fixed' | 'adaptive') — the two smoothing toggles are
// mutually exclusive by construction. The legacy boolean key (pre-R12
// "Smooth playback") migrates to 'fixed'.
const PLAYOUT_MODE_KEY = 'gawk:playout-mode';
const LEGACY_SMOOTHED_KEY = 'gawk:smoothed-playout';
// R12 T4: the experimental frame-interpolation preference.
const INTERPOLATION_KEY = 'gawk:interpolation';

function loadInterpolation(): boolean {
  try {
    return localStorage.getItem(INTERPOLATION_KEY) === '1';
  } catch {
    return false;
  }
}

function loadPlayoutMode(): PlayoutMode {
  try {
    const v = localStorage.getItem(PLAYOUT_MODE_KEY);
    if (v === 'fixed' || v === 'adaptive' || v === 'off') return v;
    return localStorage.getItem(LEGACY_SMOOTHED_KEY) === '1' ? 'fixed' : 'off';
  } catch {
    return 'off';
  }
}

const STATUS_LABEL: Record<ViewerStatus, string> = {
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

  // R5 Q3 + R12 T2: opt-in playout smoothing (trades latency for steadier
  // pacing); default off — live-edge is the product stance. 'fixed' is the
  // original 150 ms mode, 'adaptive' the R12 paced-presentation mode; each
  // menu item toggles its own mode and checking one unchecks the other.
  const [playoutMode, setPlayoutModeState] = useState<PlayoutMode>(loadPlayoutMode);
  const togglePlayoutMode = useCallback((mode: 'fixed' | 'adaptive') => {
    setPlayoutModeState((current) => {
      const next = current === mode ? 'off' : mode;
      try {
        localStorage.setItem(PLAYOUT_MODE_KEY, next);
      } catch {
        // private mode etc. — the toggle still works for this session
      }
      return next;
    });
  }, []);

  // The connection (worker-offloaded when supported, main-thread otherwise)
  // owns decode + render and reports back only view state — no VideoFrame ever
  // reaches this component (R8 S6).
  // R12 T4: experimental frame interpolation — only offered when the
  // pipeline reports it available (WebGL2 worker sink + adaptive mode).
  const [interpolation, setInterpolation] = useState(loadInterpolation);
  const toggleInterpolation = useCallback(() => {
    setInterpolation((on) => {
      const next = !on;
      try {
        localStorage.setItem(INTERPOLATION_KEY, next ? '1' : '0');
      } catch {
        // private mode etc. — the toggle still works for this session
      }
      return next;
    });
  }, []);

  const { status, stats, codec, error, errorFatal, retryNote } = useViewerConnection(
    broadcastId,
    canvasRef,
    playoutMode,
    interpolation,
  );

  const [showStats, setShowStats] = useState(false);
  const [menu, setMenu] = useState<{ x: number; y: number } | null>(null);
  const [copied, setCopied] = useState(false);
  const [statsCopied, setStatsCopied] = useState(false);

  const { isFullscreen, toggle: toggleFullscreen } = useFullscreen(rootRef);

  // R9 M7: rolling stat-sample window backing "Copy diagnostics" and the
  // derived receive bitrate. A ref, not state — it must not cause renders.
  const diagRef = useRef(new DiagnosticsBuffer<ViewerStats>());
  useEffect(() => {
    if (stats) diagRef.current.push(stats);
  }, [stats]);

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

  const copyDiagnostics = useCallback(() => {
    const json = diagRef.current.build({ surface: 'viewer', broadcastId, codec });
    void navigator.clipboard?.writeText(json).then(() => {
      setStatsCopied(true);
      setTimeout(() => setStatsCopied(false), 1800);
    });
  }, [broadcastId, codec]);

  useHotkey(STATS_HOTKEY, () => setShowStats((s) => !s));
  useHotkey({ key: 'f' }, () => toggleFullscreen());

  const controlsVisible = useAutoHide(CONTROL_IDLE_MS, status === 'watching' && !menu);
  const showControls = controlsVisible || status !== 'watching' || showStats || !!menu;

  const menuItems: MenuItem[] = [
    { label: showStats ? 'Hide stats' : 'Stats', onSelect: () => setShowStats((s) => !s) },
    { label: isFullscreen ? 'Exit fullscreen' : 'Fullscreen', onSelect: () => toggleFullscreen() },
    // R5 Q3 + R12 T2: visibly costed opt-ins — the overlay's Playout/latency
    // rows show the added delay while either is on. Mutually exclusive.
    {
      label: playoutMode === 'fixed' ? 'Smooth playback ✓' : 'Smooth playback',
      onSelect: () => togglePlayoutMode('fixed'),
    },
    {
      label:
        playoutMode === 'adaptive' ? 'Paced playback (adaptive) ✓' : 'Paced playback (adaptive)',
      onSelect: () => togglePlayoutMode('adaptive'),
    },
    // R12 T4: only offered where the pipeline can actually interpolate
    // (stats.interpolation is null on the main-thread path, non-WebGL2 sinks,
    // and outside adaptive mode).
    ...(playoutMode === 'adaptive' && stats?.interpolation != null
      ? [
          {
            label: interpolation
              ? 'Frame interpolation (experimental) ✓'
              : 'Frame interpolation (experimental)',
            onSelect: toggleInterpolation,
          },
        ]
      : []),
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
        <StatsOverlay
          stats={stats}
          codec={codec}
          bitrateBps={(() => {
            const bytesRate = diagRef.current.rate((s) => s.connection?.bytesReceived);
            return bytesRate == null ? null : bytesRate * 8;
          })()}
          onClose={() => setShowStats(false)}
          onCopy={copyDiagnostics}
          copied={statsCopied}
        />
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
