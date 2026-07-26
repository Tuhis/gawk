// R22 MF1's CI proof (docs/27 Decision 10): the production fMP4 muxer's
// output must PLAY in a real Chrome MediaSource <video> — frames present and
// currentTime advances. This file is the in-page driver the e2e harness
// bundles (rolldown) and injects into headless Chrome (`run.mjs
// --muxer-check`); it is never part of the app bundle (nothing imports it).
//
// It deliberately runs the *production* pieces end to end: the committed
// Annex-B fixture → Fmp4Muxer (worker-side code) → MsePresenter (the
// main-thread append path, exercising its Chromium object-URL fallback)
// → <video>.
//
// WHAT THIS DOES NOT COVER, and why (do not read a green run as more):
//   * It does not go through R16/R22's device gate at all — it imports the
//     muxer and presenter directly and never renders ViewerScreen. The gate is
//     the *absence* of Element.requestFullscreen, which Chrome has, so in the
//     production viewer under Chrome `gated` is false and NO R22 code runs (no
//     presentationMux in the worker init, no frame tap, no muxer, no hidden
//     <video>) — that is R16 Decision 1's byte-identity guarantee, and it means
//     the tier-1 viewer scenario proves nothing about this path by design.
//     So the gate is not stubbed or worked around here; it is simply not on the
//     path. What is elided with it: arm-at-watching, the worker/postMessage
//     crossing, the audio probe firing off the stats tick, useFullscreen's tier
//     selection, and the inline-sink audio handoff — all covered by jsdom tests
//     against the connection seam (ViewerScreen.mse.test.tsx et al), not here.
//   * Chrome has no ManagedMediaSource (verified: `typeof ManagedMediaSource
//     === 'undefined'`), so everything MMS-specific is unexercised — the
//     `streaming` parking that pump() honors, MMS's own buffer eviction, and the
//     srcObject-with-MediaSource wiring (Chrome takes the object-URL fallback).
//   * WebKit's pause-and-fire-`ended` on an MSE underrun is, definitionally,
//     what Chrome does NOT do — it stalls and resumes. That divergence is why
//     docs/27 finding 1 shipped with CI green, and no Chrome check can catch
//     its class of bug.
// What it does prove: the muxer emits valid fMP4 that a production MSE
// implementation accepts, demuxes and PLAYS (video + the Opus track), and the
// presenter's real append/dual-SourceBuffer/duration logic drives a real
// MediaSource + SourceBuffer correctly.

import { MsePresenter, probeMseAudio } from '../features/viewer/msePresentation';
import { AAC_CODEC, AacTranscoder, type TranscodeInput } from '../transport/audio-transcode';
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
  // Which tier the runtime picked (or was forced onto): 'opus' muxes verbatim,
  // 'aac' transcodes — the path iOS lands on.
  audioTier: 'opus' | 'aac' | null;
  audioTranscode: string | null;
  audioTranscodeDetail: string | null;
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

// One 20 ms PCM chunk of the same tone, shaped like what the viewer's Opus
// decoder hands the transcoder in production.
function pcmFor(timestampUs: number): TranscodeInput {
  const channels = [new Float32Array(OPUS_FRAME_SAMPLES), new Float32Array(OPUS_FRAME_SAMPLES)];
  for (let s = 0; s < OPUS_FRAME_SAMPLES; s++) {
    const t = timestampUs / 1_000_000 + s / OPUS_SAMPLE_RATE;
    const v = Math.sin(2 * Math.PI * 440 * t) * 0.1;
    channels[0][s] = v;
    channels[1][s] = v;
  }
  return {
    timestampUs,
    sampleRate: OPUS_SAMPLE_RATE,
    channels,
    frameCount: OPUS_FRAME_SAMPLES,
  };
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

  // R22 audio: probe first — a runtime with neither Opus nor AAC in MP4 skips the
  // audio leg instead of failing the video proof (and the skip is reported, never
  // silent). Chrome takes both, so `GAWK_MUXER_CHECK_AAC` forces the AAC tier to
  // exercise the path iOS actually lands on (docs/27 finding 4): the mp4a/esds
  // boxes and the AudioSpecificConfig the encoder hands back.
  const forceAac = new URLSearchParams(location.search).get('aac') === '1';
  const audioProbe = forceAac
    ? { supported: true, codec: 'aac' as const, mime: null, reason: 'forced' }
    : probeMseAudio('opus', 2);
  const audioPackets = audioProbe.supported ? await encodeOpusPackets(40) : [];
  let audioMime: string | null = null;
  // docs/27 finding 5: the audio SourceBuffer has to be created alongside the
  // video one — an MSE implementation may refuse a second buffer once the first
  // init segment is parsed. The mime is known from the tier; the init bytes are
  // not (the AAC path learns its AudioSpecificConfig from the encoder).
  if (audioProbe.supported) {
    presenter.setExpectedAudioMime(
      audioProbe.codec === 'aac' ? `audio/mp4; codecs="${AAC_CODEC}"` : 'audio/mp4; codecs="opus"',
    );
  }
  // The AAC tier re-encodes what the viewer decoded; here the fixture tone is
  // already PCM, so it feeds the transcoder directly — same muxer entry point.
  const transcoder =
    audioProbe.codec === 'aac'
      ? new AacTranscoder((out) => {
          if (out.description) {
            for (const seg of muxer.setAudioConfig({
              codec: AAC_CODEC,
              sampleRate: OPUS_SAMPLE_RATE,
              channels: 2,
              description: out.description,
            })) {
              if (seg.kind === 'init') audioMime = seg.mime;
              presenter.pushSegment(seg);
            }
          }
          for (const seg of muxer.pushAudio({
            timestampUs: BigInt(Math.round(out.timestampUs)),
            data: out.data,
          })) {
            presenter.pushSegment(seg);
          }
        })
      : null;
  if (!transcoder && audioPackets.length > 0) {
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
      if (transcoder) {
        transcoder.push(pcmFor(p.ts));
      } else {
        for (const seg of muxer.pushAudio({ timestampUs: BigInt(p.ts), data: p.data })) {
          presenter.pushSegment(seg);
        }
      }
    }
  }

  // End of input: the muxer holds one frame back (its duration is the interval
  // to its successor), and the fixture has a real end where the live stream
  // doesn't — so flush the held frame before asserting counts.
  for (const seg of muxer.flush()) presenter.pushSegment(seg);

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
    audioTier: audioProbe.codec,
    audioTranscode: transcoder?.getStats().state ?? null,
    audioTranscodeDetail: transcoder?.getStats().detail ?? null,
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
