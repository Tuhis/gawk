import { BroadcasterStatsOverlay } from 'gawk-app';

// The broadcaster's counterpart to the viewer overlay: the encode funnel
// (capture → post-gate → encoded → sent) plus this leg's connection health,
// so "is it my machine or my uplink" is answerable without leaving the stream.
const stage: React.CSSProperties = {
  background:
    'radial-gradient(circle at 30% 25%, #1f3348 0%, transparent 60%),' +
    'radial-gradient(circle at 78% 72%, #3a2440 0%, transparent 60%),' +
    'var(--bg)',
  color: 'var(--text)',
  fontFamily: 'system-ui, -apple-system, "Segoe UI", Roboto, sans-serif',
  minHeight: '620px',
  padding: '24px',
  position: 'relative',
};

const noop = () => {};

// Cast: the overlay reads a wide slice of BroadcastStats and the preview only
// needs the fields it actually renders.
const healthy = {
  captureFps: 60,
  fpsGateDropped: 0,
  encoderFps: 59.9,
  sentFps: 59.9,
  encodedFrames: 18402,
  droppedFrames: 0,
  encoderQueueDepth: 1,
  lastEncodeLatencyMs: 3.1,
  keyframes: 154,
  keyframeStreamsSent: 154,
  keyframeStreamsFailed: 0,
  datagramsSent: 241880,
  viewerCount: 7,
  autoCeiling: 1440,
  autoFps: 60,
  pipelineContext: 'worker',
  timeSyncRttMs: 8,
  audioState: 'sharing',
  connection: {
    rttMs: 9,
    rttVarMs: 2,
    packetsLost: 38,
    estimatedSendRateBps: 24_000_000,
    atSendCapacity: false,
  },
} as never;

// An uplink that has run out of headroom: the auto ladder has stepped down and
// the send path is at capacity.
const constrained = {
  captureFps: 60,
  fpsGateDropped: 118,
  encoderFps: 30.1,
  sentFps: 29.6,
  encodedFrames: 9120,
  droppedFrames: 64,
  encoderQueueDepth: 7,
  lastEncodeLatencyMs: 14.6,
  keyframes: 76,
  keyframeStreamsSent: 74,
  keyframeStreamsFailed: 2,
  datagramsSent: 98431,
  viewerCount: 41,
  autoCeiling: 720,
  autoFps: 30,
  pipelineContext: 'main-thread',
  timeSyncRttMs: 61,
  audioState: 'no-track',
  connection: {
    rttMs: 148,
    rttVarMs: 37,
    packetsLost: 2914,
    estimatedSendRateBps: 3_400_000,
    atSendCapacity: true,
  },
} as never;

/** A healthy hardware encode at the top rung. */
export const Healthy = () => (
  <div style={stage}>
    <BroadcasterStatsOverlay
      stats={healthy}
      encoderInfo={{ codec: 'avc1.42E02A', acceleration: 'hardware', width: 2560, height: 1440, framerate: 60 } as never}
      bitrateBps={7_800_000}
      onClose={noop}
      onCopy={noop}
      copied={false}
    />
  </div>
);

/** Uplink-constrained: the auto ladder has stepped down to 720p30. */
export const UplinkConstrained = () => (
  <div style={stage}>
    <BroadcasterStatsOverlay
      stats={constrained}
      encoderInfo={{ codec: 'avc1.42E01F', acceleration: 'software', width: 1280, height: 720, framerate: 30 } as never}
      bitrateBps={3_400_000}
      onClose={noop}
      onCopy={noop}
      copied={false}
    />
  </div>
);
