import { AttachSecretPrompt } from 'gawk-app';

// AttachSecretPrompt is the secret a gated static room asks for before it will
// carry a broadcaster's stream: a scrim + centred GlassPanel dialog, both
// `position: absolute; inset: 0`. The cell supplies the positioned, sized
// stage the room screen provides in the app.
const stage: React.CSSProperties = {
  position: 'relative',
  height: '380px',
  background:
    'radial-gradient(circle at 25% 30%, #2b3a6b 0%, transparent 55%),' +
    'radial-gradient(circle at 75% 70%, #6b2b45 0%, transparent 55%),' +
    'var(--bg)',
  color: 'var(--text)',
  fontFamily: 'system-ui, -apple-system, "Segoe UI", Roboto, sans-serif',
  overflow: 'hidden',
};

const noop = () => {};

/** The dialog as it opens: a password field, Attach disabled until something is typed. */
export const Default = () => (
  <div style={stage}>
    <AttachSecretPrompt onSubmit={noop} onCancel={noop} />
  </div>
);
