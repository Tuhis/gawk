// R15 N4 (docs/20 Decision 8): the audio jitter buffer's policies, pure and
// clock-injected — gap ⇒ silence, late ⇒ drop, overflow ⇒ drop, underrun ⇒
// counted, restart ⇒ flush + re-anchor — plus the Decision 12 profile
// widening under resilient mode.

import { describe, expect, it } from 'vitest';

import {
  AudioJitterBuffer,
  DEFAULT_AUDIO_PROFILE,
  DVR_AUDIO_PROFILE,
  MAX_ALIGNMENT_HOLD_MS,
  RESILIENT_AUDIO_PROFILE,
  audioProfileForDeliveryMode,
  type AudioChunk,
} from './audio-buffer';
import { getDvrBufferMs } from '../config';

const SAMPLE_RATE = 48000;
// 20 ms Opus frames — the production cadence.
const FRAME_COUNT = SAMPLE_RATE / 50;
const FRAME_US = 20_000;

function chunk(timestampUs: number, fill = 0.5): AudioChunk {
  return {
    timestampUs,
    channels: [new Float32Array(FRAME_COUNT).fill(fill), new Float32Array(FRAME_COUNT).fill(fill)],
    sampleRate: SAMPLE_RATE,
    frameCount: FRAME_COUNT,
  };
}

// A frozen, injectable clock: the buffer extrapolates the sink's drain between
// playhead reports (field finding 8), so a test that let the wall clock run
// would race its own assertions.
function collecting() {
  const emitted: AudioChunk[] = [];
  const clock = { t: 1000 };
  const buffer = new AudioJitterBuffer((c) => void emitted.push(c), DEFAULT_AUDIO_PROFILE, {
    now: () => clock.t,
  });
  return { emitted, buffer, clock };
}

function isSilence(c: AudioChunk): boolean {
  return c.channels[0].every((s) => s === 0);
}

describe('AudioJitterBuffer policies', () => {
  it('accepts a contiguous run without gaps or drops', () => {
    const { emitted, buffer } = collecting();
    for (let i = 0; i < 5; i++) {
      expect(buffer.push(chunk(i * FRAME_US))).toBe('accepted');
    }
    expect(emitted).toHaveLength(5);
    const stats = buffer.getStats();
    expect(stats.gapsConcealed).toBe(0);
    expect(stats.lateDrops).toBe(0);
    expect(stats.overflowDrops).toBe(0);
  });

  it('a gap past the lead budget ⇒ silence of exactly the missing duration', () => {
    const { emitted, buffer } = collecting();
    buffer.push(chunk(0));
    // Packets 1..8 never arrive: a 160 ms hole. Skipping one this big would
    // put audio 160 ms ahead of the video it was aligned to, so it is filled.
    expect(buffer.push(chunk(9 * FRAME_US))).toBe('gap-filled');

    expect(buffer.getStats().gapsConcealed).toBe(1);
    // Emitted: the first chunk, the silence, then the late-arriving one.
    expect(emitted).toHaveLength(3);
    const silence = emitted[1];
    expect(silence.frameCount).toBe((SAMPLE_RATE * 0.16) | 0);
    expect(isSilence(silence)).toBe(true);
    // The real audio still plays after the concealment.
    expect(emitted[2].channels[0][0]).toBeCloseTo(0.5);
  });

  it('late ⇒ dropped, never played — delay must not grow for stragglers', () => {
    const { emitted, buffer } = collecting();
    buffer.push(chunk(0));
    buffer.push(chunk(FRAME_US));
    const before = emitted.length;
    // A packet from before the playhead arrives out of order.
    expect(buffer.push(chunk(0))).toBe('late');
    expect(emitted).toHaveLength(before);
    expect(buffer.getStats().lateDrops).toBe(1);
  });

  it('overflow ⇒ the incoming chunk is dropped once the sink is backlogged', () => {
    const { emitted, buffer } = collecting();
    // Fill well past target + slack without ever draining (notePlayed).
    let ts = 0;
    let overflowed = 0;
    for (let i = 0; i < 100; i++, ts += FRAME_US) {
      if (buffer.push(chunk(ts)) === 'overflow') overflowed++;
    }
    expect(overflowed).toBeGreaterThan(0);
    expect(buffer.getStats().overflowDrops).toBe(overflowed);
    // Draining relieves it: the next push is accepted again.
    buffer.noteDepth(0);
    expect(buffer.push(chunk(ts))).not.toBe('overflow');
    expect(emitted.length).toBeGreaterThan(0);
  });

  it('underruns are counted from the sink once playback has started', () => {
    const { buffer, emitted } = collecting();
    // Pre-roll underruns (the connected worklet pulling silence before the
    // first release) are expected, not a defect, and are not counted.
    buffer.noteUnderrun(5);
    expect(buffer.getStats().underruns).toBe(0);
    // Start playback: no schedule ⇒ the 60 ms seed floor gates the release.
    for (let i = 0; i < 3; i++) buffer.push(chunk(i * FRAME_US));
    expect(emitted.length).toBeGreaterThan(0);
    // Now a dry underrun is a real dry-after-playback event and is counted.
    buffer.noteDepth(0);
    buffer.noteUnderrun();
    buffer.noteUnderrun(3);
    expect(buffer.getStats().underruns).toBe(4);
  });

  it('bufferedMs tracks queue depth as the sink drains', () => {
    const { buffer } = collecting();
    // Prime first (field finding 3): before that the sink holds nothing, so
    // there is nothing for it to report draining.
    for (let i = 0; i < 3; i++) buffer.push(chunk(i * FRAME_US));
    expect(buffer.getStats().bufferedMs).toBeCloseTo(60, 5);
    buffer.noteDepth(40);
    expect(buffer.getStats().bufferedMs).toBeCloseTo(40, 5);
    // Never negative, even if the sink reports nonsense.
    buffer.noteDepth(-5);
    expect(buffer.getStats().bufferedMs).toBe(0);
  });

  // The restart criterion: without flush/re-anchor, every packet on the new
  // timeline reads as "older than the playhead" and is late-dropped forever.
  it('a restarted timeline re-anchors instead of late-dropping forever', () => {
    const { emitted, buffer } = collecting();
    for (let i = 0; i < 5; i++) buffer.push(chunk(1_000_000 + i * FRAME_US));
    const before = emitted.length;

    // Broadcaster restart: timestamps jump back to a fresh timeline. The
    // re-anchor flushes, so the new timeline primes its cushion before playing
    // (field finding 3) — three 20 ms chunks, then everything flows again.
    expect(buffer.push(chunk(0))).toBe('accepted');
    expect(buffer.push(chunk(FRAME_US))).toBe('accepted');
    expect(buffer.push(chunk(2 * FRAME_US))).toBe('accepted');
    expect(emitted.length).toBe(before + 3);
    expect(buffer.getStats().lateDrops).toBe(0);
  });

  it('explicit flush drops the queue and re-anchors', () => {
    const { buffer } = collecting();
    buffer.push(chunk(0));
    buffer.push(chunk(FRAME_US));
    expect(buffer.getStats().bufferedMs).toBeGreaterThan(0);

    buffer.flush();
    expect(buffer.getStats().bufferedMs).toBe(0);
    expect(buffer.getStats().resets).toBe(1);

    // A far-future timestamp after a flush is an anchor, not a giant gap.
    expect(buffer.push(chunk(9_000_000))).toBe('accepted');
    expect(buffer.getStats().gapsConcealed).toBe(0);
  });

  it('flush zeroes the per-timeline counters but keeps counting resets', () => {
    // BUGS.md (2026-07-22): the sink outlives individual sessions, and flush()
    // used to bump `resets` while leaving every other counter running. So the
    // audioBuffer block in a Copy-diagnostics capture was cumulative over the
    // whole page view while everything beside it was per-attempt — which is
    // how a capture ended up reporting 12816 overflow drops against 4908
    // decoded packets, a comparison that reads as a wild accounting bug and is
    // really two different time bases. The counters describe the CURRENT
    // timeline; `resets` says how many came before.
    const { buffer } = collecting();
    buffer.push(chunk(0));
    buffer.push(chunk(3 * FRAME_US)); // a 40 ms hole: skipped inside the budget
    buffer.push(chunk(9 * FRAME_US)); // 100 ms more: past it, so concealed
    buffer.push(chunk(4 * FRAME_US)); // now late: dropped
    const before = buffer.getStats();
    expect(before.gapsConcealed).toBeGreaterThan(0);
    expect(before.gapsSkipped).toBeGreaterThan(0);
    expect(before.lateDrops).toBeGreaterThan(0);
    buffer.noteUnderrun(3);
    expect(buffer.getStats().underruns).toBe(3);

    buffer.flush();

    const after = buffer.getStats();
    expect(after.gapsConcealed).toBe(0);
    expect(after.gapsSkipped).toBe(0);
    expect(after.lateDrops).toBe(0);
    expect(after.overflowDrops).toBe(0);
    expect(after.underruns).toBe(0);
    // The one counter that must survive: it is what tells a reader how many
    // timelines the surviving numbers are NOT describing.
    expect(after.resets).toBe(1);
    buffer.flush();
    expect(buffer.getStats().resets).toBe(2);
  });
});

// Field findings 3 + 4 (docs/20). Finding 3: the target was implemented as an
// overflow *ceiling* only — every chunk went to the worklet on arrival, so the
// sink played at ~0 ms depth and any jitter ran it dry. Finding 4 (the
// video-master revision) makes the release a scheduling decision instead: hold
// audio until the video presentation schedule says it is due, because after
// playback starts the worklet runs at 1x and the alignment can never be
// changed again by buffering.
describe('AudioJitterBuffer alignment', () => {
  function scheduled(dueAt: (timestampUs: number) => number | null) {
    const emitted: AudioChunk[] = [];
    const clock = { t: 1000 };
    const buffer = new AudioJitterBuffer((c) => void emitted.push(c), DEFAULT_AUDIO_PROFILE, {
      now: () => clock.t,
      schedule: () => dueAt,
    });
    return { emitted, buffer, clock };
  }

  it('holds audio until the video schedule says it is due, then releases in order', () => {
    // Audio for timestamp 0 arrives at t=1000 but its video frame is not
    // presented until t=1300: the 300 ms hold IS the lip sync.
    const { emitted, buffer, clock } = scheduled((ts) => 1300 + ts / 1000);

    for (let i = 0; i < 5; i++) buffer.push(chunk(i * FRAME_US));
    expect(emitted).toHaveLength(0);

    clock.t = 1299;
    buffer.tick();
    expect(emitted).toHaveLength(0);

    clock.t = 1300;
    buffer.tick();
    expect(emitted).toHaveLength(5);
    expect(emitted.map((c) => c.timestampUs)).toEqual([0, FRAME_US, 2 * FRAME_US, 3 * FRAME_US, 4 * FRAME_US]);
    // That hold is now the sink's depth — the cushion finding 3 lacked.
    expect(buffer.getStats().bufferedMs).toBeCloseTo(100, 5);
    expect(buffer.getStats().alignmentHoldMs).toBeCloseTo(100, 5);
  });

  it('builds the jitter-floor cushion even when the schedule is due immediately (live-edge)', () => {
    // Live-edge: video presents on arrival, so the schedule says "due now" at
    // ~0 hold. Releasing there would leave the worklet no cushion and let
    // normal jitter starve it (docs/20 field finding 6). The adaptive jitter
    // target is a floor in every mode: hold until it is met, THEN pass through.
    const { emitted, buffer, clock } = scheduled(() => 1000); // due immediately
    buffer.push(chunk(0));
    buffer.push(chunk(FRAME_US));
    expect(emitted).toHaveLength(0); // 40 ms < 60 ms seed floor: not yet
    buffer.push(chunk(2 * FRAME_US)); // 60 ms floor reached
    expect(emitted).toHaveLength(3);
    // The cushion the release established IS the worklet's depth.
    expect(buffer.getStats().alignmentHoldMs).toBeCloseTo(60, 5);

    // Aligned now: the offset is fixed, so later chunks pass straight through.
    clock.t = 1020;
    buffer.push(chunk(3 * FRAME_US));
    expect(emitted).toHaveLength(4);
  });

  it('falls back to a depth floor when no schedule is known', () => {
    // A pipeline with no video baseline yet: still must not play at ~0 ms
    // depth (finding 3), so the jitter target becomes the gate.
    const emitted: AudioChunk[] = [];
    const buffer = new AudioJitterBuffer((c) => void emitted.push(c), DEFAULT_AUDIO_PROFILE, {
      now: () => 1000,
      schedule: () => null,
    });
    buffer.push(chunk(0));
    buffer.push(chunk(FRAME_US));
    expect(emitted).toHaveLength(0);
    buffer.push(chunk(2 * FRAME_US)); // 60 ms = seed target
    expect(emitted).toHaveLength(3);
  });

  it('never holds audio hostage to a schedule that never fires', () => {
    const { emitted, buffer } = scheduled(() => 9_999_999);
    for (let i = 0; i * 20 <= MAX_ALIGNMENT_HOLD_MS; i++) buffer.push(chunk(i * FRAME_US));
    expect(emitted.length).toBeGreaterThan(0);
  });

  it('re-primes on a dry underrun by depth, not by a schedule already past', () => {
    // The schedule for the oldest pending chunk is in the past by definition
    // at this point — that is why we ran dry. Honoring it would release
    // instantly and rebuild no cushion at all.
    const { emitted, buffer, clock } = scheduled((ts) => 1000 + ts / 1000);
    // Start playback by building the jitter-floor cushion (60 ms = 3 chunks).
    buffer.push(chunk(0));
    buffer.push(chunk(FRAME_US));
    buffer.push(chunk(2 * FRAME_US));
    expect(emitted).toHaveLength(3);

    buffer.noteDepth(0);
    buffer.noteUnderrun(4);
    const before = emitted.length;

    clock.t = 5000; // long past every chunk's scheduled slot
    buffer.push(chunk(3 * FRAME_US));
    buffer.push(chunk(4 * FRAME_US));
    expect(emitted).toHaveLength(before); // rebuilding depth
    buffer.push(chunk(5 * FRAME_US));
    expect(emitted).toHaveLength(before + 3);
  });

  it('flush re-arms the alignment decision for the new timeline', () => {
    const { emitted, buffer, clock } = scheduled((ts) => 2000 + ts / 1000);
    buffer.push(chunk(0));
    buffer.flush();
    const before = emitted.length;

    clock.t = 1500;
    // Build the floor on the new timeline (60 ms); the oldest chunk's schedule
    // slot is 2200, so it is still held against the schedule, not the depth.
    buffer.push(chunk(10 * FRAME_US));
    buffer.push(chunk(11 * FRAME_US));
    buffer.push(chunk(12 * FRAME_US));
    expect(emitted).toHaveLength(before); // held again, against the schedule
    clock.t = 2200;
    buffer.tick();
    expect(emitted.length).toBeGreaterThan(before); // schedule due AND floor met
  });

  it('a deliberate alignment hold does not read as overflow', () => {
    // The hold is legitimately far deeper than the jitter target; the old
    // ceiling would have dropped most of it as backlog.
    const { emitted, buffer, clock } = scheduled((ts) => 1400 + ts / 1000);
    for (let i = 0; i < 20; i++) buffer.push(chunk(i * FRAME_US));
    clock.t = 1400;
    buffer.tick();
    expect(buffer.getStats().overflowDrops).toBe(0);
    expect(emitted).toHaveLength(20);
  });
});

describe('AudioJitterBuffer adaptive target', () => {
  it('seeds, clamps and slews inside the default profile', () => {
    const { buffer } = collecting();
    expect(buffer.getStats().targetMs).toBe(DEFAULT_AUDIO_PROFILE.seedMs);

    // First sample applies directly; huge jitter clamps at the profile max.
    buffer.updateTarget(5000, 0);
    expect(buffer.getStats().targetMs).toBe(DEFAULT_AUDIO_PROFILE.maxMs);

    // Downward moves are slew-limited (5 ms/s): one second can't collapse it.
    buffer.updateTarget(0, 1000);
    const afterOneSec = buffer.getStats().targetMs;
    expect(afterOneSec).toBeCloseTo(DEFAULT_AUDIO_PROFILE.maxMs - 5, 5);

    // And it never goes below the profile floor.
    for (let t = 2000; t <= 60_000; t += 1000) buffer.updateTarget(0, t);
    expect(buffer.getStats().targetMs).toBe(DEFAULT_AUDIO_PROFILE.minMs);
  });

  // Decision 12: the resilient profile is what keeps audio-master pacing from
  // dragging the video buffer back to ~150 ms.
  it('follows a live profile getter into the resilient envelope', () => {
    const emitted: AudioChunk[] = [];
    let resilient = false;
    const buffer = new AudioJitterBuffer(
      (c) => void emitted.push(c),
      () => (resilient ? RESILIENT_AUDIO_PROFILE : DEFAULT_AUDIO_PROFILE),
    );
    buffer.updateTarget(200, 0);
    // Clamped to the default ceiling while in the default profile.
    expect(buffer.getStats().targetMs).toBe(DEFAULT_AUDIO_PROFILE.maxMs);

    resilient = true;
    // Entering resilient mode re-seeds into the wider envelope rather than
    // carrying a value from the other profile.
    buffer.updateTarget(null, 1000);
    expect(buffer.getStats().targetMs).toBe(RESILIENT_AUDIO_PROFILE.seedMs);

    // And it can now grow far past the default ceiling.
    buffer.updateTarget(1500, 2000);
    expect(buffer.getStats().targetMs).toBeGreaterThan(DEFAULT_AUDIO_PROFILE.maxMs);
    expect(buffer.getStats().targetMs).toBeLessThanOrEqual(RESILIENT_AUDIO_PROFILE.maxMs);
  });

  it('a flush re-seeds the target on the active profile', () => {
    const { buffer } = collecting();
    buffer.updateTarget(5000, 0);
    expect(buffer.getStats().targetMs).toBe(DEFAULT_AUDIO_PROFILE.maxMs);
    buffer.flush();
    expect(buffer.getStats().targetMs).toBe(DEFAULT_AUDIO_PROFILE.seedMs);
  });
});

// Field finding 7 (docs/20): the depth estimate is a *shadow* of the worklet's
// real queue, and it must not count audio the worklet never received. The
// pre-fix buffer incremented queuedMs for every chunk it handed to the emit
// callback, even when the callback dropped it (the AudioWorklet node was still
// booting, or its port threw). That phantom depth inflated bufferedMs past the
// overflow ceiling forever, so every further chunk spuriously overflow-dropped
// — the crackle — and, once the sink stopped draining, froze there with no
// recovery — the silence.
describe('AudioJitterBuffer honest accounting', () => {
  it('does not count chunks the sink never received as buffered depth', () => {
    // A sink that is "ready" (so release proceeds) but rejects every chunk on
    // delivery — the worklet node vanished, or its port throws. emit signals
    // the drop by returning false.
    const buffer = new AudioJitterBuffer(() => false, DEFAULT_AUDIO_PROFILE, {
      now: () => 1000,
      schedule: () => null,
      sinkReady: () => true,
    });
    // Far more than a ceiling's worth: the old buffer would have inflated
    // queuedMs past target+slack and started overflow-dropping. Nothing here
    // ever reached the worklet, so the honest depth is zero and nothing may be
    // reported as backlog.
    for (let i = 0; i < 200; i++) buffer.push(chunk(i * FRAME_US));
    expect(buffer.getStats().bufferedMs).toBe(0);
    expect(buffer.getStats().overflowDrops).toBe(0);
  });

  it('holds audio in priming until the sink can actually receive it', () => {
    const emitted: AudioChunk[] = [];
    let ready = false;
    const buffer = new AudioJitterBuffer((c) => void emitted.push(c), DEFAULT_AUDIO_PROFILE, {
      now: () => 1000,
      schedule: () => null,
      sinkReady: () => ready,
    });
    // The depth floor is met (3 × 20 ms ≥ 60 ms seed), but the worklet has not
    // booted: releasing now would hand the whole cushion to a null node and
    // lose it, leaving the worklet to starve at ~0 ms depth (finding 6 redux).
    for (let i = 0; i < 5; i++) buffer.push(chunk(i * FRAME_US));
    expect(emitted).toHaveLength(0);

    // Once the node exists, the held cushion is delivered in order and counts.
    ready = true;
    buffer.tick();
    expect(emitted.map((c) => c.timestampUs)).toEqual([
      0,
      FRAME_US,
      2 * FRAME_US,
      3 * FRAME_US,
      4 * FRAME_US,
    ]);
    expect(buffer.getStats().bufferedMs).toBeCloseTo(100, 5);
  });

  it('a mid-boot delivery drop leaves the depth honest, not inflated', () => {
    // The realistic boot race: the first chunks land while the node is still
    // coming up (dropped), then delivery succeeds. Only the delivered tail may
    // count toward depth.
    const emitted: AudioChunk[] = [];
    let delivering = false;
    const buffer = new AudioJitterBuffer(
      (c) => {
        if (!delivering) return false;
        emitted.push(c);
        return true;
      },
      DEFAULT_AUDIO_PROFILE,
      { now: () => 1000, schedule: () => null, sinkReady: () => true },
    );
    // Release fires (ready), but the sink drops these three on the floor.
    for (let i = 0; i < 3; i++) buffer.push(chunk(i * FRAME_US));
    expect(buffer.getStats().bufferedMs).toBe(0);
    // Now the worklet is live; the next two are the only real depth.
    delivering = true;
    buffer.push(chunk(3 * FRAME_US));
    buffer.push(chunk(4 * FRAME_US));
    expect(emitted).toHaveLength(2);
    expect(buffer.getStats().bufferedMs).toBeCloseTo(40, 5);
    expect(buffer.getStats().overflowDrops).toBe(0);
  });
});


// Field finding 8 (docs/20): on Safari the buffer latched into ~75 %
// synthesized silence — 37 overflow drops/s against 50 packets/s arriving,
// with zero network loss (received == decoded) and bufferedMs pinned at
// target + slack forever. The cause is a loop between two policies that each
// look right alone: `push` counts an overflow drop *before* advancing
// nextExpectedUs, so the run of dropped packets comes back as a hole, and the
// gap branch conceals that hole with exactly as much silence as was dropped —
// through emitChunk, so it re-adds the very depth the drop was meant to shed.
// Overflow-dropping therefore cannot lower the depth; it only converts audio
// into silence, at whatever rate keeps the estimate at the ceiling.
describe('AudioJitterBuffer overflow does not manufacture silence', () => {
  it('an overflow drop is a skip toward live, not a hole to conceal', () => {
    const { emitted, buffer, clock } = collecting();
    // Establish playback with the seed cushion (3 × 20 ms = 60 ms floor).
    let ts = 0;
    for (let i = 0; i < 3; i++, ts += FRAME_US) buffer.push(chunk(ts));
    expect(emitted).toHaveLength(3);

    // Backlog the sink past the ceiling without ever draining.
    let overflowed = 0;
    for (let i = 0; i < 60; i++, ts += FRAME_US) {
      if (buffer.push(chunk(ts)) === 'overflow') overflowed++;
    }
    expect(overflowed).toBeGreaterThan(5);

    // The sink drains; the next packet is contiguous with the *skipped* run,
    // not 'gap-filled'. Pre-fix this was the latch: a silence chunk exactly as
    // long as the drops, putting the depth straight back over the ceiling.
    clock.t += 1000;
    buffer.noteDepth(0);
    const before = emitted.length;
    expect(buffer.push(chunk(ts))).toBe('accepted');
    expect(emitted).toHaveLength(before + 1);
    expect(emitted.slice(before).some(isSilence)).toBe(false);
    expect(buffer.getStats().gapsConcealed).toBe(0);
  });

  // The headline regression: a realistic producer/consumer loop with the
  // production shape (50 packets/s in, a worklet draining at 1×, ~4 Hz
  // playhead reports) and one arrival burst to push it over the ceiling. The
  // field capture sat at ~25 % real audio; anything near that is the bug.
  it('recovers from an arrival burst instead of latching into silence', () => {
    const clock = { t: 0 };
    let workletMs = 0; // what the worklet actually holds
    let realMs = 0; // audio the speaker gets
    let silenceMs = 0; // audio the speaker doesn't
    const buffer = new AudioJitterBuffer(
      (c) => {
        const ms = (c.frameCount / c.sampleRate) * 1000;
        workletMs += ms;
        if (isSilence(c)) silenceMs += ms;
        else realMs += ms;
        return true;
      },
      DEFAULT_AUDIO_PROFILE,
      { now: () => clock.t },
    );

    let ts = 0;
    const push = () => {
      buffer.push(chunk(ts));
      ts += FRAME_US;
    };
    // Prime.
    for (let i = 0; i < 3; i++) push();
    // The trigger: a 300 ms arrival burst (jitter on the shared datagram
    // path — the capture showed receivedFps swinging 20→48 at ~100 ms of
    // arrival jitter). Every packet is contiguous, so none of it is late.
    for (let i = 0; i < 15; i++) push();

    // 6 s of steady state: one 20 ms packet per 20 ms, the worklet draining in
    // real time and reporting every 250 ms.
    let playedMs = 0;
    let dryQuanta = 0;
    let sinceReport = 0;
    let dropsAt2s = 0;
    for (let step = 0; step < 6000; step += 10) {
      clock.t += 10;
      const drained = Math.min(workletMs, 10);
      workletMs -= drained;
      playedMs += drained;
      if (drained < 10) dryQuanta++;
      if (step % 20 === 0) push();
      sinceReport += 10;
      if (sinceReport >= 250) {
        sinceReport = 0;
        buffer.noteDepth(workletMs);
        if (dryQuanta > 0) buffer.noteUnderrun(dryQuanta);
        playedMs = 0;
        dryQuanta = 0;
      }
      if (step === 2000) dropsAt2s = buffer.getStats().overflowDrops;
    }
    const dropsAtEnd = buffer.getStats().overflowDrops;

    // The speaker hears audio, not silence. Pre-fix: 12 % real, the rest
    // synthesized — audible as constant breakup.
    expect(realMs / (realMs + silenceMs)).toBeGreaterThan(0.99);
    // The burst is shed all the way back to the alignment depth, not merely to
    // just under the ceiling: parked at the ceiling, input rate == drain rate
    // keeps it there, dropping at the margin forever.
    const stats = buffer.getStats();
    expect(stats.bufferedMs).toBeLessThanOrEqual(stats.targetMs);
    // ...and once shed, the steady state costs nothing at all.
    expect(dropsAtEnd - dropsAt2s).toBe(0);
  });

  it('takes the sink\'s reported depth as authoritative, not its own estimate', () => {
    const { buffer, clock } = collecting();
    let ts = 0;
    for (let i = 0; i < 3; i++, ts += FRAME_US) buffer.push(chunk(ts));
    // The estimate drifts above the truth — findings 7 and 8 are both a
    // *shadow* of the worklet's queue diverging from it, and a shadow can
    // never notice on its own.
    for (let i = 0; i < 12; i++, ts += FRAME_US) buffer.push(chunk(ts));
    expect(buffer.getStats().bufferedMs).toBeGreaterThan(200);

    // The worklet says what it actually holds. That ends the argument.
    clock.t += 250;
    buffer.noteDepth(45);
    expect(buffer.getStats().bufferedMs).toBeCloseTo(45, 5);

    // And a dry report re-primes, rebuilding the cushion rather than playing
    // on at zero depth.
    clock.t += 250;
    buffer.noteDepth(0);
    buffer.noteUnderrun(4);
    expect(buffer.getStats().bufferedMs).toBe(0);
  });

  it('ignores a non-finite depth report instead of poisoning the estimate', () => {
    // Every depth comparison is a `>` or `>=`, and every one of them is false
    // against NaN: the ceiling would never shed, and — worse — the priming
    // gate's `bufferedMs() >= targetMs` would never open, so audio would go
    // permanently silent. A malformed report (a worklet/sink version skew is
    // the realistic source) must be no information, not bad information.
    const { emitted, buffer } = collecting();
    for (let i = 0; i < 3; i++) buffer.push(chunk(i * FRAME_US));
    const healthy = buffer.getStats().bufferedMs;

    buffer.noteDepth(Number.NaN);
    expect(buffer.getStats().bufferedMs).toBeCloseTo(healthy, 5);

    // And the pipeline keeps running rather than wedging.
    const before = emitted.length;
    buffer.push(chunk(3 * FRAME_US));
    expect(emitted).toHaveLength(before + 1);
  });

  it('does not re-prime when the worklet refilled before its report landed', () => {
    // The underrun happened inside the window, but the queue recovered by the
    // time the report was generated: re-priming here would pay a second
    // silence for a gap that had already closed.
    const { emitted, buffer } = collecting();
    for (let i = 0; i < 3; i++) buffer.push(chunk(i * FRAME_US));
    const before = emitted.length;
    buffer.noteDepth(40);
    buffer.noteUnderrun(2);
    buffer.push(chunk(3 * FRAME_US));
    expect(emitted).toHaveLength(before + 1); // straight through, still aligned
  });
});

// The other half of the latch: the ceiling was evaluated against an estimate
// that is only credited down on the worklet's ~4 Hz playhead report, so it
// over-read by up to a full report interval (250 ms) while OVERFLOW_SLACK_MS
// is 200 ms. A perfectly healthy real-time producer therefore cleared the
// ceiling near the end of every report window and dropped a slice of audio,
// 4×/s, forever.
describe('AudioJitterBuffer depth estimate between playhead reports', () => {
  it('does not overflow-drop a real-time producer feeding a real-time sink', () => {
    const { buffer, clock } = collecting();
    let ts = 0;
    for (let i = 0; i < 3; i++, ts += FRAME_US) buffer.push(chunk(ts));

    // 4 s of exact balance: 20 ms in per 20 ms, drained at 1×, reported at 4 Hz.
    let sinceReport = 0;
    let workletMs = 60; // the cushion the release handed it
    for (let step = 0; step < 4000; step += 20) {
      clock.t += 20;
      workletMs = Math.max(0, workletMs - 20); // drained in real time
      buffer.push(chunk(ts));
      workletMs += 20;
      ts += FRAME_US;
      sinceReport += 20;
      if (sinceReport >= 250) {
        buffer.noteDepth(workletMs);
        sinceReport = 0;
      }
    }
    expect(buffer.getStats().overflowDrops).toBe(0);
    expect(buffer.getStats().gapsConcealed).toBe(0);
  });
});

// The lead budget (user decision 2026-07-23): concealment silence exists to
// keep audio from running *ahead* of the video it was aligned to at playback
// start. Below the budget that lead is inaudible and the av-sync rate trim
// absorbs it; paying for it in synthesized silence is the worse trade.
describe('AudioJitterBuffer gap lead budget', () => {
  it('skips a small hole instead of concealing it', () => {
    const { emitted, buffer } = collecting();
    for (let i = 0; i < 3; i++) buffer.push(chunk(i * FRAME_US));
    const before = emitted.length;

    // A 40 ms hole: two packets lost. Skipping puts audio 40 ms ahead — under
    // the budget, so no silence is synthesized.
    expect(buffer.push(chunk(5 * FRAME_US))).toBe('accepted');
    expect(emitted).toHaveLength(before + 1);
    expect(emitted.slice(before).some(isSilence)).toBe(false);
    expect(buffer.getStats().gapsConcealed).toBe(0);
    expect(buffer.getStats().gapsSkipped).toBe(1);
  });

  it('conceals once the skipped lead accumulates past the budget', () => {
    const { emitted, buffer } = collecting();
    let ts = 0;
    for (let i = 0; i < 3; i++, ts += FRAME_US) buffer.push(chunk(ts));

    // Three 40 ms holes: 40, 80, then 120 ms of accumulated lead. Only the
    // third crosses the budget, and it pays back the *whole* accrued lead so
    // the timeline is exactly restored rather than half-corrected.
    let concealed = 0;
    for (let i = 0; i < 3; i++) {
      ts += 2 * FRAME_US; // two packets lost
      buffer.push(chunk(ts));
      ts += FRAME_US;
      concealed = buffer.getStats().gapsConcealed;
      if (i < 2) expect(concealed).toBe(0);
    }
    expect(concealed).toBe(1);
    expect(buffer.getStats().gapsSkipped).toBe(2);
    const silence = emitted.filter(isSilence);
    expect(silence).toHaveLength(1);
    expect((silence[0].frameCount / SAMPLE_RATE) * 1000).toBeCloseTo(120, 5);
  });

  it('a flush forgets the accrued lead with the timeline it belonged to', () => {
    const { emitted, buffer } = collecting();
    for (let i = 0; i < 3; i++) buffer.push(chunk(i * FRAME_US));
    buffer.push(chunk(5 * FRAME_US)); // 40 ms skipped
    buffer.flush();

    // New timeline: the old lead must not push the first hole here over the
    // budget, or a reconnect would open with silence it doesn't owe.
    let ts = 1_000_000;
    for (let i = 0; i < 3; i++, ts += FRAME_US) buffer.push(chunk(ts));
    const before = emitted.length;
    ts += 2 * FRAME_US;
    buffer.push(chunk(ts));
    expect(emitted.slice(before).some(isSilence)).toBe(false);
  });
});

// R21 Deep buffer (docs/26). In deep mode the video playhead sits DVR_BUFFER_MS
// (`B`) behind live while audio still arrives ~live (audio is NOT in the relay
// ring — docs/26 Decision 8/8a is unshipped), so the audio buffer must hold the
// full `B` depth or audio plays ~B ahead of its video (docs/20 field finding 4:
// video is the master clock, and alignment is a start-time decision that
// buffering can never undo). docs/26's own acceptance note ("What the viewer
// needs"): the audio depth ceiling AND the alignment-hold cap must both exceed
// `B`. The R19 resilient profile (seed 500, max 2000) and MAX_ALIGNMENT_HOLD_MS
// (3000) satisfy neither at B ≥ 3000.
describe('AudioJitterBuffer deep-buffer alignment', () => {
  it('audioProfileForDeliveryMode gives Deep buffer a floor at B, not the resilient 500 ms', () => {
    const B = getDvrBufferMs();
    const deep = audioProfileForDeliveryMode('deep');
    // The depth floor (seed/min) must equal the video offset, else audio locks
    // in ~B−500 ms ahead of video and the rate trim (±0.4 %) can never close it.
    expect(deep.seedMs).toBe(B);
    expect(deep.minMs).toBe(B);
    expect(deep.maxMs).toBeGreaterThanOrEqual(B);
    // And the other two points on the axis are unchanged — in particular
    // live-edge must NOT inherit the resilient floor (the truthy-string bug: a
    // three-valued mode read as a boolean is always truthy).
    expect(audioProfileForDeliveryMode('resilient')).toBe(RESILIENT_AUDIO_PROFILE);
    expect(audioProfileForDeliveryMode('live')).toBe(DEFAULT_AUDIO_PROFILE);
  });

  it('holds audio to the deep floor when the video schedule has not arrived yet', () => {
    // The realistic startup race: the schedule reaches the sink only on the
    // ~2 Hz stats tick, the worklet often boots faster, and audio arrives ahead
    // of video — so the first release is gated by the depth floor alone. It must
    // be B, not 500 ms.
    const emitted: AudioChunk[] = [];
    const B = getDvrBufferMs();
    const buffer = new AudioJitterBuffer((c) => void emitted.push(c), DVR_AUDIO_PROFILE, {
      now: () => 1000,
      schedule: () => null,
    });
    // 500 ms — the old resilient floor — must NOT be enough to start playback.
    for (let i = 0; i * 20 < 500; i++) buffer.push(chunk(i * FRAME_US));
    expect(emitted).toHaveLength(0);
    // Fill up to (but not across) B: still holding, no shedding.
    const chunksUnderB = Math.ceil(B / 20) - 1; // last count with < B ms of audio
    for (let i = 25; i < chunksUnderB; i++) buffer.push(chunk(i * FRAME_US));
    expect(emitted).toHaveLength(0);
    // The chunk that reaches B releases the whole cushion, establishing a ~B
    // sink depth — the cushion the resilient floor could never build.
    buffer.push(chunk(chunksUnderB * FRAME_US));
    expect(emitted.length).toBeGreaterThan(0);
    expect(buffer.getStats().bufferedMs).toBeGreaterThanOrEqual(B);
    expect(buffer.getStats().overflowDrops).toBe(0);
  });

  it('does not release before a deep schedule is due, even when B exceeds the old 3000 cap', () => {
    // B = 5000 > the historical MAX_ALIGNMENT_HOLD_MS of 3000 and its priming
    // ceiling. With the old cap the buffer sheds/escapes at ~3000 ms and audio
    // ends up ~2 s ahead of a 5 s-delayed video. The cap must track the profile.
    const B = 5000;
    const deepProfile = { seedMs: B, minMs: B, maxMs: B };
    const emitted: AudioChunk[] = [];
    const clock = { t: 1000 };
    // Video for timestamp T presents at 1000 + B + T/1000 (B behind live).
    const buffer = new AudioJitterBuffer((c) => void emitted.push(c), deepProfile, {
      now: () => clock.t,
      schedule: () => (ts: number) => 1000 + B + ts / 1000,
    });
    // Feed audio in real time. It must stay held the whole way to the schedule.
    for (let i = 0; clock.t < 1000 + B; i++) {
      buffer.push(chunk(i * FRAME_US));
      clock.t = 1000 + i * 20;
      if (clock.t < 1000 + B - 40) {
        expect(emitted).toHaveLength(0);
        expect(buffer.getStats().overflowDrops).toBe(0);
      }
    }
    // Once due, it releases with a hold ≈ B — aligned with the video playhead.
    clock.t = 1000 + B;
    buffer.tick();
    expect(emitted.length).toBeGreaterThan(0);
    expect(buffer.getStats().alignmentHoldMs).toBeGreaterThanOrEqual(B - 100);
    expect(buffer.getStats().overflowDrops).toBe(0);
  });

  it('keeps the video-schedule alignment through worklet underruns during the hold', () => {
    // The connected AudioWorklet pulls a quantum every ~2.67 ms, so during the
    // multi-second deep hold it reports a continuous run of dry underruns while
    // we DELIBERATELY hold the cushion. Those are expected pre-roll silence, not
    // a dry-after-playback event: they must not re-prime the buffer, because
    // re-prime clears alignOnSchedule and would drop the deep buffer onto the
    // depth floor (anchored to audio arrival) instead of the video playhead —
    // audible as audio drifting off video by ~the output-latency lead.
    const emitted: AudioChunk[] = [];
    const clock = { t: 1000 };
    // Schedule due LATER than the depth floor would release, so a lost
    // schedule alignment is observable: depth is ready at 3000 ms of audio but
    // the video frame is not due until t = 6000.
    const buffer = new AudioJitterBuffer((c) => void emitted.push(c), DVR_AUDIO_PROFILE, {
      now: () => clock.t,
      schedule: () => (ts: number) => 6000 + ts / 1000,
    });
    // Prime past the depth floor but under the hold cap (max(3000,3000+1000)=
    // 4000 ms), so a depth-floor release would be observable while the escape
    // cap is not yet in play: 175 chunks = 3500 ms.
    for (let i = 0; i < 175; i++) buffer.push(chunk(i * FRAME_US));
    expect(emitted).toHaveLength(0); // holding on the schedule, not depth
    buffer.noteDepth(0);
    buffer.noteUnderrun(500);
    buffer.tick();
    // Still holding: the schedule is not due, and the underruns must not have
    // forced a depth-floor release.
    expect(emitted).toHaveLength(0);
    // Pre-release underruns are expected pre-roll, not a defect — not counted.
    expect(buffer.getStats().underruns).toBe(0);
    // The schedule, not the depth floor, still governs the release.
    clock.t = 6000;
    buffer.tick();
    expect(emitted.length).toBeGreaterThan(0);
  });

  it('honours the output-latency lead under a schedule instead of gating on the full deep depth', () => {
    // The schedule already subtracts the sink's output latency (dueAt =
    // present − lead) so audio is HEARD when the frame is shown. But the depth
    // gate needs targetMs (= B in deep mode) buffered, and audio arrives ~real
    // time, so it is only met at arrival + B — LEAD ms AFTER the schedule wanted
    // the release. Requiring the full depth on top of the schedule defeats the
    // lead: audio releases late and plays output-latency behind video. Under a
    // schedule the depth check must be only a small anti-starvation cushion.
    const emitted: AudioChunk[] = [];
    const clock = { t: 1000 };
    const B = 3000;
    const LEAD = 200; // exaggerated output latency, to make the lead observable
    // First chunk arrives at t=1000 (arrival baseline 1000), video offset B, so
    // the frame for ts is shown at ts/1000 + 1000 + B; the sink asks to be
    // released LEAD ms early.
    const buffer = new AudioJitterBuffer((c) => void emitted.push(c), { seedMs: B, minMs: B, maxMs: B }, {
      now: () => clock.t,
      schedule: () => (ts: number) => ts / 1000 + 1000 + B - LEAD,
    });
    // Feed audio in real time until it releases.
    for (let i = 0; emitted.length === 0 && i < 400; i++) {
      clock.t = 1000 + i * 20;
      buffer.push(chunk(i * FRAME_US));
    }
    expect(emitted.length).toBeGreaterThan(0);
    // Released at the lead-compensated schedule (dueAt(T0) = 1000 + B − LEAD),
    // NOT LEAD ms later when the full B depth would finally be buffered.
    expect(clock.t).toBeLessThanOrEqual(1000 + B - LEAD + 40);
    const hold = buffer.getStats().alignmentHoldMs!;
    expect(hold).toBeLessThan(B - LEAD / 2); // clearly short of the full B
    expect(hold).toBeGreaterThan(B - LEAD - 100); // ≈ B − LEAD
  });
});
