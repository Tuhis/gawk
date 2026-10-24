# R76 — Stop ends the broadcast

**Status**: implemented 2026-10-24 in one PR, chunks **BE1–BE6** (§6).
Owner decisions taken 2026-10-24 (§2; OD3–OD5 in PR review).

**Relationship to earlier work**: the relay holds a broadcast through its
grace period whenever the publisher's session ends (docs/06, default 5
minutes). That covers a broadcaster who is away: a network drop, a relay
restart, a reload, desktop Pause (docs/64 D9). It also covers a
broadcaster who pressed Stop, because the relay can't tell the two apart.
This milestone gives the publisher a way to say which one it is.

---

## 1. Why, and what "done" means

### The problem (owner report, 2026-10-24)

> When I stop broadcasting, the stream hangs for viewers until it gets
> cleaned up. When I as a broadcaster press stop, the stream should truly
> stop without a static image left lingering.

Every broadcaster closes its publish session with code 0, or with no code
at all. The relay's publish loop logs "publisher session ended" and the
deferred `Publisher.Close` arms the grace timer. Viewers keep their
sessions and their decoder, and the canvas holds the last frame. Five
minutes later the GC closes them with 4000 and they see "Broadcast ended".

### The measurement (2026-10-24)

docs/59 found that the relay's close codes almost never reach Chrome
(1 of 84). The opposite direction had not been measured. A probe server on
the relay's stack (quic-go v0.62.0 via `third_party/quic-go`,
webtransport-go v0.13.0, a dev certificate pinned by hash) took sessions
from Playwright's Chromium 1234 on loopback. After the datagram loop ended,
the probe read the session's close error (`OpenUniStream` returns it) and
recorded whether it was a remote `SessionError` with the code the page sent.

| Page did | Trials | Remote code 4014 read by the relay |
|---|---|---|
| 10 datagrams, then `close({closeCode: 4014})` | 30 | 30 |
| 300 × 1100 B datagrams queued, then `close({closeCode: 4014})` | 30 | 30 |
| uni stream written, not awaited, then `close({closeCode: 4014})` | 30 | 30 (and the stream's bytes arrived in all 30) |
| uni stream written and awaited, then plain `close()` | 30 | 0, code 0 in all 30 (control) |
| the same from a dedicated worker, terminated once it reported the close | 30 | 30 |
| the same from a dedicated worker, terminated in the task after the stop was posted | 30 | 30 |

Chrome writes `WT_CLOSE_SESSION` and FINs the CONNECT stream. quic-go reads
the capsule, and `webtransport-go` records it as the session's close error
before it cancels the session context. The loss-free loopback is the best
case. On a lossy path the capsule rides the reliable CONNECT stream, so it
is retransmitted for as long as the connection lives. Chrome keeps the
connection open after `close()` for at least that long, because the session
is one of the connection's streams. A page that is unloaded at the same
instant can lose it. §4 covers that case.

The worker rows are the path a Chromium broadcaster with the R11 worker
offload takes. `WorkerBroadcastSession` terminates the worker right after
it reports the stop, and Chrome still sends the capsule: the session lives
in the network service, not in the worker.

`ReceiveDatagram` returned `EOF`, not `context.Canceled`, in every trial.
The session's close error is the authoritative copy of the code, not the
datagram loop's error.

**A client that closes its QUIC connection with the session loses the
code.** webtransport-go's `Transport.Dial` closes the connection as soon as
its session ends (`context.AfterFunc` on the session context). Its
CONNECTION_CLOSE then usually beats the close capsule: in the relay's own
test, 25 of 30 runs read "Application error 0x100 (remote)" and no 4014.
quinn's `Connection::close` likewise drops unsent stream data. Chrome keeps
the connection, which is why the table above is clean. The relay tests dial
a publisher over a connection they own (`dialStoppablePublisher`), and the
native engine waits for the capsule to be delivered before it closes the
connection (BE6).

### Milestone acceptance criteria

| Goal | Verified by |
|---|---|
| Pressing Stop in the browser ends the broadcast for its viewers at once, with no frozen frame | `transport/stop_test.go` and `hub/stop_test.go` `TestPublisherStopEndsTheBroadcastNow`; `broadcaster.test.ts` stop tests; `ViewerScreen.test.tsx` |
| The viewer shows "Broadcast ended", and if the page stays open it resumes by itself when a broadcast restarts on the same code | `viewer-session.test.ts` stopped-then-resumed tests; `useViewerConnection` / `ViewerScreen` tests |
| Desktop End and iOS Stop end the broadcast; desktop Pause, quality and source restarts, quitting and every failure keep the grace | engine `session.rs` tests; shell tests |
| A restart on the same code (web Stop → Start, desktop End's undo) recreates the broadcast | `TestPublisherStopThenReclaimRecreates` |
| A deposed or late session's 4014 never ends its successor | `TestStaleStopDoesNotEndTheSuccessor` |
| Only a publish session's 4014 counts | `TestViewerStopCodeEndsNothing` |
| Viewers on edge pods are told 4014, not the terminal 4000 | `TestEdgeViewerIsToldTheOriginsStop` |
| A LAN-party broadcast's outside viewers are told 4014, and so are party viewers of a home broadcast (D11) | `TestOutsideViewerIsToldThePartyBroadcasterStopped`, `TestPartyViewerOfAHomeBroadcastIsToldItStopped` |
| An iOS user stopping the recording ends the broadcast; a capture failure keeps the grace | `CaptureTests` `testAUserStoppedRecordingEndsTheBroadcast`, `testACaptureFailureKeepsTheGrace` |
| A room drops the stopped stream's tile, and the broadcaster's restart puts it back | the stop fires `OnBroadcastExpired` (`hub/stop_test.go`), which is the existing room expiry path, and the existing re-attach tests |
| The bus says why: `broadcast.ended` with reason `stopped` | `hub/stop_test.go`; the event schema's enum; `gawk-admin` `TestStoppedEndSaysTheBroadcasterStopped` |
| The three wire mirrors agree on 4014 | close-code pins in `wire_test.go`, `wire.test.ts`, `crates/wire/tests/golden.rs` |

---

## 2. Owner decisions (2026-10-24)

| # | Question | Decision |
|---|---|---|
| OD1 | Which stops end the broadcast? | **Every deliberate stop on every client**: the web Stop button, desktop End, iOS Stop. Desktop Pause and the same-code restarts keep the grace, so viewers wait. A closed tab, a crash or a network loss still falls back to the grace. |
| OD2 | What does a viewer see, and does it come back? | Viewers are released and see "Broadcast ended". **If the tab or room stays open, a new broadcast on the same code resumes by itself.** |
| OD3 | Desktop End from Paused has no session to close. Reconnect briefly to send 4014? | **Yes**: the short reclaim to end is fine (D9). |
| OD4 | Does the iOS user stopping the screen recording from the system UI count? | **Yes**: stopping the recording is a stop (D9). |
| OD5 | Should outside viewers of a LAN-party (PoP) broadcast wait out the home grace? | **No**: they get the stop like everyone else (D11). |

---

## 3. Decisions

| # | Decision | Why |
|---|---|---|
| D1 | **One new close code, `4014 CloseCodeBroadcasterStopped`, used in both directions.** A publisher closes its own session with 4014 when its user stops on purpose. The relay then closes every subscriber of that broadcast with 4014. Allocated after R74's 4012/4013. | The meaning is the same at both ends: the broadcaster stopped on purpose. 4000 can't carry it, because 4000 is terminal for viewers (OD2 needs them to come back) and terminal for publishers. |
| D2 | **The relay ends the broadcast when the session's close error is a remote `SessionError` with code 4014.** It reads the close error after the datagram loop returns, waiting up to `connCauseWait` for the session context (the endreason.go pattern). The publish handler releases its publisher slot, then calls `Registry.StopBroadcast(pub)`. That removes the hub through the shared `removeBroadcast` body with code 4014, so the caches, DVR rings, grace timer, Lease (`OnBroadcastExpired`), rooms and counters all go the way a GC takes them. | One removal body, as `TerminateBroadcast` requires. The close error is what the measurement showed to be reliable. |
| D3 | **A stop ends only the session that sent it.** `StopBroadcast` takes the `*Publisher`, and removes the hub only when that publisher is still the hub's latest one, by the publisher pointer and the hub generation. A session deposed by a token-bearing reclaim (4004), or a hub already re-claimed by the time a late close is read, is left alone. | "Newest publisher wins" (docs/06) must not be undone by the loser's goodbye. |
| D4 | **4014 is a client-chosen code, so the relay accepts it only on a publish session.** A viewer, internal, stripe-leg or room session closing with 4014 means nothing. Only the authenticated publisher can end its broadcast, and it already could by never coming back. Ending sooner gives no new power. | No new attack surface: a tokenless request can't get a publish session on an existing ID (R17). |
| D5 | **Viewers get the in-band notice too.** 4014 joins docs/59's notice set (`closenotice.go`), so Chrome viewers read it. | Without it Chrome would see "Connection lost." and reconnect into a 404 through the normal budget. That eventually works, but it shows "Reconnecting…" instead of "Broadcast ended". |
| D6 | **An edge pod passes 4014 on.** An edge's upstream pull that closes with 4014 ends the edge hub with 4014, like 4000 and 4006 (`edge.go`). The origin closes its internal subscribers before it deletes the Lease, so the pull's close normally beats the informer's lease deletion. If the informer wins, edge viewers get today's 4000 (terminal). That is a known limit, recorded here. | The same pass-through docs/59 CN5 built for kills, without a new channel. |
| D7 | **The viewer treats 4014 as "ended, waiting".** `ViewerSession` reports the end (`onStopped`), so the screen shows the ended card. It then keeps dialling the same code every 5 s plus up to 1 s of jitter. These dials never spend the reconnect budget, the 4012 pattern. While it waits, every dial failure (a 404 for a missing ID, which the browser can't read) keeps it waiting. A successful dial calls `onConnected` and playback resumes. A terminal code (4000, 4006) during the wait ends it for good. A user stop cancels it. The stopped stream's last frame is hidden until the restarted stream completes a frame. | OD2. A fixed cadence instead of a ladder: the broadcaster's restart is a human action seconds to minutes away, and an exponential ladder would leave a viewer staring at a live stream's ended card. 1000 waiting viewers cost ~200 handshakes/s, which the homelab handles; jitter spreads them. An audience behind one address can exceed the relay's per-IP connect rate (`-conn-rate-limit`, 3/s); a refused redial is just another failure, and the next tick tries again. |
| D8 | **The ended card says it will come back.** Title "Broadcast ended", body "The broadcaster stopped. Keep this page open and the stream comes back if they start again on this code." Back to home stays. Room tiles need no copy: the hub's expiry removes the attachment (docs/44), and the broadcaster's restart re-attaches it. | The card must tell the truth about the wait. Otherwise the viewer closes a tab that would have resumed. |
| D9 | **Which client actions send 4014.** Web: the Stop button on the broadcaster screen and the `stream` page (both `stop({ end: true })`). An unmount, a pipeline failure, the background watchdog and a terminal relay close still close plainly. Native engine: a new `Session::end()` that closes the publish leg with 4014. Desktop End calls it. Pause, a restart and quit keep `stop()`. Desktop End from Paused reclaims the code once (bounded by the engine's dial timeout) only to close that leg with 4014 (OD3); if the dial fails, the grace ends it. iOS: the End button, the Live Activity's End, and the user stopping the recording from the system UI (the indicator or Control Center, ScreenCaptureKit's `userStopped`) call `end()` (OD4). A capture error or a background-mode loss, and leaving the app, don't. | OD1: deliberate stops only. The watchdog is deliberately left out: it stops a tab that is probably abandoned, not one whose user chose to stop. |
| D11 | **A PoP broadcast ends at home too** (OD5). A PoP runs the same publish handler, so its hub ends on 4014 and closes the uplink leg with 4014 as one of its subscribers. Home's `/pop/publish` loop reads the leg's close code the way the publish loop does and calls `Publisher.Stop`, so home's viewers and edges get 4014. The PoP's dialer owns its QUIC connection and closes it `sessionCloseLinger` (1 s) after the session ends, or when home closes it first, so the capsule arrives (§1). The PoP's uplink treats a 4014 leg end as final, like 4000. The other direction needs nothing new: a party viewer of a home broadcast is fed by an edge pull of home, so D6 passes 4014 on. | Outside viewers are viewers. The fix is the same two halves the rest of this milestone needed: read the code where it arrives, and don't close the connection before it can. |
| D10 | **The bus reason is `stopped`.** `fi.ioio.gawk.broadcast.ended` gains the reason value `stopped` (schema enum, golden vector, AsyncAPI). `gawk-admin` shows it as "stopped by the broadcaster". | A consumer that pages on `killed` or graphs `gc` must not miscount a stop as either. |

---

## 4. What still freezes

- **A tab closed, a crash, a network loss.** No close capsule reaches the
  relay, so the grace holds as before. A `pagehide` handler could try to send
  4014. It was left out: `pagehide` also fires on a reload, which must keep
  the broadcast (docs/44 §4.4), and a bfcache entry can be restored.
- **Desktop End when the paused reclaim fails, and End on the
  crash-recovery Paused page** (docs/64 D11), which has no session at all.
  Both keep the grace.
- **An iOS capture failure** (an error, a background-mode loss) closes
  plainly, like every client's failures (D9).
- **Stop pressed while the web broadcaster is between sessions.** If the
  session died and the resume is waiting with nothing dialled, there is no
  session to close with 4014. The broadcast waits out the grace.
- **The edge race in D6.**

---

## 5. Wire

`CloseCodeBroadcasterStopped = 4014`, in `gawk-server/wire/wire.go`,
`gawk-app/src/transport/wire.ts` and `crates/wire/src/lib.rs`. It is pinned
by value in each mirror's close-code test. No message layout changes. The
notice for 4014 is `SessionClosing` (docs/59) with code `0x00000fae`, so its
vector is `011700000fae`.

Terminality, the table every client consults:

| Code | Viewer | Publisher |
|---|---|---|
| 4014 | not terminal: ended, then waits (D7) | never received from the relay. If it is, it is not terminal. |

---

## 6. Chunks

| Chunk | What | Acceptance |
|---|---|---|
| **BE1** | Wire: 4014 in all three mirrors and in the notice set. | Close-code pins in all three. `011700000fae` in the three golden files. |
| **BE2** | Relay: D2–D6 and D10. `Registry.StopBroadcast`, the publish loop's close-error read, the edge pass-through, the bus reason. | `TestPublisherStopEndsTheBroadcastNow` (a real webtransport-go client closes with 4014: the viewer is closed with 4014 within 1 s, the hub is gone, the Lease is deleted, `broadcast.ended` reason `stopped`). `TestPublisherPlainCloseKeepsTheGrace`. `TestStaleStopDoesNotEndTheSuccessor`. `TestPublisherStopThenReclaimRecreates`. `TestEdgeViewerIsToldTheOriginsStop`. Rooms: the attachment is removed. Every existing test green. |
| **BE3** | `gawk-admin`: the `stopped` reason's activity copy. | `activity_test.go`. |
| **BE4** | Web broadcaster (main thread and worker): `stop({ end: true })` closes with 4014. The Stop buttons pass it. | `broadcaster.test.ts`: the end stop closes with `{closeCode: 4014}`, a plain stop and a failure close with none. Worker path equivalent. |
| **BE5** | Web viewer: D7–D8 in `ViewerSession`, the worker viewer and `useViewerConnection`/`ViewerScreen`. | `viewer-session.test.ts`: 4014 fires `onStopped`, then retries on the fixed cadence without spending the budget, keeps waiting through dial failures, resumes with `onConnected`, ends on 4000 during the wait, and a user stop cancels the wait. The notice path (4014 in `SessionClosing`) behaves the same. The screen tests show the D8 copy and return to watching. |
| **BE6** | Native: `Session::end()` in the engine (the publish leg closes with a real `WT_CLOSE_SESSION` capsule through a new vendored-wtransport `Connection::close_session`, `vendor/wtransport/GAWK-PATCH.md` item 4, which waits up to 1 s for the relay to read it before the connection closes), desktop End and iOS Stop call it, and the iOS native viewer gets D7–D8. | Engine test: `end()` closes the publish leg with 4014, `stop()` and Pause with 0. Shell tests for End vs Pause. iOS viewer reconnect tests for the wait. `cargo` gates and the iOS gates green. |

Owner pass, after merge: on the reference deployment, Stop in Chrome →
a Chrome viewer and an iPhone viewer show the ended card within a second →
Start again on the same code → both play again without a reload.
