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

const readKeyframeStreams = vi.fn();

vi.mock('./connection', () => ({
  connectWebTransport: (...args: unknown[]) => connectWebTransport(...args),
  readDatagrams: (...args: unknown[]) => readDatagrams(...args),
  readKeyframeStreams: (...args: unknown[]) => readKeyframeStreams(...args),
}));

vi.mock('../media/decoder', () => ({
  Decoder: class {
    queueSize = 0;
    configure = vi.fn();
    decode = (...args: unknown[]) => decodeSpy(...args);
    close = vi.fn(() => Promise.resolve());
  },
}));

import { ViewerPipeline, type ViewerCallbacks, type ViewerStats } from './viewer';
import { CLOSE_CODE_BROADCAST_ENDED, encodeDecoderConfig, encodeVideoChunk } from './wire';

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
  readKeyframeStreams.mockReset();
  // No keyframe streams arrive in these tests (keyframes are driven via
  // datagrams); the loop just stays open for the life of the session.
  readKeyframeStreams.mockReturnValue(new Promise(() => {}));
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
      // Main-thread path (no RenderSink) → renderedFps is unknowable here.
      expect(last!.renderedFps).toBeNull();
      // The fake WebTransport has no getStats → null, never a throw.
      expect(last!.connection).toBeNull();

      await pipeline.stop();
    } finally {
      vi.useRealTimers();
    }
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
