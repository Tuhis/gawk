import { ContextMenu } from 'gawk-app';

// ContextMenu is position:fixed and clamps itself into the viewport, so the
// cells give it a tall stage and open it at a fixed coordinate.
const stage: React.CSSProperties = {
  background:
    'radial-gradient(circle at 60% 40%, #26304a 0%, transparent 60%), var(--bg)',
  color: 'var(--text)',
  fontFamily: 'system-ui, -apple-system, "Segoe UI", Roboto, sans-serif',
  minHeight: '340px',
  position: 'relative',
};

const noop = () => {};

/** The viewer's right-click menu: checked state rendered as a mark AND as ARIA. */
export const ViewerMenu = () => (
  <div style={stage}>
    <ContextMenu
      x={40}
      y={40}
      onClose={noop}
      items={[
        { label: 'Stats for nerds', onSelect: noop },
        { label: 'Playback settings', onSelect: noop },
        { label: 'Fullscreen', onSelect: noop },
        { label: 'Leave broadcast', onSelect: noop },
      ]}
    />
  </div>
);

/**
 * Selection state plus the two annotation kinds: `note` is a quiet second line
 * on an enabled item (the cost of choosing it); `reason` explains a disabled
 * one as visible text, never a tooltip — touch has no hover.
 */
export const WithStateAndReasons = () => (
  <div style={stage}>
    <ContextMenu
      x={40}
      y={40}
      onClose={noop}
      items={[
        { label: 'Lowest latency', onSelect: noop, note: '· switching reconnects' },
        { label: 'Balanced', onSelect: noop, checked: true },
        { label: 'Smoother', onSelect: noop, checked: false, note: '· switching reconnects' },
        { label: 'Interpolate frames', onSelect: noop, disabled: true, reason: 'not supported by this browser' },
      ]}
    />
  </div>
);
