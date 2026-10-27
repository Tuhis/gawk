// When this browser transmits voice (R79, docs/80 D21, OD10). Pure and
// clock-injected: the capture path asks it once per 10 ms block of captured
// PCM, and its answer is the whole decision — nothing is encoded or sent
// while it says no, which is how a Mumble client behaves (silence costs
// nothing on the wire).
//
// Two talk modes:
//   - voice activity: while RNNoise runs (R80, docs/81 D9), a speech
//     detector over its speech probability, with hysteresis, a level floor
//     on the denoised block and a hold; otherwise (Browser, Off, a fallback;
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

// docs/81 D9: `off` follows the user's `on`; the floor and the hold are fixed.
export const SPEECH_OFF_BELOW_ON = 0.2;
export const SPEECH_FLOOR_DBFS = -50;
export const SPEECH_HOLD_MS = 300;

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

// docs/81 D9, Mumble's shape plus a level floor: a block is speech when its
// probability is >= on, or >= off while the detector is already open, AND the
// denoised block is at least `floor` loud (a quieter one counts as
// probability 0); the gate stays open `hold` after the last speech block.
// The same rule NS0 measured (docs/assets/81/harness/rnnoise.mjs probGate).
//
// Fed each output block with its own call's probability: with RNNoise 0.2 the
// probability leads its audio by 10 ms, which opens the gate early, never
// late; the floor reads the delayed block itself (docs/81 D8, §10).
export class SpeechDetector {
  private on: number;
  private off: number;
  private readonly floorRms: number;
  private readonly holdMs: number;
  private open = false;
  private lastSpeechAtMs: number | null = null;

  constructor(opts: { on?: number; floorDbfs?: number; holdMs?: number } = {}) {
    this.on = opts.on ?? DEFAULT_SPEECH_THRESHOLD;
    this.off = this.on - SPEECH_OFF_BELOW_ON;
    this.floorRms = dbfsToRms(opts.floorDbfs ?? SPEECH_FLOOR_DBFS);
    this.holdMs = opts.holdMs ?? SPEECH_HOLD_MS;
  }

  setThreshold(on: number): void {
    this.on = on;
    this.off = on - SPEECH_OFF_BELOW_ON;
  }

  // Whether this block is part of a talk spurt. `rms` is the denoised block's.
  update(prob: number, rms: number, nowMs: number): boolean {
    const p = rms < this.floorRms ? 0 : prob;
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
}

export class TalkGate {
  private mode: TalkMode;
  private pttHeld = false;
  private blocked = false;
  private readonly vad: VoiceActivityDetector;
  private readonly speech: SpeechDetector;
  private sending = false;

  constructor(
    mode: TalkMode,
    vad: VoiceActivityDetector = new VoiceActivityDetector(),
    speech: SpeechDetector = new SpeechDetector(),
  ) {
    this.mode = mode;
    this.vad = vad;
    this.speech = speech;
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
    if (this.blocked) {
      this.sending = false;
      return false;
    }
    if (this.mode === 'ptt') this.sending = this.pttHeld;
    else if (prob === null) this.sending = this.vad.update(rms, nowMs);
    else this.sending = this.speech.update(prob, rms, nowMs);
    return this.sending;
  }

  // The own speaking indicator (docs/80 D16): from this gate, never from the
  // relay.
  get transmitting(): boolean {
    return this.sending;
  }
}
