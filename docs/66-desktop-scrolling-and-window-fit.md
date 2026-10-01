# R64 — Desktop scrolling and window fit (docs/66)

**Status**: designed 2026-10-02 in a Claude Design pass, owner decisions
OD1–OD5 (§2) taken the same day. Chunks **WF1–WF4** (§6): WF1–WF3
implemented 2026-10-02 (§6a records how); WF4, the owner's hardware pass,
open. Status lives in [`ROADMAP.md`](../ROADMAP.md).

**Relationship to earlier work**: a third pass over the window R58 built
([docs/60](60-desktop-redesign.md)) and R62 revised
([docs/64](64-desktop-ux-pass-2.md)). It keeps their pages, rows, tokens and
primitives, and changes where the page's buttons live and how the window
sizes itself. It revises docs/64 D6 (the Live footer) and the wording of D9,
docs/60 D7 (the wide layout) and D13 (where notices go), and
[docs/65](65-source-preview.md) D2 (the preview card's height), each with a
dated note there. The window is still the one `crates/ui/main.slint` every
shell shows (docs/54 D11).

---

## 1. Why

On Ready, Live and Paused, everything under the header is one `ScrollView`,
the page's buttons included. The heights below are added up from
`main.slint`'s own sizes (rows 52 px, the code block 114, the preview 248,
gaps 12–14). They were not measured on a running build.

| Page | Needs | Window | What's out of view |
|---|---|---|---|
| Live, alone | ≈ 893 px | 520 × 800, the default | Pause and End start below the bottom edge as soon as you go live. |
| Live, in a room of three | ≈ 1,145 px | 520 × 800 | The room card shows only its header. Pause and End start ≈ 290 px down. |
| Ready, after End (summary card) | ≈ 793 px | 440 × 600, the minimum | Go live starts 61 px below the edge. |

The room case can't be fixed by size alone. A 1080p monitor's work area is
≈ 1,032 px, and a 1080p laptop at 150 % has ≈ 672. Some pages will always
scroll on some screens, so the buttons that matter can't be part of what
scrolls. Where the screen does have room, the window should use it.

## 2. The design pass and the owner's decisions

Drawn in Claude Design on 2026-10-02: the three failures above at their real
sizes, the rule (zones and action bars), three options for where Copy link
lives, the recommended design state by state, an interactive board that
simulates window fit, and the fit rules.

- **Canvas**: <https://claude.ai/artifact/SHetFtBTHWV4uFwQv1S8Zs>, owned by
  the maintainer. It is **private**: ask the maintainer for access or an
  export. Its absence is not "no design exists".

Where this document and the canvas disagree, this document wins.

| # | Decision (owner, 2026-10-02) |
|---|---|
| OD1 | **Option 2: Copy link joins the action bar**, with Copy code, Pause (Resume) and End. Details becomes the Upload row's link. Options 1 (pin today's footer) and 3 (a sticky code strip) were not taken. |
| OD2 | **Problems viewers feel now pin under the header**, in an alert slot. |
| OD3 | **Window fit is on by default, with no switch in Settings.** Resizing the window by hand overrides it. |
| OD4 | **The window remembers its size between launches.** |
| OD5 | **The Paused view as drawn**: the dimmed last frame with the pause icon and "Paused" over the preview, and the bar with Resume as the primary. The owner singled out the bar and the overlay. |

## 3. Decisions

| # | Decision | Rationale |
|---|---|---|
| D1 | **Four zones on the main page.** Ready, Live and Paused (page 0) each have a **header zone** (the header and, on a non-default server, docs/64 D3's strip), an **alert slot** (D4), a **body** and an **action bar** (D2). Only the body scrolls. The picker page already pins Cancel and Share this / Switch. Settings, Quality and Edit server have no bar, because their changes apply as they're made and their buttons belong to their rows. Sheets and modals keep their own layout. | §1. The buttons that matter must not depend on the window's height or the scroll position. |
| D2 | **The action bar, by state.** 64 px tall: 12 px padding around 40 px buttons. **Ready**: **Go live** (large, full width) with its caption underneath ("You'll get a code to send to friends.", and today's variants), 99 px in all. **Starting**: Copy link, Copy code, Pause and End, all disabled until they apply, as today. **Live**: **Copy link** (primary) and **Copy code** on the left; **Pause** and **End** (danger) on the right. **Paused**, the crash variant included: **Copy link** steps down to secondary and **Resume** (primary) takes Pause's place. **Under 500 px wide**, Copy code is a 40 px icon button whose accessible label and hover tip (the `Tip` primitive) say "Copy code". The copy buttons confirm in place: for 1.2 s the icon is a green check and the label reads **Copied**, with the button's width held. The Live page's green "copied" line under the buttons goes. | OD1. One primary per page (docs/64 D4) still holds: Copy link on Live, Resume on Paused, Go live on Ready. At 440 px, Copy link, the icon, Pause and End take ≈ 360 of the 400 px row. Confirming in the button keeps the bar's height fixed. |
| D3 | **Details is the Upload row's link** in the narrow layout: "Upload steady · 8.4 Mbps to *host*", the dot, then **Details ›**, which opens the Details sheet as the footer button did. The wide layout keeps its inline Details and its Upload row has no link. | OD1. The stats are about the connection, and a row link is how every other row opens more (docs/64 D4). |
| D4 | **The alert slot.** It holds at most one strip, and only on Live and while starting. The strip is 44 px tall in the server strip's amber, with 8 px below it. It isn't dismissible (docs/57 OD7). Priority, highest first: **Reconnecting** ("Reconnecting. People watching see your last frame.", no action); the **uplink warning** ("Your upload can't keep up" and **Change quality ›**); the **network notice** (its first sentence, e.g. "Your Wi-Fi is dropping some video", and **Help ›**). The last two keep docs/57 D7's order: the shell already holds the network line back while the upload one shows, because the upload's remedy is the one to read. The uplink warning's full text moves to the top of the Quality page as a banner, shown while the warning is active. That's where its fix is. Everything else stays at the top of the body, where it is now: the audio-silence and minimized-window hints, the crash banner, Paused's resume error and Ready's banners. | OD2. A problem that viewers see right now shouldn't scroll away while the broadcaster looks at the room. One strip bounds the fixed height: at 440 × 600 the zones take 172 px with a strip and the body keeps the rest. |
| D5 | **One elastic element per page** gives up height before anything scrolls. **Live and Paused (narrow)**: the preview, 160 px minimum, 248 natural (today's fixed height). **Wide**: the preview is 16:9 of the left column, at most 360 px (today's fixed height), 160 minimum. **Ready**: the source card, 120 minimum, 150 natural, 220 maximum; while it shows [docs/65](65-source-preview.md)'s preview, 160 minimum and its natural size (220, or 300 wide) as the maximum, so a picture keeps room and keeps its size. Its height is the body's visible height less everything else in the body, clamped to those bounds. Above the maximum, the spare height is empty space between the body's content and the bar. With this, Live alone fits the default 520 × 800 window with the preview at 213. | Without it, Live alone needs ≈ 893 px; with it, ≈ 747 at the preview's minimum. The preview is "what viewers see", a confirmation that tolerates being smaller. Nothing else on the page does. |
| D6 | **Scroll edges.** Once the body is scrolled, a 1 px `border` line runs under the header zone. While content continues below, a 1 px line and a 24 px fade sit over the bar. Neither shows when the page fits. The std `ScrollView` and its scrollbar stay. | The edges say "there is more" without a second affordance, and stay out of the way on a page that fits. |
| D7 | **The wide layout** (≥ 900 px): both columns share the one scroll, and the bar spans the window. The right column keeps the code, Your stream and the room. Its Pause and End move to the bar, and the left column keeps the preview (D5) and inline Details. *Revises docs/60 D7.* | The same bar in both layouts. One scroll is what the window has today, and two would put two scrollbars side by side. |
| D8 | **When the window grows** (OD3). The main page publishes two heights for the window's client area: `page-needs-min`, where nothing scrolls with the elastic element at its minimum, and `page-needs`, the same with it at its natural size. The window grows only when `page-needs-min` exceeds its *floor* (D9). It grows to `page-needs`, but no further than the work area allows: the screen less the taskbar, Dock or menu bar, less the window's own frame. The top edge stays put. If the new bottom would cross the work area, the window moves up just as far as it must, and never above the work area's top. Width never changes. | Growing to the natural size gives a full preview for the cost of a few more pixels, once. Growing only past the minimum means the elastic element absorbs small changes (a stream joining, a notice) without the window moving. |
| D9 | **When it shrinks: only on a change of state.** A change of state is going live, ending, joining a room or leaving one, i.e. a change in `(on air, in a room)`. The floor is the current height within a state, so nothing shrinks, and your size (D10) on a change of state. Pause and Resume, the quick restart, notices coming and going, streams joining and leaving, and the summary card's Dismiss are not changes of state. The window never shrinks below your size. | A roster that changes every few minutes must not make the window bounce. Leaving the room is the moment the room's height stops being wanted. |
| D10 | **Your size, and your resize wins** (OD3, OD4). *Your size* is the window's width and height as the user last set them by hand. A size change the shell didn't make sets your size, and then fit **waits** until the next change of state. The shell's own changes are those within 500 ms of its last `set_size`, plus anything while the window is maximized, full screen, minimized or arranged. Your size is saved as `windowWidth` and `windowHeight` (logical px, 0 = never set, meaning 520 × 800), one second after the last manual resize and at quit. Heights that fit chose are never saved. At launch the window opens at your size, raised to the 440 × 600 minimum. | Window fit must never fight the user. A broadcaster who drags the window short beside a game has said what they want, and the next launch should remember it. |
| D11 | **Navigation never resizes.** Fit runs only while the main page shows. If the state changes while another page is open (say a room ends while you're on Quality), the change applies when the main page shows again. Coming back without one changes nothing. Sheets and modals over the main page don't pause it. | The window jumping on Back would be noise. A pending change still has to land somewhere. |
| D12 | **Hands off** when the window is maximized, full screen or minimized, when it's snapped on Windows or zoomed on macOS, and when the platform can't say where the screen ends (no placement, D15). | The window manager owns those sizes, and resizing a snapped window un-snaps it. |
| D13 | **Launch fits.** The first window is clamped to the work area on the first turn of the event loop after it shows, on every platform: a window can't say where it is before it exists. Your size isn't changed by the clamp, so a bigger screen next time gets it back. | §1: a 1080p laptop at 150 % has ≈ 672 px for the whole window, and 800 used to open under the taskbar. |
| D14 | **Linux clamps at launch and remembers your size, but doesn't grow in v1.** Wayland tells a client nothing about where its window is. winit 0.30 exposes no work area on either X11 or Wayland, and xdg-shell's `configure_bounds` (the protocol for this) isn't reachable through it. Growing without knowing the window's bottom could push the bar off the screen, which is the one thing this milestone exists to prevent. Linux's launch clamp is the monitor's height (winit's `current_monitor`) less a 64 px allowance for panels. Growing on Linux is a follow-up once one of those becomes reachable. | The bars (D1–D2) already keep everything in reach on Linux. Fit is the enhancement, and it's only safe where the geometry is known. |
| D15 | **The platform says where the screen ends.** A new hook, `Platform::placement(&self, ui) -> Option<Placement>`, returns the window's outer frame, its client height and the work area. All three are in logical px with a top-left origin, the space `slint::Window::set_position` takes. It also returns whether the position is known and whether the window is maximized, full screen or arranged. The default is `None` (D12). **Windows**: the HWND through `raw-window-handle`; `GetWindowRect`; `MonitorFromWindow` and `GetMonitorInfoW`'s `rcWork`; `IsZoomed`; `IsWindowArranged`, resolved with `GetProcAddress`, with a `GetWindowPlacement` comparison where it's missing (§4.1); physical px to logical through the window's scale factor. **macOS**: the `NSWindow`'s `frame`, its screen's `visibleFrame`, `isZoomed` and the full-screen style mask, flipped from AppKit's bottom-left origin using the primary screen's frame. **Linux**: launch only (D14). | One shape for three platforms keeps the decision in the shell's tested pure code (D16), and the platform code only measures. |
| D16 | **Fit is a pure function in `shell.rs`.** It takes `page-needs-min`, `page-needs`, the client size, your size, whether the state changed, whether fit is waiting, and the placement. It returns the new client height and, when it moves, the new top. It runs on the shell's 250 ms UI tick and applies with `Window::set_size` (client, logical) and `set_position` (outer, top-left). Every move writes a `debug.log` line (from, to, and which rule) for the hardware pass. | Slint 1.17 applies a window's preferred size only when it first shows ([gotchas](gotchas.md)), so nothing grows unless the shell drives it. The repo's habit is decisions as tested pure functions (docs/60 DR3, docs/64 DX3). |
| D17 | **The Paused view keeps R62's overlay** (OD5, docs/64 D9). The preview holds the last frame under an 80 % dim, with the pause icon (26 px) and "Paused" centred. The crash variant shows the same overlay without a frame. Both scale with the elastic preview and still fit at its 160 px minimum. The bar is D2's Paused bar. | The owner's comment. The elastic preview is the one new thing that could squeeze it. |

## 4. What does not change

The engine, the wire, the relay and the room protocol. Every page's rows,
their order and their links (docs/64 D5) other than D3's Details link. The
header's contents. The Settings, Quality and Edit server pages, apart from
the uplink banner on Quality (D4). The picker page and every sheet and
modal. Every existing config key. The two new keys default to 0, so an old
file loads unchanged. A build that rewrites the file without them leaves
the window at its default size, which is harmless.

### 4.1 Implementation notes for the chunks

- **Slint applies `preferred-height` only when the window first shows**
  ([gotchas](gotchas.md), "The desktop window"). Hence D16.
- **An element inside an `if` can't be referenced by id from outside it.**
  The pages are `if root.page == 0 && …:` blocks. The heights D8 needs are
  computed inside the block and copied to root properties with `init` and
  `changed`. A test reads them from the root.
- **Compute the elastic height from the window's height** less the fixed
  zones and the rest of the body's preferred height. Don't compute it from
  anything that depends on whether the scrollbar shows. If the content's
  width depends on the scrollbar, a wrapping banner makes the loop
  preview → content height → scrollbar → width → banner height → preview,
  and Slint panics on the recursion. The WF1 tests would catch it.
- **The `ScrollView`'s `viewport-y`** (≤ 0 when scrolled), `viewport-height`
  and `visible-height` give D6's edges. "More below" is
  `viewport-y + viewport-height > visible-height`.
- **`IsWindowArranged` is documented only for recent Windows builds** (the
  docs give Build 20348 as the minimum). A statically linked import that the
  OS's `user32.dll` lacks stops the EXE from starting, so resolve it at run
  time. The fallback: a window that isn't maximized but whose
  `GetWindowRect` differs from `GetWindowPlacement`'s `rcNormalPosition` is
  arranged. That rect is in *workspace* coordinates (relative to the work
  area), so offset it before comparing.
- **`GetWindowRect` includes Windows 10/11's invisible resize borders**
  (≈ 7 px at the sides and bottom). Using it for the bottom-edge test is
  conservative: the window stops ≈ 7 px above the taskbar. That's fine, and
  it's the same space `SetWindowPos`, and so `set_position`, uses.
- **Slint features**: `raw-window-handle-06` for the Windows and macOS shells,
  and `unstable-winit-030` for the Linux shell's `current_monitor()`. The
  latter is tied to winit 0.30. A Slint bump that moves to another winit
  fails the Linux build loudly, so it can't break silently.
- **The macOS shell needs `objc2-app-kit`** (`NSWindow`, `NSScreen`,
  `NSView`) as a direct dependency. It is already in the lockfile through
  winit.
- **Don't bind anything that changes horizontal layout to the window's
  width**, and **the testing backend's element search needs Slint's debug
  info**. Both cost time in WF1; their home is the
  [gotchas](gotchas.md) ("The desktop window").

## 5. Differences from the canvas

| Canvas | Built | Why |
|---|---|---|
| Sample data, and heights added up from constants | Real values, measured by the layout | Illustrative. |
| The alert slot drawn with the uplink strip only | Three strips, one at a time (D4) | The canvas shows the common case. |
| The "Try it" board's fit logic | D8–D10 | The board approximates the rules with fixed heights. |
| "Staying on screen", panel 2: "Creating a room" grows to 983 | The real roster's height | The panel assumes a watchers row. |

## 6. Chunks and acceptance criteria

| Chunk | What | Acceptance |
|---|---|---|
| **WF1** | `main.slint`: D1's zones on Ready, Live and Paused; D2's bars, with the in-place copy confirmation and the icon-only Copy code under 500 px; D3's Details link; D4's alert slot and the uplink banner on Quality; D5's elastic preview and source card; D6's scroll edges; D7's wide layout; D17's overlay at the minimum. Root out-properties `page-needs-min`, `page-needs` and `body-scrolls` (§4.1). The shells' property and callback contract stays one file (docs/54 D11). | Headless tests (`i-slint-backend-testing`, `ElementHandle` by accessible label). **(a)** At 440 × 600, every bar control (Go live; Copy link, Copy code, Pause or Resume, End) lies wholly inside the window in each fixture. The fixtures: Ready with the error card, the can't-reach banner, the summary card and an update; Live in a room of eight with the server strip and an alert strip; Paused in a room with the crash banner; starting. **(b)** For Ready, Live alone, Live in a room of three, and wide in a room of three: at a client height of `page-needs-min`, `body-scrolls` is false, and 1 px less makes it true. At `page-needs`, the elastic element is at its natural size. **(c)** At the preview's 160 px minimum, Paused's icon and "Paused" lie inside the preview. **(d)** With reconnecting, the network notice and the uplink warning all set, the slot shows only the reconnecting strip. `cargo build`, host `cargo clippy -D warnings`, `cargo xwin clippy` and the Linux container build are green. Every callback is still wired, and the `system-picker` card is still the only platform branch. |
| **WF2** | Shell: the fit state (your size, waiting, the last `(on air, in a room)`, the last own resize) and the pure `fit()` and `launch_size()` (D8–D13, D16), driven by the 250 ms tick; telling manual resizes from the shell's own; `windowWidth` and `windowHeight` in `config.rs`, saved per D10. | `shell.rs` tests, one or more per rule. Grows only when `page-needs-min` exceeds the floor, to `page-needs`, capped at the work area less the frame. Doesn't move when the growth fits below. Moves up just far enough, never above the work area's top, and never when the position is unknown. Never shrinks within a state. On a change of state the floor drops to your size and D8 applies from there, so the window never ends below your size. A manual resize sets your size and makes fit wait until the next change of state. A resize within 500 ms of the shell's own isn't manual. Returns nothing when maximized, full screen, arranged, with no placement, or when another page shows. A change of state made on another page applies on return. `launch_size()` clamps a remembered 520 × 1200 to a 672 px work area and raises 300 × 300 to 440 × 600. `config.rs` tests: the keys round-trip, an old file loads them as 0, and 0 means 520 × 800. The existing shell tests stay green. |
| **WF3** | Platform glue (D14, D15): `placement()` on Windows and macOS, the launch clamp on all three, and the Slint features and macOS dependency of §4.1. | The coordinate conversions are pure functions with unit tests: physical to logical at 1.25 and 1.5; AppKit's bottom-left frame to top-left with two screens, including one above the primary; `rcNormalPosition` from workspace to screen coordinates with a taskbar on the left. `cargo xwin clippy`, the `macos-latest` job and the Linux container build and tests are green. |
| **WF4** | The owner's hardware pass on the gaming PC (Windows), a Mac and the Linux desktop. | **Everywhere**: at 440 × 600, Go live, Copy link, Pause and End are in view in every state, in a room too. Live alone at the default size doesn't scroll. The size you set survives a relaunch. **Windows and macOS**: joining a room grows the window to show the room card whole (or to the work area), and near the taskbar or Dock it moves up rather than running under it. Leaving the room returns it to your size. Resizing by hand sticks. A maximized, snapped (Windows) or zoomed (macOS) window is never touched. A window opened on a short screen fits it. **Linux**: the launch clamp and the remembered size work. The window doesn't grow. |

## 6a. How it was built (2026-10-02)

- **WF1** — `main.slint`: Ready and Live are each a block of a scrolling
  body over a pinned bar. Each block computes its elastic height and its two
  heights (the wide layout's right column, being in an `if`, reports its own
  height into the block) and publishes them to the root with
  `publish-fit()` from `init` and `changed`. `Btn` gained `confirm` (the
  in-place "Copied", its label's width held by a hidden measuring `Text`)
  and `icon-only`. `CopyIconBtn` adds the tip at a fixed 40 × 40, and
  `CopyCodeBtn` picks between the two from the root's `narrow-bar`, which a
  `changed width` handler sets (§4.1). `AlertStrip` is the slot's one line.
  The engine's loss notice gained `short_text()`, the first sentence, for
  the strip. The window code no longer sets the "Link copied" or "Room link
  copied" notes, because the buttons confirm themselves. Measured on the
  testing backend at 520 px wide, Live alone needs 742 px with the preview
  at 160 and 830 at 248, and a room of three needs 994 / 1,082. The design's
  sums were 747 / 835 and 999 / 1,087. The headless tests are
  `src/layout_tests.rs`.
- **WF2** — `src/fit.rs`: `fit()` is D8–D9 for one evaluation, and
  `FitState::step()` adds D10–D12 across ticks: manual resizes (a size
  change more than 500 ms after the shell's own, or one that changed the
  width, but never the jump back from maximized, snapped, zoomed or full
  screen), changes of state, and waiting. Back on the main page it waits a
  tick, so the rebuilt page's heights settle first. The shell calls it
  from the 250 ms tick and once on the first turn of the event loop
  (D13). It applies the move with `set_size` and `set_position` and logs
  it. It saves your size through `take_save()` a second after the last
  manual resize, and again at quit. Its tests live beside it, not in
  `shell.rs`.
- **WF3** — `app-windows/src/place.rs` and `app-macos/src/place.rs` measure.
  The conversions they need (`to_logical`, `from_appkit`,
  `workspace_to_screen`, `differs_from_restore`) are pure functions in
  `fit.rs`, tested on every host. The Linux shell's `placement()` returns
  the monitor's height less 64 px and `positioned: false`, which `fit()`
  reads as "never grow".

## 7. Verification

- WF1–WF2: the tests above, in CI's `cargo llvm-cov --workspace` on Linux and
  `cargo test --workspace` on `macos-latest`. WF1's tests construct the real
  `MainWindow` on the testing backend, as the shell tests already do.
- WF3: the conversion tests in the same runs, and the Windows glue in the
  `cargo xwin clippy` job.
- WF4: the owner's hardware pass, with the `debug.log` fit lines (D16) as the
  record of what the window did.
