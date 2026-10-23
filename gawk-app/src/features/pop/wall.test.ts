// @vitest-environment jsdom
//
// docs/76 item 34: a party broadcaster's Hide and Show for the party wall. The
// command goes through the PoP on the broadcaster's own control session to
// the wall, carrying the broadcast's resume token as proof; a remembered Hide
// travels as wall=hidden on the next /publish.
import { beforeEach, describe, expect, it } from 'vitest';

import type { RoomSessionCallbacks, RoomSessionOptions } from '../../transport/room-session';
import { ROOM_CLIENT_WEB_BROADCASTER, ROOM_EVENT_COMMAND_REJECTED, type RoomState } from '../../transport/wire';
import { WALL_HIDDEN_KEY, isWallHidden, sendWallCommand, setWallHidden, type WallSessionFactory } from './wall';

class FakeSession {
  sent: string[] = [];
  stopped = false;
  readonly opts: RoomSessionOptions;
  readonly cb: RoomSessionCallbacks;
  constructor(opts: RoomSessionOptions, cb: RoomSessionCallbacks) {
    this.opts = opts;
    this.cb = cb;
  }
  async start() {
    this.cb.onConnected();
  }
  stop() {
    this.stopped = true;
  }
  attach(id: string, token: string, label: string) {
    this.sent.push(`attach ${id} ${token} ${label}`);
  }
  detach(id: string) {
    this.sent.push(`detach ${id}`);
  }
}

function factory(made: FakeSession[]): WallSessionFactory {
  return (opts, cb) => {
    const s = new FakeSession(opts, cb);
    made.push(s);
    return s;
  };
}

const STATE = { seq: 1 } as unknown as RoomState;
const TOKEN = 'ab'.repeat(16);
const base = {
  serverUrl: 'https://192.168.1.20:4433',
  certHashHex: '11'.repeat(32),
  wallCode: 'assembly-2026',
  broadcastId: 'K7XQ2M',
  resumeTokenHex: TOKEN,
  label: 'tuhis',
};

beforeEach(() => localStorage.clear());

describe('sendWallCommand', () => {
  it('Hide: joins the wall with the resume-token proof, detaches its own broadcast, then leaves', async () => {
    const made: FakeSession[] = [];
    const done = sendWallCommand({ ...base, hide: true }, factory(made), 0);
    await Promise.resolve();
    expect(made).toHaveLength(1);
    expect(made[0].opts.target).toEqual({ kind: 'join', code: 'assembly-2026' });
    expect(made[0].opts.proof).toEqual({ broadcastId: 'K7XQ2M', resumeTokenHex: TOKEN });
    expect(made[0].opts.clientKind).toBe(ROOM_CLIENT_WEB_BROADCASTER);
    made[0].cb.onState(STATE);
    expect(await done).toBe(true);
    expect(made[0].sent).toEqual(['detach K7XQ2M']);
    expect(made[0].stopped).toBe(true);
  });

  it('Show: attaches with the token and label', async () => {
    const made: FakeSession[] = [];
    const done = sendWallCommand({ ...base, hide: false }, factory(made), 0);
    await Promise.resolve();
    made[0].cb.onState(STATE);
    expect(await done).toBe(true);
    expect(made[0].sent).toEqual([`attach K7XQ2M ${TOKEN} tuhis`]);
  });

  it('reports a refused command, and a session that never got in', async () => {
    const made: FakeSession[] = [];
    const refused = sendWallCommand({ ...base, hide: true }, factory(made), 50);
    await Promise.resolve();
    made[0].cb.onState(STATE);
    made[0].cb.onEvent({ kind: ROOM_EVENT_COMMAND_REJECTED, seq: 1, command: 2, reason: 3, message: 'no' } as never);
    expect(await refused).toBe(false);

    const failed = sendWallCommand({ ...base, hide: true }, factory(made), 50);
    await Promise.resolve();
    made[1].cb.onError({ kind: 'refused', message: 'x' } as never);
    expect(await failed).toBe(false);
  });
});

describe('the remembered Hide', () => {
  it('is kept per wall code', () => {
    expect(isWallHidden('assembly-2026')).toBe(false);
    setWallHidden('Assembly-2026', true);
    expect(isWallHidden('assembly-2026')).toBe(true);
    expect(isWallHidden('other')).toBe(false);
    expect(JSON.parse(localStorage.getItem(WALL_HIDDEN_KEY) ?? '[]')).toEqual(['assembly-2026']);
    setWallHidden('assembly-2026', false);
    expect(isWallHidden('assembly-2026')).toBe(false);
  });
});
