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
import type { FeatureGate, PresentationSurfaceStats } from '../../lib/featureGates';
import { useAutoHide } from '../../lib/useAutoHide';
import { elementFullscreenAvailable, useFullscreen } from '../../lib/useFullscreen';
import { useHotkey } from '../../lib/useHotkey';
import { useViewerConnection, type ViewerStatus } from './useViewerConnection';
import type { PlayoutMode } from '../../transport/playout';
import type { ViewerStats } from '../../transport/viewer';
import { HOME } from '../../routing';

const CONTROL_IDLE_MS = 3000;

// R5 Q3 + R12 T2: the playout preference, persisted per browser as one mode
// ('off' | 'fixed' | 'adaptive') — the two smoothing toggles are mutually
// exclusive by construction. **Default: 'adaptive'** (user decision
// 2026-07-15, flipping the earlier live-edge default for the production
// viewer; the right-click menu is the disable path). Migration order: an
// explicit new-key choice wins; then the legacy boolean ('1' = the old
// "Smooth playback" → 'fixed'; '0' = an explicit live-edge choice → 'off' —
// the default flip must not overrule it); then the adaptive default.
const PLAYOUT_MODE_KEY = 'gawk:playout-mode';
const LEGACY_SMOOTHED_KEY = 'gawk:smoothed-playout';
// R12 T4: the experimental frame-interpolation preference. **Default: on**
// (same 2026-07-15 decision); a no-op wherever the pipeline can't
// interpolate (main-thread path, non-WebGL2 sink, non-adaptive mode).
const INTERPOLATION_KEY = 'gawk:interpolation';

function loadInterpolation(): boolean {
  try {
    return localStorage.getItem(INTERPOLATION_KEY) !== '0';
  } catch {
    return true;
  }
}

function loadPlayoutMode(): PlayoutMode {
  try {
    const v = localStorage.getItem(PLAYOUT_MODE_KEY);
    if (v === 'fixed' || v === 'adaptive' || v === 'off') return v;
    const legacy = localStorage.getItem(LEGACY_SMOOTHED_KEY);
    if (legacy === '1') return 'fixed';
    if (legacy === '0') return 'off';
    return 'adaptive';
  } catch {
    return 'adaptive';
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

  // R5 Q3 + R12 T2: playout smoothing (trades latency for steadier pacing).
  // 'adaptive' (the R12 paced-presentation mode) is the default since
  // 2026-07-15; 'fixed' is the original 150 ms mode. Each menu item toggles
  // its own mode, checking one unchecks the other, and unchecking the
  // active one returns to live-edge ('off').
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

  // R16 (docs/21 Decision 1): the device gate — absence of the Element
  // Fullscreen API (effectively an iPhone signature). On non-gated devices no
  // R16 code path activates: no tee flag, no video element, tier-1 fullscreen
  // exactly as before. Sampled once per mount.
  const [gated] = useState(() => !elementFullscreenAvailable());

  const { status, stats, codec, error, errorFatal, retryNote, presentation } = useViewerConnection(
    broadcastId,
    canvasRef,
    playoutMode,
    interpolation,
    gated,
  );

  const [showStats, setShowStats] = useState(false);
  const [menu, setMenu] = useState<{ x: number; y: number } | null>(null);
  const [copied, setCopied] = useState(false);
  const [statsCopied, setStatsCopied] = useState(false);

  const { probe: teeProbe, track: teeTrack, arm: armTee } = presentation;

  // R16 Decision 5: pre-arm at `watching`, not lazily on the fullscreen tap —
  // webkitEnterFullscreen must run synchronously inside the gesture on a
  // video that already has media, and the arm chain is async.
  useEffect(() => {
    if (gated && status === 'watching' && teeProbe === true) armTee();
  }, [gated, status, teeProbe, armTee]);

  // R16 Decision 6: the hidden presentation <video>, rendered only on gated
  // devices once the track exists. State (not a ref) so the effects and
  // useFullscreen re-run when it mounts.
  const [presentationVideo, setPresentationVideo] = useState<HTMLVideoElement | null>(null);
  useEffect(() => {
    const video = presentationVideo;
    if (!video || !teeTrack) return;
    video.srcObject = new MediaStream([teeTrack]);
    // Defensive: autoplay+muted should start it, but a paused hidden video
    // would fail webkitEnterFullscreen's readiness check.
    void video.play()?.catch?.(() => {});
    return () => {
      video.srcObject = null;
    };
  }, [presentationVideo, teeTrack]);
  // Track teardown lives with the controller (useViewerConnection's deferred
  // dispose): stopping it here on effect cleanup would kill it for good
  // across a StrictMode remount.

  const { isFullscreen, tier, toggle: toggleFullscreen } = useFullscreen(rootRef, presentationVideo);

  // R16 Decision 9: the Feature Gates readout — derived state, rendered on
  // every viewer (the one deliberate overlay-only R16 change on non-gated
  // devices). Active ⇔ the native path would actually be used on the next tap.
  const armed = teeTrack != null;
  const featureGates: FeatureGate[] = [
    {
      name: 'NativeVideoFullscreen',
      active: gated && teeProbe === true && armed,
      detail: !gated
        ? 'element fullscreen available'
        : teeProbe === false
          ? 'probe failed → pseudo'
          : armed
            ? 'armed'
            : 'arming',
    },
  ];
  const presentationSurface: PresentationSurfaceStats = {
    tier,
    armed,
    teedFrames: stats?.presentationTee?.teedFrames ?? 0,
    teeErrors: stats?.presentationTee?.teeErrors ?? 0,
  };

  // R9 M7: rolling stat-sample window backing "Copy diagnostics" and the
  // derived receive bitrate. A ref, not state — it must not cause renders.
  // R16: gates + presentation surface ride along into the diagnostics JSON.
  const diagRef = useRef(new DiagnosticsBuffer<ViewerStats>());
  // Keyed on stats alone: the R16 fields are derived fresh every render, and
  // a sample should land per pipeline stats tick, not per gate-state change.
  const gatesRef = useRef({ featureGates, presentationSurface });
  gatesRef.current = { featureGates, presentationSurface };
  useEffect(() => {
    if (stats) diagRef.current.push({ ...stats, ...gatesRef.current });
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
      className={[
        styles.root,
        isFullscreen && tier === 'pseudo' ? styles.pseudoFullscreen : '',
      ].join(' ')}
      onContextMenu={(e) => {
        e.preventDefault();
        setMenu({ x: e.clientX, y: e.clientY });
      }}
    >
      <canvas ref={canvasRef} className={styles.canvas} />

      {/* R16: the hidden native-fullscreen surface — exists only on gated
          devices once the tee's track arrived. Hidden by size/position, never
          display:none (that breaks webkitEnterFullscreen). */}
      {gated && teeTrack != null && (
        <video
          ref={setPresentationVideo}
          className={styles.presentationVideo}
          playsInline
          muted
          autoPlay
          aria-hidden="true"
        />
      )}

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
            // Self-counted video bytes (datagrams + keyframe streams) — the
            // getStats()-based connection counter is null in every current
            // browser (docs/13 D7).
            const bytesRate = diagRef.current.rate((s) => s.videoBytesReceived);
            return bytesRate == null ? null : bytesRate * 8;
          })()}
          featureGates={featureGates}
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
