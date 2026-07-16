// R8 S6: the viewer connection, decoupled from the cinematic UI. It runs the
// pipeline in a Web Worker (decoding + OffscreenCanvas rendering off the main
// thread) when the environment supports it, and falls back to the existing
// main-thread ViewerSession otherwise — either way exposing the same view
// state. The fallback also covers test (jsdom, no Worker/OffscreenCanvas) and
// any worker whose boot handshake reports missing WebCodecs/WebTransport.

import { useCallback, useEffect, useRef, useState, type RefObject } from 'react';
import {
  RECONNECT_MAX_ATTEMPTS,
  ViewerSession,
  type ViewerErrorKind,
} from '../../transport/viewer-session';
import { setInterpolationEnabled as setLocalInterpolation } from '../../transport/interpolation';
import { setPlayoutMode as setLocalPlayoutMode, type PlayoutMode } from '../../transport/playout';
import type { ViewerStats } from '../../transport/viewer';
import type { ViewerWorkerEvent } from '../../transport/viewer-worker-core';
import { WorkerViewerController } from './workerViewerController';
import { useTransportStore } from '../../state/transportStore';
import { log } from '../../lib/logger';

export type ViewerStatus = 'connecting' | 'watching' | 'reconnecting' | 'ended' | 'error';

// R16 (docs/21): the presentation-tee surface for the screen. `probe` is
// null until known ('not applicable' on non-gated devices, 'pending' on the
// worker path before init) — false covers both a failed worker probe and the
// main-thread pipeline fallback, which is tier-3-only by design (Decision 8).
export interface PresentationState {
  probe: boolean | null;
  track: MediaStreamTrack | null;
  arm: () => void;
}

export interface ViewerConnectionState {
  status: ViewerStatus;
  stats: ViewerStats | null;
  codec: string | null;
  error: string | null;
  // What the failure means to the user; drives the error-card copy. The raw
  // `error` message is console-only detail (users found "handshake failed"
  // meaningless).
  errorKind: ViewerErrorKind | null;
  errorFatal: boolean;
  retryNote: string | null;
  presentation: PresentationState;
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
  // R5 Q3 + R12 T2: the opt-in playout mode. Applied to whichever context
  // the pipeline runs in (worker command / main-thread module), live.
  playoutMode: PlayoutMode = 'off',
  // R12 T4: the experimental frame-interpolation toggle (only effective on a
  // WebGL2 worker sink in adaptive mode; harmless elsewhere).
  interpolation = false,
  // R16: request the presentation tee. True only on gated (element-
  // fullscreen-less) devices — false keeps every worker message byte-
  // identical to pre-R16 (docs/21 Decision 1).
  presentationTee = false,
): ViewerConnectionState {
  const [status, setStatus] = useState<ViewerStatus>('connecting');
  const [stats, setStats] = useState<ViewerStats | null>(null);
  const [codec, setCodec] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [errorKind, setErrorKind] = useState<ViewerErrorKind | null>(null);
  const [errorFatal, setErrorFatal] = useState(false);
  const [retryNote, setRetryNote] = useState<string | null>(null);
  // R16: the worker's tee-capability verdict and (post-arm) the generator's
  // track. Both are session-long — never reset by reconnects/broadcast changes.
  const [presentationProbe, setPresentationProbe] = useState<boolean | null>(null);
  const [presentationTrack, setPresentationTrack] = useState<MediaStreamTrack | null>(null);
  // Flips true if a would-be worker reports (at boot, before any canvas
  // transfer) that it lacks the codecs/transport — then we use the main thread.
  const [workerUnsupported, setWorkerUnsupported] = useState(false);
  const useWorker = canUseWorker && !workerUnsupported;

  const resetState = useCallback(() => {
    setStatus('connecting');
    setStats(null);
    setCodec(null);
    setError(null);
    setErrorKind(null);
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
        // The one place every surfaced error (worker or main-thread path)
        // passes through — the detailed message lives here in the console,
        // the card renders friendly copy keyed on `kind`.
        log.error(`viewer error (${ev.kind}):`, ev.message);
        setError(ev.message);
        setErrorKind(ev.kind);
        setErrorFatal(ev.fatal);
        setStatus('error');
        break;
      case 'ended':
        setRetryNote(null);
        // A drop before we ever connected is an error, not a clean end.
        setStatus((prev) => (prev === 'connecting' || prev === 'error' ? 'error' : 'ended'));
        break;
      // R16: gated-device-only events (the worker emits them only when init
      // carried the presentationTee flag).
      case 'presentationProbe':
        setPresentationProbe(ev.supported);
        break;
      case 'presentationTrack':
        setPresentationTrack(ev.track);
        break;
    }
  }, []);

  // ---- Worker path ---------------------------------------------------------
  const controllerRef = useRef<WorkerViewerController | null>(null);
  const disposeTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  // R16: the track is released with the controller (same deferred timing) —
  // stopping it in a plain effect cleanup would kill it for good on
  // StrictMode's synchronous cleanup→remount, since a stopped
  // MediaStreamTrack cannot restart.
  const trackRef = useRef<MediaStreamTrack | null>(null);
  trackRef.current = presentationTrack;

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
      controllerRef.current = new WorkerViewerController(
        canvasRef.current,
        {
          onEvent: applyEvent,
          onUnsupported: () => setWorkerUnsupported(true),
        },
        { presentationTee },
      );
    }
    return () => {
      const c = controllerRef.current;
      disposeTimerRef.current = setTimeout(() => {
        c?.dispose();
        if (controllerRef.current === c) controllerRef.current = null;
        disposeTimerRef.current = null;
        // Real teardown (the timer survived the StrictMode remount window):
        // the worker-side generator died with the worker; end its track too.
        trackRef.current?.stop();
        trackRef.current = null;
      }, 0);
    };
  }, [useWorker, applyEvent, canvasRef, presentationTee]);

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

  // R5 Q3 + R12 T2: the playout mode, applied on mount and on every toggle.
  // Worker path: cross into the worker's context; main-thread path: set the
  // module state the pipeline reads directly. Runs after the
  // controller-creation effect above, so the first application always has a
  // controller.
  useEffect(() => {
    if (useWorker) controllerRef.current?.setPlayoutMode(playoutMode);
    else setLocalPlayoutMode(playoutMode);
  }, [useWorker, playoutMode]);

  // R12 T4: interpolation, same live-crossing pattern.
  useEffect(() => {
    if (useWorker) controllerRef.current?.setInterpolation(interpolation);
    else setLocalInterpolation(interpolation);
  }, [useWorker, interpolation]);

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
            // Same structural mapping as the worker core: ViewerSession fires
            // onError only for a fatal pipeline verdict or an exhausted
            // reconnect budget.
            const fatal = Boolean((err as { fatal?: boolean }).fatal);
            applyEvent({
              type: 'error',
              message: err.message,
              fatal,
              kind: fatal ? 'unplayable' : 'lost',
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
      // First-connect failure is fatal by ViewerSession policy and fires no
      // callbacks — we never reached the stream.
      const err = e instanceof Error ? e : new Error(String(e));
      if (active) {
        applyEvent({ type: 'error', message: err.message, fatal: false, kind: 'unreachable' });
      }
    });

    return () => {
      active = false;
      void session.stop();
    };
  }, [useWorker, broadcastId, applyEvent, canvasRef, resetState]);

  // R16: arm the tee (screen calls this at `watching` on gated devices, after
  // a positive probe). Idempotent down the whole chain.
  const armPresentation = useCallback(() => {
    controllerRef.current?.armPresentation();
  }, []);

  return {
    status,
    stats,
    codec,
    error,
    errorKind,
    errorFatal,
    retryNote,
    presentation: {
      // The main-thread fallback pipeline can't host the worker-only
      // VideoTrackGenerator — tier 3 by design (docs/21 Decision 8), reported
      // as a failed probe so the gate detail reads correctly.
      probe: presentationTee && !useWorker ? false : presentationProbe,
      track: presentationTrack,
      arm: armPresentation,
    },
  };
}
