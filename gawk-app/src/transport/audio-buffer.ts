// R15 (docs/20 Decision 8): the viewer's audio jitter buffer — live-edge
// discipline for the third medium. Pure and clock-injected: every policy
// decision (gap → skip or conceal, late → drop, overflow → shed toward the
// alignment depth, underrun → silence, restart → flush + re-anchor) is
// unit-testable without an AudioContext.
//
// The policies are not independent: field finding 8 shipped an overflow drop
// and a gap concealment that exactly undid each other. Anything added here
// must answer "what does this do to the depth, and what does the next policy
// then do about that?"
//
// Division of labor with the AudioWorklet sink: this class owns *which*
// PCM goes to the speaker and in what order; the worklet is a dumb FIFO
// that drains 128-frame quanta and reports its playhead back (underruns are
// counted there and folded in here via noteUnderrun).

import { getDvrBufferMs } from '../config';
import type { ViewerDeliveryMode } from './resilient';

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

// R21 Deep buffer (docs/26). The audio analogue of playout.ts's
// DVR_PLAYOUT_PROFILE, and for the same reason: in deep mode the video playhead
// sits DVR_BUFFER_MS (`B`) behind live while audio still arrives ~live (it is
// not in the relay ring — docs/26 Decision 8/8a is unshipped), so the audio
// buffer must hold that full depth or it plays ~B ahead of its video. Alignment
// is a start-time decision (docs/20 field finding 4), so a shallow floor here
// is a permanent desync, not a transient the rate trim can walk out. docs/26's
// acceptance note requires the depth ceiling to exceed `B`; pinning seed = min
// = B (max ≥ B) mirrors the video profile exactly. RESILIENT_AUDIO_PROFILE's
// 2000 ms ceiling could not express B, which is why deep mode needs its own.
const DVR_BUFFER_MS = getDvrBufferMs();
export const DVR_AUDIO_PROFILE: AudioBufferProfile = {
  minMs: DVR_BUFFER_MS,
  maxMs: Math.max(RESILIENT_AUDIO_PROFILE.maxMs, DVR_BUFFER_MS),
  seedMs: DVR_BUFFER_MS,
};

// The audio profile matching a viewer delivery mode, three-valued to mirror
// playout.ts's getPlayoutProfile. Deep buffer gets DVR_AUDIO_PROFILE so the
// audio depth floor equals the video offset; resilient and live keep their R19
// profiles. A prior version tested the (now three-valued) mode as a boolean,
// which is always truthy — so it handed live-edge the resilient floor and deep
// mode no deep floor at all, stranding Deep-buffer audio ~B ahead of a video
// playhead B behind (docs/26 A/V field finding, 2026-07-23).
export function audioProfileForDeliveryMode(mode: ViewerDeliveryMode): AudioBufferProfile {
  switch (mode) {
    case 'deep':
      return DVR_AUDIO_PROFILE;
    case 'resilient':
      return RESILIENT_AUDIO_PROFILE;
    default:
      return DEFAULT_AUDIO_PROFILE;
  }
}

// Headroom over measured jitter, mirroring the R12 PlayoutController shape.
const HEADROOM_MS = 20;
// Slew limits (ms of target change per second): grow fast to avoid dropouts,
// shrink slowly so a single quiet moment doesn't collapse the buffer.
const SLEW_UP_MS_PER_S = 50;
const SLEW_DOWN_MS_PER_S = 5;
// Overflow ceiling: anything beyond target + this is backlog, not jitter.
const OVERFLOW_SLACK_MS = 200;
// How far audio may run *ahead* of the video it was aligned to before a hole
// is worth paying for in synthesized silence (field finding 8, user decision
// 2026-07-23).
//
// Concealment exists for one reason: alignment is a start-time decision (the
// worklet then runs at 1×, so no later buffering can move a sample), which
// means skipping a hole instead of filling it moves every following sample
// earlier by the hole's length. Below this budget that lead is inaudible and
// av-sync's rate trim walks it back out; above it, lip sync goes visibly
// wrong. Silence is never the cheaper option — it is only the lesser evil.
export const MAX_AUDIO_LEAD_MS = 100;
// How long the depth estimate may be extrapolated between the sink's ~4 Hz
// playhead reports before the ceiling stops trusting it. See queuedNowMs().
const MAX_DRAIN_EXTRAPOLATION_MS = 500;
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
  // Holes small enough to skip inside the lead budget (field finding 8).
  // Split from gapsConcealed rather than folded into it: they are the same
  // event with opposite treatments, and it was precisely the *ratio* of
  // concealments to overflow drops that identified the finding-8 latch.
  gapsSkipped: number;
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
// this. It is a safety net for a broken/absent schedule ("audio plays, badly
// aligned" instead of "audio never plays"), NOT a normal release path — so it
// must sit clearly above the deepest *legitimate* hold. The historical 3000 ms
// covers the live/resilient profiles, but Deep buffer holds `B` (≥ 3000 ms,
// tunable to 30 s), so the effective cap tracks the active profile's ceiling
// via alignmentHoldCapMs(); at 3000 ms flat the net fired as a matter of course
// in deep mode and preempted the very schedule it exists to back up (docs/26
// A/V field finding). This constant stays the floor for the shallow profiles,
// so live/resilient behavior is byte-identical.
export const MAX_ALIGNMENT_HOLD_MS = 3000;
// Headroom between the deepest intended hold (the profile ceiling) and the
// point the safety net trips, so ordinary jitter around a deep hold never
// crosses it.
export const ALIGNMENT_HOLD_MARGIN_MS = 1000;

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
  // True while dropping incoming audio to get back down to the alignment
  // depth after a backlog. Hysteretic — see the ceiling check in push().
  private shedding = false;

  // The next expected timestamp (µs) — how gaps are detected. Null before
  // the first accepted chunk and after every flush.
  private nextExpectedUs: number | null = null;
  // What the sink held at its last report, in ms — the worklet's own measure
  // of its queue, not an estimate maintained here (field finding 8). Between
  // reports it is stale, so read it through queuedNowMs(), never directly, or
  // the ceiling sees a sawtooth.
  private queuedMs = 0;
  // Local ms of the last ground truth about the sink's depth (a playhead
  // report, or the release that handed it the cushion). Null before playback.
  private lastDrainAtMs: number | null = null;
  // How far ahead of the video schedule the holes we chose to skip have put
  // us (field finding 8). Concealment pays this back in full, so it is a debt
  // and not a running average.
  private skipLeadMs = 0;
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
    gapsSkipped: 0,
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
  // Whether any audio has ever been released to the sink on this timeline (set
  // at the first release, cleared on flush). Before the first release the
  // connected worklet pulls silence while we deliberately hold the cushion, and
  // those "underruns" are expected pre-roll — not the dry-after-playback event
  // noteUnderrun's re-prime is for. Without this flag a multi-second deep hold's
  // ~1100 pre-roll underruns clear alignOnSchedule and drop the buffer onto the
  // depth floor, losing the video-schedule lip sync before it ever applies.
  private everReleased = false;
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
    //
    // Shedding is hysteretic: once over the ceiling, drop all the way back to
    // the depth alignment actually chose, not merely back under the ceiling.
    // Stopping at the ceiling leaves the buffer hovering exactly there, where
    // input rate == drain rate keeps it, dropping a packet at the margin
    // forever (chronic crackle) and leaving the excess as permanent A/V skew
    // for the rate trim to walk out over ~a minute at 0.4 %. One bounded shed
    // is the cheaper trade.
    const floorMs = Math.max(this.targetMs, this.establishedDepthMs);
    // While priming, the cushion is *supposed* to reach the alignment depth
    // (B in deep mode); cap it at the same safety net the release uses, not the
    // flat 3000 ms — otherwise a deep cushion sheds itself before it is built.
    const ceilingMs = this.priming ? this.alignmentHoldCapMs() : floorMs + OVERFLOW_SLACK_MS;
    const bufferedMs = this.bufferedMs();
    if (bufferedMs > ceilingMs) this.shedding = true;
    else if (this.shedding && bufferedMs <= floorMs) this.shedding = false;
    if (this.shedding) {
      this.stats.overflowDrops++;
      // Advance past what we dropped, so the drop cannot come back as a hole
      // for the gap branch to conceal (field finding 8). Filling a hole we
      // made ourselves re-adds exactly the depth the drop was meant to shed,
      // which makes overflow-dropping unable to lower the depth at all: the
      // buffer latches at the ceiling and converts audio into silence at
      // whatever rate keeps it there (~75 % of it, in the Safari capture).
      //
      // Skipping is also the *right* answer here, not merely the cheap one:
      // an overflow means the sink is holding more than the alignment asked
      // for, i.e. audio is running late, so shedding content pulls it back
      // toward the video rather than away from it. That is why this does not
      // charge skipLeadMs — there is no lead to pay back.
      if (this.nextExpectedUs !== null) {
        this.nextExpectedUs = Math.max(
          this.nextExpectedUs,
          chunk.timestampUs + durationMs * 1000,
        );
      }
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

    // Gap: packets went missing. Skipping the hole costs alignment (everything
    // after it is heard that much earlier); concealing it costs audible
    // silence. Pay in silence only once the accumulated lead is large enough
    // to matter — and then pay the whole debt at once, so the timeline is
    // exactly restored instead of half-corrected (field finding 8).
    if (deltaUs > 0) {
      this.skipLeadMs += deltaUs / 1000;
      let filled = false;
      if (this.skipLeadMs > MAX_AUDIO_LEAD_MS) {
        this.stats.gapsConcealed++;
        this.emitSilence(this.skipLeadMs, chunk.sampleRate, chunk.channels.length);
        this.skipLeadMs = 0;
        filled = true;
      } else {
        this.stats.gapsSkipped++;
      }
      this.nextExpectedUs = chunk.timestampUs + durationMs * 1000;
      this.emitChunk(chunk, durationMs);
      return filled ? 'gap-filled' : 'accepted';
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
      // Counted optimistically until the sink's next report corrects it; only
      // delivered audio counts (field finding 7).
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
    if (!due && this.pendingMs < this.alignmentHoldCapMs()) return;

    this.alignmentHoldMs = this.pendingMs;
    this.priming = false;
    this.everReleased = true;
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
    // The release is fresh ground truth about the sink's depth: it holds
    // exactly what we just handed it, and starts draining now.
    this.lastDrainAtMs = this.now();
  }

  // The sink's depth *now*, not at its last report. queuedMs is only credited
  // down on the worklet's ~4 Hz playhead report, so reading it raw over-states
  // depth by up to a full report interval (250 ms) — more than OVERFLOW_SLACK_MS
  // (200 ms). A perfectly healthy real-time producer feeding a real-time sink
  // therefore cleared the ceiling near the end of every window and dropped a
  // slice of audio, 4×/s, forever (field finding 8, ~46 drops/s in the
  // regression test). Between reports the worklet is known to drain at 1× — it
  // consumes exactly sampleRate samples per second — so extrapolate, and let
  // the next report correct it.
  //
  // Capped: if reports stop entirely (a suspended context — Safari does this
  // at will) the extrapolation must not decay a real backlog to zero and let
  // the buffer flood a worklet that is not draining. Past the cap the estimate
  // freezes and AudioSink's stall watchdog owns recovery.
  private queuedNowMs(): number {
    if (this.queuedMs <= 0) return 0;
    if (this.lastDrainAtMs === null) return this.queuedMs;
    const elapsed = Math.min(
      MAX_DRAIN_EXTRAPOLATION_MS,
      Math.max(0, this.now() - this.lastDrainAtMs),
    );
    return Math.max(0, this.queuedMs - elapsed);
  }

  private bufferedMs(): number {
    return this.queuedNowMs() + this.pendingMs;
  }

  // The safety-net hold, tracking the active profile so a deep cushion is never
  // preempted by it (see MAX_ALIGNMENT_HOLD_MS). The deepest legitimate hold is
  // the profile's ceiling; the net trips one margin above that. For the shallow
  // profiles this is exactly the historical 3000 ms.
  private alignmentHoldCapMs(): number {
    return Math.max(MAX_ALIGNMENT_HOLD_MS, this.profile().maxMs + ALIGNMENT_HOLD_MARGIN_MS);
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

  // The sink holds exactly this much audio (ms), as measured by the worklet
  // itself and reconciled for anything still in flight. Authoritative: it
  // replaces the running count rather than adjusting it.
  //
  // Findings 7 and 8 were both the same shape — a *shadow* of the worklet's
  // queue, maintained here from deliveries and drain deltas, diverging from
  // the real thing with no way to notice (undelivered chunks; a context
  // running at a rate we assumed; a suspended worklet). A shadow cannot audit
  // itself, so the queue's owner reports it instead, in content ms so the
  // context's sample rate never enters the accounting.
  noteDepth(queuedMs: number): void {
    // A non-finite report is no information, not bad information. Every depth
    // comparison here is a `>` or `>=`, and all of them are false against NaN:
    // the ceiling would stop shedding and, worse, the priming gate would never
    // open again — permanent silence. Keep the last good value and let the
    // extrapolation below decay it until a sound report lands.
    if (!Number.isFinite(queuedMs)) return;
    this.queuedMs = Math.max(0, queuedMs);
    // Ground truth: restart the extrapolation window.
    this.lastDrainAtMs = this.now();
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
    // Before the first release the worklet is connected and pulling silence
    // while we deliberately hold the alignment cushion (a deep buffer holds
    // ~B ms). That is expected pre-roll, not a dry-after-playback event:
    // counting it inflates the stat (a 3 s deep hold logs ~1100 "underruns")
    // and, worse, the re-prime below clears alignOnSchedule and would drop the
    // deep buffer onto the depth floor — anchored to audio arrival, missing the
    // output-latency lead — instead of the video playhead it is meant to align
    // to (docs/20 field finding 4). Ignore it and keep waiting for the schedule.
    if (!this.everReleased) return;
    this.stats.underruns += count;
    // Only when it is *still* dry at the report: queuedMs comes from the same
    // report (noteDepth lands first), so it says what the worklet holds now,
    // not what it held at the dry instant. A queue that already refilled must
    // not be sent back to priming to pay a second silence for a closed gap.
    if (this.queuedMs > 0) return;
    this.queuedMs = 0;
    this.establishedDepthMs = 0;
    this.priming = true;
    this.dueAtMs = null;
    this.lastDrainAtMs = null;
    this.shedding = false;
    // Alignment is being rebuilt from here, so any lead accrued against the
    // old one is meaningless — and the silence the worklet just played for
    // itself has already pushed audio the other way.
    this.skipLeadMs = 0;
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
    this.lastDrainAtMs = null;
    // The lead belonged to the timeline being dropped: carrying it over would
    // make the new timeline's first small hole pay a debt it never incurred.
    this.skipLeadMs = 0;
    this.pending = [];
    this.pendingMs = 0;
    this.priming = true;
    this.shedding = false;
    // A fresh timeline gets a fresh alignment decision — the one case where
    // re-deciding against the video schedule is exactly right.
    this.alignOnSchedule = true;
    // Back to pre-roll: the new timeline's hold will underrun the worklet again
    // and those reports must not re-prime it off the schedule (see noteUnderrun).
    this.everReleased = false;
    this.dueAtMs = null;
    this.alignmentHoldMs = null;
    this.establishedDepthMs = 0;
    // The counters describe the timeline being played, not the page view. The
    // sink deliberately outlives individual sessions (useViewerConnection:
    // "The sink outlives individual sessions"), so leaving them running made
    // the audioBuffer block the only cumulative-across-reconnects section of a
    // Copy-diagnostics capture — uncomparable with the per-attempt counters
    // beside it, and actively misleading (BUGS.md, 2026-07-22). `resets` is
    // the exception by design: it is what tells a reader how many earlier
    // timelines the surviving numbers are not describing.
    const resets = this.stats.resets + 1;
    this.stats = {
      gapsConcealed: 0,
      gapsSkipped: 0,
      lateDrops: 0,
      overflowDrops: 0,
      underruns: 0,
      resets,
    };
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
