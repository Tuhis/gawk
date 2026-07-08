import { useCallback, useEffect, useRef, useState } from 'react';
import styles from './LoopbackPage.module.css';
import { CaptureControls } from './components/CaptureControls';
import { SourcePreview } from './components/SourcePreview';
import { DecodedPreview } from './components/DecodedPreview';
import { StatsPanel } from './components/StatsPanel';
import { LoopbackPipeline } from '../../media/loopback';
import { usePipelineStore } from '../../state/pipelineStore';
import { log } from '../../lib/logger';

export function LoopbackPage() {
  const canvasRef = useRef<HTMLCanvasElement | null>(null);
  const canvasCtxRef = useRef<CanvasRenderingContext2D | null>(null);
  const pipelineRef = useRef<LoopbackPipeline | null>(null);
  const decodedFrameCountRef = useRef(0);
  const [sourceStream, setSourceStream] = useState<MediaStream | null>(null);

  const config = usePipelineStore((s) => s.config);
  const status = usePipelineStore((s) => s.status);
  const lastError = usePipelineStore((s) => s.lastError);
  const setStatus = usePipelineStore((s) => s.setStatus);
  const setStats = usePipelineStore((s) => s.setStats);
  const setEncoderInfo = usePipelineStore((s) => s.setEncoderInfo);
  const setCapturePath = usePipelineStore((s) => s.setCapturePath);
  const setError = usePipelineStore((s) => s.setError);

  useEffect(() => {
    if (canvasRef.current && !canvasCtxRef.current) {
      canvasCtxRef.current = canvasRef.current.getContext('2d');
    }
  });

  const handleStart = useCallback(async () => {
    if (pipelineRef.current) return;
    setError(null);
    setEncoderInfo(null);
    setCapturePath(null);
    setStatus('starting');
    const pipeline = new LoopbackPipeline(config, {
      onSourceStream: (s) => {
        setSourceStream(s);
        setStatus('capturing');
      },
      onDecodedFrame: ({ frame }) => {
        const ctx = canvasCtxRef.current;
        const canvas = canvasRef.current;
        decodedFrameCountRef.current++;
        if (decodedFrameCountRef.current === 1) {
          log.info(
            `First decoded frame: display=${frame.displayWidth}x${frame.displayHeight}, coded=${frame.codedWidth}x${frame.codedHeight}`,
          );
        }
        if (ctx && canvas) {
          // Always keep the wrapper's aspect in sync with the frame — even
          // when the buffer size doesn't change (which was hiding the bug on
          // Chrome when frames happen to match the initial 1920x1080).
          const wrapper = canvas.parentElement;
          if (wrapper) {
            const targetAspect = `${frame.displayWidth} / ${frame.displayHeight}`;
            if (wrapper.style.aspectRatio !== targetAspect) {
              wrapper.style.aspectRatio = targetAspect;
            }
          }
          if (canvas.width !== frame.displayWidth || canvas.height !== frame.displayHeight) {
            canvas.width = frame.displayWidth;
            canvas.height = frame.displayHeight;
          }
          ctx.drawImage(frame, 0, 0, canvas.width, canvas.height);
        }
        frame.close();
      },
      onStats: (stats) => setStats(stats),
      onEncoderConfigured: (info) => setEncoderInfo(info),
      onCapturePathChosen: (path) => setCapturePath(path),
      onError: (err) => {
        log.error(err);
        setError(err.message);
        setStatus('error');
      },
      onEnded: () => {
        setSourceStream(null);
        pipelineRef.current = null;
        if (usePipelineStore.getState().status !== 'error') setStatus('idle');
      },
    });
    pipelineRef.current = pipeline;
    try {
      await pipeline.start();
    } catch (e) {
      const err = e instanceof Error ? e : new Error(String(e));
      log.error(err);
      setError(err.message);
      setStatus('error');
      pipelineRef.current = null;
    }
  }, [config, setError, setStats, setStatus, setEncoderInfo, setCapturePath]);

  const handleStop = useCallback(async () => {
    if (!pipelineRef.current) return;
    setStatus('stopping');
    await pipelineRef.current.stop();
  }, [setStatus]);

  useEffect(() => {
    return () => {
      void pipelineRef.current?.stop();
    };
  }, []);

  return (
    <div className={styles.page}>
      <header className={styles.header}>
        <h1>gawk — loopback test</h1>
        <p>
          Capture the screen, encode via WebCodecs, decode locally, and paint the
          result to a canvas. No network transport yet.
        </p>
      </header>

      <CaptureControls onStart={handleStart} onStop={handleStop} />

      {lastError && status === 'error' && (
        <div className={styles.error}>Error: {lastError}</div>
      )}

      <div className={styles.previews}>
        <SourcePreview stream={sourceStream} />
        <DecodedPreview ref={canvasRef} />
      </div>

      <StatsPanel />
    </div>
  );
}
