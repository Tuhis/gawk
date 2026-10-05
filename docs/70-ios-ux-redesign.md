# R68 — iOS app UX redesign (docs/70)

**Status**: designed 2026-10-05 in a Claude Design pass. The owner's
decisions OD1–OD12 (§2) came from canvas comments and chat the same day. Chunks
**IX1–IX10** (§8) not started. Status lives in [`ROADMAP.md`](../ROADMAP.md).

**Relationship to earlier work**: a redesign of the screens R65 built
([docs/67](67-ios-app.md)): IO3's Broadcast and Settings screens and IO5's
Watch screen and player. It also builds the Swift half of IO6, the room
view and `gawk://` link handling, to this design. IO6's core half (link
parsing, the room watcher) shipped in #464. The visual language is the web
app's ([docs/10](10-production-ui.md), `gawk-app/src/styles/global.css`)
and the desktop window's ([docs/60](60-desktop-redesign.md),
[docs/64](64-desktop-ux-pass-2.md)), on iOS 26+ structure. The room
player follows the web room screen ([docs/44](44-rooms.md),
`RoomScreen.tsx`).

This milestone revises docs/67 **D1** (four screens become three tabs, D1
here), **D18** (the first-broadcast notice's copy, D10), **D20** (one code
box for broadcasts and rooms, D3–D4), **D21** (the room viewer's layout,
D19–D20), **D22** (Picture in Picture starts on its own when you leave
the app, D8), **D23** (the server picker's layout, D22–D23) and **§6** (a
widget extension for the Live Activity, D15). Each has a dated note in
docs/67.

---

## 1. Why

The owner's verdict on the R65 app: "Current iOS app works, but the UX is
terrible." The screens are stock SwiftUI `Form`s, and nothing about them
reads as gawk:

- **It isn't gawk.** No colour scheme is set, so the app follows the
  system and is white in light mode. Every other gawk surface is dark-only
  on the web app's tokens (docs/10 D3, docs/60 D2). There is no LIVE
  badge, no six-box code and no white primary button.
- **The player is a form row.** The video sits inside a `Form` section
  with text buttons under it (Stats, Picture in Picture, Stop). The
  latency picker, the relay address and the code field stay on screen
  while you watch. Turning the phone hides the tab bar and shows bare
  video with no controls at all.
- **Going live buries the code.** Live shows the code as 40 pt text,
  followed by three plain rows (Copy link, Copy code, Share) and a
  destructive Stop row. There's no duration, no quality and no upload
  state, and nothing tells you to switch to your game.
- **Rooms are a placeholder tab**, and joining a room by code isn't
  possible.
- **Settings puts a secret field inside the server list.** Selecting a
  server grows a text field in its row.
- **Links don't open.** `project.yml` registers `gawk://`, but the app
  has no `onOpenURL` handler.

## 2. The design pass and the owner's decisions

Drawn in Claude Design on 2026-10-05 as iPhone 17 frames (402 × 874 pt,
landscape 874 × 402) in four rows. The owner commented on every row the
same day. The canvas was redrawn after each comment, and the comments
became OD1–OD9. OD10–OD12 came from chat, OD11 and OD12 on a draft of
this document.

- **Canvas**: <https://claude.ai/artifact/7ErQDw4oYaBFxKE192r8b5>, owned
  by the maintainer. It is **private**: ask the maintainer for access or
  an export. Its absence is not "no design exists". Several frames are
  clickable prototypes (Play): the code box, the code pill's copy, the
  stats drawer's two heights and the navigation between screens work.

Where this document and the canvas disagree, this document wins. §7
lists the deliberate differences.

| Board | What it shows |
|---|---|
| **A1** Watch | The Watch tab: the join card with six code boxes, Paste and Join; saved and recent rooms |
| **A2** Watching: just the video | The player with its controls hidden: video on black, nothing else |
| **A3** Tap: controls fade in | The player's controls (D5) |
| **A4** Settings menu | The player's settings menu (D6) |
| **A5** Stats, pulled out a little | The stats drawer at its small height under the video (D7) |
| **A5b** Stats, pulled all the way out | The stats drawer at full height (D7) |
| **A6** Streamer offline | The offline card (D9) |
| **A7** Landscape, controls showing | The player turned sideways |
| **B1** Broadcast, first run | Ready with the first-run notice (D10) |
| **B2** Live | Live (D11) |
| **B3** Reconnecting | Live while the publish session reconnects (D13) |
| **B4** Ended, room still pending | Ready after End: the summary card and Go live again (D14) |
| **B5–B7** Live Activity | The Lock Screen activity and the Dynamic Island's expanded, compact and minimal views (D15) |
| **C1** Room, grid · **C2** Room, focus | The room player with three streams (D19) |
| **C1b**, **C2b** Ten streams | The room player at ten streams (D19) |
| **C2c** People | The room's people sheet (D20) |
| **C3** Add to a room | The broadcaster's room sheet (D16) |
| **C4** Live in a room | Live with the room card (D17) |
| **C5** Room sheet (creator) | The creator's room sheet (D18) |
| **D1** Settings · **D2** Edit server | The Settings tab and the Edit server page (D22–D23) |
| **D3** Another server, unreachable | The non-default server strip and the unreachable banner (D24) |
| **D4** The server refuses a start | The refused-start alert (D25) |

| # | Decision (owner, 2026-10-05) |
|---|---|
| OD1 | *(Canvas comment, A2.)* **The player is just the video**, as on YouTube and Twitch. A tap shows the controls over it, as the web viewer does. "Stats definitely is only for few curious users." |
| OD2 | *(Canvas comment, A3.)* **No bar behind the player's controls**, and **LIVE is a chip at the top**. |
| OD3 | *(Canvas comment, A3.)* **No volume control in the player.** The phone's buttons do that. |
| OD4 | *(Canvas comments, A3/A7, and chat.)* **The stream's code is a small pill at the top right** with a copy icon, and a tap copies (what: OD12). Small type. The copied state keeps the code in place: the icon turns into a check and the pill takes the accent tint for a moment. A first draft that swapped the text for "Copied" was rejected. |
| OD5 | *(Canvas comment, A5.)* **Stats is a drawer with two heights**, pulled out a little and all the way. |
| OD6 | *(Canvas comment, B2.)* **No "App sound" row.** iOS shares the whole screen and all of its sound, so there's nothing to choose. |
| OD7 | *(Canvas comment, B1.)* **No "Your whole screen" card on Broadcast.** The first-run notice says it. |
| OD8 | *(Canvas comment, D3.)* **Notices sit at the top of the page**, the unreachable-server banner included. |
| OD9 | *(Canvas comment, Rooms row.)* **The room player looks like the web room screen and the single-stream player**, in Liquid Glass. The Grid / Focus toggle stays. The "Watching as …" bottom bar goes. |
| OD10 | *(Chat.)* **No compromises on the design.** Anything the design needs that the app or the core lacks is built in this milestone (§5), not designed around. |
| OD11 | *(Chat, on this doc.)* **The Live Activity is in** (D15), with the widget extension it needs (revising docs/67 §6). |
| OD12 | *(Chat, on this doc.)* **The stream's code pill copies the link**, the same as the room's code chip. The two behave alike, and Copy link leaves the settings menus. |

Questions the owner asked on the canvas, and how they were answered:

- **Can quality change during a stream?** Yes, as on the desktop (D12).
- **What do ten participants look like?** Boards C1b and C2b (D19).
- **How does "Watch the room" work on a phone?** It can't while you're
  live, so the creator's room sheet doesn't have it (D18).

## 3. The visual language

### 3.1 Tokens

The web app's tokens, copied, not re-picked (docs/60 D2). The desktop's
additions (`s3`, the `-text` colours, `glass`) come from `main.slint`'s
`T`. One Swift home, `gawk-ios/app/Gawk/Design/Theme.swift`, and a CI check
(IX1) that its values still match `global.css`.

| Token | Value | Use |
|---|---|---|
| `bg` | `#0a0b0d` | Every screen's ground; the player and room player are `#000` |
| `s1` | `#121317` | Grouped lists, cards |
| `s2` | `#171920` | Code boxes, lists inside a sheet, tiles in the stats drawer |
| `s3` | `#1d2029` | Row icon tiles |
| `border` | `#262a33` | Code box and segmented borders, separators inside sheets |
| `borderSoft` | `#1c2028` | List hairlines and row separators |
| `text` | `#e8eaed` | Text; the primary button's fill |
| `muted` | `#9aa1ad` | Secondary text, row values, section headers |
| `faint` | `#626873` | Chevrons and placeholders only (fails 4.5:1 for body text) |
| `accent` | `#6b8afe` | The app's tint: the selected tab, switches, checkmarks, focus, the active code box |
| `accentText` | `#aebeff` | Accent-coloured text; the copied code pill's icon |
| `accentSoft` / `accentLine` | `#6b8afe` at 14 % / 34 % | Info banners, the summary card, tinted buttons, the copied pill |
| `live` / `liveSoft` / `liveText` | `#e5484d` / at 16 % / `#ff8589` | LIVE, the Go live dot, the End outline |
| `ok` / `okText` | `#30a46c` / `#8fdcb2` | Connected, Steady |
| `warn` / `warnSoft` / `warnLine` / `warnText` | `#d0a215` / at 10 % / at 34 % / `#e6c14d` | Reconnecting, away, unreachable |
| `brand` | `#863bff` | The mark only (the Live Activity's icon tile) |

Dark only: `.preferredColorScheme(.dark)` on the window group, and
`.tint(Theme.accent)`.

### 3.2 Type

SF Pro and SF Mono through Dynamic Type styles:

- **Text styles**: large title (34 bold), title 2 (22 bold), headline
  (17 semibold), body (17), subheadline (15), footnote (13) and caption
  (12).
- **Section headers**: footnote semibold, upper case, tracked 0.12 em, in
  `muted`. That's the desktop's kicker, which is also where iOS's grouped
  headers sit.
- **Codes**: always SF Mono. The code boxes are 28 semibold, the code pill
  is 12 semibold tracked 0.04 em, and links are 13.

### 3.3 Elements

One vocabulary across the app, after docs/64 D4:

| Element | Spec | Where |
|---|---|---|
| **Primary** | Near-white capsule (`text` fill, `#0a0b0d` label), 52 pt (56 pt pinned), semibold 17. One per screen, the next step. | Join, Go live, Copy link, Try again |
| **Glass** (secondary) | `.glassEffect` capsule or 44 pt circle | Paste, Share, X, Picture in Picture, settings, full screen |
| **Tinted** | `accentSoft` fill, `accentLine` border, `accentText` label | Got it, Copy room link |
| **Danger outline** | `live` at 50 % border, `#f0686c` label. Ends something others depend on, never the loudest. | End broadcast, End room for everyone |
| **Chip** | 30 pt glass capsule, 13 pt | LIVE (red), RECONNECTING (amber), viewer count, streaming count, the code pill (mono 12) |
| **Code boxes** | Six 48 × 58 pt boxes, 8 pt gaps, radius 12, `s2` with a `border` stroke. Filled boxes stroke `faint`; the active box strokes `accent` with a 3 pt `accentSoft` ring and a caret. | Watch's join card, Live |
| **Grouped list** | Inset 16 pt, radius 22, `s1` with a `borderSoft` hairline, 54 pt rows, 30 pt icon tiles (`s3`, `muted` glyph; accent tile for an action) | Every list |
| **Banner** | Radius 20, at the top of the page (OD8). Info is `accentSoft`/`accentLine`; warning is `warnSoft`/`warnLine`. Its action is a small button inside. | First run, reconnecting, unreachable, the summary card |
| **Layout toggle** | A glass segmented control with icon and label | The room player's Grid / Focus |
| **Sheet** | System sheet with detents, glass, grabber, title centred, an X glass circle | Stats, room sheets, People, Add to a room |
| **Alert** | System alert, the preferred action as the primary | A refused start |

Over video, every control is glass: Liquid Glass's `.glassEffect()` in a
`GlassEffectContainer`. The tab bar and navigation bars are the system's
own glass. Icons are SF Symbols (§7 lists the ones the canvas stands in
for).

### 3.4 Motion and feedback

- The player's and room player's controls hide after **3 s** without a
  touch: the web's `CONTROL_IDLE_MS`, from `ViewerScreen.tsx` and
  `RoomScreen.tsx`. They fade over 220 ms on `(0.2, 0, 0, 1)`, the web's
  `--dur` and `--ease`.
- **A copy** turns the copy icon into a check with a 200 ms pop and tints
  the pill or boxes with the accent for 1.5 s. It plays
  `.sensoryFeedback(.success)` and posts a VoiceOver announcement ("Link
  copied", or "Code copied" for Live's code boxes). The copied text never
  replaces the code (OD4).
- **Reduce Motion** removes the pop and the fades; state changes stay.

## 4. Decisions

| # | Decision | Rationale |
|---|---|---|
| D1 | **Three tabs: Watch, Broadcast, Settings** (*revises docs/67 D1*). The system's floating glass tab bar, tinted `accent`. There is no Rooms tab: a room is joined from the Watch code box (D3, D4), a saved room or a link. The tab bar is out of sight whenever a player is up (D5). | A room code and a broadcast code are the same six characters, and the web joins both from one box (`JoinResolver.tsx`). A tab per noun made a placeholder tab and two ways to join. |
| D2 | **Dark only, on the gawk tokens** (§3). | Every gawk surface is dark (docs/10, docs/60 D2). |
| D3 | **Watch (A1).** Large title "Watch". A glass card, the web landing's: "Join a stream", "Type the code you were sent. Room codes work here too.", the six boxes, **Paste** (the system `PasteButton`, so iOS asks nothing) and **Join** (primary; enabled at six characters; Return also joins). `CodeField`'s sanitising is kept (`BroadcastCode`, the relay's alphabet). Under the card, **Your rooms**: saved rooms first (star), then recent ones, at most 8, as docs/60 D8 has. Swipe to save or remove. Footer: "A gawk link opens here and starts playing." The web landing's blurred accent and brand glow sits behind the card. | The front door is the web's, and the rooms you go back to are one tap away. |
| D4 | **Joining resolves a code as the web's `#/join` does.** A `RoomWatcher` dials the code. A room state means a room: open the room player (D19). A refusal or an end means it's not a room: open the player (D5), which says "Streamer offline" if nothing is there. If neither arrives within 8 s, open the player. The watcher is stopped before the hop, and the room player opens its own. A `gawk://watch/<ID>` link opens the player directly and `gawk://room/<code>` opens the room player, parsed by the core's `parse_gawk_link` (docs/68). A `gawk://broadcast` link prefills the Broadcast tab and never starts anything (docs/68 D4). A link's `relay=` for a non-default server shows that server: as the strip (D24) on Watch and Broadcast, and in the player as a glass server chip under the top row of controls. | One rule for typed codes, links and the web. A link must never start a broadcast. |
| D5 | **The player (A2, A3, A7)** is a full-screen cover on black with the video aspect-fit, in both orientations. With its controls hidden it shows **only the video** (OD1). A tap shows the controls, and they hide again after 3 s. **Top left**: X (leave; stops the player), the LIVE chip, then the viewer count chip (eye icon and number). **Top right**: the code pill (D5a), then Picture in Picture. **Bottom right**: settings (D6), then full screen. In portrait, full screen turns the window to landscape (`requestGeometryUpdate`); in landscape the button turns it back. There is no bar behind any of it (OD2), no volume control (OD3), no pause and no scrubber, because the stream is live. While reconnecting, the LIVE chip becomes an amber RECONNECTING chip with a spinner, and a stream server rollout (4002 drain) reads "Stream server is updating". Connecting shows a centred spinner and "Connecting to K7XQ2M…", the web's words. | YouTube and Twitch, which the owner named. The web viewer's idle rule and status words. |
| D5a | **The code pill** (OD4, OD12) is a 30 pt glass chip at the top right: the code in SF Mono 12 semibold, then a copy icon. It shows the code and a tap copies **the join link** (`gawk.ioio.fi/#/view/K7XQ2M`, the link Live's Copy link copies), with §3.4's copied state. Its label is "Copy link to K7XQ2M". | The owner's decisions: the pill and the room's code chip (D19) behave alike, and the web's code chip is its share control (`RoomScreen.tsx`). |
| D6 | **The settings menu (A4)** is a native `Menu` that opens upward from the bottom-right button: **Latency** (an inline picker: Balanced, Lowest latency, applied at once through `setPreset`), **Share…** (`ShareLink`, the system share sheet) and **Stats**. Copy link is the pill's (D5a). | Everything a viewer tunes, one tap deep and out of the way, and no action in two places. |
| D7 | **Stats is a drawer with two heights (A5, A5b)** (OD5). Opened from the menu, it's a sheet with a small detent of about 300 pt. There, four tiles (playout delay, jitter, round trip, watching) sit under the video, and background interaction stays on, so the video keeps playing above it. Pulled all the way, it adds **Frames** (completed, dropped, recovered by parity, gap resyncs, drops to live) and **Renderer** (video samples, audio blocks, video dropped, renderer resyncs), and a **Copy** button that copies the numbers as text. Values are formatted before display, and "—" means unknown. | The core's `ViewerStats` and the renderer's counters are the whole set. Nothing new is measured. |
| D8 | **Picture in Picture starts on its own** when you leave the app with the player up (`canStartPictureInPictureAutomaticallyFromInline`), and the PiP button starts it at once (*revises docs/67 D22*, which needed the button). Background audio is unchanged. | "Swipe home and keep watching" is the iOS habit, and the whole point of docs/67 G9. |
| D9 | **Offline and ended (A6).** A centred glass card over black: an icon, the title and the body from `WatchStatusText` ("Streamer offline" / "No one is streaming at code K7XQ2M right now.", "Broadcast ended" / "The stream is over.", "Broadcast ended by a moderator."), then **Close** (glass) and **Try again** (primary; ended-by-moderator has Close only). An unsupported codec gets the same card with "This player can't play this stream's video format (VP9)." X stays at the top left. | The web viewer's card, in the same place for every terminal state. |
| D10 | **Broadcast, ready (B1).** Large title "Broadcast". On first run only, an info banner: "**Your whole screen is broadcast, with its sound.** Notifications show too, so turn on a Focus to keep them off the stream." with **Got it** (*revises docs/67 D18's copy*). It replaces the separate "Your whole screen" card (OD7). Then **Quality** with a menu value, **Auto** (D12), and its line "1080p · 60 fps on Wi-Fi, 720p · 30 on cellular". Then **Room** with **Add ›** (D16). Pinned above the tab bar: **Go live** (primary, red dot) and "You'll get a code to send to friends." | One button, then one code (docs/60 D3). No sound row (OD6). |
| D11 | **Live (B2).** Top: the LIVE chip with the time live, and the viewer count chip. Then "Your code", the six boxes and the join link in mono. **A tap on the boxes copies the code**, and a long press offers Copy code, Copy link and Share (docs/64 D17's click-to-copy). Then **Copy link** (primary) with a Share circle beside it, and "Switch to your game. gawk keeps broadcasting in the background." Then the stream rows in the desktop's order (docs/64 D5): **Quality** (resolution, frame rate and the encoder's cap, with the menu, D12) and **Upload** (the send rate with a Steady or warning dot). Then the **Room** group (D17). Pinned: **End broadcast** (danger outline), with no confirmation: End is undone by Go live again (D14). | The code is what the broadcaster came for. End is reversible, so it doesn't ask. |
| D12 | **Quality can change while live** (answers the owner's question). The menu holds **Auto** (docs/67 D19's rule: Cellular on an expensive path at start, Standard otherwise), **Standard** (1080p, 60 fps, 8 Mbps peak) and **Cellular** (720p, 30 fps, 3 Mbps). A change while live applies at once: the shared engine restarts the publish leg on the same code (`Session::republish`, docs/64 D8, D13). Viewers see a short freeze and pick up again. The badge stays LIVE, and nothing is narrated (docs/64 OD4). The app still never switches quality on its own mid-broadcast (docs/67 D19). | The desktop's rule, through the desktop's mechanism. Auto is what the app does today. |
| D13 | **Reconnecting (B3).** The LIVE chip becomes an amber RECONNECTING chip with a spinner, and the viewer count dims. A warning banner at the top reads "**Connection lost.** Your viewers keep the code while gawk reconnects." The Upload row reads "Reconnecting · attempt 2", amber, with "—" for its rate. The code, Copy link and End stay as they are. | docs/60 D13 and docs/64 D1: status where it belongs, and Stop still available. |
| D14 | **After End (B4).** Ready, with the summary card as an info banner at the top: "Broadcast ended", then **time live**, **most watching** and **average upload**, a labelled **Dismiss**, and **Use a new code next time**. The primary reads **Go live again**, with "Viewers keep code K7XQ2M for a few minutes." under it. Every start reclaims the stored identity (docs/67 G6), so this is the same button as Go live, worded for what it does. "Use a new code next time" forgets the stored identity for this server. A room you were in stays pending ("Joins when you go live", with an X), as docs/60 D8 has. There's no countdown, because the relay's grace is not known to the app (docs/60 D11). | docs/64 D10's summary and undo. The "new code" exit is docs/60 D11's "start over with a new code". |
| D15 | **A Live Activity (B5–B7)** while broadcasting (*revises docs/67 §6*: a widget extension, `GawkLiveActivity`). **Lock Screen**: the mark, "gawk", the LIVE chip with a live timer (`Text(timerInterval:)`), "Your code" and the code in SF Mono 30, the viewer count, and **Copy link** (glass) and **End** (danger outline). **Dynamic Island**: compact is a red dot and "LIVE" leading, with the eye and count trailing; minimal is the red dot; expanded is LIVE and the timer, the viewer count, the code, then Copy link and End. The app starts it when a broadcast goes live, updates it on viewer-count changes (throttled to one update a second at most) and ends it when the broadcast ends. Its buttons are `LiveActivityIntent`s, which run in the app's process: End calls the same stop as the app's End, and Copy link puts the link on the pasteboard. | The broadcaster is in a game, not in gawk. The code, the audience and a way to stop belong where iOS puts that. §6 ruled out an extension because of the capture extension's process split. A widget extension holds no media, needs no App Group or shared Keychain, and only draws the state the app sends it. |
| D16 | **Add to a room (C3)**, the broadcaster's sheet: one field, "Room code or link" (with Paste), **Create a new room** ("You get a code for friends to join"), and **Your rooms** (saved first, then recent, at most 8; swipe to save or remove). Footer: "Your stream joins the room when you go live." A pasted link's `?rt=` grant is honoured as on the desktop (docs/60 D8). A gated static room asks for its key in an alert with a text field: "This room needs a key to add your stream". | The desktop's sheet (docs/60 D8), the same rules. |
| D17 | **Live in a room (C4).** The Room group shows the room's name, "3 streaming · 3 watching", up to three avatars and **Manage ›** (D18). Under it are **Copy room link** (tinted) and **Leave** (ghost). Live's primary stays Copy link (docs/64 OD2). | docs/64 D6: a room adds a card; it doesn't rearrange Live. |
| D18 | **The room sheet (C5).** Title: the room's name, with "Room CODE · You made this room" for its creator. **Copy room link**. **Streaming**: each stream with live or away and its viewer count; **Remove** for the creator (Detach with that stream's ID). **Watching**: the nicknames. **Leave room** (glass) and, for the creator, **End room for everyone** (danger outline). **There is no "Watch the room" button.** The phone broadcasts its whole screen, so an in-app room player opened while live would send the room, a mirror of your own stream included, to everyone watching you. | docs/60 D9 and docs/64 D16, minus the one action a whole-screen capture can't allow. |
| D19 | **The room player (C1, C2, C1b, C2b)** is the single-stream player with more tiles, laid out like the web room screen (OD9). Black, tiles edge to edge with 2 pt gaps, and controls that hide after 3 s. **Top left**: X (leave the room) and the streaming count chip ("3 streaming", counting away streams as the web does). **Top right**: the room code chip and **People** (D20). The code chip copies the room link, as the web room's does and as the stream's pill does (D5a). **Bottom left**: **Grid / Focus**, a glass segmented control. **Bottom right**: settings (Latency, Share…) and full screen, in the same place as on a single stream. Each tile has a name chip at the top left, with a red dot when live and an amber dot when away. An away tile reads "Mika is away" / "Their stream comes back here on its own." A tap on a tile focuses it.<br>**Grid, portrait**: one column of 16:9 tiles up to four streams, two columns above four. **Grid, landscape**: 2 × 2 up to four, three columns above four.<br>**Focus**: the focused stream where a single stream sits, with the others in a strip under it that scrolls sideways.<br>**At most four tiles play** (docs/67 D21). In Grid the others show their last frame with a play badge, and a tap swaps that one in, pausing the playing tile that was swapped in longest ago. A one-line hint explains it: "Four play at once. Tap a paused stream to play it instead." In Focus the strip's tiles hold their last frame and only the focused one decodes. | The web room's anatomy and toggle (OD9), the single-stream player's chrome, docs/67 D21's decode budget. |
| D20 | **People (C2c)** is the web room's people panel as a sheet, opened by People. Title: the room's name and "Room CODE". **Streaming**: dot, name and viewer count; a tap focuses that stream. **People**: avatar, name, Streaming or Watching. Your own row reads "Sam (you)" with **Edit**, which opens an alert with a text field. The new nickname (at most 32 bytes, the wire's `MaxRoomNicknameLen`) goes to the room as the web's does (`SetNickname`, 0x03) and becomes Settings' nickname. This is where the "Watching as …" bar went (OD9). | The web panel's sections (`RoomPanel.tsx`), without its chat placeholder. |
| D21 | **The phone never shows a player while broadcasting without asking.** Opening any stream or room from Watch, a saved room or a link while live asks first: "You're live. Everything on screen is broadcast, so this stream would be too." with **Watch anyway** and **Cancel**. | D18's reason holds for every entry point, not only the room sheet. |
| D22 | **Settings (D1).** **Server**: one row per server with its probe status ("Checking…", "Connected · 24 ms" with a green dot, "Can't reach server" with an amber dot), a checkmark on the selected one, and an info button that opens Edit (D23). A tap on a row selects it. Then **Add a server** (accent action row). Footer: "The default is the official gawk fleet. A publish secret is only sent to the server it was saved for." **You**: Nickname (inline field; "Shown to others in a room."). **Privacy**: Send diagnostics (switch; "Off by default. Session statistics go to the server's operator, never what's on your screen."). **About**: Version and Terms of use (opens in Safari). Debug builds keep their Development section. | docs/64 D15: every server has Edit, and the secret lives there (*revises docs/67 D23's inline field*). |
| D23 | **Edit server (D2).** A header with the server's icon, name and probe status. **Server**: Name and Relay, locked with a lock glyph for the built-in server, editable for a saved one, which also gets **Delete server** (destructive row). **Publish secret**: a field with Paste. It stays a `TextField` with `.privacySensitive()`, not a `SecureField`, because a `SecureField` makes iOS offer to save an API key as a password (the comment in `SettingsScreen.swift`). Footer: "Only needed if the server asks for one. It stays in your Keychain and is sent to this server only." | docs/64 D15, D18. |
| D24 | **Another server and an unreachable one (D3).** When the selected server isn't the default, Watch and Broadcast show a strip under the large title: the server icon, "Using Home relay", the host in mono and **Change ›** (Settings). When its probe fails, a warning banner sits at the top of the page (OD8): "**Can't reach relay.example.net.** You may be offline, or UDP to port 4433 may be blocked." with **Try again**. **Go live stays enabled.** | docs/40's persistent strip and docs/64 D2: the probe can be wrong about a path the publish would take. |
| D25 | **A refused start (D4)** is a system alert, "Couldn't go live", with the reason the app already has (`BroadcastSession`: a refused secret, an expired code, a code live elsewhere, a full server, an operator's end) and one next step where there is one. A refused secret offers **Edit secret** (opens D23). The others have OK. | docs/67 §7: a full fleet or a refused secret says which. |
| D26 | **iPad.** The same three tabs (iPadOS draws the tab bar at the top), with page content at most 640 pt wide and centred. The players are full screen. The room grid fits the extra width: three columns above four streams in portrait, four in landscape. Sheets are form sheets. | docs/67 OD6 includes iPad. Not drawn on the canvas, so this text is the spec. |
| D27 | **Accessibility.** Every control has a label, including the icon-only glass buttons, the code boxes ("Code K7XQ2M, copy") and the pill. Copies and status changes are announced. Lists and sheets grow with Dynamic Type up to AX5. The code boxes keep their size and are read as one element. Text meets 4.5:1 (`muted` on `bg` is about 7:1); `faint` is never text. Reduce Motion is honoured (§3.4). | The design is only finished if everyone can use it. |

## 5. What the design needs that doesn't exist yet

Built in this milestone (OD10), each in a chunk of §8:

| # | Capability | Where | Chunk |
|---|---|---|---|
| K1 | **The room's people for viewers**: each participant's nickname, kind (streaming or watching) and the attachments, in `RoomWatcher`'s `RoomView`, and **set nickname** on the room. The engine's room state carries participants and nicknames (docs/60 DR1); the core maps only tiles and a count today. | core `rooms.rs` | IX6 |
| K2 | **A structured room summary for the broadcaster**: people, attachments, the creator flag and the room's name, in place of `on_room(text)`. Plus **Remove** (Detach by ID), **End room**, **Leave room**, **Create a room**, the gated-room key prompt and the `?rt=` grant. All of these exist in the engine (docs/60 DR1, docs/64); the core doesn't expose them. **Saved and recent rooms** as records (code, name, saved, last joined; an attach key in the Keychain), migrated from `AppSettings.recentRooms`' strings. | core `broadcast.rs`, `AppSettings`, `IdentityStore` | IX5 |
| K3 | **Quality while live**: `Broadcast::set_quality` over `Session::republish` with the new rung. | core `broadcast.rs` | IX4 |
| K4 | **Upload rate and steadiness**: the engine's send rate and its uplink state, in `BroadcastCounters` or an event. Today the counters have frames and size but no bytes. | core `broadcast.rs` | IX4 |
| K5 | **The summary's numbers**: time live, most watching and average upload, kept by the Swift session from events, as pure functions with tests (docs/60 DR3's shape). | `BroadcastSession` | IX4 |
| K6 | **Forget the stored identity** for one server ("Use a new code next time"). | `IdentityStore` | IX4 |
| K7 | **The join resolver** (D4): a `RoomWatcher` probe with the 8 s guard, as a tested function. | `Watch/` | IX2 |
| K8 | **`gawk://` handling**: `onOpenURL` → `parse_gawk_link` → the player, the room player or Broadcast's prefill. A dropped parameter is named in a notice, never its value (docs/68 D1). | `GawkApp.swift` | IX2 |
| K9 | **The Live Activity**: the widget extension target in `project.yml`, `NSSupportsLiveActivities`, the attributes and content state, two `LiveActivityIntent`s, and start, update and end from `BroadcastSession`. | new target | IX8 |
| K10 | **The token drift check**: `gawk-ios/scripts/check-theme.sh` compares `Theme.swift`'s values with `global.css` and the `main.slint` additions, and runs in `ios.yml`. | CI | IX1 |
| K11 | **The pinned server chip in the player** for a link's non-default `relay=` (D4). | `Watch/` | IX3 |

## 6. What does not change

Capture, encode, audio, transport, the viewer core's playout, the wire and
the relay. No wire change, and no relay change. The Safari viewer and the
desktop apps are unchanged. Every `AppSettings` key keeps its meaning.
`recentRooms` is rewritten to records at launch (K2), and an old value
loads as unsaved recent rooms. The R65 acceptance criteria (docs/67 G1–G12)
still hold. G11's second half (the iOS viewer plays a room in grid and
focus) is met by IX6 on this design.

## 7. Differences from the canvas

| Canvas | Built | Why |
|---|---|---|
| Icons are hand-drawn stand-ins | SF Symbols: `play.rectangle`, `dot.radiowaves.left.and.right`, `gearshape`, `xmark`, `pip.enter`, `arrow.up.left.and.arrow.down.right` / `…down.right.and.arrow.up.left`, `doc.on.doc`, `checkmark`, `person.2`, `square.grid.2x2`, `rectangle.split.3x1` (focus), `eye`, `link`, `square.and.arrow.up`, `server.rack`, `lock`, `info.circle`, `bell.slash`, `exclamationmark.triangle`, `wifi.slash`, `arrow.clockwise`, `star.fill` | The canvas can't draw SF Symbols. |
| Glass is CSS blur | Liquid Glass (`.glassEffect`), the system tab bar and sheets | The real material. |
| The status bar, keyboard, home indicator and system picker are absent | Drawn by iOS | They are iOS's. |
| Stats, counts and names are examples | Live values | — |
| No iPad, no landscape room, no player server chip, no "watch while live" prompt | D26, D19, D4, D21 | Specified here instead. |

## 8. Chunks and acceptance criteria

IX1 lands first. IX2–IX7 build on it in any order, except that IX6 needs
IX2's resolver. IX8 needs IX4. IX9 runs over everything, and IX10 last.
Every chunk keeps `ios.yml` green: the Simulator tests run on GitHub's
`xcode-27` image (docs/67 D24). Bug fixes found on the way are test-first
(`CODE-REVIEW.md`).

| Chunk | What | Accepted when |
|---|---|---|
| **IX1** | **Design foundation**: `Design/Theme.swift` (§3.1), dark only and the tint (D2), and the primitives as SwiftUI views with previews: primary, glass, tinted and danger buttons, the glass circle, the chip, the code pill with its copied state, the code boxes (input and display), the grouped-list row, the banner and the section header. K10's drift check. | The drift check is green in `ios.yml`, and a fixture with one changed hex makes it fail. Every primitive has an Xcode preview that builds in CI. A unit test pins the copied state's timing (check at once, back after 1.5 s) and that the code text never changes. |
| **IX2** | **Tabs, Watch and joining**: D1, D3, D4; K7, K8; Your rooms (with K2's records). | UI tests (Simulator, local relay): typing six characters enables Join, and Paste fills the boxes; a room code opens the room player and a broadcast code opens the player; a code that is neither opens the player's offline card; the 8 s guard opens the player. `gawk://watch/<ID>` and `gawk://room/<code>` open the right screen, `gawk://broadcast?room=…` prefills Broadcast without starting it, and a dropped parameter is named without its value. `TypingUITests` pass on the new code box. |
| **IX3** | **The player**: D5–D9; K11. | UI tests: the controls hide 3 s after the last touch and a tap shows them; the code pill puts the join link on the pasteboard and shows the check, not text; the menu has no Copy link; the menu's Latency applies the preset; Stats opens at the small height with the video still interactive and pulls to full; X stops the engine (`WatchModel.stop`); offline, ended and unsupported-codec cards show their copy. A test pins that `canStartPictureInPictureAutomaticallyFromInline` is set. A `relay=` link shows the server chip. `WatchUITests` and `WatchSmokeTests` pass. |
| **IX4** | **Broadcast**: D10–D14; K3–K6. | UI tests on D27's test source and a local relay: Go live shows the code boxes and a tap copies the code; Quality changed while live keeps the same code (integration test: the broadcast ID and token are unchanged after `set_quality`, and a viewer re-primes on the new config); the Upload row shows a rate; End shows the summary with its three numbers (unit tests on K5's functions); Go live again gets the same code; after "Use a new code next time" the next start gets a new one. `OwnerFlowUITests` and `StopBroadcastTests` pass. |
| **IX5** | **Rooms for the broadcaster**: D16–D18, D21; K2. | Tests against a local relay: Create a new room → Live shows the room card; a code or a link with `?rt=` joins on Go live; a gated room asks for its key; Remove sends Detach for that stream and the session stays up; End room for everyone ends it with the creator's reason; Leave room returns to Not in a room. The room sheet has no room-player entry, and opening a stream or a room from Watch or a link while live asks first. |
| **IX6** | **The room player**: D19–D20; K1. | UI test with five `gawk-devpub` publishers in one room: Grid shows five tiles in two columns, four playing; a tap on the paused one plays it and pauses the oldest; Focus shows the strip and decodes only the focused tile (a renderer counter test); People lists the participants, and Edit changes your nickname in the room (the roster shows it) and in Settings. An away stream shows its card. Landscape lays out 2 × 2 at four. |
| **IX7** | **Settings and the edges**: D22–D25. | UI tests: a tap selects a server; info opens Edit; the built-in server's Name and Relay are locked; a secret saved for one server is never sent to another (`SettingsTests`, extended); a non-default server shows the strip on Watch and Broadcast; an unreachable probe shows the banner with Go live enabled; a 401 start shows "Couldn't go live" with Edit secret. |
| **IX8** | **The Live Activity**: D15; K9. | In the Simulator, a test broadcast starts an activity, a viewer joining updates its count, End from the activity's intent ends the broadcast, and the activity ends with it. The extension builds in `ios.yml` and links no gawk Rust code. |
| **IX9** | **iPad and accessibility**: D26, D27. | UI tests on an iPad Simulator: content is at most 640 pt wide, and the room grid uses three columns above four. `XCUIApplication.performAccessibilityAudit()` passes on every screen. Lists lay out at AX5 without truncating a label. |
| **IX10** | **The owner's pass** on the iPhone 17 Pro Max and the iPad Pro, with docs/67's phase D. | Owner-run. The first broadcast takes Go live and the system picker, then the code. Copying the code, the link and the room link works. Quality changes while live on the same code. The Live Activity shows on the Lock Screen and in the Island, and End there ends the broadcast. Watching hides everything but the video, and the controls come and go. PiP starts when going home. A room of three plays in grid and focus. Results recorded in §10. |

## 9. Open questions

None. The one raised while drafting, whether the stream's pill and the
room's chip copy the same thing, was answered by OD12. The Live Activity
was confirmed as OD11.

## 10. Deviations and field findings

Recorded here, dated, as chunks land.
