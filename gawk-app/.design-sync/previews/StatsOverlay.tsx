import { StatsOverlay } from 'gawk-app';

// The viewer's "is it the stream or my machine" overlay — a thin row-builder
// over StatsPanel. It floats over live pixels, so the cells stage it there.
const stage: React.CSSProperties = {
  background:
    'radial-gradient(circle at 30% 25%, #23304f 0%, transparent 60%),' +
    'radial-gradient(circle at 80% 75%, #402238 0%, transparent 60%),' +
    'var(--bg)',
  color: 'var(--text)',
  fontFamily: 'system-ui, -apple-system, "Segoe UI", Roboto, sans-serif',
  minHeight: '620px',
  padding: '24px',
  position: 'relative',
};

const noop = () => {};

// A healthy hardware-decode session on the live-edge path. Cast because the
// overlay reads a wide slice of ViewerStats and the preview only needs the
// fields it actually renders.
const healthy = {
  frameWidth: 2560,
  frameHeight: 1440,
  receivedFps: 59.94,
  decoderFps: 59.91,
  renderedFps: 59.9,
  isHardwareAccelerated: true,
  renderer: 'webgl',
  pipelineContext: 'worker',
  presentation: 'paced-raf',
  interpolation: 'on',
  decoderQueueDepth: 1,
  lastDecodeLatencyMs: 2.4,
  decodedFrames: 18402,
  viewerCount: 7,
  deliveryMode: 'datagrams',
  framesCompleted: 18391,
  framesDroppedIncomplete: 7,
  framesDroppedLate: 4,
  connection: { rttMs: 9 },
} as never;

// The resilient path: reliable carrier streams instead of raw datagrams, on a
// software decoder — the shape of a session that is working hard.
const resilient = {
  frameWidth: 1920,
  frameHeight: 1080,
  receivedFps: 48.2,
  decoderFps: 47.9,
  renderedFps: 47.5,
  isHardwareAccelerated: false,
  renderer: '2d',
  pipelineContext: 'main-thread',
  presentation: 'paced-timer',
  interpolation: 'off',
  decoderQueueDepth: 6,
  lastDecodeLatencyMs: 11.8,
  decodedFrames: 9120,
  viewerCount: 34,
  deliveryMode: 'reliable',
  carrierStreams: 212,
  carrierStreamsAborted: 3,
  carrierRecords: 9120,
  framesCompleted: 9084,
  framesDroppedIncomplete: 22,
  framesDroppedLate: 14,
  connection: { rttMs: 143 },
} as never;

const gates = [
  { name: 'WebTransport', state: 'on', detail: 'datagrams and uni streams available' },
  { name: 'WebCodecs', state: 'on', detail: 'VideoDecoder present' },
  { name: 'Hardware decode', state: 'on', detail: 'decoder reported acceleration' },
  { name: 'Worker offload', state: 'off', detail: 'MediaStreamTrackProcessor is Window-only here' },
] as never;

/** A healthy hardware session on the live-edge datagram path. */
export const Healthy = () => (
  <div style={stage}>
    <StatsOverlay
      stats={healthy}
      codec="H.264 · avc1.42E02A"
      bitrateBps={7_800_000}
      telemetrySessionId="8f2a1c74d0b93e5518aa27c6"
      onClose={noop}
      onCopy={noop}
      copied={false}
    />
  </div>
);

/** The resilient path, with the Feature Gates section a gated device reports. */
export const ResilientWithGates = () => (
  <div style={stage}>
    <StatsOverlay
      stats={resilient}
      codec="H.264 · avc1.42E01F"
      bitrateBps={3_100_000}
      featureGates={gates}
      telemetrySessionId={null}
      onClose={noop}
      onCopy={noop}
      copied={false}
    />
  </div>
);

/** Before the first samples land: every row honestly reads "—". */
export const NoDataYet = () => (
  <div style={stage}>
    <StatsOverlay
      stats={null}
      codec={null}
      bitrateBps={null}
      onClose={noop}
      onCopy={noop}
      copied={false}
    />
  </div>
);
