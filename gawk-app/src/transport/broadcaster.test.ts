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
import { log } from '../lib/logger';
import { EncoderSupportProber } from '../media/probe';
import { DEFAULT_CAPTURE_CONFIG } from '../media/types';
import { CLOCK_MAPPING_INTERVAL_MS } from './time-sync';
import {
  RELAY_NOTICE_SCREENING,
  TYPE_CLOCK_MAPPING,
  TYPE_TIME_SYNC,
  encodeLocalRelayInfo,
  encodeRelayIdentity,
  encodeTimeSync,
  encodeViewerCount,
  parseClockMapping,
  parseTimeSync,
  CLOSE_CODE_BROADCASTER_STOPPED,
  CLOSE_CODE_TERMINATED_BY_OPERATOR,
} from './wire';

// Golden BroadcastAnnounce for ID K7XQ2M.
const ANNOUNCE_K7XQ2M = new Uint8Array([0x01, 0x03, 0x06, 0x4b, 0x37, 0x58, 0x51, 0x32, 0x4d]);

interface FakeWT {
  wt: WebTransport;
  close: ReturnType<typeof vi.fn>;
  // Settles `closed` like the relay ending the session.
  die: (info: unknown) => void;
}

// announceChunks: byte chunks delivered on the first server-initiated uni
// stream; null = the stream never arrives (but the connection stays up).
// moreStreams: further uni streams, one array of chunks each, after it.
function makeFakeWT(announceChunks: Uint8Array[] | null, moreStreams: Uint8Array[][] = []): FakeWT {
  let resolveClosed: (info: unknown) => void = () => {};
  const closed = new Promise((res) => {
    resolveClosed = res;
  });
  const incomingUnidirectionalStreams = new ReadableStream<ReadableStream<Uint8Array>>({
    start(controller) {
      for (const chunks of announceChunks ? [announceChunks, ...moreStreams] : moreStreams) {
        controller.enqueue(
          new ReadableStream<Uint8Array>({
            start(c) {
              for (const chunk of chunks) c.enqueue(chunk);
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
  return { wt, close, die: (info) => resolveClosed(info) };
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

// stop() while start() is still dialing runs teardown() with no session yet,
// so an unguarded dial resolves into a live publisher nobody owns (a zombie
// holding the broadcast ID) and goes on to consume the screen grant.
describe('BroadcastPipeline stop() racing the first dial', () => {
  it('closes the session the dial delivers and never starts capture', async () => {
    const fake = makeFakeWT([ANNOUNCE_K7XQ2M]);
    let resolveDial!: (wt: unknown) => void;
    connectWebTransport.mockReturnValue(new Promise((r) => (resolveDial = r)));
    startCapture.mockResolvedValue(makeCaptureHandle());
    const cbs = makeCallbacks();
    const pipeline = makePipeline(cbs);

    const started = pipeline.start();
    await pipeline.stop();
    expect(cbs.onEnded).toHaveBeenCalledTimes(1);

    resolveDial(fake.wt);
    await expect(started).resolves.toBeUndefined();
    expect(fake.close).toHaveBeenCalled();
    expect(startCapture).not.toHaveBeenCalled();
    expect(cbs.onEnded).toHaveBeenCalledTimes(1);
  });

  // The same race one await later: stop() lands while the pre-capture support
  // probe runs. teardown() closed the session, so capturing now would own a
  // stream nothing ever stops.
  it('does not start capture when stop() lands during the support probe', async () => {
    const fake = makeFakeWT([ANNOUNCE_K7XQ2M]);
    connectWebTransport.mockResolvedValue(fake.wt);
    startCapture.mockResolvedValue(makeCaptureHandle());
    let openGate!: () => void;
    const gate = new Promise<void>((r) => (openGate = r));
    const prober = new EncoderSupportProber(async () => {
      await gate;
      return { supported: false };
    });
    const cbs = makeCallbacks();
    const pipeline = new BroadcastPipeline(
      { ...DEFAULT_CAPTURE_CONFIG },
      'https://relay.test:4433',
      {},
      cbs,
      undefined,
      undefined,
      undefined,
      prober,
    );

    const started = pipeline.start();
    await vi.waitFor(() => expect(connectWebTransport).toHaveBeenCalled());
    await Promise.resolve();
    await pipeline.stop();
    openGate();
    await expect(started).resolves.toBeUndefined();
    expect(fake.close).toHaveBeenCalled();
    expect(startCapture).not.toHaveBeenCalled();
  });

  it('resolves quietly when the dial fails after stop()', async () => {
    let rejectDial!: (e: unknown) => void;
    connectWebTransport.mockReturnValue(new Promise((_, r) => (rejectDial = r)));
    const cbs = makeCallbacks();
    const pipeline = makePipeline(cbs, 'K7XQ2M');

    const started = pipeline.start();
    await pipeline.stop();
    rejectDial(new Error('Opening handshake failed.'));
    await expect(started).resolves.toBeUndefined();
    expect(cbs.onEnded).toHaveBeenCalledTimes(1);
  });

  it('drops a capture that arrives after stop() while the picker was open', async () => {
    vi.useFakeTimers();
    try {
      const fake = makeFakeWT([ANNOUNCE_K7XQ2M]);
      connectWebTransport.mockResolvedValue(fake.wt);
      const source = {
        capturePath: 'mstp' as const,
        stream: {} as MediaStream,
        nativeFps: 60,
        onEnded: vi.fn(),
        startFrames: vi.fn(() => Promise.resolve()),
        stop: vi.fn(),
      };
      let grant!: (s: typeof source) => void;
      const cbs = makeCallbacks();
      const pipeline = new BroadcastPipeline(
        { ...DEFAULT_CAPTURE_CONFIG },
        'https://relay.test:4433',
        {},
        cbs,
        undefined,
        undefined,
        () => new Promise((r) => (grant = r)),
      );

      const started = pipeline.start();
      await vi.waitFor(() => expect(grant).toBeDefined());
      await pipeline.stop();
      grant(source);
      await expect(started).resolves.toBeUndefined();
      await vi.advanceTimersByTimeAsync(2000);

      expect(source.stop).toHaveBeenCalled();
      expect(source.startFrames).not.toHaveBeenCalled();
      expect(cbs.onSourceStream).not.toHaveBeenCalled();
      expect(cbs.onStats).not.toHaveBeenCalled();
      expect(cbs.onEnded).toHaveBeenCalledTimes(1);
    } finally {
      vi.useRealTimers();
    }
  });
});

// docs/79 D9: only a deliberate Stop tells the relay to end the broadcast
// now (4014); every other teardown closes plainly, so the relay's grace keeps
// the viewers through what may be a reload or a blip.
describe('BroadcastPipeline stop() close code (R78)', () => {
  it('an end stop closes the session with 4014', async () => {
    const fake = makeFakeWT([ANNOUNCE_K7XQ2M]);
    connectWebTransport.mockResolvedValue(fake.wt);
    startCapture.mockResolvedValue(makeCaptureHandle());
    const cbs = makeCallbacks();
    const pipeline = makePipeline(cbs);
    await pipeline.start();
    await pipeline.stop({ end: true });
    expect(fake.close).toHaveBeenCalledTimes(1);
    expect(fake.close).toHaveBeenCalledWith({
      closeCode: CLOSE_CODE_BROADCASTER_STOPPED,
      reason: 'broadcaster stopped',
    });
    expect(cbs.onEnded).toHaveBeenCalledTimes(1);
  });

  it('a plain stop closes the session without a code', async () => {
    const fake = makeFakeWT([ANNOUNCE_K7XQ2M]);
    connectWebTransport.mockResolvedValue(fake.wt);
    startCapture.mockResolvedValue(makeCaptureHandle());
    const pipeline = makePipeline(makeCallbacks());
    await pipeline.start();
    await pipeline.stop();
    expect(fake.close).toHaveBeenCalledTimes(1);
    expect(fake.close.mock.calls[0]).toEqual([]);
  });

  it('a failure closes the session without a code', async () => {
    const fake = makeFakeWT([ANNOUNCE_K7XQ2M]);
    connectWebTransport.mockResolvedValue(fake.wt);
    startCapture.mockResolvedValue(makeCaptureHandle());
    const cbs = makeCallbacks();
    const pipeline = makePipeline(cbs);
    await pipeline.start();
    // A terminal relay close: fail() stops the pipeline.
    fake.die({ closeCode: CLOSE_CODE_TERMINATED_BY_OPERATOR, reason: '' });
    await vi.waitFor(() => expect(cbs.onEnded).toHaveBeenCalledTimes(1));
    expect(cbs.onError).toHaveBeenCalledTimes(1);
    expect(fake.close).toHaveBeenCalledTimes(1);
    expect(fake.close.mock.calls[0]).toEqual([]);
    // An end stop after the failure is a no-op: the session is already gone.
    await pipeline.stop({ end: true });
    expect(fake.close).toHaveBeenCalledTimes(1);
  });

  // stop() raced the first dial: the late session is the user's broadcast,
  // and the user asked to end it.
  it('an end stop racing the first dial closes the late session with 4014', async () => {
    const fake = makeFakeWT([ANNOUNCE_K7XQ2M]);
    let resolveDial!: (wt: unknown) => void;
    connectWebTransport.mockReturnValue(new Promise((r) => (resolveDial = r)));
    const pipeline = makePipeline(makeCallbacks());
    const started = pipeline.start();
    await pipeline.stop({ end: true });
    resolveDial(fake.wt);
    await started;
    expect(fake.close).toHaveBeenCalledWith({
      closeCode: CLOSE_CODE_BROADCASTER_STOPPED,
      reason: 'broadcaster stopped',
    });
  });
});

describe('BroadcastPipeline URLs', () => {
  it('dials /publish when minting', async () => {
    const fake = makeFakeWT([ANNOUNCE_K7XQ2M]);
    connectWebTransport.mockResolvedValue(fake.wt);
    startCapture.mockResolvedValue(makeCaptureHandle());
    const pipeline = makePipeline(makeCallbacks());
    await pipeline.start();
    expect(connectWebTransport).toHaveBeenCalledWith('https://relay.test:4433/publish?app=web', {});
    await pipeline.stop();
  });

  it('dials /publish/<id> when reclaiming', async () => {
    const fake = makeFakeWT([ANNOUNCE_K7XQ2M]);
    connectWebTransport.mockResolvedValue(fake.wt);
    startCapture.mockResolvedValue(makeCaptureHandle());
    const pipeline = makePipeline(makeCallbacks(), 'K7XQ2M');
    await pipeline.start();
    expect(connectWebTransport).toHaveBeenCalledWith('https://relay.test:4433/publish/K7XQ2M?app=web', {});
    await pipeline.stop();
  });

  // Publishing requires the pre-shared secret; it travels as a query
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
    expect(connectWebTransport).toHaveBeenCalledWith('https://relay.test:4433/publish?secret=s3cret&app=web', opts);
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
    expect(connectWebTransport).toHaveBeenCalledWith('https://relay.test:4433/publish/K7XQ2M?secret=s3cret&app=web', opts);
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
    // Capture/encode begins immediately; only the UI code display waits for
    // the announce.
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

// R40 (docs/71 D13): a relay with -screening-notice sends its RelayIdentity
// on the publish session, for the native broadcasters. This page shows a
// server's screening in the server picker instead, so the pipeline reads the
// stream and drops it: no warning, and nothing disturbed.
describe('BroadcastPipeline relay identity (R40)', () => {
  it("drops a screening relay's identity quietly", async () => {
    const warn = vi.spyOn(log, 'warn').mockImplementation(() => {});
    const identity = encodeRelayIdentity({
      serverVersion: '1.42.0',
      name: 'gawk home',
      notices: RELAY_NOTICE_SCREENING,
    });
    // The identity first, then the announce: once the code is out and a
    // macrotask has passed, both streams have been read (they're in memory).
    const fake = makeFakeWT([identity.subarray(0, 5), identity.subarray(5)], [[ANNOUNCE_K7XQ2M]]);
    connectWebTransport.mockResolvedValue(fake.wt);
    startCapture.mockResolvedValue(makeCaptureHandle());
    const cbs = makeCallbacks();
    const pipeline = makePipeline(cbs);
    await pipeline.start();
    await vi.waitFor(() => expect(cbs.onBroadcastId).toHaveBeenCalledWith('K7XQ2M'));
    await new Promise((r) => setTimeout(r, 0));
    expect(warn).not.toHaveBeenCalled();
    expect(cbs.onError).not.toHaveBeenCalled();
    expect(cbs.onEnded).not.toHaveBeenCalled();
    await pipeline.stop();
    warn.mockRestore();
  });
});

// R74 (docs/76 §6.12): a PoP says what it is on the publish session (0x18),
// and home may offer a PoP (0x19). The pipeline hands both out; the screen
// decides what they mean.
describe('BroadcastPipeline local relay messages (R74)', () => {
  it('hands LocalRelayInfo to onLocalRelay without a warning', async () => {
    const warn = vi.spyOn(log, 'warn').mockImplementation(() => {});
    const info = { linked: true, onWall: true, name: 'Assembly 2026', homeOrigin: 'https://gawk.ioio.fi', wallCode: 'assembly-2026' };
    const fake = makeFakeWT([encodeLocalRelayInfo(info)], [[ANNOUNCE_K7XQ2M]]);
    connectWebTransport.mockResolvedValue(fake.wt);
    startCapture.mockResolvedValue(makeCaptureHandle());
    const onLocalRelay = vi.fn<NonNullable<BroadcastCallbacks['onLocalRelay']>>();
    const pipeline = makePipeline({ ...makeCallbacks(), onLocalRelay });
    await pipeline.start();
    await vi.waitFor(() => expect(onLocalRelay).toHaveBeenCalledWith({ kind: 'info', info }));
    expect(warn).not.toHaveBeenCalled();
    await pipeline.stop();
    warn.mockRestore();
  });

  it('says lr=1 when it will follow an offer, and wall=hidden for a remembered Hide', async () => {
    const fake = makeFakeWT([ANNOUNCE_K7XQ2M]);
    connectWebTransport.mockResolvedValue(fake.wt);
    startCapture.mockResolvedValue(makeCaptureHandle());
    const opts = { followOffers: true, wallHidden: true };
    const pipeline = new BroadcastPipeline({ ...DEFAULT_CAPTURE_CONFIG }, 'https://relay.test:4433', opts, makeCallbacks());
    await pipeline.start();
    const url = new URL(connectWebTransport.mock.calls[0][0] as string);
    expect(url.searchParams.get('lr')).toBe('1');
    expect(url.searchParams.get('wall')).toBe('hidden');
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

// The broadcaster pings the relay for clock sync and, once
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

// The relay pushes the live viewer count as a
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

// The media-source seam that lets the broadcast worker inject
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
