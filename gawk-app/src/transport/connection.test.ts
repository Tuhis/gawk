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
import { encodeCarrierPrologue, encodeCarrierRecord, encodeStreamFrame, encodeVideoChunk } from './wire';

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
