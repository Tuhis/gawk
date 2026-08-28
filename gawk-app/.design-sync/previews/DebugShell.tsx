import { DebugShell } from 'gawk-app';

const canvas: React.CSSProperties = {
  background: 'var(--bg)',
  color: 'var(--text)',
  fontFamily: 'system-ui, -apple-system, "Segoe UI", Roboto, sans-serif',
  minHeight: '260px',
};

const body: React.CSSProperties = { padding: '24px', color: 'var(--muted)', fontSize: 'var(--fs-sm)' };

/**
 * Chrome for the frozen diagnostic pages — reachable only by typing #/debug,
 * never linked from the production UI. Deliberately plain; the production
 * surfaces share no components with it.
 */
export const Loopback = () => (
  <div style={canvas}>
    <DebugShell active="#/debug/loopback">
      <div style={body}>Encode → decode loopback, no relay in the path.</div>
    </DebugShell>
  </div>
);

/** A different tab active — the nav marks which frozen page you are on. */
export const Broadcast = () => (
  <div style={canvas}>
    <DebugShell active="#/debug/broadcast">
      <div style={body}>Raw broadcaster harness: capture, encode, publish.</div>
    </DebugShell>
  </div>
);
