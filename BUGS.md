# Known bugs

Confirmed, not-yet-fixed defects. Each entry says how it was found, what the
impact is, and where a fix would start. Remove entries when fixed (and move
anything durable they taught us into the relevant `docs/NN-*.md` gotchas).

## Safari viewer: keyframe delivery stops while datagrams keep flowing

- **Found**: 2026-07-21, from a Safari 26.5.2 viewer's Copy-diagnostics
  capture (broadcast `JCJHF8`, macOS). Reported as "playback suddenly
  freezes; stats still show incoming frames, but nothing is decoded".
- **Impact**: the viewer freezes permanently mid-stream. It is not a
  disconnect — the session stays up, `viewerCount` is unchanged, and deltas
  keep arriving at full rate, so nothing on either end used to notice.
- **Signature** in the diagnostics: `keyframeStreamsReceived` frozen (232
  across the whole 9.5 s capture) with `lastKeyframeAgeMs` climbing 6.6 s →
  16.1 s, while `datagramsReceived`/`framesCompleted` climb normally at
  ~54 fps; `reorderKeyframeWaitDrops` climbing at the full frame rate (every
  delta ages out of the reorder buffer's waiting-for-keyframe state);
  `decodedFrames` frozen, `decoderQueueDepth` 0. `reorderBuffered` parks at
  ~50 ≈ 1 s of frames, i.e. exactly `KEYFRAME_WAIT_MS`. The reorder buffer is
  the victim here, behaving correctly; keyframes stop at the transport
  boundary (`viewer.ts` `handleKeyframeStream`, which is where the counter
  increments).
- **Cause — the freeze is mitigated, the trigger is NOT root-caused.**
  QUIC datagrams are not flow-controlled but streams are, so the stall is
  stream-path-specific: keyframes (reliable uni streams, R8) stop dead while
  datagrams (deltas) continue. Whether the wedge is exhausted connection-level
  receive credit on WebKit's side, or its incoming-uni-stream accept path, is
  **unconfirmed** — it needs a `/statusz` capture taken *while a Safari viewer
  is frozen*, which has not been done (the reported capture was hours stale by
  the time it was investigated, and the signature only exists live).
- **Confirmation procedure**: reproduce the freeze, then read that
  subscriber's entry in `/statusz` `subscriberDetails` on its serving pod.
  Stalled writes ⇒ `keyframesSent` flat while `keyframesDropped` climbs ~2/s,
  with broadcast-level `keyframeDrops.slow` climbing in step. If instead
  `keyframeDrops.superseded` climbs, the cause is upstream (publisher-side
  keyframe cadence) and this entry is misfiled.
- **Mitigation shipped** (2026-07-21, test-first, both layers): the relay
  evicts a subscriber after `hub.KeyframeSlowEvictThreshold` (10 ≈ 5 s)
  consecutive keyframes whose stream *opened* but whose write stalled — the
  half of unreachability the R10 `KeyframeOpenFailEvictThreshold` streak
  cannot see, because it resets on every successful open. Same non-terminal
  4001 close code, so a live client reconnects. Plus a viewer-side backstop
  (`viewer.ts` `checkKeyframeStall`, `KEYFRAME_STALL_MS` 8 s) for the case the
  relay cannot see, deliberately longer than the relay's own remedy; it fires
  only while frames are still arriving, so a broadcaster who merely stepped
  away never trips it. **Both recover playback; neither explains WebKit's
  behavior.** Remove this entry when the trigger is root-caused (and move what
  it taught us into the relevant `docs/NN-*.md`).
- **Not extended to R19 carrier streams on purpose**: a stalled carrier tail
  is deliberately dropped at GOP granularity in resilient mode (docs/24
  "drops-over-stalls"), so feeding those write stalls into an eviction streak
  would disconnect healthy mobile viewers.

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
- **Verdict (2026-07-19): native fullscreen is not viable on iPhone —
  remove tier 2, ship pseudo-fullscreen.** The pre-registered clone-tee fix
  landed 2026-07-16 (the tee writes **clones of the decoded frames it
  presents** — `new VideoFrame(frame, { timestamp })`, no canvas readback
  anywhere — plus an overlay "Content sample" peak-RGB row to separate black
  frame content from a black native player). The **third on-device pass
  (2026-07-19) was still black** with the clone tee: since the tee no longer
  touches the WebGL canvas, the operative cause is the one the pre-registered
  criteria name — the native `webkitEnterFullscreen` player cannot present a
  locally generated `MediaStreamTrack` (`VideoTrackGenerator` output) on iOS
  WebKit. **Remaining fix**: a code cleanup deleting the tier-2 tee /
  generator / hidden-`<video>` path in the viewer so the black native player
  is never reached (the gate/probe already fall back to pseudo-fullscreen);
  until that lands, an iPhone that passes the tee probe still hits the black
  native player. Worth an upstream WebKit report. Remove this entry when the
  cleanup ships. See docs/21 "U4 findings" (third pass).

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
