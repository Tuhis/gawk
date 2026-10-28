// The talk gate (docs/80 D21, OD10): voice activity with its hangover,
// push-to-talk, and the blocks (mute, deafen, server mute) that override both;
// R80's speech detector over RNNoise's probability (docs/81 D9) and the
// configurable level threshold (D10).
import { describe, expect, it } from 'vitest';
import {
  SPEECH_FLOOR_CAP_DBFS,
  SPEECH_FLOOR_DBFS,
  SPEECH_GAIN_MAX_DB,
  SPEECH_GAIN_TARGET_DBFS,
  SPEECH_HOLD_MS,
  SPEECH_LEVEL_HALF_LIFE_S,
  SPEECH_LEVEL_MIN_BLOCKS,
  SpeechDetector,
  SpeechLevelTracker,
  marginForThreshold,
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

// R81 (docs/82 D2–D5): the floor follows the speaker's learned level.
describe('SpeechLevelTracker', () => {
  const feed = (t: SpeechLevelTracker, n: number, prob: number, db: number) => {
    for (let i = 0; i < n; i++) t.update(prob, db, true);
  };

  it('knows nothing until enough confident blocks, then gives their high quantile', () => {
    const t = new SpeechLevelTracker();
    feed(t, SPEECH_LEVEL_MIN_BLOCKS - 1, 0.99, -25);
    expect(t.level()).toBeNull();
    // The evidence ages as it arrives: ~3.5 s of speech is enough.
    feed(t, 50, 0.99, -25);
    expect(t.level()).toBe(-25);
  });

  it('learns only from confident blocks above the absolute floor, and only when eligible', () => {
    const t = new SpeechLevelTracker();
    feed(t, 1000, 0.9, -25); // not confident
    feed(t, 1000, 0.99, -60); // under -50 dBFS
    for (let i = 0; i < 1000; i++) t.update(0.99, -25, false); // muted, PTT up
    expect(t.level()).toBeNull();
  });

  it('a quieter voice-like sound under the speaker does not pull the quantile down', () => {
    const t = new SpeechLevelTracker();
    // 30 % speaker at -22, 70 % hum at -40: the 0.85 quantile is still the speaker.
    for (let i = 0; i < 1000; i++) t.update(0.99, i % 10 < 3 ? -22 : -40, true);
    expect(t.level()).toBe(-22);
  });

  it('old evidence fades with the half-life', () => {
    const t = new SpeechLevelTracker();
    feed(t, 400, 0.99, -20);
    // One half-life of blocks that teach nothing: 400 → 200, under the minimum.
    for (let i = 0; i < SPEECH_LEVEL_HALF_LIFE_S * 100; i++) t.update(0.1, -20, true);
    expect(t.level()).toBeNull();
  });

  it('a seed survives the first unconfident block (review round 1)', () => {
    const d = new SpeechDetector({ learned: -24 });
    d.update(0.1, dbfsToRms(-70), 0);
    expect(d.floorDb()).toBe(-36);
  });

  it('a seeded level counts as evidence that new speech overrides', () => {
    const t = new SpeechLevelTracker(-20);
    expect(t.level()).toBe(-20);
    feed(t, 2000, 0.99, -35);
    expect(t.level()).toBe(-35);
  });
});

describe('marginForThreshold', () => {
  it('is 20 dB at the lenient end, 12 at the default, 10 at the strict end', () => {
    expect(marginForThreshold(0.5)).toBeCloseTo(20);
    expect(marginForThreshold(0.9)).toBeCloseTo(12);
    expect(marginForThreshold(0.99)).toBeCloseTo(10);
    expect(marginForThreshold(0.7)).toBeCloseTo(16);
  });
});

describe('SpeechDetector with a learned level', () => {
  it('the floor is the learned level minus the margin, clamped to [-50, -30] dBFS', () => {
    expect(new SpeechDetector().floorDb()).toBe(SPEECH_FLOOR_DBFS);
    expect(new SpeechDetector({ learned: -24 }).floorDb()).toBe(-36);
    expect(new SpeechDetector({ learned: -45 }).floorDb()).toBe(SPEECH_FLOOR_DBFS);
    expect(new SpeechDetector({ learned: -5 }).floorDb()).toBe(SPEECH_FLOOR_CAP_DBFS);
    const d = new SpeechDetector({ learned: -24 });
    d.setThreshold(0.5);
    expect(d.floorDb()).toBe(-44);
  });

  it('a confident block under the learned floor is not speech', () => {
    const d = new SpeechDetector({ learned: -24 });
    expect(d.update(0.99, dbfsToRms(-40), 0)).toBe(false);
    expect(d.update(0.99, dbfsToRms(-30), 10)).toBe(true);
  });

  it('learns while it gates: a speaker 22 dB quieter is followed within 30 s', () => {
    const d = new SpeechDetector({ learned: -20 });
    const sent: boolean[] = [];
    for (let f = 0; f < 3000; f++) sent.push(d.update(0.99, dbfsToRms(-42), f * 10));
    expect(d.floorDb()).toBe(SPEECH_FLOOR_DBFS);
    expect(sent.slice(2000).every(Boolean)).toBe(true);
  });
});

describe('TalkGate learned level and gain (docs/82 D5, D6)', () => {
  it('exposes the floor only while RNNoise runs', () => {
    const g = new TalkGate('vad');
    g.update(dbfsToRms(-30), null, 0);
    expect(g.floorDb()).toBeNull();
    g.update(dbfsToRms(-30), 0.99, 10);
    expect(g.floorDb()).toBe(SPEECH_FLOOR_DBFS);
  });

  it('push-to-talk learns while the key is held; muted it learns nothing', () => {
    const g = new TalkGate('ptt');
    for (let f = 0; f < 400; f++) g.update(dbfsToRms(-30), 0.99, f * 10);
    expect(g.learnedLevelDb()).toBeNull();
    g.setPttHeld(true);
    for (let f = 0; f < 400; f++) g.update(dbfsToRms(-30), 0.99, f * 10);
    expect(g.learnedLevelDb()).toBe(-30);
    const m = new TalkGate('vad');
    m.setBlocked(true);
    for (let f = 0; f < 400; f++) m.update(dbfsToRms(-30), 0.99, f * 10);
    expect(m.learnedLevelDb()).toBeNull();
  });

  it('a stored level sets the gain from the first block but never the floor', () => {
    const g = new TalkGate('vad', undefined, undefined, -40);
    expect(g.gainDb()).toBe(SPEECH_GAIN_TARGET_DBFS + 40);
    g.update(dbfsToRms(-30), 0.99, 0);
    expect(g.floorDb()).toBe(SPEECH_FLOOR_DBFS);
    // Once this session has its own level, both follow it.
    for (let f = 1; f < 400; f++) g.update(dbfsToRms(-30), 0.99, f * 10);
    expect(g.learnedLevelDb()).toBe(-30);
    expect(g.gainDb()).toBe(SPEECH_GAIN_TARGET_DBFS + 30);
    expect(g.floorDb()).toBe(-42);
  });

  it('the gain keeps the last level through a long silence', () => {
    const g = new TalkGate('vad');
    for (let f = 0; f < 400; f++) g.update(dbfsToRms(-30), 0.99, f * 10);
    for (let f = 400; f < 40000; f++) g.update(dbfsToRms(-70), 0.01, f * 10);
    expect(g.learnedLevelDb()).toBeNull();
    expect(g.floorDb()).toBe(SPEECH_FLOOR_DBFS);
    expect(g.gainDb()).toBe(SPEECH_GAIN_TARGET_DBFS + 30);
  });

  it('the gain lifts the learned level to the target, never cuts, and stops at +20 dB', () => {
    expect(new TalkGate('vad').gainDb()).toBe(0);
    expect(new TalkGate('vad', undefined, undefined, -30).gainDb()).toBe(SPEECH_GAIN_TARGET_DBFS + 30);
    expect(new TalkGate('vad', undefined, undefined, -10).gainDb()).toBe(0);
    expect(new TalkGate('vad', undefined, undefined, -60).gainDb()).toBe(SPEECH_GAIN_MAX_DB);
  });

  it('forgetting the level returns to the fixed floor and no gain', () => {
    const g = new TalkGate('vad', undefined, undefined, -24);
    g.forgetLevel();
    expect(g.learnedLevelDb()).toBeNull();
    expect(g.gainDb()).toBe(0);
  });
});
