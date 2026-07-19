// R15 (docs/20 Decision 8): the viewer's audio jitter buffer — live-edge
// discipline for the third medium. Pure and clock-injected: every policy
// decision (gap → silence, late → drop, overflow → drop oldest, underrun →
// silence, restart → flush + re-anchor) is unit-testable without an
// AudioContext.
//
// Division of labor with the AudioWorklet sink: this class owns *which*
// PCM goes to the speaker and in what order; the worklet is a dumb FIFO
// that drains 128-frame quanta and reports its playhead back (underruns are
// counted there and folded in here via noteUnderrun).

// Decision 10: the adaptive target envelope. Default profile is small — the
// live-edge philosophy does not bend for audio; the resilient profile
// (docs/24 + docs/20 Decision 12) widens it so audio-master pacing works at
// resilient depth instead of collapsing the video buffer to ~150 ms.
export interface AudioBufferProfile {
  minMs: number;
  maxMs: number;
  seedMs: number;
}

export const DEFAULT_AUDIO_PROFILE: AudioBufferProfile = { minMs: 40, maxMs: 150, seedMs: 60 };
export const RESILIENT_AUDIO_PROFILE: AudioBufferProfile = { minMs: 150, maxMs: 2000, seedMs: 500 };

// Headroom over measured jitter, mirroring the R12 PlayoutController shape.
const HEADROOM_MS = 20;
// Slew limits (ms of target change per second): grow fast to avoid dropouts,
// shrink slowly so a single quiet moment doesn't collapse the buffer.
const SLEW_UP_MS_PER_S = 50;
const SLEW_DOWN_MS_PER_S = 5;
// Overflow ceiling: anything beyond target + this is backlog, not jitter.
const OVERFLOW_SLACK_MS = 200;
// Timeline-change thresholds, deliberately asymmetric — the same lesson the
// video path learned in R10 (docs/14 finding 5): a *serially backwards* jump
// is the restart signal, and treating it as lateness strands the viewer.
// Backwards: no straggler is a full second behind the feed position (arrival
// jitter is tens of ms; the buffer's depth doesn't make packets arrive late),
// so anything beyond this is a new timeline, not a late packet.
const BACKWARDS_RESTART_MS = 1000;
// Forwards: a hole this large is a reconnect/stall, not something to conceal
// with synthesized silence.
const FORWARD_RESTART_MS = 5000;

export interface AudioChunk {
  // Broadcaster-clock µs (docs/20 Decision 3) — the same clock video frames
  // carry, which is what makes A/V skew a subtraction.
  timestampUs: number;
  // Planar PCM, one Float32Array per channel.
  channels: Float32Array[];
  sampleRate: number;
  // Frames (samples per channel) in this chunk.
  frameCount: number;
}

export interface AudioBufferStats {
  gapsConcealed: number;
  lateDrops: number;
  overflowDrops: number;
  underruns: number;
  bufferedMs: number;
  targetMs: number;
  // Wall-clock ms the buffer has been anchored (diagnostic for re-anchors).
  resets: number;
}

export type AudioChunkSink = (chunk: AudioChunk) => void;

export class AudioJitterBuffer {
  private profile: () => AudioBufferProfile;
  private emit: AudioChunkSink;
  private targetMs: number;
  private lastSlewAtMs: number | null = null;
  // Identity of the profile the current target was learned under. A resilient
  // flip must re-seed on the incoming profile rather than carry a value from
  // the other one's envelope (docs/24 Decision 9's rule, applied to audio) —
  // and the two envelopes overlap at 150 ms, so a range check alone misses it.
  private profileSeedMs: number;

  // The next expected timestamp (µs) — how gaps are detected. Null before
  // the first accepted chunk and after every flush.
  private nextExpectedUs: number | null = null;
  // Frames handed to the sink but not yet played, in ms (the sink reports
  // its drain back through notePlayed).
  private queuedMs = 0;

  private stats = {
    gapsConcealed: 0,
    lateDrops: 0,
    overflowDrops: 0,
    underruns: 0,
    resets: 0,
  };

  constructor(
    emit: AudioChunkSink,
    profile: AudioBufferProfile | (() => AudioBufferProfile) = DEFAULT_AUDIO_PROFILE,
  ) {
    this.emit = emit;
    this.profile = typeof profile === 'function' ? profile : () => profile;
    this.targetMs = this.profile().seedMs;
    this.profileSeedMs = this.targetMs;
  }

  // Feeds one decoded chunk. Returns what the buffer did with it — the
  // caller needs nothing from this, but the tests read it as the decision.
  push(chunk: AudioChunk): 'accepted' | 'late' | 'overflow' | 'gap-filled' {
    const durationMs = (chunk.frameCount / chunk.sampleRate) * 1000;

    // Overflow: the sink is already holding more than the target plus slack.
    // Drop the *incoming* chunk — dropping the oldest would mean clawing
    // back audio already handed to the worklet, and the newest data is what
    // keeps us at the live edge.
    if (this.queuedMs > this.targetMs + OVERFLOW_SLACK_MS) {
      this.stats.overflowDrops++;
      return 'overflow';
    }

    if (this.nextExpectedUs === null) {
      // First chunk (or first after a flush): anchor here.
      this.nextExpectedUs = chunk.timestampUs + durationMs * 1000;
      this.emitChunk(chunk, durationMs);
      return 'accepted';
    }

    const deltaUs = chunk.timestampUs - this.nextExpectedUs;

    // A big jump in either direction is a new timeline, not a gap: re-anchor
    // rather than synthesizing seconds of silence or dropping forever
    // (docs/20 Decision 8, restart/reconnect policy).
    if (deltaUs < -BACKWARDS_RESTART_MS * 1000 || deltaUs > FORWARD_RESTART_MS * 1000) {
      this.flush();
      this.nextExpectedUs = chunk.timestampUs + durationMs * 1000;
      this.emitChunk(chunk, durationMs);
      return 'accepted';
    }

    // Late: this packet belongs before the playhead — delay must never grow
    // to accommodate stragglers (the third medium, same rule as video).
    if (deltaUs < -durationMs * 1000) {
      this.stats.lateDrops++;
      return 'late';
    }

    // Gap: packets went missing. Conceal with exactly the missing duration
    // of silence so the media clock keeps its meaning, then play the chunk.
    if (deltaUs > 0) {
      const gapMs = deltaUs / 1000;
      this.stats.gapsConcealed++;
      this.emitSilence(gapMs, chunk.sampleRate, chunk.channels.length);
      this.nextExpectedUs = chunk.timestampUs + durationMs * 1000;
      this.emitChunk(chunk, durationMs);
      return 'gap-filled';
    }

    this.nextExpectedUs = chunk.timestampUs + durationMs * 1000;
    this.emitChunk(chunk, durationMs);
    return 'accepted';
  }

  private emitChunk(chunk: AudioChunk, durationMs: number): void {
    this.queuedMs += durationMs;
    this.emit(chunk);
  }

  private emitSilence(gapMs: number, sampleRate: number, channelCount: number): void {
    const frameCount = Math.max(1, Math.round((gapMs / 1000) * sampleRate));
    const channels: Float32Array[] = [];
    for (let i = 0; i < channelCount; i++) channels.push(new Float32Array(frameCount));
    this.queuedMs += gapMs;
    this.emit({
      timestampUs: this.nextExpectedUs ?? 0,
      channels,
      sampleRate,
      frameCount,
    });
  }

  // The sink drained this much audio (ms). Keeps bufferedMs honest.
  notePlayed(ms: number): void {
    this.queuedMs = Math.max(0, this.queuedMs - ms);
  }

  // The sink ran dry and emitted silence itself.
  noteUnderrun(count = 1): void {
    this.stats.underruns += count;
  }

  // Drops everything pending and forgets the timeline: a broadcaster restart
  // or a viewer reconnect (docs/20 Decision 8). Without this, every packet on
  // the new timeline reads as "older than the playhead" and is late-dropped
  // forever.
  flush(): void {
    this.nextExpectedUs = null;
    this.queuedMs = 0;
    this.stats.resets++;
    const p = this.profile();
    this.targetMs = p.seedMs;
    this.profileSeedMs = p.seedMs;
    this.lastSlewAtMs = null;
  }

  // R15 Decision 10 + Decision 12: the adaptive target, slew-limited inside
  // the active profile's clamp. jitterMs is the same windowed p95−min the
  // video path measures; null leaves the target where it is.
  updateTarget(jitterMs: number | null, nowMs: number): void {
    const p = this.profile();
    // Re-seed when the profile itself changed under us (resilient flip):
    // keyed on the profile's identity, not the current value's range — the
    // two envelopes overlap, so a clamp check would silently keep the old
    // profile's target.
    if (p.seedMs !== this.profileSeedMs) {
      this.profileSeedMs = p.seedMs;
      this.targetMs = p.seedMs;
      this.lastSlewAtMs = null;
    }
    if (jitterMs === null) return;
    const desired = Math.min(p.maxMs, Math.max(p.minMs, jitterMs + HEADROOM_MS));
    if (this.lastSlewAtMs === null) {
      this.lastSlewAtMs = nowMs;
      this.targetMs = desired;
      return;
    }
    const dt = Math.max(0, (nowMs - this.lastSlewAtMs) / 1000);
    this.lastSlewAtMs = nowMs;
    if (desired > this.targetMs) {
      this.targetMs = Math.min(desired, this.targetMs + SLEW_UP_MS_PER_S * dt);
    } else {
      this.targetMs = Math.max(desired, this.targetMs - SLEW_DOWN_MS_PER_S * dt);
    }
  }

  getStats(): AudioBufferStats {
    return {
      ...this.stats,
      bufferedMs: this.queuedMs,
      targetMs: this.targetMs,
    };
  }
}
