// The browser broadcaster in Reliable mode (docs/88 BR2): the real
// KeyframeSender, CarrierSender and DatagramSender under a BroadcastPipeline,
// with a fake WebTransport that records every uni stream's bytes in write
// order (the order QUIC delivers them) and every datagram, and whose
// server-message streams the test feeds — capabilities arrive when it says.
// Fake timers; the pipeline's clock injected.
//
// Mocks for capture, preprocess and the encoder mirror
// broadcaster-uplink.test.ts.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import {
  CarrierRecordParser,
  parseStreamFrameHeader,
  parseVideoChunk,
  STREAM_FRAME_HEADER_SIZE,
  TYPE_KEYFRAME_CARRIER,
  TYPE_PARITY_CHUNK,
  TYPE_STREAM_FRAME,
  TYPE_VIDEO_CHUNK,
} from './wire';
import { CAP_KEYFRAME_CARRIERS, CAP_PARITY_CHUNKS, CAP_UPLINK_CARRIERS, encodeRelayCapabilities } from './parity';
import { ABRUPT_DROP_RETRY_DELAY_MS } from './reconnect';

const connectWebTransport = vi.fn();
const startCapture = vi.fn();

const h = vi.hoisted(() => ({
  frameCb: { value: null as null | ((frame: unknown) => void) },
  encoderCbs: { value: null as null | { onEncoded: (e: unknown) => void; onError: (e: Error) => void } },
  forced: { value: 0 },
}));

vi.mock('./connection', async (importOriginal) => {
  const actual = await importOriginal<typeof import('./connection')>();
  return {
    ...actual,
    connectWebTransport: (...args: unknown[]) => connectWebTransport(...args),
    readDatagrams: () => new Promise(() => {}),
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
      h.forced.value++;
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

import { BroadcastPipeline, type BroadcastCallbacks, type BroadcastStats } from './broadcaster';
import { DEFAULT_CAPTURE_CONFIG } from '../media/types';


// Announce for K7XQ2M + a resume token, so a session death resumes.
const ANNOUNCE = new Uint8Array([0x01, 0x03, 0x06, 0x4b, 0x37, 0x58, 0x51, 0x32, 0x4d]);
const TOKEN = new Uint8Array([0x01, 0x09, 0x10, ...new Uint8Array(16).fill(7)]);

const CHROME_UA =
  'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/151.0.0.0 Safari/537.36';
const FIREFOX_UA = 'Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:140.0) Gecko/20100101 Firefox/140.0';

const BOTH = CAP_PARITY_CHUNKS | CAP_UPLINK_CARRIERS | CAP_KEYFRAME_CARRIERS;
const caps = (flags: number) => encodeRelayCapabilities({ flags, parityLevel: 2 });

interface FakeStreamRec {
  chunks: Uint8Array[];
  closed: boolean;
  aborted: boolean;
  acked: boolean;
}

interface FakeWT {
  wt: WebTransport;
  datagrams: Uint8Array[];
  streams: FakeStreamRec[];
  // Delivers a server message (e.g. RelayCapabilities) now.
  serve(msg: Uint8Array): void;
  // Acknowledgements wait until released (holdAcks), else are immediate.
  holdAcks(): void;
  releaseAcks(): void;
  die(err: unknown): void;
}

function makeFakeWT(initial: Uint8Array[] = []): FakeWT {
  const datagrams: Uint8Array[] = [];
  const streams: FakeStreamRec[] = [];
  let ctrl!: ReadableStreamDefaultController<ReadableStream<Uint8Array>>;
  let settleClosed: (e: unknown) => void = () => {};
  const closed = new Promise((_res, rej) => {
    settleClosed = rej;
  });
  closed.catch(() => {});
  let holding = false;
  let held: (() => void)[] = [];
  const serveOne = (msg: Uint8Array) =>
    ctrl.enqueue(
      new ReadableStream<Uint8Array>({
        start(c) {
          c.enqueue(msg);
          c.close();
        },
      }),
    );
  const fake: FakeWT = {
    wt: undefined as unknown as WebTransport,
    datagrams,
    streams,
    serve: serveOne,
    holdAcks: () => {
      holding = true;
    },
    releaseAcks: () => {
      holding = false;
      for (const r of held.splice(0)) r();
    },
    die: (err) => settleClosed(err),
  };
  fake.wt = {
    closed,
    incomingUnidirectionalStreams: new ReadableStream<ReadableStream<Uint8Array>>({
      start(controller) {
        ctrl = controller;
        for (const msg of initial) serveOne(msg);
      },
    }),
    datagrams: {
      maxDatagramSize: 1200,
      writable: new WritableStream<Uint8Array>({
        write(chunk) {
          datagrams.push(chunk);
        },
      }),
    },
    createUnidirectionalStream: () => {
      const rec: FakeStreamRec = { chunks: [], closed: false, aborted: false, acked: false };
      streams.push(rec);
      const writer = {
        write: (chunk: Uint8Array) => {
          if (rec.aborted || rec.closed) return Promise.reject(new Error('closed'));
          rec.chunks.push(chunk);
          return Promise.resolve();
        },
        close: () => {
          rec.closed = true;
          return new Promise<void>((resolve) => {
            const ack = () => {
              rec.acked = true;
              resolve();
            };
            if (holding) held.push(ack);
            else ack();
          });
        },
        abort: () => {
          rec.aborted = true;
          return Promise.resolve();
        },
      };
      return Promise.resolve({ getWriter: () => writer });
    },
    close: vi.fn(),
  } as unknown as WebTransport;
  return fake;
}

function concat(chunks: Uint8Array[]): Uint8Array {
  const out = new Uint8Array(chunks.reduce((a, c) => a + c.length, 0));
  let o = 0;
  for (const c of chunks) {
    out.set(c, o);
    o += c.length;
  }
  return out;
}

// A stream's content: its kind, the keyframe's frameId, and the records after
// the keyframe (GOP streams only).
function readStream(rec: FakeStreamRec): { kind: 'gop' | 'keyframe'; keyframeId: number; records: Uint8Array[] } {
  const bytes = concat(rec.chunks);
  if (bytes[1] === TYPE_STREAM_FRAME) {
    return { kind: 'keyframe', keyframeId: parseStreamFrameHeader(bytes).frameId, records: [] };
  }
  expect(bytes[1]).toBe(TYPE_KEYFRAME_CARRIER);
  const sf = bytes.subarray(2);
  const hdr = parseStreamFrameHeader(sf);
  expect(hdr.keyframe).toBe(true);
  const rest = sf.subarray(STREAM_FRAME_HEADER_SIZE + hdr.configLen + hdr.payloadLen);
  const parser = new CarrierRecordParser();
  return { kind: 'gop', keyframeId: hdr.frameId, records: parser.push(rest) };
}

function makeCallbacks() {
  return {
    onSourceStream: vi.fn(),
    onEncoderConfigured: vi.fn(),
    onCapturePathChosen: vi.fn(),
    onStats: vi.fn<BroadcastCallbacks['onStats']>(),
    onError: vi.fn<BroadcastCallbacks['onError']>(),
    onEnded: vi.fn(),
    onBroadcastId: vi.fn(),
    onResumeToken: vi.fn(),
    onReconnecting: vi.fn(),
    onResumed: vi.fn(),
  } satisfies BroadcastCallbacks;
}

const at = async (t: number) => vi.advanceTimersByTimeAsync(Math.max(0, t - Date.now()));

async function start(link: FakeWT, delivery: 'reliable' | 'lowest-latency' = 'reliable', ua = CHROME_UA) {
  vi.stubGlobal('navigator', { userAgent: ua });
  const cbs = makeCallbacks();
  connectWebTransport.mockResolvedValueOnce(link.wt);
  startCapture.mockResolvedValue({
    stream: {} as MediaStream,
    track: { getSettings: () => ({ frameRate: 60 }), addEventListener: vi.fn() },
    capturePath: 'fake',
    startFrames: (cb: (frame: unknown) => void) => {
      h.frameCb.value = cb;
      return Promise.resolve();
    },
  });
  const pipeline = new BroadcastPipeline({ ...DEFAULT_CAPTURE_CONFIG }, 'https://relay.test:4433', {}, cbs, undefined, () =>
    Date.now(),
  );
  pipeline.setDelivery(delivery);
  await pipeline.start();
  h.frameCb.value!({
    timestamp: 0,
    displayWidth: 1920,
    displayHeight: 1080,
    codedWidth: 1920,
    codedHeight: 1080,
    close: vi.fn(),
  });
  await vi.advanceTimersByTimeAsync(0);
  return { pipeline, cbs };
}

// One encoded frame; its payload is `size` bytes of its index, so the wire
// says which frame it was.
function emit(type: 'key' | 'delta', index: number, size = 2500) {
  const bytes = new Uint8Array(size).fill(index & 0xff);
  h.encoderCbs.value!.onEncoded({
    chunk: { type, timestamp: index * 16_667, byteLength: bytes.length, copyTo: (d: Uint8Array) => d.set(bytes) },
    meta:
      index === 0
        ? { decoderConfig: { codec: 'avc1.42E01F', description: new Uint8Array([0x01, 0x42, 0xe0, 0x1f]) } }
        : undefined,
    encodeStartMs: 0,
    encodeEndMs: 1,
  });
}

async function play(from: number, to: number, keys: (i: number) => boolean = (i) => i % 30 === 0) {
  for (let i = from; i < to; i++) {
    await at(Math.round((i * 1000) / 60));
    emit(keys(i) ? 'key' : 'delta', i);
  }
  await vi.advanceTimersByTimeAsync(0);
}

async function lastStats(cbs: ReturnType<typeof makeCallbacks>): Promise<BroadcastStats> {
  await vi.advanceTimersByTimeAsync(600);
  return cbs.onStats.mock.calls.at(-1)![0];
}

const deltaIdsIn = (dgrams: Uint8Array[]) =>
  new Set(dgrams.filter((d) => d[1] === TYPE_VIDEO_CHUNK).map((d) => parseVideoChunk(d).header.frameId));

beforeEach(() => {
  vi.stubGlobal('window', globalThis);
  vi.useFakeTimers();
  vi.setSystemTime(0);
  connectWebTransport.mockReset();
  startCapture.mockReset();
  h.frameCb.value = null;
  h.encoderCbs.value = null;
  h.forced.value = 0;
});

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe('BroadcastPipeline — Reliable on a relay with both bits (docs/88 D1, D2)', () => {
  it('each GOP rides one stream, keyframe first, its deltas as records; no video datagrams', async () => {
    const link = makeFakeWT([caps(BOTH)]);
    const { pipeline, cbs } = await start(link);
    await play(0, 90);
    const stats = await lastStats(cbs);

    expect(link.streams.length).toBe(3);
    const gops = link.streams.map(readStream);
    expect(gops.map((g) => [g.kind, g.keyframeId])).toEqual([
      ['gop', 0],
      ['gop', 30],
      ['gop', 60],
    ]);
    for (const [n, g] of gops.entries()) {
      const ids = new Set(g.records.filter((r) => r[1] === TYPE_VIDEO_CHUNK).map((r) => parseVideoChunk(r).header.frameId));
      for (let i = 30 * n + 1; i < 30 * n + 30; i++) expect(ids.has(i), `GOP ${n} delta ${i}`).toBe(true);
    }
    expect(deltaIdsIn(link.datagrams).size).toBe(0);
    expect(link.streams.slice(0, 2).every((s) => s.closed && s.acked && !s.aborted)).toBe(true);
    expect(stats).toMatchObject({
      uplinkModeRequested: 'reliable',
      uplinkMode: 'carrier',
      uplinkCarriersOpened: 3,
      uplinkCarriersExpired: 0,
      uplinkRecordsShed: 0,
      staleFramesDropped: 0,
      droppedFrames: 0,
    });
    await pipeline.stop();
  });

  it('the records are byte for byte the datagrams Lowest latency sends for the same frames, parity included', async () => {
    const reliable = makeFakeWT([caps(BOTH)]);
    const r = await start(reliable, 'reliable');
    await play(0, 60);
    await r.pipeline.stop();
    const records = reliable.streams.flatMap((s) => readStream(s).records);

    vi.setSystemTime(0);
    const lowest = makeFakeWT([caps(BOTH)]);
    const l = await start(lowest, 'lowest-latency');
    await play(0, 60);
    await l.pipeline.stop();
    const media = lowest.datagrams.filter((d) => d[1] === TYPE_VIDEO_CHUNK || d[1] === TYPE_PARITY_CHUNK);

    expect(records.length).toBeGreaterThan(58);
    expect(records).toEqual(media);
  });

  it('a frame goes to the stream in one write', async () => {
    const link = makeFakeWT([caps(BOTH)]);
    const { pipeline } = await start(link);
    await play(0, 5);
    // Keyframe write, then one write per delta.
    expect(link.streams[0].chunks.length).toBe(1 + 4);
    await pipeline.stop();
  });
});

describe('BroadcastPipeline — what does not engage (docs/88 D1, D6, G6)', () => {
  // Today's path, byte for byte: keyframe streams and datagrams.
  async function datagramRun(link: FakeWT, delivery: 'reliable' | 'lowest-latency', ua = CHROME_UA) {
    const { pipeline } = await start(link, delivery, ua);
    await play(0, 60);
    await pipeline.stop();
    // Media only: TimeSync pings carry the clock and differ run to run.
    const media = link.datagrams.filter((d) => d[1] === TYPE_VIDEO_CHUNK || d[1] === TYPE_PARITY_CHUNK);
    return { datagrams: media, streams: link.streams.map((s) => concat(s.chunks)) };
  }

  it.each([
    ['an RU1-only relay (no keyframe-carrier bit)', CAP_PARITY_CHUNKS | CAP_UPLINK_CARRIERS, 'reliable', CHROME_UA],
    ['a relay with carriers off', CAP_PARITY_CHUNKS, 'reliable', CHROME_UA],
    ['Lowest latency', BOTH, 'lowest-latency', CHROME_UA],
    ['Firefox (OD4)', BOTH, 'reliable', FIREFOX_UA],
  ] as const)('%s: today\'s keyframe streams and datagrams, byte for byte', async (_name, flags, delivery, ua) => {
    const got = await datagramRun(makeFakeWT([caps(flags)]), delivery, ua);
    vi.setSystemTime(0);
    const today = await datagramRun(makeFakeWT([caps(flags)]), 'lowest-latency', CHROME_UA);
    expect(got.streams.every((s) => s[1] === TYPE_STREAM_FRAME)).toBe(true);
    expect(got).toEqual(today);
  });

  it("the first GOP goes as datagrams when the relay's capabilities arrive after its keyframe", async () => {
    const link = makeFakeWT();
    const { pipeline, cbs } = await start(link);
    await play(0, 10);
    link.serve(caps(BOTH));
    await play(10, 60);
    const stats = await lastStats(cbs);
    expect(readStream(link.streams[0]).kind).toBe('keyframe');
    expect(readStream(link.streams[1]).kind).toBe('gop');
    const dgrams = deltaIdsIn(link.datagrams);
    for (let i = 1; i < 30; i++) expect(dgrams.has(i), `delta ${i} as a datagram`).toBe(true);
    for (let i = 31; i < 60; i++) expect(dgrams.has(i), `delta ${i}`).toBe(false);
    expect(stats.uplinkMode).toBe('carrier');
    await pipeline.stop();
  });

  it('a change of setting applies at the next keyframe, live, without a restart', async () => {
    const link = makeFakeWT([caps(BOTH)]);
    const { pipeline, cbs } = await start(link, 'lowest-latency');
    await play(0, 15);
    pipeline.setDelivery('reliable'); // mid-GOP: this GOP stays on datagrams
    await play(15, 45);
    let stats = await lastStats(cbs);
    expect(stats.uplinkModeRequested).toBe('reliable');
    expect(link.streams.map((s) => readStream(s).kind)).toEqual(['keyframe', 'gop']);
    const dgrams = deltaIdsIn(link.datagrams);
    expect(dgrams.has(20)).toBe(true);
    expect(dgrams.has(40)).toBe(false);

    pipeline.setDelivery('lowest-latency');
    await play(45, 75);
    stats = await lastStats(cbs);
    expect(link.streams.map((s) => readStream(s).kind)).toEqual(['keyframe', 'gop', 'keyframe']);
    expect(deltaIdsIn(link.datagrams).has(70)).toBe(true);
    expect(stats.uplinkMode).toBe('datagram');
    await pipeline.stop();
  });
});

describe('BroadcastPipeline — Reliable lifecycle (docs/88 D3)', () => {
  it('a GOP whose stream is still unacknowledged when a third keyframe comes: that keyframe waits, its deltas are shed and counted', async () => {
    const link = makeFakeWT([caps(BOTH)]);
    const { pipeline, cbs } = await start(link);
    link.holdAcks(); // nothing is acknowledged: two GOP streams fill Chrome
    await play(0, 90);
    const stats = await lastStats(cbs);
    expect(link.streams.length).toBe(2); // GOP 60's keyframe waits
    expect(stats.uplinkRecordsShed).toBeGreaterThan(0);
    expect(stats.staleFramesDropped).toBe(0); // the lane's count, not RU9's datagram count
    expect(link.streams.every((s) => !s.aborted)).toBe(true);
    link.releaseAcks(); // the acknowledgements free Chrome: 60 starts, then 90 on its handover
    await play(90, 92);
    expect(link.streams.map((st) => readStream(st).keyframeId)).toEqual([0, 30, 60, 90]);
    await pipeline.stop();
  });

  it('a GOP stream unacknowledged 2 s after it opened is aborted when a keyframe comes, and counted as expired', async () => {
    const link = makeFakeWT([caps(BOTH)]);
    const { pipeline, cbs } = await start(link);
    link.holdAcks();
    await play(0, 160);
    const stats = await lastStats(cbs);
    expect(link.streams[0].aborted).toBe(true);
    expect(stats.uplinkCarriersExpired).toBeGreaterThanOrEqual(1);
    expect(stats.keyframeStreamsFailed).toBe(stats.uplinkCarriersExpired);
    await pipeline.stop();
  });

  it('the upload notice: five bad seconds of shed records raise it (docs/88 D5)', async () => {
    const link = makeFakeWT([caps(BOTH)]);
    const { pipeline, cbs } = await start(link);
    link.holdAcks(); // the link never acknowledges: GOPs are shed, keyframes wait
    await play(0, 60 * 3);
    expect(cbs.onStats.mock.calls.at(-1)![0].uplinkWarning).toBe(false);
    await play(60 * 3, 60 * 8);
    expect(cbs.onStats.mock.calls.at(-1)![0].uplinkWarning).toBe(true);
    await pipeline.stop();
  });
});

describe('BroadcastPipeline — Reliable resume (docs/88 BR2)', () => {
  it('a new session starts with no stream, sends nothing of the old one, and its forced keyframe opens the first GOP stream once the capability is known', async () => {
    const first = makeFakeWT([ANNOUNCE, TOKEN, caps(BOTH)]);
    const { pipeline, cbs } = await start(first);
    await play(0, 40);
    first.holdAcks();

    const second = makeFakeWT([caps(BOTH)]);
    connectWebTransport.mockResolvedValueOnce(second.wt);
    first.die(new Error('connection lost'));
    await vi.advanceTimersByTimeAsync(ABRUPT_DROP_RETRY_DELAY_MS + 10);
    expect(cbs.onResumed).toHaveBeenCalledTimes(1);
    expect(h.forced.value).toBe(1);

    // frameId = index: frames emitted while the session was down never got one.
    emit('delta', 40); // GOP 30's: its stream died with the old session
    emit('key', 41); // the forced keyframe
    emit('delta', 42);
    await vi.advanceTimersByTimeAsync(20);
    expect(second.streams.length).toBe(1);
    const g = readStream(second.streams[0]);
    expect(g.kind).toBe('gop');
    expect(g.keyframeId).toBe(41);
    const ids = g.records.filter((r) => r[1] === TYPE_VIDEO_CHUNK).map((r) => parseVideoChunk(r).header.frameId);
    expect([...new Set(ids)]).toEqual([42]);
    expect(deltaIdsIn(second.datagrams).size).toBe(0);
    await pipeline.stop();
  });
});
