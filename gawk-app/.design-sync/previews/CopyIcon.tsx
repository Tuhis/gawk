import { CopyIcon } from 'gawk-app';

// gawk's icons draw with currentColor and are sized 1em, so they scale with
// font-size and inherit the surrounding text color. A default card would show
// a ~16px muted speck on a dark canvas, so these cells set an explicit size
// and tone — the two facts that are actually the icon API.
const canvas: React.CSSProperties = {
  background: 'var(--bg)',
  color: 'var(--text)',
  fontFamily: 'system-ui, -apple-system, "Segoe UI", Roboto, sans-serif',
  minHeight: '100%',
  padding: '28px',
  display: 'flex',
  gap: '24px',
  alignItems: 'center',
};

/** Sized by font-size: 1em means the icon scales with its surrounding text. */
export const Sizes = () => (
  <div style={canvas}>
    <CopyIcon style={{ fontSize: '16px' }} />
    <CopyIcon style={{ fontSize: '24px' }} />
    <CopyIcon style={{ fontSize: '32px' }} />
    <CopyIcon style={{ fontSize: '48px' }} />
  </div>
);

/** Drawn in currentColor: the icon takes the tone of whatever it sits in. */
export const Tones = () => (
  <div style={{ ...canvas, fontSize: '32px' }}>
    <span style={{ color: 'var(--text)', display: 'inline-flex' }}><CopyIcon /></span>
    <span style={{ color: 'var(--muted)', display: 'inline-flex' }}><CopyIcon /></span>
    <span style={{ color: 'var(--accent)', display: 'inline-flex' }}><CopyIcon /></span>
    <span style={{ color: 'var(--danger)', display: 'inline-flex' }}><CopyIcon /></span>
  </div>
);
