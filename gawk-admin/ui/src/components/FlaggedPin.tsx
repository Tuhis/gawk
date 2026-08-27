import type { Broadcast } from '../api/types.ts';

/**
 * R40's hook point, present and inert (docs/42 §4.11, AP6).
 *
 * When content flags land, a flagged broadcast pins to the top of the table
 * with a red badge, and this is where that badge goes. In R39 nothing is
 * authorized to raise a flag — `POST /api/v1/content-flags` returns 404 — so
 * this renders **nothing at all**, asserted in `FlaggedPin.test.tsx`.
 *
 * It exists now rather than in R40 for one reason: it fixes where the marker
 * lives in the row and in the sort order while there is still no data to
 * argue with, so R40 adds a data source rather than a layout.
 *
 * The parameter is intentionally unread — the seam is the signature, and R40's
 * change should be to this file's body, not to every call site.
 */
export function FlaggedPinSlot(_props: { broadcast: Broadcast }) {
  return null;
}
