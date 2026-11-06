# Known bugs

Confirmed, not-yet-fixed defects. Each entry says how it was found, what the
impact is, and where a fix would start. Remove entries when fixed (and move
anything durable they taught us into the relevant `docs/NN-*.md` gotchas).

## iOS UI tests fail on the iOS 27.2 simulator (CI pinned to 27.1)

- **Found**: 2026-11-06, on PR #729's `xcode (iPhone)` job. GitHub's macOS
  runner image `20261103.629` added iOS 27.2 simulators, and `ios.yml`'s
  `Pick a simulator` step took the newest iOS 27 runtime. Every iPhone run on
  that image fails the same way, on main too (run 42083194691, 7fc677aa).
  Every run on the older image `20261021.618` passes.
- **What fails**: `AccessibilityUITests.testThePlayers`, with 8× "Hit area is
  too small" for the player header's "1 watching", "Copy link to …" and
  "Server 127.0.0.1" labels, on the Player, Stats and Room player screens.
  Also `BroadcastRoomUITests.testLeaveReturnsToNotInARoom` at line 135: after
  Leave, the "Not in a room" button doesn't appear in time.
- **Impact**: CI only, for now. `ios.yml` pins the picker to iOS 27.0–27.1
  (`MAX_MINOR`). Users on iOS 27.2 likely see the same small hit areas, and
  possibly the room-leave behaviour.
- **Fix would start**: run the UI tests on a 27.2 simulator. Give those
  labels a ≥ 44 pt hit area, or make them non-interactive if nothing should
  tap them. Find out what 27.2 changed in the leave flow (timing or a real
  state bug). Then remove the pin.

## Room header squeezed into the top-left corner while the phone sheet is open

- **Found**: 2026-11-06, in `e2e/voice-panel.mjs`'s `in-voice-360.png` while
  fixing the room footer's narrow layout.
- **What**: below 720 px the side panel is a full-width bottom sheet, but
  `.header[data-panel='true']` still sets `right: var(--panel-w)` (the
  desktop column's width), so with the sheet open the header is ~40 px wide
  and its room code, counts and icons pile up over one another at the top
  left. `.center` and `.footer` reset this on phones; the header does not.
- **Impact**: cosmetic while the sheet is open; the header's controls are
  hard to hit until it closes.
- **Fix would start**: the `@media (max-width: 719px)` rules in
  `room.module.css` (`right: 0` for `.header[data-panel='true']`), with a
  header check beside the footer one in `e2e/voice-fake.mjs` `overflowAt`.

## Voice panel head overflows at 1280 px in `e2e/voice-panel.mjs`

- **Found**: 2026-11-06: `node e2e/voice-panel.mjs` fails 4 of 4 runs on main
  at 32437418 with `horizontal overflow at 1280px` in the panel head
  (`panelHead`, `panelHeadActions`, the pin button, sometimes the
  Leave voice row). The harness is not in CI (only `voice-rnnoise.mjs` is).
- **What**: unconfirmed whether it is real or the harness: the check runs
  400 ms after resizing from 430 to 1280 px, when the phone sheet turns back
  into the pinned column (which slides in), so it may be catching the slide.
- **Fix would start**: take a screenshot at the moment of the failure; if the
  column is mid-slide, wait for its animation to end before the check.

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
  Since 2026-11-06 the same entry carries `undeliverableNotices`: a
  non-zero value means the relay watched that session's keyframe hand-offs
  move **zero bytes** for 3 s (docs/24 finding 19), which is the server-side
  half of the signature in one field — capture it before the viewer
  reconnects, which it now does about a second after the second notice.
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
- **Lead from the Deep-buffer captures (2026-07-26, iPhone, `FUHNMG` and
  `XN73GU`)**: in Resilient and Deep buffer the wedge is a total video
  blackout, not this entry's awaiting-keyframe slideshow, because every video
  path rides streams there. Deep buffer opens ~4 uni streams/s/viewer
  (keyframe + carrier per 500 ms GOP), the highest stream-open rate any mode
  asks of the browser, and `carrierStreamsAborted` stayed at 1 through the
  blackout: the client surfaced neither new streams nor resets. That fits an
  accept-path or stream-credit wedge, not a relay that stopped writing. The
  blackout itself is now bounded in every mode, audio or not, by the relay's
  undeliverable-video notice (docs/24 finding 19; it replaced the separate
  "Resilient/Deep-buffer viewers lose video entirely" entry, whose remaining
  gaps it closed).
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
    `null` for the entire session in **both** captures. The R5 TimeSync
    round trip never completed once on this Safari, so the viewer has no
    signal of its own to gate a watchdog on. **Cause found 2026-09-24:** no ping
    was ever sent. WebKit has no `datagrams.writable` (only
    `createWritable()`), and the viewer skipped its TimeSync writer when the
    attribute was missing. Fixed by `openDatagramWriter()`; see
    `docs/gotchas.md`. With pings flowing, the RTT signal a watchdog could gate
    on now exists on Safari. The watchdog itself is still unbuilt, and the
    missing ping explains none of the WebKit silence above.
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


## iPhone native fullscreen: the MSE playhead has ~100 ms of cushion that cannot grow

- **Found**: 2026-07-26, investigating the first on-device MF5 pass ("plays
  sometimes for a second, sometimes for 10, but always pauses"). Diagnosed by
  code reading, not reproduced against an instrumented build.
- **Impact**: iPhone native fullscreen only (tier 2). Micro-stalls under normal
  arrival jitter, worst in live delivery mode and during recovery in
  resilient / Deep buffer mode.
- **Cause**: the duplicated `LIVE_EDGE_REJOIN_S = 0.1` set the playhead's
  cushion for the whole session — the muxer is fed after the reorder buffer's
  paced release, so appends arrive at 1× and the SourceBuffer cannot build
  ahead while the element plays. 0.1 s is below the 72–158 ms arrival jitter
  of docs/20 finding 6, and `maybeCatchUp` re-created it (seeking to
  `end − 0.1`) on every catch-up, including during a DVR catch-up burst.
  Full mechanism: docs/27 "Still open", item 3.
- **Fix landed** (docs/27 item 3): one shared policy, `gawk-app/src/lib/liveEdge.ts`,
  used by both the entry seek (`useFullscreen`) and `MsePresenter.maybeCatchUp`.
  The target lag is per delivery mode — live 0.5 s, resilient 1.0 s, Deep
  buffer 1.5 s — and the catch-up trigger sits 1.5 s above it (live keeps
  its 2 s). Every seek lands at the target lag, never nearer the end; the
  entry seek also backs a playhead pinned at the end off to it. Unit-tested
  only. **On-device confirmation pending** — remove this entry once an iPhone
  native-fullscreen session shows, in overlay/Copy-diagnostics
  `presentationSurface`:
  - `bufferedAheadMs` sitting near the mode's target (≈500 ms on Balanced /
    Lowest latency, ≈1000 ms on Smoother, ≈1500 ms on Most stable) after
    entry and after any catch-up, no longer pinned at ~100 ms;
  - `elementPaused` staying false through a minute or more of normal
    playback, with `appendErrors: 0` and `bufferedRanges === 1`.
  - after a stall recovery (`stallRecoveries` > 0), `bufferedAheadMs` also
    back near the target. The watchdog waits for the cushion before
    `play()`, but gives up after the target plus 1 s (`CUSHION_GRACE_MS`) in
    case MMS stops feeding a paused element; a thin cushion right after a
    recovery means it hit that bound, and `endstreaming` is the signal to
    look at next.
  If `bufferedAheadMs` holds the target but the element still pauses, the
  cause is the MMS-parking entry below, not this one.

## iPhone native fullscreen: MMS parking can leave holes in the buffered timeline

- **Status**: **fix landed 2026-11-06, on-device confirmation pending** — kept
  here until an iPhone capture shows it, because the mechanism was diagnosed by
  code reading and never measured (the 4 holes of on-device pass 2 were the
  since-fixed sample-duration bug, docs/27 finding 3 revised).
- **Mechanism**: `MsePresenter.pump()` parks on `streaming === false`. While armed
  the element is *paused* (docs/27 Decision 5), so MMS stops asking for data while
  live segments keep arriving and overflow `MAX_QUEUED_SEGMENTS` (128 ≈ 2–4 s);
  the shed segments leave a gap between the last appended frame and the resume
  keyframe, which stalls the native player (`buffered` is the tracks'
  intersection, so a hole in either track does it). iPhone native fullscreen only.
- **Fix** (docs/27, "Resume-after-park is a discontinuity"): overflow discards the
  backlog up to the newest queued keyframe and marks the track discontinuous; once
  the resume keyframe is buffered, the playhead is seeked into the newest range
  and each track's older ranges are removed. Covered by unit tests with a fake
  MMS; **not CI-catchable end to end** — Chrome has no `ManagedMediaSource`, so
  `--muxer-check` never runs the `streaming === false` branch (docs/27
  Decision 10).
- **Backstop**: should a hole still form, the stall watchdog (docs/27 "Stall
  watchdog") seeks over it into the newest range, so it costs a recovery, not
  the session.
- **Remove this entry when** an on-device pass arms the surface (join, don't tap
  fullscreen) for longer than the queue depth with MMS parked, then taps
  fullscreen, and the capture shows `bufferedRanges` staying **1** throughout —
  with `discontinuitiesHealed > 0` wherever `segmentsDroppedOverflow > 0`, which
  proves the parked-overflow path was actually exercised rather than never hit.
  If instead `bufferedRanges > 1` with `segmentsDroppedOverflow` 0, the holes have
  another cause.

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
  see the `context canceled` gotcha in [`docs/gotchas.md`](docs/gotchas.md).
  That is **fixed** as of 2026-07-27
  (`internal/transport/endreason.go`), so the *next* occurrence
  should log `timeout: no recent network activity` (idle timeout) or a
  stateless-reset error, which discriminates the two hypotheses outright.
- **Fix would start**: with the next captured reason. If it is a stateless
  reset, the options are `sessionAffinity: ClientIP` (which does not survive a
  source-port change either, so probably not), accepting it and relying on
  auto-resume, or routing publishers differently from viewers. Do not change
  the LB shape speculatively — docs/22 finding 10 chose etp=Local deliberately.

## Smoother and Most stable viewers freeze on uplink loss that Balanced viewers never see

- **Found**: 2026-10-17, docs/75 F14, by replaying lab traces through the
  shipping viewer code in each preset (§11 table J). Not yet seen on the
  fleet; the mechanism is read in code.
- **Cause**: reliable delivery (R19 *Smoother*, R21 *Most stable*) has no
  loss allowance in its reorder buffer, and was served no parity. A chunk
  lost on the broadcaster's uplink is missing at the relay, so the carrier
  cannot retransmit it, and the viewer freezes to the next keyframe.
- **Partly fixed** (2026-10-20, docs/75 RU8, D11): the relay now serves
  these viewers parity on the carrier and keeps it in the DVR ring, which
  repairs the random-loss half (§11 table K: 32–89 % → 95.6–100 % at
  0.1–1 % loss). Still open: bursts (70–72 % → 81–84 % with parity, where
  *Balanced* shows 90–93 %) and outages (70–77 %, unchanged), where a burst
  or outage takes more of a frame than parity covers.
- **Impact**: under bursts and outages on a datagram uplink, these presets
  freeze where the carrier uplink would show them 94–100 % (F14).
- **Fix would start**: docs/75 RU3 (the carrier uplink) shipped in the
  native broadcasters on 2026-10-21 and has been their default delivery
  since 2026-11-05 (docs/75 D4), so their viewers are covered unless the
  broadcaster picks *Lowest latency*. Still open for the browser
  broadcaster, which has no carrier until its own Reliable milestone
  (docs/75 OD5), and for native broadcasters on *Lowest latency* or on a
  relay without carriers.

## Desktop broadcaster: every stream reset mid-`finish()` leaks a quinn waiter

- **Found**: 2026-10-23, reviewing a report of memory growth on the Windows
  broadcaster after R73. Found by reading the code, not by measuring it.
- **Mechanism**: wtransport's `SendStream::finish()` is quinn's `finish()`
  followed by `stopped()`, and `stopped()` inserts a
  `(StreamId, Arc<Notify>)` into the connection's `stopped` map. Only a
  `Finished` or `Stopped` stream event removes it. When the engine drops the
  `finish()` future and resets the stream instead, quinn-proto frees the
  stream on the RESET_STREAM ack without raising either event, and neither
  `reset()` nor `Drop` touches the map. So the entry stays until the
  connection closes. Two paths reach this: a carrier that misses
  `CARRIER_RETIRE_DEADLINE` (`carrier.rs` `retire`, counted as
  `uplinkCarriersExpired`), and a keyframe write cancelled while finishing
  (`transport.rs` `WtKeyframeWriter`). Still present in quinn 0.11.12.
- **Impact**: small. Each entry is about 100 bytes and there are at most
  about two per second (one per GOP), which caps it at roughly 0.7 MB per
  hour of a broadcast whose carriers all expire. A clean uplink rarely
  expires one. It does not explain growth of tens of MB.
- **Fix would start**: upstream in quinn, where `SendStream::reset`/`Drop`
  (or the reset-acked path) would clear the stream's `stopped` entry. The
  engine cannot reach that map. Waiting on acknowledgement some other way is
  not possible either, because `stopped()` is quinn's only ack signal
  (`docs/gotchas.md`, "quinn cannot tell you which bytes …").

(The "Viewer latency metrics emit session-age values" entry was resolved
2026-09-25 by #376: a frame whose parity arrived before its data kept
timestamp 0, so the metrics measured from the broadcaster tab's time origin.
The "relay drops high-rung publisher datagrams" entry was resolved
2026-10-21/22 by docs/75 RU2, #536 and #550: a 512-deep queue in the
vendored quic-go.)

(The "Telemetry SQL console: the `rollups` view rots after boot, and any
unpruned `sessions` query OOMs" entry was resolved 2026-09-22: views
re-register on drift, and the engine runs inside a stated memory, thread
and spill budget. A view probe now exports `gawk_telemetry_sql_view_up` for
alerting; see the gotchas in [`docs/gotchas.md`](docs/gotchas.md).)

(The Chrome 152 `WebTransport.getStats()` entry was resolved 2026-07-14: not
a gawk defect — Chromium removed the API entirely; see the gotcha in
`docs/13-observability.md` D7 and [`docs/gotchas.md`](docs/gotchas.md).)
