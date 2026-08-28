import { StatsPanel } from 'gawk-app';

// StatsPanel floats over the stream, so the cells stage it over a stand-in for
// live pixels rather than flat black.
const stage: React.CSSProperties = {
  background:
    'radial-gradient(circle at 30% 25%, #23304f 0%, transparent 60%),' +
    'radial-gradient(circle at 80% 75%, #402238 0%, transparent 60%),' +
    'var(--bg)',
  color: 'var(--text)',
  fontFamily: 'system-ui, -apple-system, "Segoe UI", Roboto, sans-serif',
  minHeight: '460px',
  padding: '24px',
  position: 'relative',
};

const noop = () => {};

// Values arrive pre-formatted ("—" for unavailable) — the panel is purely
// presentational and never computes or formats anything itself.
const sections = [
  {
    title: 'Video',
    rows: [
      ['Codec', 'H.264 · avc1.42E02A'],
      ['Resolution', '2560×1440'],
      ['Decoder', 'hardware'],
      ['Frames decoded', '18 402'],
      ['Frames dropped', '11'],
      ['Decode fps', '59.9'],
    ] as [string, string, string?][],
  },
  {
    title: 'Transport',
    rows: [
      ['Bitrate', '7.8 Mbps'],
      ['Datagrams', '241 880'],
      ['Lost', '38 (0.02%)'],
      ['RTT', '9 ms'],
    ] as [string, string, string?][],
  },
  {
    title: 'Feature gates',
    rows: [
      ['WebTransport', '✓'],
      ['WebCodecs', '✓'],
      ['Hardware decode', '✓', 'VideoDecoder reported hardware acceleration'],
      ['Worker offload', '✗', 'MediaStreamTrackProcessor is Window-only on this browser'],
    ] as [string, string, string?][],
  },
];

/** The shared sectioned overlay the viewer and broadcaster both build on. */
export const Viewer = () => (
  <div style={stage}>
    <StatsPanel ariaLabel="Stream stats" sections={sections} onClose={noop} onCopy={noop} />
  </div>
);

/** After "Copy diagnostics" — the button flashes its confirmation. */
export const Copied = () => (
  <div style={stage}>
    <StatsPanel ariaLabel="Stream stats" sections={sections} onClose={noop} onCopy={noop} copied />
  </div>
);

/** With a footer note, and no copy action offered. */
export const WithFooter = () => (
  <div style={stage}>
    <StatsPanel
      ariaLabel="Stream stats"
      sections={sections.slice(0, 2)}
      onClose={noop}
      footer={<>Session 8f2a1c · sampled every 1 s</>}
    />
  </div>
);
