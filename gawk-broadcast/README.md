# gawk-broadcast — native Linux broadcaster

Publish your screen to a gawk relay from Linux, **with hardware encode**.

```sh
gawk-broadcast-gui                          # the app you actually use
gawk-broadcast                              # same engine, headless, default relay
gawk-broadcast -url https://relay.example   # …or somebody else's
→ Broadcast code: K7M2QP   join: https://gawk.example/#/view/K7M2QP
```

<!-- GUI SCREENSHOT PLACEHOLDER ────────────────────────────────────────────
  Replace this block with:   ![gawk-broadcast GUI](docs/assets/gui.png)

  What to capture: the Gio GUI mid-broadcast — stats card visible
  (fps, bitrate, keyframe interval, viewer count), version string in the
  window header. PNG, ~800 px wide. Take it on the real broadcasting
  machine so the numbers are believable. Commit as docs/assets/gui.png.
──────────────────────────────────────────────────────────────────────── -->
> 📸 **Screenshot coming here** — the GUI mid-broadcast with live stats.

It speaks the existing publisher protocol byte for byte — zero server,
wire or viewer changes — by importing the relay's own `wire` package. The
browser broadcaster remains the path for Windows/macOS, for Linux machines
without a usable hardware encoder, and for anyone who doesn't want to
install anything.

**Why does this exist?** The browser cannot hardware-encode on Linux — a
platform gap, not a tuning problem. WebCodecs hardware encode ships on
Windows/macOS/Android only; Chromium's Linux encode path is VA-API only,
and on NVIDIA `nvidia-vaapi-driver` is decode-only by design. A native
broadcaster buys **encode capacity and game fps on the broadcasting
machine**, not lower latency. Full reasoning:
[`docs/19`](../docs/19-linux-native-broadcaster.md).

## Install

Grab the latest tarball from the
**[Releases page](https://github.com/Tuhis/gawk/releases)** (assets named
`gawk-broadcast-linux-amd64.tar.gz`), or with the GitHub CLI:

```sh
gh release download --pattern 'gawk-broadcast-linux-amd64.tar.gz'   # newest release
gh release download gawk-broadcast-v1.9.0 --pattern '*.tar.gz'      # a specific one
```

The tarball holds both binaries, `INSTALL.md` and `BUILD-INFO.txt`, with a
`SHA256SUMS` asset beside it (the binaries are unsigned, so the checksum is
the integrity check). Every green CI run also uploads an artifact
(`gawk-broadcast-linux-amd64-<sha>`) for testing unreleased builds —
[`INSTALL.md`](INSTALL.md) is written for a tester with a binary and no
context.

Built on Ubuntu 24.04, but the actual floor is **glibc 2.34** (measured
per build into `BUILD-INFO.txt`), so Ubuntu 22.04+, Debian 12+, RHEL 9+,
Fedora and Arch all run it.

**Runtime dependencies** — all stock distro packages, nothing built from
source:

| Package (Debian/Ubuntu) | Why |
|---|---|
| `gstreamer1.0-tools` | `gst-launch-1.0`, which runs capture + encode |
| `gstreamer1.0-pipewire` | `pipewiresrc` — how desktops already do screencast |
| `gstreamer1.0-plugins-base`, `-good`, `-bad` | the encoders, parser and muxer |
| `xdg-desktop-portal` + your desktop's backend | the share dialog; every Wayland desktop ships one |

On NVIDIA the `nvcodec` plugin `dlopen`s the driver's encode library at
runtime — distro packages suffice. The GUI links both window backends, so
X11 libraries must be present even on a Wayland-only session
(`libxkbcommon-x11` is the one that bites first).

**Wayland or X11?** Use Wayland. Capture goes through the XDG ScreenCast
portal, and portal screencast support is what varies: Wayland works
everywhere; X11 works on GNOME but generally not on KDE. The app gates on
the portal call succeeding, never on the session type.

## Usage

The flags you'll actually set (run `-help` for the full list):

```
-url          relay URL                (env GAWK_URL; default https://api.gawk.ioio.fi:4433)
-app-url      frontend URL, for join links          (env GAWK_APP_URL)
-secret       publish secret, if the relay requires one  (env GAWK_SECRET)
-id           reclaim this broadcast code instead of minting a new one
-resolution   1920x1080    -fps 60    -bitrate 16   (Mbps, the VBR peak)
-audio        publish system audio                  (default true)
-audio-app    when a window is shared, publish only this application's
              audio (its process binary, e.g. supertuxkart)
-encoder      force one of vulkanh264enc, nvh264enc, vah264enc
-v            verbose, including the GStreamer child's stderr
```

Settings persist in `~/.config/gawk/broadcast.json` (mode 0600 — it holds
the publish secret); flags and env override them, and the GUI writes the
same file. Blank means "the default", resolved at use — never frozen in at
save time. Prefer env vars for the secret: a command line is visible in
`ps`.

`gawk-broadcast -version` (or the GUI window header) prints
`v1.9.0+g1a2b3c4` — release plus the commit it was built from; that string
identifies any binary, released or CI-built.

### If the relay restricts origins

With `-allowed-origins` set on the relay, this broadcaster's default
`Origin` of **`gawk-broadcast://native`** must be whitelisted:

```
GAWK_ALLOWED_ORIGINS=https://gawk.example,gawk-broadcast://native
```

…or pass `-origin https://gawk.example` to send an origin the relay
already allows. Loopback connections bypass the check.

### Diagnostics reporting

Against the default relay, the broadcaster reports session diagnostics
(fps, drop counters, RTT, viewer count — never screen content, file names,
machine name or IP) to that fleet's telemetry collector. `-telemetry-url
off` sends nothing; a self-hosted relay reports nowhere unless you point
it at your own collector, since telemetry tokens are minted by the relay
you connect to ([docs/33](../docs/33-telemetry-and-diagnostics.md)). The
CLI prints where it reports on startup; the GUI shows it in settings.

### Terms of use

This broadcaster publishes under the same terms as the browser broadcaster
([`docs/29`](../docs/29-terms-and-conditions.md)): you are responsible for
what you stream. The terms live at `<app-url>/#/terms`; running the native
app is the acknowledgment.

## How it works

The picture stays on the GPU the whole way — portal → PipeWire dmabufs →
a GStreamer child scaling/converting/encoding on the GPU's encode block →
MPEG-TS back over a pipe → the Go engine demuxes access units and ships
them: keyframes whole on a reliable stream, deltas sliced into datagrams.
The Go code never touches a pixel.

Encoders are hardware-only, probed by real trial encodes at startup,
Vulkan first: `vulkanh264enc → nvh264enc → vah264enc → refusal`. There is
deliberately no software rung — a machine where nothing passes is told to
use the browser broadcaster instead.

The full walk-through, the app-audio tee, the encoder cascade and the
package layout: **[`docs/internals.md`](docs/internals.md)**.
Failure modes, debug taps (`GAWK_DUMP_TS`, `GAWK_DUMP_H264`) and hard-won
sharp edges: **[`docs/troubleshooting.md`](docs/troubleshooting.md)**.

## Build

```sh
# Go, plus Gio's Linux headers (Debian/Ubuntu) — GUI only; the CLI and
# engine need none of them:
sudo apt install gcc pkg-config libwayland-dev libx11-dev libx11-xcb-dev \
  libxkbcommon-x11-dev libgles2-mesa-dev libegl1-mesa-dev libffi-dev \
  libxcursor-dev libvulkan-dev

go build ./cmd/...
```

## Test

```sh
go test ./...                     # needs the Gio headers (builds every package)
go test ./internal/...            # engine only; no headers needed
go test -short ./internal/...     # skips the tests that build and run the real relay
```

The engine's integration tests build and run the real `gawk-server` and
publish a committed H.264 fixture through it; the app-audio tests drive
the real helper against a private headless PipeWire daemon (they skip if
PipeWire tooling is absent — CI installs it). Details:
[`docs/internals.md`](docs/internals.md#testing-notes).

This is not a container/chart/CI-deploy component: it's a binary you run
on your own PC, deliberately outside the cluster deployment model.
