import { LadderPicker, useBroadcastSettingsStore } from 'gawk-app';

// Resolution + framerate rungs. Store-backed, and never disabled as a whole:
// changing rungs mid-broadcast is a supported operation (docs/08).
const canvas: React.CSSProperties = {
  background: 'var(--bg)',
  color: 'var(--text)',
  fontFamily: 'system-ui, -apple-system, "Segoe UI", Roboto, sans-serif',
  minHeight: '100%',
  padding: '28px',
  maxWidth: '620px',
};

// A probe matrix that backs the per-option annotations: hardware up to 1080p,
// software above it. `null` (the default) renders every option unannotated.
const matrix = {
  source: { width: 2560, height: 1440 },
  hwPreference: 'auto',
  entries: new Map(),
  get: (rung: number, framerate: number) =>
    rung <= 1080 && framerate <= 60
      ? { acceleration: 'hardware', codec: 'avc1.42E02A' }
      : { acceleration: 'software', codec: 'avc1.42E02A' },
} as never;

function withSelection(resolutionSelection: unknown, framerateSelection: unknown) {
  useBroadcastSettingsStore.setState({ resolutionSelection, framerateSelection } as never);
}

/** The default: auto on both axes — R4's automatic rung selection. */
export const Auto = () => {
  withSelection('auto', 'auto');
  return <div style={canvas}><LadderPicker matrix={matrix} /></div>;
};

/** A pinned rung, with the probe matrix annotating what each option would get. */
export const PinnedRung = () => {
  withSelection(1080, 60);
  return <div style={canvas}><LadderPicker matrix={matrix} /></div>;
};

/** No probe matrix available — every option renders unannotated. */
export const Unannotated = () => {
  withSelection('native', 'native');
  return <div style={canvas}><LadderPicker matrix={null} /></div>;
};
