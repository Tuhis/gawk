# Installing gawk-broadcast (prebuilt test build)

You've been handed `gawk-broadcast-gui` (the app) and `gawk-broadcast` (a
headless CLI with the same engine). This is a **test build of unreleased
software that has never run on hardware other than the developer's** — if it
falls over, that is the expected outcome and the reason you're running it.

`BUILD-INFO.txt` next to these files records exactly which commit they came
from and what they link against.

## Does it run on your machine?

Three things decide it. Check them before anything else:

| Requirement | Check | If not |
|---|---|---|
| **Linux, x86-64** | `uname -m` → `x86_64` | No build for you yet |
| **glibc ≥ 2.34** | `ldd --version` | Build from source (below) — a prebuilt binary can't be made to work |
| **The libraries it links** | `ldd ./gawk-broadcast-gui \| grep "not found"` — silence is good | Install them (below) |
| **A hardware H.264 encoder** | just run it; it tells you | Use the browser broadcaster instead — this app has no software fallback, on purpose |

glibc 2.34 means **Ubuntu 22.04+, Debian 12+, RHEL 9+, Fedora, Arch** — i.e.
almost certainly you. The binaries are built on Ubuntu 24.04, but that's not
the floor: they only reference the glibc symbols they actually use, and the
newest of those is 2.34. `BUILD-INFO.txt` records the measured floor for the
exact build you were given.

If it *does* fail to start, read the error before assuming it's glibc — a
missing shared library is the likelier cause, and it says so plainly:

```
error while loading shared libraries: libxkbcommon-x11.so.0: cannot open shared object file
```

That's the next section, not a rebuild.

## Wayland or X11?

**Use a Wayland session.** Log out and pick one at the login screen if you're
on X11.

Not a style preference — it's about how screen capture works here. Capture goes
through the **XDG ScreenCast portal** (your desktop's own "share your screen"
dialog, the same one Zoom/OBS/Discord use), and the portal's screencast support
is what actually varies:

- **Wayland**: works on GNOME, KDE and wlroots compositors (Sway, Hyprland).
  This is the path the app is designed for.
- **X11**: works on **GNOME** — its portal exposes screencast on X11 too. On
  **KDE/X11** the portal generally offers no screencast backend, so the app
  can't capture; a Wayland session fixes it.

The app itself doesn't check which session you're in — it just asks the portal
and reports what the portal says. So "does it work on X11?" is answered by
trying it, not by us. If you're curious, try both and tell us; that's useful
data.

The GUI window works on either: it prefers Wayland natively and falls back to
X11 automatically.

## Install the runtime dependencies

Everything is a stock distro package. Nothing is built from source, nothing is
downloaded from a vendor.

**Debian / Ubuntu:**

```sh
sudo apt install \
  gstreamer1.0-tools gstreamer1.0-pipewire \
  gstreamer1.0-plugins-base gstreamer1.0-plugins-good gstreamer1.0-plugins-bad \
  xdg-desktop-portal
# plus your desktop's portal backend, if it isn't already there:
sudo apt install xdg-desktop-portal-gnome   # GNOME
sudo apt install xdg-desktop-portal-kde     # KDE
sudo apt install xdg-desktop-portal-wlr     # Sway/Hyprland/wlroots
```

**Fedora:**

```sh
sudo dnf install gstreamer1-plugins-base gstreamer1-plugins-good \
  gstreamer1-plugins-bad-free gstreamer1-plugin-pipewire xdg-desktop-portal
sudo dnf install xdg-desktop-portal-gnome   # or -kde, or -wlr
```

**Arch:**

```sh
sudo pacman -S gstreamer gst-plugins-base gst-plugins-good gst-plugins-bad \
  gst-plugin-pipewire xdg-desktop-portal
sudo pacman -S xdg-desktop-portal-gnome     # or -kde, or -wlr
```

What each is for:

| Package | Why |
|---|---|
| `gstreamer1.0-tools` | `gst-launch-1.0` — the app runs capture + encode in it as a child process |
| `gstreamer1.0-pipewire` | `pipewiresrc` — receives frames from the portal |
| `-plugins-bad` | the encoders themselves (`vulkanh264enc`, `nvh264enc`, `vah264enc`), plus `h264parse` and `mpegtsmux`. Despite the name, this is where the hardware encoders live — it's not optional |
| `-plugins-base`, `-good` | scaling, conversion, rate limiting |
| `xdg-desktop-portal` + backend | the share dialog. Your desktop almost certainly has one already |

**The GUI needs X11 libraries even on Wayland.** Both window backends are
compiled in — the app picks Wayland at runtime and only falls back to X11 — but
the binary links against both, so `libX11`, `libX11-xcb`, `libXcursor`,
`libXfixes` and `libxkbcommon-x11` must be *present* even in a session that
never uses them. Any normal desktop install already has them; a minimal or
X11-free setup is where this bites (it's the first thing that broke when we
tested the artifact on a bare box):

```sh
# Debian/Ubuntu, only if something below reports "not found":
sudo apt install libwayland-client0 libwayland-cursor0 libwayland-egl1 \
  libx11-6 libx11-xcb1 libxcursor1 libxfixes3 libxkbcommon0 \
  libxkbcommon-x11-0 libegl1 libffi8
```

To check your machine rather than guess:

```sh
ldd ./gawk-broadcast-gui | grep "not found"    # silence is good
```

`BUILD-INFO.txt` lists the full linkage of the exact build you were given.
**Vulkan is not in it**: the GUI loads it at runtime if present and uses
OpenGL/EGL otherwise, so there's nothing to install. (The *encoder* cascade
tries Vulkan first, but that's GStreamer's business, and it comes from the
plugin packages above.)

The CLI (`gawk-broadcast`) links against nothing but libc — no desktop
libraries at all.

**On NVIDIA**: nothing extra. The encoder library ships with your driver and is
loaded at runtime.

## Run it

```sh
chmod +x gawk-broadcast-gui gawk-broadcast
./gawk-broadcast-gui
```

Then: **Start** → your desktop shows its own share dialog → pick a screen → you
get a 6-character code and a join link to share.

**You pick what to share every time you Start.** We deliberately don't persist
the choice, so the desktop's share dialog appears on each run.

**It already knows where to broadcast.** Out of the box both the GUI and the
CLI point at the default relay, `https://api.gawk.ioio.fi:4433`, so there is
nothing to configure for the common case. To use a different relay (and a
publish secret, if that one needs it), fill in the GUI's fields or use the CLI:

```sh
GAWK_SECRET=… ./gawk-broadcast -url https://relay.example:4433 -app-url https://gawk.example
```

**It also reports diagnostics by default** — fps, drop counters, RTT and the
like, no screen content and nothing that identifies you or your machine — to
the default fleet's collector, and only while you're using that fleet. Type
`off` into the GUI's **Telemetry URL** field, or run the CLI with
`-telemetry-url off`, and the app sends nothing at all. The README's
"Diagnostics reporting" section has the details.

If the relay restricts origins (`-allowed-origins`) and rejects the connection,
either whitelist `gawk-broadcast://native` on the relay or run the broadcaster
with `-origin <an-already-allowed-origin>` — see the README's "If the relay
restricts origins".

Settings persist to `~/.config/gawk/broadcast.json`, mode `0600` — it holds the
publish secret, a credential that lets anyone who reads it publish under your
broadcaster identity, so treat the file accordingly.

## When it doesn't work

The failure worth reporting most is **"no hardware encoder"**: it means the
cascade (`vulkanh264enc` → `nvh264enc` → `vah264enc`) found nothing that
survives a real trial encode on your GPU. That's a legitimate result on some
hardware — the app deliberately refuses rather than falling back to a software
encoder, because burning your CPU to produce a stream the browser could have
produced is worse than saying no. If you hit it, use the browser broadcaster
and tell us what GPU/driver you have.

Run the CLI with `-v` for the GStreamer child's own stderr, which is where the
real error usually is:

```sh
./gawk-broadcast -url … -v
```

Useful facts when something looks wrong:

- **Notifications**: KDE's portal suppresses normal notifications *while you're
  screen casting*, so failure notifications are sent at critical urgency to get
  through. If you're in a fullscreen game and something breaks, that's the
  signal you should see.
- **Viewer count**: the relay pushes a live "N watching" number once a
  second; the GUI's stats card shows it and rings a critical-urgency
  notification the first time a viewer joins (once per broadcast).
- **Closing the window ends the broadcast.** There's no tray icon and no
  background mode.
- **No preview** — you're looking at your own screen already.

## Building from source instead

Needed if your glibc is older than 2.34, or you're not on x86-64.

```sh
# Go ≥ 1.23, plus Gio's build headers (Debian/Ubuntu):
sudo apt install golang gcc pkg-config libwayland-dev libx11-dev libx11-xcb-dev \
  libxkbcommon-x11-dev libgles2-mesa-dev libegl1-mesa-dev libffi-dev \
  libxcursor-dev libvulkan-dev

git clone https://github.com/Tuhis/gawk
cd gawk/gawk-broadcast
go build ./cmd/...
```

Those headers are only needed to build the GUI. The runtime dependency list
above still applies.
