// R15 N4 (docs/20 Decision 8): the audio jitter buffer's policies, pure and
// clock-injected — gap ⇒ silence, late ⇒ drop, overflow ⇒ drop, underrun ⇒
// counted, restart ⇒ flush + re-anchor — plus the Decision 12 profile
// widening under resilient mode.

import { describe, expect, it } from 'vitest';

import {
  AudioJitterBuffer,
  DEFAULT_AUDIO_PROFILE,
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
  const buffer = new AudioJitterBuffer((c) => emitted.push(c));
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
    buffer.push(chunk(0));
    buffer.push(chunk(FRAME_US));
    expect(buffer.getStats().bufferedMs).toBeCloseTo(40, 5);
    buffer.notePlayed(20);
    expect(buffer.getStats().bufferedMs).toBeCloseTo(20, 5);
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

    // Broadcaster restart: timestamps jump back to a fresh timeline.
    expect(buffer.push(chunk(0))).toBe('accepted');
    expect(buffer.push(chunk(FRAME_US))).toBe('accepted');
    expect(emitted.length).toBe(before + 2);
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
      (c) => emitted.push(c),
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

// CODE-REVIEW.md: counters survive their owner's deletion. The viewer folds
// the decode lane's stats before dropping it, so a lane that died reports
// what it did rather than zeros — "audio worked then failed" must not read as
// "audio never worked". This pins the AudioDecodeLane side of that contract:
// its getStats() is a snapshot, safe to retain after stop().
describe('AudioDecodeLane stats are a retainable snapshot', () => {
  it('getStats() survives stop() as an independent object', async () => {
    const { AudioDecodeLane } = await import('./audio-decode');
    const lane = new AudioDecodeLane({ onChunk: () => {}, onError: () => {} });
    const before = lane.getStats();
    lane.stop();
    const after = lane.getStats();
    // Distinct objects (a snapshot, not a live view the caller can mutate).
    expect(after).not.toBe(before);
    expect(after).toEqual(before);
    // And the retained copy is unaffected by later calls.
    expect(before.packetsDecoded).toBe(0);
  });
});
