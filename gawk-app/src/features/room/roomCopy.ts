// R42 (docs/44 §4.9): every sentence the room view can show for a relay
// state, in one pure module so each has a test and no component inlines a
// second wording. Room codes are deliberately absent from every diagnostic
// string here — the code is a joinable secret (D16) and these lines end up
// in logs and pasted diagnostics; only the on-screen chrome shows it.

import type { RoomFailureKind } from '../../transport/room-session';
import {
  ROOM_COMMAND_ATTACH,
  ROOM_COMMAND_DETACH,
  ROOM_COMMAND_END_ROOM,
  ROOM_COMMAND_SET_NICKNAME,
  ROOM_DETACH_REASON_CREATOR,
  ROOM_DETACH_REASON_EXPIRED,
  ROOM_DETACH_REASON_PUBLISHER,
  ROOM_DETACH_REASON_ROOM_END,
  ROOM_END_REASON_CREATOR,
  ROOM_END_REASON_EMPTY,
  ROOM_END_REASON_OPERATOR,
  ROOM_REJECT_ALREADY_ATTACHED,
  ROOM_REJECT_BAD_PROOF,
  ROOM_REJECT_FORBIDDEN,
  ROOM_REJECT_LIMIT,
  ROOM_REJECT_NOT_FOUND,
  ROOM_REJECT_UNAVAILABLE,
  ROOM_REJECT_UNSUPPORTED,
} from '../../transport/wire';

export interface Card {
  title: string;
  body: string;
}

// 4007: the room ended. `reason` is what the preceding RoomEnding said, or
// null when the close arrived without one (a reconnect into a gone room).
export function endedCard(reason: number | null): Card {
  switch (reason) {
    case ROOM_END_REASON_CREATOR:
      return { title: 'Room ended', body: 'The room was ended by its creator.' };
    case ROOM_END_REASON_OPERATOR:
      return { title: 'Room ended', body: 'The room was closed by the operator.' };
    case ROOM_END_REASON_EMPTY:
      return { title: 'Room ended', body: 'Everyone left, so the room closed.' };
    default:
      return { title: 'Room ended', body: 'This room is over. Any streams that were in it keep working by their own codes.' };
  }
}

export function errorCard(kind: RoomFailureKind): Card {
  switch (kind) {
    case 'notFound':
      return { title: 'No such room', body: 'Nothing is at this code right now. Check it, or ask for a fresh link.' };
    case 'full':
      return { title: 'Room full', body: 'This room has reached its participant limit. Try again in a moment.' };
    case 'forbidden':
      return {
        title: 'Wrong room key',
        body: 'This room refused the key in your link. Ask the room’s owner for a current link.',
      };
    case 'refused':
      return {
        title: 'Room not found or refused',
        body: 'The server didn’t let this session in. The code may be wrong, the room may have ended, or rooms may be off on this server.',
      };
    case 'lost':
      return {
        title: 'Lost the room',
        body: 'The connection dropped and couldn’t be restored. The streams may still be live — try again in a moment.',
      };
  }
}

// The toast for an attachment the relay removed.
export function removalToast(label: string, reason: number): string {
  const who = label !== '' ? `${label}’s stream` : 'A stream';
  switch (reason) {
    case ROOM_DETACH_REASON_PUBLISHER:
      return `${who} left the room`;
    case ROOM_DETACH_REASON_CREATOR:
      return `${who} was removed by the room’s creator`;
    case ROOM_DETACH_REASON_EXPIRED:
      return `${who} went away and was removed`;
    case ROOM_DETACH_REASON_ROOM_END:
      return `${who} was removed — the room is ending`;
    default:
      return `${who} was removed from the room`;
  }
}

function commandName(command: number): string {
  switch (command) {
    case ROOM_COMMAND_ATTACH:
      return 'Couldn’t add the stream';
    case ROOM_COMMAND_DETACH:
      return 'Couldn’t remove the stream';
    case ROOM_COMMAND_SET_NICKNAME:
      return 'Couldn’t change the nickname';
    case ROOM_COMMAND_END_ROOM:
      return 'Couldn’t end the room';
    default:
      return 'The room refused that';
  }
}

// A CommandRejected event, as one line. The relay's own message is appended
// when it adds something (it never carries a code).
export function rejectionToast(command: number, reason: number, message: string): string {
  const head = commandName(command);
  let why: string;
  switch (reason) {
    case ROOM_REJECT_LIMIT:
      why = 'the room is at its limit';
      break;
    case ROOM_REJECT_BAD_PROOF:
      why = 'the stream’s proof was refused';
      break;
    case ROOM_REJECT_NOT_FOUND:
      why = 'nothing by that id is here';
      break;
    case ROOM_REJECT_FORBIDDEN:
      why = 'you’re not allowed to';
      break;
    case ROOM_REJECT_ALREADY_ATTACHED:
      why = 'that stream is already in a room';
      break;
    case ROOM_REJECT_UNSUPPORTED:
      why = 'this server doesn’t support it';
      break;
    case ROOM_REJECT_UNAVAILABLE:
      why = 'the server can’t right now';
      break;
    default:
      why = 'the server said no';
  }
  const trimmed = message.trim();
  return trimmed !== '' && trimmed.toLowerCase() !== why ? `${head}: ${why} (${trimmed})` : `${head}: ${why}`;
}

export const HIDDEN_CARD: Card = {
  title: 'You’re still in the room',
  body: 'Nothing is downloading. Pick Grid or Focus to watch again.',
};

export const EMPTY_ROOM_CARD: Card = {
  title: 'Nobody is streaming yet',
  body: 'You’re in the room. Streams appear here the moment someone attaches one.',
};

export const SHARE_CODE_NOTE = 'Anyone with the room code can also see the codes of the streams in it.';

export const RECONNECTING_NOTE = 'Reconnecting to the room…';
export const DRAINING_NOTE = 'Room server is updating — reconnecting…';
