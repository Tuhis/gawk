# R86 — Choose which streams to watch in a room

**Status**: designed and implemented 2026-11-19 (VS1–VS3, §8). Open: VS4, the owner pass.

**Scope**: the web app's room view (`gawk-app/src/features/room`) and the
rooms step of `e2e/run.mjs`. No relay, wire, telemetry, iOS, Rust-core or
native-broadcaster change.

The room footer's segment has four buttons: Grid · Fit · Focus · Hide
videos ([docs/44](44-rooms.md) D11, [docs/87](87-room-fit-layout.md) D1).
Three of them choose a layout and one chooses what to show, and that one
is all or nothing: every stream plays, or none does. A participant who
wants four of five streams, because the fifth is a lobby screen, a
stream they've seen, or one their downlink can't carry, has no way to
drop just that one.

This milestone splits the two questions. The segment keeps the layouts
(Grid · Fit · Focus), and a new pill beside it says what is shown:
**All streams**, **Choose**, or **None**. None is the old Hide videos.
Choose lists the room's streams with a checkbox each. A stream you
untick closes its media session, so it costs nothing, the same as Hide
videos costs nothing for all of them.

## 1. Why, and what "done" means

The owner asked for a way to hide individual streams by the viewer's
choice, with good UX and the current look. Five designs were mocked on a
design canvas (§2), and the owner picked this one.

### Milestone acceptance criteria

| Goal | Verified by |
|---|---|
| The footer's layout segment offers **Grid · Fit · Focus**; a pill beside it opens **All streams · Choose · None**, with a checkbox per stream under Choose | VS2 component tests |
| A stream unticked under Choose has **no tile and no media session**; the others keep theirs, with no re-dial | VS2 component test (the fake viewer sessions); `e2e/run.mjs --rooms`: after unticking one of two streams, one active broadcast reads `subscribers: 0` on `/statusz` and the other ≥ 1 |
| None does what Hide videos did: no tiles, every media session closed, the control session kept, the card shown; a broadcaster's None is **Preview only** | VS2 component tests; the `e2e/run.mjs --rooms` step that was Hide videos, now through None |
| Tiles keep their room numbers when others are hidden; `1`–`4` show and focus that stream, `Shift`+`1`–`4` hide or show it, `0` returns to the last all-POV layout | VS2 component tests |
| A stream that joins the room later arrives shown | VS2 component test |
| The choice is remembered per room on this device; the old `hidden` mode preference loads as None | VS1 unit tests; VS2 component test |
| Unticking every stream shows a card that offers them back, never a black stage | VS2 component test |
| Focus skips hidden streams | VS2 component test |
| On a phone the footer still fits on one row and the popover stays on screen | VS2 CSS; VS4 owner pass |

## 2. Owner decisions (2026-11-19)

| # | Question | Decision |
|---|---|---|
| OD1 | Which of the five mocked designs? (1: a hide button on each tile; 2: a "Streams" checklist pill; 3: no new chrome, entries in the tile menu and the people panel; 4: visibility as its own control beside the layouts; 5: a "Choose mode" contact sheet) | **4**, visibility as its own control. |

The rest are this design's defaults, written as decisions (§3) so the
owner pass can revise them in place.

## 3. Decisions

| # | Decision | Why |
|---|---|---|
| D1 | **Two controls.** The segment chooses the layout: Grid · Fit · Focus. A pill beside it chooses what is shown: All streams · Choose · None. | One home for each question. The segment loses a button, which helps on a phone, where it is icon-only. |
| D2 | **None replaces Hide videos**, and for a broadcaster with a local preview it reads **Preview only**, as the segment's fourth button did. Its note says "Hide videos" so returning users find it. | It is the same state with the same cost. The note is the proposal's own answer to its one drawback: the control moved. |
| D3 | **Hiding is unsubscribing.** A hidden stream has no tile, so its media session closes, as every session does under None. Showing it again dials a new one. | The point is the download, not only the pixels. A muted, invisible tile still costs the uplink and the decoder. |
| D4 | **The choice is a set of hidden streams**, by broadcast ID, so a stream that joins later arrives shown. Choose with nothing unticked shows everything. | Someone who hid a lobby screen still expects the friend who joins next to appear. |
| D5 | **Remembered per room, on this device**: `gawk:room-picks` holds the 20 most recently changed rooms by room code, each with whether Choose is selected and up to 64 hidden IDs. All streams keeps the set, so going back to Choose restores it. None stays global (`gawk:room-videos`), as Hide videos was. | A pick belongs to a room; None is a bandwidth habit. The caps keep the key small: a room carries far fewer attachments than 64, and old rooms fall off. The room code and the broadcast IDs never leave the device, as the tile volumes (`gawk:room-volume:<id>`) never do. |
| D6 | **The old preference migrates once**: a stored `gawk:room-mode` of `hidden` becomes None, with the layout set to the last all-POV layout (`gawk:room-grid`). | A reload must not drop a participant who was in Hide videos into a full room of downloads. |
| D7 | **Numbers keep the room's order.** Stream 4 stays 4 when 2 is hidden. `1`–`4` show (if hidden, or if None is on) and focus that stream. `Shift`+`1`–`4` hide it if it is on the stage, else show it; hiding from All streams starts Choose with only that stream hidden. | A number is the stream's, not the slot's, so the keys don't move under the user. Hiding from All with the old set would bring back streams the user isn't looking at. |
| D8 | **Picking a layout leaves None**, as `0` does, and as clicking Grid did when Hide videos was the same segment. While None is on, the segment is dimmed. | Returning users clicked Grid to get out of Hide videos; that keeps working. |
| D9 | **Focus skips hidden streams.** Hiding the focused stream focuses the first one shown. | A focused tile you can't see is no focus. |
| D10 | **Every empty stage has a card.** None: "You're still in the room", with All streams and Choose…. Choose with nothing ticked: "No streams chosen", with All streams and Choose…. | docs/44's rule: every state has a visible form. |
| D11 | **The popover is a dialog** anchored above the pill: native radios for the three states, native checkboxes for the streams (each named by its label, the room number and the watching count beside it), and a line naming the keys. Outside presses and Escape close it; it holds the chrome while open. On a phone it spans the footer, and the pill reads "All", "3/5" or None's icon alone (its accessible name keeps the full text). | Native inputs bring arrow keys and screen-reader roles for free. At 390 px the full "All streams" squeezed the quality pill to one letter (§8). |
| D12 | **No eye icon.** The pill is text, with the video-off or screen icon only for None. | The eye is the "N watching" glyph across the app. |
| D13 | A tile's stats panel closes when its tile leaves the stage, as it does when its stream is detached. | A panel about a stream you hid would be about nothing on screen. |

## 4. What changes where

| File | Change |
|---|---|
| `features/room/roomPrefs.ts` | `RoomMode` loses `'hidden'`; `loadVideosOff` / `saveVideosOff`, `loadRoomPicks` / `saveRoomPicks`; the D6 migration. |
| `features/room/roomVisibility.ts` (new) | The pure parts: which attachments are shown, the pill's text, the D7 key rules. |
| `features/room/VisibilityControl.tsx` (new) | The pill and its popover. |
| `features/room/RoomScreen.tsx` | The state (layout, None, picks), the stage built from the shown streams with their room numbers, keys, cards, the segment without Hide videos. |
| `features/room/roomCopy.ts` | The pill, popover and card copy. |
| `features/room/room.module.css` | The pill, popover and dimmed segment. |
| `e2e/run.mjs`, `e2e/README.md` | The rooms pass hides one stream, then turns on None. |

## 5. Risks and limits

- **Returning users have to find Hide videos again** (the proposal's own
  drawback). D2's note and D8's layout click are the mitigations; the
  owner pass judges whether they are enough.
- **Showing a stream again re-dials it**, so it takes a keyframe to
  appear, as leaving Hide videos does today.
- **A broadcaster who restarts on a new code arrives shown**: the hidden
  set is by broadcast ID, and a new ID is a new stream (D4).

## 6. Out of scope

- The iOS room player, which has no Hide videos either.
- Hiding from the tile itself or its menu, or from the people panel
  (proposals 1 and 3). They could call the same setter later.
- Anything the streamer or the room sees: hiding is local, and the relay
  only sees a subscriber leave.

## 7. Chunks

| Chunk | What | Acceptance |
|---|---|---|
| VS1 | Preferences: None, per-room picks with the D5 caps, the D6 migration | Unit tests: round trips, caps, corrupt JSON, migration |
| VS2 | The control, the stage from the shown streams, keys, cards, CSS | Component tests for every §1 row they name |
| VS3 | `e2e/run.mjs --rooms` and its README | One stream hidden: one broadcast at 0 subscribers, the other ≥ 1; None: both at 0, the participant count unchanged |
| VS4 | Owner pass | The owner tries it on a desktop and a phone |

## 8. Implementation notes (VS1–VS3, 2026-11-19)

- **The state is two facts and a pure module.** `RoomScreen` keeps None
  and the room's picks; `roomVisibility.ts` turns them into the shown
  streams and implements every transition (a radio, a checkbox, `Shift`+a
  number, a number, a layout pick) as a function of the current state, so
  the rules of D4–D8 are unit-tested without a component. Unchanged
  transitions return the same object, and only a changed fact is saved.
- **Two updates in one event compose.** A number key picks Focus (which
  leaves None, D8) and then reveals its stream; both read the state
  through a ref the second update sees, so it doesn't undo the first.
- **`Shift`+a digit is matched by its physical key.** Shift changes the
  digit's `KeyboardEvent.key` with the layout (`!` on US, `"` on Nordic),
  so `useHotkey` now also matches a Shift shortcut's digit by `code`
  (`Digit2`), as it already did for an Alt shortcut's letter on macOS.
- **`data-mode` still reads `hidden` under None**, so the preview-only
  stage's CSS (the broadcaster's own tile, full bleed) needed no change.
- **The popover raises the footer** (`data-raised`, z-index 26): the voice
  dock, the invite chip (22) and the unmute prompt (25) sit in the band it
  opens into, above the footer's usual 20.
- **The phone footer** measured at 390 px in headless Chromium: with
  "All streams" the quality pill shrank to one letter, so the pill takes a
  short form below 720 px (D11). With it the row holds the three layout
  icons, "All", "B…" and the right-hand controls, as the four-button
  segment did.
- **Verified.** Unit and component tests for every §1 row they name; the
  `e2e/run.mjs --rooms` pass run locally against a real relay and two
  pubsims: with bravo unticked the relay counted 0 subscribers on one
  broadcast and at least 1 on the other, then 0 on both under None, with
  the participant count unchanged.
