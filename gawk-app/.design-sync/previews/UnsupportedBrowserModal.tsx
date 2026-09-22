import { UnsupportedBrowserModal } from 'gawk-app';

const canvas: React.CSSProperties = {
  background: 'var(--bg)',
  color: 'var(--text)',
  fontFamily: 'system-ui, -apple-system, "Segoe UI", Roboto, sans-serif',
  minHeight: '420px',
  position: 'relative',
};

const noop = () => {};

/**
 * Shown when the browser has no WebTransport, the one API every gawk stream
 * rides on. Deliberately an acknowledgment rather than a block — the user is
 * told what will happen, then let through. `browserLabel` is always
 * "This browser": detection is a capability probe, never a user-agent sniff.
 */
export const NoWebTransport = () => (
  <div style={canvas}>
    <UnsupportedBrowserModal
      support={{ supported: false, reason: 'no-webtransport', browserLabel: 'This browser' }}
      onContinue={noop}
    />
  </div>
);
