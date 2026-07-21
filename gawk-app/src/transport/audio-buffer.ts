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
  // How long audio was held to line up with the video schedule (docs/20
  // field finding 4). Null until playback starts; the number to look at when
  // lip sync is off.
  alignmentHoldMs: number | null;
  // Wall-clock ms the buffer has been anchored (diagnostic for re-anchors).
  resets: number;
}

// Returns whether the chunk actually reached the sink. `false` means the sink
// dropped it (the AudioWorklet node was still booting, or its port threw), and
// the buffer must NOT count it toward depth — counting undelivered audio
// inflates the estimate above the overflow ceiling forever, which is the
// field-finding-7 crackle-then-silence. `void` is treated as delivered so
// simpler sinks (the tests) need not care.
export type AudioChunkSink = (chunk: AudioChunk) => boolean | void;

// Whether the sink can receive a chunk right now. The buffer holds audio in
// priming until this is true, so the alignment cushion is never released into
// a null worklet node and lost (field finding 7). Defaults to always-ready.
export type SinkReadyFn = () => boolean;

// Maps a broadcaster audio timestamp (µs) to the local ms at which that chunk
// should be handed to the sink so it is *heard* alongside the video frame of
// the same timestamp — i.e. the video presentation schedule, less whatever
// latency the sink itself adds. Null while the video baseline is unknown.
export type AudioScheduleFn = (timestampUs: number) => number | null;

// A schedule that never fires would hold audio forever; release anyway past
// this. Generous — it only exists so a broken/absent schedule degrades to
// "audio plays, badly aligned" instead of "audio never plays".
export const MAX_ALIGNMENT_HOLD_MS = 3000;

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
  // Field findings 3 + 4 (docs/20): chunks accumulate here until playback is
  // due, then release in order. Two ways to become due, in priority order:
  //
  //   1. The video presentation schedule says so — the alignment decision,
  //      and the only one that produces lip sync (finding 4). Audio arrives
  //      earlier than video, so this normally holds a few hundred ms, which
  //      then *is* the sink's queue depth for the rest of the session.
  //   2. No schedule available (video baseline not yet established, or a
  //      pipeline that never presents video): fall back to a depth floor of
  //      targetMs, so audio still plays with a cushion rather than at the
  //      ~0 ms depth of finding 3.
  //
  // Re-armed on flush and on a still-dry underrun report.
  private priming = true;
  private pending: AudioChunk[] = [];
  private pendingMs = 0;
  // Local ms at which the oldest pending chunk should reach the sink, per the
  // video schedule. Null while no schedule is known.
  private dueAtMs: number | null = null;

  private stats = {
    gapsConcealed: 0,
    lateDrops: 0,
    overflowDrops: 0,
    underruns: 0,
    resets: 0,
  };

  private now: () => number;
  private schedule: () => AudioScheduleFn | null;
  private ready: SinkReadyFn;
  // True while the pending release is the *alignment* one (start, or after a
  // flush). An underrun re-prime clears it: by then the schedule for the
  // oldest pending chunk is already past — that's why we ran dry — so honoring
  // it would release instantly and rebuild no cushion at all. Depth floor
  // there instead, and let the rate trim walk the residual skew back out.
  private alignOnSchedule = true;
  // The hold actually applied at the alignment release, for the overlay.
  private alignmentHoldMs: number | null = null;
  // Depth established at release; the overflow ceiling rides this rather than
  // the jitter target, since a deliberate alignment hold is legitimately far
  // deeper than the target and must not read as backlog.
  private establishedDepthMs = 0;

  constructor(
    emit: AudioChunkSink,
    profile: AudioBufferProfile | (() => AudioBufferProfile) = DEFAULT_AUDIO_PROFILE,
    opts: {
      now?: () => number;
      schedule?: () => AudioScheduleFn | null;
      sinkReady?: SinkReadyFn;
    } = {},
  ) {
    this.emit = emit;
    this.profile = typeof profile === 'function' ? profile : () => profile;
    this.targetMs = this.profile().seedMs;
    this.profileSeedMs = this.targetMs;
    this.now = opts.now ?? (() => performance.now());
    this.schedule = opts.schedule ?? (() => null);
    this.ready = opts.sinkReady ?? (() => true);
  }

  // Lets the owner re-check the alignment gate when no chunk arrived to drive
  // it (chunks come at 50/s, so this is a backstop, not the main path).
  tick(): void {
    this.maybeRelease();
  }

  // Feeds one decoded chunk. Returns what the buffer did with it — the
  // caller needs nothing from this, but the tests read it as the decision.
  push(chunk: AudioChunk): 'accepted' | 'late' | 'overflow' | 'gap-filled' {
    const durationMs = (chunk.frameCount / chunk.sampleRate) * 1000;

    // Overflow: the sink is already holding more than the target plus slack.
    // Drop the *incoming* chunk — dropping the oldest would mean clawing
    // back audio already handed to the worklet, and the newest data is what
    // keeps us at the live edge.
    // While aligning, the hold is *supposed* to be deep — deeper than any
    // jitter target, since it spans audio's whole lead over video. Charging it
    // against the steady-state ceiling drops the very audio being lined up,
    // and (once the drops keep pendingMs under the cap) audio never starts at
    // all. Once playing, the ceiling rides the depth alignment established.
    const ceilingMs = this.priming
      ? MAX_ALIGNMENT_HOLD_MS
      : Math.max(this.targetMs, this.establishedDepthMs) + OVERFLOW_SLACK_MS;
    if (this.bufferedMs() > ceilingMs) {
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

  // The one exit toward the sink. While priming, chunks (silence included, so
  // the timeline stays honest across a gap) queue up here and leave in order
  // once the alignment gate opens.
  private emitChunk(chunk: AudioChunk, durationMs: number): void {
    if (!this.priming) {
      // Count toward depth only what the sink actually took (field finding 7):
      // an undelivered chunk (node booting, port throwing) that still inflated
      // queuedMs is what drove the chronic spurious overflow drops.
      if (this.emit(chunk) !== false) this.queuedMs += durationMs;
      return;
    }
    this.pending.push(chunk);
    this.pendingMs += durationMs;
    this.maybeRelease();
  }

  // The alignment decision, and the only moment audio's position relative to
  // video is chosen: after this the worklet runs at 1× and the offset is
  // fixed (drift aside — see av-sync's AudioRateController).
  private maybeRelease(): void {
    if (!this.priming || this.pending.length === 0) return;
    const oldest = this.pending[0]!;
    if (this.alignOnSchedule && this.dueAtMs === null) {
      this.dueAtMs = this.schedule()?.(oldest.timestampUs) ?? null;
    }
    const nowMs = this.now();
    // The video schedule decides *lip sync* — when audio is heard relative to
    // its frame. But in live-edge mode the frame is presented on arrival, so
    // the schedule says "due now" at ~0 hold, which leaves the worklet no
    // cushion and lets normal arrival jitter starve it into constant underrun
    // (docs/20 field finding 6: near-silent live-edge audio, worse the higher
    // the arrival jitter). So the adaptive jitter target is a *floor* in every
    // mode: never release below it. In paced modes the schedule hold already
    // exceeds the floor, so gating on both changes nothing there.
    const scheduleDue =
      this.alignOnSchedule && this.dueAtMs !== null ? nowMs >= this.dueAtMs : true;
    const depthReady = this.bufferedMs() >= this.targetMs;
    // Field finding 7: never release the cushion into a sink that can't receive
    // it — the released chunks would be dropped and the worklet would start at
    // ~0 ms depth (finding 6 redux). Hold until the worklet node exists.
    const due = scheduleDue && depthReady && this.ready();
    // The cap keeps a missing or nonsensical schedule (or a sink that never
    // comes up) from muting audio: release anyway, and honest accounting below
    // simply counts nothing if the sink still drops it.
    if (!due && this.pendingMs < MAX_ALIGNMENT_HOLD_MS) return;

    this.alignmentHoldMs = this.pendingMs;
    this.priming = false;
    this.dueAtMs = null;
    this.pendingMs = 0;
    const ready = this.pending;
    this.pending = [];
    // Count only what the sink accepted (field finding 7). If the node is up
    // (the ready() path) that is all of it, identical to before; only the
    // MAX_ALIGNMENT_HOLD escape can hit an unready sink, and there the depth
    // must stay honest rather than bake in a phantom cushion.
    let delivered = 0;
    for (const c of ready) {
      if (this.emit(c) !== false) delivered += (c.frameCount / c.sampleRate) * 1000;
    }
    this.queuedMs += delivered;
    this.establishedDepthMs = this.queuedMs;
  }

  private bufferedMs(): number {
    return this.queuedMs + this.pendingMs;
  }

  private emitSilence(gapMs: number, sampleRate: number, channelCount: number): void {
    const frameCount = Math.max(1, Math.round((gapMs / 1000) * sampleRate));
    const channels: Float32Array[] = [];
    for (let i = 0; i < channelCount; i++) channels.push(new Float32Array(frameCount));
    // Through emitChunk, never straight to the sink: concealment silence that
    // skips the priming gate arrives *before* the audio it was filling behind.
    this.emitChunk(
      { timestampUs: this.nextExpectedUs ?? 0, channels, sampleRate, frameCount },
      gapMs,
    );
  }

  // The sink drained this much audio (ms). Keeps bufferedMs honest.
  notePlayed(ms: number): void {
    this.queuedMs = Math.max(0, this.queuedMs - ms);
  }

  // The sink ran dry and emitted silence itself. Rebuild the cushion before
  // playing on — at zero depth the next blip underruns too, and the next,
  // which is exactly the "constant breaks" of field finding 3.
  //
  // Only when it is *still* dry, though: reports arrive on the sink's ~4 Hz
  // cadence (notePlayed lands first, from the same report), so a buffer that
  // already recovered would otherwise be sent back to priming — paying a
  // second silence for a gap that had closed.
  noteUnderrun(count = 1): void {
    this.stats.underruns += count;
    if (this.queuedMs > 0) return;
    this.queuedMs = 0;
    this.establishedDepthMs = 0;
    this.priming = true;
    this.dueAtMs = null;
    // Alignment is already lost (the sink played silence): rebuild depth, and
    // leave the residual skew to the rate trim rather than re-deciding the
    // alignment against a schedule that has already gone past.
    this.alignOnSchedule = false;
  }

  // Drops everything pending and forgets the timeline: a broadcaster restart
  // or a viewer reconnect (docs/20 Decision 8). Without this, every packet on
  // the new timeline reads as "older than the playhead" and is late-dropped
  // forever.
  flush(): void {
    this.nextExpectedUs = null;
    this.queuedMs = 0;
    this.pending = [];
    this.pendingMs = 0;
    this.priming = true;
    // A fresh timeline gets a fresh alignment decision — the one case where
    // re-deciding against the video schedule is exactly right.
    this.alignOnSchedule = true;
    this.dueAtMs = null;
    this.alignmentHoldMs = null;
    this.establishedDepthMs = 0;
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
      bufferedMs: this.bufferedMs(),
      targetMs: this.targetMs,
      alignmentHoldMs: this.alignmentHoldMs,
    };
  }
}
