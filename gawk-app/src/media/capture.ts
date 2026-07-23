import { log } from '../lib/logger';
import type { CaptureConfig } from './types';

export type FrameHandler = (frame: VideoFrame) => void;
// 'mstp-worker' is the R11 path: a transferred track pumped by an MSTP
// created inside the broadcast worker (docs/16).
export type CapturePath = 'mstp' | 'video-rvfc' | 'mstp-worker';

export interface CaptureHandle {
  stream: MediaStream;
  track: MediaStreamTrack;
  capturePath: CapturePath;
  // R15 field finding: the audio-bearing grant was refused and this stream is
  // the video-only retry (see acquireDisplayStream).
  audioUnavailable: boolean;
  startFrames(onFrame: FrameHandler): Promise<void>;
  stop(): void;
}

// R11 (docs/16): what BroadcastPipeline actually needs from capture — frames,
// the native fps hint, and an "ended" signal. The main-thread default wraps
// startCapture (stream present, for the preview); the worker source wraps a
// transferred track (no stream — the preview lives on the main thread).
// R13 (docs/18 Decision 6): applyConstraints aligns the capture track with
// the sticky target — live, no restart. Optional: a source without it (test
// fakes, exotic paths) simply keeps preprocessor-only scaling.
export interface BroadcastMediaSource {
  capturePath: CapturePath;
  stream: MediaStream | null;
  nativeFps: number | null;
  onEnded(cb: () => void): void;
  startFrames(onFrame: FrameHandler): Promise<void>;
  stop(): void;
  applyConstraints?(constraints: MediaTrackConstraints): Promise<void>;
  // R15 (docs/20 Decision 6): the captured system-audio track, when the
  // toggle requested one and the browser granted it. Absent/null is the
  // graceful video-only state (toggle off, Firefox, unchecked picker box).
  audioTrack?: MediaStreamTrack | null;
  // R15 field finding: audio was requested and the browser refused to start
  // an audio source at all, so this source is the video-only retry. Distinct
  // from a plain absent audioTrack (which is a choice, not a refusal) — the
  // overlay says so, and it is the difference between "no audio shared" and
  // "this OS/browser can't".
  audioUnavailable?: boolean;
}

export type BroadcastMediaSourceFactory = (config: CaptureConfig) => Promise<BroadcastMediaSource>;

export interface DisplayStreamGrant {
  stream: MediaStream;
  track: MediaStreamTrack;
  audioUnavailable: boolean;
}

// The pre-capture half of startCapture: the actual getDisplayMedia call.
// Split out (R11) because on the worker path this must run on the main thread
// (window scope + user gesture) while the frame pump runs in the worker.
//
// R15 field finding (2026-07-19): requesting system audio is not best-effort
// in Chromium. Where the platform has no system-audio source for the chosen
// surface — Linux and macOS screen/window shares; only Windows/ChromeOS and
// tab shares have one — getDisplayMedia rejects the WHOLE request with
// NotReadableError "Could not start audio source", taking video with it. That
// made the experimental audio toggle a broadcast-killer, which docs/20
// Decision 6 forbids ("audio may annotate, never abort"). So: ask again
// without audio, and remember that we had to.
//
// 2026-07-23 (docs/20): that toggle is gone — the broadcaster always asks for
// system audio — so the refusal path needs the escape hatch the toggle used
// to be. The retry below needs its own transient activation and usually has
// none left, which left the broadcast dead with "turn the toggle off" as the
// only way out. A refusal is now remembered for the rest of the page session:
// the next start asks for video only and succeeds. Deliberately NOT persisted
// — finding 1's device-state class is transient (a woken output endpoint, a
// tab share instead of a screen), so a reload earns a fresh audio attempt.
let audioSourceRefused = false;

export async function acquireDisplayStream(config: CaptureConfig): Promise<DisplayStreamGrant> {
  if (!config.audio || audioSourceRefused) {
    return {
      ...(await requestDisplayStream(config, false)),
      // Two different silences: the caller asked for none, versus this
      // browser already proved it cannot start a source. Only the second is
      // 'unavailable' on the overlay.
      audioUnavailable: Boolean(config.audio) && audioSourceRefused,
    };
  }

  try {
    return { ...(await requestDisplayStream(config, true)), audioUnavailable: false };
  } catch (e) {
    // Only an audio-source failure earns a second picker. A cancelled or
    // denied picker (NotAllowedError) must never re-prompt — R1's lesson
    // about telling "the server said no" from "the user said no" applies to
    // capture too.
    if (!isSourceStartFailure(e)) throw e;
    // Set before the retry, so it holds whichever way the retry lands: this
    // browser cannot start an audio source, and the next start must not
    // spend its one grant finding that out again.
    audioSourceRefused = true;
    log.warn('Capture with audio was refused; retrying video-only:', e);
    try {
      return { ...(await requestDisplayStream(config, false)), audioUnavailable: true };
    } catch (retryError) {
      // The usual landing spot: getDisplayMedia needs transient activation
      // and the seconds the user spent in the picker have already spent it,
      // so the retry cannot re-prompt. Surface the original cause plus the
      // way out — the retry's activation complaint would only mislead. The
      // way out is now simply "start again": the refusal recorded above makes
      // the next attempt a video-only one.
      log.warn('Video-only retry after the audio refusal also failed:', retryError);
      throw new Error(
        `${e instanceof Error ? e.message : String(e)} — capture was requested with audio. ` +
          'Chromium can only capture system audio on Windows/ChromeOS, or from a shared tab ' +
          'with "share tab audio" checked. Start the broadcast again to continue without audio.',
        { cause: e },
      );
    }
  }
}

function isSourceStartFailure(e: unknown): boolean {
  // Chromium's name for "a source in the grant would not start"; the audio
  // one carries the message "Could not start audio source".
  return (e as { name?: unknown } | null)?.name === 'NotReadableError';
}

async function requestDisplayStream(
  config: CaptureConfig,
  audio: boolean,
): Promise<{ stream: MediaStream; track: MediaStreamTrack }> {
  // The grant is deliberately broad (docs/18 Decision 6): capture alignment
  // happens post-acquisition via track.applyConstraints on the sticky
  // target, so nothing a settings change can express exceeds this request —
  // no re-prompt, ever. The old HW-probe fps cap here is gone (Decision 10):
  // the HW-aware auto ceiling covers the default path, and explicit choices
  // are honored, not silently capped.
  //
  // R15 (docs/20 Decision 6): with the audio toggle on, request system audio
  // with processing off (game audio is program material, not voice) and keep
  // the broadcaster hearing their own game. No audio track in the grant is a
  // state, not an error — the pipeline runs video-only. systemAudio /
  // suppressLocalAudioPlayback are Chromium extensions absent from the TS
  // dom lib, hence the options cast.
  const stream = await navigator.mediaDevices.getDisplayMedia({
    video: {
      frameRate: { ideal: config.framerate },
      // Constrain ONLY width. Chrome scales the source to fit width and
      // preserves the source's aspect ratio for height. Constraining both
      // width and height makes Chrome pillarbox non-16:9 sources into the
      // constrained box, which is not what we want for ultrawide displays.
      width: { max: config.width },
    },
    ...(audio
      ? ({
          audio: {
            echoCancellation: false,
            noiseSuppression: false,
            autoGainControl: false,
          },
          systemAudio: 'include',
          suppressLocalAudioPlayback: false,
        } as DisplayMediaStreamOptions)
      : { audio: false }),
  });

  const track = stream.getVideoTracks()[0];
  if (!track) throw new Error('No video track from getDisplayMedia');
  return { stream, track };
}

export async function startCapture(config: CaptureConfig): Promise<CaptureHandle> {
  const { stream, track, audioUnavailable } = await acquireDisplayStream(config);

  if (typeof (globalThis as unknown as { MediaStreamTrackProcessor?: unknown }).MediaStreamTrackProcessor === 'function') {
    return createMstpHandle(stream, track, audioUnavailable);
  }
  return createVideoRvfcHandle(stream, track, audioUnavailable);
}

// The MSTP read loop, shared by the main-thread handle and the worker-side
// track source. Re-stamps frames on the local performance.now() clock so
// downstream latency math is uniform across capture paths (constructor from
// an existing VideoFrame shares the underlying buffer — no copy).
function createMstpPump(track: MediaStreamTrack): {
  startFrames(onFrame: FrameHandler): Promise<void>;
  stop(): void;
} {
  const processor = new MediaStreamTrackProcessor({ track: track as MediaStreamVideoTrack });
  let reader: ReadableStreamDefaultReader<VideoFrame> | null = null;
  let stopped = false;

  return {
    async startFrames(onFrame) {
      reader = processor.readable.getReader();
      const pump = async () => {
        try {
          while (!stopped) {
            const { value: frame, done } = await reader!.read();
            if (done) break;
            if (!frame) continue;
            const arrivalUs = Math.round(performance.now() * 1000);
            const rebased = new VideoFrame(frame, { timestamp: arrivalUs });
            frame.close();
            onFrame(rebased);
          }
        } catch {
          // reader may be cancelled during teardown
        }
      };
      void pump();
    },
    stop() {
      stopped = true;
      try {
        void reader?.cancel();
      } catch {
        // ignore
      }
      reader = null;
    },
  };
}

// Preferred path: MediaStreamTrackProcessor. No DOM element, no compositor
// roundtrip, not affected by tab-visibility throttling. Chromium-only today.
function createMstpHandle(
  stream: MediaStream,
  track: MediaStreamTrack,
  audioUnavailable: boolean,
): CaptureHandle {
  const pump = createMstpPump(track);
  return {
    stream,
    track,
    audioUnavailable,
    capturePath: 'mstp',
    startFrames: (onFrame) => pump.startFrames(onFrame),
    stop: () => pump.stop(),
  };
}

// R11 worker-side source: an MSTP pump around a track transferred into the
// worker. MSTP construction is deferred to startFrames so the source can be
// built (and unit-tested) in scopes without MediaStreamTrackProcessor.
// nativeFps is read from the original track on the main thread and threaded
// through — getSettings() on a transferred clone is not something we rely on.
export function trackMediaSource(
  track: MediaStreamTrack,
  nativeFps: number | null,
  // R15/N3: the transferred audio clone, alongside the video clone. The
  // source owns both clones — stop() ends them together.
  audioTrack: MediaStreamTrack | null = null,
  // R15 field finding: the main thread's grant fell back to video-only.
  audioUnavailable = false,
): BroadcastMediaSource {
  let pump: ReturnType<typeof createMstpPump> | null = null;
  return {
    capturePath: 'mstp-worker',
    stream: null,
    nativeFps,
    audioTrack,
    audioUnavailable,
    onEnded: (cb) => track.addEventListener('ended', cb),
    async startFrames(onFrame) {
      pump = createMstpPump(track);
      await pump.startFrames(onFrame);
    },
    stop() {
      pump?.stop();
      track.stop();
      audioTrack?.stop();
    },
    // R13: constraints land on the transferred clone, worker-side — clones
    // hold independent constraints, and this track is the encode source
    // (the main-thread original keeps the broad grant for the preview).
    applyConstraints: (constraints) => track.applyConstraints(constraints),
  };
}

// Fallback: hidden <video> + requestVideoFrameCallback. Works in Firefox
// (and any browser where MediaStreamTrackProcessor isn't exposed) but pays
// the compositor cost and is subject to tab-visibility throttling.
function createVideoRvfcHandle(
  stream: MediaStream,
  track: MediaStreamTrack,
  audioUnavailable: boolean,
): CaptureHandle {
  const video = document.createElement('video');
  video.srcObject = stream;
  video.muted = true;
  video.playsInline = true;

  let stopped = false;
  let rvfcId: number | undefined;

  return {
    stream,
    track,
    audioUnavailable,
    capturePath: 'video-rvfc',
    async startFrames(onFrame) {
      if (typeof video.requestVideoFrameCallback !== 'function') {
        throw new Error('requestVideoFrameCallback unsupported in this browser');
      }
      await video.play();
      const tick = (_now: number, meta: VideoFrameCallbackMetadata) => {
        if (stopped) return;
        if (video.videoWidth > 0 && video.videoHeight > 0) {
          try {
            const timestampUs = Math.round(meta.presentationTime * 1000);
            const frame = new VideoFrame(video, { timestamp: timestampUs });
            onFrame(frame);
          } catch {
            // Frame construction can fail during teardown or dimension changes.
          }
        }
        rvfcId = video.requestVideoFrameCallback(tick);
      };
      rvfcId = video.requestVideoFrameCallback(tick);
    },
    stop() {
      stopped = true;
      if (rvfcId !== undefined && typeof video.cancelVideoFrameCallback === 'function') {
        video.cancelVideoFrameCallback(rvfcId);
      }
      video.srcObject = null;
    },
  };
}

export function stopCapture(handle: CaptureHandle): void {
  handle.stop();
  for (const t of handle.stream.getTracks()) t.stop();
}
