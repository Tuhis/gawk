// R42 (docs/44 §4.6): the live room snapshot and the control session's
// status, as one zustand store the room screen renders from. NOT persisted —
// a room is rebuilt from the relay's RoomState on every (re)connect and a
// stale copy would be exactly the kind of "merged" state D5 forbids.
//
// The delta application is a pure function (applyRoomEvent) so the store
// stays a thin holder and the event semantics have a unit test each.

import { create } from 'zustand';

import type { RoomFailureKind } from '../transport/room-session';
import {
  ROOM_EVENT_ATTACHMENT_ADDED,
  ROOM_EVENT_ATTACHMENT_REMOVED,
  ROOM_EVENT_ATTACHMENT_UPDATED,
  ROOM_EVENT_COMMAND_REJECTED,
  ROOM_EVENT_PARTICIPANT_JOINED,
  ROOM_EVENT_PARTICIPANT_LEFT,
  ROOM_EVENT_PARTICIPANT_UPDATED,
  ROOM_EVENT_ROOM_ENDING,
  ROOM_STATE_FLAG_ATTACH_OK,
  ROOM_STATE_FLAG_CREATOR,
  ROOM_STATE_FLAG_DYNAMIC,
  type RoomEvent,
  type RoomState,
} from '../transport/wire';

export type RoomStatus =
  | 'idle'
  | 'connecting'
  | 'joined'
  | 'reconnecting'
  // 4007: the room ended (endReason says why, when the relay said).
  | 'ended'
  // A terminal failure (errorKind says which).
  | 'error';

export interface RoomRejection {
  command: number;
  reason: number;
  message: string;
}

// The last attachment the relay removed — the room screen's toast source.
export interface RoomRemoval {
  broadcastId: string;
  label: string;
  reason: number; // ROOM_DETACH_REASON_*
  at: number;
}

export interface RoomStoreState {
  status: RoomStatus;
  snapshot: RoomState | null;
  retryNote: string | null;
  endReason: number | null;
  errorKind: RoomFailureKind | null;
  errorMessage: string | null;
  lastRejection: RoomRejection | null;
  lastRemoval: RoomRemoval | null;

  setStatus: (status: RoomStatus, retryNote?: string | null) => void;
  replaceState: (state: RoomState) => void;
  applyEvent: (ev: RoomEvent) => void;
  setEnded: (reason: number | null) => void;
  setError: (kind: RoomFailureKind, message: string) => void;
  clearRejection: () => void;
  reset: () => void;
}

// Pure: the snapshot after one delta. Unknown kinds leave it untouched.
export function applyRoomEvent(state: RoomState, ev: RoomEvent): RoomState {
  switch (ev.kind) {
    case ROOM_EVENT_PARTICIPANT_JOINED:
    case ROOM_EVENT_PARTICIPANT_UPDATED: {
      const p = ev.participant;
      const idx = state.participants.findIndex((x) => x.id === p.id);
      const participants = [...state.participants];
      if (idx === -1) participants.push(p);
      else participants[idx] = p;
      return { ...state, seq: ev.seq, participants };
    }
    case ROOM_EVENT_PARTICIPANT_LEFT:
      return {
        ...state,
        seq: ev.seq,
        participants: state.participants.filter((x) => x.id !== ev.participant.id),
      };
    case ROOM_EVENT_ATTACHMENT_ADDED:
    case ROOM_EVENT_ATTACHMENT_UPDATED: {
      const a = ev.attachment;
      const idx = state.attachments.findIndex((x) => x.broadcastId === a.broadcastId);
      const attachments = [...state.attachments];
      if (idx === -1) attachments.push(a);
      else attachments[idx] = a;
      return { ...state, seq: ev.seq, attachments };
    }
    case ROOM_EVENT_ATTACHMENT_REMOVED:
      return {
        ...state,
        seq: ev.seq,
        attachments: state.attachments.filter((x) => x.broadcastId !== ev.attachment.broadcastId),
      };
    case ROOM_EVENT_ROOM_ENDING:
    case ROOM_EVENT_COMMAND_REJECTED:
      return state;
  }
}

export function isDynamicRoom(s: RoomState | null): boolean {
  return s !== null && (s.flags & ROOM_STATE_FLAG_DYNAMIC) !== 0;
}

export function isRoomCreator(s: RoomState | null): boolean {
  return s !== null && (s.flags & ROOM_STATE_FLAG_CREATOR) !== 0;
}

export function mayAttach(s: RoomState | null): boolean {
  return s !== null && (s.flags & ROOM_STATE_FLAG_ATTACH_OK) !== 0;
}

const INITIAL = {
  status: 'idle' as RoomStatus,
  snapshot: null,
  retryNote: null,
  endReason: null,
  errorKind: null,
  errorMessage: null,
  lastRejection: null,
  lastRemoval: null,
};

export const useRoomStore = create<RoomStoreState>((set, get) => ({
  ...INITIAL,

  setStatus: (status, retryNote = null) => set({ status, retryNote }),

  replaceState: (snapshot) =>
    set({ snapshot, status: 'joined', retryNote: null }),

  applyEvent: (ev) => {
    const s = get();
    if (ev.kind === ROOM_EVENT_COMMAND_REJECTED) {
      set({ lastRejection: { command: ev.command, reason: ev.reason, message: ev.message } });
      return;
    }
    if (ev.kind === ROOM_EVENT_ROOM_ENDING) {
      set({ endReason: ev.reason });
      return;
    }
    if (s.snapshot === null) return;
    const patch: Partial<RoomStoreState> = { snapshot: applyRoomEvent(s.snapshot, ev) };
    if (ev.kind === ROOM_EVENT_ATTACHMENT_REMOVED) {
      const gone = s.snapshot.attachments.find((a) => a.broadcastId === ev.attachment.broadcastId);
      patch.lastRemoval = {
        broadcastId: ev.attachment.broadcastId,
        label: gone?.label ?? '',
        reason: ev.reason,
        at: Date.now(),
      };
    }
    set(patch);
  },

  setEnded: (reason) => set((s) => ({ status: 'ended', retryNote: null, endReason: reason ?? s.endReason })),

  setError: (kind, message) => set({ status: 'error', retryNote: null, errorKind: kind, errorMessage: message }),

  clearRejection: () => set({ lastRejection: null }),

  reset: () => set({ ...INITIAL }),
}));
