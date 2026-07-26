// R28 TM2 (docs/33 D13): the React glue around TelemetryCollector.
//
// Both surfaces need the same three things — a collector whose lifetime
// matches the screen, a `visibilitychange → hidden` flush, and a guarantee
// that unmount ends the session — so they share this hook rather than growing
// two copies that drift.
//
// Collection lives on the main thread by construction: both stats objects
// already arrive here fully assembled (the viewer screen merges in
// `audioBuffer`/`featureGates`/`presentationSurface`, and `BroadcastStats`
// comes back through the worker shell the same way), so the collector
// subscribes to exactly what the overlay renders. No worker message, no
// transport change, no pipeline change — with one recorded exception: the
// hello itself has to cross from the viewer's nested transport worker, which
// is where wire 0x0D arrives. It travels as its own worker message rather than
// as a `ViewerStats` field precisely so the token stays out of the
// Copy-diagnostics blob a user pastes into a chat.

import { useEffect, useRef } from 'react';

import { getTelemetryUrl } from '../config';
import { TelemetryCollector, type TelemetryRole } from './telemetry';

export function useTelemetryCollector<T>(role: TelemetryRole): TelemetryCollector<T> {
  const ref = useRef<TelemetryCollector<T> | null>(null);
  if (ref.current === null) {
    ref.current = new TelemetryCollector<T>({ url: getTelemetryUrl(), role });
  }
  const collector = ref.current;

  useEffect(() => {
    // `hidden` is the hook that actually fires on mobile — `pagehide` and
    // `unload` are unreliable there — and it is bfcache-compatible, so a page
    // that comes back simply keeps collecting under the same identity. It is
    // deliberately NOT a session-final flush: hidden also fires on a tab
    // switch, and a session marked final there would be finalized while it is
    // still streaming.
    const onHidden = () => {
      if (document.visibilityState === 'hidden') collector.flushForUnload();
    };
    document.addEventListener('visibilitychange', onHidden);
    return () => {
      document.removeEventListener('visibilitychange', onHidden);
      collector.stop();
    };
  }, [collector]);

  return collector;
}
