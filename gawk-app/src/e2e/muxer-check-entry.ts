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

import { MsePresenter, probeMseAudio } from '../features/viewer/msePresentation';
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
  // R22 audio (docs/27 finding 2). `audioSupported` is this Chrome's
  // Opus-in-MP4 verdict — the same question iOS answers for itself; when it is
  // false the audio leg is skipped rather than failed. `audioTrack` says the
  // presenter really created the second SourceBuffer, and `currentTime`
  // advancing WITH both tracks appended is the coupling proof: the element plays
  // only where the tracks' buffered ranges intersect, so a broken audio timeline
  // stalls this check even though the video half is perfect.
  audioSupported: boolean;
  audioPackets: number;
  audioMuxSegments: number;
  audioMuxHoles: number;
  audioSegmentsAppended: number;
  audioTrack: boolean;
  audioMime: string | null;
  audioError: string | null;
  // R22 finding 1: duration = Infinity must stick, or the native player draws a
  // finite timeline and reads a buffer underrun as end-of-media.
  liveDuration: boolean;
  // Computed in-page: JSON has no Infinity (it serializes to null), so the
  // comparison has to happen where the number still is one.
  durationIsInfinite: boolean;
}

// Real Opus packets, produced by the browser's own encoder — the R15 lane's
// shape (48 kHz stereo, 20 ms frames), so the muxer sees what it sees in
// production rather than synthetic bytes an MSE demuxer would reject.
const OPUS_FRAME_SAMPLES = 960;
const OPUS_SAMPLE_RATE = 48_000;

async function encodeOpusPackets(count: number): Promise<Array<{ ts: number; data: Uint8Array }>> {
  const packets: Array<{ ts: number; data: Uint8Array }> = [];
  const AudioEncoderCtor = (globalThis as { AudioEncoder?: typeof AudioEncoder }).AudioEncoder;
  const AudioDataCtor = (globalThis as { AudioData?: typeof AudioData }).AudioData;
  if (!AudioEncoderCtor || !AudioDataCtor) return packets;

  const encoder = new AudioEncoderCtor({
    output: (chunk) => {
      const data = new Uint8Array(chunk.byteLength);
      chunk.copyTo(data);
      packets.push({ ts: chunk.timestamp, data });
    },
    error: () => {
      /* surfaced as a short packet list */
    },
  });
  encoder.configure({
    codec: 'opus',
    sampleRate: OPUS_SAMPLE_RATE,
    numberOfChannels: 2,
    bitrate: 128_000,
  });
  for (let i = 0; i < count; i++) {
    // A quiet 440 Hz tone: silence would be legal too, but a real signal makes a
    // decoder-level rejection louder than an all-zero packet might.
    const samples = new Float32Array(OPUS_FRAME_SAMPLES * 2);
    for (let s = 0; s < OPUS_FRAME_SAMPLES; s++) {
      const t = (i * OPUS_FRAME_SAMPLES + s) / OPUS_SAMPLE_RATE;
      const v = Math.sin(2 * Math.PI * 440 * t) * 0.1;
      samples[s] = v;
      samples[OPUS_FRAME_SAMPLES + s] = v;
    }
    encoder.encode(
      new AudioDataCtor({
        format: 'f32-planar',
        sampleRate: OPUS_SAMPLE_RATE,
        numberOfFrames: OPUS_FRAME_SAMPLES,
        numberOfChannels: 2,
        timestamp: Math.round((i * OPUS_FRAME_SAMPLES * 1_000_000) / OPUS_SAMPLE_RATE),
        data: samples,
      }),
    );
  }
  await encoder.flush();
  encoder.close();
  return packets;
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

  // R22 audio: probe first — a Chrome without Opus in MP4 skips the audio leg
  // instead of failing the video proof (and the skip is reported, never silent).
  const audioProbe = probeMseAudio('opus', 2);
  const audioPackets = audioProbe.supported ? await encodeOpusPackets(40) : [];
  let audioMime: string | null = null;
  if (audioPackets.length > 0) {
    for (const seg of muxer.setAudioConfig({
      codec: 'opus',
      sampleRate: OPUS_SAMPLE_RATE,
      channels: 2,
    })) {
      if (seg.kind === 'init') audioMime = seg.mime;
      presenter.pushSegment(seg);
    }
  }

  let mime: string | null = null;
  let audioNext = 0;
  for (let i = 0; i < FIXTURE_FRAMES.length; i++) {
    const f = FIXTURE_FRAMES[i];
    const frameTsUs = Math.round(i * FIXTURE_FRAME_INTERVAL_US);
    const segments = muxer.push({
      keyframe: f.keyframe,
      timestampUs: BigInt(frameTsUs),
      data: f.data,
      // The native-broadcaster shape: parameter sets in-band, extradata empty.
      config: f.keyframe ? { codec: 'avc1.42E01F', extradata: new Uint8Array(0) } : null,
    });
    for (const seg of segments) {
      if (seg.kind === 'init' && seg.track === 'video') mime = seg.mime;
      presenter.pushSegment(seg);
    }
    // Interleave audio the way the live pipeline does: the audio lane runs ahead
    // of the paced video release, so feed every packet whose timestamp the video
    // timeline has reached, plus one frame of lead.
    while (
      audioNext < audioPackets.length &&
      audioPackets[audioNext].ts <= frameTsUs + FIXTURE_FRAME_INTERVAL_US
    ) {
      const p = audioPackets[audioNext++];
      for (const seg of muxer.pushAudio({ timestampUs: BigInt(p.ts), data: p.data })) {
        presenter.pushSegment(seg);
      }
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
    audioSupported: audioProbe.supported,
    audioPackets: audioPackets.length,
    audioMuxSegments: muxStats.audioSegments,
    audioMuxHoles: muxStats.audioHoles,
    audioSegmentsAppended: stats.audioSegmentsAppended,
    audioTrack: stats.audioTrack,
    audioMime,
    audioError: audioProbe.supported ? null : audioProbe.reason,
    liveDuration: stats.liveDuration,
    durationIsInfinite: video.duration === Infinity,
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
