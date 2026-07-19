import type { CaptureConfig } from './types';

export type FrameHandler = (frame: VideoFrame) => void;
// 'mstp-worker' is the R11 path: a transferred track pumped by an MSTP
// created inside the broadcast worker (docs/16).
export type CapturePath = 'mstp' | 'video-rvfc' | 'mstp-worker';

export interface CaptureHandle {
  stream: MediaStream;
  track: MediaStreamTrack;
  capturePath: CapturePath;
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
}

export type BroadcastMediaSourceFactory = (config: CaptureConfig) => Promise<BroadcastMediaSource>;

// The pre-capture half of startCapture: HW-probe fps capping + the actual
// getDisplayMedia call. Split out (R11) because on the worker path this must
// run on the main thread (window scope + user gesture) while the frame pump
// runs in the worker.
export async function acquireDisplayStream(
  config: CaptureConfig,
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
    ...(config.audio
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
  const { stream, track } = await acquireDisplayStream(config);

  if (typeof (globalThis as unknown as { MediaStreamTrackProcessor?: unknown }).MediaStreamTrackProcessor === 'function') {
    return createMstpHandle(stream, track);
  }
  return createVideoRvfcHandle(stream, track);
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
function createMstpHandle(stream: MediaStream, track: MediaStreamTrack): CaptureHandle {
  const pump = createMstpPump(track);
  return {
    stream,
    track,
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
): BroadcastMediaSource {
  let pump: ReturnType<typeof createMstpPump> | null = null;
  return {
    capturePath: 'mstp-worker',
    stream: null,
    nativeFps,
    audioTrack,
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
function createVideoRvfcHandle(stream: MediaStream, track: MediaStreamTrack): CaptureHandle {
  const video = document.createElement('video');
  video.srcObject = stream;
  video.muted = true;
  video.playsInline = true;

  let stopped = false;
  let rvfcId: number | undefined;

  return {
    stream,
    track,
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
