# R63 — Source preview in the desktop broadcaster (docs/65)

**Status**: designed 2026-10-01; the owner's decisions OD1–OD4 (§2) taken
the same day. Chunks **PV1–PV6** (§6): PV1–PV5 implemented 2026-10-02;
PV6, the owner's hardware pass, open. D2 revised 2026-10-02 by R64
([docs/66](66-desktop-scrolling-and-window-fit.md)). Status lives in
[`ROADMAP.md`](../ROADMAP.md).

**Relationship to earlier work**: widens the 1 Hz confidence thumbnail of
[docs/38](38-windows-native-broadcaster.md) D12.5,
[docs/54](54-macos-native-broadcaster.md) D11 and
[docs/58](58-linux-desktop-broadcaster.md) OD13, and revises
[docs/60](60-desktop-redesign.md) D4 ("not a live preview, because nothing
captures while idle"). It is still not a preview *player*: R14 Decision 16
stays settled for anything that decodes or runs at frame rate.

---

## 1. Why

The owner raised two gaps:

1. **Windows shows no preview when sharing a whole display.** The thumbnail
   was scoped to mode 1 (one app), because R14 Decision 16 argued that a
   broadcaster sharing their screen is already looking at it. Since docs/64
   the preview is the centre of the Live page, and on a display share
   it is a large box with "What viewers see" in it and nothing else. That
   reads as broken. macOS already shows the thumbnail for a display, so the
   platforms disagree too.
2. **Ready shows an icon, not the source.** After choosing what to share,
   the source card shows the window's icon and title, or a monitor glyph.
   The broadcaster cannot check that they picked the right window or the
   right display until they are live in front of viewers.

## 2. The owner's decisions

| # | Question | Decision |
|---|---|---|
| OD1 | macOS: a live preview before going live keeps an `SCStream` running, and the system shows its screen-sharing indicator in the menu bar while it does (docs/54 removed exactly this once, for an app that had nothing picked). | **Live preview; accept the indicator** while a pick is held on Ready. |
| OD2 | Exclude gawk's own window from captures (`WDA_EXCLUDEFROMCAPTURE`) so a display share is not a hall of mirrors? | **No; the window stays visible** in captures, as today. |
| OD3 | Rate. | **1 Hz, as today.** A smoother preview is a separate decision (its costs are in §7). |
| OD4 | Scope. | The Live thumbnail on Windows in both modes, and a Ready preview on all three platforms once a source is chosen. |

## 3. Decisions

| # | Decision | Rationale |
|---|---|---|
| D1 | **Windows shows the Live thumbnail in both capture modes.** `show_thumbnail` is true for a window and for a display; the thumbnail is taken from the same frame, in the same place on the frame-pool thread, after the same fps and backpressure gates. *Revises docs/38 D12.5* (mode 1 only). | The Live page is built around the preview (docs/64 D7, D9). One rule on Windows and macOS. |
| D2 | *(Revised 2026-10-02, [docs/66](66-desktop-scrolling-and-window-fit.md) D5: the card with a preview gives up height, down to 160 px, before the Ready page scrolls.)* **Ready previews the chosen source.** Once something is chosen, the source card shows a 1 Hz, about 320-pixel-wide picture of it above the source's title, in place of the icon tile. With nothing chosen, or while no frame has arrived, the card is what it is today. *Revises docs/60 D4.* | Checking the pick before going live is the point of the Ready page. |
| D3 | **The preview is a capture-only stream, owned by the platform.** No encoder, no audio, no network: the same capture API the broadcast uses, opened on the chosen source, sampled once a second into RGBA. Windows: a WGC session on its own D3D11 device, downscaled by the VideoProcessor (`Converter::thumbnail_rgba`). macOS: an `SCStream` on the picker's filter at 320×180 and 1 fps, converted by `nv12_thumbnail`. Linux: `pipewiresrc` on the held portal grant in system memory, `videorate` to 1 fps, `videoconvertscale` to RGBA, into an appsink (`gst_policy::preview_plan`). With no encoder on the path, docs/58 OD13's zero-copy concern does not apply. | The preview reuses the capture each platform already trusts, so what it shows is what the broadcast would capture. |
| D4 | **It runs only while it can be seen.** The shell asks for a preview while the window is idle (not starting, live or paused), on the main page (not the picker, Settings or Quality), and not minimized. The platform runs one exactly while that holds and a source is chosen, and stops it otherwise. The request is level-triggered, every 250 ms tick, so no transition can leave one running. | An idle app costs nothing again the moment the preview is out of sight. |
| D5 | **The preview is gone before a broadcast captures.** The shell stops it before every `prepare_start` (Go live, Resume, a quick restart), so the broadcast's capture never shares the source with it. On Linux it is also stopped before a grant is released (a re-pick, an ending), because the pipeline is reading the grant's PipeWire remote. | Two consumers of one portal node, or a pipeline outliving its grant, are failure modes nobody needs to debug. |
| D6 | **A preview never stands in the way of a broadcast.** A preview that cannot start is a debug-log line and the icon card. It is not retried until the pick changes or the page is shown again, so a broken one does not retry every tick. A preview that stops delivering keeps its last picture. Nothing in the start path waits for it. | The preview is a convenience; the broadcast is the product. |
| D7 | **One contract in the shell.** `Platform::preview(ui, wanted) -> PreviewFrame` (`Hidden` / `Keep` / `New(thumb)`). The platforms share a `PreviewSlot<K>` keyed by what is chosen: the `CaptureTarget` on Windows, a pick counter on macOS and Linux. It starts, restarts on a new key, stops, remembers a failed key and turns frames into a `PreviewFrame`. It lives in `gawk-ui`, so all of it is a host test. The window gets two properties, `preview` and `has-preview`, separate from the Live thumbnail. | The lifecycle rules (D4–D6) are written once and tested once. The platforms supply only "start a capture of this". |

### Side effects the owner accepted

- **macOS** (OD1): the menu-bar screen-sharing indicator shows while a pick
  is held on Ready, not only while live. The picker is kept active while a
  preview runs. An app with nothing picked still shows no indicator.
- **Windows**: on a build without `IsBorderRequired` (Windows 10 before
  20348), the yellow capture border shows around the chosen source on Ready,
  as it already does while live (docs/38 D6).
- **Linux**: the portal session is already held from the pick (docs/58), so
  the compositor's sharing indicator is already on. Nothing changes.
- **Display shares** (OD2): when gawk's window is on the shared display, the
  preview contains the preview. Viewers see the same while live.

## 4. What does not change

The wire, the relay, the engine, encode and audio. The Live thumbnail on
Linux keeps docs/58 F-6's rule: system-memory rung only, until V-3. No
config key. The thumbnail rate (1 Hz) and size (320 wide) are the existing
constants.

## 5. Out of scope

- A smooth preview (OD3). §7 records the cost analysis so it is not
  re-derived.
- Excluding gawk's window from capture (OD2).
- Thumbnails in the Windows picker list (docs/60's rejected table: capturing
  every window to draw one is not free).

## 6. Chunks and acceptance criteria

| Chunk | What | Acceptance |
|---|---|---|
| **PV1** | Windows: the Live thumbnail in both modes (D1); docs/38 D12.5 amended with a dated note. | The mode gate is gone from `app-windows/src/pipeline.rs`. `cargo xwin clippy` for the Windows target (CI) is green. |
| **PV2** | Shell: `PreviewSlot`, `PreviewFrame`, `PreviewSource` and `Platform::preview` (D7); the tick asks for a preview per D4 and stops it before each `prepare_start` (D5); the Ready source card shows `preview` when `has-preview` (D2). | Unit tests: a slot starts on the first wanted tick, restarts on a new key, stops when not wanted, does not retry a failed key until the key changes or it is unwanted in between, and reports `Hidden` until the first frame, then `New`, then `Keep`. Shell tests: an idle tick on the main page shows the platform's frame; a tick on another page, or a start, asks the platform for no preview before `prepare_start` runs, and `has-preview` is false. Host `cargo test -p gawk-ui` and clippy are green. |
| **PV3** | Windows platform: a capture-only WGC preview (`app-windows/src/preview.rs`), keyed by the chosen `CaptureTarget`. | Windows-target clippy (CI) is green. The capture, converter and thumbnail calls are the ones `capture/tests/warp.rs` already covers. |
| **PV4** | macOS platform: the `SCStream` preview at 320×180 and 1 fps, keyed by a pick counter; the picker is active while one runs (OD1). | The macOS CI job (build, clippy, tests) is green. |
| **PV5** | Linux: `gst_policy::preview_plan` and a `gst::Preview` runner; the platform keyed by a pick counter, stopping the preview before a grant is taken or released (D5). | `preview_plan` unit test (elements, the grant's fd and node, system memory, the 1 fps gate, the RGBA tail). A container integration test: the runner, on a `videotestsrc` plan with the same tail, delivers an RGBA thumbnail of the planned width. Linux container build, clippy and tests are green. |
| **PV6** | The owner's hardware pass on Windows, macOS and Linux. | Owner-run. Windows: Live shows the thumbnail sharing a display; Ready shows the chosen window and the chosen display, and switches within about a second when the choice changes. macOS: Ready shows the pick, with the menu-bar indicator; no indicator with nothing picked. Linux: Ready shows the portal pick; Go live works straight after it. On every platform: Go live from a previewed Ready starts normally, and Task Manager / Activity Monitor / `top` show the idle app with a pick under 2 % of one core. |

## 7. Costs, and why the preview stays at 1 Hz

Recorded from the 2026-10-01 discussion, so a smoother preview is not
re-derived from scratch. These are estimates, not measurements.

- **Bytes and pixel work are cheap.** A 640×360 RGBA frame is about 0.9 MB;
  at 30 fps that is about 28 MB/s of readback and the same again uploaded
  to the window, which no PCIe bus or unified memory notices.
- **Windows' readback is synchronous.** `thumbnail_rgba` allocates its
  target per call and maps the staging texture at once, which waits for the
  GPU on the frame-pool thread that feeds the encoder. Once a second that is
  invisible; per frame it is a stall. A smooth preview needs cached targets
  and a staging ring read two or three frames late.
- **Linux zero-copy rungs** would need a GPU scale before the download, and
  V-3's measurements first (docs/58 OD13).
- **The window repaints.** The idle window costs about nothing (docs/38's
  "idle window ~0 % CPU"). At the preview's rate it redraws and uploads a
  texture per frame, from the game's GPU budget on the broadcasting PC.
- **The hall of mirrors costs bitrate** on a display share at frame rate: a
  still desktop becomes constant motion for the encoder.
- **Sharing the encoder's texture with the window directly** (no CPU round
  trip) needs a D3D/GL, IOSurface/GL and DMABuf/EGL interop under Slint's
  renderer, a YUV→RGB shader and cross-API synchronisation with the capture
  ring, and still a CPU fallback for hybrid-GPU laptops. Not worth it for a
  confidence picture.
