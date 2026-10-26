// @vitest-environment jsdom
//
// R79 room voice in the room view (docs/80 MV6): no voice UI at all without
// the voice capability; with it, the Voice section (the §6 notice, the merged
// roster, join / listen-only), the VoiceJoin → ticket → voice session flow
// through the real controller, the in-call dock, push-to-talk on the
// keyboard and the hold button, and the close codes' visible forms. The room
// session, the voice session and the capture are faked at their seams.

import { VoiceController } from './voice/voiceController';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';

const { roomSessions, FakeRoomSession, voiceSessions, FakeVoiceSession, FakeCapture, captures } = vi.hoisted(() => {
  interface RoomCbs {
    onConnected: () => void;
    onState: (s: unknown) => void;
    onEvent: (e: unknown) => void;
    onReconnecting: (i: unknown) => void;
    onEnded: (r: number | null) => void;
    onError: (e: unknown) => void;
    onVoiceTicket?: (url: string, ticket: Uint8Array) => void;
  }
  class FakeRoomSession {
    opts: { wantVoice?: boolean };
    cbs: RoomCbs;
    voiceJoins = 0;
    constructor(opts: FakeRoomSession['opts'], cbs: RoomCbs) {
      this.opts = opts;
      this.cbs = cbs;
      roomSessions.push(this);
    }
    get code() {
      return 'AB2CD3';
    }
    async start() {
      this.cbs.onConnected();
    }
    stop() {}
    attach() {}
    detach() {}
    setNickname() {}
    endRoom() {}
    resync() {}
    sendVoiceJoin() {
      this.voiceJoins++;
      return true;
    }
  }
  const roomSessions: FakeRoomSession[] = [];

  interface VoiceCbs {
    onState: (s: { flags: number; session: number; name: string }) => void;
    onFrame: (h: unknown, p: Uint8Array) => void;
    onClosed: (i: { code: number | null; reason: string }) => void;
  }
  class FakeVoiceSession {
    opts: { url: string; helloFlags: number };
    cb: VoiceCbs;
    controls: number[] = [];
    frames: unknown[] = [];
    closed = 0;
    constructor(opts: FakeVoiceSession['opts'], cb: VoiceCbs) {
      this.opts = opts;
      this.cb = cb;
      voiceSessions.push(this);
    }
    async start() {}
    sendFrame(h: unknown) {
      this.frames.push(h);
    }
    sendControl(f: number) {
      this.controls.push(f);
    }
    close() {
      this.closed++;
    }
  }
  const voiceSessions: FakeVoiceSession[] = [];

  class FakeCapture {
    cb: { shouldSend: (rms: number, now: number) => boolean };
    stopped = 0;
    constructor(cb: FakeCapture['cb']) {
      this.cb = cb;
      captures.push(this);
    }
    async start() {}
    stop() {
      this.stopped++;
    }
  }
  const captures: FakeCapture[] = [];
  return { roomSessions, FakeRoomSession, voiceSessions, FakeVoiceSession, FakeCapture, captures };
});

vi.mock('../../transport/room-session', async (importActual) => ({
  ...(await importActual<typeof import('../../transport/room-session')>()),
  RoomSession: FakeRoomSession,
}));
vi.mock('../../transport/viewer-session', () => ({
  ViewerSession: class {
    async start() {}
    async stop() {}
  },
  RECONNECT_MAX_ATTEMPTS: 10,
}));
vi.mock('./voice/voiceSession', async (importActual) => ({
  ...(await importActual<typeof import('./voice/voiceSession')>()),
  VoiceSession: FakeVoiceSession,
}));
// Every VoicePlayback the room builds, with the volumeFor it was handed: what
// a speaker's sink is set to when the speaker is (re)created.
const playbackVolumeFors = vi.hoisted(() => [] as ((session: number) => number)[]);
vi.mock('./voice/voicePlayback', async (importActual) => {
  const actual = await importActual<typeof import('./voice/voicePlayback')>();
  class RecordingPlayback extends actual.VoicePlayback {
    constructor(...args: ConstructorParameters<typeof actual.VoicePlayback>) {
      super(...args);
      playbackVolumeFors.push(args[2]);
    }
  }
  return { ...actual, VoicePlayback: RecordingPlayback };
});
vi.mock('./voice/voiceCapture', async (importActual) => ({
  ...(await importActual<typeof import('./voice/voiceCapture')>()),
  VoiceCapture: FakeCapture,
  voiceCaptureSupported: () => true,
}));

import { RoomScreen } from './RoomScreen';
import { useRoomStore } from '../../state/roomStore';
import {
  CLOSE_CODE_SERVER_DRAINING,
  CLOSE_CODE_VOICE_REMOVED,
  CLOSE_CODE_VOICE_UNAVAILABLE,
  ROOM_CAP_VOICE,
  ROOM_CLIENT_WEB_VIEWER,
  ROOM_COMMAND_VOICE_JOIN,
  ROOM_EVENT_COMMAND_REJECTED,
  ROOM_EVENT_VOICE_ROSTER_UPDATED,
  ROOM_REJECT_UNAVAILABLE,
  ROOM_STATE_FLAG_DYNAMIC,
  ROOM_VOICE_KIND_GAWK,
  ROOM_VOICE_KIND_MUMBLE,
  ROOM_VOICE_ROSTER_FLAG_AVAILABLE,
  ROOM_VOICE_USER_SELF_MUTE,
  VOICE_CONTROL_FLAG_SELF_DEAF,
  VOICE_CONTROL_FLAG_SELF_MUTE,
  VOICE_HELLO_FLAG_LISTEN_ONLY,
  VOICE_STATE_FLAG_CONNECTED,
  VOICE_STATE_FLAG_SERVER_MUTE,
  type RoomState,
} from '../../transport/wire';
import { VOICE_NOTICE } from './voice/voiceCopy';
import { useVoiceStore } from './voice/voiceStore';

function state(caps: number): RoomState {
  return {
    flags: ROOM_STATE_FLAG_DYNAMIC,
    caps,
    seq: 3,
    yourId: 7,
    code: 'AB2CD3',
    displayName: '',
    creatorToken: new Uint8Array(0),
    key: new Uint8Array(0),
    attachments: [],
    participants: [
      { id: 7, kind: ROOM_CLIENT_WEB_VIEWER, flags: 0, nickname: 'me', identity: '' },
      { id: 8, kind: ROOM_CLIENT_WEB_VIEWER, flags: 0, nickname: 'alice', identity: '' },
    ],
  };
}

const ROSTER = {
  seq: 3,
  kind: ROOM_EVENT_VOICE_ROSTER_UPDATED,
  voiceFlags: ROOM_VOICE_ROSTER_FLAG_AVAILABLE,
  voiceRoster: [
    { session: 3, kind: ROOM_VOICE_KIND_MUMBLE, participantId: 0, flags: ROOM_VOICE_USER_SELF_MUTE, name: 'bob' },
    { session: 9, kind: ROOM_VOICE_KIND_GAWK, participantId: 8, flags: 0, name: 'alice [web]' },
  ],
};

async function join(caps: number, roster: unknown = ROSTER) {
  localStorage.setItem('gawk:nickname', 'me');
  render(<RoomScreen code="AB2CD3" />);
  await waitFor(() => expect(roomSessions).toHaveLength(1));
  const room = roomSessions[0];
  act(() => room.cbs.onState(state(caps)));
  if (roster) act(() => room.cbs.onEvent(roster));
  return room;
}

function openPanel() {
  fireEvent.click(screen.getByRole('button', { name: 'People and chat' }));
  return screen.getByRole('complementary', { name: 'People and chat' });
}

async function inVoice(listenOnly: boolean) {
  const room = await join(ROOM_CAP_VOICE);
  const panel = openPanel();
  fireEvent.click(within(panel).getByRole('button', { name: listenOnly ? 'Listen only' : 'Join voice' }));
  await waitFor(() => expect(room.voiceJoins).toBe(1));
  await act(async () => room.cbs.onVoiceTicket!('https://bridge.test/voice', new Uint8Array([1])));
  await waitFor(() => expect(voiceSessions).toHaveLength(1));
  const vs = voiceSessions[0];
  act(() => vs.cb.onState({ flags: VOICE_STATE_FLAG_CONNECTED, session: 42, name: 'me [web]' }));
  return { room, panel, vs };
}

beforeEach(() => {
  roomSessions.length = 0;
  voiceSessions.length = 0;
  captures.length = 0;
  localStorage.clear();
  sessionStorage.clear();
  useRoomStore.getState().reset();
  useVoiceStore.getState().reset();
});
afterEach(cleanup);

describe('room voice UI', () => {
  it('shows no voice UI at all without the voice capability', async () => {
    await join(0, null);
    const panel = openPanel();
    expect(within(panel).queryByTestId('voice-section')).toBeNull();
    expect(screen.queryByText(VOICE_NOTICE)).toBeNull();
    expect(screen.queryByRole('button', { name: /Join voice|Listen only/ })).toBeNull();
    expect(screen.queryByTestId('voice-dock')).toBeNull();
  });

  it('with the capability: the §6 notice and the merged roster, gawk nickname and Mumble marker', async () => {
    await join(ROOM_CAP_VOICE);
    const panel = openPanel();
    const section = within(panel).getByTestId('voice-section');
    expect(within(section).getByText(VOICE_NOTICE)).toBeTruthy();
    const rows = within(section).getAllByTestId('voice-row');
    expect(rows).toHaveLength(2);
    expect(rows[0].textContent).toContain('bob');
    expect(rows[0].textContent).toContain('Mumble');
    expect(within(rows[0]).getByLabelText('Muted')).toBeTruthy();
    expect(rows[1].textContent).toContain('alice');
    expect(rows[1].textContent).not.toContain('Mumble');
    // OD14: no speaking indicators and no per-person volume until in voice.
    expect(rows.every((r) => r.querySelector('[data-on="true"]') === null)).toBe(true);
    expect(within(section).queryByLabelText(/Volume of/)).toBeNull();
  });

  it('a roster without Available says voice is unavailable and offers no join', async () => {
    await join(ROOM_CAP_VOICE, { ...ROSTER, voiceFlags: 0, voiceRoster: [] });
    const section = within(openPanel()).getByTestId('voice-section');
    expect(within(section).getByText('Voice is unavailable right now.')).toBeTruthy();
    expect((within(section).getByRole('button', { name: 'Join voice' }) as HTMLButtonElement).disabled).toBe(true);
  });

  it('listen-only: VoiceJoin, ticket, a listen-only hello, no microphone; the dock appears', async () => {
    const { panel, vs } = await inVoice(true);
    expect(vs.opts.url).toBe('https://bridge.test/voice');
    expect(vs.opts.helloFlags).toBe(VOICE_HELLO_FLAG_LISTEN_ONLY);
    expect(captures).toHaveLength(0);
    expect(screen.getByTestId('voice-dock')).toBeTruthy();
    expect(within(panel).getByText('Listening only — your microphone is off.')).toBeTruthy();
    // In voice, others get a volume control and speaking indicators.
    expect(within(panel).getByLabelText('Volume of bob')).toBeTruthy();
    act(() => vs.cb.onFrame({ end: false, target: 0, speaker: 3, seq: 1 }, new Uint8Array([1])));
    await waitFor(() => expect(within(panel).getAllByTestId('voice-row')[0].querySelector('[data-on="true"]')).toBeTruthy());
    fireEvent.click(within(panel).getByRole('button', { name: 'Leave voice' }));
    expect(vs.closed).toBe(1);
    expect(screen.queryByTestId('voice-dock')).toBeNull();
  });

  it('per-person volume persists by Mumble name', async () => {
    const { panel } = await inVoice(true);
    fireEvent.change(within(panel).getByLabelText('Volume of bob'), { target: { value: '0.3' } });
    expect(localStorage.getItem('gawk:voice-volume:bob')).toBe('0.3');
  });

  // Review finding (PR #595): a per-person mute was saved as volume 0, so a
  // reload kept the person silent with the toggle showing them unmuted and
  // their real volume lost (D22: the VOLUME persists, not the mute).
  it('a per-person mute silences them without overwriting their saved volume', async () => {
    const gain = vi.spyOn(VoiceController.prototype, 'setSpeakerVolume');
    const { panel } = await inVoice(true);
    fireEvent.change(within(panel).getByLabelText('Volume of bob'), { target: { value: '0.3' } });
    fireEvent.click(within(panel).getByRole('button', { name: 'Mute bob for you' }));
    expect(gain.mock.calls.at(-1)).toEqual([3, 0]);
    expect(localStorage.getItem('gawk:voice-volume:bob')).toBe('0.3');
    fireEvent.click(within(panel).getByRole('button', { name: 'Unmute bob' }));
    expect(gain.mock.calls.at(-1)).toEqual([3, 0.3]);
    gain.mockRestore();
  });

  // Re-review finding (PR #595): the mute lived only in the row's state and
  // the live sink's gain, so a speaker created after the mute — the first
  // frame, a return after the 10 s idle release, any rejoin — played at the
  // saved volume. The volume a new speaker starts at must honour the mute.
  it('a per-person mute holds for a speaker created after it, and after a rejoin', async () => {
    const { room, panel, vs } = await inVoice(true);
    fireEvent.change(within(panel).getByLabelText('Volume of bob'), { target: { value: '0.3' } });
    fireEvent.click(within(panel).getByRole('button', { name: 'Mute bob for you' }));
    // bob has not spoken: his speaker is built later, at this volume.
    expect(playbackVolumeFors.at(-1)!(3)).toBe(0);
    // A drain: the rejoin builds a new playback, where bob is still muted.
    const built = playbackVolumeFors.length;
    act(() => vs.cb.onClosed({ code: CLOSE_CODE_SERVER_DRAINING, reason: 'drain' }));
    await waitFor(() => expect(room.voiceJoins).toBe(2));
    await act(async () => room.cbs.onVoiceTicket!('https://bridge.test/voice', new Uint8Array([2])));
    await waitFor(() => expect(playbackVolumeFors.length).toBeGreaterThan(built));
    expect(playbackVolumeFors.at(-1)!(3)).toBe(0);
    // bob reconnects to Mumble: a new session, so a new row, still muted.
    act(() =>
      room.cbs.onEvent({
        ...ROSTER,
        voiceRoster: [{ ...ROSTER.voiceRoster[0], session: 33 }, ROSTER.voiceRoster[1]],
      }),
    );
    await waitFor(() => expect(within(panel).getByRole('button', { name: 'Unmute bob' })).toBeTruthy());
    expect(playbackVolumeFors.at(-1)!(33)).toBe(0);
    // Unmuting restores the saved volume for the next speaker built.
    fireEvent.click(within(panel).getByRole('button', { name: 'Unmute bob' }));
    expect(playbackVolumeFors.at(-1)!(33)).toBe(0.3);
  });

  it('mute and deafen tell Mumble; a server mute disables the mic with the reason', async () => {
    const { vs } = await inVoice(false);
    const dock = screen.getByTestId('voice-dock');
    fireEvent.click(within(dock).getByRole('button', { name: 'Mute microphone' }));
    expect(vs.controls[vs.controls.length - 1]).toBe(VOICE_CONTROL_FLAG_SELF_MUTE);
    fireEvent.click(within(dock).getByRole('button', { name: 'Unmute microphone' }));
    fireEvent.click(within(dock).getByRole('button', { name: 'Deafen' }));
    expect(vs.controls[vs.controls.length - 1]).toBe(VOICE_CONTROL_FLAG_SELF_MUTE | VOICE_CONTROL_FLAG_SELF_DEAF);
    fireEvent.click(within(dock).getByRole('button', { name: 'Undeafen' }));
    act(() => vs.cb.onState({ flags: VOICE_STATE_FLAG_CONNECTED | VOICE_STATE_FLAG_SERVER_MUTE, session: 42, name: 'me [web]' }));
    const mic = within(dock).getByRole('button', { name: 'A Mumble admin muted you.' }) as HTMLButtonElement;
    expect(mic.disabled).toBe(true);
    expect(screen.getAllByText('A Mumble admin muted you.').length).toBeGreaterThan(0);
  });

  it('push-to-talk: the key talks while held, typing in a field does not, the hold button talks', async () => {
    await inVoice(false);
    const panel = screen.getByRole('complementary', { name: 'People and chat' });
    fireEvent.click(within(panel).getByRole('radio', { name: 'Push to talk' }));
    expect(localStorage.getItem('gawk:voice-mode')).toBe('ptt');
    expect(within(panel).getByText('V')).toBeTruthy();
    fireEvent.keyDown(window, { code: 'KeyV' });
    expect(useVoiceStore.getState().pttHeld).toBe(true);
    expect(captures[0].cb.shouldSend(0, 0)).toBe(true);
    fireEvent.keyUp(window, { code: 'KeyV' });
    expect(useVoiceStore.getState().pttHeld).toBe(false);
    const input = document.createElement('input');
    document.body.appendChild(input);
    fireEvent.keyDown(input, { code: 'KeyV' });
    expect(useVoiceStore.getState().pttHeld).toBe(false);
    input.remove();
    const hold = within(screen.getByTestId('voice-dock')).getByRole('button', { name: /Hold to talk/ });
    fireEvent.pointerDown(hold, { button: 0, pointerId: 1 });
    expect(useVoiceStore.getState().pttHeld).toBe(true);
    fireEvent.pointerUp(hold, { pointerId: 1 });
    expect(useVoiceStore.getState().pttHeld).toBe(false);
    // The key is configurable and remembered.
    fireEvent.click(within(panel).getByRole('button', { name: 'Change the push-to-talk key' }));
    fireEvent.keyDown(within(panel).getByRole('button', { name: 'Press a key…' }), { code: 'KeyT', key: 't' });
    expect(localStorage.getItem('gawk:voice-ptt-key')).toBe('KeyT');
    fireEvent.keyDown(window, { code: 'KeyT' });
    expect(useVoiceStore.getState().pttHeld).toBe(true);
    act(() => window.dispatchEvent(new Event('blur')));
    expect(useVoiceStore.getState().pttHeld).toBe(false);
  });

  it('4015: "You were removed from voice", the mic released, no rejoin', async () => {
    const { room, vs } = await inVoice(false);
    act(() => vs.cb.onClosed({ code: CLOSE_CODE_VOICE_REMOVED, reason: '' }));
    expect(screen.getByText('You were removed from voice.')).toBeTruthy();
    expect(captures[0].stopped).toBe(1);
    expect(screen.queryByTestId('voice-dock')).toBeNull();
    expect(room.voiceJoins).toBe(1);
  });

  it('4016: "Voice is unavailable: <reason>" with Try again', async () => {
    const { room, vs } = await inVoice(true);
    act(() => vs.cb.onClosed({ code: CLOSE_CODE_VOICE_UNAVAILABLE, reason: 'the Mumble server is full' }));
    expect(screen.getByText('Voice is unavailable: the Mumble server is full')).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: 'Try again' }));
    await waitFor(() => expect(room.voiceJoins).toBe(2));
  });

  it('4002: rejoins with a fresh ticket by itself', async () => {
    const { room, vs } = await inVoice(true);
    act(() => vs.cb.onClosed({ code: CLOSE_CODE_SERVER_DRAINING, reason: 'drain' }));
    await waitFor(() => expect(room.voiceJoins).toBe(2));
    expect(screen.getByText('Reconnecting voice…')).toBeTruthy();
  });

  it('a VoiceJoin refusal is voice\'s to explain, not a room toast', async () => {
    const room = await join(ROOM_CAP_VOICE);
    fireEvent.click(within(openPanel()).getByRole('button', { name: 'Listen only' }));
    await waitFor(() => expect(room.voiceJoins).toBe(1));
    act(() =>
      room.cbs.onEvent({
        seq: 3,
        kind: ROOM_EVENT_COMMAND_REJECTED,
        command: ROOM_COMMAND_VOICE_JOIN,
        reason: ROOM_REJECT_UNAVAILABLE,
        message: 'mumble down',
      }),
    );
    expect(screen.getByText('Voice is unavailable: mumble down')).toBeTruthy();
    expect(useRoomStore.getState().lastRejection).toBeNull();
  });

  it('asks the relay for the voice roster only where voice can play (none in jsdom)', async () => {
    await join(0, null);
    expect(roomSessions[0].opts.wantVoice).toBe(false);
  });
});
