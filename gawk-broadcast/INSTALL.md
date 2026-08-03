# Installing gawk-broadcast (prebuilt test build)

You've been handed `gawk-broadcast-gui` (the app), `gawk-broadcast` (a
headless CLI with the same engine) and `gawk-pw-helper` (a small program the
other two run for themselves — keep it next to them). This is a **test build of unreleased
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
| **`gawk-pw-helper` beside the app** | `ls gawk-pw-helper` | Only per-application audio needs it; everything else works without it |
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
# optional, for the system-audio fallback source (see "System audio" below):
sudo apt install gstreamer1.0-pulseaudio
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
| `-plugins-base`, `-good` | scaling, conversion, rate limiting — and the audio chain: `opusenc`, `audioconvert`, `audioresample` |
| `gstreamer1.0-pulseaudio` | **optional.** `pulsesrc`, the *fallback* system-audio source. The primary one is `pipewiresrc`, which you already have |
| `xdg-desktop-portal` + backend | the share dialog. Your desktop almost certainly has one already |
| `libpipewire-0.3` | already on your machine — PipeWire is what the share dialog runs on. `gawk-pw-helper` links it directly for per-application audio |

## System audio

The broadcaster publishes your system audio — whatever is coming out of your
speakers — alongside the picture, on by default. There is nothing to configure
and nothing new in the share dialog: the audio does not come from the screen
share at all (the portal carries no audio), it comes from PipeWire directly,
capturing the **monitor of your default output device**.

One consequence worth knowing before you go looking for a setting: when you
share a **whole screen**, it is whole-system output. If you do not want a
notification sound in your broadcast, mute the app, not gawk. (When you share a
*single window*, you get the choice — see the next section.)

Switching your output device mid-broadcast (headphones ↔ speakers) is expected
to keep working: the primary capture source follows your default output rather
than binding to one device at start.

**If this machine has no usable audio source, the broadcast publishes video and
says so** — in the window, in the stats and in the CLI's stats line. That is not
an error and needs no action; audio never costs a broadcast.

To turn it off entirely: `gawk-broadcast -audio=false`, or `"disableAudio":
true` in the config file. To pin one specific device instead of probing:
`-audio-device <name>` (a PulseAudio/pipewire-pulse device name, e.g.
`alsa_output.pci-0000_00_1f.3.analog-stereo.monitor` — `pactl list short
sources` lists them).

## Sharing one application (window + its audio only)

Pick a **window** rather than a screen in the share dialog and the app asks one
extra question: *whose audio?* Choose the game, and viewers hear the game and
nothing else — not your notifications, not your voice call, not the video
playing on your second monitor.

Why the extra click, when the Windows build does it in one: the share dialog is
your desktop's, not ours, and it deliberately never tells an application *which*
application owns the window you picked. There is no way to know, so gawk asks
instead of guessing — a guess that went wrong would silently stream the wrong
application's sound, which is the worst thing this feature could do.

What to expect:

- **The list only shows applications that are currently playing sound.** If it
  looks empty, start the game's audio (walk into the menu, unmute) and the list
  fills in by itself — it updates live, so there is nothing to restart.
- **Whole system** and **No audio** are always offered, so picking a window
  never forces you into per-application audio.
- The application you chose last time is preselected when it is running, so
  restarting the same game is a single Enter.
- If the game goes quiet for ten seconds, the window offers a one-click switch
  to whole-system audio. Some games route their sound through a helper process
  with a different name; that button is the escape hatch, and using it is
  invisible to viewers.
- Sharing a window also means the picture is **fitted** inside your configured
  resolution rather than stretched to fill it, so a 4:3 or portrait window keeps
  its shape. That is why the stats may say something like `1542x1080`.

This is what `gawk-pw-helper` is for. It creates a hidden PipeWire sink and
copies the chosen application's audio into it — a *copy*: the application keeps
playing to your speakers exactly as before, and nothing about your audio routing
is changed. Everything it creates belongs to its own connection, so it all
disappears when it exits, however it exits.

**If the helper is missing** the app says so in one sentence and offers
whole-system audio instead. Nothing else is affected.

Headless, the same choice is a flag:

```sh
gawk-broadcast -audio-app supertuxkart     # pick a WINDOW in the dialog
```

The name is the process's executable name — `pw-cli ls Client` shows
`application.process.binary` for everything currently playing. Unset means
system audio, exactly as before. `-audio-device` wins over both: naming a device
pins it and the question is not asked.

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
libraries at all. `gawk-pw-helper` links libc and `libpipewire-0.3`, which is
present by definition on any machine where the share dialog works. Check it the
same way:

```sh
ldd ./gawk-pw-helper | grep "not found"        # silence is good
```

**On NVIDIA**: nothing extra. The encoder library ships with your driver and is
loaded at runtime.

## Run it

```sh
chmod +x gawk-broadcast-gui gawk-broadcast gawk-pw-helper
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
# Go ≥ 1.26, plus Gio's build headers (Debian/Ubuntu):
sudo apt install golang gcc pkg-config libwayland-dev libx11-dev libx11-xcb-dev \
  libxkbcommon-x11-dev libgles2-mesa-dev libegl1-mesa-dev libffi-dev \
  libxcursor-dev libvulkan-dev

git clone https://github.com/Tuhis/gawk
cd gawk/gawk-broadcast
go build ./cmd/...
```

Those headers are only needed to build the GUI. The runtime dependency list
above still applies.
