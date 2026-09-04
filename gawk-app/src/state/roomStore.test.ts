import { beforeEach, describe, expect, it } from 'vitest';
import { applyRoomEvent, isDynamicRoom, isRoomCreator, mayAttach, useRoomStore } from './roomStore';
import {
  ROOM_CLIENT_WEB_VIEWER,
  ROOM_COMMAND_ATTACH,
  ROOM_DETACH_REASON_CREATOR,
  ROOM_END_REASON_EMPTY,
  ROOM_EVENT_ATTACHMENT_ADDED,
  ROOM_EVENT_ATTACHMENT_REMOVED,
  ROOM_EVENT_ATTACHMENT_UPDATED,
  ROOM_EVENT_COMMAND_REJECTED,
  ROOM_EVENT_PARTICIPANT_JOINED,
  ROOM_EVENT_PARTICIPANT_LEFT,
  ROOM_EVENT_PARTICIPANT_UPDATED,
  ROOM_EVENT_ROOM_ENDING,
  ROOM_REJECT_LIMIT,
  ROOM_STATE_FLAG_ATTACH_OK,
  ROOM_STATE_FLAG_CREATOR,
  ROOM_STATE_FLAG_DYNAMIC,
  type RoomState,
} from '../transport/wire';

function state(over: Partial<RoomState> = {}): RoomState {
  return {
    flags: 0,
    caps: 0,
    seq: 5,
    yourId: 1,
    code: 'AB2CD3',
    displayName: '',
    creatorToken: new Uint8Array(0),
    key: new Uint8Array(0),
    attachments: [{ broadcastId: 'ABCDEF', label: 'tuhis', live: true, viewerCount: 1 }],
    participants: [{ id: 1, kind: ROOM_CLIENT_WEB_VIEWER, flags: 0, nickname: 'me', identity: '' }],
    ...over,
  };
}

const p = (id: number, nickname = `p${id}`) => ({
  id,
  kind: ROOM_CLIENT_WEB_VIEWER,
  flags: 0,
  nickname,
  identity: '',
});

describe('applyRoomEvent', () => {
  it('upserts participants and removes on left', () => {
    let s = applyRoomEvent(state(), { seq: 6, kind: ROOM_EVENT_PARTICIPANT_JOINED, participant: p(2) });
    expect(s.participants.map((x) => x.id)).toEqual([1, 2]);
    s = applyRoomEvent(s, { seq: 7, kind: ROOM_EVENT_PARTICIPANT_UPDATED, participant: p(2, 'renamed') });
    expect(s.participants[1].nickname).toBe('renamed');
    expect(s.participants).toHaveLength(2);
    s = applyRoomEvent(s, { seq: 8, kind: ROOM_EVENT_PARTICIPANT_LEFT, participant: { id: 2 } });
    expect(s.participants.map((x) => x.id)).toEqual([1]);
    expect(s.seq).toBe(8);
  });

  it('upserts attachments and removes on removed', () => {
    let s = applyRoomEvent(state(), {
      seq: 6,
      kind: ROOM_EVENT_ATTACHMENT_ADDED,
      attachment: { broadcastId: 'GHJKMN', label: 'second', live: true, viewerCount: 0 },
    });
    expect(s.attachments).toHaveLength(2);
    s = applyRoomEvent(s, {
      seq: 7,
      kind: ROOM_EVENT_ATTACHMENT_UPDATED,
      attachment: { broadcastId: 'ABCDEF', label: 'tuhis', live: false, viewerCount: 4 },
    });
    expect(s.attachments[0]).toEqual({ broadcastId: 'ABCDEF', label: 'tuhis', live: false, viewerCount: 4 });
    s = applyRoomEvent(s, {
      seq: 8,
      kind: ROOM_EVENT_ATTACHMENT_REMOVED,
      attachment: { broadcastId: 'ABCDEF' },
      reason: ROOM_DETACH_REASON_CREATOR,
    });
    expect(s.attachments.map((a) => a.broadcastId)).toEqual(['GHJKMN']);
  });

  it('leaves the snapshot alone for ending and rejection', () => {
    const s = state();
    expect(applyRoomEvent(s, { seq: 6, kind: ROOM_EVENT_ROOM_ENDING, reason: ROOM_END_REASON_EMPTY })).toBe(s);
    expect(
      applyRoomEvent(s, {
        seq: 5,
        kind: ROOM_EVENT_COMMAND_REJECTED,
        command: ROOM_COMMAND_ATTACH,
        reason: ROOM_REJECT_LIMIT,
        message: 'full',
      }),
    ).toBe(s);
  });
});

describe('flag helpers', () => {
  it('read the grants off the snapshot', () => {
    expect(isDynamicRoom(null)).toBe(false);
    expect(isDynamicRoom(state({ flags: ROOM_STATE_FLAG_DYNAMIC }))).toBe(true);
    expect(isRoomCreator(state({ flags: ROOM_STATE_FLAG_CREATOR }))).toBe(true);
    expect(isRoomCreator(state())).toBe(false);
    expect(mayAttach(state({ flags: ROOM_STATE_FLAG_ATTACH_OK }))).toBe(true);
    expect(mayAttach(state())).toBe(false);
  });
});

describe('useRoomStore', () => {
  beforeEach(() => useRoomStore.getState().reset());

  it('replaceState marks joined and clears the retry note', () => {
    useRoomStore.getState().setStatus('reconnecting', 'Reconnecting…');
    useRoomStore.getState().replaceState(state());
    const s = useRoomStore.getState();
    expect(s.status).toBe('joined');
    expect(s.retryNote).toBeNull();
    expect(s.snapshot?.code).toBe('AB2CD3');
  });

  it('records a rejection and a removal (with the label the snapshot knew) beside the snapshot', () => {
    const st = useRoomStore.getState();
    st.replaceState(state());
    st.applyEvent({
      seq: 5,
      kind: ROOM_EVENT_COMMAND_REJECTED,
      command: ROOM_COMMAND_ATTACH,
      reason: ROOM_REJECT_LIMIT,
      message: 'full',
    });
    expect(useRoomStore.getState().lastRejection).toEqual({
      command: ROOM_COMMAND_ATTACH,
      reason: ROOM_REJECT_LIMIT,
      message: 'full',
    });
    st.clearRejection();
    expect(useRoomStore.getState().lastRejection).toBeNull();
    st.applyEvent({
      seq: 6,
      kind: ROOM_EVENT_ATTACHMENT_REMOVED,
      attachment: { broadcastId: 'ABCDEF' },
      reason: ROOM_DETACH_REASON_CREATOR,
    });
    const after = useRoomStore.getState();
    expect(after.snapshot?.attachments).toEqual([]);
    expect(after.lastRemoval).toMatchObject({ broadcastId: 'ABCDEF', label: 'tuhis', reason: ROOM_DETACH_REASON_CREATOR });
  });

  it('keeps the RoomEnding reason for the 4007 that follows', () => {
    const st = useRoomStore.getState();
    st.replaceState(state());
    st.applyEvent({ seq: 6, kind: ROOM_EVENT_ROOM_ENDING, reason: ROOM_END_REASON_EMPTY });
    st.setEnded(null);
    expect(useRoomStore.getState().status).toBe('ended');
    expect(useRoomStore.getState().endReason).toBe(ROOM_END_REASON_EMPTY);
  });

  it('an event before any snapshot is ignored, an error is terminal', () => {
    const st = useRoomStore.getState();
    st.applyEvent({ seq: 1, kind: ROOM_EVENT_PARTICIPANT_JOINED, participant: p(2) });
    expect(useRoomStore.getState().snapshot).toBeNull();
    st.setError('full', 'The room is full');
    expect(useRoomStore.getState()).toMatchObject({ status: 'error', errorKind: 'full' });
  });
});
