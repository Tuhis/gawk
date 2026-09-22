import { useMemo } from 'react';
import { OwnPreviewTile, IconButton, StatsIcon, StopIcon, CloseIcon } from 'gawk-app';

// OwnPreviewTile is the web broadcaster's own tile in a room: it paints the
// LOCAL capture preview (a MediaStream) rather than subscribing to itself, and
// carries the accent inset ring plus the "You" glass bar of broadcast controls.
// In the app `preview` is the getDisplayMedia stream; here it is a canvas
// painted as a stand-in for a game and captured with captureStream().
const canvas: React.CSSProperties = {
  background: 'var(--bg)',
  color: 'var(--text)',
  fontFamily: 'system-ui, -apple-system, "Segoe UI", Roboto, sans-serif',
  padding: '24px',
};

function useFakeCapture(): MediaStream {
  return useMemo(() => {
    const c = document.createElement('canvas');
    c.width = 960;
    c.height = 540;
    const g = c.getContext('2d')!;
    const draw = () => {
      const grad = g.createLinearGradient(0, 0, 960, 540);
      grad.addColorStop(0, '#1d2c5e');
      grad.addColorStop(0.55, '#3b2458');
      grad.addColorStop(1, '#6b2b45');
      g.fillStyle = grad;
      g.fillRect(0, 0, 960, 540);
      g.fillStyle = 'rgba(255,255,255,0.08)';
      for (let x = 0; x < 960; x += 60) g.fillRect(x, 380, 30, 160);
      g.fillStyle = 'rgba(255,255,255,0.85)';
      g.font = '600 44px system-ui, sans-serif';
      g.textAlign = 'right';
      g.fillText('Stage 3 — 02:41', 912, 88);
    };
    draw();
    const stream = c.captureStream(15);
    // captureStream only emits a frame when the canvas is painted; repaint so
    // the <video> always has a current frame to show.
    setInterval(draw, 100);
    return stream;
  }, []);
}

const own = { broadcastId: 'K7QMXP', label: 'tuhis', live: true, viewerCount: 12 };
const noop = () => {};

const controls = (
  <>
    <span style={{ fontSize: 'var(--fs-sm)', fontWeight: 600 }}>You</span>
    <IconButton label="Stats" onClick={noop}>
      <StatsIcon />
    </IconButton>
    <IconButton label="Stop broadcast" onClick={noop}>
      <StopIcon />
    </IconButton>
    <IconButton label="Detach from room" onClick={noop}>
      <CloseIcon />
    </IconButton>
  </>
);

/** The broadcaster's own grid tile: local preview, accent ring, "· you" label and the glass control bar. */
export const Live = () => {
  const preview = useFakeCapture();
  return (
    <div style={canvas}>
      <div style={{ width: '480px', height: '270px', display: 'grid' }}>
        <OwnPreviewTile
          attachment={own}
          index={1}
          variant="grid"
          preview={preview}
          ownControls={controls}
          showChrome
          onFocus={noop}
        />
      </div>
    </div>
  );
};

/** While the broadcaster's session re-dials, the preview keeps painting under a reconnecting note. */
export const Reconnecting = () => {
  const preview = useFakeCapture();
  return (
    <div style={canvas}>
      <div style={{ width: '480px', height: '270px', display: 'grid' }}>
        <OwnPreviewTile
          attachment={{ ...own, live: false }}
          index={1}
          variant="grid"
          preview={preview}
          ownControls={controls}
          showChrome
          onFocus={noop}
        />
      </div>
    </div>
  );
};
