// When this browser transmits voice (R79, docs/80 D21, OD10). Pure and
// clock-injected: the capture path asks it once per 10 ms block of captured
// PCM, and its answer is the whole decision — nothing is encoded or sent
// while it says no, which is how a Mumble client behaves (silence costs
// nothing on the wire).
//
// Two talk modes:
//   - voice activity: while RNNoise runs (R80, docs/81 D9), a speech
//     detector over its speech probability, with hysteresis, a level floor
//     on the denoised block and a hold — the floor following the speaker's
//     own learned level (R81, docs/82 D2–D4); otherwise (Browser, Off, a fallback;
//     D10) an energy threshold on the block's RMS. Either is held for a
//     hangover after the last speech block so a pause between words does
//     not chop the spurt (and the trailing consonant is not cut);
//   - push-to-talk: exactly while the key (or the hold button) is held. The
//     audio is still denoised (D18).
// Either way a block is never sent while the user is muted, deafened (Mumble
// deafen implies mute), server-muted or suppressed.

import { DEFAULT_LEVEL_THRESHOLD_DB, DEFAULT_SPEECH_THRESHOLD, type TalkMode } from './voicePrefs';

// -40 dBFS. With the browser's noise suppression and auto gain on, room
// noise sits well below it and speech well above it.
export const VAD_THRESHOLD_RMS = dbfsToRms(DEFAULT_LEVEL_THRESHOLD_DB);
// How long a spurt outlives its last loud block.
export const VAD_HANGOVER_MS = 300;

// docs/81 D9: `off` follows the user's `on`; the hold is fixed. The floor
// is at least SPEECH_FLOOR_DBFS (D9's, used until a level is learned) and at
// most SPEECH_FLOOR_CAP_DBFS (docs/82 D3).
export const SPEECH_OFF_BELOW_ON = 0.2;
export const SPEECH_FLOOR_DBFS = -50;
export const SPEECH_FLOOR_CAP_DBFS = -30;
export const SPEECH_HOLD_MS = 300;

// docs/82 D2, VX0 §12: the speaker's level is the 0.85 quantile of the
// denoised level of blocks RNNoise is confident are speech, over evidence
// that halves every 15 s, known once 300 blocks of it are in hand.
export const SPEECH_LEVEL_QUANTILE = 0.85;
export const SPEECH_LEVEL_HALF_LIFE_S = 15;
export const SPEECH_LEVEL_MIN_BLOCKS = 300;
export const SPEECH_LEVEL_CONFIDENT = 0.95;
// docs/82 D6: the gain after the gate brings the learned level here, never
// cuts, and never adds more than +20 dB.
export const SPEECH_GAIN_TARGET_DBFS = -20;
export const SPEECH_GAIN_MAX_DB = 20;

const BLOCK_MS = 10;
const LEVEL_MIN_DB = -80;
const LEVEL_BINS = 81; // 1 dB bins, -80..0 dBFS

export function dbfsToRms(db: number): number {
  return 10 ** (db / 20);
}

// RMS → dBFS, with silence pinned at -100 rather than -Infinity.
export function rmsToDbfs(rms: number): number {
  return rms > 1e-5 ? 20 * Math.log10(rms) : -100;
}

// Root mean square of one block of PCM.
export function blockRms(pcm: Float32Array): number {
  if (pcm.length === 0) return 0;
  let sum = 0;
  for (let i = 0; i < pcm.length; i++) sum += pcm[i] * pcm[i];
  return Math.sqrt(sum / pcm.length);
}

export class VoiceActivityDetector {
  private threshold: number;
  private readonly hangoverMs: number;
  private lastLoudAtMs: number | null = null;

  constructor(opts: { thresholdRms?: number; hangoverMs?: number } = {}) {
    this.threshold = opts.thresholdRms ?? VAD_THRESHOLD_RMS;
    this.hangoverMs = opts.hangoverMs ?? VAD_HANGOVER_MS;
  }

  setThresholdRms(rms: number): void {
    this.threshold = rms;
  }

  // Whether this block is part of a talk spurt.
  update(rms: number, nowMs: number): boolean {
    if (rms >= this.threshold) {
      this.lastLoudAtMs = nowMs;
      return true;
    }
    return this.lastLoudAtMs !== null && nowMs - this.lastLoudAtMs < this.hangoverMs;
  }

  reset(): void {
    this.lastLoudAtMs = null;
  }
}

// docs/82 D2: what the speaker sounds like after RNNoise. Every confident
// block teaches it, sent or not (learning only from sent blocks would let
// the floor ratchet itself up); a quieter voice-like sound under the speaker
// (a hum, a TV) does not move a high quantile. The port of
// docs/assets/82/harness/gates.mjs LevelTracker: keep the two in step.
export class SpeechLevelTracker {
  private readonly bins = new Float64Array(LEVEL_BINS);
  private mass = 0;
  private readonly decay = 2 ** (-(BLOCK_MS / 1000) / SPEECH_LEVEL_HALF_LIFE_S);

  // `seed`: a level already known, as SPEECH_LEVEL_MIN_BLOCKS of evidence.
  constructor(seed: number | null = null) {
    if (seed !== null) this.add(seed, SPEECH_LEVEL_MIN_BLOCKS);
  }

  private add(db: number, w: number): void {
    const i = Math.max(0, Math.min(LEVEL_BINS - 1, Math.round(db) - LEVEL_MIN_DB));
    this.bins[i] += w;
    this.mass += w;
  }

  // One 10 ms block: the evidence ages, and a confident block above the
  // absolute floor adds to it when the speaker could be talking (`eligible`:
  // not blocked, and in push-to-talk only while the key is held).
  update(prob: number, levelDb: number, eligible: boolean): void {
    for (let i = 0; i < LEVEL_BINS; i++) this.bins[i] *= this.decay;
    this.mass *= this.decay;
    if (eligible && prob >= SPEECH_LEVEL_CONFIDENT && levelDb >= SPEECH_FLOOR_DBFS) this.add(levelDb, 1);
  }

  // The learned level, dBFS, or null while there is too little evidence.
  level(): number | null {
    if (this.mass < SPEECH_LEVEL_MIN_BLOCKS) return null;
    const target = SPEECH_LEVEL_QUANTILE * this.mass;
    let acc = 0;
    for (let i = 0; i < LEVEL_BINS; i++) {
      acc += this.bins[i];
      if (acc >= target) return i + LEVEL_MIN_DB;
    }
    return LEVEL_MIN_DB + LEVEL_BINS - 1;
  }

  reset(): void {
    this.bins.fill(0);
    this.mass = 0;
  }
}

// docs/82 D4: how far under the learned level the floor sits, from the one
// slider: 20 dB at the lenient end (0.50), 12 at the default (0.90), 10 at
// the strict end (0.99), linear between.
export function marginForThreshold(on: number): number {
  if (on <= 0.9) return 20 + ((Math.max(0.5, on) - 0.5) / 0.4) * (12 - 20);
  return 12 + ((Math.min(0.99, on) - 0.9) / 0.09) * (10 - 12);
}

// docs/81 D9, Mumble's shape plus a level floor: a block is speech when its
// probability is >= on, or >= off while the detector is already open, AND the
// denoised block is at least `floor` loud (a quieter one counts as
// probability 0); the gate stays open `hold` after the last speech block.
// The same rule NS0 measured (docs/assets/81/harness/rnnoise.mjs probGate).
//
// Fed each output block with its own call's probability: with RNNoise 0.2 the
// probability leads its audio by 10 ms, which opens the gate early, never
// late; the floor reads the delayed block itself (docs/81 D8, §10).
//
// R81 (docs/82 D3): the floor is the learned level minus the slider's
// margin, clamped to [SPEECH_FLOOR_DBFS, SPEECH_FLOOR_CAP_DBFS]; D9's fixed
// -50 dBFS until a level is learned. `learned` seeds the tracker (tests).
export class SpeechDetector {
  private on: number;
  private off: number;
  private readonly holdMs: number;
  private readonly tracker: SpeechLevelTracker;
  private open = false;
  private lastSpeechAtMs: number | null = null;

  constructor(opts: { on?: number; holdMs?: number; learned?: number } = {}) {
    this.on = opts.on ?? DEFAULT_SPEECH_THRESHOLD;
    this.off = this.on - SPEECH_OFF_BELOW_ON;
    this.holdMs = opts.holdMs ?? SPEECH_HOLD_MS;
    this.tracker = new SpeechLevelTracker(opts.learned ?? null);
  }

  setThreshold(on: number): void {
    this.on = on;
    this.off = on - SPEECH_OFF_BELOW_ON;
  }

  // The floor the next block must reach, dBFS.
  floorDb(): number {
    const learned = this.tracker.level();
    if (learned === null) return SPEECH_FLOOR_DBFS;
    return Math.min(SPEECH_FLOOR_CAP_DBFS, Math.max(SPEECH_FLOOR_DBFS, learned - marginForThreshold(this.on)));
  }

  learnedLevelDb(): number | null {
    return this.tracker.level();
  }

  // A block the detector does not judge (push-to-talk, a blocked gate):
  // the evidence still ages, and learns when `eligible`.
  observe(prob: number, rms: number, eligible: boolean): void {
    this.tracker.update(prob, rmsToDbfs(rms), eligible);
  }

  // Whether this block is part of a talk spurt. `rms` is the denoised block's.
  update(prob: number, rms: number, nowMs: number): boolean {
    const levelDb = rmsToDbfs(rms);
    const p = levelDb < this.floorDb() ? 0 : prob;
    this.tracker.update(prob, levelDb, true);
    const speech = p >= this.on || (this.open && p >= this.off);
    this.open = speech;
    if (speech) {
      this.lastSpeechAtMs = nowMs;
      return true;
    }
    return this.lastSpeechAtMs !== null && nowMs - this.lastSpeechAtMs < this.holdMs;
  }

  reset(): void {
    this.open = false;
    this.lastSpeechAtMs = null;
  }

  forgetLevel(): void {
    this.tracker.reset();
  }
}

export class TalkGate {
  private mode: TalkMode;
  private pttHeld = false;
  private blocked = false;
  private readonly vad: VoiceActivityDetector;
  private readonly speech: SpeechDetector;
  private sending = false;
  private rnnoise = false;
  // docs/82 D6: the level the gain works from — this session's learned
  // level, else the last one it had, else the one stored from an earlier
  // session (`storedLevelDb`). Never the floor's: that waits for evidence.
  private gainLevelDb: number | null;

  constructor(
    mode: TalkMode,
    vad: VoiceActivityDetector = new VoiceActivityDetector(),
    speech: SpeechDetector = new SpeechDetector(),
    storedLevelDb: number | null = null,
  ) {
    this.mode = mode;
    this.vad = vad;
    this.speech = speech;
    this.gainLevelDb = storedLevelDb;
  }

  setMode(mode: TalkMode): void {
    if (mode === this.mode) return;
    this.mode = mode;
    this.pttHeld = false;
    this.vad.reset();
    this.speech.reset();
  }

  // The user's thresholds (docs/81 D9, D10), applied from the next block.
  setSpeechThreshold(on: number): void {
    this.speech.setThreshold(on);
  }

  setLevelThresholdDb(db: number): void {
    this.vad.setThresholdRms(dbfsToRms(db));
  }

  getMode(): TalkMode {
    return this.mode;
  }

  // The push-to-talk key or the hold button.
  setPttHeld(held: boolean): void {
    this.pttHeld = held;
  }

  // Muted, deafened, server-muted, suppressed, or not connected: nothing goes
  // out whatever the mode says. Unblocking does not resume a spurt by
  // itself — the next block decides.
  setBlocked(blocked: boolean): void {
    this.blocked = blocked;
    if (blocked) {
      this.vad.reset();
      this.speech.reset();
    }
  }

  // The decision for one block, and the gate's state until the next. `prob`
  // is RNNoise's speech probability for the block, or null when RNNoise is
  // not running (the level gate decides then).
  update(rms: number, prob: number | null, nowMs: number): boolean {
    this.rnnoise = prob !== null;
    if (this.blocked) {
      if (prob !== null) this.speech.observe(prob, rms, false);
      this.sending = false;
      return false;
    }
    if (this.mode === 'ptt') {
      if (prob !== null) this.speech.observe(prob, rms, this.pttHeld);
      this.sending = this.pttHeld;
    } else if (prob === null) this.sending = this.vad.update(rms, nowMs);
    else this.sending = this.speech.update(prob, rms, nowMs);
    const learned = this.speech.learnedLevelDb();
    if (learned !== null) this.gainLevelDb = learned;
    return this.sending;
  }

  // The level floor the last block met while RNNoise ran (the meter's grey,
  // docs/82 D7), null without RNNoise.
  floorDb(): number | null {
    return this.rnnoise ? this.speech.floorDb() : null;
  }

  // This session's learned level (what to store for the next one).
  learnedLevelDb(): number | null {
    return this.speech.learnedLevelDb();
  }

  // docs/82 D6: the gain to apply to sent blocks, dB.
  gainDb(): number {
    if (this.gainLevelDb === null) return 0;
    return Math.min(SPEECH_GAIN_MAX_DB, Math.max(0, SPEECH_GAIN_TARGET_DBFS - this.gainLevelDb));
  }

  // Reset to defaults: no learned or stored level any more.
  forgetLevel(): void {
    this.speech.forgetLevel();
    this.gainLevelDb = null;
  }

  // The own speaking indicator (docs/80 D16): from this gate, never from the
  // relay.
  get transmitting(): boolean {
    return this.sending;
  }
}
