import { ViewerSettingsPanel } from 'gawk-app';

const canvas: React.CSSProperties = {
  background: 'var(--bg)',
  color: 'var(--text)',
  fontFamily: 'system-ui, -apple-system, "Segoe UI", Roboto, sans-serif',
  minHeight: '420px',
  position: 'relative',
};

// The viewer's playback settings surface: a scrim plus a right-side GlassPanel.
// Deliberately the same shape as the broadcaster's settings so the product has
// one settings idiom rather than two (docs/37 §6.3).
const balanced = {
  delivery: 'live',
  playout: 'adaptive',
  parity: 'auto',
  striping: 'auto',
  interpolation: true,
} as const;

const stable = {
  delivery: 'deep',
  playout: 'adaptive',
  parity: 1,
  striping: 'on',
  interpolation: false,
} as const;

const noop = () => {};
const handlers = {
  onPreset: noop,
  onParity: noop,
  onStriping: noop,
  onInterpolation: noop,
  onResetAdvanced: noop,
  onClose: noop,
};

/** The default preset — what an average viewer sees if they ever open this. */
export const Balanced = () => (
  <div style={canvas}>
    <ViewerSettingsPanel config={balanced} interpolationAvailable {...handlers} />
  </div>
);

/** "Most stable" with the advanced axes moved off their defaults. */
export const MostStable = () => (
  <div style={canvas}>
    <ViewerSettingsPanel config={stable} interpolationAvailable {...handlers} />
  </div>
);
