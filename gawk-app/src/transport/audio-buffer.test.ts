// R15 N4 (docs/20 Decision 8): the audio jitter buffer's policies, pure and
// clock-injected — gap ⇒ silence, late ⇒ drop, overflow ⇒ drop, underrun ⇒
// counted, restart ⇒ flush + re-anchor — plus the Decision 12 profile
// widening under resilient mode.

import { describe, expect, it } from 'vitest';

import {
  AudioJitterBuffer,
  DEFAULT_AUDIO_PROFILE,
  MAX_ALIGNMENT_HOLD_MS,
  RESILIENT_AUDIO_PROFILE,
  type AudioChunk,
} from './audio-buffer';

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

function collecting() {
  const emitted: AudioChunk[] = [];
  const buffer = new AudioJitterBuffer((c) => void emitted.push(c));
  return { emitted, buffer };
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

  it('gap ⇒ silence of exactly the missing duration + counter', () => {
    const { emitted, buffer } = collecting();
    buffer.push(chunk(0));
    // Packets 1 and 2 never arrive: 40 ms hole.
    expect(buffer.push(chunk(3 * FRAME_US))).toBe('gap-filled');

    expect(buffer.getStats().gapsConcealed).toBe(1);
    // Emitted: the first chunk, the silence, then the late-arriving one.
    expect(emitted).toHaveLength(3);
    const silence = emitted[1];
    expect(silence.frameCount).toBe((SAMPLE_RATE * 0.04) | 0);
    expect(silence.channels[0].every((s) => s === 0)).toBe(true);
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
    buffer.notePlayed(10_000);
    expect(buffer.push(chunk(ts))).not.toBe('overflow');
    expect(emitted.length).toBeGreaterThan(0);
  });

  it('underruns are counted from the sink', () => {
    const { buffer } = collecting();
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
    buffer.notePlayed(20);
    expect(buffer.getStats().bufferedMs).toBeCloseTo(40, 5);
    // Never negative, even if the sink over-reports.
    buffer.notePlayed(500);
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

    buffer.notePlayed(60);
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

