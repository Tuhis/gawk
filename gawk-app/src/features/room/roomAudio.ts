// R42 (docs/44 §4.7): the room's audio mixer. One AudioContext for every
// tile, a master GainNode as the shared destination, and the one-shot worklet
// registration the tiles' AudioSinks call through (audioSink.ts AudioOutput).
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

  // The output every tile's sink is handed. Null where Web Audio is absent —
  // the tile then plays video-only exactly like a single viewer would.
  output(): AudioOutput | null {
    if (this.disposed || !audioSinkSupported()) return null;
    if (!this.ctx) {
      this.ctx = new AudioContext({ latencyHint: 'interactive' });
      this.master = this.ctx.createGain();
      this.master.gain.value = this.muted ? 0 : this.volume;
      this.master.connect(this.ctx.destination);
    }
    const ctx = this.ctx;
    return {
      context: ctx,
      destination: this.master!,
      ensureWorklet: () => {
        this.worklet ??= addWorkletModule(ctx).catch((e) => {
          // Let the next tile retry rather than latching every sink dead on
          // one transient failure.
          this.worklet = null;
          throw e;
        });
        return this.worklet;
      },
    };
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
    void ctx?.close().catch(() => {});
  }
}
