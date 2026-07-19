// R15 N2/N3 (docs/20 Decision 6): the pipeline's audio lane wiring — toggle
// off runs zero audio paths, no-track and no-AudioEncoder degrade to
// annotated video-only (never an error, never a placement change), the happy
// path sends audio datagrams, and a lane error annotates without touching
// the broadcast.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

const connectWebTransport = vi.fn();
const readDatagrams = vi.fn();
const sentDatagrams: Uint8Array[][] = [];

vi.mock('./connection', () => ({
  connectWebTransport: (...args: unknown[]) => connectWebTransport(...args),
  readDatagrams: (...args: unknown[]) => readDatagrams(...args),
  DatagramSender: class {
    send = vi.fn((datagrams: Uint8Array[]) => {
      sentDatagrams.push(datagrams);
      return Promise.resolve();
    });
    close = vi.fn();
  },
}));

import { BroadcastPipeline, type BroadcastCallbacks, type BroadcastStats } from './broadcaster';
import type { BroadcastMediaSource } from '../media/capture';
import { DEFAULT_CAPTURE_CONFIG } from '../media/types';
import { TYPE_AUDIO_CONFIG, TYPE_AUDIO_FRAME, parseAudioFrame } from './wire';

const ANNOUNCE_K7XQ2M = new Uint8Array([0x01, 0x03, 0x06, 0x4b, 0x37, 0x58, 0x51, 0x32, 0x4d]);

function makeFakeWT() {
  const closed = new Promise(() => {});
  const incomingUnidirectionalStreams = new ReadableStream<ReadableStream<Uint8Array>>({
    start(controller) {
      controller.enqueue(
        new ReadableStream<Uint8Array>({
          start(c) {
            c.enqueue(ANNOUNCE_K7XQ2M);
            c.close();
          },
        }),
      );
    },
  });
  return {
    closed,
    incomingUnidirectionalStreams,
    datagrams: { maxDatagramSize: 1200 },
    close: vi.fn(),
  } as unknown as WebTransport;
}

function makeCallbacks() {
  return {
    onSourceStream: vi.fn<BroadcastCallbacks['onSourceStream']>(),
    onEncoderConfigured: vi.fn<BroadcastCallbacks['onEncoderConfigured']>(),
    onCapturePathChosen: vi.fn<BroadcastCallbacks['onCapturePathChosen']>(),
    onStats: vi.fn<BroadcastCallbacks['onStats']>(),
    onError: vi.fn<BroadcastCallbacks['onError']>(),
    onEnded: vi.fn<BroadcastCallbacks['onEnded']>(),
  };
}

function makeMedia(audioTrack: MediaStreamTrack | null): BroadcastMediaSource {
  return {
    capturePath: 'mstp',
    stream: null,
    nativeFps: 30,
    audioTrack,
    onEnded: vi.fn(),
    startFrames: vi.fn(() => Promise.resolve()),
    stop: vi.fn(),
  };
}

function makePipeline(
  cbs: ReturnType<typeof makeCallbacks>,
  media: BroadcastMediaSource,
  audio: boolean,
): BroadcastPipeline {
  return new BroadcastPipeline(
    { ...DEFAULT_CAPTURE_CONFIG, ...(audio ? { audio: true } : {}) },
    'https://relay.test:4433',
    {},
    cbs,
    undefined,
    undefined,
    async () => media,
  );
}

async function lastStats(cbs: ReturnType<typeof makeCallbacks>): Promise<BroadcastStats> {
  await vi.advanceTimersByTimeAsync(500);
  const stats = cbs.onStats.mock.calls.at(-1)?.[0];
  expect(stats).toBeDefined();
  return stats!;
}

// ── Fakes for the runtime lane (globals the wrapper looks up) ──

interface FakeAudioEncoderGlobal {
  instances: FakeAudioEncoderInstance[];
}
interface FakeAudioEncoderInstance {
  state: string;
  config: unknown;
  output: (chunk: unknown, meta?: unknown) => void;
  error: (e: unknown) => void;
  encoded: number[];
}

function stubAudioGlobals() {
  const encoderGlobal: FakeAudioEncoderGlobal = { instances: [] };
  const controllers: ReadableStreamDefaultController<unknown>[] = [];

  class FakeAudioEncoder {
    state = 'unconfigured';
    config: unknown;
    encoded: number[] = [];
    private cbs: { output: (chunk: unknown, meta?: unknown) => void; error: (e: unknown) => void };
    constructor(cbs: { output: (chunk: unknown, meta?: unknown) => void; error: (e: unknown) => void }) {
      this.cbs = cbs;
      encoderGlobal.instances.push({
        state: this.state,
        config: undefined,
        output: cbs.output,
        error: cbs.error,
        encoded: this.encoded,
      });
    }
    configure(config: unknown) {
      this.config = config;
      this.state = 'configured';
      encoderGlobal.instances[encoderGlobal.instances.length - 1].config = config;
    }
    encode(data: { timestamp: number }) {
      this.encoded.push(data.timestamp);
      // Echo one encoded packet per AudioData, synchronously.
      this.cbs.output(
        {
          timestamp: data.timestamp,
          byteLength: 3,
          copyTo: (dst: Uint8Array) => dst.set([1, 2, 3]),
        },
        {},
      );
    }
    close() {
      this.state = 'closed';
    }
  }

  class FakeMSTP {
    readable = new ReadableStream({
      start(c) {
        controllers.push(c);
      },
    });
  }

  vi.stubGlobal('AudioEncoder', FakeAudioEncoder);
  vi.stubGlobal('MediaStreamTrackProcessor', FakeMSTP);
  return { encoderGlobal, controllers };
}

function audioDataLike(timestamp: number) {
  return {
    timestamp,
    sampleRate: 48000,
    numberOfChannels: 2,
    close: vi.fn(),
  };
}

const fakeAudioTrack = () => ({ stop: vi.fn() }) as unknown as MediaStreamTrack;

beforeEach(() => {
  vi.useFakeTimers({
    toFake: ['setTimeout', 'clearTimeout', 'setInterval', 'clearInterval', 'performance'],
  });
  vi.stubGlobal('window', globalThis);
  connectWebTransport.mockReset();
  connectWebTransport.mockImplementation(() => Promise.resolve(makeFakeWT()));
  readDatagrams.mockReset();
  readDatagrams.mockReturnValue(new Promise(() => {}));
  sentDatagrams.length = 0;
});

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe('BroadcastPipeline audio lane wiring', () => {
  it('toggle off: audioState stays off and no audio datagrams exist', async () => {
    const cbs = makeCallbacks();
    const pipeline = makePipeline(cbs, makeMedia(fakeAudioTrack()), false);
    await pipeline.start();
    const stats = await lastStats(cbs);
    expect(stats.audioState).toBe('off');
    expect(sentDatagrams.flat().filter((d) => d[1] === TYPE_AUDIO_FRAME)).toHaveLength(0);
    await pipeline.stop();
  });

  it('toggle on + no audio track: video-only annotation, no error', async () => {
    const cbs = makeCallbacks();
    const pipeline = makePipeline(cbs, makeMedia(null), true);
    await pipeline.start();
    const stats = await lastStats(cbs);
    expect(stats.audioState).toBe('no-track');
    expect(cbs.onError).not.toHaveBeenCalled();
    await pipeline.stop();
  });

  it('toggle on + track but no AudioEncoder in scope: unsupported annotation, video unaffected', async () => {
    // jsdom has neither AudioEncoder nor MediaStreamTrackProcessor — exactly
    // the worker-without-AudioEncoder shape (docs/20 N3): the pipeline keeps
    // its placement and annotates.
    const cbs = makeCallbacks();
    const pipeline = makePipeline(cbs, makeMedia(fakeAudioTrack()), true);
    await pipeline.start();
    const stats = await lastStats(cbs);
    expect(stats.audioState).toBe('unsupported');
    expect(cbs.onError).not.toHaveBeenCalled();
    await pipeline.stop();
  });

  it('happy path: audio datagrams flow, config piggybacked, stats count', async () => {
    const { encoderGlobal, controllers } = stubAudioGlobals();
    const cbs = makeCallbacks();
    const pipeline = makePipeline(cbs, makeMedia(fakeAudioTrack()), true);
    await pipeline.start();

    // Feed two AudioData through the fake MSTP.
    expect(controllers).toHaveLength(1);
    const first = audioDataLike(0);
    const second = audioDataLike(20_000);
    controllers[0].enqueue(first);
    controllers[0].enqueue(second);
    await vi.advanceTimersByTimeAsync(10);

    // Encoder configured from the data in hand.
    expect(encoderGlobal.instances).toHaveLength(1);
    expect(encoderGlobal.instances[0].config).toMatchObject({
      codec: 'opus',
      sampleRate: 48000,
      numberOfChannels: 2,
    });
    expect(first.close).toHaveBeenCalled();
    expect(second.close).toHaveBeenCalled();

    const flat = sentDatagrams.flat();
    const configs = flat.filter((d) => d[1] === TYPE_AUDIO_CONFIG);
    const frames = flat.filter((d) => d[1] === TYPE_AUDIO_FRAME);
    expect(configs).toHaveLength(1); // both packets inside the 1 Hz window
    expect(frames).toHaveLength(2);
    expect(parseAudioFrame(frames[0]).header.seq).toBe(0);
    expect(parseAudioFrame(frames[1]).header.seq).toBe(1);

    const stats = await lastStats(cbs);
    expect(stats.audioState).toBe('active');
    expect(stats.audioEncodedPackets).toBe(2);
    expect(stats.audioPacketsSent).toBe(2);
    expect(stats.audioSampleRate).toBe(48000);
    await pipeline.stop();
  });

  // Regression (self-review 2026-07-19): startAudioLane constructs a real
  // MediaStreamTrackProcessor, and that can throw synchronously (ended track,
  // a scope whose MSTP rejects audio tracks). It runs inside startMedia(),
  // whose throw path fails the ENTIRE broadcast with a capture-phase error —
  // exactly what Decision 6 forbids ("never the broadcast").
  it('a lane that fails to construct annotates without failing the broadcast', async () => {
    stubAudioGlobals();
    // MSTP exists (so the lane is attempted) but explodes on construction.
    vi.stubGlobal(
      'MediaStreamTrackProcessor',
      class {
        constructor() {
          throw new DOMException('audio tracks unsupported', 'NotSupportedError');
        }
      },
    );
    const cbs = makeCallbacks();
    const pipeline = makePipeline(cbs, makeMedia(fakeAudioTrack()), true);

    // The broadcast must start — not reject with BroadcastStartError.
    await expect(pipeline.start()).resolves.toBeUndefined();

    const stats = await lastStats(cbs);
    expect(stats.audioState).toBe('error');
    expect(cbs.onError).not.toHaveBeenCalled();
    expect(cbs.onEnded).not.toHaveBeenCalled();
    await pipeline.stop();
  });

  it('a lane error annotates and never fails the broadcast', async () => {
    const { encoderGlobal, controllers } = stubAudioGlobals();
    const cbs = makeCallbacks();
    const pipeline = makePipeline(cbs, makeMedia(fakeAudioTrack()), true);
    await pipeline.start();

    controllers[0].enqueue(audioDataLike(0));
    await vi.advanceTimersByTimeAsync(10);
    encoderGlobal.instances[0].error(new Error('audio encoder exploded'));

    const stats = await lastStats(cbs);
    expect(stats.audioState).toBe('error');
    expect(cbs.onError).not.toHaveBeenCalled();
    expect(cbs.onEnded).not.toHaveBeenCalled();
    await pipeline.stop();
  });
});
