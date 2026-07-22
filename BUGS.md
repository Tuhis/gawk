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
- **Related, and now confirmed**: the second variant below (the whole session
  dies and the viewer never notices) was captured client+server simultaneously
  on 2026-07-22. It is a different failure from this one — there the session is
  *gone*, here it stays up — but the two share an ending, and the viewer-side
  watchdog listed as this entry's mitigation cannot fire for either when
  nothing is arriving.

## Safari viewer holds a dead session forever — no error, no reconnect

- **Found**: 2026-07-22, from the **first successfully paired capture** of this
  class: relay `/statusz` across all three pods at **23:10:31 UTC** and the
  frozen viewer's Copy-diagnostics at **23:10:42 UTC** — 11 s apart, i.e.
  simultaneous. Safari 26.5.2 on macOS, broadcast `9MX2GZ`, R19 resilient mode
  (`deliveryMode: "reliable"`), worker pipeline + WebGL sink. An earlier
  unpaired capture the same evening (22:46 UTC, same broadcast and browser)
  shows the same ending and is the same bug.
- **Impact**: the viewer freezes permanently and **silently**. No error card,
  no reconnect attempt, no "broadcast ended" — the UI keeps rendering a stale
  frame and stale stats (`audioState: "active"`, a `viewerCount` that has not
  updated in a minute) while the transport underneath is dead. The only way out
  is a manual page reload, and nothing tells the viewer to do that.
- **Signature (client)**: *everything* stops on the same tick — not just the
  stream path. Across a 9.5 s capture `datagramsReceived` (27731),
  `audioPacketsReceived` (4908), `keyframeStreamsReceived` (183),
  `carrierStreams` (110) and `carrierRecords` (22725) are all frozen, while
  `timeSinceLastFrameMs` == `lastKeyframeAgeMs` climbs 38.9 s → 48.5 s. The
  last frame had arrived at 23:10:04 UTC, ~27 s before the relay was polled.
  `carrierStreamsAborted` is 0 — the carrier was not reset, it simply went
  quiet. Contrast the entry above, where datagrams keep flowing.
- **Signature (relay, same moment)**: **the session does not exist.**
  `reliableSubscribers: 0` on all three pods; the only live viewer in the fleet
  was a *second*, non-reliable viewer on the edge pod being served normally
  (~740 datagrams/s, 3 keyframe streams/s, ~5.4 Mbps, `dropped` /
  `sendErrors` / `keyframesDropped` / `queueDepth` all 0). The frozen viewer's
  own session had already been removed.
- **Identifying the corpse**: the origin pod still carried that departed
  reliable subscriber's broadcast-level counters — `carrierStreams` 128 vs the
  client's 110, `carrierRecords` 22987 vs 22725. The relay wrote 18 more
  streams and 262 more records than the client ever saw, which both identifies
  the session and dates its death.
- **Cause (confirmed at the boundary, not below it)**: the relay ended or lost
  the session, and **WebKit surfaced no signal to the app** — `wt.closed` never
  resolved and no read loop rejected. Those two are the viewer's *only*
  session-end signals (`viewer-transport.ts`, `LocalViewerTransport`), so the
  pipeline had nothing to react to. Why WebKit stays silent is not root-caused;
  that it stays silent is now established.
- **Why both shipped mitigations are inert here**:
  - *Relay side*: it had already dropped the subscriber. Even if the ending was
    a `KeyframeOpenFailEvictThreshold` eviction, 4001 is deliberately
    non-terminal so the client can reconnect — and the client never heard it.
  - *Viewer side*: `viewer.ts` `checkKeyframeStall` is guarded by
    `now - lastFrame > FRAMES_FLOWING_WINDOW_MS → return` (1 s). It only fires
    *while frames are still arriving*. Nothing was arriving, so the watchdog
    disqualified itself and sat out a 48 s freeze past its own 8 s threshold.
    In resilient mode this is structural rather than incidental — all video
    rides the stream path there, so a stream-path wedge stops `lastFrame` too.
  - *No independent liveness probe*: `timeSyncRttMs` and `capToRenderMs` were
    `null` for the entire session in **both** captures — the R5 TimeSync
    round trip never completed once on this Safari. The ping write failure is
    swallowed by `.catch(() => {})` in `viewer-transport.ts`, so it fails
    silently and the viewer has no signal of its own to gate a watchdog on.
- **The session was unhealthy well before it died** (origin pod, that
  subscriber's broadcast): `carrierQueueOverflow` 3100 against 22987 delivered
  records — a **12 % overflow rate** on the 256-deep queue, i.e. `drainReliable`
  chronically could not keep up with fan-out — plus `carrierRecordsDropped`
  1866, `sendErrors` 272 (datagram writes, which is the audio sideband), and
  `keyframeDrops.openFailed` 15 against an eviction threshold of 10
  *consecutive*. Whether those 15 were consecutive is not provable from
  cumulative counters, so the exact ending (4001 eviction vs. QUIC death)
  stays inferred. The 22:46 capture's earlier "video drops to 2 fps" phase —
  keyframes arriving at the 500 ms GOP rate with no deltas — is the same
  backpressure seen from the client side.
- **Fix would start**: viewer-side, and it is the higher-value half. Replace
  `checkKeyframeStall`'s "frames still arriving" guard with a real liveness
  signal, so a viewer can notice its own dead session. The guard exists to
  avoid disconnecting a viewer whose broadcaster merely stepped away
  (docs/05 D1 keepalive), but that case is distinguishable without requiring
  *frames*: a live session still has the relay's QUIC keepalive, audio
  datagrams, or a TimeSync pong. Making TimeSync's failure visible instead of
  swallowed is a prerequisite for gating on it. The relay-side backpressure
  above is worth fixing too, but it would only have made this session
  healthier, not recoverable — a client that cannot detect a dead session will
  freeze on the next cause as well.
- **Reproduction/confirmation kit**: the paired capture is the whole point —
  a client capture alone cannot distinguish "relay stopped sending" from
  "WebKit stopped delivering", and a relay capture taken minutes later shows
  nothing because the subscriber is already gone. Poll every pod (the frozen
  viewer may be origin- or edge-served; here it was origin while a healthy
  viewer sat on the edge) and take the client's Copy-diagnostics in the same
  minute, before touching the tab.

## Viewer diagnostics: audio counters are cumulative across sessions

- **Found**: 2026-07-22, while reading the captures for the entry above.
- **Impact**: the `audioBuffer` block cannot be compared against anything else
  in the same Copy-diagnostics JSON, which sends readers down false trails —
  `overflowDrops` 12816 against `audioPacketsDecoded` 4908 reads as a wild
  accounting bug rather than what it is.
- **Cause** (confirmed): `AudioSink` deliberately outlives individual sessions
  (`useViewerConnection.ts`: "The sink outlives individual sessions"), and
  `AudioJitterBuffer.flush()` increments `resets` but zeroes none of the other
  stats. So every `audioBuffer` counter is cumulative over the whole page view
  including reconnects, while every other counter in the file is per-attempt.
  `resets: 11` in the 23:10 capture says how many re-anchors it had absorbed.
- **Two smaller reporting defects found alongside**:
  - `videoBytesReceived` includes the audio sideband. In the 22:46 capture its
    per-sample delta equals `audioBytesReceived`'s exactly (8688, 8715, 8135,
    8457, 1020) once video had stopped. "Video bitrate (recv)" is overstated
    whenever audio is on.
  - After the stream dies the worklet reports an underrun every 128-sample
    quantum forever (+188 per 500 ms ≈ 375/s; `underruns` reached 300 386),
    each one driving `noteUnderrun` → re-prime. Harmless, but it destroys the
    counter's value as a severity measure and churns the port.
- **Fix would start**: reset the `AudioJitterBuffer` stats on `flush()` (or
  report them per-attempt alongside a cumulative pair), split audio bytes out
  of `videoBytesReceived`, and stop counting underruns when no audio is
  expected.

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
