import { describe, expect, it } from 'vitest';
import {
  VOICE_NOTICE,
  failureText,
  noiseCostText,
  noiseStatusText,
  serverStateNote,
  unavailableText,
} from './voiceCopy';
import type { NoiseStatus } from './voiceCapture';

describe('voice copy', () => {
  it('says the channel is Mumble’s and shared, and does not talk about recording (docs/80 §6, docs/82 OD5)', () => {
    expect(VOICE_NOTICE).toMatch(/Mumble channel shared with people outside gawk/);
    expect(VOICE_NOTICE).not.toMatch(/record/i);
  });

  it('words each failure phase apart', () => {
    expect(failureText({ error: null })).toBe('Lost the voice connection.');
    expect(failureText({ error: { phase: 'capture', message: 'x' } })).toMatch(/^Couldn’t use the microphone/);
    expect(failureText({ error: { phase: 'ticket', message: 'x' } })).toMatch(/^Couldn’t join voice/);
    expect(failureText({ error: { phase: 'connect', message: 'x' } })).toMatch(/^Couldn’t reach the voice server/);
  });

  it('says why voice is unavailable when it knows, and what the server took away', () => {
    expect(unavailableText('full')).toBe('Voice is unavailable: full');
    expect(unavailableText(null)).toBe('Voice is unavailable right now.');
    const none = { serverMuted: false, serverDeafened: false, suppressed: false };
    expect(serverStateNote(none)).toBeNull();
    expect(serverStateNote({ ...none, suppressed: true })).toMatch(/can’t speak/);
    expect(serverStateNote({ ...none, serverMuted: true })).toMatch(/muted you/);
    expect(serverStateNote({ ...none, serverMuted: true, serverDeafened: true })).toMatch(/deafened you/);
  });
});

describe('Audio section copy (docs/81 D13, D15)', () => {
  const base: NoiseStatus = {
    requested: 'rnnoise',
    running: 'rnnoise',
    loading: false,
    fallback: null,
    pending: false,
    offAvailable: true,
  };

  it('owns RNNoise’s 20 ms, and what Browser and Off cost', () => {
    expect(noiseCostText('rnnoise')).toMatch(/adds 20 ms/);
    expect(noiseCostText('browser')).toMatch(/no added delay/);
    // Off still gates on level: it does not send everything.
    expect(noiseCostText('off')).toBe('No noise suppression: the level threshold decides what is sent.');
  });

  it('says nothing when all is well, loading while it loads, and why it fell back and to what', () => {
    expect(noiseStatusText(null)).toBeNull();
    expect(noiseStatusText(base)).toBeNull();
    expect(noiseStatusText({ ...base, loading: true })).toBe('RNNoise is loading…');
    expect(noiseStatusText({ ...base, running: 'off', fallback: 'it is too slow on this device (52 % of real time)' })).toBe(
      'RNNoise isn’t available here: it is too slow on this device (52 % of real time). Using Off.',
    );
  });

  it('says nothing of a fallback while Browser or Off is the choice', () => {
    expect(noiseStatusText({ ...base, requested: 'off', running: 'off', fallback: 'it is too slow on this device (52 % of real time)' })).toBeNull();
  });

  it('says when a choice waits for the next join', () => {
    expect(noiseStatusText({ ...base, requested: 'browser', pending: true })).toBe(
      'Browser applies when you next join; RNNoise for now.',
    );
    expect(
      noiseStatusText({ ...base, running: 'browser', fallback: 'the download failed', pending: true }),
    ).toMatch(/^RNNoise isn’t available here: the download failed\. Using Browser\. RNNoise applies when you next join/);
  });
});
