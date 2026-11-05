// Conditional features a surface reports for its stats overlay's Feature
// Gates section. The union keeps the names from drifting per call site.
export type FeatureGateName = 'NativeVideoFullscreen' | 'DatagramReceiveBuffer';

export interface FeatureGate {
  name: FeatureGateName;
  active: boolean;
  // The resolved state, human-readable (e.g. 'armed', 'probe failed → pseudo').
  detail?: string;
}

// Which fullscreen tier is active: element fullscreen (the standard API), the
// iPhone-only native video fullscreen, or CSS pseudo-fullscreen.
export type FullscreenTier = 'element' | 'video' | 'pseudo';

// Presentation-surface diagnostics (the iPhone native fullscreen path) for
// Copy diagnostics; the NativeVideoFullscreen gate row derives from these. The
// path is worker muxer → transferred segments → main-thread ManagedMediaSource
// → hidden <video>, and each hop reports here so a broken fullscreen can be
// localized remotely: nothing appended, appends erroring, the element never
// ready, or the element paused.
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
  // The fields that localize "nothing was appended". `segmentsReceived` 0 means the worker→main→sink hop is broken (the muxer's
  // own counters live in the worker and keep climbing regardless);
  // `segmentsQueued` at the bound with nothing appended means the appender is
  // holding data the system will not take; `segmentsDroppedNoInit` counts media
  // discarded for want of an init segment, which is permanent once it starts
  // (the muxer emits its init exactly once per session). `mmsStreaming` is
  // ManagedMediaSource's own "I want data" flag — null on classic MediaSource.
  segmentsReceived: number;
  segmentsQueued: number;
  segmentsDroppedNoInit: number;
  // Segments shed by a queue overflow — what MMS parking a fed, paused element
  // for longer than the queue depth produces — and how many resumes after one
  // found a hole and healed it (docs/27 finding 3 item 5). A short
  // `segmentsAppended` with `segmentsDroppedOverflow` 0 is some other cause.
  segmentsDroppedOverflow: number;
  discontinuitiesHealed: number;
  mmsStreaming: boolean | null;
  // The reason for the most recent append failure. Null until one happens.
  lastAppendError: string | null;
  // Whether this MediaSource accepted duration = Infinity, i.e. whether the
  // native player treats a buffer underrun as a stall rather than the end.
  // Null before a source exists.
  liveDuration: boolean | null;
  // The audio track. `audioMode` is the resolved verdict for the stream's
  // audio lane: 'none' (no audio in the broadcast), 'muxed' (Opus in MP4
  // accepted — the native player has its own audio), or the refusal reason.
  audioMode: string;
  // The AAC transcoder's state on that path ('idle' | 'active' | 'unsupported'
  // | 'error'), null otherwise: whether an iPhone can encode AAC at all.
  audioTranscode: string | null;
  audioSegmentsAppended: number;
  audioTrackActive: boolean;
  // How many audio tracks the element ended up with: the only end-of-chain
  // proof that a muxed track became playable audio (`audioTrackActive` says a
  // SourceBuffer exists, not that the demuxer accepted it). Null where
  // HTMLMediaElement.audioTracks is unavailable.
  elementAudioTracks: number | null;
  muxAudioSegments: number;
  muxAudioHoles: number;
  // How many disjoint buffered ranges the element holds. > 1 means a hole in the
  // playable (intersection) timeline — the shape that stalls the native player.
  bufferedRanges: number | null;
  // Buffered span of the element (last end − first start) and how far the
  // playhead sits behind the buffered live edge — the fullscreen half of the
  // inline-vs-fullscreen live-edge delta. Null until media is buffered.
  bufferedMs: number | null;
  bufferedAheadMs: number | null;
  // The presenter's byte ledger: video bytes held by the current MediaSource
  // and video bytes pruned this session (the bound is PRUNE_BUDGET_BYTES).
  bufferedBytes: number;
  prunedBytes: number;
  // HTMLMediaElement.readyState (0–4); null until the element exists.
  elementReadyState: number | null;
  elementPaused: boolean | null;
  // videoWidth×videoHeight — 0×0 until the element decodes a first frame.
  elementWidth: number | null;
  elementHeight: number | null;
  // Frames the element actually presented, counted via
  // requestVideoFrameCallback; null where rVFC (or the element) is absent.
  elementFrames: number | null;
  // The tier-2 stall watchdog (presentationWatchdog.ts): recovery actions taken
  // while native fullscreen was up, how many of them seeked (hole skip,
  // live-edge landing, nudge), play() rejections among them, and the event that
  // triggered the latest. A healthy session reads 0; a climbing count is a
  // player that keeps stalling and being rescued — docs/27's on-device pass
  // reads this to tell the two apart.
  stallRecoveries: number;
  stallRecoverySeeks: number;
  stallRecoveryPlayErrors: number;
  lastStallCause: string | null;
}
