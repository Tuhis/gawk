// The merged voice roster the panel shows (R79, docs/80 D15, OD7): one list
// of everyone in the room's Mumble channel. A bridged gawk participant shows
// their gawk nickname (from the participant roster, by participant ID); a
// native Mumble user shows their Mumble name and a "Mumble" marker. Mute and
// deafen state is everyone's; speaking is only ever local (speaking.ts).

import {
  ROOM_VOICE_KIND_GAWK,
  ROOM_VOICE_USER_DEAF,
  ROOM_VOICE_USER_MUTE,
  ROOM_VOICE_USER_SELF_DEAF,
  ROOM_VOICE_USER_SELF_MUTE,
  ROOM_VOICE_USER_SUPPRESSED,
  type RoomParticipant,
  type VoiceRosterEntry,
} from '../../../transport/wire';

export interface VoiceRosterRow {
  session: number;
  // What the row says: the gawk nickname, or the Mumble name.
  label: string;
  // The Mumble name, the per-person volume's key.
  mumbleName: string;
  mumble: boolean;
  you: boolean;
  muted: boolean; // by themselves or by the server
  deafened: boolean;
  serverMuted: boolean;
  serverDeafened: boolean;
  suppressed: boolean;
}

export function mergeVoiceRoster(
  entries: readonly VoiceRosterEntry[],
  participants: readonly RoomParticipant[],
  yourId: number,
  // The own Mumble session from VoiceState, once this browser is in voice.
  ownSession: number | null,
): VoiceRosterRow[] {
  const byId = new Map(participants.map((p) => [p.id, p]));
  return entries.map((e) => {
    const gawk = e.kind === ROOM_VOICE_KIND_GAWK;
    const p = gawk ? byId.get(e.participantId) : undefined;
    const serverMuted = (e.flags & ROOM_VOICE_USER_MUTE) !== 0;
    const serverDeafened = (e.flags & ROOM_VOICE_USER_DEAF) !== 0;
    return {
      session: e.session,
      label: p?.nickname || e.name,
      mumbleName: e.name,
      mumble: !gawk,
      you: (gawk && e.participantId === yourId) || (ownSession !== null && ownSession !== 0 && e.session === ownSession),
      muted: serverMuted || (e.flags & ROOM_VOICE_USER_SELF_MUTE) !== 0,
      deafened: serverDeafened || (e.flags & ROOM_VOICE_USER_SELF_DEAF) !== 0,
      serverMuted,
      serverDeafened,
      suppressed: (e.flags & ROOM_VOICE_USER_SUPPRESSED) !== 0,
    };
  });
}
