import styles from './statsOverlay.module.css';
import { GlassPanel } from '../../ui/GlassPanel';
import { IconButton } from '../../ui/IconButton';
import { CloseIcon } from '../../ui/Icons';
import { formatHotkey } from '../../lib/useHotkey';
import { fmt } from '../../lib/format';
import type { ViewerStats } from '../../transport/viewer';
import { STATS_HOTKEY } from './hotkeys';

interface Props {
  stats: ViewerStats | null;
  codec: string | null;
  onClose: () => void;
}

// The lightweight viewer stats overlay (docs/10 J4) — "is it the stream or my
// machine" without leaving the sleek UI. Renders the ViewerStats the session
// already produces; if R5 later adds a glass-to-glass metric it slots in here.
export function StatsOverlay({ stats, codec, onClose }: Props) {
  const rows: Array<[string, string]> = [
    ['Codec', codec ?? '—'],
    ['Decode mode', stats?.isHardwareAccelerated === true ? 'Hardware' : stats?.isHardwareAccelerated === false ? 'Software' : '—'],
    ['Decoder fps', fmt(stats?.decoderFps ?? NaN)],
    ['Decoded', String(stats?.decodedFrames ?? '—')],
    ['Completed', String(stats?.framesCompleted ?? '—')],
    ['Dropped (incomplete)', String(stats?.framesDroppedIncomplete ?? '—')],
    ['Dropped (late)', String(stats?.framesDroppedLate ?? '—')],
    ['Awaiting keyframe', String(stats?.framesDiscardedAwaitingKey ?? '—')],
    ['Decoder queue', String(stats?.decoderQueueDepth ?? '—')],
    ['Decode latency', `${fmt(stats?.lastDecodeLatencyMs ?? NaN)} ms`],
    ['Datagrams', String(stats?.datagramsReceived ?? '—')],
    ['Bad datagrams', String(stats?.badDatagrams ?? '—')],
    ['Keyframe streams', String(stats?.keyframeStreamsReceived ?? '—')],
    ['Gap resyncs', String(stats?.reorderGapResyncs ?? '—')],
    ['Reorder buffered', String(stats?.reorderBuffered ?? '—')],
  ];

  return (
    <GlassPanel className={styles.overlay} role="dialog" aria-label="Stream stats">
      <div className={styles.head}>
        <span className={styles.title}>Stats</span>
        <IconButton label="Close stats" className={styles.close} onClick={onClose}>
          <CloseIcon />
        </IconButton>
      </div>
      <dl className={styles.grid}>
        {rows.map(([label, value]) => (
          <div key={label} className={styles.row}>
            <dt>{label}</dt>
            <dd>{value}</dd>
          </div>
        ))}
      </dl>
      <div className={styles.foot}>{formatHotkey(STATS_HOTKEY)} or right-click to toggle</div>
    </GlassPanel>
  );
}
