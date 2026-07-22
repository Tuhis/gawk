import { useCallback, useEffect, useRef, useState } from 'react';
import styles from './viewer.module.css';
import { GlassPanel } from '../../ui/GlassPanel';
import { IconButton } from '../../ui/IconButton';
import { Button } from '../../ui/Button';
import {
  EyeIcon,
  FullscreenExitIcon,
  FullscreenIcon,
  LeaveIcon,
  MoreIcon,
  SpeakerIcon,
  SpeakerMutedIcon,
  StatsIcon,
} from '../../ui/Icons';
import { ContextMenu, type MenuItem } from '../../ui/ContextMenu';
import { StatsOverlay } from './StatsOverlay';
import { STATS_HOTKEY } from '../../lib/hotkeys';
import { DiagnosticsBuffer } from '../../lib/diagnostics';
import type { FeatureGate, PresentationSurfaceStats } from '../../lib/featureGates';
import { log } from '../../lib/logger';
import { useAutoHide } from '../../lib/useAutoHide';
import { elementFullscreenAvailable, useFullscreen } from '../../lib/useFullscreen';
import { useHotkey } from '../../lib/useHotkey';
import { fmtWatching } from '../../lib/format';
import { useViewerConnection, type ViewerStatus } from './useViewerConnection';
import type { ViewerErrorKind } from '../../transport/viewer-session';
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
// R19 (docs/24 Decision 9): resilient mode for lossy (mobile) networks —
// reliable delta delivery + a wider adaptive buffer, at 0.5–2 s behind live.
// Default off; persisted; toggling is a deliberate reconnect.
const RESILIENT_MODE_KEY = 'gawk:resilient-mode';

function loadInterpolation(): boolean {
  try {
    return localStorage.getItem(INTERPOLATION_KEY) !== '0';
  } catch {
    return true;
  }
}

function loadResilientMode(): boolean {
  try {
    return localStorage.getItem(RESILIENT_MODE_KEY) === '1';
  } catch {
    return false;
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

// Error-card copy per failure kind. Deliberately decoupled from the raw
// transport error, which goes to the console instead (useViewerConnection
// logs it) — "Opening handshake failed." means nothing to a viewer. Caveat
// on 'unreachable': a WebTransportError hides the HTTP status, so "no such
// broadcast", "relay full" (max subscribers) and "relay down" are
// indistinguishable client-side — this copy hedges toward the common case
// and is knowingly misleading on a full relay (see BUGS.md).
function errorCardCopy(
  kind: ViewerErrorKind,
  broadcastId: string,
  codec: string | null,
): { title: string; body: string } {
  switch (kind) {
    case 'unreachable':
      return {
        title: 'Streamer offline',
        body: `No one is streaming at “${broadcastId}” right now. Check that the code is right, or try again once the streamer is live.`,
      };
    case 'lost':
      return {
        title: 'Lost the stream',
        body: 'The stream stopped and we couldn’t reconnect. The streamer may have gone offline — try again in a moment.',
      };
    case 'unplayable':
      return {
        title: 'Can’t play this stream',
        body: `Your browser can’t decode this stream’s video format${codec ? ` (codec ${codec})` : ''}. Try a different browser — Chrome works best.`,
      };
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

  // R19: resilient mode. Flipping it re-runs the connection effect — a
  // visible, deliberate reconnect with (or without) reliable delivery.
  const [resilientMode, setResilientMode] = useState(loadResilientMode);
  const toggleResilientMode = useCallback(() => {
    setResilientMode((on) => {
      const next = !on;
      try {
        localStorage.setItem(RESILIENT_MODE_KEY, next ? '1' : '0');
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

  const { status, stats, codec, errorKind, errorFatal, retryNote, presentation, audio } =
    useViewerConnection(
      broadcastId,
      canvasRef,
      playoutMode,
      interpolation,
      gated,
      resilientMode,
    );

  const [showStats, setShowStats] = useState(false);
  const [menu, setMenu] = useState<{
    x: number;
    y: number;
    anchor?: 'top-left' | 'bottom-right';
  } | null>(null);
  const [copied, setCopied] = useState(false);
  const [statsCopied, setStatsCopied] = useState(false);

  // Review finding PRODUCT-2: the menu holds the settings that matter most on
  // a phone (above all R19's Resilient mode), and a right-click is the one
  // gesture a touch device doesn't have. The control-bar button opens the
  // same menu for every pointer type — and dismisses it, which is why the
  // menu needs the button as its anchor (see ContextMenu's `anchorRef`).
  const menuButtonRef = useRef<HTMLButtonElement | null>(null);

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
    // Imperative on purpose: React's `muted` prop does not reliably reach the
    // DOM property, and an unmuted autoplay rejects on iOS — leaving exactly
    // the paused video that renders black in the native player (U4 finding).
    video.muted = true;
    video.srcObject = new MediaStream([teeTrack]);
    // Defensive: autoplay+muted should start it, but a paused hidden video
    // would fail webkitEnterFullscreen's readiness check. A rejection here is
    // recoverable (the fullscreen toggle retries play() in the gesture) but
    // worth seeing in a remote Safari inspector.
    void video.play()?.catch?.((e) => log.warn('presentation video play() rejected:', e));
    return () => {
      video.srcObject = null;
    };
  }, [presentationVideo, teeTrack]);

  // U4: count frames the element actually presents (rVFC), and periodically
  // sample their content — the max RGB channel of a 4×4 downscale. This is
  // the discriminator the first two passes lacked: rVFC climbing proves
  // frames present, but only a pixel sample tells "presenting black frames"
  // from "the native player renders black". Refs, not state — sampled into
  // presentationSurface on each stats-tick render.
  const elementFramesRef = useRef<number | null>(null);
  const elementContentPeakRef = useRef<number | null>(null);
  useEffect(() => {
    const video = presentationVideo as
      | (HTMLVideoElement & {
          requestVideoFrameCallback?: (cb: () => void) => number;
          cancelVideoFrameCallback?: (handle: number) => void;
        })
      | null;
    if (!video || typeof video.requestVideoFrameCallback !== 'function') return;
    elementFramesRef.current = 0;
    elementContentPeakRef.current = null;
    let live = true;
    let handle = 0;
    let sampleCanvas: HTMLCanvasElement | null = null;
    const samplePeak = () => {
      try {
        sampleCanvas ??= document.createElement('canvas');
        sampleCanvas.width = 4;
        sampleCanvas.height = 4;
        const ctx = sampleCanvas.getContext('2d', { willReadFrequently: true });
        if (!ctx) return;
        ctx.drawImage(video, 0, 0, 4, 4);
        const d = ctx.getImageData(0, 0, 4, 4).data;
        let peak = 0;
        for (let i = 0; i < d.length; i += 4) {
          peak = Math.max(peak, d[i], d[i + 1], d[i + 2]);
        }
        elementContentPeakRef.current = peak;
      } catch {
        // Best-effort diagnostics — never let sampling break the counter.
      }
    };
    const onFrame = () => {
      if (!live) return;
      const n = (elementFramesRef.current ?? 0) + 1;
      elementFramesRef.current = n;
      if (n % 60 === 1) samplePeak();
      handle = video.requestVideoFrameCallback!(onFrame);
    };
    handle = video.requestVideoFrameCallback(onFrame);
    return () => {
      live = false;
      video.cancelVideoFrameCallback?.(handle);
    };
  }, [presentationVideo]);
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
    elementReadyState: presentationVideo?.readyState ?? null,
    elementPaused: presentationVideo?.paused ?? null,
    elementWidth: presentationVideo?.videoWidth ?? null,
    elementHeight: presentationVideo?.videoHeight ?? null,
    elementFrames: elementFramesRef.current,
    elementContentPeak: elementContentPeakRef.current,
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

  // A kind-less error only happens on a drop before we ever connected
  // ('ended' while 'connecting') — read it as the stream being unreachable.
  const errorCopy =
    status === 'error' ? errorCardCopy(errorKind ?? 'unreachable', broadcastId, codec) : null;

  const controlsVisible = useAutoHide(CONTROL_IDLE_MS, status === 'watching' && !menu);
  const showControls = controlsVisible || status !== 'watching' || showStats || !!menu;

  const menuItems: MenuItem[] = [
    { label: showStats ? 'Hide stats' : 'Stats', onSelect: () => setShowStats((s) => !s) },
    { label: isFullscreen ? 'Exit fullscreen' : 'Fullscreen', onSelect: () => toggleFullscreen() },
    // R5 Q3 + R12 T2: visibly costed opt-ins — the overlay's Playout/latency
    // rows show the added delay while either is on. Mutually exclusive.
    // R19 (docs/24 Decision 7): while Resilient mode is on it governs pacing
    // (adaptive, wider profile) — these entries are annotated but keep their
    // stored value and regain effect the moment Resilient mode turns off.
    {
      label:
        (playoutMode === 'fixed' ? 'Smooth playback ✓' : 'Smooth playback') +
        (resilientMode ? ' — governed by Resilient mode' : ''),
      onSelect: () => togglePlayoutMode('fixed'),
    },
    {
      label:
        (playoutMode === 'adaptive' ? 'Paced playback (adaptive) ✓' : 'Paced playback (adaptive)') +
        (resilientMode ? ' — governed by Resilient mode' : ''),
      onSelect: () => togglePlayoutMode('adaptive'),
    },
    // R19: its own toggle (never a repurposed one — project rule). Toggling
    // deliberately reconnects with/without reliable delivery.
    {
      label: resilientMode
        ? 'Resilient mode (mobile networks) ✓'
        : 'Resilient mode (mobile networks)',
      onSelect: toggleResilientMode,
    },
    // R12 T4: only offered where the pipeline can actually interpolate
    // (stats.interpolation is null on the main-thread path, non-WebGL2 sinks,
    // and outside adaptive mode).
    // Review finding LIFECYCLE-2: the gate is the *effective* mode, not the
    // stored one — R19 resilient mode implies adaptive pacing (playout.ts's
    // getPlayoutMode), so a resilient viewer whose stored mode is 'off' or
    // 'fixed' has interpolation running and needs the control to reach it.
    ...((resilientMode || playoutMode === 'adaptive') && stats?.interpolation != null
      ? [
          {
            label: interpolation
              ? 'Frame interpolation (experimental) ✓'
              : 'Frame interpolation (experimental)',
            onSelect: toggleInterpolation,
          },
        ]
      : []),
    // R15 (docs/20 Decision 9): audio entries appear only when the stream
    // actually carries audio — a video-only broadcast's menu is unchanged.
    ...(audio.present
      ? [{ label: audio.muted ? 'Unmute ✓' : 'Mute', onSelect: () => audio.setMuted(!audio.muted) }]
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
            {status === 'error' && errorCopy && (
              <>
                <h2 className={styles.cardTitle}>{errorCopy.title}</h2>
                <p className={styles.cardText}>{errorCopy.body}</p>
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
          // U4: the tee/element diagnostics section — gated devices only, so
          // every other viewer's overlay stays exactly as before.
          presentationSurface={gated ? presentationSurface : undefined}
          onClose={() => setShowStats(false)}
          onCopy={copyDiagnostics}
          copied={statsCopied}
        />
      )}

      {/* R15 (docs/20 Decision 9): the browser is holding audio for a
          gesture (strict autoplay settings; the norm on iOS). Tapping
          resumes the context — the pipeline never paused. */}
      {audio.present && audio.needsGesture && status === 'watching' && (
        <button className={styles.unmutePrompt} onClick={audio.resume}>
          <SpeakerIcon /> Tap for sound
        </button>
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
          {/* R18 (docs/23 Decision 8): the live audience badge — the relay's
              fleet-global count, honest total (includes this viewer). */}
          {status === 'watching' && stats?.viewerCount != null && (
            <span className={styles.watching}>
              <EyeIcon /> {fmtWatching(stats.viewerCount)}
            </span>
          )}
        </div>
        <div className={styles.actions}>
          {/* R15 (docs/20 Decision 9): mute + volume, rendered only when the
              stream actually carries audio. A video-only stream shows exactly
              today's control bar. */}
          {audio.present && (
            <div className={styles.audioControls}>
              <IconButton
                label={audio.muted ? 'Unmute' : 'Mute'}
                onClick={() => audio.setMuted(!audio.muted)}
              >
                {audio.muted ? <SpeakerMutedIcon /> : <SpeakerIcon />}
              </IconButton>
              <input
                className={styles.volume}
                type="range"
                min={0}
                max={1}
                step={0.01}
                value={audio.muted ? 0 : audio.volume}
                aria-label="Volume"
                onChange={(e) => {
                  const v = Number(e.target.value);
                  if (audio.muted && v > 0) audio.setMuted(false);
                  audio.setVolume(v);
                }}
              />
            </div>
          )}
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
              // Grow up-left from the button's top-right corner: anchored
              // top-left it would cover the button itself (the bar sits at the
              // bottom of the viewport, so the viewport clamp pushes the menu
              // back up over it) — and then a second tap would hit a menu item
              // instead of dismissing.
              const r = e.currentTarget.getBoundingClientRect();
              setMenu({ x: r.right, y: r.top - 6, anchor: 'bottom-right' });
            }}
          >
            <MoreIcon />
          </IconButton>
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
        <ContextMenu
          items={menuItems}
          x={menu.x}
          y={menu.y}
          anchor={menu.anchor}
          anchorRef={menuButtonRef}
          onClose={() => setMenu(null)}
        />
      )}
    </div>
  );
}
