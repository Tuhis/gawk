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
- **Carrier *writes* are still not fed to an eviction streak, on purpose**: a
  stalled carrier tail is deliberately dropped at GOP granularity in resilient
  mode (docs/24 "drops-over-stalls"), so counting those write stalls would
  disconnect healthy mobile viewers. Carrier *opens* are a different signal and
  got their own streak on 2026-07-23 — see the entry below.
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
- **Mitigation shipped (2026-07-23, test-first, both layers). The freeze now
  recovers; WebKit's silence is still not root-caused, which is why this entry
  stays open.**
  - *Viewer — dead-session watchdog* (`viewer.ts` `checkSessionStall`,
    `SESSION_STALL_MS` 15 s). Fires on total inbound silence rather than on
    missing keyframes, so it works precisely where `checkKeyframeStall` cannot.
    Routed through the same `fail()` → `ViewerSession` reconnect path, so it is
    reconnectable, not terminal. It arms on the first inbound byte, never at
    connect: a viewer joining an away broadcast has legitimately received
    nothing yet.
  - *Relay — the count keepalive no longer stops when the broadcaster does*
    (`hub.go` `PumpViewerCounts`). This is what makes the watchdog sound rather
    than merely aggressive: the pump used to skip hubs with no active
    publisher, so "session dead" and "broadcaster stepped away" were
    indistinguishable on the wire, and any silence-based watchdog had to choose
    which one to get wrong. A live session now always carries the R18
    ViewerCount at least every `ViewerCountKeepalive` (5 s), so 15 s of silence
    is three missed keepalives and unambiguous. **Version-skew note**: a new
    viewer against a pre-2026-07-23 relay will reconnect every ~15 s while the
    broadcaster is away, until the relay is upgraded — bounded by the rollout,
    and strictly better than the freeze it replaces.
  - *Relay — carrier opens get their own eviction streak*
    (`CarrierOpenFailEvictThreshold`). `openCarrier` incremented the keyframe
    streak, which every successful `sendKeyframe` open zeroed — so "carrier
    never opens, keyframes always do" was structurally un-evictable and
    presented as a stable, silent 2 fps with the relay reporting a healthy
    subscriber. Carrier *write* failures are still deliberately not counted
    (dropping a stalled GOP's tail is the mode working as designed). Note the
    behavior change: a subscriber failing *both* kinds now takes a full
    threshold of GOPs rather than half of one, since the streaks no longer
    share a counter.
  - *Viewer — TimeSync send failures are no longer swallowed*
    (`viewer-transport.ts`). `timeSyncRttMs` read `null` for the whole life of
    both captured sessions with no clue why, because the ping write's rejection
    was discarded by a bare `.catch(() => {})`. Logged once per session now
    (still non-fatal — a failed ping must never take the pipeline down).
    **Landed without a test, deliberately**: there is no `viewer-transport`
    harness and standing one up to assert a log line is disproportionate; per
    CODE-REVIEW's escape clause, recorded here instead. Surfacing it in
    Copy-diagnostics needs plumbing through the worker stats boundary and was
    not done.
  - *Viewer — `timeSinceLastInboundMs`* added to `ViewerStats` and the overlay
    ("Last inbound"). This is the row that separates the two failures in a
    future capture: it resets every ≤5 s on a live session and climbs without
    bound on a dead one.
- **Diagnostics defects fixed at the same time** (found while reading these
  captures, all three shipped 2026-07-23, test-first):
  - `AudioJitterBuffer.flush()` now zeroes its counters and keeps only
    `resets`. The sink deliberately outlives sessions
    (`useViewerConnection.ts`), so those counters were the one
    cumulative-across-reconnects block in an otherwise per-attempt file — which
    is how the 23:10 capture reported 12 816 overflow drops against 4 908
    decoded packets, a comparison that reads as a wild accounting bug and is
    really two time bases.
  - `videoBytesReceived` no longer counts audio datagrams. In the 22:46
    capture its per-sample delta equalled `audioBytesReceived`'s exactly (8688,
    8715, 8135, 8457, 1020) once video had stopped, so "Video bitrate (recv)"
    was overstated by the whole audio lane whenever audio was on.
  - Worklet underruns are no longer counted once no audio is arriving
    (`AudioSink`, 1 s expectation window). After the stream died the worklet
    reported a dry quantum ~375×/s forever — `underruns` reached 300 386 —
    burying the counter's real signal. The re-prime side effect still runs, so
    audio that resumes rebuilds its cushion (docs/20 field finding 6).
- **Reading the 2026-07-22 captures**: they predate all of the above. Their
  `audioBuffer` block is cumulative across reconnects, their
  `videoBytesReceived` includes audio, their `underruns` are inflated by the
  post-death spin, and they have no `timeSinceLastInboundMs` row.
- **Deliberately NOT fixed, and why**:
  - *The carrier drain backpressure* (`carrierQueueOverflow` 3100 = 12 %).
    This is the most likely proximate cause of the session ending and it is
    still open. `drainReliable` is one goroutine doing per-record framing, the
    write, the deadline and the audio sideband, and the fix is a choice between
    batching records per write, moving audio off that goroutine, and retuning
    `QueueDepth`/`CarrierWriteTimeout` — a choice that should follow a
    measurement, not a guess. The pointed question for that work: the edge pod
    had **zero** overflow across 372 289 records while the origin had 3100
    across 22 987, so this is not inherent to the mode.
  - *Why WebKit surfaces nothing.* Needs a reduced repro (a page whose session
    the server closes abruptly). Now that the watchdog recovers playback this
    is an upstream bug report rather than an outage, and it is what would let
    the entry above this one finally close too.
  - *Resilient mode's cost on good networks.* It doubles the uni-stream open
    rate to ~4/s/viewer and, on this desktop Safari, delivered 12 % queue
    overflow — it was designed for LTE phones. Whether to relax the per-GOP
    carrier rotation or warn on the toggle should wait on the two items above.
- **Reproduction/confirmation kit**: the paired capture is the whole point —
  a client capture alone cannot distinguish "relay stopped sending" from
  "WebKit stopped delivering", and a relay capture taken minutes later shows
  nothing because the subscriber is already gone. Poll every pod (the frozen
  viewer may be origin- or edge-served; here it was origin while a healthy
  viewer sat on the edge) and take the client's Copy-diagnostics in the same
  minute, before touching the tab.

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
