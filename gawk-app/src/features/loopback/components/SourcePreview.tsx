import { useEffect, useRef } from 'react';
import styles from './PreviewPanel.module.css';
import { log } from '../../../lib/logger';

interface Props {
  stream: MediaStream | null;
}

export function SourcePreview({ stream }: Props) {
  const ref = useRef<HTMLVideoElement | null>(null);

  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    el.srcObject = stream;
    const applyAspect = () => {
      if (el.videoWidth > 0 && el.videoHeight > 0) {
        log.info(`Source <video>: videoWidth=${el.videoWidth}, videoHeight=${el.videoHeight}`);
        el.style.aspectRatio = `${el.videoWidth} / ${el.videoHeight}`;
      }
    };
    el.addEventListener('loadedmetadata', applyAspect);
    el.addEventListener('resize', applyAspect);
    if (stream) void el.play().catch(() => { /* autoplay races on mount */ });
    return () => {
      el.removeEventListener('loadedmetadata', applyAspect);
      el.removeEventListener('resize', applyAspect);
    };
  }, [stream]);

  return (
    <div className={styles.panel}>
      <div className={styles.title}>Source (raw MediaStream)</div>
      <video ref={ref} className={styles.surface} muted playsInline />
    </div>
  );
}
