# Known bugs

Confirmed, not-yet-fixed defects. Each entry says how it was found, what the
impact is, and where a fix would start. Remove entries when fixed (and move
anything durable they taught us into the relevant `docs/NN-*.md` gotchas).

## Broadcaster worker offload never engages on macOS — `Pipeline: Main thread` in every browser

- **Found**: 2026-08-03, running R11's overdue manual browser verification on
  macOS. The broadcaster stats overlay reads `Pipeline: Main thread` in every
  browser tried on that machine. R11 shipped 2026-07-14 with green automated
  gates and the manual verify never happened, so this has plausibly been true
  since the milestone landed.
- **Impact**: the whole point of R11 is lost on macOS — capture, encode and
  send run on the main thread, so UI work and encoder work contend for it.
  This is the failure mode R11 exists to remove (docs/16). Not a display
  glitch and not cosmetic: in the worker path the stats object is constructed
  *inside* the worker, where `window` is undefined, and
  `broadcaster.ts` sets `pipelineContext` from exactly that
  (`typeof window === 'undefined' ? 'worker' : 'main-thread'`), and
  `WorkerBroadcastSession` forwards the worker's own stats verbatim
  (`workerBroadcastSession.ts` `case 'stats'`). A reading of `main-thread`
  therefore means `createBroadcastSession` returned the plain
  `BroadcastPipeline` — the worker path was genuinely not taken.
- **Not yet distinguished**: which gate rejected it. `createBroadcastSession`
  falls back silently through four independent ones, and the fallback is
  *correct* behaviour for some browsers, so part of this report may be
  expected rather than defective:
  - `canAttemptWorker()` — `Worker`, `document`, `HTMLCanvasElement`,
    `HTMLCanvasElement.prototype.captureStream`;
  - the worker's boot handshake (`broadcaster.worker.ts`) — needs
    `VideoEncoder`, `WebTransport`, `MediaStreamTrackProcessor` **and**
    `OffscreenCanvas` *in worker scope*;
  - `waitForBoot`'s timeout;
  - `probeTrackTransfer` — a `DataCloneError` on transferring a
    `MediaStreamTrack`.
  **Safari and Firefox are expected to fail here** and fall back by design —
  neither has `MediaStreamTrackProcessor`, and CLAUDE.md already records that
  as the reason for the Firefox `<video>` + `rVFC` path. So the report's
  load-bearing half is **Chrome on macOS**, which does have MSTP and should
  have taken the worker path. Confirm the browser list before assuming a
  defect: if only Safari/Firefox were tried, there is no bug here and this
  entry should be closed as expected behaviour.
- **Fix would start**: by making the fallback say *why*. The four gates
  currently fail silently apart from `probeTrackTransfer`'s one `log.info`,
  which is why an on-hardware pass can see the symptom and not the cause.
  Have `createBroadcastSession` record the rejecting gate and surface it next
  to the overlay's `Pipeline` row (a reason string, the way R32's disabled
  preset rows carry theirs). That turns this and every future recurrence into
  a one-glance diagnosis. Then re-run on Chrome/macOS and, if a real gate is
  wrongly rejecting, fix that gate test-first per CODE-REVIEW.md.
- **Untested on the viewer side**: `useViewerConnection`'s `canUseWorker` is a
  *different* gate list (`Worker` + `OffscreenCanvas` +
  `transferControlToOffscreen`), and the viewer overlay has its own `Pipeline`
  row. Whether it also reads `Main thread` on the same machine was not
  checked, and it would not have the same cause if it does.

## Every WebKit viewer fails to join since the webtransport-go draft-16 bump

- **Found**: 2026-08-03, from a live report — broadcast `DDP4H7` showed
  "Streamer offline" on macOS Safari while Firefox on the same machine, same
  network, same minute played it fine. Reproduced on iPhone Safari too.
- **Impact**: **every Safari/WebKit viewer on the fleet, macOS and iOS, cannot
  join any broadcast**, and the error card tells them the streamer is offline
  (see the misleading-card entry below — this is its worst instance). Silent
  since the relay rolled `0.24.1` on 2026-08-02. The whole iOS audience is cut
  off, which also makes any R22 `MF5` iOS verification fail for reasons that
  have nothing to do with R22.
- **Symptom, from the viewer**: `viewer error (unreachable)` with an empty
  message ~144–278 ms after load. A direct dial in the Safari console gives
  `WebTransportError`, `source: "session"`, `streamErrorCode: null`, and an
  empty `message` on both `ready` and `closed`.
- **Cause — strong, one link unproven.** `chore(deps): update quic-go` (#131,
  merged 2026-07-30) moved `webtransport-go` v0.11.1 → v0.12.0, which is a
  **WebTransport draft version change**, not a patch: v0.11.1's README says it
  implements draft-15, v0.12.0's says draft-16. With it, the server stopped
  advertising draft-15's three session flow-control SETTINGS —
  `settingsWebTransportInitialMaxStreamsUni`, `…MaxStreamsBidi` and
  `…InitialMaxData`, all `1 << 60` (the `AdditionalSettings` map shrank from 6
  entries to 3) — in favour of draft-16's capsule-based flow control. A
  draft-15 client sees no session flow-control credit and cannot establish the
  session. `settingsEnableWebtransportDraft06` (`0x2b603742`) is unchanged, so
  the failure is in session establishment, not feature detection.
  **What is not proven**: that Safari 26.5.2 implements draft-15 specifically.
  Everything else here is read from the two module versions and the timeline.
- **Why the relay logged nothing** — and this is the fact that ruled out every
  other hypothesis: the failure happens during H3 SETTINGS / session
  negotiation, *upstream* of `CheckOrigin` and route dispatch. So there is no
  `origin rejected`, no `subscribe rejected pre-upgrade`, no session line, and
  the counters stay clean (`gawk_origin_rejected_total 0`,
  `gawk_rate_limited_total 0`). A viewer that cannot join leaves no trace at
  all.
- **Ruled out**, each checked against the live fleet on 2026-08-03: 404/429
  (the only `not_found`s that day were another client, hours earlier, with
  different IDs); the Origin allowlist; the per-IP rate limiter; subscriber
  caps; an empty `config.relayUrl` (real, fixed separately, but the
  `gawk.ioio.fi` hostname fallback resolves correctly); iCloud Private Relay
  (off, and toggling it changed nothing); macOS Local Network permission
  (neither browser holds a grant, and Firefox works); and certificate trust —
  a QUIC dial from the same Mac shows the relay serving the full 3-cert chain
  (leaf → LE `YR1` → `ISRG Root YR` cross-signed by `ISRG Root X1`) and the
  macOS system trust store verifies it. Worth keeping in mind separately:
  `ISRG Root YR` is **not** in the macOS trust store, so Apple clients depend
  entirely on that cross-sign continuing to be served.
- **Timeline**: macOS Safari 26.5.2 connected and streamed for minutes on
  2026-07-21/22 (the two Safari entries below are built on those captures).
  Same Safari version fails now. The bump merged 2026-07-30; the relay
  deployed 2026-08-02.
- **Confirmation procedure**: run a relay pinned to `webtransport-go v0.11.1`
  and point Safari at it. Recovery confirms the draft version is the trigger;
  no recovery means the draft change is a red herring and the quic-go
  v0.60.0 → v0.61.0 half of the same bump needs the same treatment.
- **Fix would start**: with the decision of which draft to serve, which is a
  product decision, not a dependency one — draft-16 is where the ecosystem is
  going and Safari will get there. Options are pinning back until WebKit
  catches up, or serving both drafts if webtransport-go can. Whichever is
  chosen, the durable lesson is that **`webtransport-go`'s minor version is a
  wire-compatibility surface**: it must not arrive as an unattended Renovate
  bump, and CI cannot catch this because the e2e suite has no WebKit client.
  Constrain it in `renovate.json5` and add a WebKit reachability check before
  trusting a future bump.

## Windows broadcaster: viewers black for the first minute (keyframe supersede livelock)

- **Found**: 2026-07-31, first working field broadcast from the RTX 2070
  machine (the F-8→F-10 sequence fixed; the broadcast itself ran fine
  after the first minute). Viewers saw a black screen for roughly the
  first minute after start.
- **Cause — confirmed** from broadcaster telemetry: capture/encode flat at
  60 fps, but `keyframeStreamsSuperseded` climbing ~2/s while
  `keyframeStreamsSent` landed ~1 per 15–20 s. quinn packs datagrams into
  every packet ahead of stream data, so the delta flood starves the
  keyframe uni stream; a write then outlives the 500 ms GOP and D5's
  unconditional "newest wins" cancelled it — a livelock, worst at cold
  start. The Linux/quic-go broadcaster does not exhibit it (825 sent / 26
  superseded in the comparison session). Full mechanism: docs/38 F-12.
- **Fix landed** (docs/38 F-12): in-flight keyframe writes younger than
  2 s always complete; the newest keyframe waits in a one-deep pending
  slot; older writes are still cancelled (wedge protection). A per-minute
  send-health line now lands in debug.log. **Field confirmation pending**
  — remove this entry once a fresh 2070 broadcast primes viewers within
  ~1–2 s of joining (watch `keyframe streams … sent/superseded` in the
  health lines: sent should track ~2/s, supersedes near zero on an idle
  uplink, and bounded under load).
- Related open question, same machine: the broadcaster's telemetry session
  only began reporting ~36 min into the broadcast (collector session
  record `8faea12c…` starts 20:29:30Z for a ~19:54Z broadcast). Possibly a
  restart mid-broadcast; if a fresh session shows the same late start,
  investigate the Windows reporter's begin path (TelemetryHello handling).

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
  - *Why WebKit surfaces nothing.* Needs a reduced repro (a page whose session
    the server closes abruptly). Now that the watchdog recovers playback this
    is an upstream bug report rather than an outage, and it is what would let
    the entry above this one finally close too.
  - *Resilient mode's cost on good networks.* It doubles the uni-stream open
    rate to ~4/s/viewer and, on this desktop Safari, delivered 12 % queue
    overflow — it was designed for LTE phones. Whether to relax the per-GOP
    carrier rotation or warn on the toggle should wait on the two items above.
- **Carrier drain backpressure addressed (2026-07-23, test-first, relay only;
  docs/24 finding 17).** The `carrierQueueOverflow` 3100-against-22 987 (12 %)
  measured above had three separate causes, all now bounded: the queue was only
  0.65 s deep (`QueueDepth` 256 → **1024**, ≈2.6 s — it counts datagrams, and a
  1080p frame is ~13 of them); audio was dequeued by the video drain, so one
  parked carrier write froze it for up to `CarrierWriteTimeout` (audio now has
  its own queue and `drainAudioSideband` goroutine); and overflow was handled
  per packet, so a holed GOP kept thrashing the queue and writing records the
  viewer was guaranteed to discard (the GOP is now marked dead, its queued
  deltas purged and later ones shed until the next rotation, with control
  datagrams — including the ViewerCount keepalive this entry's watchdog depends
  on — exempt). `carrierQueueOverflow` now counts **one per dead GOP** instead
  of one per packet, so the metric answers "how many GOPs did this viewer
  lose". **This does not explain the parked writes** — see the note below.
- **Still not root-caused, and the most interesting lead**: a carrier write
  parks when the *viewer* stops reading the stream. That is suspiciously close
  to the WebKit stream-path wedge in the entry above, and would also explain
  why the edge pod serving a different viewer had **zero** overflow across
  372 289 records. If that link holds, the backpressure was never an
  independent relay bug — it was the same Safari problem seen from the server
  side, and the relay work above bounds the damage rather than fixing a cause.
- **Reproduction/confirmation kit**: the paired capture is the whole point —
  a client capture alone cannot distinguish "relay stopped sending" from
  "WebKit stopped delivering", and a relay capture taken minutes later shows
  nothing because the subscriber is already gone. Poll every pod (the frozen
  viewer may be origin- or edge-served; here it was origin while a healthy
  viewer sat on the edge) and take the client's Copy-diagnostics in the same
  minute, before touching the tab.

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

## iPhone native fullscreen: the MSE playhead has ~100 ms of cushion that cannot grow

- **Found**: 2026-07-26, investigating the first on-device MF5 pass ("plays
  sometimes for a second, sometimes for 10, but always pauses"). Diagnosed by
  code reading, not yet reproduced against an instrumented build — the two
  *other* causes found in the same investigation (docs/27 findings 1 and 2, no
  `duration = Infinity` and no muxed audio) are fixed, and they may mask this
  one by turning an underrun into a recoverable stall instead of a dead player.
- **Impact**: iPhone native fullscreen only (tier 2). The inline canvas path is
  untouched and has no equivalent failure mode. Expect micro-stalls under normal
  arrival jitter, worst in **live delivery mode** and during recovery in
  resilient / Deep buffer mode.
- **Cause**: `LIVE_EDGE_REJOIN_S` is `0.1` — and the cushion it sets is the
  whole budget for the session, because the muxer is fed from `decodeReleased`,
  i.e. **after** the reorder buffer's paced release. Appends therefore arrive at
  playback rate: the SourceBuffer can never build ahead while the element plays,
  so `bufferedAhead` is a constant of the motion that only a seek (shrink) or a
  pause (grow) changes. 100 ms is below the arrival jitter measured in this
  project's own field captures (72–158 ms, docs/20 finding 6), and
  `MsePresenter.maybeCatchUp` re-creates the condition every time lag passes
  `LIVE_CATCHUP_LAG_S` (2 s) — which is exactly the state a manual play leaves.
- **Relation to the delivery modes** (worth not re-deriving): a deeper playout
  buffer (R19 resilient, R21 Deep buffer) makes the append stream *smoother*, so
  the cushion drains more slowly — but it adds **zero** cushion, because the
  delay is spent upstream of the release point and the output is still 1×. And
  during a DVR catch-up burst, when real cushion does arrive for free,
  `maybeCatchUp` throws it away by seeking back to `end − 0.1`.
- **Signature**: overlay/Copy-diagnostics `presentationSurface.bufferedAheadMs`
  sitting at ~100 ms and `elementPaused` flipping true with no `appendErrors`
  and `bufferedRanges === 1` (a hole would be the *other* open entry below).
- **Fix would start**: one shared `TARGET_LAG_S` (~0.4–0.6 s) replacing the
  constant that is currently **duplicated** in `lib/useFullscreen.ts`
  (`seekToLiveEdge`) and `features/viewer/msePresentation.ts`
  (`maybeCatchUp`) — a drift hazard in its own right. Scaling it by delivery
  mode is the natural refinement (live small, resilient ~1 s, Deep buffer
  larger): it is pure fullscreen-only latency, and those viewers have already
  bought latency for smoothness. Since docs/27 finding 2 muxed audio onto the
  same timeline, raising it no longer costs lip-sync. See docs/27 "Still open".

## iPhone native fullscreen: nothing recovers a stalled presentation element

- **Found**: 2026-07-26, same investigation. Diagnosed by code reading.
- **Impact**: iPhone native fullscreen only. Any stall that does happen is
  permanent until the user taps the native play button — which is what made
  docs/27 finding 1 so visible, and what would make the entry above visible too.
- **Cause**: no listener for `waiting` / `stalled` / `ended` / unexpected
  `pause` exists on the presentation `<video>`; the only listeners are
  `webkitbeginfullscreen`/`webkitendfullscreen` (`lib/useFullscreen.ts`) and
  rVFC frame counting (`ViewerScreen.tsx`). WebKit pauses and fires `ended` on
  an MSE buffer underrun where Chromium stalls and resumes (docs/27 finding 1),
  so "the element recovers itself" is not an assumption that holds here.
- **Fix would start**: a tier-2-only watchdog — on those events, if the playhead
  is behind the newest buffered range, seek into it and `play()` again, counting
  recoveries into `presentationSurface` so a recovering-but-unhealthy player is
  distinguishable from a healthy one. This is what production players ship
  (hls.js's gap-controller nudges, seeks over holes, and re-plays). It is also
  the general remedy for the hole entry below, which is why it is worth doing
  even if the cushion fix alone appears to settle the symptom.

## iPhone native fullscreen: MMS parking can leave holes in the buffered timeline

- **Found**: 2026-07-26, same investigation. Diagnosed by code reading; the
  frequency depends on Apple's undocumented `ManagedMediaSource` water marks, so
  it is unquantified. **Not CI-catchable**: Chrome has no `ManagedMediaSource`
  at all, so `pump()`'s `streaming === false` branch never runs there and the
  `--muxer-check` step cannot exercise this (docs/27 Decision 10's coverage
  boundary). It needs an on-device capture.
- **CORRECTION (2026-07-26, on-device pass 2)**: the 4 holes actually measured on
  the iPhone were **not** this. `segmentsAppended == muxInitSegments +
  muxMediaSegments` with `appendErrors: 0` proves nothing was shed by the queue,
  so those holes were in the *timeline*, not in what got appended — the video
  sample-duration bug, now fixed by one-frame lookahead (docs/27 finding 3
  revised). This entry stays open because the mechanism below is still reachable
  and still unmeasured; it is no longer supported by that capture.
- **Impact**: iPhone native fullscreen only. A hole in the buffered timeline
  stalls the native player at the hole — and because
  `HTMLMediaElement.buffered` is the **intersection** of the SourceBuffers'
  ranges, a hole in either track (video or the muxed audio) does it.
- **Cause**: `MsePresenter.pump()` parks on `streaming === false`. While armed
  the element is *paused* (docs/27 Decision 5), so MMS's buffered-ahead only
  grows and it stops asking for data; meanwhile live segments accumulate in the
  JS queue and front-drop past `MAX_QUEUED_SEGMENTS` (128 ≈ 2–4 s). Any arming
  period longer than the queue depth therefore leaves a gap between the last
  appended segment and the keyframe appends resume at.
- **Partly fixed 2026-07-26 (docs/27 finding 7)**: parking before the element
  has media was a *deadlock*, not a hole — MMS asks for data when the element
  needs more, and an element with no init segment needs nothing, so a source
  that opened with `streaming` false never took a byte and native fullscreen
  was unreachable for the whole session. Priming (SourceBuffer, init, first
  keyframes) now goes through regardless of `streaming`; parking resumes once
  the element reports playable media. **This entry stays open** for the
  steady-state case above, which is unchanged: a fed, paused element still
  parks and the queue still front-drops.
- **Signature**, and how to tell it from the (fixed) sample-duration cause:
  `bufferedRanges > 1` with `segmentsAppended` **lagging** `muxInitSegments +
  muxMediaSegments` — the shortfall is the queue's front-drops. Where those two
  agree, the holes are not this.
- **Fix would start**: either keep the armed playhead following the buffered
  edge so MMS never parks and the buffer stays bounded, or treat
  resume-after-park as a discontinuity — discard the stale backlog, re-anchor,
  and seek the element into the newest range. The watchdog above covers the
  symptom generically; this entry is about not manufacturing the hole.

## Resilient/Deep-buffer viewers lose video entirely to the Safari stream wedge

- **Found**: 2026-07-26, from an iPhone (iOS 18.7 / Safari 26.5.2) Copy-
  diagnostics capture on broadcast `FUHNMG`, reported as "video shows, until it
  freezes" while audio kept playing. A severity note on the two Safari entries at
  the top of this file rather than a new root cause.
- **Impact**: in **resilient** or **Deep buffer** delivery mode, a wedged stream
  path stops video completely (not the awaiting-keyframe slideshow the default
  mode degrades to). In resilient mode audio continues, so the viewer looks
  frozen with sound; **in Deep buffer nothing survives** — see the correction
  below.
- **Why the mode matters**: R19 moved video *deltas* onto reliable carrier
  streams, and keyframes were already on reliable uni streams (R8). QUIC
  datagrams are not flow-controlled but streams are — so when WebKit's stream
  path wedges, every video path is gone at once.
- **CORRECTION (2026-07-26, second iPhone capture, broadcast `XN73GU`, Deep
  buffer)**: "audio deliberately stayed on datagrams (docs/20 field finding 5)"
  is true of **resilient** mode only. R21 DV5 gives a DVR subscriber its own
  audio *carrier stream* (`hub/dvr_audio.go` `drainDVRAudio` → `writeAudioRecord`),
  precisely so audio is not head-of-line blocked behind video deltas — so in Deep
  buffer **100 % of media rides streams** and a wedge is a total blackout, not a
  freeze with sound. Only the control sideband (ClockMapping, the R18 keepalive,
  DecoderConfig) stays on datagrams (`hub/dvr.go` `drainControlSideband`,
  `hub.go` `drain`). The capture shows the split exactly: `carrierStreams: 89` for
  88 keyframes — 88 per-GOP video carriers plus the one long-lived audio carrier —
  and the last two inbound datagrams of the session were 6 B and 10 B, a
  `ViewerCount` and a `ClockMapping`.
- **Signature**: every media counter frozen across a whole capture window — in
  `XN73GU` `datagramsReceived` moved **+2 in 9.5 s** — while the *sideband* keeps
  landing, so `timeSinceLastInboundMs` resets every ≤5 s for as long as the relay
  still has the session. Both worker-side counters (`carrierRecords`, from the
  nested transport worker) and viewer-worker ones (`framesCompleted`,
  `decodedFrames`) freeze together, which places the wedge at or below WebKit's
  QUIC layer rather than in decode, render or the R22 muxer (whose
  `segmentsAppended` reconciled exactly: `1 + 2748 + 3484`, zero errors — it was
  starved, not broken).
- **Why the freeze ran 31 s, and what now ends it** (fixed 2026-07-26,
  test-first, both layers):
  - *Viewer — media-stall watchdog* (`viewer.ts` `checkMediaStall`,
    `MEDIA_STALL_MS` 6 s). This is the gap the two older watchdogs left between
    them: `checkKeyframeStall` requires frames to still be arriving (this failure
    removes them) and `checkSessionStall` requires total silence (the DVR control
    sideband holds it off *by design* — `drainControlSideband`'s comment says so).
    Two signals gate it, and the second is the one that took a design correction
    mid-implementation:
    1. The broadcaster's own **ClockMapping** — published every 5 s only while
       capturing, and datagram-borne, so it survives the wedge. Two of them
       *since the last media* rather than one is what makes it safe against the
       away transition, where the publisher's final periodic mapping can land
       just after its last frame (and the relay replays a cached mapping to
       every joiner).
    2. **Audio must have stopped too.** "No video while mappings arrive" is
       *not* sound on its own: screen capture is damage-driven and stops
       entirely on a static screen (docs/19, docs/28), so a paused game would
       have made this watchdog reconnect-loop — and docs/30 already says that
       even *telling* the user a static screen looks frozen is worse than
       silence. Audio is not damage-driven, so silence on both media at once
       cannot be produced by a static screen; it takes a transport failure.
    **Uncovered, deliberately**: a broadcast with no audio at all (nothing
    client-side distinguishes its wedge from its static screen), and a
    *resilient*-mode wedge, where audio rides datagrams and keeps arriving —
    which is also exactly what a static screen looks like. Closing either needs
    the relay to say "I have video for you and cannot deliver it", i.e. a new
    sideband wire type (0x0D+ is free); that is the natural next step here and
    would make the client-side heuristics redundant.
  - *Relay — the DVR progress timeout is 6 s, not 30* (`DefaultDVRProgressTimeout`),
    and it no longer counts idleness as failure. `drainDVR` parks on the ring's
    wake channel when the cursor is caught up, and the stall check is only
    re-evaluated when an append wakes it — so with an away broadcaster the stamp
    aged through the whole absence and the *returning* broadcaster's first frame
    evicted every Deep-buffer viewer on the pod. `dvrNoteIdle` (both park sites,
    noted after the wait) confines the timer to "the ring holds data this cursor
    cannot take", which is what made 6 s safe.
  - *Relay — DVR keyframe-stream opens feed the shared eviction streak.*
    `dvrSendKeyframe` opens its own streams and never touched
    `kfConsecOpenFailed`, so the exhausted-stream-credit zombie the live path has
    evicted since R10 was counted (`kfDroppedOpenFailed`) and ignored in this mode.
    One open per GOP is the cadence `KeyframeOpenFailEvictThreshold` was sized
    against. **Deliberately not extended to the audio carrier**: `writeAudioRecord`
    retries every `dvrRetryFloor` (20 ms), so feeding that into a 10-strike streak
    would evict on a 200 ms transient; the video path sees the same credit
    exhaustion at a defensible cadence.
- **Still not fixed here**: why WebKit wedges. Both remedies bound the damage to
  ~6 s and a reconnect; neither explains the trigger, and a `/statusz` capture
  taken **while frozen** is still the missing evidence. What this capture adds to
  that hunt: Deep buffer opens ~4 uni streams/s/viewer (keyframe + carrier per
  500 ms GOP), the highest stream-open rate any mode asks of the browser, and
  `carrierStreamsAborted` stayed at 1 throughout the blackout — the client
  surfaced neither new streams nor resets, consistent with an accept-path or
  stream-credit wedge rather than with the relay having stopped writing.

## MSE presentation buffers 20+ s of high-resolution media on a phone

- **Found**: 2026-07-26, same capture: `bufferedMs: 21225` at **3360×2100**
  (7 MP), decoded twice on the device — inline WebCodecs plus the native player.
- **Impact**: memory pressure on the one platform that can least afford it.
  Unproven as a cause of anything, but it is a plausible contributor to WebKit
  tearing down the media pipeline mid-session, which is the failure above.
- **Cause**: `PRUNE_TRIGGER_S` (30 s, keep 10 s) was sized as "generous — the
  point is boundedness over a long session", without frames this large in mind,
  and `ManagedMediaSource` did not evict on its own either.
- **Fix would start**: scale the prune trigger by frame area rather than time
  alone (a fixed byte budget is the honest bound), and consider whether the armed
  element needs 10 s of history at all — the fullscreen player only ever seeks
  forward, toward live.

## Lint advisories in `gawk-broadcast/internal/mpegts` (not runtime defects)

- **Found**: 2026-07-24, from the editor linter while touching `mpegts.go`'s
  package comment during the docs-freshness pass. Pre-existing, not introduced
  by that change.
- **Impact**: cosmetic / hygiene. Neither affects the shipped demuxer's
  behavior; recorded here so they aren't rediscovered as "new" each time the
  package is edited.
- **The two advisories**:
  - `mpegts.go:138` (`Demuxer.resync`) — `for i := 0; i < len(p); i++` can be
    modernized to `for i := range len(p)` (`rangeint`). Purely stylistic.
  - `mpegts_test.go:17` — const `fixtureGOPFrames = 15` is **unused**. It is
    defined with a comment describing the fixture's GOP-15 (500 ms @ 30 fps)
    structure but no test references it, so it reads as either a leftover or a
    missing assertion.
- **Fix would start**: modernize the `resync` loop; and decide whether
  `fixtureGOPFrames` should back a real test — e.g. assert IDR access units
  recur every 15 frames in the demuxed fixture — or just be removed. The latter
  is trivial; the former is more valuable (it gives the demuxer a keyframe-
  spacing assertion it currently lacks).

## Long-lived publisher connections die on a source-address change (fleet LB)

- **Found**: 2026-07-27, investigating broadcast `DE6G6P` — a native
  broadcaster's publisher session ended after 78 minutes with reason
  `context canceled`, and the relay garbage-collected the broadcast 60 s later
  with three viewers attached. Nothing on the fleet died at that instant, the
  relay initiated nothing (no drain, takeover, lease loss or eviction), and the
  session was not closed cleanly (that logs `EOF`, as the other two
  broadcasters on the fleet did).
- **Impact**: a broadcast drops for every viewer. Since 2026-07-27 the native
  broadcaster auto-resumes (docs/19 Decision 21), so the visible cost is now a
  ~1 s interruption rather than a dead broadcast — but the drops themselves
  still happen, and the browser broadcaster's R17 auto-resume was already
  masking them the same way.
- **Hypothesis, not a diagnosis**: the relay Service is a UDP `LoadBalancer`
  with `externalTrafficPolicy: Local` and `sessionAffinity: None`, behind
  MetalLB in **BGP** mode — so ECMP hashes the 5-tuple to a node and etp=Local
  pins it to that node's pod. Any change in the client's source IP:port (home
  NAT rebind, CGNAT remap, DHCP renew, a few seconds of uplink loss) re-hashes
  the flow to a *different* pod, which holds no state for that connection ID
  and — because `GAWK_STATELESS_RESET_KEY` is set fleet-wide (R17 W1) —
  answers with a stateless reset. Dead in ~1 RTT, silently. QUIC connection
  migration cannot help: it covers the client's address changing while talking
  to the same server, and here the server changes identity. A 78-minute
  residential session is exactly the exposure window. This is the flip side of
  the etp=Local trade docs/22 finding 10 took for BGP mode (real client IPs +
  ECMP spread), and it is not recorded there.
- **Competing explanation**, equally consistent with the evidence: a plain
  >30 s uplink drop hitting `-max-idle-timeout` on both ends.
- **Why it was undiagnosable**: the relay could not report the real reason —
  see the `context canceled` gotcha in the README. That is **fixed** as of
  2026-07-27 (`internal/transport/endreason.go`), so the *next* occurrence
  should log `timeout: no recent network activity` (idle timeout) or a
  stateless-reset error, which discriminates the two hypotheses outright.
- **Fix would start**: with the next captured reason. If it is a stateless
  reset, the options are `sessionAffinity: ClientIP` (which does not survive a
  source-port change either, so probably not), accepting it and relying on
  auto-resume, or routing publishers differently from viewers. Do not change
  the LB shape speculatively — docs/22 finding 10 chose etp=Local deliberately.


(The Chrome 152 `WebTransport.getStats()` entry was resolved 2026-07-14: not
a gawk defect — Chromium removed the API entirely; see the gotcha in
`docs/13-observability.md` D7 and the README gotcha list.)
