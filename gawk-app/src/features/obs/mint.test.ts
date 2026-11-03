// R33 (docs/83 D3): minting an OBS stream key through a mint-only publish.
// The dial is injected, so the fake WebTransport models only what the mint
// touches: server-opened uni streams, `closed` and close().
import { afterEach, describe, expect, it, vi } from 'vitest';
import { buildPublishUrl } from '../../transport/broadcaster';
import { CLOSE_CODE_PUBLISH_MINTED, encodeResumeToken, encodeSessionClosing } from '../../transport/wire';
import { MintError, mintObsKey } from './mint';

// Golden BroadcastAnnounce for ID K7XQ2M (broadcaster.test.ts).
const ANNOUNCE_K7XQ2M = new Uint8Array([0x01, 0x03, 0x06, 0x4b, 0x37, 0x58, 0x51, 0x32, 0x4d]);
const TOKEN = Uint8Array.from({ length: 16 }, (_, i) => i * 17);
const TOKEN_HEX = '00112233445566778899aabbccddeeff';
const RESUME = encodeResumeToken(TOKEN);
const CLOSING_4017 = encodeSessionClosing(CLOSE_CODE_PUBLISH_MINTED);

interface FakeWT {
  wt: WebTransport;
  close: ReturnType<typeof vi.fn>;
  push: (...chunks: Uint8Array[]) => void;
  end: (info: unknown) => void;
  fail: (err: unknown) => void;
}

function fakeWT(): FakeWT {
  let ctrl!: ReadableStreamDefaultController<ReadableStream<Uint8Array>>;
  let resolveClosed!: (v: unknown) => void;
  let rejectClosed!: (e: unknown) => void;
  const closed = new Promise((res, rej) => {
    resolveClosed = res;
    rejectClosed = rej;
  });
  const incomingUnidirectionalStreams = new ReadableStream<ReadableStream<Uint8Array>>({
    start(c) {
      ctrl = c;
    },
  });
  const close = vi.fn(() => resolveClosed({ closeCode: 0, reason: '' }));
  const wt = { closed, incomingUnidirectionalStreams, close } as unknown as WebTransport;
  return {
    wt,
    close,
    push: (...chunks) =>
      ctrl.enqueue(
        new ReadableStream<Uint8Array>({
          start(c) {
            for (const ch of chunks) c.enqueue(ch);
            c.close();
          },
        }),
      ),
    end: (info) => resolveClosed(info),
    fail: (err) => rejectClosed(err),
  };
}

afterEach(() => vi.useRealTimers());

describe('the mint URL', () => {
  it('dials /publish?mint=1, with the secret like a broadcast’s mint', () => {
    expect(buildPublishUrl('https://relay.test:4433', { mint: true }).replace(/&os=.*$/, '')).toMatch(
      /^https:\/\/relay\.test:4433\/publish\?mint=1&app=web/,
    );
    expect(buildPublishUrl('https://relay.test:4433', { mint: true, publishSecret: 's3cret' })).toMatch(
      /^https:\/\/relay\.test:4433\/publish\?mint=1&secret=s3cret&app=web/,
    );
  });

  it('never asks a reclaim to mint', () => {
    expect(buildPublishUrl('https://relay.test:4433', { mint: true, broadcastId: 'K7XQ2M', resumeToken: 'ab' })).not.toContain(
      'mint',
    );
  });
});

describe('mintObsKey', () => {
  it('dials with the secret and the pins, and builds gawk1_<ID>_<token> from announce + token', async () => {
    const f = fakeWT();
    const connect = vi.fn(async () => f.wt);
    const p = mintObsKey('https://relay.test:4433', { publishSecret: 's3cret', certHashHex: 'aa,bb' }, { connect });
    await vi.waitFor(() => expect(connect).toHaveBeenCalled());
    const [url, opts] = connect.mock.calls[0] as unknown as [string, unknown];
    expect(url).toMatch(/\/publish\?mint=1&secret=s3cret&app=web/);
    expect(opts).toEqual({ certHashHex: 'aa,bb', publishSecret: 's3cret' });
    // Order is not guaranteed on the wire: the token first here.
    f.push(RESUME);
    f.push(ANNOUNCE_K7XQ2M.subarray(0, 4), ANNOUNCE_K7XQ2M.subarray(4));
    await expect(p).resolves.toEqual({ key: `gawk1_K7XQ2M_${TOKEN_HEX}`, broadcastId: 'K7XQ2M', tokenHex: TOKEN_HEX });
    // Torn down on success.
    expect(f.close).toHaveBeenCalled();
  });

  it('accepts the in-band SessionClosing(4017) and the 4017 close that follow, never as an error', async () => {
    const f = fakeWT();
    const p = mintObsKey('https://relay.test', {}, { connect: async () => f.wt });
    await Promise.resolve();
    f.push(ANNOUNCE_K7XQ2M);
    f.push(RESUME);
    f.push(CLOSING_4017);
    f.end({ closeCode: CLOSE_CODE_PUBLISH_MINTED, reason: 'minted' });
    await expect(p).resolves.toMatchObject({ broadcastId: 'K7XQ2M' });
  });

  it('succeeds when the 4017 close overtakes the reads of the last messages', async () => {
    const f = fakeWT();
    const p = mintObsKey('https://relay.test', {}, { connect: async () => f.wt });
    await Promise.resolve();
    f.push(ANNOUNCE_K7XQ2M);
    f.push(RESUME);
    // Chrome often reads no close code; the session just fails.
    f.fail(Object.assign(new Error('Connection lost.'), { closeCode: undefined }));
    await expect(p).resolves.toMatchObject({ tokenHex: TOKEN_HEX });
  });

  it('fails in the session phase when the relay closes with 4017 but no token arrived', async () => {
    const f = fakeWT();
    const p = mintObsKey('https://relay.test', {}, { connect: async () => f.wt });
    await Promise.resolve();
    f.push(ANNOUNCE_K7XQ2M);
    f.push(CLOSING_4017);
    f.fail(new Error('Connection lost.'));
    const err = await p.catch((e: unknown) => e);
    expect(err).toBeInstanceOf(MintError);
    expect((err as MintError).phase).toBe('session');
    expect((err as MintError).message).toBe('the server closed the session without a stream key');
    expect(f.close).toHaveBeenCalled();
  });

  it('names another close code', async () => {
    const f = fakeWT();
    const p = mintObsKey('https://relay.test', {}, { connect: async () => f.wt });
    await Promise.resolve();
    f.end({ closeCode: 4002 });
    await expect(p).rejects.toThrow('the server closed the session (code 4002)');
  });

  // The relay refused the CONNECT (secret missing or wrong, 429, a ban,
  // draining): the browser exposes no status, only a rejected dial.
  it('fails in the connect phase when the dial is refused, with nothing to tear down', async () => {
    const connect = vi.fn(async () => {
      throw new Error('WebTransportError: Opening handshake failed.');
    });
    const err = await mintObsKey('https://relay.test', {}, { connect }).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(MintError);
    expect((err as MintError).phase).toBe('connect');
    expect((err as MintError).message).toContain('Opening handshake failed');
  });

  it('times out an open session that never sends a key, and closes it', async () => {
    vi.useFakeTimers();
    const f = fakeWT();
    const p = mintObsKey('https://relay.test', {}, { connect: async () => f.wt, timeoutMs: 1000 });
    const settled = p.catch((e: unknown) => e);
    await vi.advanceTimersByTimeAsync(1000);
    const err = await settled;
    expect((err as MintError).phase).toBe('session');
    expect(f.close).toHaveBeenCalled();
  });

  it('refuses a token that is not 16 bytes and tears down', async () => {
    const f = fakeWT();
    const p = mintObsKey('https://relay.test', {}, { connect: async () => f.wt });
    await Promise.resolve();
    f.push(ANNOUNCE_K7XQ2M);
    f.push(encodeResumeToken(new Uint8Array(8)));
    await expect(p).rejects.toThrow('the server sent a stream key gawk can’t use');
    expect(f.close).toHaveBeenCalled();
  });

  it('ignores the other messages a publish session carries', async () => {
    const f = fakeWT();
    const p = mintObsKey('https://relay.test', {}, { connect: async () => f.wt });
    await Promise.resolve();
    f.push(new Uint8Array([0x01, 0x7f, 0x00]));
    f.push(new Uint8Array([0x01])); // too short to parse: dropped
    f.push(ANNOUNCE_K7XQ2M);
    f.push(RESUME);
    await expect(p).resolves.toMatchObject({ broadcastId: 'K7XQ2M' });
  });
});
