// R11 (docs/16): the broadcast Web Worker entry point — a thin shell around
// BroadcastWorkerCore. All pipeline logic lives in the (DOM-free, unit-tested)
// core; this file only bridges `postMessage` to it and owns the two things
// that are genuinely worker-scoped: the capability handshake and the
// track-transfer probe target.
//
// Vite bundles this via `new Worker(new URL('./broadcaster.worker.ts', ...))`.

import {
  BroadcastWorkerCore,
  type BroadcastWorkerCommand,
  type BroadcastWorkerOutbound,
} from './broadcast-worker-core';

// The DedicatedWorkerGlobalScope, typed minimally so we don't have to pull the
// "WebWorker" lib in alongside "DOM" (they clash on globals like `self`).
interface WorkerScope {
  postMessage(message: BroadcastWorkerOutbound, transfer?: Transferable[]): void;
  onmessage: ((e: MessageEvent) => void) | null;
}
const ctx = self as unknown as WorkerScope;

// Sent by the main thread before 'start' to prove MediaStreamTrack transfer
// works end-to-end (a dummy canvas.captureStream track). Receiving it needs
// no reply — a non-transferable type throws on the *sender*.
type ProbeCommand = { type: 'probeTrack'; track: MediaStreamTrack };

// Boot handshake: report capability before the main thread commits to the
// worker path. MSTP is required here (the transferred track is pumped in this
// scope); OffscreenCanvas backs the preprocessor's scaling rungs.
const supported =
  typeof VideoEncoder !== 'undefined' &&
  typeof WebTransport !== 'undefined' &&
  typeof MediaStreamTrackProcessor !== 'undefined' &&
  typeof OffscreenCanvas !== 'undefined';
ctx.postMessage({ type: 'boot', supported });

const core = new BroadcastWorkerCore({ post: (ev) => ctx.postMessage(ev) });

ctx.onmessage = (e: MessageEvent) => {
  const cmd = e.data as BroadcastWorkerCommand | ProbeCommand;
  switch (cmd.type) {
    case 'probeTrack':
      cmd.track.stop();
      break;
    case 'start':
      core.start(cmd);
      break;
    case 'capture':
      core.capture(cmd.track, cmd.nativeFps);
      break;
    case 'captureFailed':
      core.captureFailed(cmd.message);
      break;
    case 'setLadder':
      core.setLadder(cmd.selection, cmd.framerate);
      break;
    case 'setEncoderSettings':
      core.setEncoderSettings(cmd.settings);
      break;
    case 'stop':
      void core.stop();
      break;
  }
};
