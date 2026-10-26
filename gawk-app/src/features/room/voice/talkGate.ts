// When this browser transmits voice (R79, docs/80 D21, OD10). Pure and
// clock-injected: the capture path asks it once per 10 ms block of captured
// PCM, and its answer is the whole decision — nothing is encoded or sent
// while it says no, which is how a Mumble client behaves (silence costs
// nothing on the wire).
//
// Two talk modes:
//   - voice activity: an energy threshold on the block's RMS, held for a
//     hangover after the last loud block so a pause between words does not
//     chop the spurt (and the trailing consonant is not cut);
//   - push-to-talk: exactly while the key (or the hold button) is held.
// Either way a block is never sent while the user is muted, deafened (Mumble
// deafen implies mute), server-muted or suppressed.

import type { TalkMode } from './voicePrefs';

// ~-40 dBFS. With the browser's noise suppression and auto gain on, room
// noise sits well below it and speech well above it.
export const VAD_THRESHOLD_RMS = 0.01;
// How long a spurt outlives its last loud block.
export const VAD_HANGOVER_MS = 300;

// Root mean square of one block of PCM.
export function blockRms(pcm: Float32Array): number {
  if (pcm.length === 0) return 0;
  let sum = 0;
  for (let i = 0; i < pcm.length; i++) sum += pcm[i] * pcm[i];
  return Math.sqrt(sum / pcm.length);
}

export class VoiceActivityDetector {
  private readonly threshold: number;
  private readonly hangoverMs: number;
  private lastLoudAtMs: number | null = null;

  constructor(opts: { thresholdRms?: number; hangoverMs?: number } = {}) {
    this.threshold = opts.thresholdRms ?? VAD_THRESHOLD_RMS;
    this.hangoverMs = opts.hangoverMs ?? VAD_HANGOVER_MS;
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

export class TalkGate {
  private mode: TalkMode;
  private pttHeld = false;
  private blocked = false;
  private readonly vad: VoiceActivityDetector;
  private sending = false;

  constructor(mode: TalkMode, vad: VoiceActivityDetector = new VoiceActivityDetector()) {
    this.mode = mode;
    this.vad = vad;
  }

  setMode(mode: TalkMode): void {
    if (mode === this.mode) return;
    this.mode = mode;
    this.pttHeld = false;
    this.vad.reset();
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
    if (blocked) this.vad.reset();
  }

  // The decision for one block, and the gate's state until the next.
  update(rms: number, nowMs: number): boolean {
    if (this.blocked) {
      this.sending = false;
      return false;
    }
    this.sending = this.mode === 'ptt' ? this.pttHeld : this.vad.update(rms, nowMs);
    return this.sending;
  }

  // The own speaking indicator (docs/80 D16): from this gate, never from the
  // relay.
  get transmitting(): boolean {
    return this.sending;
  }
}
