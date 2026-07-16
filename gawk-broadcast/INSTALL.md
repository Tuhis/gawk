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
| **glibc at least as new as the build's** | `ldd --version` vs. the `glibc:` line in `BUILD-INFO.txt` | Build from source (below) — a prebuilt binary can't be made to work |
| **A hardware H.264 encoder** | just run it; it tells you | Use the browser broadcaster instead — this app has no software fallback, on purpose |

The glibc one is the common trap: these binaries are built on the CI runner's
Ubuntu, so a distro with an older glibc (Debian 12, Ubuntu 22.04) refuses to
start them with a `GLIBC_2.xx not found` error. That's not a bug to report,
it's a rebuild.

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

The GUI's own libraries (Wayland, X11, EGL, Vulkan, xkbcommon) are part of any
desktop install — you should not need to install any of them. `BUILD-INFO.txt`
lists them all if something turns out to be missing; a `not found` line in its
`ldd` output is the smoking gun.

**On NVIDIA**: nothing extra. The encoder library ships with your driver and is
loaded at runtime.

## Run it

```sh
chmod +x gawk-broadcast-gui gawk-broadcast
./gawk-broadcast-gui
```

Then: **Start** → your desktop shows its own share dialog → pick a screen → you
get a 6-character code and a join link to share.

**You pick your screen once, ever.** The app stores the portal's restore token,
so later runs skip the dialog entirely.

You'll need the relay URL (and the publish secret, if it's set) from whoever
sent you this. Either put them in the GUI's fields, or use the CLI:

```sh
GAWK_SECRET=… ./gawk-broadcast -url https://relay.example:4433 -app-url https://gawk.example
```

Settings persist to `~/.config/gawk/broadcast.json`, mode `0600` — it holds the
publish secret *and* the restore token, and that token grants screen capture
with no dialog, so treat the file as a credential rather than a preference.

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
- **There's no viewer count.** Nothing in the protocol tells a broadcaster who's
  watching — not a missing feature, and the browser broadcaster doesn't know
  either.
- **Closing the window ends the broadcast.** There's no tray icon and no
  background mode.
- **No preview** — you're looking at your own screen already.

## Building from source instead

Needed if your glibc is older than the build's, or you're not on x86-64.

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
