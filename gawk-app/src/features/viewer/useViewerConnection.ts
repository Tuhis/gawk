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
  type ViewerEndReason,
  type ViewerErrorKind,
} from '../../transport/viewer-session';
import { CLOSE_CODE_SERVER_DRAINING } from '../../transport/wire';
import { setInterpolationEnabled as setLocalInterpolation } from '../../transport/interpolation';
import { setStripeMode as setLocalStripeMode, type StripeMode } from '../../transport/stripe';
import {
  setPlayoutMode as setLocalPlayoutMode,
  setViewerDeliveryMode as setLocalViewerDeliveryMode,
} from '../../transport/playout';
import type { ViewerDeliveryMode } from '../../transport/resilient';
import {
  type PlayoutMode,
} from '../../transport/playout';
import type { ViewerStats } from '../../transport/viewer';
import type { MuxSegmentEvent, ViewerWorkerEvent } from '../../transport/viewer-worker-core';
import {
  probeMseAudio,
  probeMsePresentation,
  type MseAudioProbe,
  type MseProbeResult,
} from './msePresentation';
import { WorkerViewerController } from './workerViewerController';
import { AudioSink, audioSinkSupported, type AudioOutput } from './audioSink';
import { AudioRateController, notePlayhead, resetAvSync } from '../../transport/av-sync';
import { timeOriginMs } from '../../transport/time-sync';
import {
  audioProfileForDeliveryMode,
  type AudioBufferProfile,
} from '../../transport/audio-buffer';
import { resolvedUrlIsDefault, useTransportStore } from '../../state/transportStore';
import { useTelemetryCollector } from '../../lib/useTelemetry';
import { readVisibility } from '../../lib/visibility';
import { log } from '../../lib/logger';

export type ViewerStatus = 'connecting' | 'watching' | 'reconnecting' | 'ended' | 'error';

// R22 (docs/27): the MSE presentation surface for the screen. `probe` is null
// until the capability verdict is known — on the worker path that is the
// first codec event (the mime needs the negotiated codec); the main-thread
// pipeline fallback is verdict-false immediately, tier-3-only by design
// (docs/27 Decision 11: the muxer lives in the worker, and iPhone is
// confirmed on the worker path). Segments do not pass through React state —
// they arrive up to once per frame, so the screen registers a sink callback
// and the hook routes each muxSegment event straight into it.
export interface PresentationState {
  probe: MseProbeResult | null;
  // R22 audio: the Opus-in-MP4 verdict for this stream's audio lane. Null until
  // audio is observed (a video-only broadcast never probes); false-supported
  // keeps the native player video-only with the inline AudioWorklet unchanged.
  audioProbe: MseAudioProbe | null;
  arm: () => void;
  setSegmentSink: (cb: ((seg: MuxSegmentEvent) => void) | null) => void;
}

// R15 (docs/20 Decision 9): the audio surface for the screen. `present` is
// false until audio is actually observed in the stream — every piece of audio
// UI hangs off it, so a video-only broadcast renders exactly today's viewer.
// `needsGesture` drives the tap-to-unmute affordance (autoplay policy).
export interface AudioState {
  present: boolean;
  muted: boolean;
  volume: number;
  needsGesture: boolean;
  setMuted: (muted: boolean) => void;
  setVolume: (volume: number) => void;
  resume: () => void;
  // R22 audio (docs/27 finding 2): silence the inline AudioWorklet sink WITHOUT
  // touching the persisted mute preference — used while the native fullscreen
  // player is presenting the muxed audio track, so the same audio isn't heard
  // twice from two independently-clocked outputs.
  setSuppressed: (suppressed: boolean) => void;
}

// R42 (docs/44 §4.7): what a room tile needs that the single viewer does not.
// Read through refs — none of these re-run the session effect.
export interface ViewerConnectionOptions {
  // The room's shared AudioContext + master gain. Absent ⇒ the sink owns its
  // own context, exactly as before R42.
  audioOutput?: AudioOutput;
  // 'session': mute/volume are NOT read from or written to the persisted
  // gawk:muted / gawk:volume keys — a tile's level is the room's business
  // (gawk:room-volume:<id>, roomPrefs.ts) and the broadcaster's own tile
  // starts muted without that becoming everyone's default.
  audioPrefs?: 'persist' | 'session';
  initialMuted?: boolean;
  initialVolume?: number;
  // The room's HMAC'd key (hex), stamped on this session's telemetry batches
  // (docs/44 §4.10) — never the code.
  roomKey?: string | null;
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
  // R39: 'moderated' when close code 4006 ended the broadcast (docs/42 §4.4).
  // Only meaningful while `status` is 'ended'.
  endReason: ViewerEndReason;
  retryNote: string | null;
  // R28: the id this viewer reports under, once a hello has enabled collection
  // — the join key between what the operator sees on the telemetry dashboard
  // and the person on the other end of the call. Null on a fleet that collects
  // nothing (which, from here, is indistinguishable from a relay predating
  // R28 — see lib/telemetry.ts).
  telemetrySessionId: string | null;
  presentation: PresentationState;
  audio: AudioState;
}

// R15 (docs/20 Decision 9): mute/volume persist per browser, filling the slot
// R6 reserved. Default: unmuted at full volume — the toggle is the
// broadcaster's experimental opt-in, not the viewer's.
const MUTED_KEY = 'gawk:muted';
const VOLUME_KEY = 'gawk:volume';

function loadMuted(): boolean {
  try {
    return localStorage.getItem(MUTED_KEY) === '1';
  } catch {
    return false;
  }
}

function loadVolume(): number {
  try {
    const raw = localStorage.getItem(VOLUME_KEY);
    if (raw === null) return 1;
    const v = Number(raw);
    return Number.isFinite(v) ? Math.max(0, Math.min(1, v)) : 1;
  } catch {
    return 1;
  }
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
  // R22: request the encoded-frame mux fork. True only on gated (element-
  // fullscreen-less) devices — false keeps every worker message byte-
  // identical (docs/27, carrying R16 Decision 1 forward).
  presentationMux = false,
  // R19 (docs/24 Decision 9): resilient mode. Toggling it re-runs the
  // session effect — a deliberate teardown + reconnect with (or without)
  // ?delivery=reliable; the wider reorder/playout profile is applied to the
  // pipeline's context before the session starts.
  deliveryMode = 'live' as ViewerDeliveryMode,
  // R29 (docs/34 §5.2): an opt-DOWN from the fleet parity default. Undefined
  // means "take what the fleet serves". Like deliveryMode it is negotiated at
  // subscribe time, so a change re-runs the session effect as a deliberate
  // reconnect.
  parityLevel?: 0 | 1,
  // R30 (docs/35 §5.5): the stripe mode. Deliberately NOT in the session
  // effect's deps — engagement is in-band (leg dials + the 0x10 level
  // protocol), so this is a live flip like interpolation, never a reconnect.
  stripeMode: StripeMode = 'auto',
  options: ViewerConnectionOptions = {},
): ViewerConnectionState {
  const persistAudio = options.audioPrefs !== 'session';
  const audioOutputRef = useRef(options.audioOutput ?? null);
  audioOutputRef.current = options.audioOutput ?? null;
  // The relay address is a settings value exactly like the delivery mode above:
  // read through a SUBSCRIPTION, not getState(), so changing it re-runs the
  // session effect as a deliberate teardown + reconnect. Read imperatively it
  // would only take effect on some later accidental reconnect, which is worse
  // than not applying at all — the viewer would look connected to a relay it is
  // not talking to. Only the dev-build UI can change it (the broadcaster's
  // settings panel and, since this change, the viewer's menu).
  const serverUrl = useTransportStore((s) => s.serverUrl);
  const certHashHex = useTransportStore((s) => s.certHashHex);
  const [status, setStatus] = useState<ViewerStatus>('connecting');
  const [stats, setStats] = useState<ViewerStats | null>(null);
  const [codec, setCodec] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [errorKind, setErrorKind] = useState<ViewerErrorKind | null>(null);
  const [errorFatal, setErrorFatal] = useState(false);
  // R39 (docs/42 §4.4): why the session ended, so the end card can say
  // "a moderator ended this" instead of the generic "the stream is over".
  const [endReason, setEndReason] = useState<ViewerEndReason>('normal');
  const [retryNote, setRetryNote] = useState<string | null>(null);
  // R22: the MSE capability verdict, computed on the main thread from the
  // negotiated codec (the codec event re-computes it, so a mid-view codec
  // change — a broadcaster restart with a different encoder — updates the
  // gate). Session-long otherwise.
  const [mseProbe, setMseProbe] = useState<MseProbeResult | null>(null);
  // R22 audio: probed once, from the first stats tick that reports an audio
  // codec (the lane's config). A ref mirrors it so the per-tick check stays a
  // single null test on a video-only stream.
  const [mseAudioProbe, setMseAudioProbe] = useState<MseAudioProbe | null>(null);
  const mseAudioProbedRef = useRef(false);
  // Flips true if a would-be worker reports (at boot, before any canvas
  // transfer) that it lacks the codecs/transport — then we use the main thread.
  const [workerUnsupported, setWorkerUnsupported] = useState(false);
  const useWorker = canUseWorker && !workerUnsupported;
  const presentationMuxRef = useRef(presentationMux);
  presentationMuxRef.current = presentationMux;
  // R22: where the screen's MsePresenter receives segments. A ref — segments
  // arrive up to once per frame and must never drive renders.
  const segmentSinkRef = useRef<((seg: MuxSegmentEvent) => void) | null>(null);

  // R15 (docs/20 Decisions 7-9): the audio sink lives here — one per hook
  // instance, created lazily on the first audio chunk so a video-only stream
  // never constructs an AudioContext.
  const [audioPresent, setAudioPresent] = useState(false);
  const [audioNeedsGesture, setAudioNeedsGesture] = useState(false);
  const [muted, setMutedState] = useState(() =>
    persistAudio ? loadMuted() : (options.initialMuted ?? false),
  );
  const [volume, setVolumeState] = useState(() =>
    persistAudio ? loadVolume() : Math.max(0, Math.min(1, options.initialVolume ?? 1)),
  );
  const sinkRef = useRef<AudioSink | null>(null);
  // One-shot latch for the per-packet hot path (see handleAudioChunk).
  const audioStartedRef = useRef(false);
  // Video-master drift trim (docs/20 Decision 10 revised): alignment is fixed
  // at playback start, so this is the only lever left. Lives here because it
  // drives the sink, and is fed the skew the pipeline measures.
  const rateControllerRef = useRef(new AudioRateController());
  // Read live by the sink's jitter buffer: each delivery mode widens the audio
  // envelope to match its video buffer, so the fallback depth floor is sized
  // for the mode (docs/20 Decision 12; docs/26 for the Deep-buffer floor).
  const deliveryModeRef = useRef(deliveryMode);
  deliveryModeRef.current = deliveryMode;
  const mutedRef = useRef(muted);
  mutedRef.current = muted;
  // R22 audio: an override, not a preference — the sink is muted while either
  // the user's toggle or the native-fullscreen handoff says so.
  const suppressedRef = useRef(false);
  const volumeRef = useRef(volume);
  volumeRef.current = volume;

  const useWorkerRef = useRef(useWorker);
  useWorkerRef.current = useWorker;

  const ensureSink = useCallback((): AudioSink | null => {
    if (sinkRef.current) return sinkRef.current;
    if (!audioSinkSupported()) return null;
    // Three-valued: Deep buffer needs a floor at DVR_BUFFER_MS, not the
    // resilient 500 ms — and a plain truthy check on the (now three-valued)
    // mode always returned the resilient profile, even for live-edge (docs/26
    // A/V field finding).
    const profile = (): AudioBufferProfile =>
      audioProfileForDeliveryMode(deliveryModeRef.current);
    const sink = new AudioSink(
      {
        // R15 N5 (docs/20 Decision 10): the ~4 Hz playhead report reaches
        // whichever context runs the pipeline — into the worker as a
        // command, or into this context's module state on the fallback path.
        // The sink stamps the pair itself: with getOutputTimestamp() the
        // moment a sample is at the listener is part of the measurement, not
        // "whenever this callback ran" (docs/20 field finding 13).
        onPlayhead: ({ heardUs, atEpochMs }) => {
          if (useWorkerRef.current) {
            controllerRef.current?.sendAudioPlayhead(heardUs, atEpochMs);
          } else {
            notePlayhead({ heardUs, atEpochMs });
          }
        },
      },
      profile,
      audioOutputRef.current ? { output: audioOutputRef.current } : {},
    );
    sink.setMuted(mutedRef.current || suppressedRef.current);
    sink.setVolume(volumeRef.current);
    sinkRef.current = sink;
    return sink;
  }, []);

  // Handles one decoded chunk from either pipeline path. The sink is started
  // on the first chunk (its sample rate configures the context) and the
  // audio UI appears reactively from that moment.
  const handleAudioChunk = useCallback(
    (chunk: { timestampUs: number; sampleRate: number; channels: Float32Array[]; frameCount: number }) => {
      const sink = ensureSink();
      if (!sink) return;
      // This runs 50×/s (one Opus packet per 20 ms), so everything that only
      // needs to happen once is hoisted behind the started latch — start() is
      // idempotent internally, but attaching a fresh promise pair and calling
      // two state setters per packet is churn on the hot path.
      if (!audioStartedRef.current) {
        audioStartedRef.current = true;
        setAudioPresent(true);
        void sink.start(chunk.sampleRate).then(
          () => setAudioNeedsGesture(sink.needsGesture),
          () => {
            // Context/worklet failed — video plays on; no audio UI offered.
            setAudioPresent(false);
          },
        );
      }
      sink.push(chunk);
    },
    [ensureSink],
  );

  const handleAudioReset = useCallback(() => {
    sinkRef.current?.flush();
    // A new timeline gets a fresh alignment *and* a fresh trim: the old rate
    // was correcting drift that no longer exists.
    rateControllerRef.current.reset();
    // On the main-thread path the A/V mapping lives in this context; the
    // worker path resets its own inside the pipeline.
    if (!useWorkerRef.current) resetAvSync();
  }, []);

  // The sink outlives individual sessions (reconnects re-anchor it via
  // onAudioReset) but not the hook.
  useEffect(() => {
    return () => {
      sinkRef.current?.dispose();
      sinkRef.current = null;
    };
  }, []);

  const applySinkMute = useCallback(() => {
    sinkRef.current?.setMuted(mutedRef.current || suppressedRef.current);
  }, []);

  // A mute toggle or a volume-slider drag is itself a user gesture — the same
  // kind the tap-to-unmute overlay spends on sink.resume(). Without this,
  // those controls only ever move the GainNode: a suspended/interrupted
  // AudioContext processes no audio regardless of gain, so "unmuting" via the
  // volume control silently did nothing while the context stayed blocked.
  const clearGestureBlock = useCallback(() => {
    const sink = sinkRef.current;
    if (!sink?.needsGesture) return;
    void sink.resume().then(() => setAudioNeedsGesture(sink.needsGesture));
  }, []);

  const setMuted = useCallback(
    (next: boolean) => {
      setMutedState(next);
      mutedRef.current = next;
      applySinkMute();
      clearGestureBlock();
      if (!persistAudio) return;
      try {
        localStorage.setItem(MUTED_KEY, next ? '1' : '0');
      } catch {
        // private mode etc. — the toggle still works for this session
      }
    },
    [applySinkMute, clearGestureBlock, persistAudio],
  );

  const setAudioSuppressed = useCallback(
    (next: boolean) => {
      suppressedRef.current = next;
      applySinkMute();
    },
    [applySinkMute],
  );

  const setVolume = useCallback(
    (next: number) => {
      const v = Math.max(0, Math.min(1, next));
      setVolumeState(v);
      sinkRef.current?.setVolume(v);
      clearGestureBlock();
      if (!persistAudio) return;
      try {
        localStorage.setItem(VOLUME_KEY, String(v));
      } catch {
        // private mode etc.
      }
    },
    [clearGestureBlock, persistAudio],
  );

  const resumeAudio = useCallback(() => {
    const sink = sinkRef.current;
    if (!sink) return;
    void sink.resume().then(() => setAudioNeedsGesture(sink.needsGesture));
  }, []);

  const resetState = useCallback(() => {
    setStatus('connecting');
    setStats(null);
    setCodec(null);
    setError(null);
    setErrorKind(null);
    setErrorFatal(false);
    setRetryNote(null);
  }, []);

  // R28 (docs/33 D13): telemetry rides the SAME event funnel both paths
  // already go through, so the worker and main-thread pipelines report
  // identically and neither one grows a collection path of its own.
  const telemetry = useTelemetryCollector<ViewerStats>('viewer');
  // R42: the room key rides every batch of a tile session, whichever of the
  // room's RoomState and the tile's 0x0D hello lands first.
  const roomKey = options.roomKey ?? null;
  useEffect(() => {
    telemetry.setRoomKey(roomKey);
  }, [telemetry, roomKey]);

  // R28: the collector's own id, mirrored into render state so the overlay can
  // show it. Deliberately NOT cleared when the session ends — a viewer reading
  // their id to an operator over a call is usually doing it *because* the
  // stream just died, and the dashboard keeps ended sessions in its own group
  // (docs/33 D14). A reconnect replaces it, because that is a new relay session
  // with a new token and therefore a new row.
  const [telemetrySessionId, setTelemetrySessionId] = useState<string | null>(null);

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
        // An event, not a sample: a 2 s grid cannot represent a reconnect, and
        // the close code is exactly what a human narrates about what went
        // wrong (docs/33 §4.3).
        telemetry.event(
          'reconnect',
          `attempt ${ev.attempt}${ev.closeCode == null ? '' : ` close ${ev.closeCode}`}: ${ev.reason}`,
        );
        // A 4002 drain is a planned relay rollout with an instant retry —
        // show calmer copy than the generic attempt counter (R17 W1).
        setRetryNote(
          ev.closeCode === CLOSE_CODE_SERVER_DRAINING
            ? 'Stream server is updating — reconnecting…'
            : `Reconnecting — attempt ${ev.attempt}/${RECONNECT_MAX_ATTEMPTS}`,
        );
        log.info('viewer reconnecting:', ev.reason);
        break;
      case 'codec':
        setCodec(ev.codec);
        // R22: the MSE capability verdict needs the negotiated codec — this
        // is where it becomes (and stays) known on the worker path. The
        // main-thread fallback's verdict is set eagerly by its own effect.
        if (presentationMuxRef.current && useWorkerRef.current) {
          setMseProbe(probeMsePresentation(ev.codec));
        }
        break;
      case 'stats': {
        // R15 N5 (docs/20 Decision 10): the audio jitter-buffer target rides
        // the same windowed arrival-jitter estimate the video playout
        // controller uses — one measurement, two consumers.
        const sink = sinkRef.current;
        let next: ViewerStats = ev.stats;
        if (sink) {
          const nowMs = performance.now();
          sink.updateTarget(ev.stats.arrivalJitterMs, nowMs);
          // Video-master (docs/20 Decision 10 revised): hand the sink the
          // video presentation schedule, rebased from the pipeline context's
          // epoch onto this thread's timeOrigin, and the drift trim derived
          // from measured skew. The schedule only matters until playback
          // starts; the trim runs for the session's life.
          const baseEpochMs = ev.stats.videoScheduleBaseEpochMs;
          sink.setVideoSchedule(
            baseEpochMs === null
              ? null
              : (timestampUs: number) => timestampUs / 1000 + baseEpochMs - timeOriginMs(),
          );
          sink.setRate(rateControllerRef.current.update(ev.stats.avSkewMs, nowMs));
          // The context can be suspended by the browser at any time (not just
          // at start), so the tap-to-unmute affordance follows the stats
          // cadence rather than the 50/s packet path.
          setAudioNeedsGesture(sink.needsGesture);
          // docs/20 field finding 6: the jitter-buffer counters live in the
          // main-thread sink, so merge them into the worker-assembled stats
          // here — the one place with both in hand — so the overlay and Copy
          // diagnostics see buffer depth / underruns / drops (the worker-side
          // stats omit them, which is why the first audio capture couldn't
          // show them).
          const b = sink.getStats();
          next = {
            ...ev.stats,
            audioBuffer: {
              bufferedMs: b.bufferedMs,
              targetMs: b.targetMs,
              alignmentHoldMs: b.alignmentHoldMs,
              underruns: b.underruns,
              gapsConcealed: b.gapsConcealed,
              gapsSkipped: b.gapsSkipped,
              lateDrops: b.lateDrops,
              overflowDrops: b.overflowDrops,
              resets: b.resets,
              outputLatencyMs: b.outputLatencyMs,
              contextSampleRate: b.contextSampleRate,
            },
          };
        }
        // R22 audio (docs/27 finding 2): the audio lane's codec only becomes
        // known once a packet has arrived, so the Opus-in-MP4 verdict lands here
        // rather than beside the video probe. A pass arms the worker's audio
        // fork; a refusal is recorded once and never retried (the codec can't
        // change without a new config, which would re-probe by identity anyway).
        if (
          presentationMuxRef.current &&
          useWorkerRef.current &&
          !mseAudioProbedRef.current &&
          next.audioCodec != null
        ) {
          mseAudioProbedRef.current = true;
          const verdict = probeMseAudio(next.audioCodec, next.audioChannels);
          setMseAudioProbe(verdict);
          if (verdict.supported && verdict.codec) {
            controllerRef.current?.armPresentationAudio(verdict.codec);
          }
        }
        // Tab visibility, merged here for the same reason audioBuffer is: the
        // pipeline runs in a worker and `document` does not exist there. A
        // hidden tab stops firing rAF, so renderedFps falls to 0 while decode
        // carries on — and only the document can say which of those happened.
        next = { ...next, ...readVisibility() };
        setStats(next);
        telemetry.sample(next);
        break;
      }
      case 'error':
        // The one place every surfaced error (worker or main-thread path)
        // passes through — the detailed message lives here in the console,
        // the card renders friendly copy keyed on `kind`.
        log.error(`viewer error (${ev.kind}):`, ev.message);
        telemetry.event(ev.fatal ? 'error-fatal' : 'error', `${ev.kind}: ${ev.message}`);
        setError(ev.message);
        setErrorKind(ev.kind);
        setErrorFatal(ev.fatal);
        setStatus('error');
        break;
      case 'ended':
        setRetryNote(null);
        setEndReason(ev.reason);
        // A clean end is the one moment the service can finalize this session
        // without waiting out an idle timeout — so it is a final flush, not
        // just an event.
        telemetry.event('ended');
        telemetry.finish();
        // A drop before we ever connected is an error, not a clean end.
        setStatus((prev) => (prev === 'connecting' || prev === 'error' ? 'error' : 'ended'));
        break;
      // R22: gated-device-only segment stream (the worker emits it only when
      // init carried the presentationMux flag and the screen armed). Straight
      // into the registered sink — never through state.
      case 'muxSegment':
        segmentSinkRef.current?.(ev);
        break;
      // R15: decoded PCM from the worker pipeline (the main-thread path
      // calls handleAudioChunk directly — no event round-trip).
      case 'audioChunk':
        handleAudioChunk(ev);
        break;
      case 'audioReset':
        handleAudioReset();
        break;
      // R28: this session's telemetry identity. Adopting it is what starts
      // collection at all; without one (an old relay, or telemetry off) the
      // collector stays inert and issues zero requests.
      case 'telemetryHello':
        telemetry.begin(ev.hello);
        // Read back from the collector rather than derived from the hello, so
        // the overlay can only ever show an id that is actually being reported
        // under (a disabled fleet carries a token and starts no session).
        setTelemetrySessionId(telemetry.sessionId);
        break;
      // R37 (docs/40 D15/D16): the relay-advertised ingest URL. Races the
      // hello on its own stream, so the collector accepts it in any order;
      // the disclosure flag flips only for a non-default resolution.
      case 'telemetryEndpoint':
        telemetry.setAdvertisedUrl(ev.url);
        // URL-keyed, not id-keyed (G3): a saved duplicate of the deployment's
        // own relay is not foreign.
        if (!resolvedUrlIsDefault()) {
          useTransportStore.getState().setForeignTelemetryActive(true);
        }
        break;
    }
  }, [handleAudioChunk, handleAudioReset, telemetry]);

  // ---- Worker path ---------------------------------------------------------
  const controllerRef = useRef<WorkerViewerController | null>(null);
  const disposeTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  // R22: the main-thread pipeline cannot host the worker muxer — tier 3 by
  // design (docs/27 Decision 11), and the verdict is known without waiting
  // for a codec event.
  useEffect(() => {
    if (presentationMux && !useWorker) {
      setMseProbe({ supported: false, mime: null, reason: 'main-thread pipeline' });
    }
  }, [presentationMux, useWorker]);

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
        { presentationMux },
      );
    }
    return () => {
      const c = controllerRef.current;
      disposeTimerRef.current = setTimeout(() => {
        c?.dispose();
        if (controllerRef.current === c) controllerRef.current = null;
        disposeTimerRef.current = null;
      }, 0);
    };
  }, [useWorker, applyEvent, canvasRef, presentationMux]);

  // Session start/stop per broadcast id — and per resilient-mode flip, which
  // is a deliberate reconnect with the delivery negotiation in the URL
  // (docs/24 Decision 9). The mode command goes first: worker messages
  // process in order, so the profile is live before the session starts.
  useEffect(() => {
    if (!useWorker) return;
    resetState();
    controllerRef.current?.setViewerDeliveryMode(deliveryMode);
    controllerRef.current?.start({
      serverUrl,
      broadcastId,
      connectOpts: {
        certHashHex,
        ...(deliveryMode !== 'live' ? { deliveryMode: 'reliable' as const } : {}),
        // Only on live edge: the carrier modes are served no parity, so
        // sending the param there would ask for something that cannot happen.
        ...(deliveryMode === 'live' && parityLevel != null ? { parityLevel } : {}),
      },
    });
    return () => {
      controllerRef.current?.stop();
    };
  }, [useWorker, broadcastId, deliveryMode, parityLevel, serverUrl, certHashHex, resetState]);

  // R5 Q3 + R12 T2: the playout mode, applied on mount and on every toggle.
  // Worker path: cross into the worker's context; main-thread path: set the
  // module state the pipeline reads directly. Runs after the
  // controller-creation effect above, so the first application always has a
  // controller.
  useEffect(() => {
    if (useWorker) controllerRef.current?.setPlayoutMode(playoutMode);
    else setLocalPlayoutMode(playoutMode);
  }, [useWorker, playoutMode]);

  // R30: the stripe mode, same live-crossing pattern (docs/35 §5.5).
  useEffect(() => {
    if (useWorker) controllerRef.current?.setStripeMode(stripeMode);
    else setLocalStripeMode(stripeMode);
  }, [useWorker, stripeMode]);

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
    resetState();
    // R19: the profile lives in this (main-thread) context here; set it
    // before the session starts so it is live from the first frame.
    setLocalViewerDeliveryMode(deliveryMode);

    const session = new ViewerSession(
      serverUrl,
      broadcastId,
      {
        certHashHex,
        ...(deliveryMode !== 'live' ? { deliveryMode: 'reliable' as const } : {}),
        ...(deliveryMode === 'live' && parityLevel != null ? { parityLevel } : {}),
      },
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
        onReconnecting: ({ attempt, reason, closeCode }) => {
          if (active) applyEvent({ type: 'reconnecting', attempt, reason, closeCode });
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
        onEnded: (reason) => {
          if (active) applyEvent({ type: 'ended', reason });
        },
        // R15: the main-thread pipeline decodes audio in place and feeds the
        // same sink — no worker crossing on this path.
        onAudioChunk: (chunk) => {
          if (active) handleAudioChunk(chunk);
        },
        onAudioReset: () => {
          if (active) handleAudioReset();
        },
        onTelemetryHello: (hello) => {
          if (active) applyEvent({ type: 'telemetryHello', hello });
        },
        onTelemetryEndpoint: (url) => {
          if (active) applyEvent({ type: 'telemetryEndpoint', url });
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
  }, [
    useWorker,
    broadcastId,
    deliveryMode,
    parityLevel,
    serverUrl,
    certHashHex,
    applyEvent,
    canvasRef,
    resetState,
    handleAudioChunk,
    handleAudioReset,
  ]);

  // R22: arm the worker muxer (screen calls this at `watching` on gated
  // devices, after a positive probe). Idempotent down the whole chain.
  const armPresentation = useCallback(() => {
    controllerRef.current?.armPresentation();
  }, []);

  const setSegmentSink = useCallback((cb: ((seg: MuxSegmentEvent) => void) | null) => {
    segmentSinkRef.current = cb;
  }, []);

  return {
    status,
    stats,
    codec,
    error,
    errorKind,
    errorFatal,
    endReason,
    retryNote,
    telemetrySessionId,
    presentation: {
      probe: mseProbe,
      audioProbe: mseAudioProbe,
      arm: armPresentation,
      setSegmentSink,
    },
    audio: {
      present: audioPresent,
      muted,
      volume,
      needsGesture: audioNeedsGesture,
      setMuted,
      setVolume,
      resume: resumeAudio,
      setSuppressed: setAudioSuppressed,
    },
  };
}
