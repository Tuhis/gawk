# gawk-broadcast-desktop

[![Coverage](https://img.shields.io/endpoint?url=https%3A%2F%2Fraw.githubusercontent.com%2FTuhis%2Fgawk%2Fbadges%2Fgawk-broadcast-desktop.json)](../docs/43-coverage-reporting.md)

The native **Windows** broadcaster: share one application — its window
plus **that app's own audio**, via WASAPI process loopback — or the whole
desktop, hardware-encoded through Media Foundation, straight to the gawk
relay over WebTransport. The design doc is
[docs/38](../docs/38-windows-native-broadcaster.md); read it before
changing anything here — every structural choice below is a numbered
decision there.

**Status:** implemented and CI-gated; the on-hardware acceptance pass on a
real gaming PC is what remains (docs/38 §10). All portable logic is
unit-tested cross-platform; the Windows halves are type-checked and linted
for `x86_64-pc-windows-msvc` in CI.

This is a Rust Cargo workspace, deliberately its own top-level component
with its own CI job and release line. No container, no Helm chart, no
deploy: a single binary you run on your own gaming PC.

The directory was `gawk-broadcast-windows/` until R52 renamed it ahead of a
native macOS broadcaster joining the workspace
([docs/54](../docs/54-macos-native-broadcaster.md)). Only the directory,
the workflow and the release tag changed; the EXE, its release asset and
everything it reports about itself are still `gawk-broadcast-windows`.

## Getting a build

Grab `gawk-broadcast-windows-x86_64.exe` from the
**[Releases page](https://github.com/Tuhis/gawk/releases)**, or with the
GitHub CLI:

```
gh release download --pattern 'gawk-broadcast-windows-x86_64.exe'          # newest
gh release download gawk-broadcast-windows-v1.1.0 --pattern '*.exe'        # specific
```

(Releases after v1.5.0 are tagged `gawk-broadcast-desktop/vX.Y.Z`. Before
that they were `gawk-broadcast-windows/vX.Y.Z`, and up to v1.1.0
`gawk-broadcast-windows-vX.Y.Z` — the separator changed repo-wide in August
2026.)

`BUILD-INFO.txt` and `SHA256SUMS` are attached alongside it.
The EXE is unsigned by design (distribution is to known operators, docs/38
D17), so the checksum is the integrity check — and **SmartScreen will warn
about an unknown publisher on first run**: "More info" → "Run anyway". If
that button is missing, right-click the exe → Properties → tick **Unblock**
→ OK, then run it again.

For an *unreleased* build, every green CI run uploads an artifact:

```
gh run download --name gawk-broadcast-windows-x86_64-<commit sha>
```

**Which build am I running?** The window header shows `v1.0.0+g1a2b3c4` —
the release plus the commit it was built from. That matches the artifact
name and `BUILD-INFO.txt`, so a screenshot is enough to identify a build.
The same string is the first line of `debug.log` and the `appVersion` key
in **Copy diagnostics**. There is no `--version` flag: a windowed EXE has
no console to print to.

## Requirements

| Requirement | Check |
|---|---|
| Windows 10 version 2004 (build 19041) or newer, x86-64 | `winver` — per-app audio capture needs 2004+ |
| A hardware H.264 encoder (any NVIDIA/AMD/Intel GPU from the last decade) | just run it; it tells you. There is no software encoder — use the browser broadcaster instead |

Nothing else: the EXE is fully static — one file, no runtimes, no
installer. On Windows 10 builds below 20348 captured windows and screens
show the system's yellow capture border (the API to remove it does not
exist there); Windows 11 removes it. Closing the window ends the broadcast
— there is no tray icon and no background presence.

## When it doesn't work

- **"No hardware H.264 encoder was found"** — the app refuses rather than
  software-encode. Check GPU drivers are installed (a fresh VM has none);
  otherwise use the browser broadcaster.
- **Sharing an app, viewers hear nothing** — some games play audio through
  a helper process the per-app capture can't see. The app shows a hint
  after ~10 s of silence with a one-click switch to whole-system audio.
- **Shared window went black/frozen for viewers** — minimized windows
  aren't composited, so nothing can capture them. Restore the window
  (occluded/covered is fine).
- **"Could not reach the relay"** — the relay speaks UDP on port 4433
  (QUIC). Networks that block UDP block this.
- **Toasts don't appear during a fullscreen game** — Focus Assist eats
  them; the in-window status is the truth.
- Anything else: expand **Details**, click **Copy diagnostics**, and send
  the JSON along with `debug.log`, below.

### `debug.log`

The app is a windowed EXE — nothing useful ever appears on stderr.
Instead every launch writes **`%APPDATA%\gawk\debug.log`** (next to
`broadcast.json`; the previous launch is kept as `debug.log.old`). It
records the adapter the D3D11 device landed on, the hardware encoder MFTs
enumerated, each candidate's trial verdict with the failing Media
Foundation step, and session lifecycle — enough to diagnose a "No hardware
H.264 encoder was found" refusal from the file alone. Error cards point at
it; attach it to any bug report.

## Defaults

Blank settings mean "the default", resolved at use, never at save
(docs/38 D13):

| Setting | Default |
|---|---|
| Relay | `https://api.gawk.ioio.fi:4433` |
| App URL (join links) | `https://gawk.ioio.fi` |
| Telemetry ingest | `https://gawk.ioio.fi/api/telemetry/v1/ingest` (`off` = send nothing) |
| Origin | `gawk-broadcast://windows` — the relay's `-allowed-origins` must include it |
| Rung | 1080p60, 500 ms GOP, 12 Mbps peak VBR — the resolution is a bounding box: the stream keeps the source's aspect ratio inside it |

## Rooms

The **Room** card (R42, [docs/44](../docs/44-rooms.md) §4.8) attaches the
running broadcast to a room, on a relay started with `-rooms`:

| Field | Config key | Meaning |
|---|---|---|
| Room code or link | `room` | A dynamic code, a static slug, or a pasted `…/#/room/<CODE>` link; blank = no room. Joined on every start, re-attached on every resume. |
| Attach key | `roomAttachSecret` | A static room's attach secret (DPAPI-wrapped like the publish secret) |
| Nickname | `nickname` | Your name in the room, and what your tile is called (blank = the relay picks one). Editable while live: the roster and the tile follow. |

While live, **Attach** joins the room in the field, **New room** mints a
dynamic room from this broadcast (the app is its creator), and
**Detach** / **Leave room** removes the attachment and closes the room
session — the broadcast itself keeps going. **Open room view** launches
the browser at `<app URL>/#/room/<CODE>?rt=<grant>`, where the grant is
the creator token of a room you minted or the static room's attach key;
the SPA moves it out of the URL before rendering. A room ending (close
code 4007) or a refused command shows in the card's status line and never
touches the broadcast.

## Building

Any host builds and tests the portable crates (wire, engine); the
Windows-only crates compile empty off-Windows so `cargo test` works
anywhere. The pinned toolchain in `rust-toolchain.toml` auto-installs via
rustup.

```
cargo test                                # golden vectors, chunking, defaults
cargo clippy --all-targets -- -D warnings
cargo build --release                     # target/release/gawk-broadcast(.exe)
```

Quirks that will bite you:

- **libopus needs `CMAKE_POLICY_VERSION_MINIMUM=3.5`** in the environment:
  the bundled source's CMakeLists predates CMake 4's floor. CI sets it;
  set it locally or `cargo test` dies in `audiopus_sys`'s build script.
- **The WARP conversion tests self-skip on hosts whose WARP lacks D3D11
  video** (hosted runners do). Run `cargo test -p gawk-capture` on a real
  Windows machine to actually execute them.
- Cross-type-checking from a non-Windows host works two ways. The quick
  local one is the `x86_64-pc-windows-gnu` target + mingw-w64. The one CI
  uses is **`cargo xwin` against `x86_64-pc-windows-msvc`**, which links
  the real shipped ABI; it needs `llvm` (for `llvm-lib`, or `ring` fails),
  a `/usr/bin/clang-cl` shim carrying `-mssse3 -msse4.1` (or libopus
  fails), and the MSVC SDK. All three are baked into the CI runner image —
  see docs/38 D18 before reproducing it by hand.
- **The EXE's icon is a linked `.res`, not a compiled `.rc`.**
  `crates/app-windows/build.rs` hands `assets/icon/gawk.res` (generated from the
  shared SVG by `go run ./tools/icon generate`, committed) straight to the
  linker on msvc targets; lld-link and link.exe both take it as an input, so
  there is no `rc.exe`/`llvm-rc` step to install. The window's own icon is
  `Window.icon` in `crates/ui/main.slint`, embedded by slint-build. CI checks the
  resource actually landed (`tools/icon verify-exe`) — docs/53.

## Layout

| Crate | Role |
|---|---|
| `crates/wire` | The wire-format mirror — see below |
| `crates/engine` | Session lifecycle, send policy, resume supervisor. No GUI, no COM/WinRT; media enters through traits, the network through a `RelaySession` seam |
| `crates/capture` | Windows.Graphics.Capture frame source + window/monitor picker enumeration; on macOS the ScreenCaptureKit stream and the system content picker (`sck`, `sck_picker`), with their frame policy in the portable `sck_policy` |
| `crates/encode` | Media Foundation hardware H.264 MFT cascade, trial-gated |
| `crates/audio` | WASAPI process/system loopback + Opus |
| `crates/ui` | The window both shells show (`main.slint`, compiled once), the build version, and window logic the shells share |
| `crates/app-windows` | The Windows shell — `gawk-broadcast.exe` |
| `crates/app-macos` | The macOS shell — `gawk-broadcast-macos`, bundled as `gawk-broadcast-macos.app` by `tools/macos/bundle.sh` (R52, [docs/54](../docs/54-macos-native-broadcaster.md); in progress — a stub off macOS) |

### The wire crate is a mirror, not an implementation

`gawk-server/wire/wire.go` is the source of truth; `gawk-app`'s `wire.ts`
and `gawk-broadcast/internal/wirecheck` are the other mirrors. The golden
vectors in `crates/wire/tests/golden.rs` are deliberately **restated as
literal hex**, never imported or generated — a shared fixture could be
edited once and stay green everywhere, defeating the purpose. Every wire
change lands in `wire.go` first and must be hand-mirrored here,
byte-identically. The GF(256) parity symbols are additionally pinned
against bytes generated by running the Go `ComputeParity` — the two
implementations must compute identical P/Q.

## Licensing: the one dependency that is a choice

`gawk-broadcast.exe` is Apache-2.0 like the rest of the repository, and
every crate it links is permissive — with one deliberate exception. The
GUI uses **Slint**, which is tri-licensed, and gawk takes the
**Royalty-free Desktop License v2.0** (docs/38 D12). That is what lets
this component stay Apache-2.0 instead of becoming the repository's one
GPL island. The license's one condition is attribution: the "Made with
Slint" badge in the [root README](../README.md#license) and on the
releases page.

Two other things a license scan will not tell you: **libopus** is
statically linked through `audiopus_sys` (ISC bindings over Xiph's
BSD-3-Clause C library), and the **MSVC C runtime** it links is
Microsoft's, governed by the Visual Studio license terms. Full inventory:
[THIRD-PARTY-NOTICES.md](THIRD-PARTY-NOTICES.md).

`deny.toml` in this directory is what keeps the above true: CI runs
`cargo-deny check licenses` against it, and a copyleft crate arriving
through a dependency bump fails the build.
