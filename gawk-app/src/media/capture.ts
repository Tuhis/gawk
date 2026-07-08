import type { CaptureConfig } from './types';

export type FrameHandler = (frame: VideoFrame) => void;
export type CapturePath = 'mstp' | 'video-rvfc';

export interface CaptureHandle {
  stream: MediaStream;
  track: MediaStreamTrack;
  capturePath: CapturePath;
  startFrames(onFrame: FrameHandler): Promise<void>;
  stop(): void;
}

export async function startCapture(config: CaptureConfig): Promise<CaptureHandle> {
  const stream = await navigator.mediaDevices.getDisplayMedia({
    video: {
      frameRate: { ideal: config.framerate },
      // Constrain ONLY width. Chrome scales the source to fit width and
      // preserves the source's aspect ratio for height. Constraining both
      // width and height makes Chrome pillarbox non-16:9 sources into the
      // constrained box, which is not what we want for ultrawide displays.
      width: { max: config.width },
    },
    audio: false,
  });

  const track = stream.getVideoTracks()[0];
  if (!track) throw new Error('No video track from getDisplayMedia');

  if (typeof (globalThis as unknown as { MediaStreamTrackProcessor?: unknown }).MediaStreamTrackProcessor === 'function') {
    return createMstpHandle(stream, track);
  }
  return createVideoRvfcHandle(stream, track);
}

// Preferred path: MediaStreamTrackProcessor. No DOM element, no compositor
// roundtrip, not affected by tab-visibility throttling. Chromium-only today.
function createMstpHandle(stream: MediaStream, track: MediaStreamTrack): CaptureHandle {
  const processor = new MediaStreamTrackProcessor({ track: track as MediaStreamVideoTrack });
  let reader: ReadableStreamDefaultReader<VideoFrame> | null = null;
  let stopped = false;

  return {
    stream,
    track,
    capturePath: 'mstp',
    async startFrames(onFrame) {
      reader = processor.readable.getReader();
      const pump = async () => {
        try {
          while (!stopped) {
            const { value: frame, done } = await reader!.read();
            if (done) break;
            if (!frame) continue;
            // Re-stamp on the performance.now() clock so downstream latency
            // math is uniform across capture paths. Constructor from an
            // existing VideoFrame shares the underlying buffer (no copy).
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
