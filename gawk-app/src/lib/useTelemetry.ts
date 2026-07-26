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

  // Review finding 7 (R28 / PR #151): a deferred, cancellable stop() — the
  // same trick `useViewerConnection`'s worker-controller teardown uses for
  // the identical problem (README "R8 viewer-teardown gotcha"). gawk-app
  // renders <StrictMode>, whose dev-mode mount -> cleanup -> remount reuses
  // this SAME component instance and therefore this same `ref.current`: the
  // cleanup below used to call collector.stop() synchronously, which is
  // deliberately terminal (telemetry.ts) and made begin() a permanent no-op —
  // so the collector was already dead before the wire-0x0D hello ever had a
  // chance to arrive, and every dev session collected nothing.
  //
  // Recreating the collector instead (swap the ref, force a re-render) was
  // rejected: both consuming screens close over the value this hook returns
  // inside `useCallback`s keyed on it, and StrictMode's cleanup->remount does
  // NOT re-render the component in between — a swapped instance would leave
  // those callbacks bound forever to the OLD (correctly stopped, so silently
  // inert) collector, trading a visible dev-only failure for an invisible
  // one.
  //
  // A flat "cleanup is never terminal" split was also rejected: the viewer's
  // worker-sourced `onEvent` (unlike the main-thread path) is not gated by an
  // `active` flag, and its controller's own dispose is itself deferred a
  // macrotask (R8) — so on a REAL unmount there is a genuine window where a
  // late worker message (a telemetryHello included) could still reach
  // `begin()`. Terminal stop() is what closes that window; it must still
  // apply, just not before a synchronous StrictMode remount gets a chance to
  // cancel it.
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
