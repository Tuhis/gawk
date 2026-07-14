// R8 S6: the viewer connection, decoupled from the cinematic UI. It runs the
// pipeline in a Web Worker (decoding + OffscreenCanvas rendering off the main
// thread) when the environment supports it, and falls back to the existing
// main-thread ViewerSession otherwise — either way exposing the same view
// state. The fallback also covers test (jsdom, no Worker/OffscreenCanvas) and
// any worker whose boot handshake reports missing WebCodecs/WebTransport.

import { useCallback, useEffect, useRef, useState, type RefObject } from 'react';
import { RECONNECT_MAX_ATTEMPTS, ViewerSession } from '../../transport/viewer-session';
import type { ViewerStats } from '../../transport/viewer';
import type { ViewerWorkerEvent } from '../../transport/viewer-worker-core';
import { WorkerViewerController } from './workerViewerController';
import { useTransportStore } from '../../state/transportStore';
import { log } from '../../lib/logger';

export type ViewerStatus = 'connecting' | 'watching' | 'reconnecting' | 'ended' | 'error';

export interface ViewerConnectionState {
  status: ViewerStatus;
  stats: ViewerStats | null;
  codec: string | null;
  error: string | null;
  errorFatal: boolean;
  retryNote: string | null;
}

// Whether we can even attempt the worker path. In jsdom (tests) and any browser
// missing these, this is false and we use the main-thread pipeline directly.
const canUseWorker =
  typeof Worker !== 'undefined' &&
  typeof OffscreenCanvas !== 'undefined' &&
  typeof HTMLCanvasElement !== 'undefined' &&
  'transferControlToOffscreen' in HTMLCanvasElement.prototype;

export function useViewerConnection(
  broadcastId: string,
  canvasRef: RefObject<HTMLCanvasElement | null>,
): ViewerConnectionState {
  const [status, setStatus] = useState<ViewerStatus>('connecting');
  const [stats, setStats] = useState<ViewerStats | null>(null);
  const [codec, setCodec] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [errorFatal, setErrorFatal] = useState(false);
  const [retryNote, setRetryNote] = useState<string | null>(null);
  // Flips true if a would-be worker reports (at boot, before any canvas
  // transfer) that it lacks the codecs/transport — then we use the main thread.
  const [workerUnsupported, setWorkerUnsupported] = useState(false);
  const useWorker = canUseWorker && !workerUnsupported;

  const resetState = useCallback(() => {
    setStatus('connecting');
    setStats(null);
    setCodec(null);
    setError(null);
    setErrorFatal(false);
    setRetryNote(null);
  }, []);

  // Shared mapping from a worker event (or a synthesized main-thread event) to
  // view state, so both paths render identically.
  const applyEvent = useCallback((ev: ViewerWorkerEvent) => {
    switch (ev.type) {
      case 'connected':
        setRetryNote(null);
        setStatus('watching');
        break;
      case 'reconnecting':
        setStatus('reconnecting');
        setRetryNote(`Reconnecting — attempt ${ev.attempt}/${RECONNECT_MAX_ATTEMPTS}`);
        log.info('viewer reconnecting:', ev.reason);
        break;
      case 'codec':
        setCodec(ev.codec);
        break;
      case 'stats':
        setStats(ev.stats);
        break;
      case 'error':
        setError(ev.message);
        setErrorFatal(ev.fatal);
        setStatus('error');
        break;
      case 'ended':
        setRetryNote(null);
        // A drop before we ever connected is an error, not a clean end.
        setStatus((prev) => (prev === 'connecting' || prev === 'error' ? 'error' : 'ended'));
        break;
    }
  }, []);

  // ---- Worker path ---------------------------------------------------------
  const controllerRef = useRef<WorkerViewerController | null>(null);
  const disposeTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  // Controller lifetime: constructed once (it needs the mounted <canvas>), and
  // disposed on real unmount. Dispose is deferred a macrotask so StrictMode's
  // synchronous cleanup→remount reuses the same controller — the OffscreenCanvas
  // transfer inside it is one-shot and cannot be repeated.
  useEffect(() => {
    if (!useWorker) return;
    if (disposeTimerRef.current) {
      clearTimeout(disposeTimerRef.current);
      disposeTimerRef.current = null;
    }
    if (!controllerRef.current && canvasRef.current) {
      controllerRef.current = new WorkerViewerController(canvasRef.current, {
        onEvent: applyEvent,
        onUnsupported: () => setWorkerUnsupported(true),
      });
    }
    return () => {
      const c = controllerRef.current;
      disposeTimerRef.current = setTimeout(() => {
        c?.dispose();
        if (controllerRef.current === c) controllerRef.current = null;
        disposeTimerRef.current = null;
      }, 0);
    };
  }, [useWorker, applyEvent, canvasRef]);

  // Session start/stop per broadcast id (worker path).
  useEffect(() => {
    if (!useWorker) return;
    const { serverUrl, certHashHex } = useTransportStore.getState();
    resetState();
    controllerRef.current?.start({ serverUrl, broadcastId, connectOpts: { certHashHex } });
    return () => {
      controllerRef.current?.stop();
    };
  }, [useWorker, broadcastId, resetState]);

  // ---- Main-thread fallback path -------------------------------------------
  const ctxRef = useRef<CanvasRenderingContext2D | null>(null);

  useEffect(() => {
    if (useWorker) return;
    let active = true;
    const { serverUrl, certHashHex } = useTransportStore.getState();
    resetState();

    const session = new ViewerSession(
      serverUrl,
      broadcastId,
      { certHashHex },
      {
        onDecodedFrame: ({ frame }) => {
          const canvas = canvasRef.current;
          if (canvas) {
            const ctx = ctxRef.current ?? canvas.getContext('2d');
            ctxRef.current = ctx;
            if (ctx) {
              if (canvas.width !== frame.displayWidth || canvas.height !== frame.displayHeight) {
                canvas.width = frame.displayWidth;
                canvas.height = frame.displayHeight;
              }
              ctx.drawImage(frame, 0, 0, canvas.width, canvas.height);
            }
          }
          frame.close();
        },
        onConfig: (config) => {
          if (active) applyEvent({ type: 'codec', codec: config.codec });
        },
        onStats: (s) => {
          if (active) applyEvent({ type: 'stats', stats: s });
        },
        onConnected: () => {
          if (active) applyEvent({ type: 'connected' });
        },
        onReconnecting: ({ attempt, reason }) => {
          if (active) applyEvent({ type: 'reconnecting', attempt, reason });
        },
        onError: (err) => {
          if (active) {
            applyEvent({
              type: 'error',
              message: err.message,
              fatal: Boolean((err as { fatal?: boolean }).fatal),
            });
          }
          void session.stop();
        },
        onEnded: () => {
          if (active) applyEvent({ type: 'ended' });
        },
      },
    );
    session.start().catch((e) => {
      const err = e instanceof Error ? e : new Error(String(e));
      log.error(err);
      if (active) {
        setError(err.message);
        setStatus('error');
      }
    });

    return () => {
      active = false;
      void session.stop();
    };
  }, [useWorker, broadcastId, applyEvent, canvasRef, resetState]);

  return { status, stats, codec, error, errorFatal, retryNote };
}
