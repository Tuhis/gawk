import { CodeInput } from 'gawk-app';

const canvas: React.CSSProperties = {
  background: 'var(--bg)',
  color: 'var(--text)',
  fontFamily: 'system-ui, -apple-system, "Segoe UI", Roboto, sans-serif',
  minHeight: '100%',
  padding: '36px',
  display: 'flex',
  flexDirection: 'column',
  gap: '14px',
  alignItems: 'center',
};

const noop = () => {};

/**
 * Segmented join-code entry. One visually-hidden input captures all typing and
 * paste; the six boxes are presentational, rendered from the value. The active
 * box is the next empty slot, which reads as auto-advance.
 */
export const PartiallyFilled = () => (
  <div style={canvas}>
    <CodeInput value="AB2" onChange={noop} />
  </div>
);

/** Empty — the front-door state, with the first box active. */
export const Empty = () => (
  <div style={canvas}>
    <CodeInput value="" onChange={noop} />
  </div>
);

/** Complete: every box filled, which is what fires onComplete. */
export const Complete = () => (
  <div style={canvas}>
    <CodeInput value="AB2CD3" onChange={noop} />
  </div>
);
