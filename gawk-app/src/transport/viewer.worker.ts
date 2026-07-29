// R8 S6: the Web Worker entry point — a thin shell around ViewerWorkerCore.
// All the pipeline/reconnect logic lives in the (DOM-free, unit-tested) core;
// this file only bridges `postMessage` to it and owns the two things that are
// genuinely worker-scoped: the capability handshake and the OffscreenCanvas.
//
// Vite bundles this via `new Worker(new URL('./viewer.worker.ts', ...))`.

import { notePlayhead } from './av-sync';
import { AAC_CODEC, AacTranscoder } from './audio-transcode';
import type { DecodedAudioChunk } from './audio-decode';
import { Fmp4Muxer, type AudioMuxCodec, type Fmp4Segment } from './fmp4-muxer';
import { setInterpolationEnabled } from './interpolation';
import { getPlayoutMode, setPlayoutMode, setViewerDeliveryMode } from './playout';
import { setStripeMode } from './stripe';
import { createRenderSink, type RenderSink } from './render-sink';
import type { ReleasedFrame } from './reorder-buffer';
import {
  ViewerWorkerCore,
  type AudioTapEvent,
  type ViewerWorkerCommand,
  type ViewerWorkerEvent,
  type ViewerWorkerOutbound,
} from './viewer-worker-core';
import type { ViewerTransportFactory } from './viewer-transport';
import { WorkerViewerTransport } from './worker-viewer-transport';

// The DedicatedWorkerGlobalScope, typed minimally so we don't have to pull the
// "WebWorker" lib in alongside "DOM" (they clash on globals like `self`).
interface WorkerScope {
  postMessage(message: ViewerWorkerOutbound, transfer?: Transferable[]): void;
  onmessage: ((e: MessageEvent) => void) | null;
}
const ctx = self as unknown as WorkerScope;

// Boot handshake: report capability *before* the main thread transfers the
// canvas, so an unsupported worker can be discarded without detaching the
// canvas from the main-thread fallback path. Buffered until main attaches its
// handler.
const supported = typeof VideoDecoder !== 'undefined' && typeof WebTransport !== 'undefined';
ctx.postMessage({ type: 'boot', supported });

// R10 P3: run the WebTransport read loops in a *nested* transport worker so
// decode/render work here can never starve the incoming-datagram queue. One
// nested worker per pipeline attempt (spawned on connect, reaped on close).
// Where nested workers don't exist, the pipeline keeps its in-process
// transport — same behavior as before the split.
const transportFactory: ViewerTransportFactory | undefined =
  typeof Worker === 'function'
    ? (url, opts) =>
        new WorkerViewerTransport(
          () => new Worker(new URL('./transport.worker.ts', import.meta.url), { type: 'module' }),
          url,
          opts,
        )
    : undefined;

let core: ViewerWorkerCore | null = null;
let sink: RenderSink | null = null;
// R22 (docs/27 Decision 3): the fMP4 muxer lives here at the host level,
// beside the sink — it survives pipeline attempts/reconnects, so the segment
// stream continues across a reconnect without re-arming. Created on 'arm'
// (gated devices only); until then the frame tap is a null check per frame.
let muxRequested = false;
// Which audio encapsulation the main thread negotiated (docs/27 findings 2 + 4):
// 'opus' muxes the R15 lane verbatim, 'aac' transcodes the decoded PCM for iOS.
let muxAudio: AudioMuxCodec | null = null;
let muxer: Fmp4Muxer | null = null;
let transcoder: AacTranscoder | null = null;

// The encoded-frame fork (upstream of the decoder — the whole reason MSE
// renders where the R16 presented-frame tee was black, docs/27). Segments
// post with their buffers transferred; the muxer allocates exact-size
// buffers, so no copies happen here.
const postSegments = (segments: Fmp4Segment[]): void => {
  for (const seg of segments) {
    const data = seg.data.buffer as ArrayBuffer;
    if (seg.kind === 'init') {
      ctx.postMessage(
        { type: 'muxSegment', kind: 'init', track: seg.track, mime: seg.mime, data },
        [data],
      );
    } else {
      ctx.postMessage(
        { type: 'muxSegment', kind: 'media', track: seg.track, keyframe: seg.keyframe, data },
        [data],
      );
    }
  }
};

const frameTap = (frame: ReleasedFrame): void => {
  const m = muxer;
  if (!m) return;
  postSegments(m.push(frame));
};

// R22 audio (docs/27 finding 2): the encoded-audio fork. Installed only when
// the main thread's probe said this device accepts Opus in MP4 — a refusal keeps
// the native player video-only rather than muxing a track nothing can decode.
// Forked at arrival (not at a playout gate): both tracks carry broadcaster-clock
// timestamps mapped through one shared offset, so *when* a packet is appended
// doesn't affect where it plays — and audio arriving ahead of the paced video
// release is exactly the cushion the audio SourceBuffer wants.
const audioTap = (ev: AudioTapEvent): void => {
  const m = muxer;
  // The encoded lane only feeds the muxer where the runtime takes Opus in MP4.
  // On the AAC path the encoded Opus is of no use to the presentation — the
  // transcoded PCM is (see audioPcmTap).
  if (!m || muxAudio !== 'opus') return;
  postSegments(ev.kind === 'config' ? m.setAudioConfig(ev.config) : m.pushAudio(ev.packet));
};

// R22 audio, iOS path (docs/27 finding 4): decoded PCM → AAC → the audio track.
// The transcoder lives here beside the muxer so it survives pipeline attempts;
// its own init segment rides the first output's AudioSpecificConfig.
const audioPcmTap = (chunk: DecodedAudioChunk): void => {
  const m = muxer;
  if (!m || muxAudio !== 'aac') return;
  transcoder ??= new AacTranscoder((out) => {
    if (out.description) {
      postSegments(
        m.setAudioConfig({
          codec: AAC_CODEC,
          sampleRate: chunk.sampleRate,
          channels: chunk.channels.length,
          description: out.description,
        }),
      );
    }
    postSegments(m.pushAudio({ timestampUs: BigInt(Math.round(out.timestampUs)), data: out.data }));
  });
  transcoder.push(chunk);
};

// R22: the muxer's counters ride the existing stats events (only when the mux
// fork was requested — non-gated stats are byte-identical).
const post = (ev: ViewerWorkerEvent, transfer?: Transferable[]): void => {
  if (ev.type === 'stats' && muxRequested) {
    ctx.postMessage({
      ...ev,
      stats: {
        ...ev.stats,
        presentationMux: {
          armed: muxer !== null,
          audioTranscode: transcoder?.getStats().state ?? (muxAudio === 'aac' ? 'idle' : null),
          audioTranscodeDetail: transcoder?.getStats().detail ?? null,
          ...(muxer?.getStats() ?? {
            initSegments: 0,
            mediaSegments: 0,
            skippedAwaitingInit: 0,
            errors: 0,
            audioInitSegments: 0,
            audioSegments: 0,
            audioSkipped: 0,
            audioHoles: 0,
          }),
        },
      },
    });
  } else {
    // R15: audio chunks arrive with their channel buffers in the transfer
    // list; everything else posts as before.
    ctx.postMessage(ev, transfer);
  }
};

ctx.onmessage = (e: MessageEvent) => {
  const cmd = e.data as ViewerWorkerCommand;
  switch (cmd.type) {
    case 'init': {
      // WebGL (2D fallback) wrapped in the paced presentation sink — R10 P1
      // semantics by default, display-slot pacing in adaptive mode (R12).
      // R22 (gated devices only): install the frame tap so the pipeline
      // forks released frames to the (idle-until-armed) muxer.
      muxRequested = Boolean(cmd.presentationMux);
      sink = createRenderSink(cmd.canvas);
      core = new ViewerWorkerCore({
        post,
        renderSink: sink,
        transportFactory,
        ...(muxRequested ? { frameTap, audioTap, audioPcmTap } : {}),
      });
      break;
    }
    case 'arm': {
      // Idempotent: one muxer per worker, ever — a repeat arm (or one after a
      // reconnect) must not restart the segment timeline. `audio` is the main
      // thread's Opus-in-MP4 verdict; a later arm may only ever turn it on
      // (audio can start after the first arm — the config is a 1 Hz message).
      muxAudio = muxAudio ?? cmd.audio ?? null;
      if (!muxRequested || muxer) break;
      muxer = new Fmp4Muxer();
      break;
    }
    case 'start':
      core?.start(cmd);
      break;
    case 'stop':
      void core?.stop();
      break;
    case 'playout':
      // Worker-context module state; the live pipeline reads it on every
      // advance/decode (R5 Q3 + R12 T2). Valid before/after init and start
      // alike. Leaving adaptive mode presents the newest held frame now
      // instead of letting it wait out a schedule that no longer applies.
      setPlayoutMode(cmd.mode);
      if (cmd.mode !== 'adaptive') sink?.flush?.(true);
      break;
    case 'interpolation':
      // R12 T4: read live by the paced sink on every tick.
      setInterpolationEnabled(cmd.enabled);
      break;
    case 'audioPlayhead':
      // R15 N5: module state in this worker's context, read live by the
      // pipeline on every decoded frame (same pattern as playout/resilient).
      notePlayhead({ heardUs: cmd.heardUs, atEpochMs: cmd.atEpochMs });
      break;
    case 'resilient':
      // R19: module state for the resilient reorder/playout profile. The
      // controller sends it before 'start', so the profile is active from
      // the session's first frame. Turning it off can drop the effective
      // mode out of adaptive — present any held frame now, like 'playout'.
      setViewerDeliveryMode(cmd.mode);
      if (getPlayoutMode() !== 'adaptive') sink?.flush?.(true);
      break;
    case 'stripeMode':
      // R30: module state in this worker's context, read live by the stripe
      // controller at every decide() (same pattern as playout/resilient —
      // but a live flip, never a reconnect: engagement is in-band).
      setStripeMode(cmd.mode);
      break;
  }
};
