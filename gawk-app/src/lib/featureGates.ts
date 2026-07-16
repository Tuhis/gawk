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
// the R12 pacing-placement field.)
export interface PresentationSurfaceStats {
  tier: FullscreenTier | null;
  armed: boolean;
  teedFrames: number;
  teeErrors: number;
}
