import { useCallback, useEffect, useRef, useState, type RefObject } from 'react';
import type { FullscreenTier } from './featureGates';

// Fullscreen for a target element (docs/10 J3), tiered since R16 (docs/21
// Decision 2). Where the Element Fullscreen API exists (desktop, Android,
// iPad) tier 1 is the entire feature, byte-identical to the pre-R16 hook. On
// element-fullscreen-less devices (iPhone — WebKit ships the API on iPadOS
// only, and every iOS browser is WebKit) the toggle tries the one native
// fullscreen that exists there, HTMLVideoElement.webkitEnterFullscreen() on
// the presentation video, and falls through to CSS pseudo-fullscreen so the
// button always visibly does something.
//
// R22 (docs/27 Decision 2): the presentation video's media source changed
// from the R16 MediaStream tee (black on iOS — docs/21 U4) to an
// MSE/ManagedMediaSource feed; tier 2 gains a seek-to-live before the
// in-gesture play (the armed video sits paused while its buffer follows the
// live edge — docs/27 Decision 5) and pauses the hidden video again on exit
// so the second decode stops with the native player.
//
// State tracking is per tier: `fullscreenchange` (tier 1),
// `webkitbeginfullscreen`/`webkitendfullscreen` on the video (tier 2 — the
// native fullscreen does NOT fire fullscreenchange), local state (tier 3).

// R16 Decision 1: the device gate. Absence of Element.requestFullscreen is
// effectively an iPhone signature; on devices where it exists, no R16 code
// path activates.
export function elementFullscreenAvailable(): boolean {
  return (
    typeof document !== 'undefined' &&
    typeof document.documentElement.requestFullscreen === 'function'
  );
}

// webkitEnterFullscreen needs media at readyState ≥ HAVE_METADATA.
const HAVE_METADATA = 1;

// R22: how far behind the buffered end the playhead may sit before the
// in-gesture entry seeks it forward, and where the seek lands (a hair inside
// the buffered range — seeking to the exact end can stall HAVE_CURRENT_DATA).
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

export function useFullscreen(
  ref: RefObject<HTMLElement | null>,
  // R16: the hidden presentation <video> on gated devices (null elsewhere and
  // until armed) — tier 2's target. Passed as an element, not a ref, so the
  // event listeners re-attach when it mounts.
  presentationVideo: HTMLVideoElement | null = null,
) {
  // The gate is per-device; sampled once per mount (stable across renders).
  const [tier1Available] = useState(elementFullscreenAvailable);

  const [state, setState] = useState<FullscreenState>(() => ({
    fullscreen: typeof document !== 'undefined' && document.fullscreenElement != null,
    tier:
      typeof document !== 'undefined' && document.fullscreenElement != null ? 'element' : null,
  }));
  const stateRef = useRef(state);
  stateRef.current = state;

  // Tier 1 tracking — exactly the pre-R16 behavior.
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
  // UI's own exit affordance). On exit the hidden video pauses again (R22):
  // playback only exists for the native player, and leaving it running would
  // keep a second decode burning battery under the inline canvas.
  useEffect(() => {
    if (tier1Available || !presentationVideo) return;
    const onBegin = () => setState({ fullscreen: true, tier: 'video' });
    const onEnd = () => {
      try {
        presentationVideo.pause();
      } catch {
        // pausing a hidden video is best-effort
      }
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
        // and pause eagerly too — a missed event must not leave a second
        // decode running under the canvas (R22).
        try {
          video?.pause();
        } catch {
          // best-effort
        }
      }
      setState({ fullscreen: false, tier: null });
      return;
    }

    // Tier 2: the native video fullscreen, synchronously inside the user
    // gesture (an async hop here would void the gesture — docs/21). The
    // armed MSE video is loaded-but-paused (docs/27 Decision 5): seek to the
    // live edge, play, enter — all in-gesture.
    const video = presentationVideo as WebKitVideoElement | null;
    if (
      video &&
      typeof video.webkitEnterFullscreen === 'function' &&
      video.readyState >= HAVE_METADATA
    ) {
      try {
        seekToLiveEdge(video);
        // In-gesture play(): succeeds even where a muted autoplay would be
        // blocked (e.g. iOS Low Power Mode) — and a paused video is exactly
        // what the native player must not be handed (docs/21 U4).
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
