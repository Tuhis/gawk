// connectWebTransport path-MTU logging. A path maxDatagramSize below the
// wire MAX_DATAGRAM_SIZE (Firefox negotiates 1024) is a *handled* condition:
// packetizeFrame sizes chunks to the actual path limit (docs/11), so the log
// must describe the adaptation — the pre-fix "will be dropped" wording sent a
// real debugging session chasing a bug that no longer exists.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { log } from '../lib/logger';
import {
  connectWebTransport,
  newCarrierCounters,
  readServerStreams,
  type KeyframeStreamFrame,
} from './connection';
import {
  encodeCarrierPrologue,
  encodeCarrierRecord,
  encodeStreamFrame,
  encodeTelemetryHello,
  encodeVideoChunk,
  type TelemetryHelloMessage,
} from './wire';
import {
  CAP_PARITY_CHUNKS,
  CAP_STRIPED_DELIVERY,
  encodeRelayCapabilities,
  type RelayCapabilities,
} from './parity';

function stubWebTransport(maxDatagramSize: number): void {
  vi.stubGlobal(
    'WebTransport',
    class {
      ready = Promise.resolve();
      datagrams = { maxDatagramSize };
      constructor(_url: string, _init?: WebTransportOptions) {}
    },
  );
}

const messages: string[] = [];

function loggedMessages(): string[] {
  return messages;
}

beforeEach(() => {
  messages.length = 0;
  const record = (...args: unknown[]) => {
    messages.push(String(args[0]));
  };
  vi.spyOn(log, 'info').mockImplementation(record);
  vi.spyOn(log, 'warn').mockImplementation(record);
});

afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

describe('connectWebTransport path-MTU log', () => {
  it('describes adaptive chunk sizing (not drops) when the path max is below the wire max', async () => {
    stubWebTransport(1024);
    await connectWebTransport('https://relay.test:4433/publish');
    const messages = loggedMessages();
    expect(messages.some((m) => m.includes('chunks will be sized to the path limit'))).toBe(true);
    expect(messages.some((m) => m.includes('will be dropped'))).toBe(false);
  });

  it('stays silent about the path max when it meets the wire max', async () => {
    stubWebTransport(1200);
    await connectWebTransport('https://relay.test:4433/publish');
    expect(loggedMessages().some((m) => m.includes('maxDatagramSize') && m.includes('<'))).toBe(false);
  });
});

// A WebTransport stub whose incoming uni streams deliver the given chunk
// sequences (one inner array per stream).
function wtWithStreams(streams: Uint8Array[][]): WebTransport {
  const incoming = new ReadableStream<ReadableStream<Uint8Array>>({
    start(controller) {
      for (const chunks of streams) {
        controller.enqueue(
          new ReadableStream<Uint8Array>({
            start(inner) {
              for (const c of chunks) inner.enqueue(c);
              inner.close();
            },
          }),
        );
      }
      controller.close();
    },
  });
  return { incomingUnidirectionalStreams: incoming } as unknown as WebTransport;
}

describe('readServerStreams — keyframes', () => {
  it('reports the whole StreamFrame message size as streamBytes', async () => {
    // streamBytes feeds the viewer's self-counted received bitrate; it must
    // match what the broadcaster counted when sending (msg.length: header +
    // config + payload), not just the decoded payload.
    const payload = new Uint8Array(100).fill(7);
    const msg = encodeStreamFrame({ keyframe: true, frameId: 5, timestampUs: 5000n }, new Uint8Array(0), payload);

    // One uni stream delivering the message split across two reads.
    const wt = wtWithStreams([[msg.subarray(0, 40), msg.subarray(40)]]);
    const frames: KeyframeStreamFrame[] = [];
    const counters = newCarrierCounters();
    await readServerStreams(wt, { onKeyframe: (kf) => frames.push(kf), onCarrierRecord: () => {} }, counters);

    expect(frames).toHaveLength(1);
    expect(frames[0].frameId).toBe(5);
    expect(frames[0].data).toEqual(payload);
    expect(frames[0].streamBytes).toBe(msg.length);
    expect(counters.streamsOpened).toBe(0); // keyframes are not carriers
    expect(counters.malformed).toBe(0);
  });
});

describe('readServerStreams — in-flight task pruning (INGEST-1)', () => {
  it('keeps the working set proportional to open streams, not session length', async () => {
    // A long mobile session accepts thousands of short-lived server streams
    // (keyframe + carrier per GOP). Each must be dropped from the in-flight
    // set the moment it settles; the pre-fix code kept one settled promise per
    // stream for the whole session, an unbounded slow leak that bites the
    // hours-long viewers resilient mode targets.
    const N = 40;
    const kf = () =>
      encodeStreamFrame({ keyframe: true, frameId: 1, timestampUs: 0n }, new Uint8Array(0), new Uint8Array(4).fill(1));

    let delivered = 0;
    const incoming = new ReadableStream<ReadableStream<Uint8Array>>({
      // A macrotask gap between accepts drains the microtask queue, so the
      // previously-accepted stream's task fully settles (and, once fixed,
      // prunes) before the next stream is accepted. The observed peak then
      // reflects true concurrency rather than timing luck.
      pull(controller) {
        return new Promise<void>((resolve) => {
          setTimeout(() => {
            if (delivered < N) {
              delivered++;
              controller.enqueue(
                new ReadableStream<Uint8Array>({
                  start(inner) {
                    inner.enqueue(kf());
                    inner.close();
                  },
                }),
              );
            } else {
              controller.close();
            }
            resolve();
          }, 0);
        });
      },
    });
    const wt = { incomingUnidirectionalStreams: incoming } as unknown as WebTransport;

    let peakInFlight = 0;
    let kfCount = 0;
    const counters = newCarrierCounters();
    await readServerStreams(
      wt,
      { onKeyframe: () => kfCount++, onCarrierRecord: () => {} },
      counters,
      undefined,
      { onInFlightChange: (n) => (peakInFlight = Math.max(peakInFlight, n)) },
    );

    // Every stream was actually read to completion...
    expect(kfCount).toBe(N);
    // ...yet the transport never held more than a couple of tasks at once.
    // Pre-fix this reached N (the array never shrank).
    expect(peakInFlight).toBeLessThanOrEqual(2);
  });
});

describe('readServerStreams — reliable carriers (R19)', () => {
  const dgram = (frameId: number) =>
    encodeVideoChunk(
      { keyframe: false, frameId, chunkIndex: 0, chunkCount: 1, timestampUs: BigInt(frameId) * 1000n },
      new Uint8Array([1, 2, 3]),
    );

  function carrierChunks(dgrams: Uint8Array[]): Uint8Array {
    let out = encodeCarrierPrologue() as Uint8Array;
    for (const d of dgrams) {
      const rec = encodeCarrierRecord(d);
      const next = new Uint8Array(out.length + rec.length);
      next.set(out, 0);
      next.set(rec, out.length);
      out = next;
    }
    return out;
  }

  it('feeds each record to the datagram handler, byte-identical and in order', async () => {
    const d1 = dgram(1);
    const d2 = dgram(2);
    const stream = carrierChunks([d1, d2]);
    // Split mid-record to prove reassembly across reads.
    const wt = wtWithStreams([[stream.subarray(0, 7), stream.subarray(7)]]);

    const records: Uint8Array[] = [];
    const counters = newCarrierCounters();
    await readServerStreams(
      wt,
      { onKeyframe: () => {}, onCarrierRecord: (r) => records.push(r) },
      counters,
    );

    expect(records.map((r) => Array.from(r))).toEqual([Array.from(d1), Array.from(d2)]);
    expect(counters.streamsOpened).toBe(1);
    expect(counters.recordsReceived).toBe(2);
    expect(counters.streamsAborted).toBe(0);
    expect(counters.malformed).toBe(0);
  });

  it('dispatches interleaved keyframe and carrier streams by their kind bytes', async () => {
    const payload = new Uint8Array(10).fill(9);
    const kfMsg = encodeStreamFrame({ keyframe: true, frameId: 3, timestampUs: 0n }, new Uint8Array(0), payload);
    const d = dgram(4);
    const wt = wtWithStreams([[carrierChunks([d])], [kfMsg]]);

    const frames: KeyframeStreamFrame[] = [];
    const records: Uint8Array[] = [];
    const counters = newCarrierCounters();
    await readServerStreams(
      wt,
      { onKeyframe: (kf) => frames.push(kf), onCarrierRecord: (r) => records.push(r) },
      counters,
    );

    expect(frames).toHaveLength(1);
    expect(frames[0].frameId).toBe(3);
    expect(records).toHaveLength(1);
    expect(counters.streamsOpened).toBe(1);
  });

  it('counts a malformed record length and abandons the stream without wedging the loop', async () => {
    const bad = new Uint8Array([0x01, 0x0a, 0x00, 0x00, 0xff]); // record length 0
    const good = carrierChunks([dgram(1)]);
    const wt = wtWithStreams([[bad], [good]]);

    const records: Uint8Array[] = [];
    const counters = newCarrierCounters();
    await readServerStreams(
      wt,
      { onKeyframe: () => {}, onCarrierRecord: (r) => records.push(r) },
      counters,
    );

    expect(counters.malformed).toBe(1);
    // The healthy carrier still delivered.
    expect(records).toHaveLength(1);
    expect(counters.recordsReceived).toBe(1);
  });

  it('counts an unknown stream kind as malformed and keeps accepting', async () => {
    const unknown = new Uint8Array([0x01, 0x7f, 0xde, 0xad]);
    const good = carrierChunks([dgram(1)]);
    const wt = wtWithStreams([[unknown], [good]]);

    const counters = newCarrierCounters();
    const records: Uint8Array[] = [];
    await readServerStreams(
      wt,
      { onKeyframe: () => {}, onCarrierRecord: (r) => records.push(r) },
      counters,
    );

    expect(counters.malformed).toBe(1);
    expect(records).toHaveLength(1);
  });

  it('counts a clean EOF mid-record as malformed (relay only closes at record boundaries)', async () => {
    const truncated = carrierChunks([dgram(1)]).subarray(0, 10);
    const wt = wtWithStreams([[truncated]]);

    const counters = newCarrierCounters();
    await readServerStreams(wt, { onKeyframe: () => {}, onCarrierRecord: () => {} }, counters);

    expect(counters.streamsOpened).toBe(1);
    expect(counters.malformed).toBe(1);
    expect(counters.recordsReceived).toBe(0);
  });

  it('counts a reset carrier as aborted (the relay shedding a stalled GOP tail)', async () => {
    const d = dgram(1);
    const prefix = carrierChunks([d]);
    // Pull-based: the chunk must be consumed before the error lands
    // (controller.error discards queued chunks).
    let pulls = 0;
    const erroring = new ReadableStream<Uint8Array>({
      pull(controller) {
        if (pulls++ === 0) controller.enqueue(prefix);
        else controller.error(new Error('stream reset'));
      },
    });
    const incoming = new ReadableStream<ReadableStream<Uint8Array>>({
      start(controller) {
        controller.enqueue(erroring);
        controller.close();
      },
    });
    const wt = { incomingUnidirectionalStreams: incoming } as unknown as WebTransport;

    const records: Uint8Array[] = [];
    const counters = newCarrierCounters();
    await readServerStreams(
      wt,
      { onKeyframe: () => {}, onCarrierRecord: (r) => records.push(r) },
      counters,
    );

    // Records before the reset still delivered; the reset itself is counted.
    expect(records).toHaveLength(1);
    expect(counters.streamsAborted).toBe(1);
    expect(counters.malformed).toBe(0);
  });
});

// R28 (docs/33 D2): the telemetry hello arrives as its own server uni stream,
// and must be dispatched by its wire type rather than mistaken for a keyframe
// or a carrier — the three now share the same accept loop.
describe('readServerStreams — telemetry hello (R28)', () => {
  const hello = () =>
    encodeTelemetryHello({
      enabled: true,
      reportIntervalMs: 2000,
      token: '00012345000102030405060708090a0ba1a2a3a4a5a6a7a8',
      broadcastKey: '1a2b3c4d5e6f',
    });

  it('dispatches a hello to its own callback, never to the media path', async () => {
    const wt = wtWithStreams([[hello()]]);
    const hellos: TelemetryHelloMessage[] = [];
    const keyframes: KeyframeStreamFrame[] = [];
    const records: Uint8Array[] = [];
    const counters = newCarrierCounters();
    await readServerStreams(
      wt,
      {
        onKeyframe: (kf) => keyframes.push(kf),
        onCarrierRecord: (r) => records.push(r),
        onTelemetryHello: (h) => hellos.push(h),
      },
      counters,
    );

    expect(hellos).toEqual([
      {
        enabled: true,
        reportIntervalMs: 2000,
        token: '00012345000102030405060708090a0ba1a2a3a4a5a6a7a8',
        broadcastKey: '1a2b3c4d5e6f',
      },
    ]);
    expect(keyframes).toHaveLength(0);
    expect(records).toHaveLength(0);
    expect(counters.malformed).toBe(0);
    expect(counters.streamsOpened).toBe(0);
  });

  it('reassembles a hello split across reads', async () => {
    const msg = hello();
    const wt = wtWithStreams([[msg.subarray(0, 3), msg.subarray(3, 20), msg.subarray(20)]]);
    const hellos: TelemetryHelloMessage[] = [];
    await readServerStreams(
      wt,
      { onKeyframe: () => {}, onCarrierRecord: () => {}, onTelemetryHello: (h) => hellos.push(h) },
      newCarrierCounters(),
    );
    expect(hellos).toHaveLength(1);
  });

  // A viewer that does not do telemetry (or an unreadable hello) must lose
  // nothing but telemetry: the media path is untouched and the session runs on.
  it('survives a malformed hello and keeps serving keyframes', async () => {
    const truncated = hello().subarray(0, 20);
    const kf = encodeStreamFrame(
      { keyframe: true, frameId: 9, timestampUs: 900n },
      new Uint8Array(0),
      new Uint8Array(8).fill(3),
    );
    const wt = wtWithStreams([[truncated], [kf]]);
    const keyframes: KeyframeStreamFrame[] = [];
    const hellos: TelemetryHelloMessage[] = [];
    const counters = newCarrierCounters();
    await readServerStreams(
      wt,
      {
        onKeyframe: (k) => keyframes.push(k),
        onCarrierRecord: () => {},
        onTelemetryHello: (h) => hellos.push(h),
      },
      counters,
    );
    expect(hellos).toHaveLength(0);
    expect(counters.malformed).toBe(1);
    expect(keyframes).toHaveLength(1);
    expect(keyframes[0].frameId).toBe(9);
  });

  it('ignores a hello when the consumer does not do telemetry', async () => {
    const wt = wtWithStreams([[hello()]]);
    const counters = newCarrierCounters();
    await readServerStreams(wt, { onKeyframe: () => {}, onCarrierRecord: () => {} }, counters);
    expect(counters.malformed).toBe(0);
  });
});

// R30 ST4, docs/35 §12 finding 1: the relay has sent RelayCapabilities on the
// subscribe route since R29 (whenever parity is enabled — the fleet default),
// but the viewer had no 0x0F branch: every production viewer session counted
// one malformed stream and logged a warning. The capability is also R30's
// version-skew gate, so this is both the wart fix and the striping enabler.
describe('readServerStreams — relay capabilities (R29/R30)', () => {
  it('dispatches capabilities to their own callback instead of counting malformed', async () => {
    const caps = encodeRelayCapabilities({
      flags: CAP_PARITY_CHUNKS | CAP_STRIPED_DELIVERY,
      parityLevel: 2,
    });
    const wt = wtWithStreams([[caps]]);
    const seen: RelayCapabilities[] = [];
    const counters = newCarrierCounters();
    await readServerStreams(
      wt,
      {
        onKeyframe: () => {},
        onCarrierRecord: () => {},
        onRelayCapabilities: (c) => seen.push(c),
      },
      counters,
    );
    expect(seen).toEqual([{ flags: CAP_PARITY_CHUNKS | CAP_STRIPED_DELIVERY, parityLevel: 2 }]);
    expect(counters.malformed).toBe(0);
  });

  it('tolerates capabilities with no callback wired (pre-R30 caller shape)', async () => {
    const caps = encodeRelayCapabilities({ flags: CAP_PARITY_CHUNKS, parityLevel: 2 });
    const wt = wtWithStreams([[caps]]);
    const counters = newCarrierCounters();
    await readServerStreams(wt, { onKeyframe: () => {}, onCarrierRecord: () => {} }, counters);
    expect(counters.malformed).toBe(0);
  });

  it('still counts a truncated capabilities stream as malformed', async () => {
    const caps = encodeRelayCapabilities({ flags: CAP_PARITY_CHUNKS, parityLevel: 2 });
    const wt = wtWithStreams([[caps.subarray(0, 4)]]);
    const seen: RelayCapabilities[] = [];
    const counters = newCarrierCounters();
    await readServerStreams(
      wt,
      { onKeyframe: () => {}, onCarrierRecord: () => {}, onRelayCapabilities: (c) => seen.push(c) },
      counters,
    );
    expect(seen).toHaveLength(0);
    expect(counters.malformed).toBe(1);
  });
});
