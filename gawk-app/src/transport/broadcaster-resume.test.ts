// R17 W2 (docs/22 Decision 5): broadcaster auto-resume. On session death
// mid-broadcast the pipeline keeps capture + encoder alive and reconnects
// the transport only, presenting the relay-minted resume token (wire 0x09)
// on the /publish/{id} claim; on re-attach it forces the next frame to be a
// keyframe (stream + embedded config) while frameIDs continue — continuity
// is the viewer's resume-vs-restart signal (Decision 6). Mocks mirror
// broadcaster-keyframe.test.ts.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { parseStreamFrameHeader, parseDecoderConfig, STREAM_FRAME_HEADER_SIZE } from './wire';
import { CLOSE_CODE_SERVER_DRAINING } from './wire';
import { ABRUPT_DROP_RETRY_DELAY_MS } from './reconnect';

const connectWebTransport = vi.fn();
const startCapture = vi.fn();

const h = vi.hoisted(() => ({
  frameCb: { value: null as null | ((frame: unknown) => void) },
  encoderCbs: { value: null as null | { onEncoded: (e: unknown) => void; onError: (e: Error) => void } },
  encoderForcedKeyframes: { value: 0 },
  sends: [] as Uint8Array[][],
}));

vi.mock('./connection', async (importOriginal) => {
  const actual = await importOriginal<typeof import('./connection')>();
  return {
    ...actual,
    connectWebTransport: (...args: unknown[]) => connectWebTransport(...args),
    readDatagrams: () => new Promise(() => {}),
    DatagramSender: class {
      send = (datagrams: Uint8Array[]) => {
        h.sends.push(datagrams);
        return Promise.resolve();
      };
      close = vi.fn();
    },
  };
});

vi.mock('../media/capture', () => ({
  startCapture: (...args: unknown[]) => startCapture(...args),
  stopCapture: vi.fn(),
}));

vi.mock('../media/preprocess', () => ({
  FramePreprocessor: class {
    setTarget() {}
    process(frame: unknown) {
      return frame;
    }
    getStats() {
      return { gateDropped: 0, scaledFrames: 0 };
    }
  },
}));

vi.mock('../media/encoder', () => ({
  Encoder: class {
    config: { width: number; height: number; framerate: number; bitrate: number };
    constructor(
      config: { width: number; height: number; framerate: number; bitrate: number },
      cbs: { onEncoded: (e: unknown) => void; onError: (e: Error) => void },
    ) {
      this.config = config;
      h.encoderCbs.value = cbs;
    }
    configure() {
      return Promise.resolve({
        codec: 'avc1.42E01F',
        variantLabel: 'fake',
        acceleration: 'hardware',
        width: this.config.width,
        height: this.config.height,
        framerate: this.config.framerate,
        bitrate: this.config.bitrate,
      });
    }
    encode() {
      return true;
    }
    forceNextKeyframe() {
      h.encoderForcedKeyframes.value++;
    }
    get queueSize() {
      return 0;
    }
    dispose() {}
    close() {
      return Promise.resolve();
    }
  },
}));

import { BroadcastPipeline, type BroadcastCallbacks } from './broadcaster';
import { DEFAULT_CAPTURE_CONFIG } from '../media/types';

// Announce for K7XQ2M + a 16-byte resume token (golden values).
const ANNOUNCE_K7XQ2M = new Uint8Array([0x01, 0x03, 0x06, 0x4b, 0x37, 0x58, 0x51, 0x32, 0x4d]);
const TOKEN_BYTES = new Uint8Array([
  0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f,
]);
const TOKEN_HEX = '000102030405060708090a0b0c0d0e0f';
const TOKEN_MSG = new Uint8Array([0x01, 0x09, 0x10, ...TOKEN_BYTES]);

interface FakeWT {
  wt: WebTransport;
  streams: Uint8Array[]; // fully-written outgoing keyframe streams
  die: (err?: unknown) => void; // settle `closed` like a server drop
}

function makeFakeWT(serverMessages: Uint8Array[] = [ANNOUNCE_K7XQ2M, TOKEN_MSG]): FakeWT {
  let settleClosed: { resolve: (v: unknown) => void; reject: (e: unknown) => void } = {
    resolve: () => {},
    reject: () => {},
  };
  const closed = new Promise((resolve, reject) => {
    settleClosed = { resolve, reject };
  });
  // Swallow the rejection if nobody re-awaits it after the first handler.
  closed.catch(() => {});
  const streams: Uint8Array[] = [];
  const wt = {
    closed,
    incomingUnidirectionalStreams: new ReadableStream<ReadableStream<Uint8Array>>({
      start(controller) {
        for (const msg of serverMessages) {
          controller.enqueue(
            new ReadableStream<Uint8Array>({
              start(c) {
                c.enqueue(msg);
                c.close();
              },
            }),
          );
        }
      },
    }),
    datagrams: { maxDatagramSize: 1200 },
    createUnidirectionalStream: () => {
      const chunks: Uint8Array[] = [];
      const ws = new WritableStream<Uint8Array>({
        write(chunk) {
          chunks.push(chunk);
        },
        close() {
          let len = 0;
          for (const c of chunks) len += c.length;
          const buf = new Uint8Array(len);
          let o = 0;
          for (const c of chunks) {
            buf.set(c, o);
            o += c.length;
          }
          streams.push(buf);
        },
      });
      return Promise.resolve(ws);
    },
    close: vi.fn(),
  } as unknown as WebTransport;
  return {
    wt,
    streams,
    die: (err?: unknown) => {
      if (err !== undefined) settleClosed.reject(err);
      else settleClosed.resolve(undefined);
    },
  };
}

function makeCaptureHandle() {
  return {
    stream: {} as MediaStream,
    track: { getSettings: () => ({ frameRate: 30 }), addEventListener: vi.fn() },
    capturePath: 'fake',
    startFrames: (cb: (frame: unknown) => void) => {
      h.frameCb.value = cb;
      return Promise.resolve();
    },
  };
}

function fakeFrame(tsUs: number) {
  return {
    timestamp: tsUs,
    displayWidth: 1920,
    displayHeight: 1080,
    codedWidth: 1920,
    codedHeight: 1080,
    close: vi.fn(),
  };
}

function fakeChunk(type: 'key' | 'delta', tsUs: number, bytes: Uint8Array) {
  return {
    type,
    timestamp: tsUs,
    byteLength: bytes.length,
    copyTo: (dst: Uint8Array) => dst.set(bytes),
  };
}

async function flush() {
  for (let i = 0; i < 8; i++) await Promise.resolve();
}

function makeCallbacks() {
  return {
    onSourceStream: vi.fn(),
    onEncoderConfigured: vi.fn(),
    onCapturePathChosen: vi.fn(),
    onStats: vi.fn<BroadcastCallbacks['onStats']>(),
    onError: vi.fn<BroadcastCallbacks['onError']>(),
    onEnded: vi.fn(),
    onBroadcastId: vi.fn<NonNullable<BroadcastCallbacks['onBroadcastId']>>(),
    onResumeToken: vi.fn<NonNullable<BroadcastCallbacks['onResumeToken']>>(),
    onReconnecting: vi.fn<NonNullable<BroadcastCallbacks['onReconnecting']>>(),
    onResumed: vi.fn<NonNullable<BroadcastCallbacks['onResumed']>>(),
  } satisfies BroadcastCallbacks;
}

async function startBroadcast(cbs: ReturnType<typeof makeCallbacks>) {
  const first = makeFakeWT();
  connectWebTransport.mockResolvedValueOnce(first.wt);
  startCapture.mockResolvedValue(makeCaptureHandle());
  const pipeline = new BroadcastPipeline(
    { ...DEFAULT_CAPTURE_CONFIG },
    'https://relay.test:4433',
    {},
    cbs,
  );
  await pipeline.start();
  await flush(); // announce + token streams consumed
  expect(cbs.onBroadcastId).toHaveBeenCalledWith('K7XQ2M');
  expect(cbs.onResumeToken).toHaveBeenCalledWith(TOKEN_HEX);

  // Prime the encoder and send one keyframe + one delta pre-death, so the
  // config datagram exists and the frameId counter is past zero.
  h.frameCb.value!(fakeFrame(1000));
  await flush();
  expect(h.encoderCbs.value).not.toBeNull();
  h.encoderCbs.value!.onEncoded({
    chunk: fakeChunk('key', 1000, new Uint8Array([0xaa])),
    meta: { decoderConfig: { codec: 'avc1.42E01F', description: new Uint8Array([0x01, 0x42, 0xe0, 0x1f]) } },
    encodeStartMs: 0,
    encodeEndMs: 1,
  });
  await flush();
  h.encoderCbs.value!.onEncoded({
    chunk: fakeChunk('delta', 2000, new Uint8Array([0xbb])),
    meta: undefined,
    encodeStartMs: 0,
    encodeEndMs: 1,
  });
  await flush();
  expect(first.streams.length).toBe(1); // frameId 0, the pre-death keyframe

  return { pipeline, first };
}

beforeEach(() => {
  vi.stubGlobal('window', globalThis);
  vi.useFakeTimers();
  connectWebTransport.mockReset();
  startCapture.mockReset();
  h.frameCb.value = null;
  h.encoderCbs.value = null;
  h.encoderForcedKeyframes.value = 0;
  h.sends.length = 0;
});

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe('broadcaster auto-resume (R17 W2)', () => {
  it('reconnects with the resume token, forces a keyframe, and keeps frameIDs', async () => {
    const cbs = makeCallbacks();
    const { pipeline, first } = await startBroadcast(cbs);

    const second = makeFakeWT();
    connectWebTransport.mockResolvedValueOnce(second.wt);

    // Abrupt death (no close code): fast 250ms first retry.
    first.die(new Error('connection lost'));
    await flush();
    expect(cbs.onReconnecting).toHaveBeenCalledWith(
      expect.objectContaining({ attempt: 1, delayMs: ABRUPT_DROP_RETRY_DELAY_MS }),
    );
    expect(cbs.onError).not.toHaveBeenCalled();
    expect(startCapture).toHaveBeenCalledTimes(1); // capture never restarted

    await vi.advanceTimersByTimeAsync(ABRUPT_DROP_RETRY_DELAY_MS);
    await flush();

    // Reclaimed the same broadcast with the token from the 0x09 message.
    expect(connectWebTransport).toHaveBeenCalledTimes(2);
    expect(connectWebTransport.mock.calls[1][0]).toBe(
      `https://relay.test:4433/publish/K7XQ2M?resume=${TOKEN_HEX}`,
    );
    expect(cbs.onResumed).toHaveBeenCalledTimes(1);
    // The pipeline demanded a keyframe from the encoder on re-attach.
    expect(h.encoderForcedKeyframes.value).toBe(1);

    // The next (forced) keyframe rides a stream on the NEW session with the
    // config embedded, and its frameId CONTINUES (2 after key=0, delta=1).
    h.encoderCbs.value!.onEncoded({
      chunk: fakeChunk('key', 3000, new Uint8Array([0xcc])),
      meta: undefined,
      encodeStartMs: 0,
      encodeEndMs: 1,
    });
    await flush();
    expect(second.streams.length).toBe(1);
    const msg = second.streams[0];
    const header = parseStreamFrameHeader(msg);
    expect(header.keyframe).toBe(true);
    expect(header.frameId).toBe(2); // no reset — resume, not restart
    expect(header.configLen).toBeGreaterThan(0);
    const config = parseDecoderConfig(
      msg.subarray(STREAM_FRAME_HEADER_SIZE, STREAM_FRAME_HEADER_SIZE + header.configLen),
    );
    expect(config.codec).toBe('avc1.42E01F');

    await pipeline.stop();
  });

  it('reconnects immediately on a 4002 drain close', async () => {
    const cbs = makeCallbacks();
    const { pipeline, first } = await startBroadcast(cbs);

    const second = makeFakeWT();
    connectWebTransport.mockResolvedValueOnce(second.wt);

    first.die({ closeCode: CLOSE_CODE_SERVER_DRAINING, reason: 'server draining' });
    await flush();
    expect(cbs.onReconnecting).toHaveBeenCalledWith(
      expect.objectContaining({ attempt: 1, delayMs: 0, closeCode: CLOSE_CODE_SERVER_DRAINING }),
    );
    await vi.advanceTimersByTimeAsync(0);
    await flush();
    expect(connectWebTransport).toHaveBeenCalledTimes(2);
    expect(cbs.onResumed).toHaveBeenCalledTimes(1);

    await pipeline.stop();
  });

  it('fails terminally once the resume ladder is exhausted', async () => {
    const cbs = makeCallbacks();
    const { pipeline, first } = await startBroadcast(cbs);

    connectWebTransport.mockRejectedValue(new Error('relay unreachable'));
    first.die(new Error('connection lost'));
    await flush();

    // Burn through the whole ladder (10 attempts, 15s cap).
    for (let i = 0; i < 12; i++) {
      await vi.advanceTimersByTimeAsync(15_000);
      await flush();
    }
    expect(cbs.onError).toHaveBeenCalledTimes(1);
    expect(cbs.onError.mock.calls[0][0].message).toMatch(/resume failed after 10 attempts/);
    expect(cbs.onEnded).toHaveBeenCalled(); // fail() stops the pipeline

    await pipeline.stop();
  });

  it('stop() during a pending resume cancels it cleanly', async () => {
    const cbs = makeCallbacks();
    const { pipeline, first } = await startBroadcast(cbs);

    first.die(new Error('connection lost'));
    await flush();
    expect(cbs.onReconnecting).toHaveBeenCalled();

    await pipeline.stop();
    expect(cbs.onEnded).toHaveBeenCalledTimes(1);

    await vi.advanceTimersByTimeAsync(60_000);
    await flush();
    expect(connectWebTransport).toHaveBeenCalledTimes(1); // never redialed
    expect(cbs.onError).not.toHaveBeenCalled();
  });

  it('a session death before the announce/token landed stays terminal', async () => {
    const cbs = makeCallbacks();
    // No server messages at all: the pipeline has no ID/token to resume with.
    const first = makeFakeWT([]);
    connectWebTransport.mockResolvedValueOnce(first.wt);
    startCapture.mockResolvedValue(makeCaptureHandle());
    const pipeline = new BroadcastPipeline(
      { ...DEFAULT_CAPTURE_CONFIG },
      'https://relay.test:4433',
      {},
      cbs,
    );
    await pipeline.start();
    await flush();

    first.die(new Error('connection lost'));
    await flush();
    expect(cbs.onError).toHaveBeenCalledTimes(1);
    expect(cbs.onReconnecting).not.toHaveBeenCalled();
  });
});
