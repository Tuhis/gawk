// The voice panel's words (R79), kept apart from the components so they are
// unit-tested on their own.

import { LOST_MESSAGE } from './voiceController';
import type { VoiceViewState } from './voiceController';

// docs/80 §6: the channel is Mumble's, shared and recordable.
export const VOICE_NOTICE =
  'This is a Mumble channel shared with people outside gawk; anyone in it can hear you, and Mumble can record it.';
export const VOICE_ROOM_UNAVAILABLE = 'Voice is unavailable right now.';

export function unavailableText(reason: string | null): string {
  return reason ? `Voice is unavailable: ${reason}` : VOICE_ROOM_UNAVAILABLE;
}

export function failureText(s: Pick<VoiceViewState, 'error'>): string {
  const e = s.error;
  if (!e) return LOST_MESSAGE;
  switch (e.phase) {
    case 'capture':
      return `Couldn’t use the microphone. ${e.message}`;
    case 'ticket':
      return `Couldn’t join voice. ${e.message}`;
    default:
      return `Couldn’t reach the voice server. ${e.message}`;
  }
}

// Why the microphone (or playback) is out of the user's hands, from VoiceState.
export function serverStateNote(s: Pick<VoiceViewState, 'serverMuted' | 'serverDeafened' | 'suppressed'>): string | null {
  if (s.serverDeafened) return 'A Mumble admin deafened you.';
  if (s.serverMuted) return 'A Mumble admin muted you.';
  if (s.suppressed) return 'You can’t speak in this Mumble channel.';
  return null;
}

