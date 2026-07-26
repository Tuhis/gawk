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

// R16, reshaped by R22 (docs/27 Decision 8): raw presentation-surface
// diagnostics for Copy diagnostics; the NativeVideoFullscreen gate row is
// derived from these. (Named presentationSurface in ViewerStats —
// `presentation` was already taken by the R12 pacing-placement field.) The
// pipeline is worker muxer → transferred segments → main-thread
// ManagedMediaSource → hidden <video>, and each hop reports here so a broken
// native fullscreen localizes remotely: muxer producing but nothing appended
// (segmentsAppended stuck) vs appends erroring vs the element never reaching
// readiness vs a paused element.
export interface PresentationSurfaceStats {
  tier: FullscreenTier | null;
  armed: boolean;
  // Worker-side muxer output (from ViewerStats.presentationMux).
  muxInitSegments: number;
  muxMediaSegments: number;
  muxErrors: number;
  // Main-thread SourceBuffer side.
  segmentsAppended: number;
  appendErrors: number;
  // Buffered span of the element (last end − first start) and how far the
  // playhead sits behind the buffered live edge — the fullscreen half of the
  // inline-vs-fullscreen live-edge delta. Null until media is buffered.
  bufferedMs: number | null;
  bufferedAheadMs: number | null;
  // HTMLMediaElement.readyState (0–4); null until the element exists.
  elementReadyState: number | null;
  elementPaused: boolean | null;
  // videoWidth×videoHeight — 0×0 until the element decodes a first frame.
  elementWidth: number | null;
  elementHeight: number | null;
  // Frames the element actually presented, counted via
  // requestVideoFrameCallback; null where rVFC (or the element) is absent.
  elementFrames: number | null;
}
