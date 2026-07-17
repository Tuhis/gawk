# gawk-broadcast — native Linux broadcaster

Publish your screen to a gawk relay from Linux, **with hardware encode**.

```sh
gawk-broadcast-gui                          # the app you actually use
gawk-broadcast -url https://relay.example   # same engine, headless
→ Broadcast code: K7M2QP   join: https://gawk.example/#/view/K7M2QP
```

It speaks the existing publisher protocol byte for byte — zero server changes,
zero wire changes, zero viewer changes — by importing the relay's own `wire`
package rather than reimplementing it. The browser broadcaster is untouched and
remains the path for Windows/macOS, for Linux machines without a usable hardware
encoder, and for anyone who doesn't want to install anything.

Design and decisions: [`docs/19`](../docs/19-linux-native-broadcaster.md).

## Why this exists

**The browser cannot hardware-encode on Linux.** This is a platform gap, not a
tuning problem, and it is worth stating precisely so nobody re-litigates it with
a flag hunt:

- WebCodecs `VideoEncoder` hardware encode ships on Windows, macOS and Android.
  Linux gets hardware *decode* only, and Chromium's own VA-API doc disclaims
  Linux support outright.
- On NVIDIA it is structurally impossible: Chromium's Linux encode path is
  VA-API only, and `nvidia-vaapi-driver` is NVDEC-backed and decode-only by
  design.

So a Linux broadcaster means software x264 — and the game fps that costs —
unless it stops being a browser. What this buys is **encode capacity and game
fps on the broadcasting machine**, not lower latency: the glass-to-glass budget
is unchanged.

## Install

Handing a build to someone else to test? Point them at
[`INSTALL.md`](INSTALL.md) — it's written for a tester with a binary and no
context, and CI ships a copy of it inside the artifact. Every green CI run on a
branch or PR uploads `gawk-broadcast-linux-amd64-<sha>` (both binaries +
`BUILD-INFO.txt` recording the commit, the glibc floor and the exact library
linkage). Grab it from the run's Artifacts, or:

```sh
gh run download --name gawk-broadcast-linux-amd64-<sha>
```

Built on Ubuntu 24.04, but that is **not** the floor: the binaries reference
glibc symbols only up to **2.34** (measured per build into `BUILD-INFO.txt`,
never assumed from the builder), so Ubuntu 22.04+, Debian 12+, RHEL 9+, Fedora
and Arch all run them. Two linkage facts worth knowing before someone reports a
bug: the GUI links **both** window backends, so X11 libraries must be present
even on a Wayland-only session (`libxkbcommon-x11` is the one that bites
first), and **Vulkan is `dlopen`ed, not linked** — nothing to install for it.

**Wayland or X11?** Use Wayland. Capture goes through the XDG ScreenCast
portal, and portal screencast support is what varies: Wayland works everywhere
(GNOME/KDE/wlroots), X11 works on GNOME but generally not on KDE, whose portal
offers no screencast backend there. The app gates on the portal call
succeeding, never on the session type (docs/19 Decision 5) — so it doesn't
refuse X11, it just reports what the portal says. The Gio window itself prefers
Wayland and falls back to X11 on its own.

Everything below is a stock distro package. Nothing is built from source.

**Runtime:**

| Package (Debian/Ubuntu) | Why |
|---|---|
| `gstreamer1.0-tools` | `gst-launch-1.0`, which runs capture + encode |
| `gstreamer1.0-pipewire` | `pipewiresrc` — how desktops already do screencast |
| `gstreamer1.0-plugins-base`, `-good`, `-bad` | the encoders (`vulkanh264enc`, `nvh264enc`, `vah264enc`), the parser and the muxer |
| `xdg-desktop-portal` + your desktop's backend | the share dialog; every Wayland desktop already ships one |

On NVIDIA the `nvcodec` plugin `dlopen`s the driver's encode library at runtime,
so distro packages suffice — there is nothing special to build.

**Build:**

```sh
# Go, plus Gio's Linux headers (Debian/Ubuntu):
sudo apt install gcc pkg-config libwayland-dev libx11-dev libx11-xcb-dev \
  libxkbcommon-x11-dev libgles2-mesa-dev libegl1-mesa-dev libffi-dev \
  libxcursor-dev libvulkan-dev

go build ./cmd/...
```

Those headers are the honest price of a native Wayland GUI (docs/19
Decision 14). They are only needed for `cmd/gawk-broadcast-gui`; the engine and
the CLI need none of them, so `go test ./internal/...` works without them.

## Usage

```
gawk-broadcast -url https://relay.example:4433 [flags]

  -url         relay URL                       (env GAWK_URL)
  -app-url     frontend URL, for join links    (env GAWK_APP_URL)
  -secret      publish secret, if required     (env GAWK_SECRET)
  -origin      Origin header to send           (env GAWK_ORIGIN)
  -id          reclaim this broadcast code instead of minting a new one
  -resolution  1920x1080     -fps 60     -bitrate 8   (Mbps)
  -encoder     force one of vulkanh264enc, nvh264enc, vah264enc
  -insecure    skip TLS verification (development certificates only)
  -v           verbose, including the GStreamer child's stderr
```

Settings live in `~/.config/gawk/broadcast.json` (mode 0600); flags and env
override them. The GUI writes the same file. Prefer the env vars for the secret:
a command line is visible in `ps`.

### If the relay restricts origins

If your relay runs with `-allowed-origins` / `GAWK_ALLOWED_ORIGINS` set (the
"safe by default" install), it checks the `Origin` header on every connection.
A browser sends the frontend's origin automatically; this native broadcaster
sends **`gawk-broadcast://native`** by default. Either whitelist that on the
relay —

```
GAWK_ALLOWED_ORIGINS=https://gawk.example,gawk-broadcast://native
```

— or point the broadcaster at an origin the relay already allows, with
`-origin https://gawk.example` (env `GAWK_ORIGIN`), and add no relay entry.
Without one of these, the relay rejects the dial and the broadcaster reports it
could not connect. (Loopback connections bypass the check, so this only bites
over a real network — the homelab case.)

**The file is 0600 for a reason.** It holds the publish secret, which is a
credential — anything that can read it can publish to the relay under your
broadcaster identity.

## How it works

The shape to hold in your head: **the picture stays on the GPU the whole way,
and our Go code never touches a pixel.** By the time anything reaches this
process it is already H.264 — the app is a byte pump, which is why it can be
simple.

1. **You click Start.** It dials `/publish` *first*. If the relay refuses —
   wrong secret, code taken, at capacity — no share dialog ever appears.
2. **It asks your desktop for a screen, over D-Bus.** The XDG ScreenCast portal
   shows *your* desktop's share dialog. We don't draw it, we don't theme it.
   **The picker appears on every start — we never persist the choice, so you
   pick what to share each run.**
3. **The portal returns a PipeWire fd.** Frames are dmabufs — handles to GPU
   memory, not pixels. Nothing has been copied.
4. **GStreamer runs as a child process**, given that fd, writing to our pipe.
5. **It rate-limits (drop-only), scales and converts on the GPU**, then
   **encodes on the GPU's dedicated encode block** — not the CPU, and not the
   shader cores, so it isn't taking from the game's frame budget. This is the
   step the browser cannot do on Linux.
6. **Compressed MPEG-TS comes back down the pipe.** Now it is in CPU memory —
   but it's ~1% of the size, because it's already H.264.
7. **We split it into access units** (one PES = one frame — structural, no
   start-code guessing), stamp arrival, and read the NAL types.
8. **Keyframes go whole on a reliable stream; deltas are sliced into
   datagrams.** Exactly the browser's R8 design, because the bytes are
   identical.

## Encoders

Hardware only, probed per machine at startup, **Vulkan first**:

```
vulkanh264enc  →  nvh264enc  →  vah264enc  →  refusal
```

Vulkan Video is the target encode API (docs/19 Decision 21): the only one
spanning RADV, ANV and NVIDIA with no asterisk. NVENC and VAAPI stay behind it
permanently — friends broadcast from GPUs we can't survey.

**There is no software rung, by decision.** Hardware encode is the entire reason
this app exists, and the browser broadcaster already covers software encode
fine. A machine where nothing passes is told to use the browser, not quietly
degraded to x264 — which would burn the game's CPU budget to produce a stream
the browser could have produced.

Candidates are accepted only by a **real trial encode**, never by the element
being listed: `gst-inspect-1.0` happily lists elements that fail at runtime on
the actual device. Trials encode `videotestsrc`, never the portal — probing must
not pop share dialogs. The last-good encoder is cached and *re-verified* (not
trusted) on the next run, and because a `videotestsrc` trial can't prove the real
path's dmabuf import, **the live start is the final probe**: a child that dies
immediately retries with the capture pinned to system memory, then advances the
cascade — every retry reusing the portal grant.

## Fixed rung

1080p60, 500 ms GOP. No ladder, no auto-fallback, no mid-session changes: this
engine is hardware-encode by construction, so 1080p60 is coherent rather than
aspirational. R4's `FallbackController` is deliberately not ported — its trigger
is `encodeQueueSize` growth, and R4's own hardware finding was that hardware
encoders drain frames without that signal ever firing.

## Gotchas

- **This is the Annex-B publisher.** The engine emits raw Annex-B with **empty
  DecoderConfig extradata** and never builds an avcC record; the viewer's
  `isAnnexB` start-code sniff routes it into the branch that ignores extradata.
  The browser broadcaster always sends AVCC, so **this path is the only thing
  exercising the viewer's Annex-B branch** — if it ever regresses, native
  broadcasts break and browser ones don't.
- **`h264parse config-interval=-1` is load-bearing, not cosmetic.** Empty
  extradata means a late joiner primed with the relay's cached keyframe can only
  decode if SPS/PPS travel *inside* the keyframe AU. Without it, late joiners
  see nothing while everyone already watching is fine — the worst kind of bug.
- **Don't gate on Wayland.** The portal works on X11 GNOME sessions too. Gate on
  the portal call succeeding.
- **`pipewiresrc` dying in preroll with `stream error: unhandled format` is a
  capture problem, not an encoder problem.** The compositor's chosen screencast
  format sometimes can't be mapped onto the downstream caps (DMA-BUF
  modifier/DRM-caps skew between `gst-plugin-pipewire` and the encoder's
  converter, or 10-bit HDR desktop formats). The live start retries each
  encoder with system-memory capture (bare `video/x-raw` pinned at the source)
  before advancing, and an all-`pipewiresrc` failure reports as a capture
  error, never "no hardware encoder". Diagnose with `GST_DEBUG=pipewire*:5` in
  the environment — the child inherits it and its stderr lands in the debug
  log.
- **`GAWK_DUMP_TS=<path>` tees the raw MPEG-TS to disk** while broadcasting.
  Play it with `mpv`/`ffplay` — it is exactly what the encoder produced, so it
  splits "the capture is black at the source" from "the viewer can't decode
  it" in one step.
- **A dropped delta drops the rest of its GOP.** The pump's channel-full
  drops happen before frameIds are assigned, so the wire stays contiguous and
  the viewer's freeze-on-gap cannot see them — sending the GOP remainder
  would decode as reference-broken garbage (the first real viewer session
  did exactly that). `Source.offer` gates deltas until the next keyframe,
  and a keyframe arriving into a full queue flushes the stale backlog rather
  than being dropped. Under sustained backpressure the stream degrades to a
  clean keyframe cadence, never to artifacts.
- **`pipewiregrab` is not in mainline FFmpeg** — an unmerged patchset carried
  downstream by Jami. Mainline ffmpeg has no PipeWire input at all. Don't
  re-propose it without checking it actually merged.
- **Notifications must be critical urgency to matter.** KDE's portal inhibits
  normal notifications *while screen casting*, so the act of broadcasting
  suppresses them. Failures use critical urgency; going live doesn't.
- **No viewer count.** Nothing on the wire tells a publisher about subscribers.
  The browser broadcaster doesn't know either.
- **Building the GUI needs the Gio headers above**; `go test ./internal/...`
  does not.

## Layout

```
internal/engine    Session{Start,Stop} + Callbacks + StartError{Phase,Status}
internal/portal    XDG ScreenCast handshake (godbus); picker every start
internal/gst       subprocess supervision + pipeline construction + cascade
internal/mpegts    TS/PES demux → one AU per PES
internal/config    ~/.config/gawk/broadcast.json (0600)
internal/notify    D-Bus notifications with urgency
internal/app       the GUI's logic, without the GUI
cmd/gawk-broadcast      CLI shell — headless, harness, debug
cmd/gawk-broadcast-gui  GUI shell — Gio window + notifications
```

`Session`/`Callbacks` deliberately mirror the TypeScript
`BroadcastSessionLike`/`BroadcastCallbacks` in
`gawk-app/src/transport/broadcaster.ts`. Same idea, same names, other language:
a second broadcaster only stays survivable if the two remain legible to each
other.

## Tests

```sh
go test ./...                     # needs the Gio headers (it builds every package)
go test ./internal/...            # engine only; no headers needed
go test -short ./internal/...     # skips the tests that build and run the real relay
```

The engine's integration tests **build and run the real `gawk-server`** and
publish a committed H.264 fixture through it, then attach a real subscriber and
check what comes out. A hand-written fake relay would only test the engine
against our belief about the relay — the belief most worth doubting in a second
implementation.

This is not a container/chart/CI-deploy component: it's a binary you run on your
own PC, deliberately outside the cluster deployment model.
