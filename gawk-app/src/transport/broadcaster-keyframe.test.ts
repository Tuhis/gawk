// S4 (docs/12): handleEncoded routes keyframes onto a reliable unidirectional
// stream (config embedded) and deltas onto datagrams. Mocks mirror
// broadcaster-fallback.test.ts: a fake Encoder whose onEncoded callback the
// test drives directly, a passthrough preprocessor, a DatagramSender that
// records sends, and a fake WebTransport whose createUnidirectionalStream
// captures the bytes written to each keyframe stream.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import {
  parseStreamFrameHeader,
  parseDecoderConfig,
  parseVideoChunk,
  peekType,
  TYPE_VIDEO_CHUNK,
  STREAM_FRAME_HEADER_SIZE,
} from './wire';

const connectWebTransport = vi.fn();
const startCapture = vi.fn();

const h = vi.hoisted(() => ({
  frameCb: { value: null as null | ((frame: unknown) => void) },
  encoderCbs: { value: null as null | { onEncoded: (e: unknown) => void; onError: (e: Error) => void } },
  sends: [] as Uint8Array[][],
  streams: [] as Uint8Array[], // one entry per fully-written keyframe stream
  kfFail: { value: false }, // when true, keyframe stream opens reject
}));

vi.mock('./connection', () => ({
  connectWebTransport: (...args: unknown[]) => connectWebTransport(...args),
  // Time-sync reply loop (R5 Q2): stays open, delivers nothing.
  readDatagrams: () => new Promise(() => {}),
  DatagramSender: class {
    send = (datagrams: Uint8Array[]) => {
      h.sends.push(datagrams);
      return Promise.resolve();
    };
    close = vi.fn();
  },
}));

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

function makeFakeWT() {
  let resolveClosed: (v: unknown) => void = () => {};
  const closed = new Promise((res) => {
    resolveClosed = res;
  });
  return {
    closed,
    incomingUnidirectionalStreams: new ReadableStream({ start() {} }),
    datagrams: { maxDatagramSize: 1200 },
    createUnidirectionalStream: () => {
      if (h.kfFail.value) return Promise.reject(new Error('stream open failed'));
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
          h.streams.push(buf);
        },
      });
      return Promise.resolve(ws);
    },
    close: vi.fn(() => resolveClosed({ closeCode: 0, reason: '' })),
  } as unknown as WebTransport;
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
  // 1920x1080 avoids the >1080p hardware-probe/cap branch.
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
  for (let i = 0; i < 6; i++) await Promise.resolve();
}

async function makeStartedPipeline(now?: () => number) {
  h.sends.length = 0;
  h.streams.length = 0;
  h.frameCb.value = null;
  h.encoderCbs.value = null;
  h.kfFail.value = false;

  const cbs = {
    onSourceStream: vi.fn(),
    onEncoderConfigured: vi.fn(),
    onCapturePathChosen: vi.fn(),
    onStats: vi.fn<BroadcastCallbacks['onStats']>(),
    onError: vi.fn(),
    onEnded: vi.fn(),
    onBroadcastId: vi.fn(),
  } satisfies BroadcastCallbacks;
  connectWebTransport.mockResolvedValue(makeFakeWT());
  startCapture.mockResolvedValue(makeCaptureHandle());

  const pipeline = new BroadcastPipeline(
    { ...DEFAULT_CAPTURE_CONFIG },
    'https://relay.test:4433',
    {},
    cbs,
    undefined,
    now,
  );
  await pipeline.start();

  // Prime the encoder: one frame triggers initEncoder (async configure()).
  h.frameCb.value!(fakeFrame(1000));
  await flush();
  expect(h.encoderCbs.value).not.toBeNull();
  return { pipeline, cbs };
}

beforeEach(() => {
  vi.stubGlobal('window', globalThis);
  connectWebTransport.mockReset();
  startCapture.mockReset();
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('handleEncoded channel split (R8)', () => {
  it('sends a keyframe over a uni stream with the config embedded, deltas over datagrams', async () => {
    const { pipeline } = await makeStartedPipeline();

    const keyBytes = new Uint8Array([0xaa, 0xbb, 0xcc, 0xdd, 0xee]);
    h.encoderCbs.value!.onEncoded({
      chunk: fakeChunk('key', 1000, keyBytes),
      meta: { decoderConfig: { codec: 'avc1.42E01F', description: new Uint8Array([0x01, 0x42, 0xe0, 0x1f]) } },
      encodeStartMs: 0,
      encodeEndMs: 1,
    });
    await flush();

    // Keyframe went to exactly one uni stream, never to datagrams. (The
    // pipeline also sends TimeSync pings as datagrams since R5 Q2 — only
    // video chunks count here.)
    const videoSends = () =>
      h.sends.filter((batch) => batch.some((d) => peekType(d).msgType === TYPE_VIDEO_CHUNK));
    expect(h.streams.length).toBe(1);
    expect(videoSends().length).toBe(0);

    const msg = h.streams[0];
    const header = parseStreamFrameHeader(msg);
    expect(header.keyframe).toBe(true);
    expect(header.frameId).toBe(0);
    expect(header.configLen).toBeGreaterThan(0);

    const config = parseDecoderConfig(msg.subarray(STREAM_FRAME_HEADER_SIZE, STREAM_FRAME_HEADER_SIZE + header.configLen));
    expect(config.codec).toBe('avc1.42E01F');
    const payload = msg.subarray(STREAM_FRAME_HEADER_SIZE + header.configLen);
    expect(Array.from(payload)).toEqual(Array.from(keyBytes));

    // A following delta goes over datagrams, never a stream.
    const deltaBytes = new Uint8Array([0x11, 0x22, 0x33]);
    h.encoderCbs.value!.onEncoded({
      chunk: fakeChunk('delta', 2000, deltaBytes),
      meta: undefined,
      encodeStartMs: 0,
      encodeEndMs: 1,
    });
    await flush();

    expect(h.streams.length).toBe(1); // unchanged
    expect(videoSends().length).toBe(1);
    const datagrams = videoSends()[0];
    expect(peekType(datagrams[0]).msgType).toBe(TYPE_VIDEO_CHUNK);
    const { header: dh, payload: dp } = parseVideoChunk(datagrams[0]);
    expect(dh.keyframe).toBe(false);
    expect(dh.frameId).toBe(1); // numbering continues across channels
    expect(Array.from(dp)).toEqual(Array.from(deltaBytes));

    await pipeline.stop();
  });
});

describe('funnel rates (R9 M6, docs/13 D5)', () => {
  it('captureFps counts pre-gate frames; sentFps counts only successful sends', async () => {
    vi.useFakeTimers();
    try {
      let t = 0;
      const { pipeline, cbs } = await makeStartedPipeline(() => t);

      // Successful keyframe stream, then a failed one, then a delta over
      // datagrams. Sent = keyframe + delta = 2; the failed keyframe must not
      // count as sent.
      const meta = {
        decoderConfig: { codec: 'avc1.42E01F', description: new Uint8Array([0x01, 0x42, 0xe0, 0x1f]) },
      };
      h.encoderCbs.value!.onEncoded({
        chunk: fakeChunk('key', 1000, new Uint8Array([0xaa])),
        meta,
        encodeStartMs: 0,
        encodeEndMs: 1,
      });
      await flush();
      h.kfFail.value = true;
      h.encoderCbs.value!.onEncoded({
        chunk: fakeChunk('key', 2000, new Uint8Array([0xbb])),
        meta: undefined,
        encodeStartMs: 0,
        encodeEndMs: 1,
      });
      await flush();
      h.kfFail.value = false;
      h.encoderCbs.value!.onEncoded({
        chunk: fakeChunk('delta', 3000, new Uint8Array([0xcc])),
        meta: undefined,
        encodeStartMs: 0,
        encodeEndMs: 1,
      });
      await flush();

      // Two more captured frames on top of the priming one → 3 in the window.
      h.frameCb.value!(fakeFrame(2000));
      h.frameCb.value!(fakeFrame(3000));
      await flush();

      t = 500; // the injected clock the pipeline computes dt from
      await vi.advanceTimersByTimeAsync(500); // fire the stats interval
      const stats = cbs.onStats.mock.calls.at(-1)?.[0];
      expect(stats).toBeDefined();
      expect(stats!.captureFps).toBeCloseTo(3 / 0.5, 5);
      expect(stats!.sentFps).toBeCloseTo(2 / 0.5, 5);
      expect(stats!.keyframeStreamsSent).toBe(1);
      expect(stats!.keyframeStreamsFailed).toBe(1);
      expect(stats!.keyframeBytesSent).toBeGreaterThan(0);
      // No getStats on the fake WebTransport → connection stays null, never throws.
      expect(stats!.connection).toBeNull();

      await pipeline.stop();
    } finally {
      vi.useRealTimers();
    }
  });
});
