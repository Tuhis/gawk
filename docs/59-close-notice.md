# R57 — Close codes Chrome can read: the in-band close notice

**Status**: shipped 2026-09-24 in one PR, chunks **CN1–CN4**, all done
(§6). Owner decision taken 2026-09-24 (§2).

**Relationship to earlier work**: every terminal close code the relay
sends was specified as if browsers read it: 4000 (docs/06), 4004 (docs/06
revision 2026-07-18), 4006 (docs/42 §4.4) and 4007 (docs/44 §4.4). Chrome
reads none of them. This milestone doesn't change what any code means. It
changes how the meaning reaches Chrome.

---

## 1. Why, and what "done" means

### The finding (2026-09-24)

Ending a room from the web broadcaster left the page on a black stage under
"Reconnecting to the room…". The relay had done everything right: it sent
the `RoomEnding` event, waited its settle, and closed the session with
**4007**. A Chrome net-log showed why the page never learned that. The close
packet carried **STOP_SENDING on the CONNECT stream ahead of the
`WT_CLOSE_SESSION` capsule**, and Chrome failed the session on that frame
before it read the capsule. `WebTransport.closed` rejected with
"Connection lost." and no `closeCode`.

The cause is in webtransport-go's `closeSessionStream`. It writes the capsule
and then calls `CancelRead` on the CONNECT stream, and quic-go packs control
frames before stream data in the same packet. Upstream closed this as a
Chromium bug and won't change it (quic-go/webtransport-go#242,
issues.chromium.org/485669950). Firefox reads the codes.

A probe server on the relay's exact stack (quic-go v0.62.0, webtransport-go
v0.13.0, the relay's `webtransport.Server` config) closed sessions with every
gawk code in four session shapes: idle, room-like bidi, viewer-like and
publisher-like. **Chrome received 1 close code out of 84.** So every
code-driven client behaviour was dead in Chrome:

| Code | Who | What Chrome did instead |
|---|---|---|
| 4000 broadcast ended | viewer | treated it as a drop and reconnected into a 404 |
| 4004 superseded | publisher | auto-resumed back and fought the session that replaced it |
| 4006 terminated by operator | publisher, viewer | retried into the ban gate (a 451 the browser can't read, docs/42 D15) |
| 4007 room ended | room participant | "Reconnecting to the room…" until the budget ran out |
| 4001, 4002, 4005 | viewer, all | reconnect anyway; losing the code costs at most 4002's 0 ms first retry |

### Milestone acceptance criteria

| Goal | Verified by |
|---|---|
| A Chrome viewer shows "Broadcast ended" when the relay GCs its broadcast | `viewer.test.ts` notice tests; real Chrome run, §8 |
| A Chrome viewer and publisher both stop, with the moderator copy, on a kill | `viewer.test.ts`, `broadcaster-resume.test.ts`; real Chrome run, §8 |
| A deposed Chrome publisher doesn't resume | `broadcaster-resume.test.ts` notice test; real Chrome run, §8 |
| Ending a room in Chrome ends it, for the creator and for the other participants | `room-session.test.ts`, `RoomScreen.test.tsx`; real Chrome run, §8 |
| No webtransport-go fork | `gawk-server/go.mod` has no `replace` for it |
| The four wire mirrors agree | golden vector `011700000fa4` in all four (CN1) |

---

## 2. Owner decision (2026-09-24)

**OD1 — In-band notice, not a patched library.** A one-site patch to a
vendored webtransport-go (delay the STOP_SENDING) fixed every code for every
client, and Chrome read all 84 closes with it. The owner chose not to
maintain a fork. The relay states the code on the session itself instead,
which is the pattern `RoomEnding` already follows for rooms.

---

## 3. Non-goals

- **The non-terminal codes** (4001, 4002, 4005). Each already means
  "reconnect", which is what a client does with no code. A notice for 4002
  would also have to fit inside the drain's staggered window (docs/22), for
  nothing a client would do differently.
- **Room control sessions.** `RoomEnding` already carries 4007's meaning
  in-band. The client now acts on it (CN4).
- **Internal sessions** (`/internal/*`). The relay's own edge client is Go and
  reads close codes directly.

---

## 4. Decisions

### D1 — `SessionClosing` (0x17), on its own uni stream

A relay→client message: `Version ‖ 0x17 ‖ uint32 BE code`, exactly 6 bytes,
code in 4000–4999, parsed strictly. It rides a server-opened unidirectional
stream, like ResumeToken, TelemetryHello and RelayIdentity. A datagram can be
lost, and the browser API exposes no response headers. Allocated after the
room types 0x13–0x16 in `gawk-server/wire/closing.go` and mirrored in
`wire.ts`, `gawk-broadcast/internal/wirecheck` and
`gawk-broadcast-desktop/crates/wire`.

### D2 — Sent only where a browser needs it

The relay sends the notice for **4000, 4004 and 4006** on **external publish
and subscribe sessions**, but not on:

- `/internal/subscribe`, where the edge client reads every uni stream as a
  keyframe;
- stripe legs, where the viewer reads no streams (docs/35 §14);
- room control sessions (§3).

Both native broadcasters already ignore unknown server message types, so a
native publisher gets the stream and drops it. An older web viewer counts it
as a malformed stream and logs a warning.

### D3 — Notice, settle, then close

A session close cancels every stream and discards what the peer hasn't read.
A notice sharing a packet with the close would be lost: the room registry's
`closeSettle` gotcha. So the relay writes the notice and closes
**250 ms** later (`closeNoticeSettle`). There are two shapes
(`internal/transport/closenotice.go`):

- **Blocking** (`closeWithNotice`) at handler sites that return right after
  closing. A session must not outlive its handler.
- **Async** (`closeWithNoticeAsync`, a timer) where the caller must not stall:
  the hub closing every viewer of a broadcast, a moderation kill, the session
  adapter. The session's own handler keeps it alive until the timer fires,
  and the hub has already detached it, so a deposed publisher's late frames
  drop (docs/06).

The session adapter carries a `closeNotice` flag. It is true for publish and
external subscribe, false for internal subscribe and stripe legs.

### D4 — The client keeps a code it read, and falls back to the notice

`closed` carrying a code wins. That's Firefox, and any browser that fixes the
bug. Otherwise the viewer transport and the broadcaster use the noticed code:

- the viewer in `reportClosed`, and also in `reportDropped` when the read loop
  dies first;
- the broadcaster per session generation in `handleSessionGone`.

Everything downstream (the terminal-code sets in `reconnect.ts`, the end-card
copy) is unchanged.

### D5 — Rooms: RoomEnding ends the room

The room control session already had its notice: the relay sends `RoomEnding`
a settle before the 4007. `RoomSession` now treats any session end that
follows a `RoomEnding` as the end of the room, whatever the code (docs/44 §4.6
revision 2026-09-24). The broadcaster page's room UX changed with it (docs/44
§4.9 revision 2026-09-24):

- ending your own room returns you to your live stage with no card;
- a room someone else ended, or your stream removed by the creator, shows a
  card with **Back to my stream**;
- a broadcast that fails while you're in a room leaves the room and shows
  "Your broadcast stopped" with the reason. It used to leave the room view up,
  reading LIVE.

---

## 5. Architecture

```
relay                                         Chrome
─────                                         ──────
terminal close (4000/4004/4006)
  └─ OpenUniStream → [01 17 00 00 0f a4] ───▶ readServerStreams / readServerMessage
     wait closeNoticeSettle (250 ms)             remembers code
  └─ CloseWithError(code) ─ STOP_SENDING ─────▶ closed rejects "Connection lost." (no code)
                                                 → uses the remembered code
```

---

## 6. Chunks and acceptance criteria

| Chunk | Scope | Acceptance criteria | Status |
|---|---|---|---|
| **CN1** | Wire: 0x17 in all four mirrors | Golden vector `011700000fa4` byte-identical in `closing_test.go`, `wire.test.ts`, `wirecheck_test.go`, `golden.rs`; the type and size pinned in each constants table; strict parsers reject a wrong size, version, type or out-of-range code | ✅ |
| **CN2** | Relay sends the notice | `closenotice_test.go`: a Go client sees the notice *and then* the same close code for a GC'd broadcast's viewer (4000), a deposed publisher (4004), and a killed broadcast's publisher and viewer (4006); `TestReclaimSupersedesActivePublisher` updated to read through the notice; `go test -race ./...` green | ✅ |
| **CN3** | Web viewer and broadcaster use it | `connection.test.ts` dispatches the notice without touching media and counts a malformed one; `viewer.test.ts` reports the noticed code when `closed` has none and when the read loop dies first; `broadcaster-resume.test.ts` treats a noticed 4004/4006 as terminal with no resume dial | ✅ |
| **CN4** | Rooms act on RoomEnding; broadcaster room UX | `room-session.test.ts` RoomEnding + code-less loss → `onEnded`; `RoomScreen.test.tsx` self-end returns without a card, others' end and own-stream removal show a card and return on acknowledge; `BroadcasterScreen.room.test.tsx` a mid-broadcast failure leaves the room and says "Your broadcast stopped" | ✅ |

---

## 7. Risks

- **A relay older than this sends no notice.** Chrome clients then behave as
  before, except rooms, which D5 covers. The fleet deploys the relay
  automatically on release.
- **The notice is lost.** The stream can't be opened (no stream credit), or
  the write misses its 100 ms deadline. The session still closes with its
  code, so Firefox is unaffected and Chrome sees the old drop behaviour.
- **250 ms later terminal closes.** Deliberate: that's the settle that makes
  the notice readable.
- **New close codes.** Any code a browser must act on needs the notice too.
  R43's planned refusal codes 4008–4011 (docs/45, not started) are the next
  case. `SessionClosing` already accepts the whole 4000–4999 range, and
  `noticedCloseCode` is the one list to extend.

---

## 8. Measurements

Real Chrome (headless, tab capture), a relay built from this branch, 2026-09-24:

- **4000:** broadcaster stops, 3 s grace. The viewer's `closed` rejects
  "Connection lost."; the viewer logs "Broadcast ended by server (code
  4000)" and shows **Broadcast ended**.
- **4006:** a `file:` moderation source bans the live ID. The viewer shows
  **Broadcast ended by a moderator**. The broadcaster logs "Relay ended this
  publisher session (code 4006). Not resuming." and shows **Your broadcast
  stopped — This broadcast was terminated by the server operator.**
- **4004:** a second browser reclaims with the resume token while the first
  is in a room. The first logs "(code 4004). Not resuming.", leaves the room
  and shows **Your broadcast stopped — Another broadcaster took over this
  code…**
- **4007:** own End room returns to the live stage. Another participant sees
  **Room ended … Your stream is still live on its own code.**, and
  acknowledging it returns them to their live stage. The creator detaching
  a participant's stream shows **Your stream was removed from the room**.

The probe matrix behind §1 (84 closes, four shapes) and the patched-library
comparison are in the PR description.

---

## 9. References

- quic-go/webtransport-go#242. Closed upstream: "a browser bug, not a bug in
  webtransport-go".
- issues.chromium.org/485669950 (not public).
- docs/44 §4.6 and §4.9 revisions 2026-09-24; docs/gotchas.md (webtransport-go,
  Rooms).
