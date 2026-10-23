// Whether the in-session indicator has a local relay surface to show
// (LocalRelayChip.tsx): kept apart so that file exports components only.

import { useTransportStore } from '../../state/transportStore';
import { usePopStore } from './popStore';

// True when there is anything for LocalRelaySurfaces to show.
export function useHasLocalRelaySurface(): boolean {
  const onPop = useTransportStore((s) => s.resolvedSource === 'pop');
  const toast = usePopStore((s) => s.toast);
  const note = usePopStore((s) => s.note);
  const declined = usePopStore((s) => s.declined);
  return onPop || toast !== null || note !== null || declined !== null;
}
