// The room control session against a scripted WebTransport: hello on the
// one bidi stream, state replace, event delivery with sequence checking,
// resync on a gap, 4007 terminal, everything else reconnecting with the
// shared ladder and a re-sent hello (nickname remembered, attach re-sent).

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { RoomConnectError, RoomSession, type RoomSessionCallbacks } from './room-session';
import {
  CLOSE_CODE_ROOM_ENDED,
  CLOSE_CODE_SERVER_DRAINING,
  ROOM_CLIENT_WEB_BROADCASTER,
  ROOM_CLIENT_WEB_VIEWER,
  ROOM_COMMAND_ATTACH,
  ROOM_COMMAND_RESYNC,
  ROOM_COMMAND_SET_NICKNAME,
  ROOM_END_REASON_CREATOR,
  ROOM_EVENT_COMMAND_REJECTED,
  ROOM_EVENT_PARTICIPANT_JOINED,
  ROOM_EVENT_PARTICIPANT_LEFT,
  ROOM_EVENT_ROOM_ENDING,
  ROOM_REJECT_LIMIT,
  ROOM_STATE_FLAG_CREATOR,
  ROOM_STATE_FLAG_DYNAMIC,
  RoomRecordReader,
  TYPE_ROOM_COMMAND,
  TYPE_ROOM_HELLO,
  encodeRoomEvent,
  encodeRoomRecord,
  encodeRoomState,
  parseRoomCommand,
  parseRoomHello,
  type RoomEvent,
  type RoomState,
} from './wire';
import { ABRUPT_DROP_RETRY_DELAY_MS } from './reconnect';

// ---- A scripted WebTransport --------------------------------------------

interface FakeWt {
  url: string;
  init: WebTransportOptions | undefined;
  // What the client wrote, parsed back into records.
  written: Uint8Array[];
  push(record: Uint8Array): void;
  // End the session: the readable closes and wt.closed settles with the code.
  end(closeCode?: number, reason?: string): void;
  // Break it: wt.closed rejects.
  crash(): void;
  closeCalls: number;
}

const fakes: FakeWt[] = [];
// When set, the next constructed session's `ready` rejects (pre-upgrade
// refusal — the browser hides the status).
let refuseNext = false;
// When set, every constructed session's `ready` rejects (a dead relay).
let refuseAll = false;

function installFakeWebTransport() {
  vi.stubGlobal(
    'WebTransport',
    class {
      ready: Promise<void>;
      closed: Promise<{ closeCode?: number; reason?: string }>;
      private resolveClosed!: (v: { closeCode?: number; reason?: string }) => void;
      private rejectClosed!: (e: unknown) => void;
      private readableCtl!: ReadableStreamDefaultController<Uint8Array>;
      private readable: ReadableStream<Uint8Array>;
      private fake: FakeWt;
      constructor(url: string, init?: WebTransportOptions) {
        this.closed = new Promise((res, rej) => {
          this.resolveClosed = res;
          this.rejectClosed = rej;
        });
        this.readable = new ReadableStream<Uint8Array>({
          start: (c) => {
            this.readableCtl = c;
          },
        });
        const written: Uint8Array[] = [];
        const reader = new RoomRecordReader();
        const fake: FakeWt = {
          url,
          init,
          written,
          closeCalls: 0,
          push: (rec) => this.readableCtl.enqueue(encodeRoomRecord(rec)),
          end: (closeCode, reason) => {
            try {
              this.readableCtl.close();
            } catch {
              // already closed
            }
            this.resolveClosed(closeCode === undefined ? {} : { closeCode, reason });
          },
          crash: () => {
            try {
              this.readableCtl.error(new Error('reset'));
            } catch {
              // already closed
            }
            this.rejectClosed(new Error('connection lost'));
          },
        };
        this.fake = fake;
        this.writable = new WritableStream<Uint8Array>({
          write(chunk) {
            for (const rec of reader.push(chunk)) written.push(rec);
          },
        });
        fakes.push(fake);
        if (refuseNext || refuseAll) {
          refuseNext = false;
          this.ready = Promise.reject(new Error('WebTransport connection rejected'));
          this.ready.catch(() => {});
          this.closed.catch(() => {});
          this.rejectClosed(new Error('rejected'));
        } else {
          this.ready = Promise.resolve();
        }
      }
      private writable: WritableStream<Uint8Array>;
      async createBidirectionalStream() {
        return { readable: this.readable, writable: this.writable };
      }
      close() {
        this.fake.closeCalls++;
        this.fake.end();
      }
    },
  );
}

// Fake timers are on: advancing by 0 drains microtasks AND the zero-delay
// timers the session's read loop / reconnect scheduling use.
const flush = () => vi.advanceTimersByTimeAsync(0);

function baseState(over: Partial<RoomState> = {}): RoomState {
  return {
    flags: 0,
    caps: 0,
    seq: 10,
    yourId: 2,
    code: 'TuhisRoom',
    displayName: '',
    creatorToken: new Uint8Array(0),
    key: new Uint8Array([1, 2, 3, 4, 5, 6]),
    attachments: [],
    participants: [{ id: 2, kind: ROOM_CLIENT_WEB_VIEWER, flags: 0, nickname: 'me', identity: '' }],
    ...over,
  };
}

function joined(seq: number, id: number): RoomEvent {
  return {
    seq,
    kind: ROOM_EVENT_PARTICIPANT_JOINED,
    participant: { id, kind: ROOM_CLIENT_WEB_VIEWER, flags: 0, nickname: `p${id}`, identity: '' },
  };
}

function makeCallbacks() {
  const events: RoomEvent[] = [];
  const states: RoomState[] = [];
  const cb: RoomSessionCallbacks = {
    onConnected: vi.fn(),
    onState: (s) => states.push(s),
    onEvent: (e) => events.push(e),
    onReconnecting: vi.fn(),
    onEnded: vi.fn(),
    onError: vi.fn(),
  };
  return { cb, events, states };
}

function commandsOf(wt: FakeWt) {
  return wt.written.filter((r) => r[1] === TYPE_ROOM_COMMAND).map((r) => parseRoomCommand(r));
}

beforeEach(() => {
  fakes.length = 0;
  refuseNext = false;
  refuseAll = false;
  installFakeWebTransport();
  vi.useFakeTimers();
});

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe('RoomSession dial + hello', () => {
  it('dials /room/<code> with the grant and cert hash, and sends the hello first', async () => {
    const { cb } = makeCallbacks();
    const s = new RoomSession(
      {
        serverUrl: 'https://relay.test:4433/',
        certHashHex: 'ab'.repeat(32),
        target: { kind: 'join', code: 'TuhisRoom' },
        nickname: 'tuhis',
        clientKind: ROOM_CLIENT_WEB_VIEWER,
        grant: { kind: 'attach', secret: 's3cret' },
      },
      cb,
    );
    await s.start();
    await flush();
    expect(fakes).toHaveLength(1);
    const wt = fakes[0];
    expect(wt.url).toBe('https://relay.test:4433/room/TuhisRoom?name=tuhis&attach=s3cret');
    expect(wt.init?.serverCertificateHashes?.[0]?.algorithm).toBe('sha-256');
    expect(cb.onConnected).toHaveBeenCalledTimes(1);
    expect(wt.written[0][1]).toBe(TYPE_ROOM_HELLO);
    expect(parseRoomHello(wt.written[0])).toEqual({
      protocol: 1,
      clientKind: ROOM_CLIENT_WEB_VIEWER,
      wantCaps: 0,
      nickname: 'tuhis',
    });
    s.stop();
  });

  it('a mint dials /room/new with the proof, then becomes a join holding the creator token', async () => {
    const { cb, states } = makeCallbacks();
    const s = new RoomSession(
      {
        serverUrl: 'https://relay.test:4433',
        target: { kind: 'mint', broadcastId: 'AB2CD3', resumeTokenHex: 'a0'.repeat(16), label: 'tuhis' },
        nickname: 'tuhis',
        clientKind: ROOM_CLIENT_WEB_BROADCASTER,
      },
      cb,
    );
    await s.start();
    await flush();
    expect(fakes[0].url).toBe(
      `https://relay.test:4433/room/new?name=tuhis&broadcast=AB2CD3&resume=${'a0'.repeat(16)}&label=tuhis`,
    );
    const token = new Uint8Array(16).map((_, i) => i);
    fakes[0].push(
      encodeRoomState(
        baseState({
          flags: ROOM_STATE_FLAG_DYNAMIC | ROOM_STATE_FLAG_CREATOR,
          code: '5UP4XW',
          creatorToken: token,
        }),
      ),
    );
    await flush();
    expect(states).toHaveLength(1);
    expect(s.code).toBe('5UP4XW');

    // A reconnect now dials the minted code with the creator token.
    fakes[0].crash();
    await flush();
    await vi.advanceTimersByTimeAsync(ABRUPT_DROP_RETRY_DELAY_MS);
    await flush();
    expect(fakes).toHaveLength(2);
    expect(fakes[1].url).toBe('https://relay.test:4433/room/5UP4XW?name=tuhis&creator=000102030405060708090a0b0c0d0e0f');
    s.stop();
  });

  it('a first dial the relay refuses rejects start() as "not found or refused"', async () => {
    refuseNext = true;
    const { cb } = makeCallbacks();
    const s = new RoomSession(
      {
        serverUrl: 'https://relay.test:4433',
        target: { kind: 'join', code: 'NOPE99' },
        nickname: '',
        clientKind: ROOM_CLIENT_WEB_VIEWER,
      },
      cb,
    );
    const err = await s.start().catch((e: unknown) => e);
    expect(err).toBeInstanceOf(RoomConnectError);
    expect(err).toMatchObject({ kind: 'refused' });
    expect(cb.onError).not.toHaveBeenCalled();
  });

  it('a post-upgrade close before any state maps the status code to a failure kind', async () => {
    for (const [code, kind] of [
      [429, 'full'],
      [404, 'notFound'],
      [403, 'forbidden'],
      [400, 'refused'],
    ] as const) {
      fakes.length = 0;
      const { cb } = makeCallbacks();
      const s = new RoomSession(
        {
          serverUrl: 'https://relay.test:4433',
          target: { kind: 'join', code: 'AB2CD3' },
          nickname: 'x',
          clientKind: ROOM_CLIENT_WEB_VIEWER,
        },
        cb,
      );
      await s.start();
      await flush();
      fakes[0].end(code, 'refused');
      await flush();
      await vi.advanceTimersByTimeAsync(300);
      expect(cb.onError).toHaveBeenCalledTimes(1);
      expect((cb.onError as ReturnType<typeof vi.fn>).mock.calls[0][0]).toMatchObject({ kind, closeCode: code });
      expect(cb.onReconnecting).not.toHaveBeenCalled();
    }
  });
});

describe('RoomSession state + events', () => {
  async function connected() {
    const made = makeCallbacks();
    const s = new RoomSession(
      {
        serverUrl: 'https://relay.test:4433',
        target: { kind: 'join', code: 'TuhisRoom' },
        nickname: 'me',
        clientKind: ROOM_CLIENT_WEB_VIEWER,
      },
      made.cb,
    );
    await s.start();
    await flush();
    fakes[0].push(encodeRoomState(baseState({ seq: 10 })));
    await flush();
    return { s, ...made, wt: fakes[0] };
  }

  it('delivers the state and in-order events', async () => {
    const { s, states, events, wt } = await connected();
    expect(states).toHaveLength(1);
    wt.push(encodeRoomEvent(joined(11, 3)));
    wt.push(encodeRoomEvent({ seq: 12, kind: ROOM_EVENT_PARTICIPANT_LEFT, participant: { id: 3 } }));
    await flush();
    expect(events.map((e) => e.seq)).toEqual([11, 12]);
    s.stop();
  });

  it('sends Resync on a sequence gap and ignores deltas until the next state', async () => {
    const { s, states, events, wt } = await connected();
    wt.push(encodeRoomEvent(joined(11, 3)));
    wt.push(encodeRoomEvent(joined(13, 5))); // 12 missed
    await flush();
    expect(events.map((e) => e.seq)).toEqual([11]);
    const cmds = commandsOf(wt);
    expect(cmds.at(-1)).toEqual({ kind: ROOM_COMMAND_RESYNC });
    // Deltas in the resync window are dropped; the snapshot replaces.
    wt.push(encodeRoomEvent(joined(14, 6)));
    wt.push(encodeRoomState(baseState({ seq: 14 })));
    wt.push(encodeRoomEvent(joined(15, 7)));
    await flush();
    expect(states).toHaveLength(2);
    expect(events.map((e) => e.seq)).toEqual([11, 15]);
    // Exactly one Resync for one gap.
    expect(commandsOf(wt).filter((c) => c.kind === ROOM_COMMAND_RESYNC)).toHaveLength(1);
    s.stop();
  });

  it('a CommandRejected carries the current seq and is delivered without advancing it', async () => {
    const { s, events, wt } = await connected();
    wt.push(
      encodeRoomEvent({
        seq: 10,
        kind: ROOM_EVENT_COMMAND_REJECTED,
        command: ROOM_COMMAND_ATTACH,
        reason: ROOM_REJECT_LIMIT,
        message: 'room full',
      }),
    );
    wt.push(encodeRoomEvent(joined(11, 3)));
    await flush();
    expect(events.map((e) => e.kind)).toEqual([ROOM_EVENT_COMMAND_REJECTED, ROOM_EVENT_PARTICIPANT_JOINED]);
    expect(commandsOf(wt).some((c) => c.kind === ROOM_COMMAND_RESYNC)).toBe(false);
    s.stop();
  });

  it('a duplicate delta is dropped', async () => {
    const { s, events, wt } = await connected();
    wt.push(encodeRoomEvent(joined(11, 3)));
    wt.push(encodeRoomEvent(joined(11, 3)));
    await flush();
    expect(events).toHaveLength(1);
    s.stop();
  });
});

describe('RoomSession endings', () => {
  async function connected() {
    const made = makeCallbacks();
    const s = new RoomSession(
      {
        serverUrl: 'https://relay.test:4433',
        target: { kind: 'join', code: 'TuhisRoom' },
        nickname: 'me',
        clientKind: ROOM_CLIENT_WEB_BROADCASTER,
      },
      made.cb,
    );
    await s.start();
    await flush();
    fakes[0].push(encodeRoomState(baseState({ seq: 10 })));
    await flush();
    return { s, ...made };
  }

  it('4007 is terminal: onEnded with the RoomEnding reason, no reconnect', async () => {
    const { s, cb } = await connected();
    fakes[0].push(encodeRoomEvent({ seq: 11, kind: ROOM_EVENT_ROOM_ENDING, reason: ROOM_END_REASON_CREATOR }));
    await flush();
    fakes[0].end(CLOSE_CODE_ROOM_ENDED, 'room ended');
    await flush();
    await vi.advanceTimersByTimeAsync(20_000);
    expect(cb.onEnded).toHaveBeenCalledWith(ROOM_END_REASON_CREATOR);
    expect(cb.onReconnecting).not.toHaveBeenCalled();
    expect(fakes).toHaveLength(1);
    s.stop();
  });

  it('4002 reconnects immediately, re-sending the hello with the remembered nickname and the attach', async () => {
    const { s, cb } = await connected();
    s.setNickname('renamed');
    s.attach('AB2CD3', 'a0'.repeat(16), 'my pov');
    await flush();
    expect(commandsOf(fakes[0]).map((c) => c.kind)).toEqual([ROOM_COMMAND_SET_NICKNAME, ROOM_COMMAND_ATTACH]);

    fakes[0].end(CLOSE_CODE_SERVER_DRAINING, 'draining');
    await flush();
    expect(cb.onReconnecting).toHaveBeenCalledWith(
      expect.objectContaining({ attempt: 1, delayMs: 0, closeCode: CLOSE_CODE_SERVER_DRAINING }),
    );
    await vi.advanceTimersByTimeAsync(0);
    await flush();
    expect(fakes).toHaveLength(2);
    expect(fakes[1].url).toContain('name=renamed');
    expect(parseRoomHello(fakes[1].written[0]).nickname).toBe('renamed');
    // The attach travels again on the new connection, before any state.
    const cmds = commandsOf(fakes[1]);
    expect(cmds).toHaveLength(1);
    expect(cmds[0]).toMatchObject({ kind: ROOM_COMMAND_ATTACH, broadcastId: 'AB2CD3', label: 'my pov' });
    // And a detach clears the auto re-attach.
    s.detach('AB2CD3');
    fakes[1].push(encodeRoomState(baseState({ seq: 20 })));
    await flush();
    fakes[1].crash();
    await flush();
    await vi.advanceTimersByTimeAsync(ABRUPT_DROP_RETRY_DELAY_MS);
    await flush();
    expect(fakes).toHaveLength(3);
    expect(commandsOf(fakes[2])).toHaveLength(0);
    s.stop();
  });

  it('an abrupt drop follows the shared ladder and gives up as "lost"', async () => {
    const { s, cb } = await connected();
    fakes[0].crash();
    await flush();
    expect(cb.onReconnecting).toHaveBeenCalledWith(
      expect.objectContaining({ attempt: 1, delayMs: ABRUPT_DROP_RETRY_DELAY_MS }),
    );
    // Every reconnect dial is refused: the budget burns out.
    refuseAll = true;
    for (let i = 0; i < 12; i++) {
      await vi.advanceTimersByTimeAsync(15_000);
      await flush();
    }
    expect(cb.onError).toHaveBeenCalledTimes(1);
    expect((cb.onError as ReturnType<typeof vi.fn>).mock.calls[0][0]).toMatchObject({ kind: 'lost' });
    s.stop();
  });

  it('a reconnect that lands in an ended room (404 before state) reports ended', async () => {
    const { s, cb } = await connected();
    fakes[0].crash();
    await flush();
    await vi.advanceTimersByTimeAsync(ABRUPT_DROP_RETRY_DELAY_MS);
    await flush();
    expect(fakes).toHaveLength(2);
    fakes[1].end(404, 'gone');
    await flush();
    await vi.advanceTimersByTimeAsync(300);
    expect(cb.onEnded).toHaveBeenCalledWith(null);
    expect(cb.onError).not.toHaveBeenCalled();
    s.stop();
  });

  it('stop() closes the transport and fires nothing', async () => {
    const { s, cb } = await connected();
    s.stop();
    await flush();
    expect(fakes[0].closeCalls).toBe(1);
    await vi.advanceTimersByTimeAsync(1000);
    expect(cb.onReconnecting).not.toHaveBeenCalled();
    expect(cb.onEnded).not.toHaveBeenCalled();
    expect(cb.onError).not.toHaveBeenCalled();
  });
});
