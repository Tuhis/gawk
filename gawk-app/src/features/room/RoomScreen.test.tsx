// @vitest-environment jsdom
//
// R42 (docs/44 §4.9): the room view per mode and per relay state. The room
// control session and every tile's media session are faked at the transport
// seam (the ViewerScreen test's FakeViewerSession pattern), so each test
// drives RoomState / RoomEvent through the control session's callbacks and
// asserts the tiles, the roster and the copy that result — and, for hide
// videos, that NO /subscribe session exists at all.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';

const { roomSessions, roomState, FakeRoomSession, viewerSessions, FakeViewerSession } = vi.hoisted(() => {
  interface RoomCbs {
    onConnected: () => void;
    onState: (s: unknown) => void;
    onEvent: (e: unknown) => void;
    onReconnecting: (i: { attempt: number; delayMs: number; reason: string; closeCode: number | null }) => void;
    onEnded: (reason: number | null) => void;
    onError: (e: { kind: string; message: string }) => void;
  }
  const roomState = { failStartWith: null as null | { kind: string; message: string } };
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
      const t = this.opts.target as { kind: string; code?: string };
      return t.kind === 'join' ? t.code : null;
    }
    async start() {
      if (roomState.failStartWith) throw roomState.failStartWith;
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
    setNickname(nickname: string) {
      this.sent.push({ kind: 'nick', nickname });
    }
    endRoom() {
      this.sent.push({ kind: 'end' });
    }
    resync() {}
  }
  const roomSessions: FakeRoomSession[] = [];

  interface ViewerCbs {
    onConnected: () => void;
    onStats: (s: unknown) => void;
    onEnded: (reason: 'normal' | 'moderated') => void;
  }
  class FakeViewerSession {
    id: string;
    cbs: ViewerCbs;
    stopped = false;
    constructor(_url: string, id: string, _opts: unknown, cbs: ViewerCbs) {
      this.id = id;
      this.cbs = cbs;
      viewerSessions.push(this);
    }
    async start() {}
    async stop() {
      this.stopped = true;
      this.cbs.onEnded('normal');
    }
  }
  const viewerSessions: FakeViewerSession[] = [];
  return { roomSessions, roomState, FakeRoomSession, viewerSessions, FakeViewerSession };
});

vi.mock('../../transport/room-session', async (importActual) => ({
  ...(await importActual<typeof import('../../transport/room-session')>()),
  RoomSession: FakeRoomSession,
}));
vi.mock('../../transport/viewer-session', () => ({
  ViewerSession: FakeViewerSession,
  RECONNECT_MAX_ATTEMPTS: 10,
}));

import { RoomScreen, RoomView } from './RoomScreen';
import { useRoomStore } from '../../state/roomStore';
import {
  ROOM_CLIENT_WEB_VIEWER,
  ROOM_COMMAND_ATTACH,
  ROOM_DETACH_REASON_CREATOR,
  ROOM_END_REASON_CREATOR,
  ROOM_EVENT_ATTACHMENT_REMOVED,
  ROOM_EVENT_COMMAND_REJECTED,
  ROOM_EVENT_ROOM_ENDING,
  ROOM_REJECT_LIMIT,
  ROOM_STATE_FLAG_ATTACH_OK,
  ROOM_STATE_FLAG_CREATOR,
  ROOM_STATE_FLAG_DYNAMIC,
  type RoomState,
} from '../../transport/wire';

function state(over: Partial<RoomState> = {}): RoomState {
  return {
    flags: ROOM_STATE_FLAG_DYNAMIC | ROOM_STATE_FLAG_ATTACH_OK,
    caps: 0,
    seq: 3,
    yourId: 7,
    code: 'AB2CD3',
    displayName: '',
    creatorToken: new Uint8Array(0),
    key: new Uint8Array([1, 2, 3, 4, 5, 6]),
    attachments: [
      { broadcastId: 'AAAAAA', label: 'alpha', live: true, viewerCount: 2 },
      { broadcastId: 'BBBBBB', label: 'bravo', live: false, viewerCount: 0 },
      { broadcastId: 'CCCCCC', label: 'charlie', live: true, viewerCount: 1 },
    ],
    participants: [
      { id: 7, kind: ROOM_CLIENT_WEB_VIEWER, flags: 0, nickname: 'me', identity: '' },
      { id: 8, kind: ROOM_CLIENT_WEB_VIEWER, flags: 2, nickname: 'alpha-streamer', identity: '' },
    ],
    ...over,
  };
}

const activeViewerIds = () => viewerSessions.filter((s) => !s.stopped).map((s) => s.id).sort();

async function joinAs(nickname = 'tuhis', over: Partial<RoomState> = {}) {
  localStorage.setItem('gawk:nickname', nickname);
  render(<RoomScreen code="AB2CD3" />);
  await waitFor(() => expect(roomSessions).toHaveLength(1));
  act(() => roomSessions[0].cbs.onState(state(over)));
  return roomSessions[0];
}

beforeEach(() => {
  roomSessions.length = 0;
  viewerSessions.length = 0;
  roomState.failStartWith = null;
  localStorage.clear();
  sessionStorage.clear();
  window.location.hash = '';
  useRoomStore.getState().reset();
});
afterEach(cleanup);

describe('RoomScreen nickname prompt (D10)', () => {
  it('asks before dialing on the first join, remembers the answer, and dials with it', async () => {
    render(<RoomScreen code="AB2CD3" />);
    expect(screen.getByRole('dialog', { name: 'Nickname' })).toBeTruthy();
    expect(roomSessions).toHaveLength(0);
    fireEvent.change(screen.getByRole('textbox', { name: 'Nickname' }), { target: { value: '  tuhis ' } });
    fireEvent.click(screen.getByRole('button', { name: 'Join' }));
    await waitFor(() => expect(roomSessions).toHaveLength(1));
    expect(roomSessions[0].opts.nickname).toBe('tuhis');
    expect(roomSessions[0].opts.target).toEqual({ kind: 'join', code: 'AB2CD3' });
    expect(localStorage.getItem('gawk:nickname')).toBe('tuhis');
    expect(screen.queryByRole('dialog', { name: 'Nickname' })).toBeNull();
  });

  it('"join as a guest" dials with an empty nickname and remembers nothing', async () => {
    render(<RoomScreen code="AB2CD3" />);
    fireEvent.click(screen.getByRole('button', { name: 'Join as a guest' }));
    await waitFor(() => expect(roomSessions).toHaveLength(1));
    expect(roomSessions[0].opts.nickname).toBe('');
    expect(localStorage.getItem('gawk:nickname')).toBeNull();
  });

  it('a remembered nickname skips the prompt and a stashed grant rides the dial', async () => {
    sessionStorage.setItem('gawk:room-grant:ab2cd3', JSON.stringify({ kind: 'creator', tokenHex: 'a'.repeat(32) }));
    await joinAs();
    expect(screen.queryByRole('dialog', { name: 'Nickname' })).toBeNull();
    expect(roomSessions[0].opts.grant).toEqual({ kind: 'creator', tokenHex: 'a'.repeat(32) });
  });
});

describe('RoomScreen modes', () => {
  it('grid: one tile and one /subscribe session per attachment, numbered, away marked', async () => {
    await joinAs();
    const tiles = screen.getAllByTestId('room-tile');
    expect(tiles).toHaveLength(3);
    expect(tiles.map((t) => t.getAttribute('data-variant'))).toEqual(['grid', 'grid', 'grid']);
    expect(tiles.map((t) => t.getAttribute('data-broadcast-index'))).toEqual(['1', '2', '3']);
    await waitFor(() => expect(activeViewerIds()).toEqual(['AAAAAA', 'BBBBBB', 'CCCCCC']));
    expect(screen.getByText('· away')).toBeTruthy();
    expect(screen.getByText('3 streaming')).toBeTruthy();
    // The room key (never the code) is what tile telemetry is grouped by.
    expect(screen.getByTitle('Room code').textContent).toBe('AB2CD3');
  });

  it('a number key focuses that POV; 0 returns to grid; neither fires while typing', async () => {
    await joinAs();
    fireEvent.keyDown(window, { key: '2' });
    let tiles = screen.getAllByTestId('room-tile');
    expect(tiles.map((t) => t.getAttribute('data-variant'))).toEqual(['small', 'focus', 'small']);
    expect(localStorage.getItem('gawk:room-mode')).toBe('focus');
    // Focus keeps every session — a switch is a gain change, not a re-dial.
    expect(activeViewerIds()).toEqual(['AAAAAA', 'BBBBBB', 'CCCCCC']);

    // Clicking a small tile focuses it.
    fireEvent.click(screen.getByRole('button', { name: 'Focus charlie' }));
    tiles = screen.getAllByTestId('room-tile');
    expect(tiles.map((t) => t.getAttribute('data-variant'))).toEqual(['small', 'small', 'focus']);

    fireEvent.keyDown(window, { key: '0' });
    tiles = screen.getAllByTestId('room-tile');
    expect(tiles.map((t) => t.getAttribute('data-variant'))).toEqual(['grid', 'grid', 'grid']);

    // While a text field has focus the keys are typing, not commands.
    fireEvent.click(screen.getByRole('button', { name: 'People and chat' }));
    fireEvent.click(screen.getByRole('button', { name: 'Edit nickname' }));
    const input = screen.getByRole('textbox', { name: 'New nickname' });
    fireEvent.keyDown(input, { key: '2' });
    expect(screen.getAllByTestId('room-tile').map((t) => t.getAttribute('data-variant'))).toEqual(['grid', 'grid', 'grid']);
  });

  it('hide videos: no tiles, every /subscribe session closed, the control session kept, the card shown', async () => {
    const room = await joinAs();
    await waitFor(() => expect(activeViewerIds()).toHaveLength(3));
    fireEvent.click(screen.getByRole('radio', { name: 'Hide videos' }));
    expect(screen.queryAllByTestId('room-tile')).toHaveLength(0);
    await waitFor(() => expect(activeViewerIds()).toEqual([]));
    expect(room.stopped).toBe(false);
    expect(screen.getByText('You’re still in the room')).toBeTruthy();
    expect(screen.getByText(/Nothing is downloading/)).toBeTruthy();
    expect(localStorage.getItem('gawk:room-mode')).toBe('hidden');

    // Back to grid re-dials.
    fireEvent.click(screen.getByRole('radio', { name: 'Grid' }));
    await waitFor(() => expect(activeViewerIds()).toHaveLength(3));
  });

  it('a remembered hidden mode opens no media session at all', async () => {
    localStorage.setItem('gawk:room-mode', 'hidden');
    await joinAs();
    await new Promise((r) => setTimeout(r, 20));
    expect(viewerSessions).toHaveLength(0);
  });
});

describe('RoomScreen people-and-chat panel', () => {
  it('renders the roster and streams from the RoomState, with the share note', async () => {
    await joinAs();
    fireEvent.click(screen.getByRole('button', { name: 'People and chat' }));
    const panel = screen.getByRole('complementary', { name: 'People and chat' });
    expect(panel).toBeTruthy();
    expect(screen.getAllByTestId('stream-row')).toHaveLength(3);
    expect(screen.getAllByTestId('person-row')).toHaveLength(2);
    expect(screen.getByText('me (you)')).toBeTruthy();
    expect(screen.getByText('alpha-streamer')).toBeTruthy();
    expect(screen.getByText('streaming')).toBeTruthy();
    expect(screen.getByText(/Anyone with the room code can also see/)).toBeTruthy();
    // Chat is reserved: absent until the relay advertises the capability.
    expect(screen.queryByText('Chat')).toBeNull();
    // A dynamic room offers the code; the link is always there.
    expect(within(panel).getByRole('button', { name: /Copy room code/ })).toBeTruthy();
    expect(within(panel).getByRole('button', { name: /Copy room link/ })).toBeTruthy();
    // Not the creator: no detach, no end room.
    expect(screen.queryByRole('button', { name: /^Detach/ })).toBeNull();
    expect(screen.queryByRole('button', { name: 'End room' })).toBeNull();
  });

  it('the creator sees detach and end room, and they send the commands', async () => {
    const room = await joinAs('tuhis', { flags: ROOM_STATE_FLAG_DYNAMIC | ROOM_STATE_FLAG_CREATOR });
    fireEvent.click(screen.getByRole('button', { name: 'People and chat' }));
    fireEvent.click(screen.getByRole('button', { name: 'Detach bravo' }));
    expect(room.sent).toContainEqual({ kind: 'detach', broadcastId: 'BBBBBB' });
    fireEvent.click(screen.getByRole('button', { name: 'End room' }));
    expect(room.sent).toContainEqual({ kind: 'end' });
  });

  it('editing the nickname sends SetNickname and remembers it', async () => {
    const room = await joinAs();
    fireEvent.click(screen.getByRole('button', { name: 'People and chat' }));
    fireEvent.click(screen.getByRole('button', { name: 'Edit nickname' }));
    fireEvent.change(screen.getByLabelText('New nickname'), { target: { value: 'renamed' } });
    fireEvent.click(screen.getByRole('button', { name: 'Save' }));
    expect(room.sent).toContainEqual({ kind: 'nick', nickname: 'renamed' });
    expect(localStorage.getItem('gawk:nickname')).toBe('renamed');
  });

  it('"start streaming here" stashes the code and hops to the broadcaster', async () => {
    await joinAs('tuhis', { attachments: [] });
    expect(screen.getByText('Nobody is streaming yet')).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: 'Start streaming here' }));
    expect(sessionStorage.getItem('gawk:room-return')).toBe('AB2CD3');
    expect(window.location.hash).toBe('#/broadcast');
  });
});

describe('RoomScreen relay states', () => {
  it('room ended (4007) shows the reason card and leaves the media alone', async () => {
    const room = await joinAs();
    act(() => room.cbs.onEvent({ seq: 4, kind: ROOM_EVENT_ROOM_ENDING, reason: ROOM_END_REASON_CREATOR }));
    act(() => room.cbs.onEnded(ROOM_END_REASON_CREATOR));
    expect(screen.getByText('Room ended')).toBeTruthy();
    expect(screen.getByText('The room was ended by its creator.')).toBeTruthy();
  });

  it('reconnecting shows the pill with the relay note', async () => {
    const room = await joinAs();
    act(() => room.cbs.onReconnecting({ attempt: 1, delayMs: 100, reason: 'reset', closeCode: null }));
    expect(screen.getByText('Reconnecting to the room…')).toBeTruthy();
    act(() => room.cbs.onReconnecting({ attempt: 2, delayMs: 100, reason: 'drain', closeCode: 4002 }));
    expect(screen.getByText(/Room server is updating/)).toBeTruthy();
  });

  it('an attachment removal drops the tile and toasts', async () => {
    const room = await joinAs();
    act(() =>
      room.cbs.onEvent({
        seq: 4,
        kind: ROOM_EVENT_ATTACHMENT_REMOVED,
        attachment: { broadcastId: 'BBBBBB' },
        reason: ROOM_DETACH_REASON_CREATOR,
      }),
    );
    expect(screen.getAllByTestId('room-tile')).toHaveLength(2);
    expect(screen.getByRole('status').textContent).toContain('bravo’s stream was removed by the room’s creator');
  });

  it('a rejected command toasts once', async () => {
    const room = await joinAs();
    act(() =>
      room.cbs.onEvent({
        seq: 3,
        kind: ROOM_EVENT_COMMAND_REJECTED,
        command: ROOM_COMMAND_ATTACH,
        reason: ROOM_REJECT_LIMIT,
        message: '',
      }),
    );
    expect(screen.getByRole('status').textContent).toContain('the room is at its limit');
    expect(useRoomStore.getState().lastRejection).toBeNull();
  });

  it('a first-dial refusal reads "not found or refused"; full and wrong key have their own cards', async () => {
    roomState.failStartWith = { kind: 'refused', message: 'Room not found or refused' };
    localStorage.setItem('gawk:nickname', 'tuhis');
    render(<RoomScreen code="AB2CD3" />);
    await waitFor(() => expect(screen.getByText('Room not found or refused')).toBeTruthy());
    cleanup();

    localStorage.setItem('gawk:nickname', 'tuhis');
    roomState.failStartWith = null;
    render(<RoomScreen code="AB2CD3" />);
    await waitFor(() => expect(roomSessions).toHaveLength(2));
    act(() => roomSessions[1].cbs.onError({ kind: 'full', message: 'The room is full' }));
    expect(screen.getByText('Room full')).toBeTruthy();
    cleanup();

    render(<RoomScreen code="TuhisRoom" />);
    await waitFor(() => expect(roomSessions).toHaveLength(3));
    act(() => roomSessions[2].cbs.onError({ kind: 'forbidden', message: 'refused' }));
    expect(screen.getByText('Wrong room key')).toBeTruthy();
  });

  it('leave goes home and stops the control session', async () => {
    const room = await joinAs();
    fireEvent.click(screen.getByRole('button', { name: 'Leave room' }));
    expect(window.location.hash).toBe('#/');
    cleanup();
    expect(room.stopped).toBe(true);
  });
});

describe('RoomView with an own broadcast (RM5)', () => {
  it('attaches on join, shows the own tile with its glass bar and no self-subscribe, and detach sends the command', async () => {
    localStorage.setItem('gawk:nickname', 'tuhis');
    const onDetach = vi.fn();
    const preview = { getTracks: () => [] } as unknown as MediaStream;
    render(
      <RoomView
        target={{ kind: 'mint', broadcastId: 'AAAAAA', resumeTokenHex: 'b'.repeat(32), label: 'mine' }}
        own={{
          broadcastId: 'AAAAAA',
          resumeTokenHex: 'b'.repeat(32),
          label: 'mine',
          attachEpoch: 0,
          preview,
          controls: <button type="button">Stop</button>,
          onDetach,
        }}
        onLeave={() => {}}
      />,
    );
    await waitFor(() => expect(roomSessions).toHaveLength(1));
    const room = roomSessions[0];
    expect(room.opts.target).toMatchObject({ kind: 'mint', broadcastId: 'AAAAAA' });
    act(() => room.cbs.onState(state({ flags: ROOM_STATE_FLAG_DYNAMIC | ROOM_STATE_FLAG_CREATOR | ROOM_STATE_FLAG_ATTACH_OK })));
    expect(room.sent).toContainEqual({ kind: 'attach', broadcastId: 'AAAAAA', resumeTokenHex: 'b'.repeat(32), label: 'mine' });

    const ownTile = screen.getAllByTestId('room-tile').find((t) => t.getAttribute('data-own') === 'true');
    expect(ownTile).toBeTruthy();
    expect(screen.getByTestId('own-preview')).toBeTruthy();
    expect(screen.getByTestId('own-bar').textContent).toContain('Stop');
    // The own broadcast is painted from the local preview: only the OTHER
    // two POVs get a /subscribe session.
    await waitFor(() => expect(activeViewerIds()).toEqual(['BBBBBB', 'CCCCCC']));

    fireEvent.click(screen.getByRole('button', { name: 'Detach from room' }));
    expect(room.sent).toContainEqual({ kind: 'detach', broadcastId: 'AAAAAA' });
    expect(onDetach).toHaveBeenCalled();
  });
});
