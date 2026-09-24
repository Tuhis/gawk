import { useCallback, useEffect, useRef, useState, type RefObject } from 'react';
import type { FullscreenTier } from './featureGates';

// Fullscreen for a target element, in tiers. Where the Element Fullscreen API
// exists (desktop, Android, iPad) tier 1 is the whole feature. On iPhone
// (WebKit ships the API on iPadOS only, and every iOS browser is WebKit) the
// toggle tries the one native fullscreen there, webkitEnterFullscreen() on the
// MSE-fed presentation video, and falls through to CSS pseudo-fullscreen so
// the button always visibly does something. The armed video sits paused, so
// entry seeks it to the live edge and plays it in the gesture; exit pauses it
// again so the second decode stops with the native player.
//
// State tracking is per tier: `fullscreenchange` (tier 1),
// `webkitbeginfullscreen`/`webkitendfullscreen` on the video (tier 2 — the
// native fullscreen does NOT fire fullscreenchange), local state (tier 3).

// The device gate: no Element.requestFullscreen is effectively an iPhone.
export function elementFullscreenAvailable(): boolean {
  return (
    typeof document !== 'undefined' &&
    typeof document.documentElement.requestFullscreen === 'function'
  );
}

// webkitEnterFullscreen needs media at readyState ≥ HAVE_METADATA.
const HAVE_METADATA = 1;

// How far behind the buffered end the playhead may sit before the in-gesture
// entry seeks it forward, and where the seek lands (a hair inside the buffered
// range: seeking to the exact end can stall at HAVE_CURRENT_DATA).
const SEEK_IF_BEHIND_S = 0.5;
const LIVE_EDGE_REJOIN_S = 0.1;

// The armed video is paused at (near) the live edge; its buffered ranges keep
// growing under it. Jump to the newest range's end before playing, or the
// native player would resume seconds — eventually minutes — behind live.
// Best-effort: a seek failure must not void the gesture path.
function seekToLiveEdge(video: HTMLVideoElement): void {
  try {
    const b = video.buffered;
    if (b.length === 0) return;
    const end = b.end(b.length - 1);
    if (end - video.currentTime > SEEK_IF_BEHIND_S) {
      video.currentTime = Math.max(b.start(b.length - 1), end - LIVE_EDGE_REJOIN_S);
    }
  } catch {
    // buffered/seek quirks — the entry attempt proceeds from wherever it is
  }
}

interface WebKitVideoElement extends HTMLVideoElement {
  webkitEnterFullscreen?: () => void;
  webkitExitFullscreen?: () => void;
}

interface FullscreenState {
  fullscreen: boolean;
  tier: FullscreenTier | null;
}

// The tier-2 transitions the caller reacts to (the audio hand-over). Both
// fire synchronously; the enter hook runs inside the gesture, before play().
export interface FullscreenHooks {
  onNativeEnter?: (video: HTMLVideoElement) => void;
  onNativeExit?: (video: HTMLVideoElement) => void;
}

export function useFullscreen(
  ref: RefObject<HTMLElement | null>,
  // The hidden presentation <video> on gated devices (null elsewhere and until
  // armed): tier 2's target. An element, not a ref, so the listeners
  // re-attach when it mounts.
  presentationVideo: HTMLVideoElement | null = null,
  hooks: FullscreenHooks = {},
) {
  // Read through a ref: the hooks close over live state (mute preference, the
  // audio sink) and must not re-create `toggle` — which is a dependency of the
  // control bar and the keyboard handler — on every change.
  const hooksRef = useRef(hooks);
  hooksRef.current = hooks;
  // The gate is per-device; sampled once per mount (stable across renders).
  const [tier1Available] = useState(elementFullscreenAvailable);

  const [state, setState] = useState<FullscreenState>(() => ({
    fullscreen: typeof document !== 'undefined' && document.fullscreenElement != null,
    tier:
      typeof document !== 'undefined' && document.fullscreenElement != null ? 'element' : null,
  }));
  const stateRef = useRef(state);
  stateRef.current = state;

  // Tier 1 tracking.
  useEffect(() => {
    if (!tier1Available) return;
    const onChange = () => {
      const fs = document.fullscreenElement != null;
      setState({ fullscreen: fs, tier: fs ? 'element' : null });
    };
    document.addEventListener('fullscreenchange', onChange);
    return () => document.removeEventListener('fullscreenchange', onChange);
  }, [tier1Available]);

  // Tier 2 tracking: webkitEnterFullscreen does not fire fullscreenchange —
  // state travels on these WebKit-prefixed video events (incl. the system
  // UI's own exit affordance). On exit the hidden video pauses again: playback
  // only exists for the native player, and leaving it running would keep a
  // second decode burning battery under the inline canvas.
  useEffect(() => {
    if (tier1Available || !presentationVideo) return;
    const onBegin = () => setState({ fullscreen: true, tier: 'video' });
    const onEnd = () => {
      try {
        presentationVideo.pause();
      } catch {
        // pausing a hidden video is best-effort
      }
      // The inline sink takes the audio back (the native player was the only
      // thing playing the muxed track).
      hooksRef.current.onNativeExit?.(presentationVideo);
      setState({ fullscreen: false, tier: null });
    };
    presentationVideo.addEventListener('webkitbeginfullscreen', onBegin);
    presentationVideo.addEventListener('webkitendfullscreen', onEnd);
    return () => {
      presentationVideo.removeEventListener('webkitbeginfullscreen', onBegin);
      presentationVideo.removeEventListener('webkitendfullscreen', onEnd);
    };
  }, [tier1Available, presentationVideo]);

  const toggle = useCallback(() => {
    if (tier1Available) {
      if (document.fullscreenElement) {
        void document.exitFullscreen?.();
      } else {
        void ref.current?.requestFullscreen?.();
      }
      return;
    }

    const current = stateRef.current;
    if (current.fullscreen) {
      if (current.tier === 'video') {
        const video = presentationVideo as WebKitVideoElement | null;
        video?.webkitExitFullscreen?.();
        // webkitendfullscreen confirms (and pauses the hidden video); update
        // eagerly so the button follows the tap even if the event is late,
        // and pause eagerly too: a missed event must not leave a second
        // decode running under the canvas.
        try {
          video?.pause();
        } catch {
          // best-effort
        }
        if (video) hooksRef.current.onNativeExit?.(video);
      }
      setState({ fullscreen: false, tier: null });
      return;
    }

    // Tier 2: the native video fullscreen, synchronously inside the user
    // gesture (an async hop would void it). The armed video is loaded but
    // paused: seek to the live edge, play, enter, all in-gesture.
    const video = presentationVideo as WebKitVideoElement | null;
    if (
      video &&
      typeof video.webkitEnterFullscreen === 'function' &&
      video.readyState >= HAVE_METADATA
    ) {
      try {
        seekToLiveEdge(video);
        // Hand the audio over before play(): still inside the gesture, so an
        // unmuted start is allowed, and the inline sink goes quiet before the
        // native player's first sample instead of overlapping it.
        hooksRef.current.onNativeEnter?.(video);
        // In-gesture play(): succeeds even where a muted autoplay would be
        // blocked (e.g. iOS Low Power Mode), and a paused video is exactly
        // what the native player must not be handed.
        if (video.paused) void video.play()?.catch?.(() => {});
        video.webkitEnterFullscreen();
        setState({ fullscreen: true, tier: 'video' });
        return;
      } catch {
        // InvalidStateError (no media yet, etc.) — fall through to tier 3.
      }
    }

    // Tier 3: CSS pseudo-fullscreen. The caller styles its root off `tier`.
    setState({ fullscreen: true, tier: 'pseudo' });
  }, [tier1Available, ref, presentationVideo]);

  return { isFullscreen: state.fullscreen, tier: state.fullscreen ? state.tier : null, toggle };
}
