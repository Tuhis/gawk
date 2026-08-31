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
 * The WebKit case: Safari and every iOS browser refuse the WebTransport
 * connection, so live broadcasts look offline. Deliberately an acknowledgment
 * rather than a block — the user is told what will happen, then let through.
 */
export const Webkit = () => (
  <div style={canvas}>
    <UnsupportedBrowserModal
      support={{ supported: false, reason: 'webkit', browserLabel: 'Safari' }}
      onContinue={noop}
    />
  </div>
);

/** The generic case: an engine with no WebTransport at all. */
export const NoWebTransport = () => (
  <div style={canvas}>
    <UnsupportedBrowserModal
      support={{ supported: false, reason: 'no-webtransport', browserLabel: 'This browser' }}
      onContinue={noop}
    />
  </div>
);
