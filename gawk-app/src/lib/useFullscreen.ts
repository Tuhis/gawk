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
  // UI's own exit affordance).
  useEffect(() => {
    if (tier1Available || !presentationVideo) return;
    const onBegin = () => setState({ fullscreen: true, tier: 'video' });
    const onEnd = () => setState({ fullscreen: false, tier: null });
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
        // webkitendfullscreen confirms; update eagerly so the button follows
        // the tap even if the event is late.
      }
      setState({ fullscreen: false, tier: null });
      return;
    }

    // Tier 2: the native video fullscreen, synchronously inside the user
    // gesture (an async hop here would void the gesture — docs/21).
    const video = presentationVideo as WebKitVideoElement | null;
    if (
      video &&
      typeof video.webkitEnterFullscreen === 'function' &&
      video.readyState >= HAVE_METADATA
    ) {
      try {
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
