import { useCallback, useEffect, useRef, useState } from 'react';
import styles from './stream.module.css';
import { ServerSettings } from './ServerSettings';
import { StatsGrid } from './StatsGrid';
import { fmt } from '../../lib/format';
import { DecodedPreview } from '../loopback/components/DecodedPreview';
import { ViewerPipeline, type ViewerStats } from '../../transport/viewer';
import { useTransportStore } from '../../state/transportStore';
import { log } from '../../lib/logger';

type Status = 'idle' | 'connecting' | 'watching' | 'stopping' | 'error';

export function ViewPage() {
  const canvasRef = useRef<HTMLCanvasElement | null>(null);
  const canvasCtxRef = useRef<CanvasRenderingContext2D | null>(null);
  const pipelineRef = useRef<ViewerPipeline | null>(null);
  const [status, setStatus] = useState<Status>('idle');
  const [stats, setStats] = useState<ViewerStats | null>(null);
  const [codec, setCodec] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (canvasRef.current && !canvasCtxRef.current) {
      canvasCtxRef.current = canvasRef.current.getContext('2d');
    }
  });

  const handleStart = useCallback(async () => {
    if (pipelineRef.current) return;
    const { serverUrl, certHashHex } = useTransportStore.getState();
    setError(null);
    setStats(null);
    setCodec(null);
    setStatus('connecting');
    const pipeline = new ViewerPipeline(serverUrl, { certHashHex }, {
      onDecodedFrame: ({ frame }) => {
        const ctx = canvasCtxRef.current;
        const canvas = canvasRef.current;
        if (ctx && canvas) {
          // Same aspect-sync dance as the loopback page — see the bug note
          // there about Chrome and canvas intrinsic aspect ratios.
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
      onConfig: (config) => {
        setCodec(config.codec);
        setStatus('watching');
      },
      onStats: (s) => setStats(s),
      onError: (err) => {
        setError(err.message);
        setStatus('error');
      },
      onEnded: () => {
        pipelineRef.current = null;
        setStatus((prev) => (prev === 'error' ? prev : 'idle'));
      },
    });
    pipelineRef.current = pipeline;
    try {
      await pipeline.start();
      // Connected; the first picture appears once the relay primes us.
      setStatus((prev) => (prev === 'connecting' ? 'watching' : prev));
    } catch (e) {
      const err = e instanceof Error ? e : new Error(String(e));
      log.error(err);
      setError(err.message);
      setStatus('error');
      pipelineRef.current = null;
    }
  }, []);

  const handleStop = useCallback(async () => {
    if (!pipelineRef.current) return;
    setStatus('stopping');
    await pipelineRef.current.stop();
  }, []);

  useEffect(() => {
    return () => {
      void pipelineRef.current?.stop();
    };
  }, []);

  const running = status === 'connecting' || status === 'watching';

  return (
    <div className={styles.page}>
      <header className={styles.header}>
        <h1>gawk — view</h1>
        <p>
          Subscribe to the relay, reassemble datagrams into encoded frames, decode
          via WebCodecs, and paint to a canvas.
        </p>
      </header>

      <ServerSettings disabled={running} />

      <div className={styles.controls}>
        {!running ? (
          <button onClick={handleStart} disabled={status === 'stopping'}>
            Start Watching
          </button>
        ) : (
          <button className="danger" onClick={handleStop}>
            Stop
          </button>
        )}
        <span className={styles.statusPill}>{status}</span>
      </div>

      {error && status === 'error' && <div className={styles.error}>Error: {error}</div>}

      <div className={styles.single}>
        <DecodedPreview ref={canvasRef} />
      </div>

      <StatsGrid
        items={[
          ['Codec', codec ?? '—'],
          ['Decoder fps', fmt(stats?.decoderFps ?? NaN)],
          ['Decoded frames', String(stats?.decodedFrames ?? '—')],
          ['Frames completed', String(stats?.framesCompleted ?? '—')],
          ['Dropped (incomplete)', String(stats?.framesDroppedIncomplete ?? '—')],
          ['Dropped (late)', String(stats?.framesDroppedLate ?? '—')],
          ['Awaiting keyframe', String(stats?.framesDiscardedAwaitingKey ?? '—')],
          ['Datagrams received', String(stats?.datagramsReceived ?? '—')],
          ['Bad datagrams', String(stats?.badDatagrams ?? '—')],
          ['Configs applied', String(stats?.configsApplied ?? '—')],
          ['Decoder queue', String(stats?.decoderQueueDepth ?? '—')],
          ['Decode latency', `${fmt(stats?.lastDecodeLatencyMs ?? NaN)} ms`],
        ]}
      />
    </div>
  );
}
