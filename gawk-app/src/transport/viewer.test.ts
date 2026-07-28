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
import { INCOMING_DATAGRAM_BUFFER } from './datagram-buffer';
import { getAvSkewMs, notePlayhead, resetAvSync } from './av-sync';
import { SESSION_STALL_MS, ViewerPipeline, type ViewerCallbacks, type ViewerStats } from './viewer';
import type { RenderSink } from './render-sink';
import { DVR_BUFFER_MS, getDvrGranted, setDvrGranted, setViewerDeliveryMode } from './playout';
import type { ViewerTransport, ViewerTransportCallbacks } from './viewer-transport';
import {
  CLOSE_CODE_BROADCAST_ENDED,
  encodeAudioConfig,
  encodeAudioFrame,
  encodeClockMapping,
  encodeDecoderConfig,
  encodeVideoChunk,
  encodeViewerCount,
  encodeDeliveryAck,
} from './wire';

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
// One Opus packet. The audio lane is the media-stall watchdog's continuous
// reference medium (video capture is damage-driven; audio is not).
function audioDgram(seq: number): Uint8Array {
  return encodeAudioFrame({ seq, timestampUs: BigInt(seq * 20_000) }, new Uint8Array([1, 2, 3]));
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

  it('appends ?parity= only when the viewer opted DOWN from the fleet default (R29)', async () => {
    // docs/34 §5.2: the fleet decides the level and the viewer can only opt
    // down, so the DEFAULT must send no parameter at all — that is what lets
    // the relay's default apply and keeps a pre-R29 relay's URL unchanged.
    connectWebTransport.mockResolvedValue(makeFakeWT(60_000, {}));
    readDatagrams.mockReturnValue(new Promise(() => {}));
    const { cbs } = makeCallbacks();
    const p1 = new ViewerPipeline('https://relay.test:4433', 'K7XQ2M', {}, cbs);
    await p1.start();
    expect(connectWebTransport).toHaveBeenCalledWith(
      'https://relay.test:4433/subscribe/K7XQ2M',
      {},
    );
    await p1.stop();

    for (const level of [0, 1] as const) {
      connectWebTransport.mockClear();
      const opts = { parityLevel: level };
      const p2 = new ViewerPipeline('https://relay.test:4433', 'K7XQ2M', opts, cbs);
      await p2.start();
      expect(connectWebTransport).toHaveBeenCalledWith(
        `https://relay.test:4433/subscribe/K7XQ2M?parity=${level}`,
        opts,
      );
      await p2.stop();
    }
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
    // R21 (docs/26 Decisions 7 + 15): plain Resilient mode asks for carriers
    // and NOT for a ring. Sending buffer= here would have the relay serve this
    // viewer from a cursor with a 3 s staleness bound while it holds ~0.5 s —
    // the two ends disagreeing about how far behind it is.
    expect(connectWebTransport).toHaveBeenCalledWith(
      'https://relay.test:4433/subscribe/K7XQ2M?delivery=reliable',
      opts,
    );
    await pipeline.stop();
  });

  it('asks for a ring only on the deep-buffer step (R21)', async () => {
    connectWebTransport.mockResolvedValue(makeFakeWT(60_000, {}));
    readDatagrams.mockReturnValue(new Promise(() => {}));
    setViewerDeliveryMode('deep');
    try {
      const { cbs } = makeCallbacks();
      const opts = { deliveryMode: 'reliable' as const };
      const pipeline = new ViewerPipeline('https://relay.test:4433', 'K7XQ2M', opts, cbs);
      await pipeline.start();
      expect(connectWebTransport).toHaveBeenCalledWith(
        `https://relay.test:4433/subscribe/K7XQ2M?delivery=reliable&buffer=${DVR_BUFFER_MS}`,
        opts,
      );
      await pipeline.stop();
    } finally {
      setViewerDeliveryMode('live');
    }
  });

  it('reconnects when keyframes stop while deltas keep arriving (Safari stream-path stall)', async () => {
    // Safari field finding (2026-07-21): the viewer's stream path can wedge
    // while datagrams keep flowing (QUIC datagrams are not flow-controlled;
    // streams are). Keyframes stop, the reorder buffer parks in
    // waiting-for-keyframe and ages out every delta, and playback freezes
    // permanently — with no close code and no error, so nothing reconnects.
    // A viewer taking frames but starved of keyframes must give up and let
    // ViewerSession reconnect into a fresh session.
    vi.useFakeTimers({
      toFake: ['setTimeout', 'clearTimeout', 'setInterval', 'clearInterval', 'performance'],
    });
    try {
      let deliver!: (d: Uint8Array) => void;
      const fakeTransport: ViewerTransport = {
        kind: 'in-process',
        connect: async (cb) => {
          deliver = cb.onDatagram;
        },
        sampleConnectionStats: () => null,
        sampleTimeSync: () => null,
        close: () => {},
      };
      const { cbs, errors } = makeCallbacks();
      const pipeline = new ViewerPipeline(
        'https://relay.test:4433',
        'K7XQ2M',
        {},
        cbs,
        null,
        () => fakeTransport,
      );
      await pipeline.start();

      // A healthy start: config + a keyframe, so the watchdog has a baseline.
      deliver(configDgram());
      deliver(frameDgram(1, true));
      await vi.advanceTimersByTimeAsync(0);

      // Deltas keep arriving, but no keyframe ever lands again.
      for (let i = 0; i < 60; i++) {
        deliver(frameDgram(i + 2, false));
        await vi.advanceTimersByTimeAsync(200);
      }

      expect(errors.length).toBeGreaterThan(0);
      // Reconnectable, not fatal: no close code means ViewerSession retries.
      expect((errors[0] as Error & { closeCode?: number }).closeCode).toBeUndefined();

      await pipeline.stop();
    } finally {
      vi.useRealTimers();
    }
  });

  it('does not reconnect when the broadcaster is away but the session is alive', async () => {
    // A broadcaster who stepped away sends no media at all, and the viewer
    // must stay connected (docs/05 D1 keepalive holds the session open on
    // purpose), not reconnect-loop against an idle broadcast. What proves the
    // session is still alive is the relay's R18 ViewerCount keepalive, which
    // keeps arriving every ViewerCountKeepalive (5 s) with the publisher away.
    vi.useFakeTimers({
      toFake: ['setTimeout', 'clearTimeout', 'setInterval', 'clearInterval', 'performance'],
    });
    try {
      let deliver!: (d: Uint8Array) => void;
      const fakeTransport: ViewerTransport = {
        kind: 'in-process',
        connect: async (cb) => {
          deliver = cb.onDatagram;
        },
        sampleConnectionStats: () => null,
        sampleTimeSync: () => null,
        close: () => {},
      };
      const { cbs, errors } = makeCallbacks();
      const pipeline = new ViewerPipeline(
        'https://relay.test:4433',
        'K7XQ2M',
        {},
        cbs,
        null,
        () => fakeTransport,
      );
      await pipeline.start();

      deliver(configDgram());
      deliver(frameDgram(1, true));
      await vi.advanceTimersByTimeAsync(0);

      // No media for 30 s, but the relay's count keepalive keeps landing.
      for (let i = 0; i < 6; i++) {
        deliver(encodeViewerCount(1));
        await vi.advanceTimersByTimeAsync(5_000);
      }

      expect(errors).toEqual([]);
      await pipeline.stop();
    } finally {
      vi.useRealTimers();
    }
  });

  it('reconnects when all media stops while the broadcaster keeps publishing (stream wedge)', async () => {
    // BUGS.md (2026-07-26, iPhone Deep-buffer capture `XN73GU`): in Deep buffer
    // every medium rides the stream path — video carriers, keyframe streams and
    // (R21 DV5) audio's own carrier — so a WebKit stream wedge stops all of it
    // dead. That disqualifies checkKeyframeStall, which requires frames to still
    // be arriving, while the relay's control sideband keeps landing on datagrams
    // and holds checkSessionStall off. Both watchdogs blind, 31 s frozen.
    //
    // The discriminators are the broadcaster's ClockMapping (published every 5 s
    // *only while capturing*, on datagrams) and audio having stopped too — see
    // the static-screen guard below for why audio is not optional here.
    vi.useFakeTimers({
      toFake: ['setTimeout', 'clearTimeout', 'setInterval', 'clearInterval', 'performance'],
    });
    try {
      let deliver!: (d: Uint8Array) => void;
      const fakeTransport: ViewerTransport = {
        kind: 'in-process',
        connect: async (cb) => {
          deliver = cb.onDatagram;
        },
        sampleConnectionStats: () => null,
        sampleTimeSync: () => null,
        close: () => {},
      };
      const { cbs, errors } = makeCallbacks();
      const pipeline = new ViewerPipeline(
        'https://relay.test:4433',
        'K7XQ2M',
        {},
        cbs,
        null,
        () => fakeTransport,
      );
      await pipeline.start();

      deliver(configDgram());
      deliver(frameDgram(1, true));
      deliver(audioDgram(1));
      await vi.advanceTimersByTimeAsync(0);

      // Both media are gone; the sideband keeps arriving — the keepalive every
      // 5 s and the broadcaster's mapping on its own 5 s cadence.
      for (let i = 0; i < 4; i++) {
        await vi.advanceTimersByTimeAsync(5_000);
        deliver(encodeViewerCount(1));
        deliver(encodeClockMapping(1_000n));
      }

      expect(errors.length).toBeGreaterThan(0);
      // Reconnectable, not fatal: no close code means ViewerSession retries.
      expect((errors[0] as Error & { closeCode?: number }).closeCode).toBeUndefined();
      // Specifically the media watchdog: the sideband never goes quiet for more
      // than 5 s here, so the dead-session one cannot be what fired.
      expect(errors[0].message).toContain('still publishing');

      await pipeline.stop();
    } finally {
      vi.useRealTimers();
    }
  });

  it('does not fire the media watchdog on a static screen (damage-driven capture)', async () => {
    // The case that makes "no video while mappings arrive" unsound on its own:
    // screen capture is damage-driven and stops entirely on a static screen
    // (docs/19, docs/28), so a paused game produces no frames for minutes while
    // the broadcaster is perfectly healthy and its 5 s mappings keep coming.
    // Audio is what separates the two — it is not damage-driven, so it keeps
    // flowing here and must veto the watchdog.
    vi.useFakeTimers({
      toFake: ['setTimeout', 'clearTimeout', 'setInterval', 'clearInterval', 'performance'],
    });
    try {
      let deliver!: (d: Uint8Array) => void;
      const fakeTransport: ViewerTransport = {
        kind: 'in-process',
        connect: async (cb) => {
          deliver = cb.onDatagram;
        },
        sampleConnectionStats: () => null,
        sampleTimeSync: () => null,
        close: () => {},
      };
      const { cbs, errors } = makeCallbacks();
      const pipeline = new ViewerPipeline(
        'https://relay.test:4433',
        'K7XQ2M',
        {},
        cbs,
        null,
        () => fakeTransport,
      );
      await pipeline.start();

      deliver(configDgram());
      deliver(frameDgram(1, true));
      await vi.advanceTimersByTimeAsync(0);

      // 30 s of static screen: no frames at all, audio and mappings unaffected.
      for (let i = 0; i < 30; i++) {
        deliver(audioDgram(i + 1));
        await vi.advanceTimersByTimeAsync(1_000);
        if (i % 5 === 0) deliver(encodeClockMapping(1_000n));
      }

      expect(errors).toEqual([]);
      await pipeline.stop();
    } finally {
      vi.useRealTimers();
    }
  });

  it('does not fire the media watchdog when the broadcaster stops publishing', async () => {
    // The away transition, which must NOT reconnect: the broadcaster's last
    // mapping can land right after its last media, so one mapping after the
    // silence starts proves nothing. Requiring two is what makes the watchdog
    // structurally safe here — a publisher that stopped sends no more.
    vi.useFakeTimers({
      toFake: ['setTimeout', 'clearTimeout', 'setInterval', 'clearInterval', 'performance'],
    });
    try {
      let deliver!: (d: Uint8Array) => void;
      const fakeTransport: ViewerTransport = {
        kind: 'in-process',
        connect: async (cb) => {
          deliver = cb.onDatagram;
        },
        sampleConnectionStats: () => null,
        sampleTimeSync: () => null,
        close: () => {},
      };
      const { cbs, errors } = makeCallbacks();
      const pipeline = new ViewerPipeline(
        'https://relay.test:4433',
        'K7XQ2M',
        {},
        cbs,
        null,
        () => fakeTransport,
      );
      await pipeline.start();

      deliver(configDgram());
      deliver(frameDgram(1, true));
      deliver(audioDgram(1));
      await vi.advanceTimersByTimeAsync(0);
      // One trailing mapping, then the publisher is gone — only the relay's
      // keepalive remains, for well past the media threshold.
      deliver(encodeClockMapping(1_000n));
      for (let i = 0; i < 2; i++) {
        await vi.advanceTimersByTimeAsync(5_000);
        deliver(encodeViewerCount(1));
      }

      expect(errors).toEqual([]);
      await pipeline.stop();
    } finally {
      vi.useRealTimers();
    }
  });

  it('reconnects when the session goes completely silent (dead session, no signal)', async () => {
    // BUGS.md (2026-07-22 paired capture): the relay had already dropped the
    // subscriber while WebKit surfaced nothing — wt.closed never resolved and
    // no read loop rejected — so the viewer sat on a corpse for 48 s showing
    // stale stats, no error and no reconnect. The keyframe watchdog could not
    // help: it only fires while frames are still arriving, and nothing was.
    // Total inbound silence past SESSION_STALL_MS is a dead session, because a
    // live one always carries at least the ViewerCount keepalive.
    vi.useFakeTimers({
      toFake: ['setTimeout', 'clearTimeout', 'setInterval', 'clearInterval', 'performance'],
    });
    try {
      let deliver!: (d: Uint8Array) => void;
      const fakeTransport: ViewerTransport = {
        kind: 'in-process',
        connect: async (cb) => {
          deliver = cb.onDatagram;
        },
        sampleConnectionStats: () => null,
        sampleTimeSync: () => null,
        close: () => {},
      };
      const { cbs, errors } = makeCallbacks();
      const pipeline = new ViewerPipeline(
        'https://relay.test:4433',
        'K7XQ2M',
        {},
        cbs,
        null,
        () => fakeTransport,
      );
      await pipeline.start();

      deliver(configDgram());
      deliver(frameDgram(1, true));
      await vi.advanceTimersByTimeAsync(0);

      await vi.advanceTimersByTimeAsync(SESSION_STALL_MS + 2_000);

      expect(errors.length).toBeGreaterThan(0);
      // Reconnectable, not fatal: no close code means ViewerSession retries.
      expect((errors[0] as Error & { closeCode?: number }).closeCode).toBeUndefined();

      await pipeline.stop();
    } finally {
      vi.useRealTimers();
    }
  });

  it('does not fire the session watchdog before anything has ever arrived', async () => {
    // A viewer joining a broadcast whose broadcaster is away receives nothing
    // until the first keepalive. Arming the watchdog from connect time would
    // tear that session down; it arms on the first inbound byte.
    vi.useFakeTimers({
      toFake: ['setTimeout', 'clearTimeout', 'setInterval', 'clearInterval', 'performance'],
    });
    try {
      const fakeTransport: ViewerTransport = {
        kind: 'in-process',
        connect: async () => {},
        sampleConnectionStats: () => null,
        sampleTimeSync: () => null,
        close: () => {},
      };
      const { cbs, errors } = makeCallbacks();
      const pipeline = new ViewerPipeline(
        'https://relay.test:4433',
        'K7XQ2M',
        {},
        cbs,
        null,
        () => fakeTransport,
      );
      await pipeline.start();
      await vi.advanceTimersByTimeAsync(SESSION_STALL_MS + 10_000);
      expect(errors).toEqual([]);
      await pipeline.stop();
    } finally {
      vi.useRealTimers();
    }
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

  // R29 finding 2 (docs/34), the fix itself: LocalViewerTransport is the
  // object that owns the session in EVERY placement — main thread, viewer
  // worker, and the nested transport worker — so raising the buffer there is
  // what makes the knob reach the path the loss was measured on. A fake whose
  // datagrams expose the legacy attribute stands in for Firefox 154.
  it('raises the incoming datagram buffer on the session it connects (R29)', async () => {
    vi.useFakeTimers({
      toFake: ['setTimeout', 'clearTimeout', 'setInterval', 'clearInterval', 'performance'],
    });
    try {
      const wt = makeFakeWT(600_000, {}) as unknown as { datagrams: { incomingHighWaterMark: number } };
      wt.datagrams = { incomingHighWaterMark: 1 };
      connectWebTransport.mockResolvedValue(wt);
      readDatagrams.mockReturnValue(new Promise(() => {}));
      const { cbs } = makeCallbacks();
      const stats: ViewerStats[] = [];
      cbs.onStats = (s) => stats.push(s);
      const pipeline = new ViewerPipeline('https://relay.test:4433', 'K7XQ2M', {}, cbs);
      await pipeline.start();
      await vi.advanceTimersByTimeAsync(500);
      expect(wt.datagrams.incomingHighWaterMark).toBe(INCOMING_DATAGRAM_BUFFER);
      expect(stats.at(-1)!.datagramBuffer).toEqual({
        property: 'incomingHighWaterMark',
        requested: INCOMING_DATAGRAM_BUFFER,
        effective: INCOMING_DATAGRAM_BUFFER,
        applied: true,
      });
      await pipeline.stop();
    } finally {
      vi.useRealTimers();
    }
  });

  // R29 finding 2 (docs/34): the receive-buffer verdict is transport-owned —
  // only the realm holding the WebTransport can set or read it — so the
  // pipeline must forward it verbatim rather than deriving anything. Null
  // where a transport doesn't report one, because "unknown" and "at the
  // browser default" are different states and only one of them is a problem.
  it('forwards the transport datagram-buffer verdict into ViewerStats (R29)', async () => {
    vi.useFakeTimers({
      toFake: ['setTimeout', 'clearTimeout', 'setInterval', 'clearInterval', 'performance'],
    });
    try {
      const datagramBuffer = {
        property: 'incomingMaxBufferedDatagrams' as const,
        requested: 256,
        effective: 256,
        applied: true,
      };
      const fakeTransport: ViewerTransport = {
        kind: 'in-process',
        connect: async () => {},
        sampleConnectionStats: () => null,
        sampleTimeSync: () => null,
        sampleDatagramBuffer: () => datagramBuffer,
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
      await vi.advanceTimersByTimeAsync(500);
      expect(stats.at(-1)!.datagramBuffer).toEqual(datagramBuffer);
      await pipeline.stop();
    } finally {
      vi.useRealTimers();
    }
  });

  it('reports null datagramBuffer where the transport reports none (R29)', async () => {
    vi.useFakeTimers({
      toFake: ['setTimeout', 'clearTimeout', 'setInterval', 'clearInterval', 'performance'],
    });
    try {
      const fakeTransport: ViewerTransport = {
        kind: 'in-process',
        connect: async () => {},
        sampleConnectionStats: () => null,
        sampleTimeSync: () => null,
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
      await vi.advanceTimersByTimeAsync(500);
      expect(stats.at(-1)!.datagramBuffer).toBeNull();
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

  it('samples A/V skew at presentation via the render sink, not at decode', async () => {
    // The paced sink holds a frame for the playout offset, so sampling skew at
    // decode reads the buffer depth as skew. On the render-sink (worker) path
    // the pipeline installs a presentation observer and does NOT sample at
    // decode — the sink fires the observer when the frame is on screen.
    resetAvSync();
    // An audio clock: at local 0 the speaker is playing broadcaster ts 1.000 s.
    notePlayhead({ heardUs: 1_000_000, atEpochMs: timeOriginMs() }, 0);

    let observer: ((ts: number, at: number) => void) | null = null;
    const draws: number[] = [];
    const sink = {
      kind: 'webgl' as const,
      setPresentationObserver: (o: ((ts: number, at: number) => void) | null) => {
        observer = o;
      },
      draw: (f: { timestamp: number; close?: () => void }) => {
        draws.push(f.timestamp);
        f.close?.();
      },
      drawnFrames: () => draws.length,
    };
    connectWebTransport.mockResolvedValue(makeFakeWT(60_000, {}));
    readDatagrams.mockReturnValue(new Promise(() => {}));
    const { cbs } = makeCallbacks();
    const pipeline = new ViewerPipeline(
      'https://relay.test:4433',
      'K7XQ2M',
      {},
      cbs,
      sink as unknown as RenderSink,
    );
    await pipeline.start();
    expect(observer).toBeTypeOf('function'); // observer installed at start

    // A decoded frame stamped 1.050 s reaches the sink but is NOT sampled yet.
    decoderCbs.value!.onDecoded({
      frame: { timestamp: 1_050_000, displayWidth: 640, displayHeight: 480 },
      decodeStartMs: 0,
      decodeEndMs: 1,
    });
    expect(draws).toEqual([1_050_000]);
    expect(getAvSkewMs(0)).toBeNull(); // decode did not sample skew

    // The sink presents the frame (its own clock): skew is sampled now, and
    // reads video − audio = 50 ms, not the buffering depth. Read on the same
    // clock the observer was given — a skew is only a reading while it is
    // fresh (docs/20 field finding 13).
    observer!(1_050_000, 0);
    expect(getAvSkewMs(0)).toBeCloseTo(50, 0);

    await pipeline.stop();
    resetAvSync();
  });

  it('samples A/V skew at decode on the main-thread fallback (no render sink)', async () => {
    // Without a paced sink the frame is drawn on arrival, so decode ≈ present
    // and decode-time sampling is correct there.
    vi.useFakeTimers({
      toFake: ['setTimeout', 'clearTimeout', 'setInterval', 'clearInterval', 'performance'],
    });
    try {
      resetAvSync();
      notePlayhead({ heardUs: 1_000_000, atEpochMs: timeOriginMs() }, 0);
      connectWebTransport.mockResolvedValue(makeFakeWT(60_000, {}));
      readDatagrams.mockReturnValue(new Promise(() => {}));
      const { cbs } = makeCallbacks();
      const pipeline = new ViewerPipeline('https://relay.test:4433', 'K7XQ2M', {}, cbs); // no sink
      await pipeline.start();

      decoderCbs.value!.onDecoded({
        frame: { timestamp: 1_050_000, displayWidth: 640, displayHeight: 480 },
        decodeStartMs: 0,
        decodeEndMs: 1,
      });
      // performance.now() is 0 under fake timers, matching the mapping anchor.
      expect(getAvSkewMs()).toBeCloseTo(50, 0);

      await pipeline.stop();
      resetAvSync();
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

// R15 N4 (docs/20 Decision 7): the viewer's audio lane is strictly additive.
// It is built lazily on the first audio message, never built at all without
// an audio consumer or an AudioDecoder, and its absence is annotated rather
// than fatal — video is untouched in every case.
describe('ViewerPipeline audio lane', () => {
  function audioFrameDgram(seq: number): Uint8Array {
    return encodeAudioFrame({ seq, timestampUs: BigInt(seq * 20_000) }, new Uint8Array([1, 2, 3]));
  }
  function audioConfigDgram(): Uint8Array {
    return encodeAudioConfig({
      codec: 'opus',
      sampleRate: 48000,
      channels: 2,
      description: new Uint8Array(0),
    });
  }

  async function runWith(
    cbs: ViewerCallbacks,
    feed: (deliver: (d: Uint8Array) => void) => void,
  ): Promise<ViewerStats> {
    connectWebTransport.mockResolvedValue(makeFakeWT(60_000, {}));
    let deliver: ((d: Uint8Array) => void) | null = null;
    readDatagrams.mockImplementation((_wt: unknown, onDatagram: (d: Uint8Array) => void) => {
      deliver = onDatagram;
      return new Promise(() => {});
    });
    let latest: ViewerStats | null = null;
    const pipeline = new ViewerPipeline('https://relay.test:4433', 'K7XQ2M', {}, {
      ...cbs,
      onStats: (s) => {
        latest = s;
      },
    });
    await pipeline.start();
    feed(deliver as unknown as (d: Uint8Array) => void);
    await flush();
    // Force a stats publication without waiting out the 500 ms timer.
    (pipeline as unknown as { publishStats(): void }).publishStats();
    await pipeline.stop();
    return latest!;
  }

  it('stays absent for a video-only stream', async () => {
    const { cbs } = makeCallbacks();
    const stats = await runWith({ ...cbs, onAudioChunk: () => {} }, (deliver) => {
      deliver(configDgram());
      deliver(frameDgram(0, true));
    });
    expect(stats.audioState).toBe('absent');
    expect(stats.audioPacketsReceived).toBe(0);
  });

  // The N4 criterion: a scope without AudioDecoder plays video-only and says
  // so, rather than erroring or changing pipeline placement.
  it('annotates unsupported when the scope has no AudioDecoder', async () => {
    // jsdom has no AudioDecoder — exactly the shape being tested.
    const { cbs, errors } = makeCallbacks();
    const chunks: unknown[] = [];
    const stats = await runWith({ ...cbs, onAudioChunk: (c) => chunks.push(c) }, (deliver) => {
      deliver(audioConfigDgram());
      deliver(audioFrameDgram(0));
    });
    expect(stats.audioState).toBe('unsupported');
    // Audio still counted as received (it IS in the stream), just not decoded.
    expect(stats.audioPacketsReceived).toBe(1);
    expect(stats.audioPacketsDecoded).toBe(0);
    expect(chunks).toHaveLength(0);
    // Never an error: video plays on.
    expect(errors).toHaveLength(0);
  });

  it('never builds a lane when the consumer takes no audio', async () => {
    const { cbs, errors } = makeCallbacks();
    // No onAudioChunk at all.
    const stats = await runWith(cbs, (deliver) => {
      deliver(audioConfigDgram());
      deliver(audioFrameDgram(0));
    });
    expect(stats.audioState).toBe('absent');
    // The reassembler still demuxes (cheap, and the counter is honest)…
    expect(stats.audioPacketsReceived).toBe(1);
    // …but nothing was built and nothing failed.
    expect(errors).toHaveLength(0);
  });
});

// CODE-REVIEW.md ("counters and stats survive their owner's deletion") +
// docs/20 post-implementation review finding 2. The viewer used to read the
// decode counters straight off the live lane, so nulling it on error left the
// overlay reporting "State: Error, decoded 0, format —" — which reads as
// "audio never worked" rather than "audio worked, then died", inverting the
// diagnosis on the one screen used to debug it.
describe('ViewerPipeline audio stats survive lane death', () => {
  // A drivable AudioDecoder: configure succeeds, each decode emits one
  // AudioData-like, and the test can fire the error callback on demand.
  function stubAudioDecoder() {
    const handles: { output: (d: unknown) => void; error: (e: Error) => void }[] = [];
    class FakeAudioDecoder {
      state = 'unconfigured';
      private cbs: { output: (d: unknown) => void; error: (e: Error) => void };
      constructor(cbs: { output: (d: unknown) => void; error: (e: Error) => void }) {
        this.cbs = cbs;
        handles.push(cbs);
      }
      configure() {
        this.state = 'configured';
      }
      decode() {
        this.cbs.output({
          timestamp: 0,
          sampleRate: 48000,
          numberOfChannels: 2,
          numberOfFrames: 960,
          copyTo: () => {},
          close: () => {},
        });
      }
      close() {
        this.state = 'closed';
      }
    }
    vi.stubGlobal('AudioDecoder', FakeAudioDecoder);
    vi.stubGlobal(
      'EncodedAudioChunk',
      class {
        constructor(init: unknown) {
          Object.assign(this, init);
        }
      },
    );
    return handles;
  }

  it('reports what a dead lane decoded, not zeros', async () => {
    const handles = stubAudioDecoder();
    connectWebTransport.mockResolvedValue(makeFakeWT(60_000, {}));
    let deliver: ((d: Uint8Array) => void) | null = null;
    readDatagrams.mockImplementation((_wt: unknown, onDatagram: (d: Uint8Array) => void) => {
      deliver = onDatagram;
      return new Promise(() => {});
    });

    let latest: ViewerStats | null = null;
    const { cbs } = makeCallbacks();
    const pipeline = new ViewerPipeline('https://relay.test:4433', 'K7XQ2M', {}, {
      ...cbs,
      onAudioChunk: () => {},
      onStats: (s) => {
        latest = s;
      },
    });
    await pipeline.start();
    const send = deliver as unknown as (d: Uint8Array) => void;

    // Audio works: config + three packets decode cleanly.
    send(encodeAudioConfig({ codec: 'opus', sampleRate: 48000, channels: 2, description: new Uint8Array(0) }));
    for (let i = 0; i < 3; i++) {
      send(encodeAudioFrame({ seq: i, timestampUs: BigInt(i * 20_000) }, new Uint8Array([1, 2, 3])));
    }
    await flush();
    (pipeline as unknown as { publishStats(): void }).publishStats();
    expect(latest!.audioState).toBe('active');
    expect(latest!.audioPacketsDecoded).toBe(3);

    // …then the decoder dies mid-stream.
    expect(handles).toHaveLength(1);
    handles[0].error(new Error('decoder exploded'));
    await flush();
    (pipeline as unknown as { publishStats(): void }).publishStats();

    // The lane is gone and annotated — but what it DID must survive, or the
    // overlay says "audio never worked".
    expect(latest!.audioState).toBe('error');
    expect(latest!.audioPacketsDecoded).toBe(3);
    expect(latest!.audioCodec).toBe('opus');
    expect(latest!.audioSampleRate).toBe(48000);
    expect(latest!.audioChannels).toBe(2);
    await pipeline.stop();
  });
});

// R21 (docs/26 Decision 7a): the relay states the served mode once at join,
// because a ring-replayed GOP is byte-identical on the wire to a live one and
// nothing the viewer can observe would tell the two apart. The deeper playout
// floor is applied only on that confirmation — against a relay that cannot
// keep it filled it would be pure latency.
describe('ViewerPipeline delivery ack', () => {
  afterEach(() => setDvrGranted(false));

  async function runWithAck(ack: Uint8Array | null) {
    let deliver!: (d: Uint8Array) => void;
    const fakeTransport: ViewerTransport = {
      kind: 'in-process',
      connect: async (cb) => {
        deliver = cb.onDatagram;
      },
      sampleConnectionStats: () => null,
      sampleTimeSync: () => null,
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
    if (ack) deliver(ack);
    return { pipeline, stats, deliver };
  }

  it('reports ring-backed delivery and deepens the playout floor', async () => {
    const { pipeline, stats } = await runWithAck(encodeDeliveryAck('dvr', 3000));
    expect(getDvrGranted()).toBe(true);
    await new Promise((r) => setTimeout(r, 600)); // one stats tick
    const s = stats.at(-1);
    expect(s?.deliveryMode).toBe('dvr');
    expect(s?.dvrBufferMs).toBe(3000);
    await pipeline.stop();
  });

  it('does not deepen the buffer when the relay serves plain carriers', async () => {
    const { pipeline } = await runWithAck(encodeDeliveryAck('reliable', 0));
    expect(getDvrGranted()).toBe(false);
    await pipeline.stop();
  });

  it('survives a malformed ack without breaking playback', async () => {
    // A diagnostics message must never be able to take the stream down.
    const bad = new Uint8Array([0x01, 0x0c, 0x09, 0x00, 0x00]);
    const { pipeline, deliver } = await runWithAck(bad);
    expect(getDvrGranted()).toBe(false);
    deliver(configDgram());
    deliver(frameDgram(1, true));
    await pipeline.stop();
  });
});
