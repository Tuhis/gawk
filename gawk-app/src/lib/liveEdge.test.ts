import { describe, expect, it } from 'vitest';
import {
  ENTRY_SEEK_SLACK_S,
  liveEdgePolicy,
  liveEdgeSeekTarget,
  type LiveEdgePolicy,
} from './liveEdge';

// The worst arrival jitter this project has measured in the field (docs/20
// finding 6: 72–158 ms). The MSE presentation's cushion is fixed by the seek
// that lands the playhead (appends arrive at 1×, after the paced release), so
// the target lag IS the jitter budget for the whole fullscreen session.
const WORST_MEASURED_JITTER_S = 0.158;

function ranges(list: Array<[number, number]>): TimeRanges {
  return {
    length: list.length,
    start: (i: number) => list[i]![0],
    end: (i: number) => list[i]![1],
  } as TimeRanges;
}

describe('liveEdgePolicy', () => {
  it('leaves live viewers a cushion well above the measured arrival jitter', () => {
    const live = liveEdgePolicy('live');
    // ≥ 2.5× the worst measured jitter, inside the 0.4–0.6 s band docs/27
    // finding 3 calls for (the old 0.1 s sat below the jitter itself).
    expect(live.targetLagS).toBeGreaterThanOrEqual(2.5 * WORST_MEASURED_JITTER_S);
    expect(live.targetLagS).toBeGreaterThanOrEqual(0.4);
    expect(live.targetLagS).toBeLessThanOrEqual(0.6);
  });

  it('scales the cushion with the latency the delivery mode already bought', () => {
    const live = liveEdgePolicy('live');
    const resilient = liveEdgePolicy('resilient');
    const deep = liveEdgePolicy('deep');
    expect(resilient.targetLagS).toBeGreaterThan(live.targetLagS);
    expect(deep.targetLagS).toBeGreaterThan(resilient.targetLagS);
  });

  it('keeps the catch-up trigger clear of the target so a landed seek never re-triggers', () => {
    for (const mode of ['live', 'resilient', 'deep'] as const) {
      const p: LiveEdgePolicy = liveEdgePolicy(mode);
      // At least a second of drift before another seek: catching up is a
      // visible jump, and a band narrower than a jitter excursion would turn
      // it into a seek loop.
      expect(p.catchUpLagS - p.targetLagS).toBeGreaterThanOrEqual(1);
    }
    // Live's trigger is unchanged from the original 2 s.
    expect(liveEdgePolicy('live').catchUpLagS).toBe(2);
  });
});

describe('liveEdgeSeekTarget', () => {
  it('lands a lagging playhead exactly the target lag behind the buffered end', () => {
    expect(liveEdgeSeekTarget(ranges([[0, 30]]), 2, 0.5, 1)).toBeCloseTo(29.5, 9);
  });

  it('measures against the NEWEST range and never seeks before its start', () => {
    expect(liveEdgeSeekTarget(ranges([[0, 5], [20, 30]]), 2, 0.5, 1)).toBeCloseTo(29.5, 9);
    expect(liveEdgeSeekTarget(ranges([[0, 5], [29.8, 30]]), 2, 0.5, 1)).toBeCloseTo(29.8, 9);
  });

  it('does nothing inside the trigger, or with nothing buffered', () => {
    expect(liveEdgeSeekTarget(ranges([[0, 30]]), 29.2, 0.5, 1)).toBeNull();
    expect(liveEdgeSeekTarget(ranges([]), 2, 0.5, 1)).toBeNull();
  });

  it('exposes a positive entry slack smaller than any target', () => {
    expect(ENTRY_SEEK_SLACK_S).toBeGreaterThan(0);
    expect(ENTRY_SEEK_SLACK_S).toBeLessThan(liveEdgePolicy('live').targetLagS);
  });
});
