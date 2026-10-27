// The talk gate (docs/80 D21, OD10): voice activity with its hangover,
// push-to-talk, and the blocks (mute, deafen, server mute) that override both;
// R80's speech detector over RNNoise's probability (docs/81 D9) and the
// configurable level threshold (D10).
import { describe, expect, it } from 'vitest';
import {
  SPEECH_HOLD_MS,
  SpeechDetector,
  TalkGate,
  VAD_HANGOVER_MS,
  VAD_THRESHOLD_RMS,
  VoiceActivityDetector,
  blockRms,
  dbfsToRms,
  rmsToDbfs,
} from './talkGate';

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
    expect(g.update(LOUD, null, 0)).toBe(true);
    expect(g.transmitting).toBe(true);
    expect(g.update(QUIET, null, VAD_HANGOVER_MS + 1)).toBe(false);
    expect(g.transmitting).toBe(false);
  });

  it('push-to-talk: exactly while held, whatever the level', () => {
    const g = new TalkGate('ptt');
    expect(g.getMode()).toBe('ptt');
    expect(g.update(LOUD, null, 0)).toBe(false);
    g.setPttHeld(true);
    expect(g.update(0, null, 10)).toBe(true);
    g.setPttHeld(false);
    expect(g.update(LOUD, null, 20)).toBe(false);
  });

  it('blocked (muted, deafened, server-muted) sends nothing in either mode', () => {
    const g = new TalkGate('ptt');
    g.setPttHeld(true);
    g.setBlocked(true);
    expect(g.update(LOUD, null, 0)).toBe(false);
    g.setBlocked(false);
    expect(g.update(0, null, 10)).toBe(true);
    g.setMode('vad');
    g.setBlocked(true);
    expect(g.update(LOUD, null, 20)).toBe(false);
    // Unblocking does not resume a hangover from before the block.
    g.setBlocked(false);
    expect(g.update(QUIET, null, 30)).toBe(false);
  });

  it('a mode change drops a held key', () => {
    const g = new TalkGate('ptt');
    g.setPttHeld(true);
    g.setMode('vad');
    g.setMode('ptt');
    expect(g.update(LOUD, null, 0)).toBe(false);
    g.setMode('ptt'); // no-op
  });
});

describe('dBFS', () => {
  it('-40 dBFS is the R79 threshold, and silence is pinned at -100', () => {
    expect(VAD_THRESHOLD_RMS).toBeCloseTo(0.01, 10);
    expect(dbfsToRms(-20)).toBeCloseTo(0.1, 10);
    expect(rmsToDbfs(0.1)).toBeCloseTo(-20, 10);
    expect(rmsToDbfs(0)).toBe(-100);
  });
});

// docs/81 D9, frame by frame: on 0.90, off 0.70, hold 300 ms, floor -50 dBFS.
describe('SpeechDetector', () => {
  const SPEECHY = dbfsToRms(-30);
  const UNDER_FLOOR = dbfsToRms(-55);

  it('opens at on, stays open down to off, and closes below it after the hold', () => {
    const d = new SpeechDetector();
    expect(d.update(0.89, SPEECHY, 0)).toBe(false);
    expect(d.update(0.9, SPEECHY, 10)).toBe(true);
    // Hysteresis: 0.7..0.9 keeps it open while it is open.
    expect(d.update(0.75, SPEECHY, 20)).toBe(true);
    expect(d.update(0.7, SPEECHY, 30)).toBe(true);
    // Below off: the hold runs from the last speech block (30 ms).
    expect(d.update(0.69, SPEECHY, 40)).toBe(true);
    expect(d.update(0.69, SPEECHY, 30 + SPEECH_HOLD_MS - 10)).toBe(true);
    expect(d.update(0.69, SPEECHY, 30 + SPEECH_HOLD_MS)).toBe(false);
  });

  it('does not open between off and on from closed', () => {
    const d = new SpeechDetector();
    expect(d.update(0.8, SPEECHY, 0)).toBe(false);
    expect(d.update(0.85, SPEECHY, 10)).toBe(false);
  });

  it('a block quieter than the floor counts as probability 0, closing the hysteresis too', () => {
    const d = new SpeechDetector();
    expect(d.update(0.99, UNDER_FLOOR, 0)).toBe(false);
    expect(d.update(0.99, SPEECHY, 10)).toBe(true);
    // Under the floor: not speech, so after it 0.8 no longer holds it open.
    expect(d.update(0.99, UNDER_FLOOR, 20)).toBe(true); // held
    expect(d.update(0.8, SPEECHY, 20 + SPEECH_HOLD_MS)).toBe(false);
  });

  it('the threshold moves on, and off follows at on - 0.2', () => {
    const d = new SpeechDetector();
    d.setThreshold(0.6);
    expect(d.update(0.6, SPEECHY, 0)).toBe(true);
    expect(d.update(0.41, SPEECHY, 10)).toBe(true);
    expect(d.update(0.39, SPEECHY, 20)).toBe(true); // held
    expect(d.update(0.39, SPEECHY, 10 + SPEECH_HOLD_MS)).toBe(false);
    expect(d.update(0.5, SPEECHY, 1000)).toBe(false);
  });

  it('reset closes it and forgets the hold', () => {
    const d = new SpeechDetector();
    d.update(0.95, SPEECHY, 0);
    d.reset();
    expect(d.update(0.8, SPEECHY, 10)).toBe(false);
  });
});

describe('TalkGate with RNNoise', () => {
  const SPEECHY = dbfsToRms(-30);

  it('a probability drives the speech detector; null falls back to the level gate', () => {
    const g = new TalkGate('vad');
    // Loud but not speech: RNNoise says no.
    expect(g.update(0.5, 0.1, 0)).toBe(false);
    expect(g.update(SPEECHY, 0.95, 1000)).toBe(true);
    // Without RNNoise the same quiet-for-a-level block is judged on level.
    expect(g.update(VAD_THRESHOLD_RMS / 4, null, 5000)).toBe(false);
    expect(g.update(VAD_THRESHOLD_RMS * 4, null, 6000)).toBe(true);
  });

  it('the user thresholds apply to the next block', () => {
    const g = new TalkGate('vad');
    g.setSpeechThreshold(0.5);
    expect(g.update(SPEECHY, 0.55, 0)).toBe(true);
    g.setLevelThresholdDb(-20);
    expect(g.update(dbfsToRms(-30), null, 5000)).toBe(false);
    expect(g.update(dbfsToRms(-19), null, 6000)).toBe(true);
  });

  it('push-to-talk ignores the probability (docs/81 D18)', () => {
    const g = new TalkGate('ptt');
    expect(g.update(SPEECHY, 0.99, 0)).toBe(false);
    g.setPttHeld(true);
    expect(g.update(0, 0, 10)).toBe(true);
  });

  it('blocking resets the speech detector too', () => {
    const g = new TalkGate('vad');
    expect(g.update(SPEECHY, 0.95, 0)).toBe(true);
    g.setBlocked(true);
    g.setBlocked(false);
    expect(g.update(SPEECHY, 0.8, 10)).toBe(false);
  });
});
