// The room's audio mixer. One AudioContext for every tile, a master GainNode
// as the shared destination, and the one-shot worklet registration the
// tiles' AudioSinks call through (audioSink.ts AudioOutput).
//
// Mixing is client-side and entirely here: each tile keeps its own sink
// (jitter buffer, drift trim, its own GainNode for the tile's level and
// mute) and feeds the master gain instead of the context's destination. The
// footer speaker drives the master; focus mode silences the non-focused
// tiles through their sinks' `setSuppressed`, so switching POV is a gain
// change, never a pipeline change.
//
// Lazy: the context is opened on the first tile that asks, from whatever
// gesture reached the room (a join click, a tile tap) — a room with no audio
// never constructs one.

import { addWorkletModule, audioSinkSupported, type AudioOutput } from '../viewer/audioSink';

export class RoomAudioMixer {
  private ctx: AudioContext | null = null;
  private master: GainNode | null = null;
  private worklet: Promise<void> | null = null;
  private volume = 1;
  private muted = false;
  private disposed = false;
  private voiceBus: GainNode | null = null;
  private voiceMuted = false;

  // The output every tile's sink is handed. Null where Web Audio is absent —
  // the tile then plays video-only exactly like a single viewer would.
  output(): AudioOutput | null {
    const ctx = this.context();
    if (!ctx) return null;
    return { context: ctx, destination: this.master!, ensureWorklet: () => this.ensureWorklet(ctx) };
  }

  // R79 (docs/80 D22): the room voice's output — the same context (one per
  // room, docs/44 §4.7), but its own bus straight to the speakers, beside the
  // streams' master rather than under it: the footer's master volume is the
  // streams', and deafen is the voice bus's mute. Each speaker's sink keeps
  // its own gain (the per-person volume) in front of the bus.
  voiceOutput(): AudioOutput | null {
    const ctx = this.context();
    if (!ctx) return null;
    if (!this.voiceBus) {
      this.voiceBus = ctx.createGain();
      this.voiceBus.gain.value = this.voiceMuted ? 0 : 1;
      this.voiceBus.connect(ctx.destination);
    }
    return { context: ctx, destination: this.voiceBus, ensureWorklet: () => this.ensureWorklet(ctx) };
  }

  setVoiceMuted(muted: boolean): void {
    this.voiceMuted = muted;
    if (this.voiceBus) this.voiceBus.gain.value = muted ? 0 : 1;
  }

  // The shared context, opened on first use (the voice capture worklet runs
  // in it too). Null where Web Audio is absent or after dispose.
  context(): AudioContext | null {
    if (this.disposed || !audioSinkSupported()) return null;
    if (!this.ctx) {
      this.ctx = new AudioContext({ latencyHint: 'interactive' });
      this.master = this.ctx.createGain();
      this.master.gain.value = this.muted ? 0 : this.volume;
      this.master.connect(this.ctx.destination);
    }
    return this.ctx;
  }

  private ensureWorklet(ctx: AudioContext): Promise<void> {
    this.worklet ??= addWorkletModule(ctx).catch((e) => {
      // Let the next tile retry rather than latching every sink dead on
      // one transient failure.
      this.worklet = null;
      throw e;
    });
    return this.worklet;
  }

  setMasterVolume(volume: number): void {
    this.volume = Math.max(0, Math.min(1, volume));
    if (this.master && !this.muted) this.master.gain.value = this.volume;
  }

  setMasterMuted(muted: boolean): void {
    this.muted = muted;
    if (this.master) this.master.gain.value = muted ? 0 : this.volume;
  }

  get masterVolume(): number {
    return this.volume;
  }

  get masterMuted(): boolean {
    return this.muted;
  }

  // The shared context is gesture-gated once, for every tile: the room's
  // single "Tap for sound" resumes it here.
  get needsGesture(): boolean {
    const state = this.ctx?.state as AudioContextState | 'interrupted' | undefined;
    return state === 'suspended' || state === 'interrupted';
  }

  async resume(): Promise<void> {
    try {
      await this.ctx?.resume();
    } catch {
      // the tiles' own resume paths report; nothing to add here
    }
  }

  dispose(): void {
    if (this.disposed) return;
    this.disposed = true;
    const ctx = this.ctx;
    this.ctx = null;
    this.master = null;
    this.voiceBus = null;
    void ctx?.close().catch(() => {});
  }
}
