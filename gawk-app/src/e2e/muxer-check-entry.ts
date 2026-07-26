// R22 MF1's CI proof (docs/27 Decision 10): the production fMP4 muxer's
// output must PLAY in a real Chrome MediaSource <video> — frames present and
// currentTime advances. This file is the in-page driver the e2e harness
// bundles (rolldown) and injects into headless Chrome (`run.mjs
// --muxer-check`); it is never part of the app bundle (nothing imports it).
//
// It deliberately runs the *production* pieces end to end: the committed
// Annex-B fixture → Fmp4Muxer (worker-side code) → MsePresenter (the
// main-thread append path, exercising its Chromium object-URL fallback)
// → <video>. Only the worker/postMessage crossing is elided — bytes don't
// change shape crossing it.

import { MsePresenter } from '../features/viewer/msePresentation';
import { Fmp4Muxer } from '../transport/fmp4-muxer';
import { FIXTURE_FRAMES, FIXTURE_FRAME_INTERVAL_US } from '../transport/h264-fixture';

interface MuxerCheckResult {
  mime: string | null;
  initSegments: number;
  mediaSegments: number;
  muxErrors: number;
  segmentsAppended: number;
  appendErrors: number;
  framesPresented: number;
  currentTime: number;
  videoWidth: number;
  videoHeight: number;
  videoError: string | null;
}

async function runMuxerCheck(): Promise<MuxerCheckResult> {
  const video = document.createElement('video');
  video.muted = true;
  video.playsInline = true;
  document.body.appendChild(video);

  // rVFC counts real presented frames — the same discriminator the viewer
  // surface uses (a buffered-but-black element would not fire it per frame).
  let framesPresented = 0;
  const rvfc = video as HTMLVideoElement & {
    requestVideoFrameCallback?: (cb: () => void) => number;
  };
  const onFrame = () => {
    framesPresented++;
    rvfc.requestVideoFrameCallback?.(onFrame);
  };
  rvfc.requestVideoFrameCallback?.(onFrame);

  const muxer = new Fmp4Muxer();
  const presenter = new MsePresenter();
  presenter.attach(video);

  let mime: string | null = null;
  for (let i = 0; i < FIXTURE_FRAMES.length; i++) {
    const f = FIXTURE_FRAMES[i];
    const segments = muxer.push({
      keyframe: f.keyframe,
      timestampUs: BigInt(Math.round(i * FIXTURE_FRAME_INTERVAL_US)),
      data: f.data,
      // The native-broadcaster shape: parameter sets in-band, extradata empty.
      config: f.keyframe ? { codec: 'avc1.42E01F', extradata: new Uint8Array(0) } : null,
    });
    for (const seg of segments) {
      if (seg.kind === 'init') mime = seg.mime;
      presenter.pushSegment(seg);
    }
  }

  // Play through the fixture (18 frames @ 30 fps = 600 ms of media) and give
  // the pipeline a bounded moment to present it.
  try {
    await video.play();
  } catch {
    // a play() rejection shows up as framesPresented staying 0
  }
  const deadline = Date.now() + 8000;
  while (Date.now() < deadline && (framesPresented < 10 || video.currentTime < 0.3)) {
    await new Promise((r) => setTimeout(r, 100));
  }

  const stats = presenter.getStats();
  const muxStats = muxer.getStats();
  return {
    mime,
    initSegments: muxStats.initSegments,
    mediaSegments: muxStats.mediaSegments,
    muxErrors: muxStats.errors,
    segmentsAppended: stats.segmentsAppended,
    appendErrors: stats.appendErrors,
    framesPresented,
    currentTime: video.currentTime,
    videoWidth: video.videoWidth,
    videoHeight: video.videoHeight,
    videoError: video.error ? `${video.error.code}: ${video.error.message}` : null,
  };
}

declare global {
  interface Window {
    __gawkMuxerCheck: () => Promise<MuxerCheckResult>;
  }
}

window.__gawkMuxerCheck = runMuxerCheck;
