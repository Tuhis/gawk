// The React glue around TelemetryCollector: a collector living as long as the
// screen, a flush when the page is hidden, and a session end on unmount.
// Collection runs on the main thread over exactly the stats object the
// overlay renders. The telemetry hello crosses from the viewer's transport
// worker as its own message, not a ViewerStats field, so its token stays out
// of the Copy-diagnostics blob.

import { useEffect, useRef } from 'react';

import { getTelemetryUrl } from '../config';
import { TelemetryCollector, type TelemetryRole } from './telemetry';

export function useTelemetryCollector<T>(role: TelemetryRole): TelemetryCollector<T> {
  const ref = useRef<TelemetryCollector<T> | null>(null);
  if (ref.current === null) {
    // The configured URL is the fallback on any relay; a relay-advertised
    // endpoint wins when it arrives (collector.setAdvertisedUrl).
    ref.current = new TelemetryCollector<T>({ url: getTelemetryUrl(), role });
  }
  const collector = ref.current;

  // stop() is terminal (it refuses every later begin()), so it is deferred a
  // macrotask and cancelled by a remount: StrictMode's development mount →
  // cleanup → remount reuses this instance and this ref, and a synchronous
  // stop() would kill the collector before the hello arrived. It cannot just
  // be dropped either: on a real unmount a late worker message can still
  // reach begin(). Recreating the collector would not work, because the
  // screens' callbacks close over the instance this hook returned and
  // StrictMode does not re-render between cleanup and remount.
  const stopTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  useEffect(() => {
    // A synchronous remount (StrictMode) always runs before any timer fires —
    // there is no other way for this effect to run again. Cancel whatever the
    // previous cleanup scheduled so the terminal half never lands.
    if (stopTimerRef.current !== null) {
      clearTimeout(stopTimerRef.current);
      stopTimerRef.current = null;
    }

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
      // The session-ending half stays synchronous: a real unmount must flush
      // the final batch immediately, not a macrotask later.
      collector.finish();
      // Only the PERMANENT half — refusing every later begin() — is deferred.
      // finish() already made stop() a no-op past its `token = null` guard,
      // so by the time this fires (a real unmount, nothing cancelled it) it
      // does exactly one thing: flip the terminal flag.
      stopTimerRef.current = setTimeout(() => {
        stopTimerRef.current = null;
        collector.stop();
      }, 0);
    };
  }, [collector]);

  return collector;
}
