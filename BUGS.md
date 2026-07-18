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
- **Cause** (narrowed over two on-device passes — see the U4 findings
  section of
  [`docs/21-ios-video-fullscreen.md`](docs/21-ios-video-fullscreen.md)):
  pass 1 shipped defenses for three WebKit candidates (tee-local PTS,
  `preserveDrawingBuffer`, gesture-context `play()` + element diagnostics);
  pass 2 showed frames flowing end-to-end (tee climbing, element playing
  and presenting) yet still black — ruling the `VideoFrame`-from-WebGL-
  canvas readback content the operative cause (black even with
  `preserveDrawingBuffer`).
- **Fix shipped, unverified**: the pre-registered next step landed
  2026-07-16 — the tee now writes **clones of the decoded frames it
  presents** (`new VideoFrame(frame, { timestamp })`, no canvas readback
  anywhere; interpolated mid-blends no longer cross — fullscreen shows real
  frames at paced cadence), plus an overlay "Content sample" (peak-RGB)
  row that finally separates black frame content from a black native
  player. Awaiting the third on-device pass; if *still* black with a high
  Content sample, the pre-registered verdict is: the native player can't
  present locally generated MediaStreams on this WebKit → remove tier 2,
  ship pseudo-fullscreen. Remove this entry when either lands.

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

## Broadcaster screen shows LIVE indefinitely if the broadcast worker dies silently

- **Found**: 2026-07-19, by inspection while fixing the start-failure
  stranding bug (a `start()` rejection after capture left the LIVE stage
  rendered with no error card — fixed the same day, test-first, in
  `BroadcasterScreen.tsx` / `BroadcasterScreen.test.tsx`). This is the
  rarer cousin; not yet reproduced.
- **Impact**: if the broadcast Web Worker dies without delivering a message
  (hard crash, renderer OOM kill), no `'error'`/`'ended'` ever reaches
  `WorkerBroadcastSession`, so the screen keeps claiming LIVE with a frozen
  preview while nothing is being sent. Not a hard strand: pressing Stop
  recovers via the session's 3 s `STOP_TIMEOUT_MS` force-teardown — but
  nothing tells the broadcaster to press it.
- **Cause** (confirmed by inspection): `WorkerBroadcastSession` installs
  only `worker.onmessage`; the `onerror` handler from `waitForBoot`
  resolves an already-settled promise post-boot (a no-op). And a killed
  worker fires no event at all — `onerror` only covers unhandled
  exceptions — so detection needs a liveness signal, not just a handler.
- **Fix would start**: `workerBroadcastSession.ts` — a post-boot
  `worker.onerror` mapping to `onError` + teardown, plus a stats-silence
  watchdog (the pipeline posts stats every 500 ms while started; several
  seconds of silence ⇒ treat the worker as dead: surface the error,
  terminate, fire `onEnded`). The screen's `onError`/`onEnded` handling
  already lands on the right card.

(The Chrome 152 `WebTransport.getStats()` entry was resolved 2026-07-14: not
a gawk defect — Chromium removed the API entirely; see the gotcha in
`docs/13-observability.md` D7 and the README gotcha list.)
