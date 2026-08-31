import { StatsGrid } from 'gawk-app';

const canvas: React.CSSProperties = {
  background: 'var(--bg)',
  color: 'var(--text)',
  fontFamily: 'system-ui, -apple-system, "Segoe UI", Roboto, sans-serif',
  minHeight: '100%',
  padding: '28px',
};

/** The frozen debug pages' at-a-glance readout — label/value pairs, pre-formatted. */
export const BroadcastRun = () => (
  <div style={canvas}>
    <StatsGrid
      items={[
        ['Resolution', '2560×1440'],
        ['Framerate', '60 fps'],
        ['Codec', 'avc1.42E02A'],
        ['Bitrate', '7.8 Mbps'],
        ['Frames encoded', '18 402'],
        ['Frames sent', '18 391'],
        ['Datagrams', '241 880'],
        ['RTT', '9 ms'],
      ]}
    />
  </div>
);

/** A short grid — the layout has to hold with only a few items. */
export const Minimal = () => (
  <div style={canvas}>
    <StatsGrid
      items={[
        ['Status', 'capturing'],
        ['Encoder', 'hardware'],
        ['Dropped', '0'],
      ]}
    />
  </div>
);
