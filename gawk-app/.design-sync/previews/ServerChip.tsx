import { ServerChip, useTransportStore } from 'gawk-app';

// The landing page's only new element (R37): quiet on the default server so
// join-by-code reads exactly as before, prominent when a non-default server is
// selected. It reads the transport store, so each cell sets the state first.
const canvas: React.CSSProperties = {
  background: 'var(--bg)',
  color: 'var(--text)',
  fontFamily: 'system-ui, -apple-system, "Segoe UI", Roboto, sans-serif',
  minHeight: '100%',
  padding: '32px',
  display: 'flex',
  gap: '12px',
  alignItems: 'center',
};

function withState(patch: Record<string, unknown>) {
  useTransportStore.setState(patch as never);
  return <ServerChip />;
}

/** Quiet: on the deployment's own relay, the front door reads as it always did. */
export const Quiet = () => (
  <div style={canvas}>
    {withState({ resolvedSource: 'default', serverUrl: 'https://api.gawk.ioio.fi:4433' })}
  </div>
);

/** Prominent: a non-default server is selected, so the chip names its host. */
export const Prominent = () => (
  <div style={canvas}>
    {withState({ resolvedSource: 'selected', serverUrl: 'https://gawk.lan:4433' })}
  </div>
);
