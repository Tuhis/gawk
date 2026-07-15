// connectWebTransport path-MTU logging. A path maxDatagramSize below the
// wire MAX_DATAGRAM_SIZE (Firefox negotiates 1024) is a *handled* condition:
// packetizeFrame sizes chunks to the actual path limit (docs/11), so the log
// must describe the adaptation — the pre-fix "will be dropped" wording sent a
// real debugging session chasing a bug that no longer exists.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { log } from '../lib/logger';
import { connectWebTransport } from './connection';

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
