import { NicknamePrompt } from 'gawk-app';

// NicknamePrompt is a scrim + centred GlassPanel dialog. Both layers are
// `position: absolute; inset: 0`, so they fill the nearest positioned ancestor
// — in the app that is the room screen; here each cell supplies a sized stage
// painted as a stand-in for the room's video behind the scrim.
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

/** First join: asks before dialing, with a guest fallback. Join stays disabled until the field has a name. */
export const FirstJoin = () => (
  <div style={stage}>
    <NicknamePrompt onSubmit={noop} onSkip={noop} />
  </div>
);

/** Editing from the roster: the remembered name is pre-filled, and Cancel replaces the guest option. */
export const Editing = () => (
  <div style={stage}>
    <NicknamePrompt initial="tuhis" editing onSubmit={noop} onCancel={noop} />
  </div>
);
