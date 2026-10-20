// The browser broadcaster on a slow uplink (docs/75 RU9; BUGS.md "a slow
// uplink becomes unbounded delay, not dropped frames"). The real
// DatagramSender and keyframe state machine under a BroadcastPipeline, with a
// fake WebTransport whose datagram writer can stall and whose keyframe
// streams hand over and acknowledge when the test says — all on fake timers,
// the pipeline's clock injected.
//
// Mocks for capture, preprocess and the encoder mirror
// broadcaster-keyframe.test.ts.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import {
  parseStreamFrameHeader,
  parseVideoChunk,
  TYPE_PARITY_CHUNK,
  TYPE_VIDEO_CHUNK,
} from './wire';
import { CAP_PARITY_CHUNKS, encodeRelayCapabilities } from './parity';
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

// When a keyframe stream (by frameId) hands over and is acknowledged, in ms
// after its write() is called; absent = at once.
interface StreamPlan {
  handOverAfterMs?: number;
  ackAfterMs?: number;
  never?: boolean; // write() never resolves
}

interface FakeWT {
  wt: WebTransport;
  // Every datagram the writer accepted, in order.
  datagrams: Uint8Array[];
  // frameIds of keyframe streams whose close() resolved (acknowledged).
  acked: number[];
  // frameIds written to a keyframe stream (handed to write()).
  opened: number;
  stallNextDatagram(ms: number): void;
  slowDatagrams(ms: number): void;
  plans: Map<number, StreamPlan>;
  die(err: unknown): void;
}

function makeFakeWT(serverMessages: Uint8Array[] = []): FakeWT {
  const datagrams: Uint8Array[] = [];
  const acked: number[] = [];
  const plans = new Map<number, StreamPlan>();
  let stall = 0;
  let slow = 0;
  let settleClosed: (e: unknown) => void = () => {};
  const closed = new Promise((_res, rej) => {
    settleClosed = rej;
  });
  closed.catch(() => {});
  const writable = new WritableStream<Uint8Array>({
    async write(chunk) {
      const ms = stall || slow;
      stall = 0;
      if (ms > 0) await new Promise((r) => setTimeout(r, ms));
      datagrams.push(chunk);
    },
  });
  const fake: FakeWT = {
    wt: undefined as unknown as WebTransport,
    datagrams,
    acked,
    opened: 0,
    stallNextDatagram: (ms) => {
      stall = ms;
    },
    slowDatagrams: (ms) => {
      slow = ms;
    },
    plans,
    die: (err) => settleClosed(err),
  };
  fake.wt = {
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
    datagrams: { maxDatagramSize: 1200, writable },
    createUnidirectionalStream: () => {
      fake.opened++;
      let frameId = -1;
      let aborted: (e: unknown) => void = () => {};
      const abortP = new Promise<never>((_r, rej) => {
        aborted = rej;
      });
      abortP.catch(() => {});
      const after = (ms: number | undefined) =>
        Promise.race([new Promise<void>((r) => setTimeout(r, ms ?? 0)), abortP]);
      const writer = {
        write: (msg: Uint8Array) => {
          frameId = parseStreamFrameHeader(msg).frameId;
          const plan = plans.get(frameId) ?? {};
          if (plan.never) return Promise.race([new Promise<void>(() => {}), abortP]);
          return after(plan.handOverAfterMs);
        },
        close: () =>
          after(plans.get(frameId)?.ackAfterMs).then(() => {
            acked.push(frameId);
          }),
        abort: () => {
          aborted(new Error('aborted'));
          return Promise.resolve();
        },
      };
      return Promise.resolve({ getWriter: () => writer });
    },
    close: vi.fn(),
  } as unknown as WebTransport;
  return fake;
}

function makeCaptureHandle() {
  return {
    stream: {} as MediaStream,
    track: { getSettings: () => ({ frameRate: 60 }), addEventListener: vi.fn() },
    capturePath: 'fake',
    startFrames: (cb: (frame: unknown) => void) => {
      h.frameCb.value = cb;
      return Promise.resolve();
    },
  };
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

async function start(link: FakeWT) {
  const cbs = makeCallbacks();
  connectWebTransport.mockResolvedValueOnce(link.wt);
  startCapture.mockResolvedValue(makeCaptureHandle());
  const pipeline = new BroadcastPipeline({ ...DEFAULT_CAPTURE_CONFIG }, 'https://relay.test:4433', {}, cbs, undefined, () =>
    Date.now(),
  );
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
  expect(h.encoderCbs.value).not.toBeNull();
  return { pipeline, cbs };
}

// Emits one encoded frame; its payload byte is its index, so the wire says
// which frame it was (frameIds continue across the priming frame).
function emit(type: 'key' | 'delta', index: number) {
  const bytes = new Uint8Array(200).fill(index & 0xff);
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

// Video frameIds of every delta datagram written (data chunks only).
function deltaIds(link: FakeWT): Set<number> {
  const out = new Set<number>();
  for (const d of link.datagrams) if (d[1] === TYPE_VIDEO_CHUNK) out.add(parseVideoChunk(d).header.frameId);
  return out;
}

// Emits frames 0..count-1 at 60 fps from t=0, a keyframe every 30 frames
// unless `keys` says otherwise.
async function play(count: number, keys: (i: number) => boolean = (i) => i % 30 === 0) {
  for (let i = 0; i < count; i++) {
    await at(Math.round((i * 1000) / 60));
    emit(keys(i) ? 'key' : 'delta', i);
  }
}

async function lastStats(cbs: ReturnType<typeof makeCallbacks>): Promise<BroadcastStats> {
  await vi.advanceTimersByTimeAsync(600);
  return cbs.onStats.mock.calls.at(-1)![0];
}

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

describe('BroadcastPipeline — the boundary (RU9)', () => {
  it('a datagram writer stalled 400 ms drops nothing; keyframe streams complete at once', async () => {
    const link = makeFakeWT();
    const { pipeline, cbs } = await start(link);
    // frameId = index here (the priming frame produced no output).
    link.stallNextDatagram(400);
    await play(90);
    const stats = await lastStats(cbs);

    const sent = deltaIds(link);
    for (let i = 1; i < 90; i++) if (i % 30 !== 0) expect(sent.has(i), `delta ${i}`).toBe(true);
    expect(link.acked).toEqual([0, 30, 60]);
    expect(stats.staleFramesDropped).toBe(0);
    expect(stats.droppedFrames).toBe(0);
    await pipeline.stop();
  });

  it('stalled 600 ms, only the frames past the bound and the rest of their GOP are dropped', async () => {
    const link = makeFakeWT();
    const { pipeline, cbs } = await start(link);
    link.stallNextDatagram(600); // delta 1 (t≈17) holds the writer until ≈617
    await play(90);
    const stats = await lastStats(cbs);

    const sent = deltaIds(link);
    expect(sent.has(1)).toBe(true);
    for (let i = 2; i < 30; i++) expect(sent.has(i), `delta ${i}`).toBe(false);
    for (let i = 31; i < 90; i++) if (i % 30 !== 0) expect(sent.has(i), `delta ${i}`).toBe(true);
    expect(link.acked).toEqual([0, 30, 60]);
    expect(stats.staleFramesDropped).toBe(28);
    expect(stats.droppedFrames).toBe(0);
    await pipeline.stop();
  });
});

describe('BroadcastPipeline — a drop is counted as stale, never as sent (RU9)', () => {
  it('dropped frames move none of the sent counters and none of the failure ones', async () => {
    const link = makeFakeWT([encodeRelayCapabilities({ flags: CAP_PARITY_CHUNKS, parityLevel: 1 })]);
    const { pipeline, cbs } = await start(link);
    await vi.advanceTimersByTimeAsync(0); // capabilities read
    link.stallNextDatagram(600);
    await play(60);
    await at(1500);
    const stats = await lastStats(cbs);

    const video = link.datagrams.filter((d) => d[1] === TYPE_VIDEO_CHUNK);
    const parity = link.datagrams.filter((d) => d[1] === TYPE_PARITY_CHUNK);
    expect(parity.length).toBeGreaterThan(0);
    expect(stats.datagramsSent).toBe(video.length);
    expect(stats.parityChunksSent).toBe(parity.length);
    let bytes = 0;
    for (const d of video) bytes += d.length;
    // bytesSent = delta data bytes + the two keyframe streams' bytes.
    expect(stats.bytesSent - stats.keyframeBytesSent).toBe(bytes);
    expect(stats.staleFramesDropped).toBe(28);
    expect(stats.droppedFrames).toBe(0);
    expect(cbs.onError).not.toHaveBeenCalled();
    await pipeline.stop();
  });
});

describe('BroadcastPipeline — deltas give way to a late keyframe (RU9)', () => {
  // Keyframe n-1 is frame 0 (acknowledged at once), keyframe n frame 30
  // (t=500), keyframe n+1 frame 60 (t=1000); the datagram writer keeps up.
  it('keyframe n handed over 400 ms after production: n+1 does not wait and nothing is dropped', async () => {
    const link = makeFakeWT();
    const { pipeline, cbs } = await start(link);
    link.plans.set(30, { handOverAfterMs: 400 });
    await play(90);
    const stats = await lastStats(cbs);

    const sent = deltaIds(link);
    for (let i = 1; i < 90; i++) if (i % 30 !== 0) expect(sent.has(i), `delta ${i}`).toBe(true);
    expect(link.acked).toEqual([0, 30, 60]);
    expect(stats.staleFramesDropped).toBe(0);
    await pipeline.stop();
  });

  it("handed over at 600 ms: every delta of GOP n+1 is dropped (its keyframe still goes out), GOP n's are not", async () => {
    const link = makeFakeWT();
    const { pipeline, cbs } = await start(link);
    link.plans.set(30, { handOverAfterMs: 600 });
    await play(90);
    const stats = await lastStats(cbs);

    const sent = deltaIds(link);
    for (let i = 31; i < 60; i++) expect(sent.has(i), `GOP n delta ${i}`).toBe(true);
    for (let i = 61; i < 90; i++) expect(sent.has(i), `GOP n+1 delta ${i}`).toBe(false);
    expect(link.acked).toEqual([0, 30, 60]);
    expect(stats.staleFramesDropped).toBe(29);
    await pipeline.stop();
  });

  it('a keyframe forced 100 ms after n, with n handed over at 150 ms, drops nothing', async () => {
    const link = makeFakeWT();
    const { pipeline, cbs } = await start(link);
    link.plans.set(30, { handOverAfterMs: 150 });
    // Frame 36 (t=600) is a forced keyframe, off the GOP cadence.
    await play(90, (i) => i % 30 === 0 || i === 36);
    const stats = await lastStats(cbs);

    const sent = deltaIds(link);
    for (let i = 1; i < 90; i++) if (i % 30 !== 0 && i !== 36) expect(sent.has(i), `delta ${i}`).toBe(true);
    expect(link.acked).toEqual([0, 30, 36, 60]);
    expect(stats.staleFramesDropped).toBe(0);
    expect(stats.keyframeStreamsSuperseded).toBe(0);
    await pipeline.stop();
  });

  it("a GOP whose keyframe failed drops its deltas — they could not be decoded", async () => {
    const link = makeFakeWT();
    const { pipeline, cbs } = await start(link);
    link.plans.set(30, { never: true });
    // Keyframe 30's write never finishes; 60 waits behind it (late), 90
    // replaces 60, and 150 (t=2500, 2 s after 30 opened) aborts it.
    await play(160);
    const stats = await lastStats(cbs);
    const sent = deltaIds(link);
    expect(sent.has(31)).toBe(true); // GOP n itself was being written
    expect(sent.has(155)).toBe(true); // the GOP after the abort
    expect(stats.keyframeStreamsFailed).toBe(1);
    expect(stats.keyframeStreamsSuperseded).toBeGreaterThanOrEqual(2);
    await pipeline.stop();
  });
});

describe('BroadcastPipeline — resume (RU9)', () => {
  it('a new sender: nothing queued on the old one is sent, keyframes in Chrome fail, the waiter is superseded, the forced keyframe starts at once', async () => {
    const first = makeFakeWT([ANNOUNCE, TOKEN]);
    const { pipeline, cbs } = await start(first);
    await vi.advanceTimersByTimeAsync(0);
    // 60 is never handed over (being written), and 65 — forced — waits
    // behind an on-time 60, so its deltas still go.
    first.plans.set(60, { never: true });
    await play(70, (i) => i % 30 === 0 || i === 65);
    first.stallNextDatagram(10_000);
    emit('delta', 70); // in flight on the old writer
    emit('delta', 71); // queued behind it

    const second = makeFakeWT();
    connectWebTransport.mockResolvedValueOnce(second.wt);
    first.die(new Error('connection lost'));
    await vi.advanceTimersByTimeAsync(ABRUPT_DROP_RETRY_DELAY_MS + 10);
    expect(cbs.onResumed).toHaveBeenCalledTimes(1);
    expect(h.forced.value).toBe(1);

    // A late delta of the waiting (discarded) keyframe's GOP: undecodable.
    emit('delta', 72);
    emit('key', 73); // the forced keyframe — starts at once
    emit('delta', 74);
    await vi.advanceTimersByTimeAsync(20);
    expect(second.opened).toBe(1);
    expect(second.acked).toEqual([73]);
    const sent = deltaIds(second);
    expect(sent.has(70)).toBe(false);
    expect(sent.has(71)).toBe(false);
    expect(sent.has(72)).toBe(false);
    expect(sent.has(74)).toBe(true);

    const stats = await lastStats(cbs);
    expect(stats.keyframeStreamsFailed).toBe(1); // 60, in Chrome when the session died
    expect(stats.keyframeStreamsSuperseded).toBe(1); // 65, the waiter
    await pipeline.stop();
  });
});

describe('BroadcastPipeline — the upload notice (RU9)', () => {
  it("raises uplinkWarning after five bad seconds of stale drops, not before", async () => {
    const link = makeFakeWT();
    const { pipeline, cbs } = await start(link);
    link.slowDatagrams(50); // 20 datagrams a second against 60 fps
    await play(60 * 3);
    expect(cbs.onStats.mock.calls.at(-1)![0].uplinkWarning).toBe(false);
    for (let i = 180; i < 60 * 8; i++) {
      await at(Math.round((i * 1000) / 60));
      emit(i % 30 === 0 ? 'key' : 'delta', i);
    }
    expect(cbs.onStats.mock.calls.at(-1)![0].uplinkWarning).toBe(true);
    await pipeline.stop();
  });
});
