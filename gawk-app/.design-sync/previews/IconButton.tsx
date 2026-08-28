import {
  IconButton, GlassPanel,
  GearIcon, CloseIcon, SpeakerIcon, SpeakerMutedIcon,
  FullscreenIcon, MoreIcon, StatsIcon, LeaveIcon,
} from 'gawk-app';

// IconButton is deliberately quiet: .iconBtn paints itself var(--muted) and
// only lifts to var(--text) on hover. It reads correctly in the place it
// actually lives — a glass control bar floating over the stream — so the cells
// stage it there rather than on flat black.
const stage: React.CSSProperties = {
  background:
    'radial-gradient(circle at 30% 35%, #24314e 0%, transparent 60%),' +
    'radial-gradient(circle at 75% 65%, #3c2338 0%, transparent 60%),' +
    'var(--bg)',
  color: 'var(--text)',
  fontFamily: 'system-ui, -apple-system, "Segoe UI", Roboto, sans-serif',
  minHeight: '180px',
  padding: '32px',
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'center',
};

const bar: React.CSSProperties = {
  padding: '6px 10px',
  display: 'inline-flex',
  gap: '2px',
  alignItems: 'center',
};

/** The viewer's control bar — icon-only buttons, each with a required label. */
export const ControlBar = () => (
  <div style={stage}>
    <GlassPanel style={bar}>
      <IconButton label="Mute"><SpeakerIcon /></IconButton>
      <IconButton label="Stats"><StatsIcon /></IconButton>
      <IconButton label="Playback settings"><GearIcon /></IconButton>
      <IconButton label="Fullscreen"><FullscreenIcon /></IconButton>
      <IconButton label="More"><MoreIcon /></IconButton>
      <IconButton label="Leave broadcast"><LeaveIcon /></IconButton>
    </GlassPanel>
  </div>
);

/** Muted: the icon changes, and the accessible name changes with it. */
export const Muted = () => (
  <div style={stage}>
    <GlassPanel style={bar}>
      <IconButton label="Unmute"><SpeakerMutedIcon /></IconButton>
      <IconButton label="Stats"><StatsIcon /></IconButton>
      <IconButton label="Playback settings"><GearIcon /></IconButton>
    </GlassPanel>
  </div>
);

/** Disabled drops to 40% — an action this surface cannot offer. */
export const Disabled = () => (
  <div style={stage}>
    <GlassPanel style={bar}>
      <IconButton label="Fullscreen unavailable" disabled><FullscreenIcon /></IconButton>
      <IconButton label="Stats unavailable" disabled><StatsIcon /></IconButton>
      <IconButton label="Close"><CloseIcon /></IconButton>
    </GlassPanel>
  </div>
);
