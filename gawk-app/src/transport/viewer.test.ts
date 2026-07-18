// ViewerPipeline close handling. connection + decoder are mocked; the fake
// WebTransport models only what the pipeline touches (closed promise,
// close()). The datagram read loop is driven by the mocked readDatagrams.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

const connectWebTransport = vi.fn();
const readDatagrams = vi.fn();
// Shared across every mocked Decoder instance so a test can assert exactly
// which frames actually reached the decoder (the observable freeze-on-gap
// behavior: gapped deltas are never handed to decode()).
const decodeSpy = vi.fn();

const readServerStreams = vi.fn();

vi.mock('./connection', () => ({
  connectWebTransport: (...args: unknown[]) => connectWebTransport(...args),
  readDatagrams: (...args: unknown[]) => readDatagrams(...args),
  readServerStreams: (...args: unknown[]) => readServerStreams(...args),
  newCarrierCounters: () => ({ streamsOpened: 0, recordsReceived: 0, streamsAborted: 0, malformed: 0 }),
}));

// The latest Decoder instance's callbacks, so a test can fire onDecoded (the
// pipeline's measurement point for live-edge drift + absolute latency).
const decoderCbs: { value: { onDecoded: (d: unknown) => void } | null } = { value: null };

vi.mock('../media/decoder', () => ({
  Decoder: class {
    queueSize = 0;
    configure = vi.fn();
    decode = (...args: unknown[]) => decodeSpy(...args);
    close = vi.fn(() => Promise.resolve());
    constructor(cbs: { onDecoded: (d: unknown) => void }) {
      decoderCbs.value = cbs;
    }
  },
}));

import { timeOriginMs } from './time-sync';
import { ViewerPipeline, type ViewerCallbacks, type ViewerStats } from './viewer';
import type { ViewerTransport, ViewerTransportCallbacks } from './viewer-transport';
import { CLOSE_CODE_BROADCAST_ENDED, encodeClockMapping, encodeDecoderConfig, encodeVideoChunk } from './wire';

function makeFakeWT(closedAfterMs: number, closeInfo: unknown) {
  const closed = new Promise((res) => {
    setTimeout(() => res(closeInfo), closedAfterMs);
  });
  return {
    closed,
    close: vi.fn(),
  } as unknown as WebTransport;
}

interface Recorded {
  cbs: ViewerCallbacks;
  errors: { message: string; closeCode?: number }[];
  events: string[];
}

function makeCallbacks(): Recorded {
  const errors: Recorded['errors'] = [];
  const events: string[] = [];
  const cbs: ViewerCallbacks = {
    onDecodedFrame: () => {},
    onConfig: () => {},
    onStats: () => {},
    onError: (e) => {
      errors.push({ message: e.message, closeCode: (e as { closeCode?: number }).closeCode });
      events.push('error');
    },
    onEnded: () => events.push('ended'),
  };
  return { cbs, errors, events };
}

beforeEach(() => {
  vi.stubGlobal('window', globalThis);
  // WebCodecs is absent under node; the viewer wraps each frame in one.
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
  readServerStreams.mockReset();
  // No keyframe streams arrive in these tests (keyframes are driven via
  // datagrams); the loop just stays open for the life of the session.
  readServerStreams.mockReturnValue(new Promise(() => {}));
  decodeSpy.mockReset();
});

// Single-chunk datagrams (chunkCount 1 => the reassembler emits on arrival).
// The decoder is mocked, so payload bytes are irrelevant.
function configDgram(): Uint8Array {
  return encodeDecoderConfig({ codec: 'vp8', extradata: new Uint8Array(0) });
}
function frameDgram(frameId: number, keyframe: boolean): Uint8Array {
  return encodeVideoChunk(
    { keyframe, frameId, chunkIndex: 0, chunkCount: 1, timestampUs: BigInt(frameId * 1000) },
    new Uint8Array([1, 2, 3]),
  );
}
// Lets the decoder op-chain (promise microtasks) settle after delivering.
async function flush(): Promise<void> {
  await new Promise((r) => setTimeout(r, 0));
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('ViewerPipeline', () => {
  it('dials /subscribe/<id>', async () => {
    connectWebTransport.mockResolvedValue(makeFakeWT(60_000, {}));
    readDatagrams.mockReturnValue(new Promise(() => {})); // session stays up
    const { cbs } = makeCallbacks();
    const pipeline = new ViewerPipeline('https://relay.test:4433', 'K7XQ2M', {}, cbs);
    await pipeline.start();
    expect(connectWebTransport).toHaveBeenCalledWith(
      'https://relay.test:4433/subscribe/K7XQ2M',
      {},
    );
    await pipeline.stop();
  });

  it('appends ?delivery=reliable when resilient delivery is requested (R19)', async () => {
    // The session/URL seam of docs/24 Decision 6: the toggle reaches the
    // relay as a subscribe-time query param, nothing else changes.
    connectWebTransport.mockResolvedValue(makeFakeWT(60_000, {}));
    readDatagrams.mockReturnValue(new Promise(() => {}));
    const { cbs } = makeCallbacks();
    const opts = { deliveryMode: 'reliable' as const };
    const pipeline = new ViewerPipeline('https://relay.test:4433', 'K7XQ2M', opts, cbs);
    await pipeline.start();
    expect(connectWebTransport).toHaveBeenCalledWith(
      'https://relay.test:4433/subscribe/K7XQ2M?delivery=reliable',
      opts,
    );
    await pipeline.stop();
  });

  it('reports the delivery mode truthfully, incl. the requested-but-datagrams fallback (R19)', async () => {
    vi.useFakeTimers({
      toFake: ['setTimeout', 'clearTimeout', 'setInterval', 'clearInterval', 'performance'],
    });
    try {
      const carrier = { streamsOpened: 0, recordsReceived: 0, streamsAborted: 0, malformed: 0 };
      const fakeTransport: ViewerTransport = {
        kind: 'in-process',
        connect: async () => {},
        sampleConnectionStats: () => null,
        sampleTimeSync: () => null,
        sampleCarrierStats: () => ({ ...carrier }),
        close: () => {},
      };
      const { cbs } = makeCallbacks();
      const stats: ViewerStats[] = [];
      cbs.onStats = (s) => stats.push(s);
      const pipeline = new ViewerPipeline(
        'https://relay.test:4433',
        'K7XQ2M',
        { deliveryMode: 'reliable' },
        cbs,
        null,
        () => fakeTransport,
      );
      await pipeline.start();

      // Requested, but no carrier has appeared: the Decision 8 fallback state.
      await vi.advanceTimersByTimeAsync(500);
      expect(stats.at(-1)!.deliveryMode).toBe('reliable-requested');
      expect(stats.at(-1)!.carrierStreams).toBe(0);

      // Carriers observed: the mode is truthfully reliable.
      carrier.streamsOpened = 2;
      carrier.recordsReceived = 40;
      await vi.advanceTimersByTimeAsync(500);
      expect(stats.at(-1)!.deliveryMode).toBe('reliable');
      expect(stats.at(-1)!.carrierStreams).toBe(2);
      expect(stats.at(-1)!.carrierRecords).toBe(40);

      await pipeline.stop();
    } finally {
      vi.useRealTimers();
    }
  });

  it('reports datagram delivery when resilient mode was not requested (R19)', async () => {
    vi.useFakeTimers({
      toFake: ['setTimeout', 'clearTimeout', 'setInterval', 'clearInterval', 'performance'],
    });
    try {
      connectWebTransport.mockResolvedValue(makeFakeWT(600_000, {}));
      readDatagrams.mockReturnValue(new Promise(() => {}));
      const { cbs } = makeCallbacks();
      const stats: ViewerStats[] = [];
      cbs.onStats = (s) => stats.push(s);
      const pipeline = new ViewerPipeline('https://relay.test:4433', 'K7XQ2M', {}, cbs);
      await pipeline.start();
      await vi.advanceTimersByTimeAsync(500);
      expect(stats.at(-1)!.deliveryMode).toBe('datagrams');
      await pipeline.stop();
    } finally {
      vi.useRealTimers();
    }
  });

  it('surfaces the close code even when the read loop settles before wt.closed', async () => {
    // On a server GC close both the datagram reader and wt.closed settle,
    // in unspecified order. Only wt.closed carries the close code; if the
    // read loop wins the race the viewer must still report code 4000 so
    // ViewerSession shows "broadcast ended" instead of reconnect-looping.
    const wt = makeFakeWT(20, { closeCode: CLOSE_CODE_BROADCAST_ENDED, reason: 'broadcast ended' });
    connectWebTransport.mockResolvedValue(wt);
    readDatagrams.mockResolvedValue(undefined); // read loop ends immediately
    const { cbs, errors, events } = makeCallbacks();
    const pipeline = new ViewerPipeline('https://relay.test:4433', 'K7XQ2M', {}, cbs);
    await pipeline.start();

    await vi.waitFor(() => expect(events).toContain('ended'), { timeout: 2000 });
    expect(errors.some((e) => e.closeCode === CLOSE_CODE_BROADCAST_ENDED)).toBe(true);
  });

  it('freezes on a frame-id gap: discards deltas until the next keyframe', async () => {
    // A lost frame (dropped incomplete upstream) leaves a hole in the frameId
    // sequence. Decoding the deltas after the hole renders visible corruption
    // (they reference a frame that never arrived), so the viewer must instead
    // hold the last good frame — discard deltas until the next keyframe.
    connectWebTransport.mockResolvedValue(makeFakeWT(60_000, {}));
    let deliver: ((d: Uint8Array) => void) | null = null;
    readDatagrams.mockImplementation((_wt: unknown, onDatagram: (d: Uint8Array) => void) => {
      deliver = onDatagram;
      return new Promise(() => {}); // session stays up
    });
    const { cbs } = makeCallbacks();
    const pipeline = new ViewerPipeline('https://relay.test:4433', 'K7XQ2M', {}, cbs);
    await pipeline.start();
    const push = deliver as unknown as (d: Uint8Array) => void;

    // Config + contiguous keyframe/deltas all decode.
    push(configDgram());
    push(frameDgram(0, true));
    push(frameDgram(1, false));
    push(frameDgram(2, false));
    await flush();
    expect(decodeSpy).toHaveBeenCalledTimes(3);

    // Frame 3 is lost; deltas 4 and 5 must NOT reach the decoder.
    push(frameDgram(4, false));
    push(frameDgram(5, false));
    await flush();
    expect(decodeSpy).toHaveBeenCalledTimes(3);

    // The next keyframe re-syncs; deltas flow again.
    push(frameDgram(6, true));
    push(frameDgram(7, false));
    await flush();
    expect(decodeSpy).toHaveBeenCalledTimes(5);

    await pipeline.stop();
  });

  it('reports received fps, stall ages and a null connection (R9 M6)', async () => {
    // Fake performance too: the stats window (dt) and the stall ages are
    // computed from performance.now inside the pipeline.
    vi.useFakeTimers({
      toFake: ['setTimeout', 'clearTimeout', 'setInterval', 'clearInterval', 'performance'],
    });
    try {
      connectWebTransport.mockResolvedValue(makeFakeWT(600_000, {}));
      let deliver: ((d: Uint8Array) => void) | null = null;
      readDatagrams.mockImplementation((_wt: unknown, onDatagram: (d: Uint8Array) => void) => {
        deliver = onDatagram;
        return new Promise(() => {});
      });
      const { cbs } = makeCallbacks();
      const stats: ViewerStats[] = [];
      cbs.onStats = (s) => stats.push(s);
      const pipeline = new ViewerPipeline('https://relay.test:4433', 'K7XQ2M', {}, cbs);
      await pipeline.start();
      const push = deliver as unknown as (d: Uint8Array) => void;

      push(configDgram());
      push(frameDgram(0, true)); // keyframe → also stamps the keyframe age
      push(frameDgram(1, false));

      await vi.advanceTimersByTimeAsync(500); // fire the stats interval
      const last = stats.at(-1);
      expect(last).toBeDefined();
      // Two complete frames over the 500 ms window.
      expect(last!.receivedFps).toBeCloseTo(2 / 0.5, 3);
      // Both frames arrived at t≈0; the stats tick ran at t=500.
      expect(last!.timeSinceLastFrameMs).toBeCloseTo(500, 0);
      expect(last!.lastKeyframeAgeMs).toBeCloseTo(500, 0);
      // Main-thread path (no RenderSink) → renderedFps is unknowable here,
      // and no sink means no renderer kind either (R10).
      expect(last!.renderedFps).toBeNull();
      expect(last!.renderer).toBeNull();
      // These tests stub `window`, so the pipeline detects the main-thread
      // context, with the default in-process transport (R10 P3).
      expect(last!.pipelineContext).toBe('main-thread');
      expect(last!.transport).toBe('in-process');
      // The fake WebTransport has no getStats → null, never a throw.
      expect(last!.connection).toBeNull();
      // R5 Q1: the drift metric is null before any decoded frame (the mocked
      // decoder never emits), and the field must ride every stats tick.
      expect(last!.liveEdgeDriftMs).toBeNull();
      // Frame dimensions come from decoded frames, so they're null here too.
      expect(last!.frameWidth).toBeNull();
      expect(last!.frameHeight).toBeNull();
      // R5 Q2: absolute latency + self-owned RTT stay null against a relay /
      // transport that never answers time sync (older server: no regression).
      expect(last!.capToRenderMs).toBeNull();
      expect(last!.timeSyncRttMs).toBeNull();

      await pipeline.stop();
    } finally {
      vi.useRealTimers();
    }
  });

  it('counts self-received video bytes: datagrams + keyframe stream messages', async () => {
    // "Video bitrate (recv)" is derived from this counter — the viewer counts
    // what it receives itself because WebTransport.getStats() ships in no
    // browser (docs/13 D7), mirroring the broadcaster's bytesSent.
    vi.useFakeTimers({
      toFake: ['setTimeout', 'clearTimeout', 'setInterval', 'clearInterval', 'performance'],
    });
    try {
      connectWebTransport.mockResolvedValue(makeFakeWT(600_000, {}));
      let deliver: ((d: Uint8Array) => void) | null = null;
      readDatagrams.mockImplementation((_wt: unknown, onDatagram: (d: Uint8Array) => void) => {
        deliver = onDatagram;
        return new Promise(() => {});
      });
      let deliverKf: ((kf: unknown) => void) | null = null;
      readServerStreams.mockImplementation((_wt: unknown, cb: { onKeyframe: (kf: unknown) => void }) => {
        deliverKf = cb.onKeyframe;
        return new Promise(() => {});
      });
      const { cbs } = makeCallbacks();
      const stats: ViewerStats[] = [];
      cbs.onStats = (s) => stats.push(s);
      const pipeline = new ViewerPipeline('https://relay.test:4433', 'K7XQ2M', {}, cbs);
      await pipeline.start();
      const push = deliver as unknown as (d: Uint8Array) => void;
      const pushKf = deliverKf as unknown as (kf: unknown) => void;

      const d1 = configDgram();
      const d2 = frameDgram(0, true);
      const d3 = frameDgram(1, false);
      push(d1);
      push(d2);
      push(d3);
      // streamBytes is the whole StreamFrame message as read off the wire
      // (header + config + payload) — the sender-side msg.length mirror.
      pushKf({ frameId: 2, timestampUs: 2000n, config: null, data: new Uint8Array([9]), streamBytes: 240 });

      await vi.advanceTimersByTimeAsync(500); // one stats tick
      const last = stats.at(-1)!;
      expect(last.videoBytesReceived).toBe(d1.byteLength + d2.byteLength + d3.byteLength + 240);

      await pipeline.stop();
    } finally {
      vi.useRealTimers();
    }
  });

  it('computes absolute capture→render latency from both clock legs (R5 Q2)', async () => {
    vi.useFakeTimers({
      toFake: ['setTimeout', 'clearTimeout', 'setInterval', 'clearInterval', 'performance'],
    });
    try {
      // A fake transport supplies this leg's clock sync; the broadcaster leg
      // arrives as a ClockMapping datagram through the normal datagram path.
      let tcb: ViewerTransportCallbacks | null = null;
      const fakeTransport: ViewerTransport = {
        kind: 'in-process',
        connect: async (cb) => {
          tcb = cb;
        },
        sampleConnectionStats: () => null,
        sampleTimeSync: () => ({ offsetUs: 1_000_000n, rttMs: 5, timeOriginMs: timeOriginMs() }),
        close: () => {},
      };
      const { cbs } = makeCallbacks();
      const stats: ViewerStats[] = [];
      cbs.onStats = (s) => stats.push(s);
      const pipeline = new ViewerPipeline(
        'https://relay.test:4433',
        'K7XQ2M',
        {},
        cbs,
        null,
        () => fakeTransport,
      );
      await pipeline.start();

      // Broadcaster leg: relayUs = timestampUs + 2_000_000.
      (tcb as unknown as ViewerTransportCallbacks).onDatagram(encodeClockMapping(2_000_000n));

      // Choose the frame timestamp so the true latency is exactly 250 ms:
      //   (nowUs + viewerOffset) − (ts + broadcastOffset) = 250_000
      const nowUsV = BigInt(Math.round(performance.now() * 1000));
      const ts = Number(nowUsV + 1_000_000n - 2_000_000n - 250_000n);
      decoderCbs.value!.onDecoded({
        frame: { timestamp: ts, displayWidth: 1920, displayHeight: 1080 },
        captureTimestampUs: ts,
        decodeStartMs: 0,
        decodeEndMs: 1,
      });

      await vi.advanceTimersByTimeAsync(500); // one stats tick
      const last = stats.at(-1)!;
      expect(last.capToRenderMs).toBeCloseTo(250, 0);
      expect(last.timeSyncRttMs).toBe(5);
      expect(last.liveEdgeDriftMs).toBe(0); // single sample IS the baseline
      // The decoded frame's own dimensions ride the stats (docs/01: trust the
      // VideoFrame in hand, not track metadata).
      expect(last.frameWidth).toBe(1920);
      expect(last.frameHeight).toBe(1080);

      await pipeline.stop();
    } finally {
      vi.useRealTimers();
    }
  });

  it('translates a TimeSync sample from another clock domain (nested transport worker)', async () => {
    // The TimeSync estimator runs inside the nested transport worker, whose
    // performance.now() counts from its own timeOrigin — set at worker
    // creation. A reconnect mid-view (the resilient-mode toggle, a 4002
    // rollout drain, any auto-reconnect) spawns a fresh transport worker
    // minutes after the viewer worker, and applying its offset to the viewer
    // worker's now() inflated capture→render by exactly that gap (field
    // report: ~3 minutes after ~3 minutes of watching).
    vi.useFakeTimers({
      toFake: ['setTimeout', 'clearTimeout', 'setInterval', 'clearInterval', 'performance'],
    });
    try {
      const WORKER_AGE_GAP_MS = 180_000; // transport worker born 3 min into the view
      let tcb: ViewerTransportCallbacks | null = null;
      const fakeTransport: ViewerTransport = {
        kind: 'worker',
        connect: async (cb) => {
          tcb = cb;
        },
        sampleConnectionStats: () => null,
        sampleTimeSync: () => ({
          offsetUs: 1_000_000n,
          rttMs: 5,
          timeOriginMs: timeOriginMs() + WORKER_AGE_GAP_MS,
        }),
        close: () => {},
      };
      const { cbs } = makeCallbacks();
      const stats: ViewerStats[] = [];
      cbs.onStats = (s) => stats.push(s);
      const pipeline = new ViewerPipeline(
        'https://relay.test:4433',
        'K7XQ2M',
        {},
        cbs,
        null,
        () => fakeTransport,
      );
      await pipeline.start();

      // Broadcaster leg: relayUs = timestampUs + 2_000_000.
      (tcb as unknown as ViewerTransportCallbacks).onDatagram(encodeClockMapping(2_000_000n));

      // The offset maps the TRANSPORT WORKER's clock to the relay clock:
      //   relayUs = (viewerNowUs − gapUs) + offsetUs
      // Choose the frame timestamp so the true latency is exactly 250 ms.
      const nowUsV = BigInt(Math.round(performance.now() * 1000));
      const relayNowUs = nowUsV - BigInt(WORKER_AGE_GAP_MS) * 1000n + 1_000_000n;
      const ts = Number(relayNowUs - 2_000_000n - 250_000n);
      decoderCbs.value!.onDecoded({
        frame: { timestamp: ts, displayWidth: 1920, displayHeight: 1080 },
        captureTimestampUs: ts,
        decodeStartMs: 0,
        decodeEndMs: 1,
      });

      await vi.advanceTimersByTimeAsync(500); // one stats tick
      const last = stats.at(-1)!;
      // Buggy cross-domain math read ≈ WORKER_AGE_GAP_MS + 250 here.
      expect(last.capToRenderMs).toBeCloseTo(250, 0);

      await pipeline.stop();
    } finally {
      vi.useRealTimers();
    }
  });

  it('recovers delta flow after a broadcaster restart (stream keyframe resets the reassembler watermark)', async () => {
    // R10 field finding (docs/14): keyframes ride streams since R8, so the
    // reassembler's datagram-keyframe watermark reset never fires in
    // practice. After a restart (frameIds reset to 0), the stream keyframe
    // must reset the watermark or every new-session delta is dropped as
    // late — keyframe-only 2 fps playback.
    connectWebTransport.mockResolvedValue(makeFakeWT(600_000, {}));
    let deliver: ((d: Uint8Array) => void) | null = null;
    readDatagrams.mockImplementation((_wt: unknown, onDatagram: (d: Uint8Array) => void) => {
      deliver = onDatagram;
      return new Promise(() => {});
    });
    let deliverKf: ((kf: unknown) => void) | null = null;
    readServerStreams.mockImplementation((_wt: unknown, cb: { onKeyframe: (kf: unknown) => void }) => {
      deliverKf = cb.onKeyframe;
      return new Promise(() => {});
    });
    const { cbs } = makeCallbacks();
    const pipeline = new ViewerPipeline('https://relay.test:4433', 'K7XQ2M', {}, cbs);
    await pipeline.start();
    const push = deliver as unknown as (d: Uint8Array) => void;
    const pushKf = deliverKf as unknown as (kf: unknown) => void;

    // Old session: watermark climbs to 100001.
    push(configDgram());
    push(frameDgram(100_000, true));
    push(frameDgram(100_001, false));
    await flush();
    expect(decodeSpy).toHaveBeenCalledTimes(2);

    // Broadcaster restart: the new session's keyframe (id 3) arrives over the
    // reliable stream, then its deltas arrive as datagrams.
    pushKf({ frameId: 3, timestampUs: 3000n, config: null, data: new Uint8Array([9]), streamBytes: 25 });
    push(frameDgram(4, false));
    push(frameDgram(5, false));

    // The reorder buffer holds the backwards-jump keyframe for the delta-gap
    // grace before jumping; real timers drive its tick.
    await vi.waitFor(() => expect(decodeSpy).toHaveBeenCalledTimes(5), { timeout: 2000 });

    await pipeline.stop();
  });

  it('fails without a close code when the session drops abruptly', async () => {
    // A transient drop (no clean close) must keep today's reconnect path:
    // an error with no closeCode.
    const wt = makeFakeWT(60_000, {}); // closed never settles in this test
    connectWebTransport.mockResolvedValue(wt);
    readDatagrams.mockRejectedValue(new Error('connection lost'));
    const { cbs, errors, events } = makeCallbacks();
    const pipeline = new ViewerPipeline('https://relay.test:4433', 'K7XQ2M', {}, cbs);
    await pipeline.start();

    await vi.waitFor(() => expect(events).toContain('ended'), { timeout: 2000 });
    expect(errors.length).toBeGreaterThan(0);
    expect(errors.every((e) => e.closeCode === undefined)).toBe(true);
  });
});
