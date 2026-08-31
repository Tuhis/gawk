import { GlassPanel } from 'gawk-app';

// GlassPanel is a translucent, blurred surface meant to float over LIVE PIXELS
// (viewer/broadcaster chrome, the landing card, menus). On flat black the blur
// and translucency are invisible, so these cells put it over a stand-in for
// the stream — that is the only context in which the component is truthful.
const stage: React.CSSProperties = {
  background:
    'radial-gradient(circle at 25% 30%, #2b3a6b 0%, transparent 55%),' +
    'radial-gradient(circle at 75% 70%, #6b2b45 0%, transparent 55%),' +
    'var(--bg)',
  color: 'var(--text)',
  fontFamily: 'system-ui, -apple-system, "Segoe UI", Roboto, sans-serif',
  minHeight: '260px',
  padding: '32px',
  display: 'flex',
  gap: '20px',
  alignItems: 'center',
  justifyContent: 'center',
};

/** The canonical use: floating chrome over the stream. */
export const OverStream = () => (
  <div style={stage}>
    <GlassPanel style={{ padding: '20px 24px', maxWidth: '320px' }}>
      <p style={{ margin: '0 0 6px', fontSize: 'var(--fs-lg)', fontWeight: 600 }}>Sharing tips</p>
      <p style={{ margin: 0, color: 'var(--muted)', fontSize: 'var(--fs-sm)', lineHeight: 1.5 }}>
        Pick <strong>Entire screen</strong> so exclusive-fullscreen games keep streaming.
      </p>
    </GlassPanel>
  </div>
);

/** As a compact status pill — the same surface at a smaller radius of content. */
export const Pill = () => (
  <div style={stage}>
    <GlassPanel style={{ padding: '10px 16px', display: 'inline-flex', gap: '10px', alignItems: 'center' }}>
      <span style={{ width: 8, height: 8, borderRadius: '50%', background: 'var(--live)' }} />
      <span style={{ fontSize: 'var(--fs-sm)' }}>Live · 3 viewers</span>
    </GlassPanel>
  </div>
);
