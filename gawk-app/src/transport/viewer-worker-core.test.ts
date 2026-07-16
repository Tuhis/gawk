// R8 S6 acceptance: ViewerWorkerCore is unit-testable synchronously with a fake
// host + fake render sink — no real Worker, OffscreenCanvas, or DOM. Two
// angles: (1) an injected fake session factory pins the event mapping and the
// generation guard deterministically; (2) one integration pass with the real
// ViewerSession/ViewerPipeline (connection + decoder mocked) proves decoded
// frames reach the render sink and NEVER cross back as host events.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

const connectWebTransport = vi.fn();
const readDatagrams = vi.fn();
const readKeyframeStreams = vi.fn();
vi.mock('./connection', () => ({
  connectWebTransport: (...a: unknown[]) => connectWebTransport(...a),
  readDatagrams: (...a: unknown[]) => readDatagrams(...a),
  readKeyframeStreams: (...a: unknown[]) => readKeyframeStreams(...a),
}));

const decodeSpy = vi.fn();
function makeFakeFrame() {
  return { displayWidth: 2, displayHeight: 2, close: vi.fn() } as unknown as VideoFrame;
}
vi.mock('../media/decoder', () => ({
  Decoder: class {
    queueSize = 0;
    isHardwareAccelerated = false;
    private onDecoded: (d: unknown) => void;
    constructor(cbs: { onDecoded: (d: unknown) => void }) {
      this.onDecoded = cbs.onDecoded;
    }
    configure = vi.fn(() => Promise.resolve());
    decode = (chunk: unknown) => {
      decodeSpy(chunk);
      // Emit a decoded frame synchronously so the pipeline hands it to the sink.
      this.onDecoded({ frame: makeFakeFrame(), captureTimestampUs: 0, decodeStartMs: 0, decodeEndMs: 1 });
    };
    close = vi.fn(() => Promise.resolve());
  },
}));

import {
  ViewerWorkerCore,
  type ViewerSessionFactory,
  type ViewerSessionLike,
  type ViewerWorkerEvent,
  type WorkerHost,
} from './viewer-worker-core';
import type { RenderSink } from './render-sink';
import type { ViewerSessionCallbacks } from './viewer-session';
import { encodeDecoderConfig, encodeVideoChunk } from './wire';

function fakeSink() {
  const drawn: VideoFrame[] = [];
  const sink: RenderSink = {
    kind: '2d',
    draw: (frame) => {
      drawn.push(frame);
      frame.close();
    },
    drawnFrames: () => drawn.length,
  };
  return { sink, drawn };
}

function fakeHost(sink: RenderSink) {
  const events: ViewerWorkerEvent[] = [];
  const host: WorkerHost = { post: (ev) => events.push(ev), renderSink: sink };
  return { host, events };
}

async function flush(): Promise<void> {
  await new Promise((r) => setTimeout(r, 0));
}

afterEach(() => {
  vi.unstubAllGlobals();
  vi.clearAllMocks();
});

// ---- Group 1: deterministic mapping via an injected fake session ----------

interface FakeSession extends ViewerSessionLike {
  cbs: ViewerSessionCallbacks;
  sink: RenderSink;
  startCalls: number;
  stopCalls: number;
  startResult: Promise<void>;
}

function fakeFactory() {
  const sessions: FakeSession[] = [];
  let nextStart: () => Promise<void> = () => Promise.resolve();
  const factory: ViewerSessionFactory = (_url, _id, _opts, cbs, sink) => {
    const s: FakeSession = {
      cbs,
      sink,
      startCalls: 0,
      stopCalls: 0,
      startResult: Promise.resolve(),
      start() {
        this.startCalls++;
        this.startResult = nextStart();
        return this.startResult;
      },
      stop() {
        this.stopCalls++;
        return Promise.resolve();
      },
    };
    sessions.push(s);
    return s;
  };
  return { factory, sessions, setNextStart: (fn: () => Promise<void>) => (nextStart = fn) };
}

const startParams = { serverUrl: 'https://relay.test:4433', broadcastId: 'K7XQ2M', connectOpts: {} };

describe('ViewerWorkerCore mapping (fake session)', () => {
  it('forwards the injected render sink to the session and starts it', () => {
    const { sink } = fakeSink();
    const { host } = fakeHost(sink);
    const { factory, sessions } = fakeFactory();
    new ViewerWorkerCore(host, factory).start(startParams);
    expect(sessions).toHaveLength(1);
    expect(sessions[0].sink).toBe(sink);
    expect(sessions[0].startCalls).toBe(1);
  });

  it('maps session callbacks to host events', () => {
    const { sink } = fakeSink();
    const { host, events } = fakeHost(sink);
    const { factory, sessions } = fakeFactory();
    new ViewerWorkerCore(host, factory).start(startParams);
    const { cbs } = sessions[0];

    cbs.onConnected();
    cbs.onConfig({ codec: 'vp8', extradata: new Uint8Array() });
    cbs.onStats({ decoderFps: 30 } as never);
    cbs.onReconnecting({ attempt: 2, delayMs: 2000, reason: 'lost' });

    expect(events).toEqual([
      { type: 'connected' },
      { type: 'codec', codec: 'vp8' },
      { type: 'stats', stats: { decoderFps: 30 } },
      { type: 'reconnecting', attempt: 2, reason: 'lost' },
    ]);
  });

  it('marks a fatal error and stops the session', () => {
    const { sink } = fakeSink();
    const { host, events } = fakeHost(sink);
    const { factory, sessions } = fakeFactory();
    new ViewerWorkerCore(host, factory).start(startParams);
    const err = Object.assign(new Error('bad codec'), { fatal: true });
    sessions[0].cbs.onError(err);
    expect(events).toContainEqual({ type: 'error', message: 'bad codec', fatal: true, kind: 'unplayable' });
    expect(sessions[0].stopCalls).toBe(1);
  });

  it('marks a non-fatal session error as a lost stream', () => {
    const { sink } = fakeSink();
    const { host, events } = fakeHost(sink);
    const { factory, sessions } = fakeFactory();
    new ViewerWorkerCore(host, factory).start(startParams);
    sessions[0].cbs.onError(new Error('reconnect failed after 10 attempts: gone'));
    expect(events).toContainEqual({
      type: 'error',
      message: 'reconnect failed after 10 attempts: gone',
      fatal: false,
      kind: 'lost',
    });
  });

  it('emits ended for the current session', () => {
    const { sink } = fakeSink();
    const { host, events } = fakeHost(sink);
    const { factory, sessions } = fakeFactory();
    new ViewerWorkerCore(host, factory).start(startParams);
    sessions[0].cbs.onEnded();
    expect(events).toContainEqual({ type: 'ended' });
  });

  it('ignores a superseded session’s late callbacks (generation guard)', () => {
    const { sink } = fakeSink();
    const { host, events } = fakeHost(sink);
    const { factory, sessions } = fakeFactory();
    const core = new ViewerWorkerCore(host, factory);
    core.start(startParams);
    const first = sessions[0];

    core.start({ ...startParams, broadcastId: 'AB2CD3' }); // supersede
    expect(first.stopCalls).toBe(1);
    const second = sessions[1];

    // The old session's async teardown finally fires onEnded — must be ignored.
    first.cbs.onEnded();
    first.cbs.onConnected();
    expect(events).toEqual([]);

    // The live session still maps.
    second.cbs.onConnected();
    expect(events).toEqual([{ type: 'connected' }]);
  });

  it('surfaces a first-connect failure as an error (no onEnded fires)', async () => {
    const { sink } = fakeSink();
    const { host, events } = fakeHost(sink);
    const { factory, setNextStart } = fakeFactory();
    setNextStart(() => Promise.reject(Object.assign(new Error('unreachable'), { fatal: false })));
    new ViewerWorkerCore(host, factory).start(startParams);
    await flush();
    expect(events).toContainEqual({ type: 'error', message: 'unreachable', fatal: false, kind: 'unreachable' });
  });
});

// ---- Group 2: integration — frames hit the sink, never the host -----------

describe('ViewerWorkerCore integration (real pipeline, mocked I/O)', () => {
  beforeEach(() => {
    vi.stubGlobal('window', globalThis);
    vi.stubGlobal(
      'EncodedVideoChunk',
      class {
        init: unknown;
        constructor(init: unknown) {
          this.init = init;
        }
      },
    );
    connectWebTransport.mockReset();
    readDatagrams.mockReset();
    readKeyframeStreams.mockReset();
    decodeSpy.mockReset();
    readKeyframeStreams.mockReturnValue(new Promise(() => {}));
  });

  it('renders decoded frames to the sink and posts no frame to the host', async () => {
    const wt = { closed: new Promise(() => {}), close: vi.fn() } as unknown as WebTransport;
    connectWebTransport.mockResolvedValue(wt);
    let deliver: ((d: Uint8Array) => void) | null = null;
    readDatagrams.mockImplementation((_wt: unknown, onDatagram: (d: Uint8Array) => void) => {
      deliver = onDatagram;
      return new Promise(() => {});
    });

    const { sink, drawn } = fakeSink();
    const { host, events } = fakeHost(sink);
    const core = new ViewerWorkerCore(host);
    core.start(startParams);
    await flush();

    expect(connectWebTransport).toHaveBeenCalledWith(
      'https://relay.test:4433/subscribe/K7XQ2M',
      {},
    );
    const push = deliver as unknown as (d: Uint8Array) => void;
    push(encodeDecoderConfig({ codec: 'vp8', extradata: new Uint8Array(0) }));
    push(
      encodeVideoChunk(
        { keyframe: true, frameId: 0, chunkIndex: 0, chunkCount: 1, timestampUs: 0n },
        new Uint8Array([1, 2, 3]),
      ),
    );
    await flush();

    expect(drawn.length).toBeGreaterThanOrEqual(1);
    expect(drawn[0].close).toHaveBeenCalled();
    // Crux: no host event carries a VideoFrame.
    expect(events.every((e) => !('frame' in (e as object)))).toBe(true);
    expect(events).toContainEqual({ type: 'connected' });
    expect(events.some((e) => e.type === 'codec')).toBe(true);

    await core.stop();
  });
});
