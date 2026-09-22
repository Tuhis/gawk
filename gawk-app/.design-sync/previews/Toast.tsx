import { Toast } from 'gawk-app';

// Toast is the transient bottom-centre pill every surface flashes. It is
// `position: absolute` (bottom 5.5rem, centred), so it anchors to the nearest
// positioned ancestor — each cell supplies one, painted as a stand-in for the
// stream the pill floats over (its glass blur is invisible on flat black).
// The parent owns the timing: render it while the message shows, unmount after.
const stage: React.CSSProperties = {
  position: 'relative',
  height: '180px',
  background:
    'radial-gradient(circle at 25% 30%, #2b3a6b 0%, transparent 55%),' +
    'radial-gradient(circle at 75% 70%, #6b2b45 0%, transparent 55%),' +
    'var(--bg)',
  color: 'var(--text)',
  fontFamily: 'system-ui, -apple-system, "Segoe UI", Roboto, sans-serif',
  overflow: 'hidden',
};

/** The copy-confirmation flash after "Copy join link". */
export const LinkCopied = () => (
  <div style={stage}>
    <Toast>Link copied</Toast>
  </div>
);

/** A room event notice — long text truncates with an ellipsis at 480px. */
export const RoomNotice = () => (
  <div style={stage}>
    <Toast>mika&apos;s stream was removed from the room</Toast>
  </div>
);
