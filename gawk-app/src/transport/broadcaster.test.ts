// BroadcastPipeline start/announce behavior. connection + media modules are
// mocked so no WebTransport or WebCodecs is involved; the fake WebTransport
// only models what the pipeline touches (closed promise, incoming uni
// streams, close()).

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

const connectWebTransport = vi.fn();
const startCapture = vi.fn();
const stopCapture = vi.fn();

vi.mock('./connection', () => ({
  connectWebTransport: (...args: unknown[]) => connectWebTransport(...args),
  DatagramSender: class {
    send = vi.fn(() => Promise.resolve());
    close = vi.fn();
  },
}));

vi.mock('../media/capture', () => ({
  startCapture: (...args: unknown[]) => startCapture(...args),
  stopCapture: (...args: unknown[]) => stopCapture(...args),
}));

vi.mock('../media/encoder', () => ({
  Encoder: class {},
}));

import { BroadcastPipeline, type BroadcastCallbacks } from './broadcaster';
import { DEFAULT_CAPTURE_CONFIG } from '../media/types';

// Golden BroadcastAnnounce for ID K7XQ2M (docs/06-multi-broadcaster.md).
const ANNOUNCE_K7XQ2M = new Uint8Array([0x01, 0x03, 0x06, 0x4b, 0x37, 0x58, 0x51, 0x32, 0x4d]);

interface FakeWT {
  wt: WebTransport;
  close: ReturnType<typeof vi.fn>;
}

// announceChunks: byte chunks delivered on the first server-initiated uni
// stream; null = the stream never arrives (but the connection stays up).
function makeFakeWT(announceChunks: Uint8Array[] | null): FakeWT {
  let resolveClosed: (info: unknown) => void = () => {};
  const closed = new Promise((res) => {
    resolveClosed = res;
  });
  const incomingUnidirectionalStreams = new ReadableStream<ReadableStream<Uint8Array>>({
    start(controller) {
      if (announceChunks) {
        controller.enqueue(
          new ReadableStream<Uint8Array>({
            start(c) {
              for (const chunk of announceChunks) c.enqueue(chunk);
              c.close();
            },
          }),
        );
      }
    },
  });
  const close = vi.fn(() => resolveClosed({ closeCode: 0, reason: '' }));
  const wt = {
    closed,
    incomingUnidirectionalStreams,
    datagrams: { maxDatagramSize: 1200 },
    close,
  } as unknown as WebTransport;
  return { wt, close };
}

function makeCaptureHandle() {
  return {
    stream: {} as MediaStream,
    track: {
      getSettings: () => ({ frameRate: 30 }),
      addEventListener: vi.fn(),
    },
    capturePath: 'fake',
    startFrames: vi.fn(() => Promise.resolve()),
  };
}

// Typed per-callback so the mocks satisfy BroadcastCallbacks: vitest 4 types
// a bare vi.fn() as Mock<Procedure | Constructable>, which tsc rejects
// against a concrete callback signature.
function makeCallbacks() {
  return {
    onSourceStream: vi.fn<BroadcastCallbacks['onSourceStream']>(),
    onEncoderConfigured: vi.fn<BroadcastCallbacks['onEncoderConfigured']>(),
    onCapturePathChosen: vi.fn<BroadcastCallbacks['onCapturePathChosen']>(),
    onStats: vi.fn<BroadcastCallbacks['onStats']>(),
    onError: vi.fn<BroadcastCallbacks['onError']>(),
    onEnded: vi.fn<BroadcastCallbacks['onEnded']>(),
    onBroadcastId: vi.fn<NonNullable<BroadcastCallbacks['onBroadcastId']>>(),
  };
}

function makePipeline(cbs: BroadcastCallbacks, broadcastId?: string): BroadcastPipeline {
  return new BroadcastPipeline(
    { ...DEFAULT_CAPTURE_CONFIG },
    'https://relay.test:4433',
    {},
    cbs,
    broadcastId,
  );
}

beforeEach(() => {
  vi.stubGlobal('window', globalThis);
  connectWebTransport.mockReset();
  startCapture.mockReset();
  stopCapture.mockReset();
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('BroadcastPipeline URLs', () => {
  it('dials /publish when minting', async () => {
    const fake = makeFakeWT([ANNOUNCE_K7XQ2M]);
    connectWebTransport.mockResolvedValue(fake.wt);
    startCapture.mockResolvedValue(makeCaptureHandle());
    const pipeline = makePipeline(makeCallbacks());
    await pipeline.start();
    expect(connectWebTransport).toHaveBeenCalledWith('https://relay.test:4433/publish', {});
    await pipeline.stop();
  });

  it('dials /publish/<id> when reclaiming', async () => {
    const fake = makeFakeWT([ANNOUNCE_K7XQ2M]);
    connectWebTransport.mockResolvedValue(fake.wt);
    startCapture.mockResolvedValue(makeCaptureHandle());
    const pipeline = makePipeline(makeCallbacks(), 'K7XQ2M');
    await pipeline.start();
    expect(connectWebTransport).toHaveBeenCalledWith('https://relay.test:4433/publish/K7XQ2M', {});
    await pipeline.stop();
  });

  // R2: publishing requires the pre-shared secret; it travels as a query
  // param because the WebTransport JS API cannot set request headers.
  it('appends ?secret= when a publish secret is configured', async () => {
    const fake = makeFakeWT([ANNOUNCE_K7XQ2M]);
    connectWebTransport.mockResolvedValue(fake.wt);
    startCapture.mockResolvedValue(makeCaptureHandle());
    const opts = { publishSecret: 's3cret' };
    const pipeline = new BroadcastPipeline(
      { ...DEFAULT_CAPTURE_CONFIG },
      'https://relay.test:4433',
      opts,
      makeCallbacks(),
    );
    await pipeline.start();
    expect(connectWebTransport).toHaveBeenCalledWith('https://relay.test:4433/publish?secret=s3cret', opts);
    await pipeline.stop();
  });

  it('appends ?secret= on the reclaim URL too', async () => {
    const fake = makeFakeWT([ANNOUNCE_K7XQ2M]);
    connectWebTransport.mockResolvedValue(fake.wt);
    startCapture.mockResolvedValue(makeCaptureHandle());
    const opts = { publishSecret: 's3cret' };
    const pipeline = new BroadcastPipeline(
      { ...DEFAULT_CAPTURE_CONFIG },
      'https://relay.test:4433',
      opts,
      makeCallbacks(),
      'K7XQ2M',
    );
    await pipeline.start();
    expect(connectWebTransport).toHaveBeenCalledWith('https://relay.test:4433/publish/K7XQ2M?secret=s3cret', opts);
    await pipeline.stop();
  });
});

describe('BroadcastPipeline announce handling', () => {
  it('delivers onBroadcastId from a multi-chunk announce stream read', async () => {
    const fake = makeFakeWT([
      ANNOUNCE_K7XQ2M.subarray(0, 3),
      ANNOUNCE_K7XQ2M.subarray(3, 6),
      ANNOUNCE_K7XQ2M.subarray(6),
    ]);
    connectWebTransport.mockResolvedValue(fake.wt);
    startCapture.mockResolvedValue(makeCaptureHandle());
    const cbs = makeCallbacks();
    const pipeline = makePipeline(cbs);
    await pipeline.start();
    await vi.waitFor(() => expect(cbs.onBroadcastId).toHaveBeenCalledWith('K7XQ2M'));
    await pipeline.stop();
  });

  it('does not gate media start on the announce arriving', async () => {
    // The design locks this: capture/encode begins immediately; only the UI
    // code display waits for the announce (docs/06, "Announce vs. first
    // datagrams ordering").
    const fake = makeFakeWT(null); // announce stream never arrives
    connectWebTransport.mockResolvedValue(fake.wt);
    startCapture.mockResolvedValue(makeCaptureHandle());
    const pipeline = makePipeline(makeCallbacks());
    const outcome = await Promise.race([
      pipeline.start().then(() => 'started'),
      new Promise<string>((r) => setTimeout(() => r('gated-on-announce'), 300)),
    ]);
    expect(outcome).toBe('started');
    expect(startCapture).toHaveBeenCalled();
    await pipeline.stop();
  });
});

describe('BroadcastPipeline start failures', () => {
  it('closes the WebTransport session when capture fails after connect', async () => {
    // A user cancelling the share picker must not leak a live publisher
    // session (a zombie would hold the broadcast ID until the tab closes).
    const fake = makeFakeWT([ANNOUNCE_K7XQ2M]);
    connectWebTransport.mockResolvedValue(fake.wt);
    startCapture.mockRejectedValue(new DOMException('Permission denied', 'NotAllowedError'));
    const pipeline = makePipeline(makeCallbacks());
    await expect(pipeline.start()).rejects.toThrow();
    expect(fake.close).toHaveBeenCalled();
  });

  it('tags a capture failure with phase "capture"', async () => {
    const fake = makeFakeWT([ANNOUNCE_K7XQ2M]);
    connectWebTransport.mockResolvedValue(fake.wt);
    startCapture.mockRejectedValue(new Error('user cancelled the picker'));
    const pipeline = makePipeline(makeCallbacks());
    await expect(pipeline.start()).rejects.toMatchObject({ phase: 'capture' });
  });

  it('tags a connect failure with phase "connect"', async () => {
    connectWebTransport.mockRejectedValue(new Error('connect failed'));
    const pipeline = makePipeline(makeCallbacks());
    await expect(pipeline.start()).rejects.toMatchObject({ phase: 'connect' });
    expect(startCapture).not.toHaveBeenCalled();
  });
});
