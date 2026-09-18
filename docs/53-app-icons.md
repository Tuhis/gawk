# R44 — App icons for the native broadcasters (docs/53)

**Status**: designed 2026-09-18; **IC1–IC5 implemented 2026-09-18**. Chunks
**IC1–IC5** (`IC` = Icon; two-letter prefix per the R21+ convention).
Packaging and GUI shell only: nothing here touches the wire format, the
relay, either engine pipeline or the browser app. The on-desktop half of the
acceptance (a real taskbar, a real Explorer listing) is a manual pass at the
end of §4, exactly as R14 and R34 verify anything a runner cannot see.

## 1. Purpose

Neither desktop broadcaster has an application icon. The Gio window is
title-only (`gawk-broadcast/cmd/gawk-broadcast-gui/main.go`), the Slint
`MainWindow` declares no `icon`, and `gawk-broadcast.exe` carries no
resource, so Explorer, the taskbar, the "unknown publisher" dialog and the
Linux launcher all show a stock placeholder. The only icon name in either
tree is the freedesktop `video-display` the Linux notifier borrows.

The mark already exists: the web app's favicon is a lightning bolt in the
project's purple. This milestone turns it into a proper application icon
for both apps, generated from one source, and proves in CI that the icon
actually reaches the shipped binaries — the R2 question ("does it reach
production?") asked of an asset.

What already exists, so the reader does not go looking:

- **The mark.** `gawk-app/public/favicon.svg`: a 48×46 bolt path filled
  `#863bff`, under fifteen blurred, `color(display-p3 …)`-filled glow
  ellipses. The bolt path is what this milestone lifts; the glow is not
  carried over (D1).
- **The Gio app ID.** `gioui.org/app.ID` is the Wayland `app_id` and the
  X11 class hint. It defaults to `filepath.Base(os.Args[0])`, so today the
  window's identity depends on what the binary was renamed to.
- **The Linux tarball.** `ci.yml`'s `broadcast` job builds `dist/`, the
  `attach-broadcast-release` job packs it as
  `gawk-broadcast-linux-amd64.tar.gz` and asserts every expected member is
  present (docs/19 Decision 24).
- **The Windows build.** `cargo xwin build --release` on the self-hosted
  Linux runners with clang-cl + lld-link (docs/38 D18). The runner image is
  in another repository and its preflight asserts exactly the tools the
  build needs; `rc.exe` does not exist there and `llvm-rc` is not asserted.
- **The Slint window.** `crates/app/ui/main.slint` `MainWindow inherits
  Window`; `slint-build` for Rust output embeds every `@image-url` into the
  binary by default (`i-slint-compiler` `EmbedAllResources`), and the winit
  backend forwards `Window.icon` to `set_window_icon`.

## 2. Decisions

| # | Decision | Rationale |
|---|---|---|
| D1 | **The icon is the flat bolt on a rounded purple tile**, one shared SVG at `assets/icon/gawk.svg`: a 256×256 viewBox, a `#863bff` square with 56-unit corner radii, and the favicon's bolt path **verbatim** (translated and scaled, never re-traced) filled white. No glow layers, no `display-p3`, no dark-theme variant. The web favicon is untouched. | Roadmap owner decision 2026-09-10. Blurred ellipses and wide-gamut fills do not survive icon rasterisers, and a bare bolt on transparent is illegible at 16 px and on a dark taskbar; the tile is what keeps it readable on both light and dark chrome, which is also why one variant suffices. Lifting the path verbatim keeps the two marks provably the same shape. |
| D2 | **One source, generated derivatives committed, drift-checked.** `tools/icon` (its own Go module, stdlib + `golang.org/x/image/vector`) rasterises the SVG itself — a parser for exactly the subset the icon uses (`rect` with `rx`, `path` with `M L H V C A Z` in both cases, `translate`/`scale` transforms) — and writes `assets/icon/png/gawk-{16,24,32,48,64,128,256}.png`, `assets/icon/gawk.ico` and `assets/icon/gawk.res`. All three are committed. `go run ./tools/icon check` re-renders and compares **decoded pixels and structure**, not bytes, and fails on any difference; a CI job runs it whenever `assets/icon/` or `tools/icon/` moves. | Neither build may need an SVG rasteriser: the Linux job is a plain Go build, the Windows job must stay inside docs/38 D18's toolchain, and nothing on the self-hosted image rasterises SVG. Go is what both jobs already have. The check compares pixels rather than bytes so that a `compress/flate` change in a Go release does not read as icon drift while a real edit to the SVG or the renderer still does — the same shape as the wire golden vectors, applied to an image. A third-party SVG library (`oksvg`) was rejected: untagged, unmaintained, and far larger than the fifty lines of SVG this icon uses. |
| D3 | **The Linux app ID is `fi.ioio.gawk.broadcast`**, set by assigning `app.ID` in the GUI's `main` package `init` (not a `-ldflags -X`), and it is also the desktop entry's file name, its `Icon=`, its `StartupWMClass=` and the hicolor icon name. A test reads the checked-in desktop entry and asserts all four agree with the constant. | Gio exposes no window-icon API on X11 or Wayland; the desktop resolves the icon from the `app_id`/class hint through a desktop entry of the same name, so the ID is the whole mechanism and every mirror of it must match. Reverse-DNS is what GNOME Shell and KDE match exactly (`<app_id>.desktop`). Package `init` runs after Gio's own (dependency order) and before any window exists, so it is a plain build with no linker flag to forget. This changes what window managers key per-app settings on, which is why it lands as its own commit inside the PR. |
| D4 | **The tarball ships `share/` plus `install-desktop.sh`; there is no subcommand.** `share/applications/fi.ioio.gawk.broadcast.desktop` and `share/icons/hicolor/{16,24,32,48,64,128,256}x…/apps/*.png` + `scalable/apps/*.svg` are assembled by `gawk-broadcast/desktop/assemble-share.sh` from `assets/icon/` and `gawk-broadcast/desktop/`, in CI and from a checkout alike. The POSIX `sh` script copies them into `${XDG_DATA_HOME:-~/.local/share}`, rewrites `Exec=` to the absolute path of the `gawk-broadcast-gui` beside it, refreshes the desktop and icon caches if the tools exist, and takes `--uninstall`. | The key question in the roadmap. A subcommand would put an installer's job into a binary whose posture is "no installer" (docs/19 Decision 24, docs/38 OD6), and the one thing an install must know — where the binary ended up — is exactly what a script next to the binary knows and a documented hand copy gets wrong. The script is the documented copy, automated; INSTALL.md still shows the two `cp` lines for anyone who prefers them. |
| D5 | **The notifier uses the installed icon when it can find it.** `internal/desktop` resolves the icon name at startup: `fi.ioio.gawk.broadcast` if `icons/hicolor/*/apps/fi.ioio.gawk.broadcast.{svg,png}` exists under `$XDG_DATA_HOME` or any `$XDG_DATA_DIRS` entry, else the stock `video-display`. | A notification naming an icon the theme cannot resolve renders with no icon at all, which is worse than the stock one. The lookup is the freedesktop icon-theme search path, restricted to hicolor because that is the only theme the tarball installs into. |
| D6 | **Windows title bar and taskbar: `Window.icon` in Slint**, pointing at `assets/icon/png/gawk-256.png` by relative `@image-url`, embedded by the compiler's default for Rust output. | Toolkit-level, so it holds on any renderer and any host that builds the crate, cross-compiled or not. 256 px because winit rescales to 64 logical px × the scale factor and downsampling is the direction that stays sharp. |
| D7 | **Windows Explorer and the publisher dialog: a generated `.res` linked directly.** `tools/icon` writes `assets/icon/gawk.res` — `RT_ICON` 1–7 (32-bit DIBs with AND masks up to 128 px, PNG at 256 px, the same set the `.ico` holds) and one `RT_GROUP_ICON` — and `crates/app/build.rs` emits `cargo::rustc-link-arg-bins=<abs path>/gawk.res` when the target is `windows`/`msvc`. No `rc.exe`, no `llvm-rc`, no `winresource`/`embed-resource`. | The roadmap named the resource step as the one real risk because the build cross-compiles. Every resource-compiler route adds a tool the runner image does not assert and that lives in another repository; `lld-link` and MSVC `link.exe` both consume a `.res` as an input file directly, so the compiler is unnecessary once the `.res` is a checked-in, drift-checked derivative like the `.ico`. Restricted to msvc because GNU `ld` needs `windres` to convert a `.res` — the `x86_64-pc-windows-gnu` route the README mentions is a type-check, not a shipped binary. |
| D8 | **CI proves the resource landed.** `go run ../tools/icon verify-exe target/…/gawk-broadcast.exe` runs in `broadcast-windows.yml`'s `build` job right after the link: it walks the PE's `.rsrc` directory and fails unless `RT_GROUP_ICON` and `RT_ICON` are present. | The roadmap's condition: "the design doc owns proving that in CI before anything else". An icon resource that the linker silently dropped would be invisible until someone opened Explorer; the walk is forty lines of `debug/pe` and answers the R2 question at the only point it can be asked. |
| D9 | **Sizes**: 16, 24, 32, 48, 64, 128, 256 — PNG set, `.ico` and `.res` all carry the same seven. | The freedesktop hicolor set that GNOME and KDE actually ask for, plus the four Explorer view sizes (16/32/48/256) and the 24 px Windows title bar. |
| D10 | **The CLI, `gawk-pw-helper` and the browser app are untouched.** No tray icon (R14 Decision 15, docs/38 OD7), no installer or signing (docs/38 OD6/D17, R47 for signing), no macOS. | Roadmap non-goals, restated so nobody re-derives them. |

### Rejected

- **`winresource` / `embed-resource` + `llvm-rc`.** Adds a resource compiler
  to a toolchain that lives in another repository, for a step the linker
  performs itself on a `.res`. Re-open only if a *second* resource (a
  `VERSIONINFO` block, say) makes a hand-written `.res` writer the wrong
  tool — and even then `llvm-rc`, never `rc.exe` (docs/38 D18).
- **Rasterising in CI with ImageMagick / rsvg-convert.** Not on the
  self-hosted image, and a derivative that only CI can regenerate is a
  derivative nobody regenerates. The generator runs wherever Go runs.
- **Byte-exact PNG golden vectors.** `image/png` output is deterministic
  for one Go version but not promised across versions; pixel comparison is
  the invariant that actually matters.
- **A dark-theme tile variant.** The tile is what makes one variant enough;
  a second asset is a second thing to drift.
- **An `install-desktop` subcommand.** D4.
- **Setting `app.ID` via `-ldflags -X`.** A local `go build ./cmd/...`
  would ship a window with the wrong identity; the constant belongs in the
  code.

## 3. Where it plugs in

| Piece | Where it is today | What IC changes |
|---|---|---|
| Source mark | `gawk-app/public/favicon.svg` (bolt path + glow) | Read once, by hand: the bolt `d` attribute is copied verbatim into `assets/icon/gawk.svg`. The favicon is not modified. |
| Generator | — | **New** `tools/icon` module: `svg.go` (subset parser + arc→cubic), `render.go` (x/image/vector), `ico.go`, `res.go`, `pe.go`, `main.go` (`generate` / `check` / `verify-exe`). |
| Derivatives | — | **New** `assets/icon/png/gawk-N.png`, `assets/icon/gawk.ico`, `assets/icon/gawk.res`, marked `linguist-generated` + `-diff` in `.gitattributes`. |
| Linux ID | `cmd/gawk-broadcast-gui/main.go` (no ID; Gio defaults to the binary name) | `func init() { app.ID = desktop.AppID }` with the constant in **new** `internal/desktop`. |
| Linux desktop entry | — | **New** `gawk-broadcast/desktop/fi.ioio.gawk.broadcast.desktop` and `gawk-broadcast/desktop/install-desktop.sh`. |
| Linux notifier | `internal/notify/notify.go` `icon = "video-display"` constant | `New(iconName string)`; the GUI passes `desktop.IconName()`. |
| Linux CI | `ci.yml` `broadcast` job builds `dist/`; `attach-broadcast-release` packs a fixed member list | `desktop/assemble-share.sh` lays `dist/share/` out from `assets/icon` + `desktop/` (one script owns the layout, so a checkout can produce the same tree); the tarball gains `share/` and `install-desktop.sh` and the member check names them; a step runs the script against a scratch `XDG_DATA_HOME`, validates the entry with `desktop-file-validate`, checks the absolute `Exec=` and that `--uninstall` leaves nothing behind. The `broadcast` path filter gains `assets/icon/`; the desktop files themselves live in the module. |
| Windows window | `crates/app/ui/main.slint:77` `MainWindow` | `icon: @image-url("../../../../assets/icon/png/gawk-256.png");` |
| Windows resource | `crates/app/build.rs` (slint compile + build rev) | `emit_icon_resource()`: link arg on msvc targets, `rerun-if-changed` on the `.res`. |
| Windows CI | `broadcast-windows.yml` `build` job | `verify-exe` step after the build; path filter gains `assets/icon/**` and `tools/icon/**`. |
| Root CI | `ci.yml` `changes` decisions; `tidy` loop | New `icon` decision + job (`go test`, `check`); `tools/icon` joins the tidy loop. |
| Docs | both `INSTALL.md`, both `README.md`, `docs/gotchas.md`, `docs/README.md`, `ROADMAP.md` | One section each; the gotchas that surfaced (§6). |

## 4. Chunks and acceptance criteria

| Chunk | Scope | Verified by |
|---|---|---|
| **IC1** | `assets/icon/gawk.svg` (D1), `tools/icon` generator + `check` + `verify-exe` (D2, D7, D8, D9), the committed derivatives, `.gitattributes` | `go test ./...` in `tools/icon`: the SVG subset parser reproduces known coordinates for each command class incl. relative forms and the arc; the renderer fills a rect at every corner pixel and leaves the corner radius transparent; the `.ico` writer's output parses back (independent reader in the test) to seven entries with the expected sizes, DIB entries below 256 and a PNG entry at 256; the `.res` writer's output parses back to seven `RT_ICON` + one `RT_GROUP_ICON` whose directory names every image with matching sizes; a synthetic `.rsrc` directory with and without type 14 drives `verify-exe`'s walker; `check` passes on the committed files and fails when a PNG is replaced by a re-encode of a shifted render. `go run . check` from a clean checkout exits 0. |
| **IC2** | Linux: `internal/desktop` (D3, D5), `app.ID` in the GUI `init`, notifier icon parameter, desktop entry + `install-desktop.sh` (D4) | `desktop_test.go`: `IconName()` returns the ID when a hicolor entry exists under a scratch `XDG_DATA_HOME` or a `XDG_DATA_DIRS` entry, `video-display` otherwise; the desktop entry's file name, `Icon=`, `StartupWMClass=` equal `AppID` and `Exec=` names `gawk-broadcast-gui`. `main_test.go`: `app.ID == desktop.AppID` after package init. `install-desktop.sh` under `sh -e` with a scratch `XDG_DATA_HOME`: the entry lands with an absolute `Exec=`, all eight icon files land, `--uninstall` removes exactly them; `desktop-file-validate` passes on the installed entry. `go vet`, `go test -race ./...` green. |
| **IC3** | Windows: `Window.icon` (D6), `build.rs` link arg (D7) | `cargo clippy -D warnings` on both the msvc target and the host; `cargo test --workspace` green (the `.res` arg is emitted only for msvc, so host builds and tests are unaffected). The `build` job's `verify-exe` step passes on the artifact (D8) — this is the proof the roadmap asked for, and the PR is not mergeable without it. |
| **IC4** | CI: `ci.yml` `icon` job + tidy loop + broadcast dist/tarball/install test; `broadcast-windows.yml` filter + verify step | The `icon` job runs on a PR touching only `assets/icon/`; the tarball member check names `install-desktop.sh` and `share/applications/…desktop`; the install step passes; `broadcast-windows` runs on a PR touching only `tools/icon/`. |
| **IC5** | Docs: both INSTALL.md and READMEs, `docs/gotchas.md`, `docs/README.md`, ROADMAP row + entry, this document's status | Review. INSTALL.md (Linux) shows the script and the two-line hand copy; INSTALL.md (Windows) says nothing new (the icon needs no step) beyond noting it in the SmartScreen paragraph. |

Success criterion, end to end — **the manual pass, owner-run**:

1. Linux, GNOME or KDE on Wayland: extract a release tarball, run
   `./install-desktop.sh`, launch `gawk-broadcast-gui` from the launcher.
   The launcher entry, the taskbar/dock entry and the window's title bar
   (where the shell draws one) show the purple bolt; a notification from the
   app shows the same icon. `./install-desktop.sh --uninstall` returns the
   desktop to a nameless window.
2. Windows 10/11: download `gawk-broadcast-windows-x86_64.exe`. Explorer
   shows the bolt at every view size, the SmartScreen/"unknown publisher"
   dialog shows it, and after launch the title bar and taskbar show it.

## 5. Security considerations

Nothing new reaches the network, the wire or the relay. The desktop entry
carries an absolute `Exec=` path written by a script the user runs under
their own account into their own `XDG_DATA_HOME`; it never writes outside
it and never elevates. The `.res` is data the linker copies into a section;
it executes nothing. `tools/icon` reads one SVG from the repository and
takes no network input.

## 6. Gotchas surfaced (mirrored in `docs/gotchas.md`)

- **Gio has no window-icon API on Linux; `app.ID` is the whole mechanism**,
  and it must equal the desktop entry's file name, `Icon=` and
  `StartupWMClass=` — a mismatch in any one of them shows the stock icon
  with no error anywhere. The default ID is the binary's basename, so a
  renamed binary used to change the window's identity.
- **`lld-link` and `link.exe` take a `.res` directly**; no resource compiler
  is needed when the `.res` is generated by other means. GNU `ld` does not.
- **Slint embeds `@image-url` files for Rust output by default**, so an icon
  outside the crate directory is fine — but `SLINT_EMBED_RESOURCES=false`
  in the environment would turn it into an absolute build-machine path in
  the shipped binary. Nothing in CI sets it; do not.
- **A notification that names an icon the theme cannot resolve shows no
  icon at all**, not the stock one — hence D5's fallback.
