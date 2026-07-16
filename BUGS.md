# Known bugs

Confirmed, not-yet-fixed defects. Each entry says how it was found, what the
impact is, and where a fix would start. Remove entries when fixed (and move
anything durable they taught us into the relevant `docs/NN-*.md` gotchas).

## iPhone native fullscreen enters but shows a black video

- **Found**: 2026-07-16, first R16 U4 on-device pass (the predecessor bug —
  the button being a *silent no-op* — was fixed by R16 U1–U3 the same day:
  the tap now enters real native fullscreen).
- **Impact**: iPhone viewers using the fullscreen button get the system
  player over black instead of the stream; exiting returns to the working
  inline viewer.
- **Cause** (narrowed to three WebKit candidates, not yet distinguishable
  on-device — see the U4 findings section of
  [`docs/21-ios-video-fullscreen.md`](docs/21-ios-video-fullscreen.md)):
  black `VideoFrame`-from-WebGL-canvas readback, foreign broadcaster-clock
  PTS never presenting, or a paused (autoplay-rejected) element.
- **Fix shipped, unverified**: defenses for all three landed 2026-07-16
  (tee-local zero-based PTS, `preserveDrawingBuffer` on the tee'd context,
  gesture-context `play()` in the fullscreen toggle, plus an overlay
  "Native Fullscreen" diagnostics section that localizes any remaining
  blackness). Awaiting the on-device re-verify; remove this entry when it
  passes.

## Viewer "Streamer offline" card is misleading when the relay rejects the join

- **Found**: 2026-07-16, introduced knowingly with the viewer error-copy
  rework (raw transport errors like "Opening handshake failed." moved to the
  console; the card now shows friendly copy keyed on an error kind).
- **Impact**: a viewer whose *first* connect is rejected for capacity — the
  broadcast at its subscriber limit or the relay at `-max-subscribers`
  (R2 limits, HTTP 429) — sees "Streamer offline. No one is streaming at …"
  while the stream is in fact live. Same wrong story for a down or
  misconfigured relay (bad cert, wrong URL). With ~15 friends against the
  default caps this is rare, but when it happens the card points the viewer
  at the wrong cause (checking the code won't help; the console has the
  real error).
- **Cause** (confirmed, structural): `WebTransportError` exposes no HTTP
  status, so 404 (no such broadcast), 429 (full) and network/cert failures
  are indistinguishable in JS — the documented `viewer-session.ts` policy.
  The copy deliberately hedges toward the overwhelmingly common case
  (nobody live at that code).
- **Fix would start**: relay-side. Upgrade the session and *then* close with
  a dedicated close code per rejection reason (the post-upgrade GC race
  already does exactly this with 4000 "broadcast ended" in
  `handleSubscribe`), letting the viewer map each reason to honest copy
  (`errorCardCopy` in `ViewerScreen.tsx` is the single mapping point).
  Costs: an upgrade for every rejected join, new `wire` close codes on both
  sides, and rate-limit thought (an upgraded-then-closed session is more
  expensive than a pre-upgrade 404).

(The Chrome 152 `WebTransport.getStats()` entry was resolved 2026-07-14: not
a gawk defect — Chromium removed the API entirely; see the gotcha in
`docs/13-observability.md` D7 and the README gotcha list.)
