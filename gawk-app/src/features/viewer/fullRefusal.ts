// Capacity refusals as the screens see them (BUGS.md fix 2026-11-06). A pure
// module of its own, not an export of useViewerConnection: tests mock that
// hook wholesale, and a helper living there would vanish with the mock.

import type { ViewerErrorKind } from '../../transport/viewer-session';
import { CLOSE_CODE_BROADCAST_FULL, CLOSE_CODE_RELAY_FULL } from '../../transport/wire';

// The card a capacity refusal shows while the session retries, or null for
// any other close: 4018 the broadcast is full, 4019 the relay is.
export function fullKind(closeCode: number | null | undefined): ViewerErrorKind | null {
  if (closeCode === CLOSE_CODE_BROADCAST_FULL) return 'broadcast-full';
  if (closeCode === CLOSE_CODE_RELAY_FULL) return 'relay-full';
  return null;
}

// Whether an error kind is a capacity refusal: its card shows while the
// status is 'reconnecting', because the session is still retrying.
export function isFullErrorKind(kind: ViewerErrorKind | null): boolean {
  return kind === 'broadcast-full' || kind === 'relay-full';
}
