// R16 (docs/21 Decision 9): gate-controlled features, reported by a surface
// for its stats overlay's Feature Gates section — the generic home for "which
// conditional features are live on this client". Names are UpperCamelCase by
// policy, enforced as this string-literal union so they can't drift per call
// site. Future gates (paced playout, interpolation, worker placement, audio)
// are natural later entries.
export type FeatureGateName = 'NativeVideoFullscreen';

export interface FeatureGate {
  name: FeatureGateName;
  active: boolean;
  // The resolved state, human-readable (e.g. 'armed', 'probe failed → pseudo').
  detail?: string;
}

// R16: which fullscreen tier is active — element fullscreen (the standard
// API), the iPhone-only native video fullscreen, or CSS pseudo-fullscreen.
export type FullscreenTier = 'element' | 'video' | 'pseudo';

// R16: raw presentation-surface diagnostics for Copy diagnostics; the
// NativeVideoFullscreen gate row is derived from these. (Named
// presentationSurface in ViewerStats — `presentation` was already taken by
// the R12 pacing-placement field.) The element* fields (U4 black-screen
// finding) report the presentation <video>'s own view of the tee stream, so
// a black native fullscreen localizes remotely: tee writing but element
// starved (elementFrames stuck) vs element presenting black content
// (elementFrames climbing) vs a paused element (black by definition).
export interface PresentationSurfaceStats {
  tier: FullscreenTier | null;
  armed: boolean;
  teedFrames: number;
  teeErrors: number;
  // HTMLMediaElement.readyState (0–4); null until the element exists.
  elementReadyState: number | null;
  elementPaused: boolean | null;
  // videoWidth×videoHeight — 0×0 until the element decodes a first frame.
  elementWidth: number | null;
  elementHeight: number | null;
  // Frames the element actually presented, counted via
  // requestVideoFrameCallback; null where rVFC (or the element) is absent.
  elementFrames: number | null;
  // Max RGB channel (0–255) from a periodic 4×4 pixel sample of the element —
  // 0 with a bright inline stream ⇒ the element is presenting black frames;
  // high ⇒ content is fine and any black fullscreen is the player itself.
  elementContentPeak: number | null;
}
