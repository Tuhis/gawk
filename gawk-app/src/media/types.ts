export type PipelineStatus = 'idle' | 'starting' | 'capturing' | 'stopping' | 'error';

export interface CaptureConfig {
  codecPreferences: string[];
  width: number;
  height: number;
  bitrate: number;
  framerate: number;
  // Time-based (docs/08): frame-count cadence would stretch the GOP to 24s
  // at the ladder's 5 fps rung.
  keyframeIntervalMs: number;
}

// Ordered by preference. Encoder walks this list and picks the first one
// isConfigSupported() approves for the negotiated width/height/framerate.
// - H.264 baseline lvl 4.2 / lvl 3.1: HW on Chromium/Safari, best decode compat.
// - H.264 high profile lvl 4.0: what most HW encoders internally prefer.
// - VP9 profile 0 lvl 4.0 / lvl 3.1: cross-browser software, sometimes HW.
// - VP8: universal software fallback.
export const DEFAULT_CODEC_PREFERENCES: string[] = [
  'avc1.640034', // H.264 High Profile Level 5.2 (4K @ 60fps)
  'avc1.640033', // H.264 High Profile Level 5.1 (4K @ 30fps)
  'avc1.42E02A',
  'avc1.640028',
  'avc1.42E01F',
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
  keyframeIntervalMs: 2000,
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
