// The talk gate (docs/80 D21, OD10): voice activity with its hangover,
// push-to-talk, and the blocks (mute, deafen, server mute) that override both.
import { describe, expect, it } from 'vitest';
import { TalkGate, VAD_HANGOVER_MS, VAD_THRESHOLD_RMS, VoiceActivityDetector, blockRms } from './talkGate';

const LOUD = VAD_THRESHOLD_RMS * 4;
const QUIET = VAD_THRESHOLD_RMS / 4;

describe('blockRms', () => {
  it('is the root mean square, and zero for an empty block', () => {
    expect(blockRms(new Float32Array(0))).toBe(0);
    expect(blockRms(new Float32Array([0.5, -0.5, 0.5, -0.5]))).toBeCloseTo(0.5);
    expect(blockRms(new Float32Array(480))).toBe(0);
  });
});

describe('VoiceActivityDetector', () => {
  it('opens on a loud block and holds for the hangover after the last one', () => {
    const vad = new VoiceActivityDetector();
    expect(vad.update(QUIET, 0)).toBe(false);
    expect(vad.update(LOUD, 10)).toBe(true);
    expect(vad.update(QUIET, 20)).toBe(true);
    expect(vad.update(LOUD, 100)).toBe(true); // re-armed
    expect(vad.update(QUIET, 100 + VAD_HANGOVER_MS - 1)).toBe(true);
    expect(vad.update(QUIET, 100 + VAD_HANGOVER_MS)).toBe(false);
  });

  it('is exactly at the threshold talking, and reset forgets the hangover', () => {
    const vad = new VoiceActivityDetector({ thresholdRms: 0.2, hangoverMs: 50 });
    expect(vad.update(0.2, 0)).toBe(true);
    vad.reset();
    expect(vad.update(0, 1)).toBe(false);
  });
});

describe('TalkGate', () => {
  it('voice activity: follows the detector', () => {
    const g = new TalkGate('vad');
    expect(g.update(LOUD, 0)).toBe(true);
    expect(g.transmitting).toBe(true);
    expect(g.update(QUIET, VAD_HANGOVER_MS + 1)).toBe(false);
    expect(g.transmitting).toBe(false);
  });

  it('push-to-talk: exactly while held, whatever the level', () => {
    const g = new TalkGate('ptt');
    expect(g.getMode()).toBe('ptt');
    expect(g.update(LOUD, 0)).toBe(false);
    g.setPttHeld(true);
    expect(g.update(0, 10)).toBe(true);
    g.setPttHeld(false);
    expect(g.update(LOUD, 20)).toBe(false);
  });

  it('blocked (muted, deafened, server-muted) sends nothing in either mode', () => {
    const g = new TalkGate('ptt');
    g.setPttHeld(true);
    g.setBlocked(true);
    expect(g.update(LOUD, 0)).toBe(false);
    g.setBlocked(false);
    expect(g.update(0, 10)).toBe(true);
    g.setMode('vad');
    g.setBlocked(true);
    expect(g.update(LOUD, 20)).toBe(false);
    // Unblocking does not resume a hangover from before the block.
    g.setBlocked(false);
    expect(g.update(QUIET, 30)).toBe(false);
  });

  it('a mode change drops a held key', () => {
    const g = new TalkGate('ptt');
    g.setPttHeld(true);
    g.setMode('vad');
    g.setMode('ptt');
    expect(g.update(LOUD, 0)).toBe(false);
    g.setMode('ptt'); // no-op
  });
});
