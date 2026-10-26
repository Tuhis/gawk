// The voice session against a scripted WebTransport (docs/80 D5, D20): the
// hello first on the one bidi stream, VoiceState records in, VoiceControl
// out, VoiceFrame datagrams both ways, and the ending reported once from the
// one authoritative signal (wt.closed's code, else the in-band notice).
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  CLOSE_CODE_SERVER_DRAINING,
  CLOSE_CODE_VOICE_REMOVED,
  CLOSE_CODE_VOICE_UNAVAILABLE,
  RoomRecordReader,
  TYPE_VOICE_CONTROL,
  TYPE_VOICE_HELLO,
  VOICE_HELLO_FLAG_LISTEN_ONLY,
  encodeRoomRecord,
  encodeSessionClosing,
  encodeVoiceFrame,
  encodeVoiceState,
  parseVoiceControl,
  parseVoiceFrame,
  parseVoiceHello,
} from '../../../transport/wire';
import { CLOSE_INFO_GRACE_MS, VoiceError, VoiceSession, type VoiceCloseInfo } from './voiceSession';

interface FakeWt {
  url: string;
  init: WebTransportOptions | undefined;
  records: Uint8Array[];
  datagramsOut: Uint8Array[];
  pushRecord(msg: Uint8Array): void;
  pushRaw(bytes: Uint8Array): void;
  pushDatagram(d: Uint8Array): void;
  pushUni(msg: Uint8Array): void;
  endStream(): void;
  settleClosed(info: { closeCode?: number; reason?: string }): void;
  crash(): void;
  closeCalls: number;
}

const fakes: FakeWt[] = [];
let refuse = false;
let failStream = false;

function install() {
  vi.stubGlobal(
    'WebTransport',
    class {
      ready: Promise<void>;
      closed: Promise<{ closeCode?: number; reason?: string }>;
      datagrams: { readable: ReadableStream<Uint8Array>; writable: WritableStream<BufferSource> };
      incomingUnidirectionalStreams: ReadableStream<ReadableStream<Uint8Array>>;
      private fake: FakeWt;
      private readable: ReadableStream<Uint8Array>;
      private writable: WritableStream<Uint8Array>;
      constructor(url: string, init?: WebTransportOptions) {
        let resolveClosed!: (v: { closeCode?: number; reason?: string }) => void;
        let rejectClosed!: (e: unknown) => void;
        this.closed = new Promise((res, rej) => {
          resolveClosed = res;
          rejectClosed = rej;
        });
        let streamCtl!: ReadableStreamDefaultController<Uint8Array>;
        this.readable = new ReadableStream({ start: (c) => void (streamCtl = c) });
        let dgCtl!: ReadableStreamDefaultController<Uint8Array>;
        let uniCtl!: ReadableStreamDefaultController<ReadableStream<Uint8Array>>;
        const records: Uint8Array[] = [];
        const datagramsOut: Uint8Array[] = [];
        const reader = new RoomRecordReader();
        this.writable = new WritableStream({ write: (chunk) => void records.push(...reader.push(chunk)) });
        this.datagrams = {
          readable: new ReadableStream({ start: (c) => void (dgCtl = c) }),
          writable: new WritableStream({ write: (d) => void datagramsOut.push(new Uint8Array(d as Uint8Array)) }),
        };
        this.incomingUnidirectionalStreams = new ReadableStream({ start: (c) => void (uniCtl = c) });
        const end = () => {
          for (const c of [streamCtl, dgCtl]) {
            try {
              c.close();
            } catch {
              // closed
            }
          }
        };
        this.fake = {
          url,
          init,
          records,
          datagramsOut,
          closeCalls: 0,
          pushRecord: (msg) => streamCtl.enqueue(encodeRoomRecord(msg)),
          pushRaw: (b) => streamCtl.enqueue(b),
          pushDatagram: (d) => dgCtl.enqueue(d),
          pushUni: (msg) =>
            uniCtl.enqueue(
              new ReadableStream({
                start(c) {
                  c.enqueue(msg);
                  c.close();
                },
              }),
            ),
          endStream: () => end(),
          settleClosed: (info) => {
            end();
            resolveClosed(info);
          },
          crash: () => {
            try {
              streamCtl.error(new Error('reset'));
            } catch {
              // closed
            }
            rejectClosed(new Error('connection lost'));
          },
        };
        fakes.push(this.fake);
        if (refuse) {
          this.ready = Promise.reject(new Error('handshake refused'));
          this.ready.catch(() => {});
          rejectClosed(new Error('refused'));
        } else {
          this.ready = Promise.resolve();
        }
      }
      async createBidirectionalStream() {
        if (failStream) throw new Error('no stream credit');
        return { readable: this.readable, writable: this.writable };
      }
      close() {
        this.fake.closeCalls++;
        this.fake.settleClosed({});
      }
    },
  );
}

const flush = () => vi.advanceTimersByTimeAsync(0);
const TICKET = new Uint8Array([1, 2, 3, 4, 5]);

function makeSession(helloFlags = 0) {
  const states: unknown[] = [];
  const frames: { header: unknown; payload: Uint8Array }[] = [];
  const closed: VoiceCloseInfo[] = [];
  const s = new VoiceSession(
    {
      url: 'https://bridge.test:4443/voice',
      ticket: TICKET,
      helloFlags,
      init: { serverCertificateHashes: [{ algorithm: 'sha-256', value: new Uint8Array(32) }] },
    },
    {
      onState: (st) => states.push(st),
      onFrame: (header, payload) => frames.push({ header, payload: payload.slice() }),
      onClosed: (info) => closed.push(info),
    },
  );
  return { s, states, frames, closed };
}

beforeEach(() => {
  fakes.length = 0;
  refuse = false;
  failStream = false;
  install();
  vi.useFakeTimers();
});
afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe('VoiceSession', () => {
  it('dials the ticket URL with the pins and sends the hello first', async () => {
    const { s } = makeSession(VOICE_HELLO_FLAG_LISTEN_ONLY);
    await s.start();
    await flush();
    const wt = fakes[0];
    expect(wt.url).toBe('https://bridge.test:4443/voice');
    expect(wt.init?.serverCertificateHashes).toHaveLength(1);
    expect(wt.records[0][1]).toBe(TYPE_VOICE_HELLO);
    expect(parseVoiceHello(wt.records[0])).toEqual({ protocol: 1, flags: VOICE_HELLO_FLAG_LISTEN_ONLY, ticket: TICKET });
    s.close();
  });

  it('delivers VoiceState records and VoiceFrame datagrams, skipping what it does not know', async () => {
    const { s, states, frames } = makeSession();
    await s.start();
    fakes[0].pushRecord(encodeVoiceState({ flags: 0x20, session: 42, name: 'tuhis [web]' }));
    fakes[0].pushRecord(new Uint8Array([1, 0x7f, 0])); // unknown record type
    fakes[0].pushDatagram(encodeVoiceFrame({ end: false, target: 0, speaker: 7, seq: 3 }, new Uint8Array([9])));
    fakes[0].pushDatagram(new Uint8Array([1, 0x07, 0, 0])); // an AudioFrame: not voice
    fakes[0].pushDatagram(new Uint8Array([1, 0x1b, 0x80, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1])); // bad flags
    await flush();
    expect(states).toEqual([{ flags: 0x20, session: 42, name: 'tuhis [web]' }]);
    expect(frames).toHaveLength(1);
    expect(frames[0].header).toEqual({ end: false, target: 0, speaker: 7, seq: 3 });
    s.close();
  });

  it('sends VoiceFrame datagrams and VoiceControl records', async () => {
    const { s } = makeSession();
    await s.start();
    s.sendFrame({ end: false, target: 0, speaker: 0, seq: 12 }, new Uint8Array([0xfc]));
    s.sendFrame({ end: true, target: 0, speaker: 0, seq: 13 }, new Uint8Array(0));
    s.sendFrame({ end: false, target: 0, speaker: 0, seq: 14 }, new Uint8Array(0)); // invalid: dropped
    s.sendControl(3);
    await flush();
    const wt = fakes[0];
    expect(wt.datagramsOut.map((d) => parseVoiceFrame(d).header.seq)).toEqual([12, 13]);
    expect(parseVoiceFrame(wt.datagramsOut[1]).header.end).toBe(true);
    const ctl = wt.records.filter((r) => r[1] === TYPE_VOICE_CONTROL);
    expect(ctl.map((r) => parseVoiceControl(r))).toEqual([3]);
    s.close();
    s.sendFrame({ end: false, target: 0, speaker: 0, seq: 15 }, new Uint8Array([1]));
    s.sendControl(1);
    await flush();
    expect(wt.datagramsOut).toHaveLength(2);
  });

  it.each([CLOSE_CODE_VOICE_REMOVED, CLOSE_CODE_VOICE_UNAVAILABLE, CLOSE_CODE_SERVER_DRAINING])(
    'reports a %d close from wt.closed exactly once',
    async (code) => {
      const { s, closed } = makeSession();
      await s.start();
      fakes[0].settleClosed({ closeCode: code, reason: 'murmur is full' });
      await flush();
      await vi.advanceTimersByTimeAsync(CLOSE_INFO_GRACE_MS * 2);
      expect(closed).toEqual([{ code, reason: 'murmur is full' }]);
      expect(fakes[0].closeCalls).toBeGreaterThanOrEqual(1);
    },
  );

  // Chrome never reads a webtransport-go close code: the in-band notice is
  // what says it was a 4015.
  it('uses the in-band SessionClosing code when wt.closed settles without one', async () => {
    const { s, closed } = makeSession();
    await s.start();
    fakes[0].pushUni(encodeSessionClosing(CLOSE_CODE_VOICE_REMOVED));
    await flush();
    fakes[0].crash();
    await flush();
    await vi.advanceTimersByTimeAsync(CLOSE_INFO_GRACE_MS);
    expect(closed).toEqual([{ code: CLOSE_CODE_VOICE_REMOVED, reason: 'connection lost' }]);
  });

  it('a stream that ends first waits for wt.closed, whose code wins', async () => {
    const { s, closed } = makeSession();
    await s.start();
    fakes[0].endStream();
    await flush();
    expect(closed).toHaveLength(0);
    await vi.advanceTimersByTimeAsync(CLOSE_INFO_GRACE_MS / 2);
    fakes[0].settleClosed({ closeCode: CLOSE_CODE_VOICE_UNAVAILABLE, reason: 'autobanned' });
    await flush();
    await vi.advanceTimersByTimeAsync(CLOSE_INFO_GRACE_MS);
    expect(closed).toEqual([{ code: CLOSE_CODE_VOICE_UNAVAILABLE, reason: 'autobanned' }]);
  });

  it('a stream end with no close in the grace is a connection loss (code null)', async () => {
    const { s, closed } = makeSession();
    await s.start();
    fakes[0].endStream();
    await vi.advanceTimersByTimeAsync(CLOSE_INFO_GRACE_MS);
    expect(closed).toEqual([{ code: null, reason: 'voice stream ended' }]);
    expect(fakes[0].closeCalls).toBe(1); // the session is released
  });

  it('a malformed VoiceState ends the session as a loss', async () => {
    const { s, closed } = makeSession();
    await s.start();
    fakes[0].pushRecord(new Uint8Array([1, 0x1c, 0x80, 0, 0, 0, 0, 0]));
    await flush();
    expect(closed).toEqual([{ code: null, reason: 'malformed voice record' }]);
  });

  it('a local close reports nothing', async () => {
    const { s, closed } = makeSession();
    await s.start();
    s.close();
    s.close();
    await vi.advanceTimersByTimeAsync(CLOSE_INFO_GRACE_MS * 2);
    expect(closed).toEqual([]);
    expect(fakes[0].closeCalls).toBe(1);
  });

  it('a refused dial rejects with a connect VoiceError', async () => {
    refuse = true;
    const { s } = makeSession();
    const err = await s.start().catch((e: unknown) => e);
    expect(err).toBeInstanceOf(VoiceError);
    expect((err as VoiceError).phase).toBe('connect');
  });

  it('a stream that cannot open rejects AND closes the session it dialled', async () => {
    failStream = true;
    const { s } = makeSession();
    const err = await s.start().catch((e: unknown) => e);
    expect((err as VoiceError).phase).toBe('connect');
    expect(fakes[0].closeCalls).toBe(1);
  });

  it('closed while dialling: the late session is closed and start rejects', async () => {
    const { s } = makeSession();
    const started = s.start();
    s.close();
    const err = await started.catch((e: unknown) => e);
    expect((err as VoiceError).phase).toBe('connect');
    expect(fakes[0].closeCalls).toBe(1);
  });
});
