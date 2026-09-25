import type { HwPreference } from './probe';

export type PipelineStatus = 'idle' | 'starting' | 'capturing' | 'stopping' | 'error';

export interface CaptureConfig {
  codecPreferences: string[];
  width: number;
  height: number;
  bitrate: number;
  framerate: number;
  // Absent means 'auto': prefer hardware, fall back to software.
  hwPreference?: HwPreference;
  // Time-based, not frame-count: a frame-count cadence would stretch the GOP
  // to 24s at the ladder's 5 fps rung. A short 500ms GOP bounds recovery from
  // a lost or gap-discarded frame to <=0.5s (a delta referencing a missing
  // frame corrupts everything until the next keyframe).
  keyframeIntervalMs: number;
  // Request system audio in the getDisplayMedia grant. Snapshot at broadcast
  // start and never live-applied: an audio track can't be added without
  // re-prompting.
  audio?: boolean;
}

// Ordered by preference. The encoder walks this list and picks the first one
// isConfigSupported() approves for the negotiated width/height/framerate.
// - H.264 Main L5.2 / L5.1 first: the levels that fit 4K capture. Main, not
//   High, because Chrome's VideoDecoder cannot decode High at L5.1/L5.2.
// - Then a baseline/main/high ladder at L4.2, L4.0 and L3.1 for smaller
//   captures and encoders that reject the higher levels.
// - VP9 profile 0 L4.0 / L3.1: cross-browser software, sometimes HW.
// - VP8: universal software fallback.
export const DEFAULT_CODEC_PREFERENCES: string[] = [
  'avc1.4D4034', // H.264 Main Profile Level 5.2 (4K @ 60fps)
  'avc1.4D4033', // H.264 Main Profile Level 5.1 (4K @ 30fps)
  'avc1.42E02A', // H.264 Constrained Baseline Profile Level 4.2
  'avc1.4D402A', // H.264 Main Profile Level 4.2
  'avc1.640028', // H.264 High Profile Level 4.0
  'avc1.4D4028', // H.264 Main Profile Level 4.0
  'avc1.42E01F', // H.264 Constrained Baseline Profile Level 3.1
  'avc1.4D401F', // H.264 Main Profile Level 3.1
  'vp09.00.40.08',
  'vp09.00.31.08',
  'vp8',
];

export const DEFAULT_CAPTURE_CONFIG: CaptureConfig = {
  codecPreferences: DEFAULT_CODEC_PREFERENCES,
  width: 3840,
  height: 2160,
  bitrate: 6_000_000,
  framerate: 60,
  keyframeIntervalMs: 500,
};

export interface PipelineStats {
  encodedFrames: number;
  decodedFrames: number;
  droppedFrames: number;
  keyframes: number;
  encoderQueueDepth: number;
  decoderQueueDepth: number;
  lastEncodeLatencyMs: number;
  lastDecodeLatencyMs: number;
  lastEndToEndLatencyMs: number;
  encoderFps: number;
  decoderFps: number;
}

export const EMPTY_STATS: PipelineStats = {
  encodedFrames: 0,
  decodedFrames: 0,
  droppedFrames: 0,
  keyframes: 0,
  encoderQueueDepth: 0,
  decoderQueueDepth: 0,
  lastEncodeLatencyMs: 0,
  lastDecodeLatencyMs: 0,
  lastEndToEndLatencyMs: 0,
  encoderFps: 0,
  decoderFps: 0,
};
