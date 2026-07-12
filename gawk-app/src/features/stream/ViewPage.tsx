import { useCallback, useEffect, useRef, useState } from 'react';
import styles from './stream.module.css';
import { ServerSettings } from './ServerSettings';
import { StatsGrid } from './StatsGrid';
import { fmt } from '../../lib/format';
import { DecodedPreview } from '../loopback/components/DecodedPreview';
import { type ViewerStats } from '../../transport/viewer';
import { ViewerSession, RECONNECT_MAX_ATTEMPTS } from '../../transport/viewer-session';
import { useTransportStore } from '../../state/transportStore';
import { log } from '../../lib/logger';

type Status = 'idle' | 'connecting' | 'watching' | 'reconnecting' | 'stopping' | 'error' | 'ended';

const BROADCAST_ID_ALPHABET = '23456789ABCDEFGHJKMNPQRSTUVWXYZ';

function getBroadcastIdFromHash(): string | null {
  const hash = window.location.hash;
  const match = hash.match(/^#\/view\/([a-zA-Z0-9]+)$/);
  if (!match) return null;
  const id = match[1].toUpperCase();
  if (id.length !== 6) return null;
  for (let i = 0; i < id.length; i++) {
    if (BROADCAST_ID_ALPHABET.indexOf(id[i]) === -1) return null;
  }
  return id;
}

function validateBroadcastId(id: string): boolean {
  if (id.length !== 6) return false;
  for (let i = 0; i < id.length; i++) {
    if (BROADCAST_ID_ALPHABET.indexOf(id[i].toUpperCase()) === -1) return false;
  }
  return true;
}

export function ViewPage() {
  const canvasRef = useRef<HTMLCanvasElement | null>(null);
  const canvasCtxRef = useRef<CanvasRenderingContext2D | null>(null);
  const sessionRef = useRef<ViewerSession | null>(null);
  const [broadcastId, setBroadcastId] = useState(() => getBroadcastIdFromHash() || '');
  const [status, setStatus] = useState<Status>('idle');
  const [stats, setStats] = useState<ViewerStats | null>(null);
  const [codec, setCodec] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [retryNote, setRetryNote] = useState<string | null>(null);

  useEffect(() => {
    if (canvasRef.current && !canvasCtxRef.current) {
      canvasCtxRef.current = canvasRef.current.getContext('2d');
    }
  });

  const handleStart = useCallback(async (overrideId?: string) => {
    if (sessionRef.current) return;
    const id = overrideId || broadcastId;
    if (!validateBroadcastId(id)) return;
    const normalizedId = id.toUpperCase();
    setBroadcastId(normalizedId);
    window.location.hash = `#/view/${normalizedId}`;

    const { serverUrl, certHashHex } = useTransportStore.getState();
    setError(null);
    setStats(null);
    setCodec(null);
    setRetryNote(null);
    setStatus('connecting');
    const session = new ViewerSession(serverUrl, normalizedId, { certHashHex }, {
      onDecodedFrame: ({ frame }) => {
        const ctx = canvasCtxRef.current;
        const canvas = canvasRef.current;
        if (ctx && canvas) {
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
      },
      onStats: (s) => setStats(s),
      onConnected: () => {
        setRetryNote(null);
        setStatus('watching');
      },
      onReconnecting: ({ attempt, reason }) => {
        setStatus('reconnecting');
        setRetryNote(`Connection lost — retrying (attempt ${attempt}/${RECONNECT_MAX_ATTEMPTS}): ${reason}`);
      },
      onError: (err) => {
        setError(err.message);
        setStatus('error');
        void sessionRef.current?.stop();
      },
      onEnded: () => {
        sessionRef.current = null;
        setRetryNote(null);
        setStatus((prev) => {
          if (prev === 'watching' || prev === 'reconnecting') {
            return 'ended';
          }
          return prev === 'error' ? 'error' : 'idle';
        });
      },
    });
    sessionRef.current = session;
    try {
      await session.start();
    } catch (e) {
      const err = e instanceof Error ? e : new Error(String(e));
      log.error(err);
      setError(err.message);
      setStatus('error');
      sessionRef.current = null;
    }
  }, [broadcastId]);

  const handleStop = useCallback(async () => {
    if (!sessionRef.current) return;
    setStatus('stopping');
    await sessionRef.current.stop();
  }, []);

  // Auto-join on mount if code is in hash
  useEffect(() => {
    const id = getBroadcastIdFromHash();
    if (id && status === 'idle' && !sessionRef.current) {
      void handleStart(id);
    }
  }, [handleStart]);

  // Listen to hashchange to auto-join new ID
  useEffect(() => {
    const onHashChange = () => {
      const id = getBroadcastIdFromHash();
      if (id) {
        setBroadcastId(id);
        if (status === 'idle' && !sessionRef.current) {
          void handleStart(id);
        }
      }
    };
    window.addEventListener('hashchange', onHashChange);
    return () => window.removeEventListener('hashchange', onHashChange);
  }, [status, handleStart]);

  useEffect(() => {
    return () => {
      void sessionRef.current?.stop();
    };
  }, []);

  const running = status === 'connecting' || status === 'watching' || status === 'reconnecting';

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
          <div className={styles.joinForm}>
            <input
              type="text"
              placeholder="Enter 6-char code"
              value={broadcastId}
              onChange={(e) => {
                setBroadcastId(e.target.value.toUpperCase());
                if (status === 'ended' || status === 'error') {
                  setStatus('idle');
                }
              }}
              disabled={status === 'stopping'}
              maxLength={6}
              className={styles.codeInput}
            />
            <button
              onClick={() => handleStart()}
              disabled={status === 'stopping' || !validateBroadcastId(broadcastId)}
            >
              Watch
            </button>
          </div>
        ) : (
          <button className="danger" onClick={handleStop}>
            Stop
          </button>
        )}
        <span className={styles.statusPill}>{status}</span>
      </div>

      {error && status === 'error' && <div className={styles.error}>Error: {error}</div>}
      {status === 'ended' && <div className={styles.notice}>Broadcast has ended.</div>}
      {retryNote && status === 'reconnecting' && <div className={styles.notice}>{retryNote}</div>}

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
