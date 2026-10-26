// The merged voice roster (docs/80 D15, OD7).
import { describe, expect, it } from 'vitest';
import {
  ROOM_CLIENT_WEB_VIEWER,
  ROOM_VOICE_KIND_GAWK,
  ROOM_VOICE_KIND_MUMBLE,
  ROOM_VOICE_USER_DEAF,
  ROOM_VOICE_USER_MUTE,
  ROOM_VOICE_USER_SELF_DEAF,
  ROOM_VOICE_USER_SELF_MUTE,
  ROOM_VOICE_USER_SUPPRESSED,
} from '../../../transport/wire';
import { mergeVoiceRoster } from './voiceRoster';

const participants = [
  { id: 2, kind: ROOM_CLIENT_WEB_VIEWER, flags: 0, nickname: 'tuhis', identity: '' },
  { id: 5, kind: ROOM_CLIENT_WEB_VIEWER, flags: 0, nickname: 'other', identity: '' },
];

describe('mergeVoiceRoster', () => {
  it('shows a bridged participant by gawk nickname and a native user by Mumble name, marked', () => {
    const rows = mergeVoiceRoster(
      [
        { session: 3, kind: ROOM_VOICE_KIND_MUMBLE, participantId: 0, flags: ROOM_VOICE_USER_SELF_MUTE, name: 'bob' },
        { session: 9, kind: ROOM_VOICE_KIND_GAWK, participantId: 2, flags: 0, name: 'tuhis [web]' },
        { session: 11, kind: ROOM_VOICE_KIND_GAWK, participantId: 5, flags: ROOM_VOICE_USER_SELF_DEAF, name: 'other [web]' },
      ],
      participants,
      2,
      null,
    );
    expect(rows.map((r) => [r.label, r.mumble, r.you])).toEqual([
      ['bob', true, false],
      ['tuhis', false, true],
      ['other', false, false],
    ]);
    expect(rows[0].muted).toBe(true);
    expect(rows[0].mumbleName).toBe('bob');
    expect(rows[1].mumbleName).toBe('tuhis [web]');
    expect(rows[2].deafened).toBe(true);
  });

  it('falls back to the Mumble name for a participant not (yet) in the room roster', () => {
    const [row] = mergeVoiceRoster(
      [{ session: 4, kind: ROOM_VOICE_KIND_GAWK, participantId: 99, flags: 0, name: 'ghost [web]' }],
      participants,
      2,
      null,
    );
    expect(row.label).toBe('ghost [web]');
    expect(row.mumble).toBe(false);
  });

  it('carries server mute, deafen and suppression apart from self state', () => {
    const [row] = mergeVoiceRoster(
      [
        {
          session: 4,
          kind: ROOM_VOICE_KIND_MUMBLE,
          participantId: 0,
          flags: ROOM_VOICE_USER_MUTE | ROOM_VOICE_USER_DEAF | ROOM_VOICE_USER_SUPPRESSED,
          name: 'x',
        },
      ],
      [],
      2,
      null,
    );
    expect(row).toMatchObject({ muted: true, deafened: true, serverMuted: true, serverDeafened: true, suppressed: true });
  });

  it('marks the own row by the VoiceState session too', () => {
    const rows = mergeVoiceRoster(
      [{ session: 42, kind: ROOM_VOICE_KIND_MUMBLE, participantId: 0, flags: 0, name: 'me [web]' }],
      [],
      2,
      42,
    );
    expect(rows[0].you).toBe(true);
    expect(mergeVoiceRoster([{ session: 0, kind: 0, participantId: 0, flags: 0, name: 'n' }], [], 2, 0)[0].you).toBe(false);
  });
});
