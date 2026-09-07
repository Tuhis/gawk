// @vitest-environment jsdom
//
// R42 RM5 (docs/44 §4.8): the broadcaster's Room panel. The publish session
// and the room control session are both faked at their transport seams; the
// test walks the real page from "Start a stream" through "New room" to the
// in-page room view, and asserts the mint carries the running broadcast's ID
// and resume token, that the own tile appears under the broadcaster's own
// topbar (direction A: no duplicated controls, "preview only" mode), that a
// publish resume re-sends Attach, and that Leave lands back on the live page
// with the broadcast still running. A room chosen BEFORE the stream is live
// (a room's "start streaming here", or join-by-code from the pre-start card)
// is pending until the broadcast can prove itself, then joins by itself —
// no panel to re-open, no code to re-type, no nickname asked twice.

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
import { BroadcastStartError } from '../../transport/broadcaster';
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
    fireEvent.change(screen.getByRole('textbox', { name: 'Your name' }), { target: { value: 'my desk' } });
    fireEvent.click(screen.getByRole('button', { name: 'New room' }));

    await waitFor(() => expect(roomSessions).toHaveLength(1));
    const room = roomSessions[0];
    expect(room.opts.target).toEqual({ kind: 'mint', broadcastId: 'AB2CD3', resumeTokenHex: TOKEN, label: 'my desk' });
    expect(room.opts.clientKind).toBe(ROOM_CLIENT_WEB_BROADCASTER);
    // The one name field names the tile AND the participant.
    expect(room.opts.nickname).toBe('my desk');
    expect(screen.getByText('Creating the room…')).toBeTruthy();

    act(() => room.cbs.onState(mintedState()));
    // Direction A (docs/44 §4.8 revision 2026-09-05): the broadcaster's own
    // topbar over the room's stage — LIVE, the broadcast code, Stop /
    // Settings / Stats where the live view has them — plus the room pill.
    // The room's own header (the "Room code" pill) is not rendered.
    expect(screen.queryByTitle('Room code')).toBeNull();
    expect(screen.getByText('LIVE')).toBeTruthy();
    expect(screen.getByTestId('room-pill').textContent).toContain('RM2CD3');
    expect(screen.getByTestId('room-pill').textContent).toContain('1 streaming');
    for (const name of ['Stop broadcast', 'Settings', 'Show stats', 'People and chat']) {
      expect(screen.getByRole('button', { name })).toBeTruthy();
    }
    const ownTile = screen.getByTestId('room-tile');
    expect(ownTile.getAttribute('data-own')).toBe('true');
    expect(screen.getByTestId('own-preview')).toBeTruthy();
    // No own-tile glass bar: nothing is duplicated. Detach lives in the panel.
    expect(screen.queryByTestId('own-bar')).toBeNull();
    expect(screen.queryByRole('button', { name: 'Detach from room' })).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: 'People and chat' }));
    expect(screen.getByRole('button', { name: 'Detach my desk' })).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: 'Hide people and chat' }));
    // The attach is (re)sent on join — idempotent on the relay.
    expect(room.sent).toContainEqual({ kind: 'attach', broadcastId: 'AB2CD3', resumeTokenHex: TOKEN, label: 'my desk' });

    // The room's layout modes are the broadcaster's too; the third reads
    // "Preview only" and shows the own screen alone, no /subscribe to anyone.
    expect(screen.queryByRole('radio', { name: /hide videos/i })).toBeNull();
    fireEvent.click(screen.getByRole('radio', { name: /preview only/i }));
    expect(screen.getByTestId('own-preview')).toBeTruthy();
    expect(screen.getByTestId('room-tile').getAttribute('data-variant')).toBe('focus');
    expect(screen.queryByText(/still in the room/i)).toBeNull();
    fireEvent.click(screen.getByRole('radio', { name: /grid/i }));

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

  it('a room’s "start streaming here" waits quietly, then joins by itself once the stream is live — no panel, no prompt', async () => {
    // What the room stashes (roomReturn.ts): the code and the nickname the
    // participant already answered there; a grant the link carried applies.
    sessionStorage.setItem('gawk:room-return', JSON.stringify({ code: 'AB2CD3', nickname: 'roomie' }));
    sessionStorage.setItem('gawk:room-grant:ab2cd3', JSON.stringify({ kind: 'creator', tokenHex: 'a'.repeat(32) }));
    localStorage.removeItem('gawk:nickname');
    scripts.push(async (cbs) => {
      cbs.onBroadcastId?.('AB2CD3');
      cbs.onResumeToken?.(TOKEN);
      cbs.onSourceStream(fakeStream);
    });
    render(<BroadcasterScreen />);
    // One hop, one use; nothing opens, the card just says what will happen.
    expect(sessionStorage.getItem('gawk:room-return')).toBeNull();
    expect(screen.queryByRole('dialog', { name: 'Room' })).toBeNull();
    expect(screen.getByTestId('pending-room').textContent).toContain('AB2CD3');
    expect(roomSessions).toHaveLength(0);

    fireEvent.click(screen.getByRole('button', { name: /start a stream/i }));
    await waitFor(() => expect(roomSessions).toHaveLength(1));
    const room = roomSessions[0];
    expect(room.opts.target).toEqual({ kind: 'join', code: 'AB2CD3' });
    expect(room.opts.grant).toEqual({ kind: 'creator', tokenHex: 'a'.repeat(32) });
    expect(room.opts.clientKind).toBe(ROOM_CLIENT_WEB_BROADCASTER);
    // The nickname rode along: no prompt, dialed with it, and the tile label
    // defaults to it.
    expect(room.opts.nickname).toBe('roomie');
    expect(screen.queryByRole('dialog', { name: 'Nickname' })).toBeNull();
    expect(screen.getByText('Joining the room…')).toBeTruthy();
    act(() => room.cbs.onState({ ...mintedState(), attachments: [{ broadcastId: 'AB2CD3', label: 'roomie', live: true, viewerCount: 0 }] }));
    expect(room.sent).toContainEqual({ kind: 'attach', broadcastId: 'AB2CD3', resumeTokenHex: TOKEN, label: 'roomie' });
  });

  it('a guest who starts streaming from a room stays a guest — no nickname prompt either', async () => {
    sessionStorage.setItem('gawk:room-return', JSON.stringify({ code: 'devroom', nickname: null }));
    localStorage.removeItem('gawk:nickname');
    scripts.push(async (cbs) => {
      cbs.onBroadcastId?.('AB2CD3');
      cbs.onResumeToken?.(TOKEN);
      cbs.onSourceStream(fakeStream);
    });
    render(<BroadcasterScreen />);
    fireEvent.click(screen.getByRole('button', { name: /start a stream/i }));
    await waitFor(() => expect(roomSessions).toHaveLength(1));
    expect(roomSessions[0].opts.target).toEqual({ kind: 'join', code: 'devroom' });
    expect(roomSessions[0].opts.nickname).toBe('');
    expect(screen.queryByRole('dialog', { name: 'Nickname' })).toBeNull();
  });

  it('Join by code before the stream starts is deferred, shown on the card, and fires when live', async () => {
    scripts.push(async (cbs) => {
      cbs.onBroadcastId?.('AB2CD3');
      cbs.onResumeToken?.(TOKEN);
      cbs.onSourceStream(fakeStream);
    });
    render(<BroadcasterScreen />);
    fireEvent.click(screen.getByRole('button', { name: 'Room' }));
    // New room needs a live broadcast; join does not.
    expect((screen.getByRole('button', { name: 'New room' }) as HTMLButtonElement).disabled).toBe(true);
    fireEvent.change(screen.getByRole('textbox', { name: 'Room code' }), { target: { value: 'TuhisRoom' } });
    fireEvent.click(screen.getByRole('button', { name: 'Join by code' }));
    // The panel closes; the page is still the pre-start card with the
    // pending chip; no room session was dialed (nothing to attach yet).
    expect(screen.queryByRole('dialog', { name: 'Room' })).toBeNull();
    expect(screen.getByRole('button', { name: /start a stream/i })).toBeTruthy();
    expect(screen.getByTestId('pending-room').textContent).toContain('TuhisRoom');
    expect(roomSessions).toHaveLength(0);

    fireEvent.click(screen.getByRole('button', { name: /start a stream/i }));
    await waitFor(() => expect(roomSessions).toHaveLength(1));
    expect(roomSessions[0].opts.target).toEqual({ kind: 'join', code: 'TuhisRoom' });
    // The remembered nickname still applies on this path.
    expect(roomSessions[0].opts.nickname).toBe('tuhis');
  });

  it('after Stop → Start, a pending room waits for the NEW resume token before joining (review, PR #302)', async () => {
    // Go live once (token delivered), then stop: the old latch must not
    // vouch for the next session.
    await goLive();
    fireEvent.click(screen.getByRole('button', { name: /stop broadcast/i }));
    await waitFor(() => expect(screen.getByRole('button', { name: /start a stream/i })).toBeTruthy());
    // A join chosen while stopped is pending.
    fireEvent.click(screen.getByRole('button', { name: 'Room' }));
    fireEvent.change(screen.getByRole('textbox', { name: 'Room code' }), { target: { value: 'TuhisRoom' } });
    fireEvent.click(screen.getByRole('button', { name: 'Join by code' }));
    expect(screen.getByTestId('pending-room').textContent).toContain('TuhisRoom');
    // Start reclaims the old ID first; the relay refuses (connect phase),
    // so the page falls back to a mint — which resets the ID and the token
    // it held. The minted session then reports its ID and goes live BEFORE
    // its resume token arrives (the two ride separate uni streams; order
    // unspecified). The pending room must wait for that token, or the
    // broadcaster dials the room as a viewer with nothing to attach.
    scripts.push(async () => {
      throw new BroadcastStartError('connect', new Error('opening handshake failed'));
    });
    let deliverToken: (() => void) | null = null;
    scripts.push(async (cbs) => {
      cbs.onBroadcastId?.('EF4GH5');
      cbs.onSourceStream(fakeStream);
      deliverToken = () => cbs.onResumeToken?.('d'.repeat(32));
    });
    fireEvent.click(screen.getByRole('button', { name: /start a stream/i }));
    await waitFor(() => expect(screen.getByText('LIVE')).toBeTruthy());
    // Nothing to attach yet: no room session at all — in particular not a
    // viewer-shaped one that the next stats tick would quietly upgrade.
    await act(async () => {
      await new Promise((r) => setTimeout(r, 50));
    });
    expect(roomSessions).toHaveLength(0);
    act(() => deliverToken?.());
    await waitFor(() => expect(roomSessions).toHaveLength(1));
    expect(roomSessions[0].opts.clientKind).toBe(ROOM_CLIENT_WEB_BROADCASTER);
    act(() => roomSessions[0].cbs.onState({ ...mintedState(), code: 'TuhisRoom', attachments: [] }));
    expect(roomSessions[0].sent).toContainEqual(expect.objectContaining({ kind: 'attach', broadcastId: 'EF4GH5', resumeTokenHex: 'd'.repeat(32) }));
  });

  it('a pending room can be dismissed before the stream starts', async () => {
    sessionStorage.setItem('gawk:room-return', JSON.stringify({ code: 'AB2CD3', nickname: 'roomie' }));
    scripts.push(async (cbs) => {
      cbs.onBroadcastId?.('AB2CD3');
      cbs.onResumeToken?.(TOKEN);
      cbs.onSourceStream(fakeStream);
    });
    render(<BroadcasterScreen />);
    fireEvent.click(screen.getByRole('button', { name: 'Don’t join the room' }));
    expect(screen.queryByTestId('pending-room')).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: /start a stream/i }));
    await waitFor(() => expect(screen.getByText('LIVE')).toBeTruthy());
    expect(roomSessions).toHaveLength(0);
  });
});
