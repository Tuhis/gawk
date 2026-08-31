import { CaptureControls, usePipelineStore } from 'gawk-app';

// CaptureControls swaps its own button from Start to Stop off the pipeline
// store's status, so each cell drives that status rather than passing a prop.
const canvas: React.CSSProperties = {
  background: 'var(--bg)',
  color: 'var(--text)',
  fontFamily: 'system-ui, -apple-system, "Segoe UI", Roboto, sans-serif',
  minHeight: '100%',
  padding: '28px',
};

const noop = () => {};

function withStatus(status: string) {
  usePipelineStore.setState({ status } as never);
  return <CaptureControls onStart={noop} onStop={noop} />;
}

/** Idle: the Start button, and the status pill reading the current state. */
export const Idle = () => <div style={canvas}>{withStatus('idle')}</div>;

/** Capturing: the control flips to the danger-styled Stop. */
export const Capturing = () => <div style={canvas}>{withStatus('capturing')}</div>;

/** Starting counts as running, so the control is already Stop — but busy, so disabled. */
export const Starting = () => <div style={canvas}>{withStatus('starting')}</div>;
