import { forwardRef } from 'react';
import styles from './PreviewPanel.module.css';

export const DecodedPreview = forwardRef<HTMLCanvasElement>((_props, ref) => {
  return (
    <div className={styles.panel}>
      <div className={styles.title}>Decoded (WebCodecs → Canvas 2D)</div>
      <div className={styles.canvasFrame}>
        <canvas ref={ref} className={styles.canvasFill} width={1920} height={1080} />
      </div>
    </div>
  );
});
DecodedPreview.displayName = 'DecodedPreview';
