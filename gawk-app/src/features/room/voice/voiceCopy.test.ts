import { describe, expect, it } from 'vitest';
import { VOICE_NOTICE, failureText, serverStateNote, unavailableText } from './voiceCopy';

describe('voice copy', () => {
  it('says the channel is Mumble’s, shared, and recordable (docs/80 §6)', () => {
    expect(VOICE_NOTICE).toMatch(/Mumble channel shared with people outside gawk/);
    expect(VOICE_NOTICE).toMatch(/can record it/);
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
