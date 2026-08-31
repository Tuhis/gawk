import { Button } from 'gawk-app';

// gawk is a dark-only design system (global.css sets `color-scheme: dark` and
// paints html/body with --bg). Preview cells render in isolation, so each one
// paints the canvas itself — .primary is a near-white fill and .ghost is muted
// gray, both of which misread badly on a default white page.
const canvas: React.CSSProperties = {
  background: 'var(--bg)',
  color: 'var(--text)',
  fontFamily: 'system-ui, -apple-system, "Segoe UI", Roboto, sans-serif',
  minHeight: '100%',
  padding: '28px',
  display: 'flex',
  gap: '12px',
  alignItems: 'center',
  flexWrap: 'wrap',
};

/** The four shipped variants — the only place button styling is defined. */
export const Variants = () => (
  <div style={canvas}>
    <Button variant="primary">Go live</Button>
    <Button variant="secondary">Copy join link</Button>
    <Button variant="ghost">Cancel</Button>
    <Button variant="danger">End broadcast</Button>
  </div>
);

/** Disabled drops to 45% opacity across every variant. */
export const Disabled = () => (
  <div style={canvas}>
    <Button variant="primary" disabled>Go live</Button>
    <Button variant="secondary" disabled>Copy join link</Button>
    <Button variant="ghost" disabled>Cancel</Button>
    <Button variant="danger" disabled>End broadcast</Button>
  </div>
);

/** The primary action as it appears on the broadcaster's front door. */
export const Primary = () => (
  <div style={{ ...canvas, padding: '40px' }}>
    <Button variant="primary">Share my screen</Button>
  </div>
);
