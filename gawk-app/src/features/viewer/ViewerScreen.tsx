import type { ViewerDeliveryMode } from '../../transport/resilient';
import type { StripeMode } from '../../transport/stripe';
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
import { Toast } from '../../ui/Toast';
import { allowCustomRelays } from '../../config';
import { StatsOverlay } from './StatsOverlay';
import { ViewerSettingsPanel } from './ViewerSettingsPanel';
import {
  PRESETS,
  RECONNECT_NOTE,
  ADVANCED_DEFAULTS,
  presetConfig,
  presetLabel,
  resolvePreset,
  type ParityChoice,
  type PlaybackConfig,
  type PresetId,
} from './playbackPresets';
import { STATS_HOTKEY } from '../../lib/hotkeys';
import { DiagnosticsBuffer } from '../../lib/diagnostics';
import type { FeatureGate, PresentationSurfaceStats } from '../../lib/featureGates';
import { MsePresenter } from './msePresentation';
import { useAutoHide } from '../../lib/useAutoHide';
import { elementFullscreenAvailable, useFullscreen } from '../../lib/useFullscreen';
import { useHotkey } from '../../lib/useHotkey';
import { useWakeLock } from '../../lib/useWakeLock';
import { fmtWatching } from '../../lib/format';
import { buildViewLink } from '../../lib/shareLink';
import { useViewerConnection, type ViewerStatus } from './useViewerConnection';
import type { ViewerEndReason, ViewerErrorKind } from '../../transport/viewer-session';
import type { PlayoutMode } from '../../transport/playout';
import type { ViewerStats } from '../../transport/viewer';
import { HOME } from '../../routing';
import { ServerIndicator } from '../servers/ServerIndicator';
import { ServerPickerPanel } from '../servers/ServerPickerPanel';

const CONTROL_IDLE_MS = 3000;

// R5 Q3 + R12 T2: the playout preference, persisted per browser as one mode
// ('off' | 'fixed' | 'adaptive'). **Default: 'adaptive'** (user decision
// 2026-07-15, flipping the earlier live-edge default for the production
// viewer; the right-click menu is the disable path).
//
// docs/17 Decision 10 (2026-07-23) retired 'fixed' from the production menu:
// adaptive dominates it on every axis — its clamp floor (50 ms) is *below*
// fixed's constant 150 ms on a clean link, its ceiling above it on a dirty
// one, its first ~5 s are the same 150 ms seed, and only adaptive computes a
// displayTargetMs, so fixed paid the buffering latency while presenting
// unpaced (viewer.ts `displayTargetMs` returns undefined outside adaptive).
// The *mode* survives as a developer diagnostic — a measurement-free control
// for telling a pacing bug from a bug in the thing measuring the pacing,
// which PLAYOUT-1 (docs/24 finding 8) proved is a real failure mode — so the
// entry is gated on isDevEnvironment() exactly like the broadcaster's dev
// settings, rather than left as an unreachable branch.
//
// Migration order: an explicit new-key choice wins ('fixed' outside a dev
// build resolves to 'adaptive' — the mode it was a worse approximation of);
// then the legacy boolean ('1' = the R5 "Smooth playback" opt-in → 'adaptive',
// which is what that user was asking for; '0' = an explicit live-edge choice
// → 'off', the default flip must not overrule it); then the adaptive default.
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
// R21 (docs/26 Decision 15): R19's boolean became three points on one axis —
// live-edge, resilient, deep buffer — because each step buys smoothness with
// delay and a second boolean would have made two controls out of one choice.
// The legacy key migrates: an R19 viewer that had resilient mode on keeps
// exactly the latency it had, and opts into the deep buffer separately.
const DELIVERY_MODE_KEY = 'gawk:viewer-delivery';
// R29 (docs/34 §5.2): an opt-DOWN from the fleet parity default, persisted.
// Absent/'auto' means "take what the fleet serves", which is the default and
// the only way to get the maximum — a viewer cannot ask for MORE parity than
// the producer emitted.
const PARITY_LEVEL_KEY = 'gawk:parity-level';
// R30 (docs/35 §5.5): connection striping. 'auto' (default) engages when the
// client detects the burst-threshold loss signature on itself; 'on' forces
// it as soon as frames are sized; 'off' forbids it. A live flip, never a
// reconnect — engagement is in-band.
const STRIPE_MODE_KEY = 'gawk:stripe-mode';

// R32 (docs/37 decision 1): the five keys above stay exactly as they are and
// the *preset* is derived from them — never stored. No migration for existing
// viewers, no second source of truth that can drift from the values it claims
// to describe, and a legacy R19-era configuration keeps working and simply
// reads "Custom". `ParityChoice` now lives in playbackPresets.ts beside the
// model that consumes it.

function loadStripeMode(): StripeMode {
  try {
    const v = localStorage.getItem(STRIPE_MODE_KEY);
    if (v === 'on' || v === 'off') return v;
  } catch {
    // private mode etc. — fall through to auto
  }
  return 'auto';
}

function loadParityChoice(): ParityChoice {
  try {
    const v = localStorage.getItem(PARITY_LEVEL_KEY);
    if (v === '0') return 0;
    if (v === '1') return 1;
  } catch {
    // private mode etc. — fall through to the fleet default
  }
  return 'auto';
}

function loadInterpolation(): boolean {
  try {
    return localStorage.getItem(INTERPOLATION_KEY) !== '0';
  } catch {
    return true;
  }
}

function loadDeliveryMode(): ViewerDeliveryMode {
  try {
    const v = localStorage.getItem(DELIVERY_MODE_KEY);
    if (v === 'live' || v === 'resilient' || v === 'deep') return v;
    // Legacy R19 boolean: on ⇒ resilient, never deep. Silently promoting it
    // would hand an existing viewer a 10x latency change it never asked for.
    if (localStorage.getItem(RESILIENT_MODE_KEY) === '1') return 'resilient';
    return 'live';
  } catch {
    return 'live';
  }
}

function loadPlayoutMode(): PlayoutMode {
  try {
    const v = localStorage.getItem(PLAYOUT_MODE_KEY);
    // R32 removed 'fixed' outright, so a viewer carrying one — from before
    // docs/17 Decision 10 retired it, or from a dev build that could still
    // select it — lands on adaptive: the mode fixed was a worse approximation
    // of, and the one its stored value was already migrating to everywhere a
    // real viewer could see it.
    if (v === 'fixed') return 'adaptive';
    if (v === 'adaptive' || v === 'off') return v;
    const legacy = localStorage.getItem(LEGACY_SMOOTHED_KEY);
    if (legacy === '1') return 'adaptive';
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

// End-card copy. A moderator kill (close code 4006) is deliberately NOT the
// generic ending: R39 allocated its own code so viewers of a broadcast the
// operator took down are told that, rather than being left to assume the
// streamer stopped (docs/42 D6, §4.4). No retry affordance either — the ID is
// banned for at least the kill cooldown.
function endCardCopy(reason: ViewerEndReason): { title: string; body: string } {
  switch (reason) {
    case 'moderated':
      return {
        title: 'Broadcast ended by a moderator',
        body: 'This broadcast was ended by a moderator.',
      };
    case 'normal':
      return { title: 'Broadcast ended', body: 'The stream is over.' };
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
  // Two modes since R32 removed the retired 'fixed' one: 'adaptive' (the R12
  // paced-presentation mode, the default since 2026-07-15) and 'off'
  // (live-edge). Which one is in force is a property of the chosen preset —
  // Lowest latency is 'off', every other preset is 'adaptive'.
  const [playoutMode, setPlayoutModeState] = useState<PlayoutMode>(loadPlayoutMode);

  // R37 (docs/40 §4.3, F1): the picker replaced the dev-only relay override
  // panel — the picker is a production surface (gated by allowCustomRelays at
  // the menu item), and selecting a server is a deliberate reconnect because
  // useViewerConnection depends on the store's resolved serverUrl.
  const [showServerPicker, setShowServerPicker] = useState(false);
  const setPlayoutMode = useCallback((next: PlayoutMode) => {
    setPlayoutModeState(() => {
      try {
        localStorage.setItem(PLAYOUT_MODE_KEY, next);
      } catch {
        // private mode etc. — the choice still holds for this session
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
  const chooseInterpolation = useCallback((next: boolean) => {
    setInterpolation(() => {
      try {
        localStorage.setItem(INTERPOLATION_KEY, next ? '1' : '0');
      } catch {
        // private mode etc. — the choice still holds for this session
      }
      return next;
    });
  }, []);

  // R19/R21: where this viewer sits on the latency-for-smoothness axis.
  // Changing it re-runs the connection effect — a visible, deliberate
  // reconnect, because delivery is negotiated at subscribe time.
  const [deliveryMode, setDeliveryMode] = useState(loadDeliveryMode);
  const chooseDeliveryMode = useCallback((next: ViewerDeliveryMode) => {
    setDeliveryMode(() => {
      try {
        localStorage.setItem(DELIVERY_MODE_KEY, next);
      } catch {
        // private mode etc. — the choice still holds for this session
      }
      return next;
    });
  }, []);

  // R29: like delivery, this is negotiated at subscribe time, so changing it
  // is a deliberate reconnect rather than an in-session morph.
  const [parityChoice, setParityChoice] = useState(loadParityChoice);
  const chooseParity = useCallback((next: ParityChoice) => {
    setParityChoice(() => {
      try {
        if (next === 'auto') localStorage.removeItem(PARITY_LEVEL_KEY);
        else localStorage.setItem(PARITY_LEVEL_KEY, String(next));
      } catch {
        // private mode etc. — the choice still holds for this session
      }
      return next;
    });
  }, []);

  // R30: connection striping — a live flip (never a reconnect), so the value
  // deliberately does NOT reach the session effect's deps.
  const [stripeMode, setStripeModeState] = useState(loadStripeMode);
  const chooseStripeMode = useCallback((next: StripeMode) => {
    setStripeModeState(() => {
      try {
        if (next === 'auto') localStorage.removeItem(STRIPE_MODE_KEY);
        else localStorage.setItem(STRIPE_MODE_KEY, next);
      } catch {
        // private mode etc. — the choice still holds for this session
      }
      return next;
    });
  }, []);

  // R32 UX2 (docs/37 §6.1): the five stored values as one configuration, and
  // the preset it resolves to — `null` meaning Custom, which is a state you
  // land in rather than an option anyone picks.
  const playbackConfig: PlaybackConfig = {
    delivery: deliveryMode,
    playout: playoutMode,
    parity: parityChoice,
    striping: stripeMode,
    interpolation,
  };
  const currentPreset = resolvePreset(playbackConfig);

  // Decision 2: a preset is a *complete* configuration, so applying one also
  // returns the advanced knobs to their defaults. The alternative — sticky,
  // orthogonal advanced values — makes the pill label lie ("Balanced" while
  // striping is forced off). The cost (a deliberate advanced choice is lost on
  // a preset switch) is accepted, bounded, and made visible beforehand by the
  // panel's "· N changed" marker.
  const applyPreset = useCallback(
    (id: PresetId) => {
      const next = presetConfig(id);
      chooseDeliveryMode(next.delivery);
      setPlayoutMode(next.playout);
      chooseParity(next.parity);
      chooseStripeMode(next.striping);
      chooseInterpolation(next.interpolation);
    },
    [chooseDeliveryMode, setPlayoutMode, chooseParity, chooseStripeMode, chooseInterpolation],
  );

  // Advanced only — deliberately leaves delivery and pacing alone, so a viewer
  // can undo an experiment without losing the preset they chose.
  const resetAdvanced = useCallback(() => {
    chooseParity(ADVANCED_DEFAULTS.parity);
    chooseStripeMode(ADVANCED_DEFAULTS.striping);
    chooseInterpolation(ADVANCED_DEFAULTS.interpolation);
  }, [chooseParity, chooseStripeMode, chooseInterpolation]);

  const [showSettings, setShowSettings] = useState(false);

  // R16 (docs/21 Decision 1): the device gate — absence of the Element
  // Fullscreen API (effectively an iPhone signature). On non-gated devices no
  // R16 code path activates: no tee flag, no video element, tier-1 fullscreen
  // exactly as before. Sampled once per mount.
  const [gated] = useState(() => !elementFullscreenAvailable());

  const {
    status,
    stats,
    codec,
    errorKind,
    errorFatal,
    endReason,
    retryNote,
    telemetrySessionId,
    presentation,
    audio,
  } = useViewerConnection(
    broadcastId,
    canvasRef,
    playoutMode,
    interpolation,
    gated,
    deliveryMode,
    parityChoice === 'auto' ? undefined : parityChoice,
    stripeMode,
  );

  const [showStats, setShowStats] = useState(false);
  const [menu, setMenu] = useState<{
    x: number;
    y: number;
    anchor?: 'top-left' | 'bottom-right';
  } | null>(null);
  const [copied, setCopied] = useState(false);
  const [statsCopied, setStatsCopied] = useState(false);
  // R32 UX4: the preset popover, anchored to the control-bar pill. Its own
  // state (not the "⋮" menu's) so the two can never be open at once.
  const [presetMenu, setPresetMenu] = useState<{ x: number; y: number } | null>(null);
  const presetButtonRef = useRef<HTMLButtonElement | null>(null);

  // Review finding PRODUCT-2: the menu holds the settings that matter most on
  // a phone (above all R19's Resilient mode), and a right-click is the one
  // gesture a touch device doesn't have. The control-bar button opens the
  // same menu for every pointer type — and dismisses it, which is why the
  // menu needs the button as its anchor (see ContextMenu's `anchorRef`).
  const menuButtonRef = useRef<HTMLButtonElement | null>(null);

  const { probe: mseProbe, audioProbe: mseAudioProbe, arm: armMux, setSegmentSink } = presentation;
  // R22 audio (docs/27 finding 2): true once the muxed audio track is what the
  // native player will output — which is when the inline sink must go quiet and
  // the hidden element must be audible.
  const nativeAudio = mseAudioProbe?.supported === true;

  // R22 (docs/27 Decision 3): the main-thread presenter — MMS + SourceBuffer
  // behind the hidden <video>. One per screen, created lazily on the gated
  // path only; survives re-renders in a ref (its cached init segment is what
  // lets a remounted video element re-prime without a worker round-trip).
  const presenterRef = useRef<MsePresenter | null>(null);
  const [armed, setArmed] = useState(false);

  // R22 Decision 5: pre-arm at `watching`, not lazily on the fullscreen tap —
  // webkitEnterFullscreen must run synchronously inside the gesture on a
  // video that already has media, and the MMS arm chain (mux → transfer →
  // append → metadata) is async.
  useEffect(() => {
    if (!gated || status !== 'watching' || mseProbe?.supported !== true) return;
    presenterRef.current ??= new MsePresenter();
    const presenter = presenterRef.current;
    setSegmentSink((seg) => {
      presenter.pushSegment(
        seg.kind === 'init'
          ? { kind: 'init', track: seg.track, mime: seg.mime, data: new Uint8Array(seg.data) }
          : {
              kind: 'media',
              track: seg.track,
              keyframe: seg.keyframe,
              data: new Uint8Array(seg.data),
            },
      );
    });
    armMux();
    setArmed(true);
    // Deliberately no cleanup that unregisters the sink (docs/27 finding 7).
    // The worker muxer emits its init segment exactly once per session and
    // survives reconnects, while this effect re-runs whenever `status` leaves
    // 'watching' — so clearing the sink here opens a window in which that one
    // init can be posted with nobody to receive it, after which the presenter
    // drops every media segment for want of it, for the rest of the session,
    // with no error anywhere. The sink is cleared with the presenter instead,
    // on unmount.
  }, [gated, status, mseProbe, armMux, setSegmentSink]);

  // R22 audio: hand the presenter the audio mime the moment the tier is known —
  // both SourceBuffers must be created before the first init segment is appended
  // (docs/27 finding 5), so this cannot wait for the first audio segment.
  useEffect(() => {
    presenterRef.current?.setExpectedAudioMime(mseAudioProbe?.mime ?? null);
  }, [mseAudioProbe]);

  // Presenter teardown on real unmount. (On StrictMode's synchronous initial
  // cleanup→remount nothing exists yet — arming starts at `watching` — so a
  // plain cleanup is safe here.)
  useEffect(() => {
    return () => {
      setSegmentSink(null);
      presenterRef.current?.dispose();
      presenterRef.current = null;
    };
  }, [setSegmentSink]);

  // R16 Decision 6 (kept by R22): the hidden presentation <video>, rendered
  // only on gated devices once armed. State (not a ref) so the effects and
  // useFullscreen re-run when it mounts. Attached to the presenter's
  // MediaSource; kept loaded-but-paused near live until the in-gesture play
  // (docs/27 Decision 5 — no continuous dual decode while inline).
  const [presentationVideo, setPresentationVideo] = useState<HTMLVideoElement | null>(null);
  useEffect(() => {
    const video = presentationVideo;
    const presenter = presenterRef.current;
    if (!video || !presenter) return;
    presenter.attach(video);
    return () => presenter.detach();
  }, [presentationVideo, armed]);

  // Count frames the element actually presents (rVFC) — kept from R16: it is
  // what separates "segments appended" from "the element presents them".
  // A ref, not state — sampled into presentationSurface on each stats render.
  const elementFramesRef = useRef<number | null>(null);
  useEffect(() => {
    const video = presentationVideo as
      | (HTMLVideoElement & {
          requestVideoFrameCallback?: (cb: () => void) => number;
          cancelVideoFrameCallback?: (handle: number) => void;
        })
      | null;
    if (!video || typeof video.requestVideoFrameCallback !== 'function') return;
    elementFramesRef.current = 0;
    let live = true;
    let handle = 0;
    const onFrame = () => {
      if (!live) return;
      elementFramesRef.current = (elementFramesRef.current ?? 0) + 1;
      handle = video.requestVideoFrameCallback!(onFrame);
    };
    handle = video.requestVideoFrameCallback(onFrame);
    return () => {
      live = false;
      video.cancelVideoFrameCallback?.(handle);
    };
  }, [presentationVideo]);

  // R22 audio (docs/27 finding 2): the audio handoff. Only one output may be
  // audible at a time — the inline AudioWorklet sink (paced to the inline canvas)
  // and the native player (paced by its own MSE playhead) are independently
  // clocked, so both at once is an echo, not stereo. Video-only muxing (probe
  // refused, or a broadcast without audio) leaves the inline sink alone: the
  // native player is then silent by construction, and suppressing it would make
  // fullscreen mute.
  const suppressAudio = audio.setSuppressed;
  const onNativeEnter = useCallback(() => {
    if (nativeAudio) suppressAudio(true);
  }, [nativeAudio, suppressAudio]);
  const onNativeExit = useCallback(() => {
    suppressAudio(false);
  }, [suppressAudio]);
  const {
    isFullscreen,
    tier,
    toggle: toggleFullscreen,
  } = useFullscreen(rootRef, presentationVideo, { onNativeEnter, onNativeExit });

  // The native player has no access to the viewer's volume slider, so mirror it
  // onto the element. (Mute rides the declarative `muted` prop below.)
  useEffect(() => {
    if (!presentationVideo) return;
    try {
      presentationVideo.volume = audio.volume;
    } catch {
      // volume is best-effort — iOS honors the hardware switch regardless
    }
  }, [presentationVideo, audio.volume]);

  // R16 Decision 9 / R22 Decision 8: the Feature Gates readout — derived
  // state, rendered on every viewer (the one deliberate overlay-only change
  // on non-gated devices). Active ⇔ probe passed AND the MMS surface is armed
  // and healthy — i.e. the native path would actually be used on the next tap.
  const presenterStats = presenterRef.current?.getStats() ?? null;
  const surfaceHealthy = armed && presenterStats?.failed !== true;
  const featureGates: FeatureGate[] = [
    {
      name: 'NativeVideoFullscreen',
      active: gated && mseProbe?.supported === true && surfaceHealthy,
      detail: !gated
        ? 'element fullscreen available'
        : mseProbe == null
          ? 'probing'
          : !mseProbe.supported
            ? `${mseProbe.reason} → pseudo`
            : presenterStats?.failed
              ? 'MSE surface failed → pseudo'
              : armed
                ? 'armed'
                : 'arming',
    },
    // R29 finding 2 (docs/34): whether this browser gave us a receive queue
    // deep enough to hold a frame's burst. Three states have to stay apart,
    // because the failure is silent — a browser that accepts the assignment
    // and ignores it is indistinguishable from success at the call site — and
    // an unreported buffer reads *unknown*, never green (the docs/33 TM8 rule
    // that an absence of evidence is not health).
    {
      name: 'DatagramReceiveBuffer',
      // Green ONLY when the write landed on the attribute the spec makes the
      // drop threshold. R29 finding 3: writing the legacy attribute succeeds
      // and reads back on Firefox while dropping continues unchanged, so
      // `applied` alone would keep saying "fixed" about a fix that isn't.
      active: stats?.datagramBuffer?.applied === true && stats.datagramBuffer.governsDrops,
      detail: ((b) =>
        b == null
          ? 'unknown'
          : b.property == null
            ? 'unsupported → browser default'
            : !b.applied
              ? `requested ${b.requested}, browser kept ${b.effective}`
              : b.governsDrops
                ? `${b.effective} datagrams (was ${b.defaultDepth})`
                : `set ${b.effective} on ${b.property} (was ${b.defaultDepth}), which does not govern drops`)(
        stats?.datagramBuffer,
      ),
    },
  ];
  // The element's buffered window — span, and how far the playhead trails the
  // buffered live edge (the fullscreen half of the live-edge delta).
  let bufferedMs: number | null = null;
  let bufferedAheadMs: number | null = null;
  let bufferedRanges: number | null = null;
  if (presentationVideo) {
    try {
      const b = presentationVideo.buffered;
      bufferedRanges = b.length;
      if (b.length > 0) {
        bufferedMs = (b.end(b.length - 1) - b.start(0)) * 1000;
        bufferedAheadMs = (b.end(b.length - 1) - presentationVideo.currentTime) * 1000;
      }
    } catch {
      // buffered access quirks are diagnostics-only
    }
  }
  const presentationSurface: PresentationSurfaceStats = {
    tier,
    armed,
    muxInitSegments: stats?.presentationMux?.initSegments ?? 0,
    muxMediaSegments: stats?.presentationMux?.mediaSegments ?? 0,
    muxErrors: stats?.presentationMux?.errors ?? 0,
    segmentsAppended: presenterStats?.segmentsAppended ?? 0,
    appendErrors: presenterStats?.appendErrors ?? 0,
    segmentsReceived: presenterStats?.received ?? 0,
    segmentsQueued: presenterStats?.queued ?? 0,
    segmentsDroppedNoInit: presenterStats?.droppedNoInit ?? 0,
    mmsStreaming: presenterStats?.streaming ?? null,
    lastAppendError: presenterStats?.lastError ?? null,
    liveDuration: presenterStats?.sourceOpen ? presenterStats.liveDuration : null,
    // The audio verdict, resolved: no audio in the stream reads 'none', a passing
    // probe 'muxed', and a refusal carries its reason (e.g. 'unsupported:
    // audio/mp4; codecs="opus"') — which is the row that says whether iOS took
    // Opus in MP4 at all.
    audioMode:
      mseAudioProbe == null
        ? stats?.audioCodec == null
          ? 'none'
          : 'probing'
        : mseAudioProbe.supported
          ? `muxed as ${mseAudioProbe.codec}`
          : mseAudioProbe.reason,
    audioTranscode: stats?.presentationMux?.audioTranscode
      ? stats.presentationMux.audioTranscode +
        (stats.presentationMux.audioTranscodeDetail
          ? ` (${stats.presentationMux.audioTranscodeDetail})`
          : '')
      : null,
    audioSegmentsAppended: presenterStats?.audioSegmentsAppended ?? 0,
    audioTrackActive: presenterStats?.audioTrack ?? false,
    // docs/27 finding 6: the end of the chain. A SourceBuffer that exists and has
    // taken bytes still yields 0 tracks if the demuxer rejected them, which is
    // precisely the state the silent session was in.
    elementAudioTracks:
      (presentationVideo as (HTMLVideoElement & { audioTracks?: { length: number } }) | null)
        ?.audioTracks?.length ?? null,
    muxAudioSegments: stats?.presentationMux?.audioSegments ?? 0,
    muxAudioHoles: stats?.presentationMux?.audioHoles ?? 0,
    bufferedMs,
    bufferedAheadMs,
    bufferedRanges,
    elementReadyState: presentationVideo?.readyState ?? null,
    elementPaused: presentationVideo?.paused ?? null,
    elementWidth: presentationVideo?.videoWidth ?? null,
    elementHeight: presentationVideo?.videoHeight ?? null,
    elementFrames: elementFramesRef.current,
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
    const link = buildViewLink(broadcastId);
    void navigator.clipboard?.writeText(link).then(() => {
      setCopied(true);
      setTimeout(() => setCopied(false), 1800);
    });
  }, [broadcastId]);

  const copyDiagnostics = useCallback(() => {
    // R28: the full 24-hex id travels here, where the overlay row shows only
    // its first 8 — a pasted blob is what turns "my stream is stuttering" into
    // a `diagnose(sessionId)` call. The token it derives from does not travel,
    // and must not: this blob gets pasted into chats.
    const json = diagRef.current.build({
      surface: 'viewer',
      broadcastId,
      codec,
      telemetrySessionId,
    });
    void navigator.clipboard?.writeText(json).then(() => {
      setStatsCopied(true);
      setTimeout(() => setStatsCopied(false), 1800);
    });
  }, [broadcastId, codec, telemetrySessionId]);

  useHotkey(STATS_HOTKEY, () => setShowStats((s) => !s));
  useHotkey({ key: 'f' }, () => toggleFullscreen());

  // A kind-less error only happens on a drop before we ever connected
  // ('ended' while 'connecting') — read it as the stream being unreachable.
  const errorCopy =
    status === 'error' ? errorCardCopy(errorKind ?? 'unreachable', broadcastId, codec) : null;
  const endCopy = status === 'ended' ? endCardCopy(endReason) : null;

  // Keep the display awake while a stream is on screen. The viewer paints a
  // canvas, never a playing <video>, so nothing tells the OS that anything is
  // being watched and it dims/sleeps the screen on its normal idle timer —
  // fullscreen included. Held through 'reconnecting' too: a blip is still
  // someone sitting there watching, and dropping the lock for it would restart
  // the idle countdown. See lib/useWakeLock.ts.
  useWakeLock(status === 'watching' || status === 'reconnecting');

  const anyOverlayOpen = !!menu || !!presetMenu || showSettings;
  const controlsVisible = useAutoHide(CONTROL_IDLE_MS, status === 'watching' && !anyOverlayOpen);
  const showControls = controlsVisible || status !== 'watching' || showStats || anyOverlayOpen;

  // R32 UX4: the preset popover — the *same* ContextMenu the "⋮" button opens,
  // so there is one menu implementation to build, test and describe, and the
  // pill inherits the anchorRef dismissal rule (docs/24 finding 9) for free.
  const presetItems: MenuItem[] = [
    ...PRESETS.map((preset) => ({
      label: preset.label,
      checked: currentPreset === preset.id,
      // Only a delivery change re-dials: delivery and parity are in
      // useViewerConnection's session-effect deps, pacing/striping/
      // interpolation cross into the live pipeline instead. So the step
      // between Lowest latency and Balanced is silent, and the carrier
      // presets are not (docs/37 decision 7).
      note:
        preset.delivery !== deliveryMode ? `${preset.sub} ${RECONNECT_NOTE}` : preset.sub,
      onSelect: () => applyPreset(preset.id),
    })),
    // Custom is never offered on a clean install: it renders only while it is
    // what you already are, checked and inert (docs/37 decision 3).
    ...(currentPreset === null
      ? [
          {
            label: presetLabel(null),
            checked: true,
            disabled: true,
            reason: 'Your advanced settings don’t match a preset.',
            onSelect: () => {},
          },
        ]
      : []),
    { label: 'More settings…', onSelect: () => setShowSettings(true) },
  ];

  // R32 UX4 (docs/37 §6.4): actions only. Every tuning control moved to the
  // preset pill and the settings panel — eleven of the seventeen rows this
  // menu had grown to were knobs that already ship with the right default for
  // the average viewer, and they were the first thing anyone opening the menu
  // to mute a stream had to read past.
  const menuItems: MenuItem[] = [
    { label: showStats ? 'Hide stats' : 'Stats', onSelect: () => setShowStats((s) => !s) },
    { label: isFullscreen ? 'Exit fullscreen' : 'Fullscreen', onSelect: () => toggleFullscreen() },
    // R15 (docs/20 Decision 9): audio entries appear only when the stream
    // actually carries audio — a video-only broadcast's menu is unchanged.
    ...(audio.present
      ? [
          {
            label: audio.muted ? 'Unmute' : 'Mute',
            onSelect: () => audio.setMuted(!audio.muted),
          },
        ]
      : []),
    { label: 'Playback settings…', onSelect: () => setShowSettings(true) },
    { label: 'Copy link', onSelect: copyLink },
    // R37 (docs/40 §4.3): the server picker, a production surface gated by
    // the deployment's allowCustomRelays flag (D6).
    ...(allowCustomRelays()
      ? [{ label: 'Server…', onSelect: () => setShowServerPicker(true) }]
      : []),
    // R23 (docs/29): terms reachable from the viewer without adding chrome.
    // Opens in a new tab so reading the terms never tears down the live
    // stream (a hash change would unmount the viewer).
    {
      label: 'Terms of use',
      onSelect: () =>
        window.open(
          `${window.location.origin}${window.location.pathname}#/terms`,
          '_blank',
          'noopener',
        ),
    },
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

      {/* R22 (keeping R16 Decision 6's hiding rules): the hidden native-
          fullscreen surface — exists only on gated devices once armed AND
          while the probe still passes: a mid-view codec change to a VP
          broadcast flips the probe false, and keeping the ready video
          mounted would let the next tap native-present stale frozen content
          instead of falling to pseudo. Hidden by size/position, never
          display:none (that breaks webkitEnterFullscreen). Loaded but NOT
          autoplaying: the video sits paused near live until the in-gesture
          play (docs/27 Decision 5). */}
      {gated && armed && mseProbe?.supported === true && (
        <video
          ref={setPresentationVideo}
          className={styles.presentationVideo}
          playsInline
          // R22 audio: muted unless the muxed audio track is what the native
          // player will output — declarative so React owns it, and correct before
          // the tap because both inputs are known in advance (an unmuted element
          // is silent anyway while it sits paused inline).
          muted={!nativeAudio || audio.muted}
          aria-hidden="true"
        />
      )}

      {/* R37 (docs/40 §4.3): the server picker (replaces the dev-only relay
          override panel). Selecting a server is a deliberate reconnect:
          useViewerConnection depends on the store's resolved values. */}
      {showServerPicker && <ServerPickerPanel onClose={() => setShowServerPicker(false)} />}

      {/* R37 (docs/40 §4.3 F2): the in-session server indicator — renders
          only when this session is not on the deployment's own relay (or a
          link's relay was quietly ignored). */}
      <ServerIndicator />

      {(status === 'connecting' || status === 'ended' || status === 'error') && (
        <div className={styles.center}>
          <GlassPanel className={styles.card}>
            {status === 'connecting' && (
              <>
                <div className={styles.spinner} aria-hidden="true" />
                <p className={styles.cardText}>Connecting to {broadcastId}…</p>
              </>
            )}
            {status === 'ended' && endCopy && (
              <>
                <h2 className={styles.cardTitle}>{endCopy.title}</h2>
                <p className={styles.cardText}>{endCopy.body}</p>
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
          telemetrySessionId={telemetrySessionId}
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

      {copied && <Toast>Link copied</Toast>}

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
          {/* R32 UX4: the preset pill — the one tuning control an average
              viewer meets. Text, not an icon, because its label IS the state
              readout. The accessible name carries the "Playback quality"
              prefix the visible label drops for width. */}
          <button
            ref={presetButtonRef}
            type="button"
            className={styles.presetPill}
            aria-haspopup="menu"
            aria-expanded={presetMenu != null}
            aria-label={`Playback quality: ${presetLabel(currentPreset)}`}
            onClick={(e) => {
              if (presetMenu) {
                setPresetMenu(null);
                return;
              }
              // Same geometry as the "⋮" button: grow up-left from the pill's
              // top-right, or the bottom-bar viewport clamp puts the menu back
              // over the control that opened it.
              const r = e.currentTarget.getBoundingClientRect();
              // Close the other menu explicitly. A pointer already does this
              // via ContextMenu's outside-pointerdown listener, but a keyboard
              // activation fires click with no pointerdown at all — so without
              // this, Enter on the pill leaves both menus on screen.
              setMenu(null);
              setPresetMenu({ x: r.right, y: r.top - 6 });
            }}
          >
            {presetLabel(currentPreset)}
            <span className={styles.presetPillCaret} aria-hidden="true">
              ▾
            </span>
          </button>
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
              setPresetMenu(null); // see the preset pill's handler
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

      {/* R32 UX3: rendered here, inside the viewer root — in CSS
          pseudo-fullscreen the root IS the fullscreen element, so a panel
          portalled to document.body would be invisible exactly on the iPhone
          that needs it most (docs/37 decision 5). */}
      {showSettings && (
        <ViewerSettingsPanel
          config={playbackConfig}
          interpolationAvailable={stats == null ? null : stats.interpolation != null}
          onPreset={applyPreset}
          onParity={chooseParity}
          onStriping={chooseStripeMode}
          onInterpolation={chooseInterpolation}
          onResetAdvanced={resetAdvanced}
          onClose={() => setShowSettings(false)}
        />
      )}
    </div>
  );
}
