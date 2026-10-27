// The voice panel's words (R79), kept apart from the components so they are
// unit-tested on their own.

import { LOST_MESSAGE } from './voiceController';
import type { VoiceViewState } from './voiceController';
import type { NoiseStatus } from './voiceCapture';
import { RNNOISE_DELAY_MS } from './voiceFraming';
import type { NoiseMode } from './voicePrefs';

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


// ---- The Audio section (R80, docs/81 D13, D15) ----------------------------

export const NOISE_MODE_LABEL: Record<NoiseMode, string> = {
  rnnoise: 'RNNoise',
  browser: 'Browser',
  off: 'Off',
};

// What the chosen mode costs, in one line (D15; D8 owns the 20 ms).
export function noiseCostText(mode: NoiseMode): string {
  switch (mode) {
    case 'rnnoise':
      return `RNNoise adds ${RNNOISE_DELAY_MS} ms and opens on speech, not on loudness.`;
    case 'browser':
      return 'The browser’s own noise suppression: no added delay.';
    default:
      return 'No noise suppression: the level threshold decides what is sent.';
  }
}

// The status line under the selector, or null when there is nothing to say.
export function noiseStatusText(n: NoiseStatus | null): string | null {
  if (!n) return null;
  const parts: string[] = [];
  // A fallback matters only while RNNoise is the choice: a late watchdog or
  // download failure after a switch away from it is nobody's business.
  if (n.fallback !== null && n.requested === 'rnnoise') {
    parts.push(`RNNoise isn’t available here: ${n.fallback}. Using ${NOISE_MODE_LABEL[n.running]}.`);
  } else if (n.loading) {
    parts.push('RNNoise is loading…');
  }
  if (n.pending) {
    parts.push(`${NOISE_MODE_LABEL[n.requested]} applies when you next join; ${NOISE_MODE_LABEL[n.running]} for now.`);
  }
  return parts.length > 0 ? parts.join(' ') : null;
}
