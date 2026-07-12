import { useCallback, useEffect, useRef, useState } from 'react';
import styles from './stream.module.css';
import { ServerSettings } from './ServerSettings';
import { StatsGrid } from './StatsGrid';
import { fmt } from '../../lib/format';
import { SourcePreview } from '../loopback/components/SourcePreview';
import { BroadcastPipeline, type BroadcastStats } from '../../transport/broadcaster';
import type { EncoderConfigured } from '../../media/encoder';
import { DEFAULT_CAPTURE_CONFIG } from '../../media/types';
import { useTransportStore } from '../../state/transportStore';
import { log } from '../../lib/logger';

type Status = 'idle' | 'connecting' | 'broadcasting' | 'stopping' | 'error';

export function BroadcastPage() {
  const pipelineRef = useRef<BroadcastPipeline | null>(null);
  const [status, setStatus] = useState<Status>('idle');
  const [sourceStream, setSourceStream] = useState<MediaStream | null>(null);
  const [stats, setStats] = useState<BroadcastStats | null>(null);
  const [encoderInfo, setEncoderInfo] = useState<EncoderConfigured | null>(null);
  const [capturePath, setCapturePath] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [broadcastId, setBroadcastId] = useState<string | null>(null);
  const [reclaimFailedNote, setReclaimFailedNote] = useState<string | null>(null);
  const [copied, setCopied] = useState(false);

  const handleStart = useCallback(async () => {
    if (pipelineRef.current) return;
    const { serverUrl, certHashHex } = useTransportStore.getState();
    setError(null);
    setStats(null);
    setEncoderInfo(null);
    setCapturePath(null);

    let activeId = broadcastId;
    let triedReclaim = false;

    if (activeId) {
      triedReclaim = true;
      setStatus('connecting');
      const pipeline = new BroadcastPipeline(
        { ...DEFAULT_CAPTURE_CONFIG },
        serverUrl,
        { certHashHex },
        {
          onSourceStream: (s) => {
            setSourceStream(s);
            setStatus('broadcasting');
          },
          onEncoderConfigured: (info) => setEncoderInfo(info),
          onCapturePathChosen: (path) => setCapturePath(path),
          onStats: (s) => setStats(s),
          onBroadcastId: (id) => {
            setBroadcastId(id);
            setReclaimFailedNote(null);
          },
          onError: (err) => {
            setError(err.message);
            setStatus('error');
          },
          onEnded: () => {
            setSourceStream(null);
            pipelineRef.current = null;
            setStatus((prev) => (prev === 'error' ? prev : 'idle'));
          },
        },
        activeId,
      );
      pipelineRef.current = pipeline;
      try {
        await pipeline.start();
        return; // Success!
      } catch (e) {
        log.warn("Reclaim failed, falling back to mint:", e);
        pipelineRef.current = null;
        setBroadcastId(null);
        activeId = null;
      }
    }

    // Mint path
    setStatus('connecting');
    const pipeline = new BroadcastPipeline(
      { ...DEFAULT_CAPTURE_CONFIG },
      serverUrl,
      { certHashHex },
      {
        onSourceStream: (s) => {
          setSourceStream(s);
          setStatus('broadcasting');
        },
        onEncoderConfigured: (info) => setEncoderInfo(info),
        onCapturePathChosen: (path) => setCapturePath(path),
        onStats: (s) => setStats(s),
        onBroadcastId: (id) => {
          setBroadcastId(id);
          if (triedReclaim) {
            setReclaimFailedNote("Reclaim failed (expired/taken); started a new broadcast.");
          } else {
            setReclaimFailedNote(null);
          }
        },
        onError: (err) => {
          setError(err.message);
          setStatus('error');
        },
        onEnded: () => {
          setSourceStream(null);
          pipelineRef.current = null;
          setStatus((prev) => (prev === 'error' ? prev : 'idle'));
        },
      },
    );
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
  }, [broadcastId]);

  const handleStop = useCallback(async () => {
    if (!pipelineRef.current) return;
    setStatus('stopping');
    await pipelineRef.current.stop();
  }, []);

  const handleCopy = () => {
    if (!broadcastId) return;
    const joinLink = `${window.location.origin}${window.location.pathname}#/view/${broadcastId}`;
    navigator.clipboard.writeText(joinLink).then(() => {
      setCopied(true);
      setTimeout(() => setCopied(false), 2000);
    });
  };

  useEffect(() => {
    return () => {
      void pipelineRef.current?.stop();
    };
  }, []);

  const running = status === 'connecting' || status === 'broadcasting';

  return (
    <div className={styles.page}>
      <header className={styles.header}>
        <h1>gawk — broadcast</h1>
        <p>
          Capture the screen, encode via WebCodecs, and publish chunked datagrams
          to the relay&apos;s /publish endpoint.
        </p>
      </header>

      <ServerSettings disabled={running} />

      <div className={styles.controls}>
        {!running ? (
          <button onClick={handleStart} disabled={status === 'stopping'}>
            Start Broadcast
          </button>
        ) : (
          <button className="danger" onClick={handleStop} disabled={status === 'connecting'}>
            Stop
          </button>
        )}
        <span className={styles.statusPill}>{status}</span>

        {broadcastId && (
          <div className={styles.shareCode}>
            <span>Code: <code>{broadcastId}</code></span>
            <button className="secondary" onClick={handleCopy}>
              {copied ? 'Copied!' : 'Copy Link'}
            </button>
          </div>
        )}
      </div>

      {reclaimFailedNote && <div className={styles.notice}>{reclaimFailedNote}</div>}
      {error && status === 'error' && <div className={styles.error}>Error: {error}</div>}

      <SourcePreview stream={sourceStream} />

      <StatsGrid
        items={[
          ['Capture path', capturePath ?? '—'],
          ['Codec', encoderInfo?.codec ?? '—'],
          ['Acceleration', encoderInfo?.acceleration ?? '—'],
          ['Encoder fps', fmt(stats?.encoderFps ?? NaN)],
          ['Encoded frames', String(stats?.encodedFrames ?? '—')],
          ['Keyframes', String(stats?.keyframes ?? '—')],
          ['Dropped (source)', String(stats?.droppedFrames ?? '—')],
          ['Datagrams sent', String(stats?.datagramsSent ?? '—')],
          ['Sent', `${fmt((stats?.bytesSent ?? 0) / 1_000_000, 1)} MB`],
          ['Configs sent', String(stats?.configsSent ?? '—')],
          ['Encoder queue', String(stats?.encoderQueueDepth ?? '—')],
          ['Encode latency', `${fmt(stats?.lastEncodeLatencyMs ?? NaN)} ms`],
        ]}
      />
    </div>
  );
}
