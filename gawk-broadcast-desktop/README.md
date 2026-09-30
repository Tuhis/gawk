# gawk-broadcast-desktop

[![Coverage](https://img.shields.io/endpoint?url=https%3A%2F%2Fraw.githubusercontent.com%2FTuhis%2Fgawk%2Fbadges%2Fgawk-broadcast-desktop.json)](../docs/43-coverage-reporting.md)

The native desktop broadcasters for **Windows**, **macOS** and **Linux**:
share one application — its window plus **that app's own audio** — or the
whole desktop, hardware-encoded, straight to the gawk relay over
WebTransport. On Windows that is WASAPI process loopback and Media
Foundation ([docs/38](../docs/38-windows-native-broadcaster.md)); on a Mac,
ScreenCaptureKit through the system picker and VideoToolbox's low-latency
encoder ([docs/54](../docs/54-macos-native-broadcaster.md), and
[macOS](#macos) below); on Linux, the desktop's screen-share portal,
GStreamer's hardware encoders in-process and a PipeWire tee for one app's
audio ([docs/58](../docs/58-linux-desktop-broadcaster.md), and
[Linux](#linux) below). Read the design doc before changing anything here —
every structural choice is a numbered decision there.

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

**Which build am I running?** The foot of the first page (and of Settings)
shows `v1.0.0+g1a2b3c4` — the release plus the commit it was built from. That matches the artifact
name and `BUILD-INFO.txt`, so a screenshot is enough to identify a build.
The same string is the first line of `debug.log` and the `appVersion` key
in **Copy diagnostics**. There is no `--version` flag: a windowed EXE has
no console to print to.

**Is there a newer one?** At launch, at most once a day, the app fetches its
platform's release manifest (`releases/<platform>/latest.json` on this
repository's `badges` branch, from `raw.githubusercontent.com`). When a newer
release exists, a line under the Go live button reads "v2.1.0 available —
release notes" and opens the release page; **Dismiss** hides it until the
next release. The request is a plain GET with a fixed User-Agent: no version,
no identifier, nothing about you or your broadcasts. Turn it off with
**Settings → Advanced → Check for updates at launch**, or by setting
`GAWK_NO_UPDATE_CHECK=1`. Downloading and installing is still up to you
([docs/47](../docs/47-desktop-update-check.md)).

## macOS

Grab `gawk-broadcast-macos-arm64.zip` from the same
**[Releases page](https://github.com/Tuhis/gawk/releases)**, unzip it, and
open `gawk-broadcast-macos` (drag it to Applications first if you like).
It is signed with a Developer ID and notarized, so a fresh account opens
it without a Gatekeeper dialog. Then: **Choose what to share…** opens
macOS's own picker — one window, one app, or a display — and **Go live**.
The picker path needs no Screen Recording permission.

| Requirement | Why |
|---|---|
| macOS 14 (Sonoma) or newer | the system content picker |
| Apple silicon | Intel Macs: the browser broadcaster hardware-encodes fine there |

Sharing a window or an app sends **only that app's audio**; sharing a
display sends the whole system's (except gawk-broadcast's own). If an app
stays silent — some games play audio through a helper process — the window
offers **Share all sound instead**, which re-opens the picker on a display.

Settings live in `~/Library/Application Support/gawk/broadcast.json`
(mode 0600: credentials sit in it as plain text, as on Linux), next to
`debug.log`. **Settings… ⌘,** opens the same page as the header's
Settings button.

**A CI build is different.** Every green CI run uploads
`gawk-broadcast-macos-<commit sha>` (`gh run download --name …`), but a
pull-request build is **ad-hoc signed**, not notarized — its
`BUILD-INFO.txt` says so. Open it once via System Settings → Privacy &
Security → **Open Anyway**, and expect any permission it collects to die
with that build: macOS keys grants to the signature, and an ad-hoc
signature is the binary's own hash.

When it doesn't work, on a Mac:

- **The picker never appears** — the app must be the frontmost window; and
  `debug.log` says why if the system refused to start it.
- **"No hardware H.264 encoder was found"** — a VM, or an Intel Mac. The
  browser broadcaster is the answer there; this app never software-encodes.
- **Relay refuses the connection** (`origin rejected` in the relay log) —
  the relay's `allowedOrigins` needs `gawk-broadcast://macos`
  ([self-hosting](../docs/self-hosting.md)).
- **The shared window went frozen** — it is minimized; restore it.

### Broadcasting over Wi-Fi on a Mac

If the Live page says **"Your Wi-Fi is dropping some video"**, packets
are leaving the Mac and not arriving at the server. Each lost packet costs
viewers a brief pause until the next keyframe, up to half a second. On a
Mac the usual cause is AWDL, the link behind AirDrop, AirPlay, Sidecar and
Universal Control: it takes the Wi-Fi radio off your network's channel
several times a second to listen for nearby Apple devices. In the first
Mac test this was most of the loss. With AWDL down, viewers' broken
frames fell from 20–170 a minute to 0–4.

In order of preference:

1. **Use a cable** if you can. Nothing else fixes every cause.
2. **Take AWDL down while you stream.** In Terminal:

   ```sh
   sudo ifconfig awdl0 down
   ```

   AirDrop, AirPlay to other devices, Sidecar and Universal Control stop
   working until you run `sudo ifconfig awdl0 up` or restart the Mac.
   macOS can bring the link back up by itself when something asks for it
   (opening AirDrop in Finder, say); the **Network** row under **Details**
   shows whether it is up. Turning AirDrop off in Control Center is
   gentler but leaves the link to the other features.
3. **If you manage your router**, put its 5 GHz network on channel 149
   (or 44). AWDL listens on those channels, so the radio stops leaving
   your network to hop to them, and nothing on the Mac needs turning off.

The line appears only when packets are actually being lost while someone
is watching, and it goes away by itself once the loss stops. It can't be
dismissed: while viewers are losing video, you should know. **Details**
shows the packet counts behind it.

## Linux

On Ubuntu 24.04+ or Debian 13+, grab `gawk-broadcast_<version>_amd64.deb`
from the same **[Releases page](https://github.com/Tuhis/gawk/releases)**
and run `sudo apt install ./gawk-broadcast_<version>_amd64.deb`. It brings the
GStreamer and portal packages, the launcher entry and the icon. On Wayland
the icon only appears once that entry is installed (docs/63). If you used the
tarball's `install-desktop.sh` before, run it with `--uninstall` first.

Elsewhere, grab `gawk-broadcast-linux-x86_64.tar.gz` (from
`gawk-broadcast-desktop/v2.0.0` on), unpack it and run
`./gawk-broadcast-linux`; `./install-desktop.sh` adds a launcher entry and
icon. The tarball's `INSTALL.md` has the package lines for apt, dnf and
pacman.

| Requirement | Why |
|---|---|
| Ubuntu 24.04 or newer, or equivalent (glibc 2.39, GStreamer 1.24, PipeWire 1.0) | the build's floor (docs/58 OD14) |
| xdg-desktop-portal and your desktop's backend | the only way to capture on Wayland; tested on KDE Plasma |
| A hardware H.264 encoder that `vulkanh264enc`, `nvh264enc` or `vah264enc` can drive | no software fallback: the browser covers that |

**Choose what to share…** opens the desktop's own picker. For a window, the
Sound row's **Choose** names whose audio goes out: the apps playing sound
now, **Whole system** or **No audio**. The portal never says whose window
was picked, so the app asks instead of guessing. The last one used is
preselected.

Settings live in `~/.config/gawk/broadcast.json` (honouring
`XDG_CONFIG_HOME`): the older Go app's file, read as it is, so relays,
secrets, rooms and the encoder cache carry over. Four keys are Linux-only
and hand-edited:

| Key | Effect |
|---|---|
| `encoder` | Pin one cascade element (`vulkanh264enc`, `nvh264enc`, `vah264enc`); it is still trialled |
| `audioDevice` | Pin a PulseAudio device name; skips the audio cascade and the whose-audio step |
| `audioApp` | The last app chosen (written by the app) |
| `lastGoodAudioSource` | The audio cascade's cached winner (written by the app) |

`GAWK_DUMP_H264=<path>` in the environment writes every access unit as sent
(Annex-B) to that file, for a bitstream bug report.

**A CI build**: every green run uploads
`gawk-broadcast-linux-x86_64-<commit sha>`, the same tarball a release
attaches.

When it doesn't work, on Linux:

- **"No working hardware H.264 encoder was found"**: check
  `gst-inspect-1.0 vah264enc` (or `nvh264enc`, `vulkanh264enc`); the
  plugins come from `gstreamer1.0-plugins-bad`. `debug.log` lists each
  candidate's trial verdict.
- **"No screen-share portal found"**: install `xdg-desktop-portal` and the
  backend for your desktop, then log in again.
- **Relay refuses the connection** (`origin rejected` in the relay log):
  the relay's `allowedOrigins` needs `gawk-broadcast://linux`.
- **The whose-audio list is empty**: an app appears only while it plays
  sound.

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
- Anything else: open **Details** while live (or **Settings → Advanced**),
  click **Copy diagnostics**, and send the JSON along with `debug.log`,
  below.

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
| Update check | On: one GET of the release manifest from GitHub at launch, at most daily (off in Advanced, or `GAWK_NO_UPDATE_CHECK=1`) |
| Origin | `gawk-broadcast://windows` — the relay's `-allowed-origins` must include it |
| Rung | 1080p60, 500 ms GOP, 12 Mbps peak VBR — the resolution is a bounding box: the stream keeps the source's aspect ratio inside it |

## Rooms

The **Room** row (R42, [docs/44](../docs/44-rooms.md) §4.8; the R58 layout,
[docs/60](../docs/60-desktop-redesign.md) D8–D10) adds the broadcast to a
room, on a relay started with `-rooms`. **Add** opens a sheet with one
field for a room code or a pasted room link, **Create a new room**, and
**Your rooms**. Chosen before going live, the room shows on the Ready page
("Joins when you go live") and is joined on every start until you dismiss
it; chosen while live, it is joined now.

| Config key | Meaning |
|---|---|
| `room` | The code (or static slug) of the room the next broadcast joins; blank = no room. A pasted link is reduced to its code. Re-attached on every resume. |
| `roomAttachSecret` | That room's attach key, from a pasted link's `?rt=a:…` or typed when the room asked for it (DPAPI-wrapped like the publish secret) |
| `roomCreatorToken` | That room's creator token, from a pasted link's `?rt=c:…` (wrapped the same way) |
| `recentRooms` | "Your rooms": the rooms this app joined, saved ones first, at most 8 unsaved; each keeps its attach key (wrapped the same way) |
| `nickname` | Your name in the room, and what your tile is called (blank = the relay picks one). Editable while live: the roster and the tile follow. |

A gated static room admits you as a watcher and the window asks for its
key in place. In a room, Live shows who is streaming (with **Watch**,
which opens that stream in the browser) and who is watching; a room you
created adds **Manage**: remove a stream, or end the room for everyone.
**Leave room** removes the attachment and closes the room session; the
broadcast itself keeps going. **Copy room link** copies a plain room link
for friends. **Open room view** launches the browser at
`<app URL>/#/room/<CODE>?rt=<grant>`, where the grant is `c:<creator token>`
for a room you made or `a:<attach key>` for a gated static room; the SPA
moves it out of the URL before rendering. Someone else ending the room,
or removing your stream, shows a card; your broadcast is untouched.

## Building

Any host builds and tests the portable crates (wire, engine); the
Windows- and macOS-only code compiles empty elsewhere. A **Linux** host also
builds the Linux shell, which links GStreamer and PipeWire, so it needs
their headers: on Ubuntu 24.04, `libgstreamer1.0-dev
libgstreamer-plugins-base1.0-dev libpipewire-0.3-dev libfontconfig-dev
libxkbcommon-dev clang cmake pkg-config` (`clang` is for the PipeWire
bindings' bindgen). The pinned toolchain in `rust-toolchain.toml`
auto-installs via rustup.

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
- **The Linux integration tests need a sound server and GStreamer's
  plugins.** `crates/audio/tests/pwctl_daemon.rs` starts a private
  PipeWire, WirePlumber and session bus (`pipewire wireplumber dbus
  pipewire-bin`), and the gst tests use `videotestsrc` and a test-only
  `x264enc` (`gstreamer1.0-plugins-{base,good,bad,ugly}
  gstreamer1.0-pipewire`). Without them those tests skip; CI sets
  `GAWK_REQUIRE_PIPEWIRE=1 GAWK_REQUIRE_GST=1`, which turns a skip into a
  failure.
- **The EXE's icon is a linked `.res`, not a compiled `.rc`.**
  `crates/app-windows/build.rs` hands `assets/icon/gawk.res` (generated from the
  shared SVG by `go run ./tools/icon generate`, committed) straight to the
  linker on msvc targets; lld-link and link.exe both take it as an input, so
  there is no `rc.exe`/`llvm-rc` step to install. The window's own icon is
  `Window.icon` in `crates/ui/main.slint`, embedded by slint-build. CI checks the
  resource actually landed (`tools/icon verify-exe`) — docs/53.
- **`Window.icon` only reaches Windows and X11.** winit drops it on macOS and
  Wayland. The macOS Dock icon is the bundle's `gawk.icns` (another
  `tools/icon` derivative, copied in and checked with `iconutil` by
  `tools/macos/bundle.sh`, docs/53 D11). On Wayland it is the installed
  desktop entry matching the app ID, so an uninstalled binary shows the
  stock icon until the `.deb` (docs/63) or `install-desktop.sh` installs it.

## Layout

| Crate | Role |
|---|---|
| `crates/wire` | The wire-format mirror — see below |
| `crates/engine` | Session lifecycle, send policy, resume supervisor. No GUI, no COM/WinRT; media enters through traits, the network through a `RelaySession` seam |
| `crates/capture` | Windows.Graphics.Capture frame source + window/monitor picker enumeration; on macOS the ScreenCaptureKit stream and the system content picker (`sck`, `sck_picker`), with their frame policy in the portable `sck_policy`; on Linux the screen-share portal (`portal`) and the one-clock mapper (`pwclock`) |
| `crates/encode` | Hardware H.264, trial-gated: the Media Foundation MFT cascade on Windows, VideoToolbox's low-latency session (`vt`) on macOS, the GStreamer cascade in-process on Linux (`gst`, with its plan in the portable `gst_policy`); the Annex-B and cadence rules are portable |
| `crates/audio` | Opus under R25's contract: WASAPI process/system loopback on Windows; on macOS the portable PCM shim and audio lane fed by ScreenCaptureKit's audio (`pcm`, `lane`); on Linux the system cascade in GStreamer (`gstsrc`) and the app-audio control plane on its own PipeWire connection (`pwctl`, with its graph rules in the portable `pwgraph`) |
| `crates/ui` | The window both apps show (`main.slint`, compiled once) and the shell that drives it (`shell`: settings, rooms, the session lifecycle, stats, diagnostics) — each app plugs in a `Platform` and a `Media` pipeline |
| `crates/app-windows` | The Windows platform — WGC picker, Media Foundation pipeline, toasts, DPAPI — built as `gawk-broadcast.exe` |
| `crates/app-macos` | The macOS platform — system picker, ScreenCaptureKit → VideoToolbox pipeline — built as `gawk-broadcast-macos`, bundled as `gawk-broadcast-macos.app` by `tools/macos/bundle.sh` (R52, [docs/54](../docs/54-macos-native-broadcaster.md); a stub off macOS) |
| `crates/app-linux` | The Linux platform — portal Share card, whose-audio sheet, the cascade × capture-ladder walk with mid-session rebuild, D-Bus notifications — built as `gawk-broadcast-linux`, packed with `tools/linux/` into the tarball (R56, [docs/58](../docs/58-linux-desktop-broadcaster.md); a stub off Linux) |

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
