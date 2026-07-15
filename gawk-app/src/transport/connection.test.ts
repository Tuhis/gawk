// connectWebTransport path-MTU logging. A path maxDatagramSize below the
// wire MAX_DATAGRAM_SIZE (Firefox negotiates 1024) is a *handled* condition:
// packetizeFrame sizes chunks to the actual path limit (docs/11), so the log
// must describe the adaptation — the pre-fix "will be dropped" wording sent a
// real debugging session chasing a bug that no longer exists.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { log } from '../lib/logger';
import { connectWebTransport, readKeyframeStreams, type KeyframeStreamFrame } from './connection';
import { encodeStreamFrame } from './wire';

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

describe('readKeyframeStreams', () => {
  it('reports the whole StreamFrame message size as streamBytes', async () => {
    // streamBytes feeds the viewer's self-counted received bitrate; it must
    // match what the broadcaster counted when sending (msg.length: header +
    // config + payload), not just the decoded payload.
    const payload = new Uint8Array(100).fill(7);
    const msg = encodeStreamFrame({ keyframe: true, frameId: 5, timestampUs: 5000n }, new Uint8Array(0), payload);

    // One uni stream delivering the message split across two reads.
    const frameStream = new ReadableStream<Uint8Array>({
      start(controller) {
        controller.enqueue(msg.subarray(0, 40));
        controller.enqueue(msg.subarray(40));
        controller.close();
      },
    });
    const incoming = new ReadableStream<ReadableStream<Uint8Array>>({
      start(controller) {
        controller.enqueue(frameStream);
        controller.close();
      },
    });
    const wt = { incomingUnidirectionalStreams: incoming } as unknown as WebTransport;

    const frames: KeyframeStreamFrame[] = [];
    await readKeyframeStreams(wt, (kf) => frames.push(kf));

    expect(frames).toHaveLength(1);
    expect(frames[0].frameId).toBe(5);
    expect(frames[0].data).toEqual(payload);
    expect(frames[0].streamBytes).toBe(msg.length);
  });
});
