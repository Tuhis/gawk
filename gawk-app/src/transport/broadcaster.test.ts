// BroadcastPipeline start/announce behavior. connection + media modules are
// mocked so no WebTransport or WebCodecs is involved; the fake WebTransport
// only models what the pipeline touches (closed promise, incoming uni
// streams, close()).

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

const connectWebTransport = vi.fn();
const startCapture = vi.fn();
const stopCapture = vi.fn();
const readDatagrams = vi.fn();
// Every datagram batch handed to any DatagramSender instance, in order — the
// observable send stream (pings, clock mappings, video).
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

vi.mock('../media/capture', () => ({
  startCapture: (...args: unknown[]) => startCapture(...args),
  stopCapture: (...args: unknown[]) => stopCapture(...args),
}));

vi.mock('../media/encoder', () => ({
  Encoder: class {},
}));

import { BroadcastPipeline, type BroadcastCallbacks } from './broadcaster';
import { DEFAULT_CAPTURE_CONFIG } from '../media/types';
import { CLOCK_MAPPING_INTERVAL_MS } from './time-sync';
import {
  TYPE_CLOCK_MAPPING,
  TYPE_TIME_SYNC,
  encodeTimeSync,
  encodeViewerCount,
  parseClockMapping,
  parseTimeSync,
} from './wire';

// Golden BroadcastAnnounce for ID K7XQ2M (docs/06-multi-broadcaster.md).
const ANNOUNCE_K7XQ2M = new Uint8Array([0x01, 0x03, 0x06, 0x4b, 0x37, 0x58, 0x51, 0x32, 0x4d]);

interface FakeWT {
  wt: WebTransport;
  close: ReturnType<typeof vi.fn>;
}

// announceChunks: byte chunks delivered on the first server-initiated uni
// stream; null = the stream never arrives (but the connection stays up).
function makeFakeWT(announceChunks: Uint8Array[] | null): FakeWT {
  let resolveClosed: (info: unknown) => void = () => {};
  const closed = new Promise((res) => {
    resolveClosed = res;
  });
  const incomingUnidirectionalStreams = new ReadableStream<ReadableStream<Uint8Array>>({
    start(controller) {
      if (announceChunks) {
        controller.enqueue(
          new ReadableStream<Uint8Array>({
            start(c) {
              for (const chunk of announceChunks) c.enqueue(chunk);
              c.close();
            },
          }),
        );
      }
    },
  });
  const close = vi.fn(() => resolveClosed({ closeCode: 0, reason: '' }));
  const wt = {
    closed,
    incomingUnidirectionalStreams,
    datagrams: { maxDatagramSize: 1200 },
    close,
  } as unknown as WebTransport;
  return { wt, close };
}

function makeCaptureHandle() {
  return {
    stream: {} as MediaStream,
    track: {
      getSettings: () => ({ frameRate: 30 }),
      addEventListener: vi.fn(),
    },
    capturePath: 'fake',
    startFrames: vi.fn(() => Promise.resolve()),
  };
}

// Typed per-callback so the mocks satisfy BroadcastCallbacks: vitest 4 types
// a bare vi.fn() as Mock<Procedure | Constructable>, which tsc rejects
// against a concrete callback signature.
function makeCallbacks() {
  return {
    onSourceStream: vi.fn<BroadcastCallbacks['onSourceStream']>(),
    onEncoderConfigured: vi.fn<BroadcastCallbacks['onEncoderConfigured']>(),
    onCapturePathChosen: vi.fn<BroadcastCallbacks['onCapturePathChosen']>(),
    onStats: vi.fn<BroadcastCallbacks['onStats']>(),
    onError: vi.fn<BroadcastCallbacks['onError']>(),
    onEnded: vi.fn<BroadcastCallbacks['onEnded']>(),
    onBroadcastId: vi.fn<NonNullable<BroadcastCallbacks['onBroadcastId']>>(),
  };
}

function makePipeline(cbs: BroadcastCallbacks, broadcastId?: string): BroadcastPipeline {
  return new BroadcastPipeline(
    { ...DEFAULT_CAPTURE_CONFIG },
    'https://relay.test:4433',
    {},
    cbs,
    broadcastId,
  );
}

beforeEach(() => {
  vi.stubGlobal('window', globalThis);
  connectWebTransport.mockReset();
  startCapture.mockReset();
  stopCapture.mockReset();
  readDatagrams.mockReset();
  // The reply loop stays open for the life of the session by default.
  readDatagrams.mockReturnValue(new Promise(() => {}));
  sentDatagrams.length = 0;
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('BroadcastPipeline URLs', () => {
  it('dials /publish when minting', async () => {
    const fake = makeFakeWT([ANNOUNCE_K7XQ2M]);
    connectWebTransport.mockResolvedValue(fake.wt);
    startCapture.mockResolvedValue(makeCaptureHandle());
    const pipeline = makePipeline(makeCallbacks());
    await pipeline.start();
    expect(connectWebTransport).toHaveBeenCalledWith('https://relay.test:4433/publish', {});
    await pipeline.stop();
  });

  it('dials /publish/<id> when reclaiming', async () => {
    const fake = makeFakeWT([ANNOUNCE_K7XQ2M]);
    connectWebTransport.mockResolvedValue(fake.wt);
    startCapture.mockResolvedValue(makeCaptureHandle());
    const pipeline = makePipeline(makeCallbacks(), 'K7XQ2M');
    await pipeline.start();
    expect(connectWebTransport).toHaveBeenCalledWith('https://relay.test:4433/publish/K7XQ2M', {});
    await pipeline.stop();
  });

  // R2: publishing requires the pre-shared secret; it travels as a query
  // param because the WebTransport JS API cannot set request headers.
  it('appends ?secret= when a publish secret is configured', async () => {
    const fake = makeFakeWT([ANNOUNCE_K7XQ2M]);
    connectWebTransport.mockResolvedValue(fake.wt);
    startCapture.mockResolvedValue(makeCaptureHandle());
    const opts = { publishSecret: 's3cret' };
    const pipeline = new BroadcastPipeline(
      { ...DEFAULT_CAPTURE_CONFIG },
      'https://relay.test:4433',
      opts,
      makeCallbacks(),
    );
    await pipeline.start();
    expect(connectWebTransport).toHaveBeenCalledWith('https://relay.test:4433/publish?secret=s3cret', opts);
    await pipeline.stop();
  });

  it('appends ?secret= on the reclaim URL too', async () => {
    const fake = makeFakeWT([ANNOUNCE_K7XQ2M]);
    connectWebTransport.mockResolvedValue(fake.wt);
    startCapture.mockResolvedValue(makeCaptureHandle());
    const opts = { publishSecret: 's3cret' };
    const pipeline = new BroadcastPipeline(
      { ...DEFAULT_CAPTURE_CONFIG },
      'https://relay.test:4433',
      opts,
      makeCallbacks(),
      'K7XQ2M',
    );
    await pipeline.start();
    expect(connectWebTransport).toHaveBeenCalledWith('https://relay.test:4433/publish/K7XQ2M?secret=s3cret', opts);
    await pipeline.stop();
  });
});

describe('BroadcastPipeline announce handling', () => {
  it('delivers onBroadcastId from a multi-chunk announce stream read', async () => {
    const fake = makeFakeWT([
      ANNOUNCE_K7XQ2M.subarray(0, 3),
      ANNOUNCE_K7XQ2M.subarray(3, 6),
      ANNOUNCE_K7XQ2M.subarray(6),
    ]);
    connectWebTransport.mockResolvedValue(fake.wt);
    startCapture.mockResolvedValue(makeCaptureHandle());
    const cbs = makeCallbacks();
    const pipeline = makePipeline(cbs);
    await pipeline.start();
    await vi.waitFor(() => expect(cbs.onBroadcastId).toHaveBeenCalledWith('K7XQ2M'));
    await pipeline.stop();
  });

  it('does not gate media start on the announce arriving', async () => {
    // The design locks this: capture/encode begins immediately; only the UI
    // code display waits for the announce (docs/06, "Announce vs. first
    // datagrams ordering").
    const fake = makeFakeWT(null); // announce stream never arrives
    connectWebTransport.mockResolvedValue(fake.wt);
    startCapture.mockResolvedValue(makeCaptureHandle());
    const pipeline = makePipeline(makeCallbacks());
    const outcome = await Promise.race([
      pipeline.start().then(() => 'started'),
      new Promise<string>((r) => setTimeout(() => r('gated-on-announce'), 300)),
    ]);
    expect(outcome).toBe('started');
    expect(startCapture).toHaveBeenCalled();
    await pipeline.stop();
  });
});

describe('BroadcastPipeline start failures', () => {
  it('closes the WebTransport session when capture fails after connect', async () => {
    // A user cancelling the share picker must not leak a live publisher
    // session (a zombie would hold the broadcast ID until the tab closes).
    const fake = makeFakeWT([ANNOUNCE_K7XQ2M]);
    connectWebTransport.mockResolvedValue(fake.wt);
    startCapture.mockRejectedValue(new DOMException('Permission denied', 'NotAllowedError'));
    const pipeline = makePipeline(makeCallbacks());
    await expect(pipeline.start()).rejects.toThrow();
    expect(fake.close).toHaveBeenCalled();
  });

  it('tags a capture failure with phase "capture"', async () => {
    const fake = makeFakeWT([ANNOUNCE_K7XQ2M]);
    connectWebTransport.mockResolvedValue(fake.wt);
    startCapture.mockRejectedValue(new Error('user cancelled the picker'));
    const pipeline = makePipeline(makeCallbacks());
    await expect(pipeline.start()).rejects.toMatchObject({ phase: 'capture' });
  });

  it('tags a connect failure with phase "connect"', async () => {
    connectWebTransport.mockRejectedValue(new Error('connect failed'));
    const pipeline = makePipeline(makeCallbacks());
    await expect(pipeline.start()).rejects.toMatchObject({ phase: 'connect' });
    expect(startCapture).not.toHaveBeenCalled();
  });
});

// R5 Q2 (docs/15): the broadcaster pings the relay for clock sync and, once
// an offset sample exists, publishes a ClockMapping so viewers can compute
// absolute capture→render latency.
describe('BroadcastPipeline time sync + clock mapping', () => {
  it('pings on start, then publishes the mapping derived from the reply', async () => {
    vi.useFakeTimers({
      toFake: ['setTimeout', 'clearTimeout', 'setInterval', 'clearInterval', 'performance'],
    });
    try {
      const fake = makeFakeWT([ANNOUNCE_K7XQ2M]);
      connectWebTransport.mockResolvedValue(fake.wt);
      startCapture.mockResolvedValue(makeCaptureHandle());
      let deliver: ((d: Uint8Array) => void) | null = null;
      readDatagrams.mockImplementation((_wt: unknown, onDatagram: (d: Uint8Array) => void) => {
        deliver = onDatagram;
        return new Promise(() => {});
      });

      const pipeline = makePipeline(makeCallbacks());
      await pipeline.start();

      // The first ping goes out at start (type 0x05, serverTimeUs 0).
      const pings = sentDatagrams.flat().filter((d) => d[1] === TYPE_TIME_SYNC);
      expect(pings.length).toBeGreaterThanOrEqual(1);
      const ping = parseTimeSync(pings[0]);
      expect(ping.serverTimeUs).toBe(0n);

      // Relay reply: echo the client time, server clock at 90s. With fake
      // timers, t1 === t0 → rtt 0 → offset = 90_000_000 − t0.
      const reply = encodeTimeSync({ clientTimeUs: ping.clientTimeUs, serverTimeUs: 90_000_000n });
      (deliver as unknown as (d: Uint8Array) => void)(reply);

      // The mapping check runs on a timer; advance past it.
      await vi.advanceTimersByTimeAsync(1_100);
      const mappings = sentDatagrams.flat().filter((d) => d[1] === TYPE_CLOCK_MAPPING);
      expect(mappings.length).toBe(1);
      expect(parseClockMapping(mappings[0])).toBe(90_000_000n - ping.clientTimeUs);

      // Refresh cadence: no re-send inside CLOCK_MAPPING_INTERVAL_MS, one after.
      await vi.advanceTimersByTimeAsync(2_000);
      expect(sentDatagrams.flat().filter((d) => d[1] === TYPE_CLOCK_MAPPING).length).toBe(1);
      await vi.advanceTimersByTimeAsync(CLOCK_MAPPING_INTERVAL_MS);
      expect(
        sentDatagrams.flat().filter((d) => d[1] === TYPE_CLOCK_MAPPING).length,
      ).toBeGreaterThanOrEqual(2);

      // The self-owned RTT surfaces in stats.
      const cbs = makeCallbacks();
      void cbs; // (stats asserted via the pipeline's own callback below)
      await pipeline.stop();
    } finally {
      vi.useRealTimers();
    }
  });

  it('reports timeSyncRttMs in stats once synced, null before', async () => {
    vi.useFakeTimers({
      toFake: ['setTimeout', 'clearTimeout', 'setInterval', 'clearInterval', 'performance'],
    });
    try {
      const fake = makeFakeWT([ANNOUNCE_K7XQ2M]);
      connectWebTransport.mockResolvedValue(fake.wt);
      startCapture.mockResolvedValue(makeCaptureHandle());
      let deliver: ((d: Uint8Array) => void) | null = null;
      readDatagrams.mockImplementation((_wt: unknown, onDatagram: (d: Uint8Array) => void) => {
        deliver = onDatagram;
        return new Promise(() => {});
      });

      const cbs = makeCallbacks();
      const pipeline = makePipeline(cbs);
      await pipeline.start();

      await vi.advanceTimersByTimeAsync(500); // one stats tick, unsynced
      const first = cbs.onStats.mock.calls.at(-1)?.[0];
      expect(first?.timeSyncRttMs).toBeNull();

      const ping = parseTimeSync(sentDatagrams.flat().filter((d) => d[1] === TYPE_TIME_SYNC)[0]);
      (deliver as unknown as (d: Uint8Array) => void)(
        encodeTimeSync({ clientTimeUs: ping.clientTimeUs, serverTimeUs: 1_000_000n }),
      );
      await vi.advanceTimersByTimeAsync(500);
      const synced = cbs.onStats.mock.calls.at(-1)?.[0];
      expect(synced?.timeSyncRttMs).not.toBeNull();

      await pipeline.stop();
    } finally {
      vi.useRealTimers();
    }
  });
});

// R18 (docs/23 Decision 7): the relay pushes the live viewer count as a
// datagram on the publisher session; the read loop surfaces it in stats
// without disturbing the TimeSync replies sharing that loop.
describe('BroadcastPipeline viewer count (R18)', () => {
  it('surfaces the pushed count in stats, ignores malformed, keeps time sync working', async () => {
    vi.useFakeTimers({
      toFake: ['setTimeout', 'clearTimeout', 'setInterval', 'clearInterval', 'performance'],
    });
    try {
      const fake = makeFakeWT([ANNOUNCE_K7XQ2M]);
      connectWebTransport.mockResolvedValue(fake.wt);
      startCapture.mockResolvedValue(makeCaptureHandle());
      let deliver: ((d: Uint8Array) => void) | null = null;
      readDatagrams.mockImplementation((_wt: unknown, onDatagram: (d: Uint8Array) => void) => {
        deliver = onDatagram;
        return new Promise(() => {});
      });

      const cbs = makeCallbacks();
      const pipeline = makePipeline(cbs);
      await pipeline.start();

      await vi.advanceTimersByTimeAsync(500); // one stats tick, no push yet
      expect(cbs.onStats.mock.calls.at(-1)?.[0].viewerCount).toBeNull();

      (deliver as unknown as (d: Uint8Array) => void)(encodeViewerCount(3));
      await vi.advanceTimersByTimeAsync(500);
      expect(cbs.onStats.mock.calls.at(-1)?.[0].viewerCount).toBe(3);

      // A malformed push (right type, wrong version) is dropped, keeping the
      // last good count.
      const bad = encodeViewerCount(9);
      bad[0] = 0x02;
      (deliver as unknown as (d: Uint8Array) => void)(bad);
      await vi.advanceTimersByTimeAsync(500);
      expect(cbs.onStats.mock.calls.at(-1)?.[0].viewerCount).toBe(3);

      // The TimeSync reply sharing this read loop still lands.
      const ping = parseTimeSync(sentDatagrams.flat().filter((d) => d[1] === TYPE_TIME_SYNC)[0]);
      (deliver as unknown as (d: Uint8Array) => void)(
        encodeTimeSync({ clientTimeUs: ping.clientTimeUs, serverTimeUs: 1_000_000n }),
      );
      await vi.advanceTimersByTimeAsync(500);
      const last = cbs.onStats.mock.calls.at(-1)?.[0];
      expect(last?.timeSyncRttMs).not.toBeNull();
      expect(last?.viewerCount).toBe(3);

      await pipeline.stop();
    } finally {
      vi.useRealTimers();
    }
  });
});

// R11 (docs/16): the media-source seam that lets the broadcast worker inject
// a transferred-track source in place of main-thread getDisplayMedia capture.
describe('BroadcastPipeline media-source seam', () => {
  function makeFakeSource() {
    return {
      capturePath: 'mstp-worker' as const,
      stream: null,
      nativeFps: 42,
      onEnded: vi.fn(),
      startFrames: vi.fn(() => Promise.resolve()),
      stop: vi.fn(),
    };
  }

  function makeSeamPipeline(
    cbs: BroadcastCallbacks,
    source: ReturnType<typeof makeFakeSource>,
  ): BroadcastPipeline {
    return new BroadcastPipeline(
      { ...DEFAULT_CAPTURE_CONFIG },
      'https://relay.test:4433',
      {},
      cbs,
      undefined,
      undefined,
      async () => source,
    );
  }

  it('runs an injected stream-less source without firing onSourceStream', async () => {
    const fake = makeFakeWT([ANNOUNCE_K7XQ2M]);
    connectWebTransport.mockResolvedValue(fake.wt);
    const cbs = makeCallbacks();
    const source = makeFakeSource();
    const pipeline = makeSeamPipeline(cbs, source);
    await pipeline.start();
    expect(startCapture).not.toHaveBeenCalled();
    expect(cbs.onSourceStream).not.toHaveBeenCalled();
    expect(cbs.onCapturePathChosen).toHaveBeenCalledWith('mstp-worker');
    expect(source.startFrames).toHaveBeenCalled();
    expect(source.onEnded).toHaveBeenCalled();
    await pipeline.stop();
  });

  it('stops the injected source on teardown', async () => {
    const fake = makeFakeWT([ANNOUNCE_K7XQ2M]);
    connectWebTransport.mockResolvedValue(fake.wt);
    const source = makeFakeSource();
    const pipeline = makeSeamPipeline(makeCallbacks(), source);
    await pipeline.start();
    await pipeline.stop();
    expect(source.stop).toHaveBeenCalled();
    expect(fake.close).toHaveBeenCalled();
  });

  it('detects pipelineContext from the realm (window present ⇒ main-thread)', async () => {
    vi.useFakeTimers();
    try {
      const fake = makeFakeWT([ANNOUNCE_K7XQ2M]);
      connectWebTransport.mockResolvedValue(fake.wt);
      const cbs = makeCallbacks();
      const pipeline = makeSeamPipeline(cbs, makeFakeSource());
      await pipeline.start();
      await vi.advanceTimersByTimeAsync(500);
      expect(cbs.onStats).toHaveBeenCalled();
      expect(cbs.onStats.mock.calls[0][0].pipelineContext).toBe('main-thread');
      await pipeline.stop();
    } finally {
      vi.useRealTimers();
    }
  });

  it('detects pipelineContext "worker" when window is absent', async () => {
    vi.unstubAllGlobals(); // drop the window stub — a worker realm has none
    vi.useFakeTimers();
    try {
      const fake = makeFakeWT([ANNOUNCE_K7XQ2M]);
      connectWebTransport.mockResolvedValue(fake.wt);
      const cbs = makeCallbacks();
      const pipeline = makeSeamPipeline(cbs, makeFakeSource());
      await pipeline.start();
      await vi.advanceTimersByTimeAsync(500);
      expect(cbs.onStats).toHaveBeenCalled();
      expect(cbs.onStats.mock.calls[0][0].pipelineContext).toBe('worker');
      await pipeline.stop();
    } finally {
      vi.useRealTimers();
    }
  });
});
