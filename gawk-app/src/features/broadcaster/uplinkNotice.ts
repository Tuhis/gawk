// The browser broadcaster's upload notice (docs/75 RU9, §6). One home for the
// copy, which BroadcasterScreen (a pill described by the sentence) and
// BroadcastPage (the sentence as a warning block) both show. The pill reads
// as the native apps' does; the sentence differs from theirs where the
// browser's remedy does — there is no upload cap here, and since RU9 the
// stream is no longer delayed, only frozen while frames are dropped.

import type { BroadcastStats } from '../../transport/broadcaster';
import type { ResolutionSelection } from '../../media/ladder';

export const UPLINK_NOTICE_PILL = "Your upload can't keep up";

export function uplinkNoticeSentence(quality: string): string {
  return `Your upload can't keep up with the stream at ${quality}, so people watching may see frozen video. Choose a lower quality, or free up upload bandwidth.`;
}

// The rung being sent: in Auto the applied rung (RU9 does not step the
// ladder, so lowering it means choosing a quality), otherwise the explicit
// one.
export function sentQuality(
  stats: Pick<BroadcastStats, 'autoRung' | 'autoCeiling'> | null,
  selection: ResolutionSelection,
): ResolutionSelection {
  if (selection !== 'auto') return selection;
  return stats?.autoRung ?? stats?.autoCeiling ?? selection;
}
