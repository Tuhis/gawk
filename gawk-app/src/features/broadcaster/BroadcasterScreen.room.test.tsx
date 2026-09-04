// @vitest-environment jsdom
//
// R42 RM5 (docs/44 §4.8): the broadcaster's Room panel. The publish session
// and the room control session are both faked at their transport seams; the
// test walks the real page from "Start a stream" through "New room" to the
// in-page room view, and asserts the mint carries the running broadcast's ID
// and resume token, that the own tile appears with its glass bar, that a
// publish resume re-sends Attach, and that Leave lands back on the live page
// with the broadcast still running.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import type { BroadcastCallbacks } from '../../transport/broadcaster';

const { created, scripts, roomSessions, FakeRoomSession } = vi.hoisted(() => {
  interface FakeSession {
    callbacks: BroadcastCallbacks;
    start(): Promise<void>;
    stop(): Promise<void>;
    setLadder(): void;
    setEncoderSettings(): void;
  }
  const created: FakeSession[] = [];
  const scripts: Array<(cbs: BroadcastCallbacks) => Promise<void>> = [];

  interface RoomCbs {
    onConnected: () => void;
    onState: (s: unknown) => void;
    onEvent: (e: unknown) => void;
    onReconnecting: (i: unknown) => void;
    onEnded: (reason: number | null) => void;
    onError: (e: unknown) => void;
  }
  class FakeRoomSession {
    opts: { target: unknown; nickname: string; clientKind: number; grant: unknown };
    cbs: RoomCbs;
    sent: unknown[] = [];
    stopped = false;
    constructor(opts: FakeRoomSession['opts'], cbs: RoomCbs) {
      this.opts = opts;
      this.cbs = cbs;
      roomSessions.push(this);
    }
    get code() {
      return null;
    }
    async start() {
      this.cbs.onConnected();
    }
    stop() {
      this.stopped = true;
    }
    attach(broadcastId: string, resumeTokenHex: string, label: string) {
      this.sent.push({ kind: 'attach', broadcastId, resumeTokenHex, label });
    }
    detach(broadcastId: string) {
      this.sent.push({ kind: 'detach', broadcastId });
    }
    setNickname() {}
    endRoom() {}
    resync() {}
  }
  const roomSessions: FakeRoomSession[] = [];
  return { created, scripts, roomSessions, FakeRoomSession };
});

vi.mock('./workerBroadcastSession', () => ({
  createBroadcastSession: async (_config: unknown, _url: string, _opts: unknown, callbacks: BroadcastCallbacks) => {
    const script = scripts.shift();
    if (!script) throw new Error('test bug: no session script queued');
    const session = {
      callbacks,
      start: () => script(callbacks),
      stop: async () => callbacks.onEnded(),
      setLadder: () => {},
      setEncoderSettings: () => {},
    };
    created.push(session);
    return session;
  },
}));
vi.mock('../../transport/room-session', async (importActual) => ({
  ...(await importActual<typeof import('../../transport/room-session')>()),
  RoomSession: FakeRoomSession,
}));

import { BroadcasterScreen } from './BroadcasterScreen';
import { acceptCurrentTerms } from '../terms/acceptance';
import { useRoomStore } from '../../state/roomStore';
import {
  ROOM_CLIENT_WEB_BROADCASTER,
  ROOM_STATE_FLAG_ATTACH_OK,
  ROOM_STATE_FLAG_CREATOR,
  ROOM_STATE_FLAG_DYNAMIC,
  type RoomState,
} from '../../transport/wire';

const TOKEN = 'c'.repeat(32);
const fakeStream = { getTracks: () => [], getVideoTracks: () => [] } as unknown as MediaStream;

function mintedState(): RoomState {
  return {
    flags: ROOM_STATE_FLAG_DYNAMIC | ROOM_STATE_FLAG_CREATOR | ROOM_STATE_FLAG_ATTACH_OK,
    caps: 0,
    seq: 1,
    yourId: 1,
    code: 'RM2CD3',
    displayName: '',
    creatorToken: new Uint8Array(16),
    key: new Uint8Array(6),
    attachments: [{ broadcastId: 'AB2CD3', label: 'my desk', live: true, viewerCount: 0 }],
    participants: [{ id: 1, kind: ROOM_CLIENT_WEB_BROADCASTER, flags: 2, nickname: 'tuhis', identity: '' }],
  };
}

async function goLive() {
  scripts.push(async (cbs) => {
    cbs.onBroadcastId?.('AB2CD3');
    cbs.onResumeToken?.(TOKEN);
    cbs.onSourceStream(fakeStream);
  });
  render(<BroadcasterScreen />);
  fireEvent.click(screen.getByRole('button', { name: /start a stream/i }));
  await waitFor(() => expect(screen.getByText('LIVE')).toBeTruthy());
}

beforeEach(() => {
  created.length = 0;
  scripts.length = 0;
  roomSessions.length = 0;
  window.__GAWK_CONFIG__ = { requirePublishSecret: false };
  localStorage.clear();
  sessionStorage.clear();
  acceptCurrentTerms();
  localStorage.setItem('gawk:nickname', 'tuhis');
  useRoomStore.getState().reset();
  Object.defineProperty(HTMLMediaElement.prototype, 'srcObject', {
    configurable: true,
    get: () => null,
    set: () => {},
  });
  HTMLMediaElement.prototype.play = () => Promise.resolve();
});

afterEach(() => {
  cleanup();
  delete window.__GAWK_CONFIG__;
  localStorage.clear();
});

describe('BroadcasterScreen Room panel (RM5)', () => {
  it('New room mints from the running broadcast and lands in the room view with the own tile', async () => {
    await goLive();
    fireEvent.click(screen.getByRole('button', { name: 'Room' }));
    const panel = screen.getByRole('dialog', { name: 'Room' });
    expect(panel).toBeTruthy();
    fireEvent.change(screen.getByRole('textbox', { name: 'Tile label' }), { target: { value: 'my desk' } });
    fireEvent.click(screen.getByRole('button', { name: 'New room' }));

    await waitFor(() => expect(roomSessions).toHaveLength(1));
    const room = roomSessions[0];
    expect(room.opts.target).toEqual({ kind: 'mint', broadcastId: 'AB2CD3', resumeTokenHex: TOKEN, label: 'my desk' });
    expect(room.opts.clientKind).toBe(ROOM_CLIENT_WEB_BROADCASTER);
    expect(room.opts.nickname).toBe('tuhis');
    expect(screen.getByText('Creating the room…')).toBeTruthy();

    act(() => room.cbs.onState(mintedState()));
    expect(screen.getByTitle('Room code').textContent).toBe('RM2CD3');
    const ownTile = screen.getByTestId('room-tile');
    expect(ownTile.getAttribute('data-own')).toBe('true');
    expect(screen.getByTestId('own-preview')).toBeTruthy();
    const bar = screen.getByTestId('own-bar');
    for (const name of ['Stop broadcast', 'Change source', 'Quality', 'Show stats', 'Detach from room']) {
      expect(screen.getByRole('button', { name })).toBeTruthy();
    }
    expect(bar.textContent).toContain('You');
    // The attach is (re)sent on join — idempotent on the relay.
    expect(room.sent).toContainEqual({ kind: 'attach', broadcastId: 'AB2CD3', resumeTokenHex: TOKEN, label: 'my desk' });

    // A publish auto-resume re-sends it.
    const sentBefore = room.sent.length;
    act(() => created[0].callbacks.onReconnecting?.({ attempt: 1, delayMs: 1, reason: 'x', closeCode: null }));
    act(() => created[0].callbacks.onResumed?.());
    expect(room.sent.length).toBe(sentBefore + 1);

    // Leave: back on the live page, broadcast untouched, room session stopped.
    fireEvent.click(screen.getByRole('button', { name: 'Leave room' }));
    expect(screen.getByText('LIVE')).toBeTruthy();
    expect(room.stopped).toBe(true);
    expect(created).toHaveLength(1);
  });

  it('Join by code dials the typed room; a typed attach secret becomes the grant', async () => {
    await goLive();
    fireEvent.click(screen.getByRole('button', { name: 'Room' }));
    fireEvent.change(screen.getByRole('textbox', { name: 'Room code' }), { target: { value: 'TuhisRoom' } });
    fireEvent.change(screen.getByLabelText('Attach secret'), { target: { value: 'hunter2' } });
    fireEvent.click(screen.getByRole('button', { name: 'Join by code' }));
    await waitFor(() => expect(roomSessions).toHaveLength(1));
    expect(roomSessions[0].opts.target).toEqual({ kind: 'join', code: 'TuhisRoom' });
    expect(roomSessions[0].opts.grant).toEqual({ kind: 'attach', secret: 'hunter2' });
  });

  it('Use a room link reads the code and its ?rt= grant', async () => {
    await goLive();
    fireEvent.click(screen.getByRole('button', { name: 'Room' }));
    fireEvent.change(screen.getByRole('textbox', { name: 'Room link' }), {
      target: { value: `https://gawk.example/#/room/AB2CD3?rt=c:${'a'.repeat(32)}` },
    });
    fireEvent.click(screen.getByRole('button', { name: 'Use a room link' }));
    await waitFor(() => expect(roomSessions).toHaveLength(1));
    expect(roomSessions[0].opts.target).toEqual({ kind: 'join', code: 'AB2CD3' });
    expect(roomSessions[0].opts.grant).toEqual({ kind: 'creator', tokenHex: 'a'.repeat(32) });
  });

  it('a room’s "start streaming here" pre-fills and opens the panel before the stream starts', () => {
    sessionStorage.setItem('gawk:room-return', 'AB2CD3');
    render(<BroadcasterScreen />);
    expect(screen.getByRole('dialog', { name: 'Room' })).toBeTruthy();
    expect((screen.getByRole('textbox', { name: 'Room code' }) as HTMLInputElement).value).toBe('AB2CD3');
    // One hop, one use.
    expect(sessionStorage.getItem('gawk:room-return')).toBeNull();
    // New room needs a live broadcast.
    expect((screen.getByRole('button', { name: 'New room' }) as HTMLButtonElement).disabled).toBe(true);
  });
});
