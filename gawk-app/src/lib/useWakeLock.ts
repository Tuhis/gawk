import { useEffect } from 'react';
import { log } from './logger';

// Screen Wake Lock — keeps the display awake while a stream is running.
//
// Why gawk needs it explicitly: browsers hold a display power-save blocker
// only while an HTMLMediaElement is *playing video*. Neither of our surfaces
// is one. The viewer decodes with WebCodecs and paints VideoFrames onto a
// canvas (R8/R10 render sinks — the only <video> in the viewer is the R22 MSE
// presentation element, which exists on iPhone alone); the broadcaster's
// preview is a muted local <video> that the OS has no reason to treat as
// consumption. So the page looks idle, and the OS runs its normal idle timer:
// on macOS the display dims a couple of minutes in and then sleeps, mid-stream.
// navigator.wakeLock is the standard opt-out for exactly this case (canvas and
// WebGL players are the API's motivating example).
//
// Two rules the API's shape imposes, both covered by useWakeLock.test.ts:
//
//  1. The UA **auto-releases the sentinel whenever the document becomes
//     hidden** and never re-acquires it. Without the visibilitychange
//     re-request below, one tab switch would silently lose the lock for the
//     rest of the stream — the classic way this feature ships half-working.
//  2. request() is async and rejects at the UA's discretion (battery saver, a
//     document that lost visibility mid-flight). A refusal must degrade to
//     today's behaviour, never break the surface; and a sentinel that arrives
//     after teardown must be released, or the lock outlives the broadcast.
//
// Deliberately no listener on the sentinel's own 'release' event: the only
// release we can *act* on is the visibility one (it tells us when to ask
// again), and a UA that drops the lock for its own reasons is a battery
// decision we should not fight — the next visibility flip re-asks anyway.
//
// Support: Chrome 84+, Safari 16.4+, Firefox 126+. Secure-context only, which
// WebTransport already requires of every gawk surface.

// Structural, not the lib.dom types: we depend on exactly the two members we
// call, so the hook compiles the same whether or not the TS DOM lib in use
// declares WakeLock.
interface WakeLockSentinelLike {
  release: () => Promise<void>;
}

interface WakeLockLike {
  request: (type: 'screen') => Promise<WakeLockSentinelLike>;
}

function wakeLockApi(): WakeLockLike | null {
  const api = (navigator as unknown as { wakeLock?: WakeLockLike }).wakeLock;
  return api && typeof api.request === 'function' ? api : null;
}

/**
 * Holds a screen wake lock for as long as `enabled` is true and the document
 * is visible. Inert (zero requests, no listeners) when disabled or where the
 * API is absent.
 */
export function useWakeLock(enabled: boolean): void {
  useEffect(() => {
    if (!enabled) return;
    const api = wakeLockApi();
    if (!api) return;

    let active = true;
    // `pending` is what keeps a burst of visibility events from stacking
    // locks: the sentinel is not observable until the request resolves, so
    // `sentinel` alone cannot answer "are we already asking?".
    let pending = false;
    let sentinel: WakeLockSentinelLike | null = null;

    const acquire = () => {
      if (!active || pending || sentinel) return;
      // Requesting while hidden is a guaranteed rejection; wait for the flip.
      if (document.visibilityState !== 'visible') return;
      pending = true;
      void api.request('screen').then(
        (s) => {
          pending = false;
          // Torn down (or hidden again) while the request was in flight — the
          // sentinel is ours to release and nobody else will.
          if (!active || document.visibilityState !== 'visible') {
            void s.release().catch(() => {});
            return;
          }
          sentinel = s;
        },
        (err) => {
          pending = false;
          // Battery saver and friends. Degraded, not broken — and the next
          // visibility change asks again.
          log.info('screen wake lock refused', err);
        },
      );
    };

    const onVisibility = () => {
      // Rule 1: on hide the UA has already released whatever we held, so drop
      // the stale reference either way and re-ask once we are back.
      sentinel = null;
      acquire();
    };

    acquire();
    document.addEventListener('visibilitychange', onVisibility);

    return () => {
      active = false;
      document.removeEventListener('visibilitychange', onVisibility);
      const held = sentinel;
      sentinel = null;
      if (held) void held.release().catch(() => {});
    };
  }, [enabled]);
}
