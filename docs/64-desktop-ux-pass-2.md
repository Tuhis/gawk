# R62 — Desktop broadcaster UX pass 2 (docs/64)

**Status**: designed 2026-09-30 in a Claude Design pass; the owner's
decisions OD1–OD3 (§2) taken the same day, OD4–OD10 from the owner's
canvas comments the same evening. Chunks **DX1–DX6** (§6): DX1–DX5
implemented 2026-10-01 (§6a records how); DX6, the owner's hardware pass,
open. D6 and D9 revised 2026-10-02 by R64
([docs/66](66-desktop-scrolling-and-window-fit.md)). Status lives in
[`ROADMAP.md`](../ROADMAP.md).

**Relationship to earlier work**: a second pass over the window R58 built
([docs/60](60-desktop-redesign.md)). It keeps that milestone's pages, tokens
and primitives, and revises three of its decisions: D3 (the primary action
in a room), D11 (the stopped summary page) and D12 (the crash-resume page),
each with a dated note there. The server indicator is the desktop form of
[docs/40](40-relay-server-picker.md) §4.3. The restart reuses the resume
path of [docs/05](05-resilience-deploy.md) and R17's resume token.

---

## 1. Why

The owner raised three problems with the R58 window:

1. **The server pill misleads.** The header shows `● gawk.ioio.fi ⌄`. The
   green dot is drawn unconditionally (`main.slint`, `dot: T.ok`): nothing
   checks the relay while idle. The ⌄ promises a menu, but a click opens
   Settings, which the gear beside it also does. Alternative servers are
   rarely used. What a broadcaster does want to know is whether the relay
   is reachable, and whether they are about to go live somewhere other
   than the default. The web app already says the second (docs/40 F2); the
   desktop app doesn't.
2. **There is no pause.** A stop replaces the Ready page with the stopped
   summary. Its only actions are **Go live again on CODE** and **Done,
   start over with a new code**. To change what you share, the quality or
   the room and keep the code, you'd have to get back to Ready with the
   code held, and no path does that. Changing the source while live works
   only on macOS.
3. **A room rearranges Live.** In a room, the Sound, Room and Connection
   rows are replaced by the roster, and sound and upload rate shrink to a
   footer line. The code boxes shrink. The white button becomes Copy room
   link. The watching pill says "on you" instead of "watching". Joining a
   room should add the room, not move everything else.

A heuristic review of the whole window (Nielsen's ten) found more of the
same kind:

| Finding | Heuristic |
|---|---|
| Settings are disabled while live under a banner that says they "apply to the next broadcast". The uplink banner's **Open quality settings** leads to those disabled controls. | Consistency; error prevention |
| The upload cap is set in two places with two ranges: a 2–40 Mbps slider in Settings and a 1–100 field in Advanced. | Consistency |
| Custom size is chosen in Settings (**Custom**) but typed in Advanced ("Custom size is set in Advanced"). | Recognition over recall |
| The update check is a row in Settings and a checkbox in Advanced. | Minimalism |
| Three screens bring back a stopped broadcast: the crash page, the summary page, and the error card's button. They look and read differently. | Consistency |
| **Leave room** is in the Live footer and in the room sheet. Managing a room is a second sheet. | Minimalism |
| "Add" on Ready vs "Add to a room" on Live; "N watching" vs "N on you". | Consistency |
| macOS changes the source from a pill over the preview. Windows can't change it while live. | Consistency across platforms |
| The Windows capture target is read from the picker tab being *viewed*, not from the selection (§3 D14). | Visibility of system status; a bug |

## 2. The design pass and the owner's decisions

Drawn in Claude Design on 2026-09-30: window frames in four rows (Ready
and the server; Live alone, in a room, wide, after leaving a room, and the
click-to-copy states; pause, change and resume; Settings, Edit server and
the room sheet), plus an **Element rules** board, all on the web app's
tokens. The owner's comments on the first draft the same evening became
OD4–OD8, and the canvas was redrawn to them.

- **Canvas**: <https://claude.ai/artifact/92kWnb4UYTprbkzy5s71f7>, owned by
  the maintainer. It is **private**: ask the maintainer for access or an
  export. Its absence is not "no design exists".

Where this document and the canvas disagree, this document wins.

| # | Decision (owner, 2026-09-30) |
|---|---|
| OD1 | **Changing the source or the quality while live is a quick restart on the same code.** A second or so of frozen video is acceptable. No in-place swap on Windows or Linux. macOS keeps its existing in-place re-pick. |
| OD2 | **In a room, Live's primary stays the broadcaster's own Copy link.** Copy room link is an accent button in the room card. Revises docs/60 D3. |
| OD3 | **Stop becomes Pause and End.** Pause keeps the code. End goes to Ready with a summary card and an undo. |
| OD4 | *(Canvas comment.)* **The restart is not narrated.** Pressing Switch switches. No copy about pauses or codes, and the badge stays LIVE. |
| OD5 | *(Canvas comment.)* **Paused is the Live view.** Only the badge and the buttons differ from Live. The first draft drew Paused as Ready plus a code card, and the owner rejected it. |
| OD6 | *(Canvas comment.)* **Leave room is in view** on the room card, not only inside a sheet. |
| OD7 | *(Canvas comment.)* **No Advanced page.** Every server, the official one included, has **Edit**, and the publish secret is set there. |
| OD8 | *(Canvas comment.)* **The summary card looks like a passing notice**, with its dismiss control labelled. |
| OD9 | *(Canvas comment.)* **Click to copy: code option 1 and link option A** (canvas board 2d, D17). |
| OD10 | *(Canvas comment.)* **Settings splits in two** (D18): the Quality row's **Change ›** opens a **Quality** page, and the gear opens **Settings** with Server and App. |

## 3. Decisions

| # | Decision | Rationale |
|---|---|---|
| D1 | **The header states the server's status. It isn't a control.** On Ready and the Settings pages, a status line in the header reads **Checking…**, **Connected · 24 ms** or **Can't reach server**, with the dot grey, green and amber. The dot is green only after a probe succeeded. The probe is the web picker's (docs/40 §4.4) over the engine's own QUIC stack: a WebTransport session on `/echo`, the median RTT of five echoed datagrams, and the relay's `RelayIdentity` if it arrives within 1.5 s. Connect timeout is 4 s, and every failure is one state. It runs at launch, when the selected server changes, on **Try again**, and every 30 s while idle. On Live and Paused it doesn't render: the broadcast badge and the Upload row say the same thing with better data. The pill and its ⌄ are removed. The gear is the one way into Settings. | A status that is drawn rather than measured is worse than none. The web app's chip carries no dot for the same reason (`servers.module.css`: "a coloured dot there would claim one it does not have"). One entry point to Settings removes a redundant, misleading control. |
| D2 | **Unreachable is a banner, not a lock.** When the probe fails, Ready shows a warning banner naming the host, with the likely cause (offline, or UDP to the relay's port blocked) and **Try again**. **Go live** stays enabled. | The probe can be wrong about a path the publish would take. A failed start still reports its own error. |
| D3 | **A non-default server gets a strip under the header.** On Ready, Paused and Live, whenever the selected server isn't the pinned default: "Using server: *name*", the host in mono, and **Change ›** to Settings › Server. It adds "Diagnostics from this broadcast go to this server's operator" when the diagnostics endpoint belongs to that operator (docs/40 D16). Before the first connection, that is known only from the configured address. The strip is not dismissible. It never renders on the default server. | docs/40 F2's in-session indicator, which the desktop apps never had. Amber is the web chip's prominent colour. |
| D4 | **One vocabulary of elements**, drawn on the canvas's Element rules board. **Primary** (near-white): one per page, the next step. **Secondary**: acts now. **Danger outline**: ends something others depend on, never the loudest. **Ghost**: low stakes. **Row link** (`Change ›`): the only control with a chevron, and it always opens a page or sheet. It reads **Add** when the row is empty, **Change** when it is set, and **Manage** for a room. **Pills** state facts and are never clickable. **Badges** say what the broadcast is doing (LIVE, PAUSED, RECONNECTING). One verb per action everywhere: Go live · Pause · Resume · End; Copy link · Copy code · Copy room link. "Watching" always means the broadcaster's own stream. | The pill that navigated and the "on you" label are what this rule would have caught. |
| D5 | **The same rows, in the same order, on every page.** A stream list and then a Room list, always separate. Ready: *Sound · Quality*, then *Room*. Live and Paused: *Sharing · Sound · Quality · Upload*, then *Room*. The Room list is the only part of the page that changes when a room is joined. | Ask 3. A broadcaster who has learned where the upload rate is finds it in the same place in a room. |
| D6 | *(Revised 2026-10-02, [docs/66](66-desktop-scrolling-and-window-fit.md) OD1, D2–D3: the footer becomes a pinned action bar, **Copy link** · **Copy code** · **Pause** · **End**, and **Details** becomes the Upload row's **Details ›**.)* **Live is one layout, alone or in a room.** The code boxes stay full size, and **Copy link** / **Copy code** stay the page's buttons (OD2). The Your stream list is unchanged. A room adds two things: the room pill in the header, and the **room card** in place of the Room list's single row. The card has a header (room code, "N streaming · M watching", **Manage ›**), the roster rows with **Watch**, the watchers row, and an actions row: **Copy room link** (accent), **Open room view**, then **Leave room** (ghost) at the right end (OD6). **Leave room** leaves the Live footer and appears only here. After leaving, the Room list's row reads "Not in a room · You left CODE" with a **Rejoin** button beside **Add ›**. It is an in-window line, not a toast (docs/47 D7), and it stays until a room is added or the broadcast ends. The footer is **Details** · **Pause** · **End**. | Ask 3. Revises docs/60 D3 (OD2) and D9's layout. D9's roster content and creator controls are kept. |
| D7 | **Sharing is a row on Live**, with **Change ›** on every platform. The macOS pill over the preview goes. The encoder badge over the preview moves into the Quality row ("1080p · 60 fps", "H.264 hardware · up to 12 Mbps"), so the preview is only a preview. | One place per fact. The pill was a clickable status element (D4). |
| D8 | **Restart on the same code** (OD1): one engine-and-shell operation that changes what the broadcast sends and keeps its identity. The media pipeline stops, and the publish session is re-established with the persisted broadcast ID and resume token. It then runs with the new source or the new quality. It is not a stop: no summary, no "Broadcast ended" notification, `wasLive` stays set, and the LIVE clock keeps counting. **It is not narrated** (OD4): no copy before or during it, and the badge stays LIVE. Only a restart that fails to resume surfaces, through the normal failed-resume path. Viewers see a short freeze and pick up again by themselves: a new publisher session invalidates the relay's caches, so they re-prime on the new keyframe and config (the resume path). **The room survives it.** The preferred mechanism keeps the engine `Session`, and so its room control session, and re-establishes only its publish leg, the way a network resume does. The room then re-attaches on the new generation (docs/44, `room.rs`). If the sender can't be fed by a new pipeline without a new `Session`, the fallback is stop plus resume, with the room rejoined automatically. DX2 picks one and records which. | OD1. One primitive serves source changes on Windows and Linux, quality changes while live everywhere, and Pause (D9). Its viewer-facing behaviour is the already-shipped resume. |
| D9 | *(Revised 2026-10-02, [docs/66](66-desktop-scrolling-and-window-fit.md) D2 and D17: "the footer" is now the pinned action bar, where **Resume** takes Pause's place and **Copy link** steps down. The overlay is kept as built.)* **Pause** (OD3, OD5) stops sending and keeps the broadcast's identity. **The Paused page is the Live page**, with two differences. First, the badge: a neutral **PAUSED** with the time since the pause, in place of LIVE (the watching pill is hidden, because no count arrives without a publisher). Second, the buttons: **Resume** takes Pause's place in the footer as the page's white primary, and **Copy link** turns secondary. Everything else stays as it was on Live. The preview holds the last frame, dimmed, under "Paused". The code and the link are there. Every row keeps its **Change ›**, so the source, sound and quality can change before resuming. The Upload row reads "Paused · Nothing is sent until you resume". **In a room, the pause keeps the room**: the room card stays, and the broadcaster's own roster row reads "Paused". Mechanically this is D8's split: the media pipeline and the publish leg close, and the engine `Session`, with its room control session, stays up. The relay shows the attached stream as away, and Resume re-establishes the publish leg with the resume token, which re-attaches it. If DX2 falls back to stop plus resume, the room becomes the pending room (docs/60 D8) and Resume rejoins it. No countdown, because the app doesn't know the relay's grace (docs/60 D11). A pause is not persisted: quitting while paused asks nothing, and the next launch is plain Ready. | Ask 2, and the owner's comment that a Paused page different from Live was ugly: a pause is the same broadcast, resting. |
| D10 | **End** goes straight to Ready. The docs/60 D11 summary becomes a dismissible **summary card** at the top of Ready: time live, most watching, average upload, what was sent. It has the info banner's accent tint and border, its heading in the accent text colour, and a labelled **Dismiss** button (OD8). Its last line is "Ended by mistake? **Resume on CODE ›**", the same resume, available until the card is dismissed or a new broadcast starts. The primary is **Go live**, captioned "You'll get a new code". *Revises docs/60 D11: the summary page is retired.* | The summary no longer hides the page, and the undo covers a misclick on End. |
| D11 | **Crash recovery is the Paused page** (revises docs/60 D12). If the app starts with `wasLive` set, it opens on Paused with an info banner, "gawk closed while you were live", in place of the separate question page. There's no held frame (the preview shows only "Paused") and no room session: the room the broadcast was in is the pending room, and Resume rejoins it. **Resume** and **End** mean what they mean after a pause. | Paused, the summary page and the crash page brought a broadcast back in three different ways. Now it's one. |
| D12 | **Changing the source while live.** *Windows*: the picker page opens from Live. It has the LIVE badge, "SHARING NOW" on the current source, the audio note, and **Switch to *name*** as its primary (with **Cancel**). Switching is D8's restart, unannounced (OD4). While paused, the same page has **Share this** and nothing restarts. *Linux*: **Change** opens the portal picker, then D8 — for a window pick with the whose-audio choice the pick preselects (the last app used, docs/39 D5), since the sheet is Ready's and Paused's. `share-picker-available` is true while live. *macOS*: unchanged, the existing in-place re-pick (`pipeline.rs::repick`), now reached from the Sharing row. | OD1. Every platform has the same entry point. The one that can swap in place keeps doing so. |
| D13 | **Quality is editable while live, and a change applies right away** through D8's restart. It is debounced: one restart about a second after the last change, and the slider applies on release. There's no Apply button and no banner (OD4). While paused or idle, changes apply at the next Resume or Go live. The uplink banner's action becomes **Change quality**, and opens the Quality page (D18), whose controls now work. In Settings, the **server list stays disabled while live**, with the reason: a different server is a different code. | The old page contradicted itself (§1). A broadcaster who changes the quality expects the stream to change. |
| D14 | **The selected source is one value, independent of the visible picker tab** (*bug, fixed test-first*). Today `prepare_start` (Windows, `app-windows/src/main.rs`) and `current_source_key` (`shell.rs`) read `picker-tab` to decide between the selected window and the selected display. The source card reads it too. Choose a window, open the picker, click **Whole display** and press Back: Ready shows "Nothing chosen yet", and **Go live** fails with "Pick a screen (or a window) to share first." The selection is one value (kind plus key). The picker edits a draft of it: **Share this** (or **Switch**, D12) commits the draft, and Back or **Cancel** discards it. Today a row click changes the live selection at once, so Back cannot cancel. The tab is only the view. | Found in this pass; fixed test-first in DX3 (§6a). Back meaning cancel is what a page with a commit button leads users to expect. **Amended 2026-10-06 (owner):** a row click now commits at once, as Share this / Switch would (re-clicking what is already shared only closes the page); Back and Cancel still leave the source alone, and the selection stays independent of the tab. The page opens on Apps and games until the user looks at a tab, then on the tab last looked at (`pickerView` in the settings). |
| D15 | **No Advanced page, and no setting in two places** (OD7). *Servers*: every row in Settings › Server has **Edit**, the pinned default included, which opens the **Edit server** page. For the default, the name and relay address are shown as built in and locked, and the **publish secret** is editable, which is its credential slot (docs/40 F4). For a saved server, the name, relay address and secret are editable, with **Remove**. Both have **Test connection**: D1's probe on demand, showing the RTT and the name the relay calls itself (`RelayIdentity`). *Global settings*: the web address for join links (`appUrl`) and the diagnostics address (`telemetryUrl`) belong to no one server, so they become two rows in Settings › App, **Join links** and **Diagnostics**. Each has **Change ›**, which opens a small sheet with the field, its blank-means-default rule and, for Diagnostics, **Copy diagnostics** (also in Live's Details). *Quality* (on its own page, D18): the upload cap is the slider only (2–40 Mbps). A value from an older file outside 2–40 is kept, and shown as a number beside the slider, until the slider is moved. Custom size is two fields inline under **Custom**. *Updates*: one switch plus **Check now**. | §1's duplicates. The owner found Advanced and Add a server overlapping: both were really about servers. |
| D16 | **One room sheet**, opened from **Manage ›** on the room card. The sheet of an active room (name, open room view) and the Manage sheet merge. Title: "Room CODE", plus "You made this room" for its creator. **Copy room link** (accent) and **Open room view**. The name row with **Change name**. **Streams**, with **Remove** per stream for the creator. For the creator, **End room for everyone** (danger). **Leave room** is not here: it's on the card (D6). The **Add to a room** sheet is unchanged. | Two sheets for one room, and Leave in two places (§1). |
| D17 | **The code and the join link copy on click** (OD9). *The code*: the six boxes are one copy target. Hover and keyboard focus draw an accent tint and outline around the group (with the box borders in the accent line colour), a copy icon after the last box and a "Click to copy" tooltip (added 2026-10-01 at the owner's request, matching the link). Focus adds the focus ring. A click copies the code, and for 1.2 s the icon becomes a green check under a "Copied" tooltip. *The link*: hover gives the mono link text an accent-tinted pill, a trailing copy icon and a "Click to copy" tooltip. A click copies the link, and for 1.2 s it shows a check and "Copied". It has the same keyboard focus treatment as the code. Both work wherever the code and the link are shown: Live and Paused. The **Copy link** / **Copy code** buttons stay: the hover is a shortcut, and the buttons are the obvious path. | The owner's comments: both look copyable but gave no cue. |
| D18 | **Two pages, two ways in** (OD10; *revises docs/60 D6*). **Quality** holds resolution (with custom size inline), frame rate and the upload cap. It is opened by the Quality row's **Change ›** on Ready, Live and Paused, and by the uplink banner's **Change quality**. While live, its header carries the LIVE badge. **Settings** holds Server (the list with **Edit** and **Add a server**) and App (updates, Join links, Diagnostics), and the terms line. It is opened by the gear, and on macOS by **Settings… ⌘,**. Neither page links to the other. | The owner's split: quality is something a broadcaster tunes for this stream, and servers and app settings are set once. Each entry point now opens exactly what its row or icon names. |

## 4. What does not change

The wire, the relay, capture, encode and audio. The session lifecycle
changes only by D8's composition of existing stop and resume steps.
Telemetry, the uplink monitor, the room protocol, the identity latch and
every config key keep their meaning. The upload cap's accepted range stays
1–100; D15 changes only how it is edited. No new config key is needed: the
probe result is not persisted, and a pause is not persisted (D9).

## 5. Differences from the canvas

| Canvas | Built | Why |
|---|---|---|
| Sample data (names, codes, rates, the game preview) | Real values | Illustrative only. |
| Edit server's Test connection shows `24 ms · calls itself "gawk"` | The probe's RTT and `RelayIdentity` display name when one arrived, else the RTT alone | A relay that predates `RelayIdentity` sends none (docs/40 §4.4). |

## 6. Chunks and acceptance criteria

| Chunk | What | Acceptance |
|---|---|---|
| **DX1** | Engine: `probe(relay_url, origin, insecure) -> ProbeResult` (D1): a WebTransport session on `/echo`, five echoed datagrams 120 ms apart, the median RTT, `RelayIdentity` read until 1.5 s after the session is ready, 4 s connect timeout, one `Failed` state. Behind the same dialer seam as the session so tests script it. | Unit tests with a scripted connection: the median of five samples is reported; an identity arriving inside the deadline is returned, and one after it is not; a connect timeout, a refused session and zero successful echoes are each `Failed`. The integration suite probes a real relay and gets `Ok` with an identity. |
| **DX2** | Engine and shell: the publish leg separated from the `Session` (D8, D9). Pause closes the media pipeline and the publish leg and keeps the `Session` and its room control session. Resume and restart re-establish the leg with the resume token. The mechanism chosen (or the stop-plus-resume fallback) is recorded in this doc. | Tests: a restart keeps the broadcast ID and the resume token; shows no summary and sends no "Broadcast ended" notification; keeps `wasLive` set and the LIVE clock running; sends the new rung or source afterwards. A pause in a room keeps the room session: the roster still lists the broadcaster, as away, and Resume re-attaches without the user acting, with the creator grant intact. A restart or resume that fails ends like any failed resume, with its error. |
| **DX3** | Shell view logic, as pure functions with tests. The Live → Paused → Live and Paused → End → Ready transitions, End's undo, and the crash variant with its pending room (D9–D11). The Rejoin line after leaving a room (D6). The probe schedule and the status text (D1–D2). The non-default strip and its disclosure (D3). **The selection model fix (D14), test first.** The debounced apply of a quality change while live (D13). The upload cap out-of-range rule (D15). | `shell.rs` tests for each. D14's test fails before the fix: select a window, switch the tab to Whole display without choosing, and the start still resolves the window. D13's test: three quality changes inside the debounce window make one restart. The existing shell tests stay green. |
| **DX4** | The window: `main.slint` per D1, D3–D7, D9–D13 and D15–D18 on the existing primitives. Removed: the server pill, the preview pills, the summary page, the crash page and the Advanced page. Added: the room card with Leave room, the Paused variant of Live, the summary card, the Quality page split from Settings (D18), the Edit server page, the Join links and Diagnostics sheets, the merged room sheet, and click to copy on the code and the link (D17). Both shells' property and callback contract stays one file (docs/54 D11). | `cargo build`, host `cargo clippy -D warnings`, `cargo xwin clippy` and the Linux container build are green. Every callback the old window had is still wired, or its removal is listed in the PR. The `system-picker` card is still the only platform branch. |
| **DX5** | Platform glue (D12). Windows: the picker page from Live, **Switch** → restart. Linux: the portal re-pick while live → restart. macOS: the in-place re-pick from the Sharing row; a quality change while live → restart. | The Windows target clippy (CI), the Linux container tests and the macOS job are green. Which source a restart uses is decided in DX3's tested functions, not in platform code. |
| **DX6** | Manual pass on the gaming PC (Windows), a Mac and the Linux desktop. | Owner-run. The header status reads Connected with an RTT, and Can't reach server with the network off. A non-default server shows the strip on Ready and Live. Live in a room differs from Live alone only by the room pill and the room card. Paused differs from Live only by the badge and the buttons. Pause in a room → change the source → Resume keeps the code, the viewers and the room, and other participants see the stream away and back. **Change** while live switches the source with about a second's freeze on Windows and Linux, and in place on macOS, with no message. A quality change in Settings while live applies by itself. Leave room → Rejoin works. The official server's Edit page takes a publish secret. End → the summary card → **Resume on CODE** keeps the code. A killed app reopens on Paused with the crash banner. |

## 6a. How it was built (2026-10-01)

- **DX1** — `gawk-engine::probe`: `probe()` dials `/echo` through the
  transport's `dial`, and `probe_session()` measures any `RelaySession`
  (the seam the scripted tests drive on a paused tokio clock). The operator's
  name is sanitized: control and bidi-override characters dropped. The shell
  probes at launch, on a server change, on **Try again**, and every 30 s
  while idle; a probe of a server no longer selected is discarded and the
  selected one probed at once.
- **DX2** — the preferred mechanism, not the fallback. `Session::pause()`
  closes the publish leg cleanly (code 0) and parks the run loop, emitting
  `EngineEvent::Paused`; `Session::republish()` bumps an epoch, and a leg
  serving an older epoch hands over to the resume path's reclaim (first
  attempt at once, the loss delay only after). The room control session is
  never touched, and re-attaches on the new generation as after a network
  resume. `Sender::new_lineage()` drops the cached DecoderConfig and audio
  config so a rebuilt pipeline describes itself; the frameId space carries
  on. The reclaim dials through a new `PublishDialer` seam. Verified against
  the real relay: `a_pause_keeps_the_room_and_a_republish_resumes_the_same_code`.
- **DX3–DX4** — the shell drives Pause, Resume, the quick restart
  (`restart_media`, debounced 1.2 s for quality), the crash's Paused page,
  Rejoin and the probe; `main.slint` is rewritten on the same primitives.
  The picker edits a draft (`draft-tab`, `draft-window`, `draft-monitor`)
  that only a row click or Share this / Switch commits (D14, test-first:
  `looking_at_the_other_picker_tab_keeps_the_chosen_source`; the row click
  and the remembered tab: `clicking_a_picker_row_chooses_it`,
  `the_picker_reopens_on_the_tab_last_looked_at`).
- **DX5** — `Media::shutdown_keep_source()` hands a platform's source back
  on a pause or restart and `Platform::source_returned()` takes it: Linux
  returns the portal grant (capture rebuilds already reuse it, docs/58 D3),
  so Resume asks the picker nothing. `Platform::take_restart_request()`
  lets Linux's live pick ask for the restart. Windows and macOS keep their
  own selection and need neither. `Prepared` names the source for Live's
  Sharing row.
- docs/57's planned **Video delivery** switch ("Settings → Advanced") has no
  Advanced page to land on: it goes on the Quality page when it is built.

## 7. Verification

- DX1–DX3: the unit tests above, in CI's `cargo llvm-cov --workspace` on
  Linux and `cargo test --workspace` on `macos-latest`. DX1's integration
  test runs in the engine's relay integration suite.
- DX4–DX5: the Slint compile runs in every build. The Windows glue is linted
  by the `cargo xwin clippy` job, and the Linux shell builds in the
  `ubuntu:24.04` container (docs/58 D12).
- DX6: the owner's hardware pass.
