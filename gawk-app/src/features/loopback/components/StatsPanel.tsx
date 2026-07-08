import styles from './StatsPanel.module.css';
import { usePipelineStore } from '../../../state/pipelineStore';

function fmt(n: number, digits = 1): string {
  return Number.isFinite(n) ? n.toFixed(digits) : '—';
}

export function StatsPanel() {
  const stats = usePipelineStore((s) => s.stats);
  const encoderInfo = usePipelineStore((s) => s.encoderInfo);
  const capturePath = usePipelineStore((s) => s.capturePath);

  const items: Array<[string, string]> = [
    ['Capture path', capturePath ?? '—'],
    ['Codec', encoderInfo?.codec ?? '—'],
    ['Acceleration', encoderInfo?.acceleration ?? '—'],
    ['Encoder variant', encoderInfo?.variantLabel ?? '—'],
    ['Encoder fps', fmt(stats.encoderFps)],
    ['Decoder fps', fmt(stats.decoderFps)],
    ['Encoded frames', String(stats.encodedFrames)],
    ['Decoded frames', String(stats.decodedFrames)],
    ['Dropped (source)', String(stats.droppedFrames)],
    ['Keyframes', String(stats.keyframes)],
    ['Encoder queue', String(stats.encoderQueueDepth)],
    ['Decoder queue', String(stats.decoderQueueDepth)],
    ['Encode latency', `${fmt(stats.lastEncodeLatencyMs)} ms`],
    ['Decode latency', `${fmt(stats.lastDecodeLatencyMs)} ms`],
    ['End-to-end', `${fmt(stats.lastEndToEndLatencyMs)} ms`],
  ];

  return (
    <div className={styles.panel}>
      <div className={styles.grid}>
        {items.map(([label, value]) => (
          <div key={label} className={styles.item}>
            <span className={styles.label}>{label}</span>
            <span className={styles.value}>{value}</span>
          </div>
        ))}
      </div>
    </div>
  );
}
