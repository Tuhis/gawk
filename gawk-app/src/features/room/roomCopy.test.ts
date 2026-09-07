import { describe, expect, it } from 'vitest';
import { endedCard, errorCard, rejectionToast, removalToast } from './roomCopy';
import {
  ROOM_COMMAND_ATTACH,
  ROOM_COMMAND_END_ROOM,
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
} from '../../transport/wire';

describe('room copy', () => {
  it('has a distinct ending for every reason, and a hedge for none', () => {
    const bodies = [ROOM_END_REASON_CREATOR, ROOM_END_REASON_OPERATOR, ROOM_END_REASON_EMPTY, null].map(
      (r) => endedCard(r).body,
    );
    expect(new Set(bodies).size).toBe(4);
    expect(endedCard(null).body).toMatch(/own codes/);
  });

  it('names every failure kind', () => {
    expect(errorCard('notFound').title).toBe('No such room');
    expect(errorCard('full').title).toBe('Room full');
    expect(errorCard('forbidden').title).toBe('Wrong room key');
    expect(errorCard('refused').title).toBe('Room not found or refused');
    expect(errorCard('lost').title).toBe('Lost the room');
  });

  it('words a removal by its reason, using the label when there is one', () => {
    expect(removalToast('tuhis', ROOM_DETACH_REASON_PUBLISHER)).toBe('tuhis’s stream left the room');
    expect(removalToast('', ROOM_DETACH_REASON_CREATOR)).toBe('A stream was removed by the room’s creator');
    expect(removalToast('x', ROOM_DETACH_REASON_EXPIRED)).toMatch(/went away/);
    expect(removalToast('x', ROOM_DETACH_REASON_ROOM_END)).toMatch(/ending/);
    expect(removalToast('x', 99)).toMatch(/removed from the room/);
  });

  it('words a rejection by command and reason, appending a non-redundant relay message', () => {
    expect(rejectionToast(ROOM_COMMAND_ATTACH, ROOM_REJECT_LIMIT, '')).toBe(
      'Couldn’t add the stream: the room is at its limit',
    );
    expect(rejectionToast(ROOM_COMMAND_ATTACH, ROOM_REJECT_BAD_PROOF, 'resume token mismatch')).toBe(
      'Couldn’t add the stream: the stream’s proof was refused (resume token mismatch)',
    );
    expect(rejectionToast(ROOM_COMMAND_END_ROOM, ROOM_REJECT_FORBIDDEN, '')).toMatch(/end the room: you’re not allowed/);
    expect(rejectionToast(ROOM_COMMAND_ATTACH, ROOM_REJECT_ALREADY_ATTACHED, '')).toMatch(/already in a room/);
    expect(rejectionToast(0x41, 99, '')).toBe('The room refused that: the server said no');
  });
});
