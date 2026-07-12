// ViewerPipeline close handling. connection + decoder are mocked; the fake
// WebTransport models only what the pipeline touches (closed promise,
// close()). The datagram read loop is driven by the mocked readDatagrams.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

const connectWebTransport = vi.fn();
const readDatagrams = vi.fn();

vi.mock('./connection', () => ({
  connectWebTransport: (...args: unknown[]) => connectWebTransport(...args),
  readDatagrams: (...args: unknown[]) => readDatagrams(...args),
}));

vi.mock('../media/decoder', () => ({
  Decoder: class {
    queueSize = 0;
    configure = vi.fn();
    decode = vi.fn();
    close = vi.fn(() => Promise.resolve());
  },
}));

import { ViewerPipeline, type ViewerCallbacks } from './viewer';
import { CLOSE_CODE_BROADCAST_ENDED } from './wire';

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
  connectWebTransport.mockReset();
  readDatagrams.mockReset();
});

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
