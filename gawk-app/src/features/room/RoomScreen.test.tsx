// @vitest-environment jsdom
//
// The room view per mode and per relay state. The room
// control session and every tile's media session are faked at the transport
// seam (the ViewerScreen test's FakeViewerSession pattern), so each test
// drives RoomState / RoomEvent through the control session's callbacks and
// asserts the tiles, the roster and the copy that result — and, for hide
// videos, that NO /subscribe session exists at all.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { StrictMode } from 'react';
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
import { useTransportStore } from '../../state/transportStore';
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
    // The header carries the room's totals — watching = the participants
    // who are not streaming (one of the two is) — and no tile carries a
    // per-POV viewer count (that figure lives in the panel only).
    expect(screen.getByText('1 watching')).toBeTruthy();
    for (const t of tiles) expect(t.textContent).not.toMatch(/watching/);
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

  it('switching focus never re-dials, whatever the chosen preset', async () => {
    localStorage.setItem('gawk:room-preset', 'smoother');
    await joinAs();
    await waitFor(() => expect(activeViewerIds()).toHaveLength(3));
    const created = viewerSessions.length;
    fireEvent.keyDown(window, { key: '2' });
    fireEvent.keyDown(window, { key: '3' });
    fireEvent.keyDown(window, { key: '0' });
    await act(async () => {});
    expect(viewerSessions).toHaveLength(created);
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
  it('renders the roster and streams from the RoomState; sharing lives on the header chip, not here', async () => {
    await joinAs();
    fireEvent.click(screen.getByRole('button', { name: 'People and chat' }));
    const panel = screen.getByRole('complementary', { name: 'People and chat' });
    expect(panel).toBeTruthy();
    expect(screen.getAllByTestId('stream-row')).toHaveLength(3);
    expect(screen.getAllByTestId('person-row')).toHaveLength(2);
    expect(screen.getByText('me (you)')).toBeTruthy();
    expect(screen.getByText('alpha-streamer')).toBeTruthy();
    expect(screen.getByText('streaming')).toBeTruthy();
    // Chat is reserved: absent until the relay advertises the capability.
    expect(screen.queryByText('Chat')).toBeNull();
    // No copy buttons in the panel: the header's code
    // chip copies the link, the More menu keeps both copies.
    expect(within(panel).queryByRole('button', { name: /Copy room/ })).toBeNull();
    // Not the creator: no detach, no end room.
    expect(screen.queryByRole('button', { name: /^Detach/ })).toBeNull();
    expect(screen.queryByRole('button', { name: /End room/ })).toBeNull();
  });

  it('the header code chip is the copy: click it and the room link is copied, the note riding the toast', async () => {
    const writeText = vi.fn(() => Promise.resolve());
    Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText } });
    await joinAs();
    // One copy control in the header, and it is the chip showing the code
    // (the separate copy-link icon beside the panel toggle is gone).
    const chip = screen.getByRole('button', { name: 'Copy room link' });
    expect(chip.textContent).toContain('AB2CD3');
    expect(chip.getAttribute('title')).toBe('Room code');
    fireEvent.click(chip);
    await waitFor(() => expect(writeText).toHaveBeenCalledTimes(1));
    expect(writeText.mock.calls[0]).toEqual([expect.stringMatching(/#\/room\/AB2CD3$/)]);
    // The code-visibility note shows at the moment of sharing.
    await waitFor(() => expect(screen.getByText(/Room link copied/)).toBeTruthy());
    expect(screen.getByText(/can also see the codes of the streams/)).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Copied' })).toBeTruthy();
  });

  it('a static room’s chip shows its slug and copies its link too', async () => {
    const writeText = vi.fn(() => Promise.resolve());
    Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText } });
    render(<RoomScreen code="devroom" />);
    fireEvent.click(screen.getByRole('button', { name: 'Join as a guest' }));
    await waitFor(() => expect(roomSessions).toHaveLength(1));
    act(() => roomSessions[0].cbs.onState(state({ flags: ROOM_STATE_FLAG_ATTACH_OK, code: 'devroom' })));
    const chip = screen.getByRole('button', { name: 'Copy room link' });
    expect(chip.textContent).toContain('devroom');
    fireEvent.click(chip);
    await waitFor(() => expect(writeText).toHaveBeenCalledWith(expect.stringMatching(/#\/room\/devroom$/)));
  });

  it('the creator sees detach and end room; ending asks first, and only the confirm sends it', async () => {
    const room = await joinAs('tuhis', { flags: ROOM_STATE_FLAG_DYNAMIC | ROOM_STATE_FLAG_CREATOR });
    fireEvent.click(screen.getByRole('button', { name: 'People and chat' }));
    fireEvent.click(screen.getByRole('button', { name: 'Detach bravo' }));
    expect(room.sent).toContainEqual({ kind: 'detach', broadcastId: 'BBBBBB' });
    fireEvent.click(screen.getByRole('button', { name: 'End room…' }));
    expect(room.sent).not.toContainEqual({ kind: 'end' });
    expect(screen.getByText('End the room for everyone?')).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(screen.queryByText('End the room for everyone?')).toBeNull();
    expect(room.sent).not.toContainEqual({ kind: 'end' });
    fireEvent.click(screen.getByRole('button', { name: 'End room…' }));
    fireEvent.click(screen.getByRole('button', { name: 'End room' }));
    expect(room.sent).toContainEqual({ kind: 'end' });
  });

  it('a creator watching from a tab (a native "Open room view") is not offered "start streaming here"', async () => {
    // The creator flag only arrives with the creator token, and a tab that
    // holds it without a broadcast of its own is the native app's room view:
    // that person already streams from the app. Neither the empty-room card
    // nor the panel offers a second, browser stream.
    await joinAs('tuhis', { flags: ROOM_STATE_FLAG_DYNAMIC | ROOM_STATE_FLAG_CREATOR, attachments: [] });
    expect(screen.getByText('Nobody is streaming yet')).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: 'People and chat' }));
    expect(screen.queryByRole('button', { name: 'Start streaming here' })).toBeNull();
    // End room stays: that is what the creator token is for.
    expect(screen.getByRole('button', { name: 'End room…' })).toBeTruthy();
  });

  it('the creator sees a Creator chip whose help says what the role can and cannot do', async () => {
    await joinAs('tuhis', { flags: ROOM_STATE_FLAG_DYNAMIC | ROOM_STATE_FLAG_CREATOR });
    const chip = screen.getByRole('button', { name: 'Creator' });
    expect(chip.getAttribute('aria-expanded')).toBe('false');
    fireEvent.click(chip);
    expect(chip.getAttribute('aria-expanded')).toBe('true');
    const help = screen.getByRole('dialog', { name: 'Your role in this room' });
    expect(help.textContent).toContain('You created this room');
    expect(help.textContent).toContain('Remove any stream');
    expect(help.textContent).toContain('End the room for everyone');
    // Honest about the limit: streams, not people.
    expect(help.textContent).toContain('can’t remove people');
    // Escape and a second click both close it.
    fireEvent.keyDown(document, { key: 'Escape' });
    expect(screen.queryByRole('dialog', { name: 'Your role in this room' })).toBeNull();
    fireEvent.click(chip);
    fireEvent.click(chip);
    expect(screen.queryByRole('dialog', { name: 'Your role in this room' })).toBeNull();
  });

  it('a participant without the creator token sees no Creator chip', async () => {
    await joinAs();
    expect(screen.queryByRole('button', { name: 'Creator' })).toBeNull();
  });

  it('End room from the More menu opens the panel on the same confirm instead of ending at once', async () => {
    const room = await joinAs('tuhis', { flags: ROOM_STATE_FLAG_DYNAMIC | ROOM_STATE_FLAG_CREATOR });
    fireEvent.click(screen.getByRole('button', { name: 'More options' }));
    fireEvent.click(screen.getByRole('menuitem', { name: 'End room…' }));
    expect(room.sent).not.toContainEqual({ kind: 'end' });
    expect(screen.getByRole('complementary', { name: 'People and chat' })).toBeTruthy();
    expect(screen.getByText('End the room for everyone?')).toBeTruthy();
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

  it('"start streaming here" stashes the code AND the nickname, then hops to the broadcaster', async () => {
    await joinAs('tuhis', { attachments: [] });
    expect(screen.getByText('Nobody is streaming yet')).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: 'Start streaming here' }));
    expect(JSON.parse(sessionStorage.getItem('gawk:room-return') ?? 'null')).toEqual({ code: 'AB2CD3', nickname: 'tuhis' });
    expect(window.location.hash).toBe('#/broadcast');
  });

  it('"start streaming here" keeps a room link\'s relay across the hop', async () => {
    useTransportStore.getState().setSessionOverride('https://relay.example:4433');
    try {
      await joinAs('tuhis', { attachments: [] });
      fireEvent.click(screen.getByRole('button', { name: 'Start streaming here' }));
      expect(window.location.hash).toBe(`#/broadcast?relay=${encodeURIComponent('https://relay.example:4433')}`);
    } finally {
      useTransportStore.getState().setSessionOverride(null);
    }
  });

  it('a guest’s "start streaming here" hands over a null nickname, so the broadcaster asks nothing', async () => {
    render(<RoomScreen code="AB2CD3" />);
    fireEvent.click(screen.getByRole('button', { name: 'Join as a guest' }));
    await waitFor(() => expect(roomSessions).toHaveLength(1));
    act(() => roomSessions[0].cbs.onState(state({ attachments: [] })));
    fireEvent.click(screen.getByRole('button', { name: 'Start streaming here' }));
    expect(JSON.parse(sessionStorage.getItem('gawk:room-return') ?? 'null')).toEqual({ code: 'AB2CD3', nickname: null });
  });

  it('a guest who later picks a nickname hands it over on "start streaming here"', async () => {
    render(<RoomScreen code="AB2CD3" />);
    fireEvent.click(screen.getByRole('button', { name: 'Join as a guest' }));
    await waitFor(() => expect(roomSessions).toHaveLength(1));
    act(() => roomSessions[0].cbs.onState(state({ attachments: [] })));
    fireEvent.click(screen.getByRole('button', { name: 'People and chat' }));
    fireEvent.click(screen.getByRole('button', { name: 'Edit nickname' }));
    fireEvent.change(screen.getByLabelText('New nickname'), { target: { value: 'named' } });
    fireEvent.click(screen.getByRole('button', { name: 'Save' }));
    fireEvent.click(screen.getAllByRole('button', { name: 'Start streaming here' })[0]);
    expect(JSON.parse(sessionStorage.getItem('gawk:room-return') ?? 'null')).toEqual({ code: 'AB2CD3', nickname: 'named' });
  });

  it('a nickname changed while the first join is connecting reaches the relay', async () => {
    localStorage.setItem('gawk:nickname', 'old');
    render(<RoomScreen code="AB2CD3" />);
    await waitFor(() => expect(roomSessions).toHaveLength(1));
    fireEvent.contextMenu(document.querySelector('[data-status]')!);
    fireEvent.click(screen.getByRole('menuitem', { name: 'Change nickname…' }));
    fireEvent.change(screen.getByRole('textbox', { name: 'Nickname' }), { target: { value: 'new' } });
    fireEvent.click(screen.getByRole('button', { name: 'Save' }));
    expect(roomSessions[0].sent).toContainEqual({ kind: 'nick', nickname: 'new' });
  });

  it('presetNickname skips the prompt: a string dials with it, null joins as a guest', async () => {
    const target = { kind: 'join', code: 'AB2CD3' } as const;
    const { unmount } = render(<RoomView target={target} presetNickname="handed" />);
    expect(screen.queryByRole('dialog', { name: 'Nickname' })).toBeNull();
    await waitFor(() => expect(roomSessions).toHaveLength(1));
    expect(roomSessions[0].opts.nickname).toBe('handed');
    unmount();
    useRoomStore.getState().reset();
    render(<RoomView target={target} presetNickname={null} />);
    expect(screen.queryByRole('dialog', { name: 'Nickname' })).toBeNull();
    await waitFor(() => expect(roomSessions).toHaveLength(2));
    expect(roomSessions[1].opts.nickname).toBe('');
  });

  // A name typed on the broadcaster page is bounded in characters there, but
  // the wire limits are bytes: 20 × 'ä' is 40.
  it('bounds a handed-in name to the wire limits before it reaches the relay', async () => {
    const name = 'ä'.repeat(20);
    const own = {
      broadcastId: 'AAAAAA',
      resumeTokenHex: 'b'.repeat(32),
      label: name,
      attachEpoch: 0,
      preview: null,
      controls: null,
      onDetach: () => {},
    };
    render(<RoomView target={{ kind: 'join', code: 'AB2CD3' }} own={own} presetNickname={name} onLeave={() => {}} />);
    await waitFor(() => expect(roomSessions).toHaveLength(1));
    const room = roomSessions[0];
    act(() => room.cbs.onState(state({ attachments: [] })));
    const bytes = (s: string) => new TextEncoder().encode(s).length;
    expect(bytes(room.opts.nickname)).toBeLessThanOrEqual(32);
    const attach = room.sent.find((c) => (c as { kind: string }).kind === 'attach') as { label: string };
    expect(bytes(attach.label)).toBeLessThanOrEqual(32);
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

  // The control session and the media sessions are independent: a room
  // reconnect (a relay rollout drains it) must not cut anyone's video.
  it('a control-session reconnect keeps every tile and its media session', async () => {
    const room = await joinAs();
    await waitFor(() => expect(activeViewerIds()).toHaveLength(3));
    const created = viewerSessions.length;
    act(() => room.cbs.onReconnecting({ attempt: 1, delayMs: 0, reason: 'drain', closeCode: 4002 }));
    expect(screen.getAllByTestId('room-tile')).toHaveLength(3);
    act(() => room.cbs.onState(state()));
    expect(viewerSessions).toHaveLength(created);
    expect(activeViewerIds()).toHaveLength(3);
  });

  it('entering another room shows nothing of the previous one', async () => {
    const room = await joinAs();
    act(() =>
      room.cbs.onEvent({
        seq: 4,
        kind: ROOM_EVENT_ATTACHMENT_REMOVED,
        attachment: { broadcastId: 'BBBBBB' },
        reason: ROOM_DETACH_REASON_CREATOR,
      }),
    );
    cleanup();
    viewerSessions.length = 0;

    render(<RoomScreen code="XY2ZW3" />);
    expect(viewerSessions).toHaveLength(0);
    expect(screen.queryByTestId('room-tile')).toBeNull();
    expect(screen.queryByRole('status')).toBeNull();
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

// main.tsx renders <StrictMode>, which mounts, cleans up and remounts every
// effect in development. A mint that reached the relay twice was refused the
// second time ("broadcast is in another room").
describe('RoomView under StrictMode', () => {
  it('dials the room once', async () => {
    render(
      <StrictMode>
        <RoomView
          target={{ kind: 'mint', broadcastId: 'AAAAAA', resumeTokenHex: 'b'.repeat(32), label: 'mine' }}
          presetNickname="tuhis"
          onLeave={() => {}}
        />
      </StrictMode>,
    );
    await waitFor(() => expect(roomSessions.filter((s) => !s.stopped)).toHaveLength(1));
    await act(async () => {
      await new Promise((r) => setTimeout(r, 20));
    });
    expect(roomSessions).toHaveLength(1);
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

  // A broadcaster in a room, joined to someone else's (or its own) room.
  function renderOwn(onLeave: () => void) {
    localStorage.setItem('gawk:nickname', 'tuhis');
    const preview = { getTracks: () => [] } as unknown as MediaStream;
    render(
      <RoomView
        target={{ kind: 'join', code: 'AB2CD3' }}
        own={{
          broadcastId: 'AAAAAA',
          resumeTokenHex: 'b'.repeat(32),
          label: 'alpha',
          attachEpoch: 0,
          preview,
          controls: null,
          onDetach: () => {},
        }}
        onLeave={onLeave}
      />,
    );
  }

  it('ending the room yourself goes straight back to the broadcast — no card to dismiss', async () => {
    const onLeave = vi.fn();
    renderOwn(onLeave);
    await waitFor(() => expect(roomSessions).toHaveLength(1));
    const room = roomSessions[0];
    act(() => room.cbs.onState(state({ flags: ROOM_STATE_FLAG_DYNAMIC | ROOM_STATE_FLAG_CREATOR | ROOM_STATE_FLAG_ATTACH_OK })));
    fireEvent.click(screen.getByRole('button', { name: 'People and chat' }));
    fireEvent.click(screen.getByRole('button', { name: 'End room…' }));
    fireEvent.click(screen.getByRole('button', { name: 'End room' }));
    expect(onLeave).not.toHaveBeenCalled();
    act(() => room.cbs.onEvent({ seq: 4, kind: ROOM_EVENT_ROOM_ENDING, reason: ROOM_END_REASON_CREATOR }));
    act(() => room.cbs.onEnded(ROOM_END_REASON_CREATOR));
    expect(onLeave).toHaveBeenCalledTimes(1);
  });

  it('a room someone else ended says so, says the stream is still live, and returns on acknowledge', async () => {
    const onLeave = vi.fn();
    renderOwn(onLeave);
    await waitFor(() => expect(roomSessions).toHaveLength(1));
    const room = roomSessions[0];
    act(() => room.cbs.onState(state()));
    act(() => room.cbs.onEvent({ seq: 4, kind: ROOM_EVENT_ROOM_ENDING, reason: ROOM_END_REASON_CREATOR }));
    act(() => room.cbs.onEnded(ROOM_END_REASON_CREATOR));
    expect(screen.getByText('Room ended')).toBeTruthy();
    expect(screen.getByText(/ended by its creator/)).toBeTruthy();
    expect(screen.getByText(/Your stream is still live/)).toBeTruthy();
    expect(onLeave).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole('button', { name: 'Back to my stream' }));
    expect(onLeave).toHaveBeenCalledTimes(1);
  });

  it('the creator removing YOUR stream is a card, not a toast, and acknowledging it leaves the room', async () => {
    const onLeave = vi.fn();
    renderOwn(onLeave);
    await waitFor(() => expect(roomSessions).toHaveLength(1));
    const room = roomSessions[0];
    act(() => room.cbs.onState(state()));
    act(() =>
      room.cbs.onEvent({
        seq: 4,
        kind: ROOM_EVENT_ATTACHMENT_REMOVED,
        attachment: { broadcastId: 'AAAAAA' },
        reason: ROOM_DETACH_REASON_CREATOR,
      }),
    );
    expect(screen.getByText('Your stream was removed from the room')).toBeTruthy();
    expect(screen.getByText(/Your stream is still live/)).toBeTruthy();
    expect(screen.queryByRole('status')).toBeNull();
    expect(onLeave).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole('button', { name: 'Back to my stream' }));
    expect(onLeave).toHaveBeenCalledTimes(1);
  });

  it('someone else’s stream being removed is still just a toast', async () => {
    const onLeave = vi.fn();
    renderOwn(onLeave);
    await waitFor(() => expect(roomSessions).toHaveLength(1));
    const room = roomSessions[0];
    act(() => room.cbs.onState(state()));
    act(() =>
      room.cbs.onEvent({
        seq: 4,
        kind: ROOM_EVENT_ATTACHMENT_REMOVED,
        attachment: { broadcastId: 'BBBBBB' },
        reason: ROOM_DETACH_REASON_CREATOR,
      }),
    );
    expect(screen.getByRole('status').textContent).toContain('bravo’s stream was removed by the room’s creator');
    expect(screen.queryByText('Your stream was removed from the room')).toBeNull();
  });
});

describe('a gated static room that refused the attach grant (D8)', () => {
  // The relay clears ATTACH_OK for a participant who brought no
  // attach secret, the attach effect is guarded on that flag, so no Attach
  // command is sent and no CommandRejected ever comes back. The state has to
  // speak for itself.
  const ownBroadcast = {
    broadcastId: 'AAAAAA',
    resumeTokenHex: 'b'.repeat(32),
    label: 'mine',
    attachEpoch: 0,
    preview: null,
    controls: null,
    onDetach: () => {},
  };
  const renderOwn = () =>
    render(
      <RoomView target={{ kind: 'join', code: 'AB2CD3' }} own={ownBroadcast} presetNickname="tuhis" onLeave={() => {}} />,
    );

  it('says so instead of failing silently, and the typed secret re-dials with an attach grant', async () => {
    renderOwn();
    await waitFor(() => expect(roomSessions).toHaveLength(1));
    const room = roomSessions[0];
    act(() => room.cbs.onState(state({ flags: 0, attachments: [] })));

    // The guard stands: sending a command the relay is bound to refuse is
    // pointless — the copy is what was missing.
    expect(room.sent).toEqual([]);
    expect(screen.getByText('Your stream isn’t in this room')).toBeTruthy();
    expect(screen.getByText(/needs an attach secret/)).toBeTruthy();
    // And not the "nobody is streaming" card, which would be the wrong story.
    expect(screen.queryByText('Nobody is streaming yet')).toBeNull();

    fireEvent.click(screen.getByRole('button', { name: 'Enter the secret' }));
    fireEvent.change(screen.getByLabelText('Attach secret'), { target: { value: ' hunter2 ' } });
    fireEvent.click(screen.getByRole('button', { name: 'Attach' }));

    // A fresh dial: the grant rides RoomHello, so it cannot be a command.
    await waitFor(() => expect(roomSessions).toHaveLength(2));
    expect(roomSessions[1].opts.grant).toEqual({ kind: 'attach', secret: 'hunter2' });
    expect(room.stopped).toBe(true);
    // Nothing is kept until the relay has granted on it: a stashed typo would
    // fail every reload with no field in sight to correct it.
    expect(sessionStorage.getItem('gawk:room-grant:ab2cd3')).toBeNull();

    act(() => roomSessions[1].cbs.onState(state({ flags: ROOM_STATE_FLAG_ATTACH_OK, attachments: [] })));
    // Accepted, so now it rides a reload of this tab (grantHandoff.ts).
    expect(JSON.parse(sessionStorage.getItem('gawk:room-grant:ab2cd3') ?? 'null')).toEqual({
      kind: 'attach',
      secret: 'hunter2',
    });
    expect(roomSessions[1].sent).toContainEqual({
      kind: 'attach',
      broadcastId: 'AAAAAA',
      resumeTokenHex: 'b'.repeat(32),
      label: 'mine',
    });
    expect(screen.queryByText('Your stream isn’t in this room')).toBeNull();
  });

  it('a later lost session after an accepted secret does not blame the secret', async () => {
    renderOwn();
    await waitFor(() => expect(roomSessions).toHaveLength(1));
    act(() => roomSessions[0].cbs.onState(state({ flags: 0, attachments: [] })));
    fireEvent.click(screen.getByRole('button', { name: 'Enter the secret' }));
    fireEvent.change(screen.getByLabelText('Attach secret'), { target: { value: 'hunter2' } });
    fireEvent.click(screen.getByRole('button', { name: 'Attach' }));
    await waitFor(() => expect(roomSessions).toHaveLength(2));
    act(() => roomSessions[1].cbs.onState(state({ flags: ROOM_STATE_FLAG_ATTACH_OK, attachments: [] })));
    act(() => roomSessions[1].cbs.onError({ kind: 'lost', message: 'lost' }));
    expect(screen.getByText('Lost the room')).toBeTruthy();
    expect(screen.queryByText('That secret didn’t work')).toBeNull();
  });

  it('with other POVs on the stage it is a pill, not a card over the video', async () => {
    renderOwn();
    await waitFor(() => expect(roomSessions).toHaveLength(1));
    act(() => roomSessions[0].cbs.onState(state({ flags: 0 })));
    expect(screen.getAllByTestId('room-tile')).toHaveLength(3);
    const pill = screen.getByTestId('attach-gated-pill');
    expect(pill.textContent).toMatch(/needs an attach secret/);
    fireEvent.click(pill);
    expect(screen.getByLabelText('Attach secret')).toBeTruthy();
  });

  it('a refused secret says so, offers another instead of a reload, and re-dials even for the same one', async () => {
    renderOwn();
    await waitFor(() => expect(roomSessions).toHaveLength(1));
    act(() => roomSessions[0].cbs.onState(state({ flags: 0, attachments: [] })));
    fireEvent.click(screen.getByRole('button', { name: 'Enter the secret' }));
    fireEvent.change(screen.getByLabelText('Attach secret'), { target: { value: 'wrong' } });
    fireEvent.click(screen.getByRole('button', { name: 'Attach' }));
    await waitFor(() => expect(roomSessions).toHaveLength(2));

    // The relay refuses a wrong secret at join, and the browser cannot see
    // WHICH status it was — 403 and 404 reach JS as one opaque failure, so
    // the honest generic card is "not found or refused". Here we know a
    // secret was just supplied, so the card names it.
    act(() => roomSessions[1].cbs.onError({ kind: 'refused', message: 'Room not found or refused' }));
    expect(screen.getByText('That secret didn’t work')).toBeTruthy();
    expect(screen.queryByText('Room not found or refused')).toBeNull();
    // Reload would kill the live broadcast this page is running.
    expect(screen.queryByRole('button', { name: 'Retry' })).toBeNull();
    expect(screen.getByRole('button', { name: 'Back to my stream' })).toBeTruthy();

    // Same secret again still re-dials (a typo may have been "fixed" back to
    // it, or the room's secret rotated) — the nonce, not the grant, moves.
    fireEvent.click(screen.getByRole('button', { name: 'Try another secret' }));
    fireEvent.change(screen.getByLabelText('Attach secret'), { target: { value: 'wrong' } });
    fireEvent.click(screen.getByRole('button', { name: 'Attach' }));
    await waitFor(() => expect(roomSessions).toHaveLength(3));
    expect(roomSessions[2].opts.grant).toEqual({ kind: 'attach', secret: 'wrong' });
  });

  it('a viewer with nothing to attach stays silent', async () => {
    await joinAs('tuhis', { flags: 0, attachments: [] });
    expect(screen.getByText('Nobody is streaming yet')).toBeTruthy();
    expect(screen.queryByText('Your stream isn’t in this room')).toBeNull();
    expect(screen.queryByTestId('attach-gated-pill')).toBeNull();
  });
});
