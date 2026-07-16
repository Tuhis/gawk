// R8 S6: the Web Worker entry point — a thin shell around ViewerWorkerCore.
// All the pipeline/reconnect logic lives in the (DOM-free, unit-tested) core;
// this file only bridges `postMessage` to it and owns the two things that are
// genuinely worker-scoped: the capability handshake and the OffscreenCanvas.
//
// Vite bundles this via `new Worker(new URL('./viewer.worker.ts', ...))`.

import { log } from '../lib/logger';
import { setInterpolationEnabled } from './interpolation';
import { setPlayoutMode } from './playout';
import {
  PacedPresentationSink,
  createContextSink,
  createRenderSink,
  type RenderSink,
} from './render-sink';
import { TeeRenderSink, getVideoTrackGenerator, probePresentationTee } from './tee-render-sink';
import {
  ViewerWorkerCore,
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
// R16: the presentation tee + its generator live here at the host level,
// beside the sink — they survive pipeline attempts/reconnects, so the track
// keeps flowing across a reconnect without re-arming (docs/21 Decision 4).
let tee: TeeRenderSink | null = null;
let teeArmed = false;

// R16: the tee's counters ride the existing stats events (only when a tee
// exists — non-gated stats are byte-identical).
const post = (ev: ViewerWorkerEvent): void => {
  if (ev.type === 'stats' && tee) {
    ctx.postMessage({ ...ev, stats: { ...ev.stats, presentationTee: tee.teeStats() } });
  } else {
    ctx.postMessage(ev);
  }
};

ctx.onmessage = (e: MessageEvent) => {
  const cmd = e.data as ViewerWorkerCommand;
  switch (cmd.type) {
    case 'init': {
      // WebGL (2D fallback) wrapped in the paced presentation sink — R10 P1
      // semantics by default, display-slot pacing in adaptive mode (R12).
      // R16 (gated devices only): probe the tee capability, and when it holds,
      // slip the idle TeeRenderSink between the paced sink and the context
      // sink — pass-through-only until armed.
      if (cmd.presentationTee) {
        const supported = probePresentationTee();
        ctx.postMessage({ type: 'presentationProbe', supported });
        if (supported) {
          tee = new TeeRenderSink(createContextSink(cmd.canvas));
          sink = new PacedPresentationSink(tee);
        }
      }
      sink ??= createRenderSink(cmd.canvas);
      core = new ViewerWorkerCore({
        post,
        renderSink: sink,
        transportFactory,
      });
      break;
    }
    case 'arm': {
      // Idempotent: one generator/track per worker, ever — a repeat arm (or
      // one after a reconnect) must not mint a second track.
      if (!tee || teeArmed) break;
      try {
        const Generator = getVideoTrackGenerator();
        if (!Generator) break; // probe said no; arm should never arrive
        const generator = new Generator();
        tee.arm(generator.writable.getWriter());
        teeArmed = true;
        ctx.postMessage({ type: 'presentationTrack', track: generator.track }, [generator.track]);
      } catch (e) {
        log.warn('presentation tee arm failed:', e);
      }
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
  }
};
