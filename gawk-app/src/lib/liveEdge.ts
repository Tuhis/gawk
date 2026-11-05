import type { ViewerDeliveryMode } from '../transport/resilient';

// Where the iPhone native-fullscreen playhead sits relative to the MSE
// buffered end — the ONE definition, shared by the in-gesture entry seek
// (useFullscreen) and the presenter's catch-up (MsePresenter.maybeCatchUp).
// It used to be a 0.1 s constant duplicated in both (docs/27 finding 3).
//
// Why the target lag is the whole jitter budget: the muxer is fed from
// `decodeReleased`, i.e. AFTER the reorder buffer's paced release, so appends
// arrive at playback rate and the SourceBuffer cannot build ahead while the
// element plays. Whatever cushion a seek leaves is what the session has until
// the next seek. 0.1 s was below the 72–158 ms arrival jitter this project has
// measured in the field (docs/20 finding 6), so WebKit — which pauses on an MSE
// underrun — paused.
//
// Per delivery mode, because the cushion is pure fullscreen-only latency, and
// viewers who chose resilient or Deep buffer have already bought latency for
// smoothness. A deeper playout buffer does NOT supply this cushion itself: its
// delay is spent upstream of the release point and the output is still 1×.
//
//   live       0.5 s — ~3× the worst measured jitter (158 ms); the middle of
//              the 0.4–0.6 s band docs/27 finding 3 calls for
//   resilient  1.0 s — the top of resilient's ~150–500 ms playout envelope
//              plus the live cushion: rides out a carrier handover burst
//   deep       1.5 s — Deep buffer viewers sit seconds behind already
//
// The catch-up trigger sits 1.5 s above each target: a catch-up is a visible
// jump, and a band narrower than a jitter excursion would turn it into a seek
// loop. Live keeps the original 2 s trigger. Since docs/27 finding 2 muxed
// audio onto the same timeline, none of this costs lip-sync.
export interface LiveEdgePolicy {
  // Lag behind the buffered end that a seek lands the playhead at.
  targetLagS: number;
  // Lag past which the playing presentation catches up (back to targetLagS).
  catchUpLagS: number;
}

const CATCH_UP_BAND_S = 1.5;

function policy(targetLagS: number): LiveEdgePolicy {
  return { targetLagS, catchUpLagS: targetLagS + CATCH_UP_BAND_S };
}

const POLICIES: Record<ViewerDeliveryMode, LiveEdgePolicy> = {
  live: policy(0.5),
  resilient: policy(1.0),
  deep: policy(1.5),
};

export function liveEdgePolicy(mode: ViewerDeliveryMode): LiveEdgePolicy {
  return POLICIES[mode] ?? POLICIES.live;
}

// How far the entry seek tolerates the playhead sitting from the target lag
// (either side) before it re-lands it. Small: the entry is the one moment a
// seek is free (the element is paused and about to start).
export const ENTRY_SEEK_SLACK_S = 0.25;

// The seek a playhead at `currentTime` needs: to `targetLagS` behind the
// newest buffered range's end (never before that range's start), when it lags
// by more than `triggerLagS`. Null when no seek is due or nothing is buffered.
export function liveEdgeSeekTarget(
  buffered: TimeRanges,
  currentTime: number,
  targetLagS: number,
  triggerLagS: number,
): number | null {
  if (buffered.length === 0) return null;
  const end = buffered.end(buffered.length - 1);
  if (end - currentTime <= triggerLagS) return null;
  return Math.max(buffered.start(buffered.length - 1), end - targetLagS);
}
