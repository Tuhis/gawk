# R58 — Desktop broadcaster redesign (docs/60)

**Status**: designed 2026-09-28 in a Claude Design pass and approved by the
owner the same day; chunks **DR1–DR6** (§6), all done: DR1–DR5
implemented 2026-09-28, DR6 passed on Windows and macOS 2026-09-29.
Status lives in
[`ROADMAP.md`](../ROADMAP.md).

**Relationship to earlier work**: the window is the one both desktop shells
show ([docs/54](54-macos-native-broadcaster.md) D11: one `crates/ui/main.slint`,
platforms differ in properties and one card). Its information architecture
came from the Linux GUI's card stack ([docs/38](38-windows-native-broadcaster.md)
D12). The visual language is the web app's ([docs/10](10-production-ui.md)).
The room features build on [docs/44](44-rooms.md) and revise its D13 (§3 D9).

---

## 1. Why

The desktop window grew one card per milestone: header, error, share,
picker, controls, code, room, thumbnail, details, settings. It is a
settings form with a Start button in the middle. Three things follow from
that:

- **The common case is buried.** A first-time broadcaster sees a window
  list, three room fields, a Details checkbox and a Settings checkbox
  before anything says "press this to go live". The code, which is the
  whole point, appears in the middle of the stack.
- **It doesn't look like gawk.** It uses its own palette (`#14161a` cards,
  a green LIVE dot) and the std-widgets look. The web app's tokens,
  primitives and red LIVE badge are what a viewer sees; the broadcaster
  should look like the same product.
- **Rooms are a text field.** The room card is a code field, an attach-key
  field ("static rooms only"), a nickname field and a status sentence. The
  broadcaster can't see who else is in the room, can't manage a room they
  made, and has to retype the group's room every time it changes.

The priorities, in order, as the owner set them: **simplicity for the end
user**, then **flexibility for the power user**, then **better room
features** in the app.

## 2. The design pass

Drawn in Claude Design on 2026-09-28: 17 window frames in four rows (core
flow, rooms, power user, edge states), all on the web app's real tokens.

- **Canvas**: <https://claude.ai/artifact/EPVFWE5AG7Xn5zoUi1PVLe> — a
  Claude artifact owned by the maintainer. It is **private**: ask the
  maintainer for access or an export. Its absence is not "no design
  exists".

Where this document and the canvas disagree, this document wins. The
deliberate differences are listed in §5.

## 3. Decisions

| # | Decision | Rationale |
|---|---|---|
| D1 | **Pages, not cards.** The window shows one page at a time: **Ready**, **Live**, **Settings**, **Advanced** and (Windows) **Choose what to share**. Sheets and cards over the page carry the room flows and the blocking questions. It is still one window, and the window is still the app (docs/38 D12): no tray, closing while live asks first. | A card stack shows every control at once. A page shows the ones that apply now. |
| D2 | **The web app's visual language, verbatim.** `global.css` tokens (surfaces `#0a0b0d`/`#121317`/`#171920`, border `#262a33`, text `#e8eaed`/`#9aa1ad`, one accent `#6b8afe`, LIVE `#e5484d`), the white primary button, the six-box join code, the red LIVE badge, and the brand purple only on the mark. Dark only. Custom primitives in `main.slint`, not the std-widgets look. | The desktop app should be recognisably gawk. The web app's tokens are the one source (docs/10 D3), so they are copied, not re-picked. The old green LIVE dot disagreed with every other surface. |
| D3 | **One primary action per page.** Ready → **Go live**. Live → **Copy link** (in a room: **Copy room link**). Stopped → **Go live again on CODE**. Stop is a danger outline, never the loudest control. | The owner's first priority. A user who reads nothing else presses the one white button and gets a code. |
| D4 | **Ready is three rows.** Under the source card: **Sound**, **Room** and **Quality**, each with a plain-language value and one action. The source card shows what is selected: the app's icon and title, or the display. It is not a live preview, because nothing captures while idle (macOS shows its screen-sharing indicator while a stream exists, docs/54 D4). | Everything a first broadcast needs, and nothing it doesn't. |
| D5 | **Windows picks a source by default.** On first run the primary display is selected. After that, the last source is remembered (`lastSource`: a display by its label, a window by its title) and re-selected when it is still there. If it isn't, the selection falls back to the primary display. The window list opens from **Change** as its own page. macOS keeps the system picker (docs/54 D4). | Whole-screen is the documented happy path for exclusive-fullscreen games. Remembering the choice makes the second broadcast one click. |
| D6 | **Settings has three levels.** **Settings** holds Quality (resolution and frame rate as segmented controls, an upload cap) and the server list. **Advanced** holds the raw fields: server name, relay URL, the web address for join links, publish secret, custom size, diagnostics address, and **Copy diagnostics**. Every field and rule from docs/38 D13 and docs/40 stays, including blank meaning the default and the pinned default server. | The power user reaches everything in two clicks. Nobody else meets a telemetry URL. |
| D7 | **A wide window becomes two columns.** At ≥ 900 px wide, Live shows the preview and the details grid on the left, and code, rows and Stop on the right. Details is also one click away in the narrow layout. | Flexibility without a mode switch: resize the window. |
| D8 | **Rooms: one field, a pending chip, saved and recent rooms.** The **Add to a room** sheet has one "room code or link" field (the web app's 2026-09-23 shape, docs/44 §4.8), **Create a new room**, and **Your rooms**: saved rooms first, then the most recent (at most 8). A room chosen before going live is a pending chip on the Room row ("Joins when you go live", with a dismiss). As today, it persists until dismissed. A pasted room link's `?rt=` grant is honoured: an attach key (`a:`) becomes the room's attach key, and a creator token (`c:`) rejoins as creator. There is **no attach-key field**: a gated static room admits the broadcaster as a watcher, and the app asks for the key in place (**This room needs a key to add your stream**). | Matches the web broadcaster, where the owner removed the attach-key field because it asked users to know what a static room is. |
| D9 | **A read-only roster and creator controls in the app** (*revises docs/44 D13, 2026-09-28*). In a room, Live shows who is streaming (label, live or away, viewer count), who is watching (nicknames), and the speaking ring when the `SPEAKING` flag is set. Each stream has **Watch**, which opens `#/view/<ID>` in the browser. A creator gets **Manage room**: copy code and link, **Remove** per stream (Detach with that stream's ID), and **End room for everyone** (EndRoom). **Open room view** stays for the full grid. | D13 said "the browser is the room UI". It still is for watching. But a broadcaster in a fullscreen game glances at the desktop window, and "who's here" and "remove the stream that's gone wrong" should not need a browser tab. The wire has carried nicknames, participant flags, any-ID Detach and EndRoom since RM1, so this is engine and UI work with **no wire change**. |
| D10 | **How a room ends, per docs/44 §4.9 (2026-09-24).** Ending the room yourself shows no card; the Room row goes back to "Not in a room". Someone else ending it shows a card: "Room CODE has ended", "Your stream is still live on its own code", **Back to my stream**. The creator removing your stream shows the same card with its own sentence. Other removals, and every other room message, are one status line on the Room row. | The web broadcaster's rules, so the two broadcasters behave alike. |
| D11 | **Stopped is a summary.** After a stop, Ready shows how long you were live, the most people watching, the average upload and the resolution sent, plus **Go live again on CODE**. That button is the existing resume (the persisted ID and token). It shows no countdown, because the relay's grace period (default 5 minutes) is not known to the app. **Done, start over with a new code** returns to plain Ready. | A stop is usually "I'll be back in a minute". The resume path already exists; the summary gives it a reason to be the default. |
| D12 | **A crash gets its own question.** A `wasLive` flag is set when a broadcast goes live and cleared when it ends in the app. If the app starts with it set, Ready opens as **Pick up where you left off?**: the code, the room it was in, **Resume on CODE**, or **Start with a new code instead**. | Without the flag, a Resume button after a normal quit and one after a crash look identical, and the crash is the case where viewers are waiting. |
| D13 | **Notices are banners with a next step.** The uplink warning, the audio-silence hint and the minimized-window hint are dismissible banners on Live. The audio hint keeps its one-click fix (**Share all sound instead**). The uplink banner points at Quality, since settings apply to the next broadcast. Reconnecting is an amber badge replacing LIVE, with the preview dimmed and Stop still available. Close-while-live is a modal with **Keep broadcasting** as the primary. The R45 update notice gets a reserved header slot and is built by R45. | One status-line string used to carry four kinds of message. Each now has a place that fits its urgency. |
| D14 | **The room-view grant is the SPA's format.** "Open room view" sends `?rt=c:<hex>` for a creator token and `?rt=a:<key>` for an attach key (`grantHandoff.ts` is the format's home). The native apps sent both bare. The SPA reads a bare value only as a creator token, so a static room's attach key was dropped, or misread as a creator token if it happened to be 32 hex digits. Fixed test-first in this milestone. | A real bug found during the pass. |

## 4. What does not change

Capture, encode, audio, the sender, the session lifecycle, resume, the
identity latch, telemetry, the uplink monitor, the relay and the wire.
Every existing config key keeps its meaning, except that `room` now only
ever holds a code: a pasted link's `?rt=` grant goes to the wrapped
`roomAttachSecret` or the new `roomCreatorToken`, and a file that stored a
link is rewritten that way at launch. The new keys (`recentRooms`,
`roomCreatorToken`, `wasLive`, `lastSource`) default to absent, so an old
file loads unchanged. Room attach keys in `recentRooms` and the creator
token are credentials: DPAPI-wrapped on Windows and plaintext in the
mode-0600 file on macOS, like `roomAttachSecret` (docs/54 D12).

## 5. Differences from the canvas

| Canvas | Built | Why |
|---|---|---|
| Recent rooms show "2 streaming now" | Saved / last joined only | A live count needs a way to look into a room without joining it. That is a new relay route, which this milestone does not add. |
| "Join TuhisRoom every time I go live" switch | None | The pending room already persists until dismissed, which is that behaviour. A second control for it would be noise. |
| "Kept for 4:12" countdown on Stopped | None | The app doesn't know the relay's grace period (D11). |
| Uplink banner **Drop to 720p** | **Open quality settings** | Quality applies to the next broadcast. A button that seems to act now and doesn't is worse than a pointer. |
| **Ctrl+L** copies the link | Not built | Not needed for the goals, and a window-level key handler in Slint competes with the text fields. |
| Picker grid with window thumbnails | A list with app icons | The enumerator returns icons, not thumbnails, and capturing every window to draw one is not free. |
| Update-ready pill | Reserved slot | R45 isn't built (D13). |
| Settings › App switches (check for updates, ask before quitting) | None | The update check is R45's. Asking before quitting while live is not optional (docs/38 D12). |
| "Welcome back … 2 minutes ago" | "gawk closed while you were live" | The app doesn't record when it died. |

## 6. Chunks and acceptance criteria

| Chunk | What | Acceptance |
|---|---|---|
| **DR1** | Engine: `RoomSummary.people` (id, nickname, kind, streaming, speaking), replaced from RoomState and patched by participant events; `Session::room_remove(id)` (Detach with another stream's ID) and `Session::room_end()` (EndRoom); a `RoomGrant` type with `parse_room_input` (code, plus the `?rt=` grant of a pasted link) and `room_link` formatting `c:`/`a:` (D14). | Unit tests in `room.rs`: a snapshot's participants arrive in the summary; joined, updated (speaking, streaming) and left patch it; `participants` equals `people.len()`; Remove sends Detach with the other ID and leaves the session up; EndRoom sends the command and the relay's 4007 ends it with the creator reason. `lib.rs`: the grant formats as `c:`/`a:`; a pasted link yields code plus grant; the old bare-key link test fails before the fix. |
| **DR2** | Config: `recentRooms` (code, saved, last joined, attach key, most recent first, at most 8 with saved rooms kept), `wasLive`, `lastSource`. | `config.rs` tests: the fields round-trip; attach keys go through `Credentials` both ways; a file without them loads with defaults; the recents list caps without dropping a saved room; a re-join moves the room to the front. |
| **DR3** | Shell view logic, as pure functions with tests: roster rows (streams first: own, live, away; then watchers), the pending room chip, the stopped summary (duration, peak, average upload), which room end shows a card (D10), the Windows default and remembered source (D5), and the key prompt for a gated room. | `shell.rs` tests for each function. The existing shell tests (identity latch, server combo, parsers) stay green. |
| **DR4** | The window: `main.slint` rewritten per D1–D4, D6, D7 and D13 on shared primitives (Button, Pill, Row, Segmented, Switch, Field, Icon), with the room sheet, Manage room, the room-ended card, the key prompt, the crash-resume page and the quit modal. Both shells' property and callback contract stays one file (docs/54 D11). | `cargo build`, host `cargo clippy -D warnings` and `cargo xwin clippy` green (the Slint compile is part of the build). Every callback the old window had is still wired. The `system-picker` card is the only platform branch. |
| **DR5** | Platform glue: the Windows shell selects and remembers the source (D5) and opens the picker page from **Change**; the macOS shell is unchanged apart from property names. | Windows target clippy green (CI). The default-source choice lives in DR3's tested function, not in `cfg(windows)` code. |
| **DR6** | Manual pass on the gaming PC (Windows) and a Mac. | Owner-run: first broadcast from a fresh config takes one click after launch; a room from the sheet joins on go-live; roster, Watch, Manage room (remove, end) work against two web participants; a gated static room asks for its key; stop → Go live again keeps viewers; a killed app offers resume on relaunch; the wide layout appears at ≥ 900 px. |

## 7. Verification

- DR1–DR3: the unit tests above run in CI's `cargo llvm-cov --workspace` on
  Linux and `cargo test --workspace` on `macos-latest`.
- DR4–DR5: the Slint compile runs inside every build. The Windows-only glue
  is linted by the `cargo xwin clippy` job.
- DR6: the owner's hardware pass, done 2026-09-29 on the gaming PC
  (Windows) and a Mac: every item in the DR6 acceptance row passed on both.
