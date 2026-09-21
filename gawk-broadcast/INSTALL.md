# Installing gawk-broadcast (Linux)

The tarball holds `gawk-broadcast-gui` (the app), `gawk-broadcast` (the same
engine as a headless CLI) and `gawk-pw-helper`, which the other two run for
per-application audio — keep all three in the same directory. `BUILD-INFO.txt`
records the commit they were built from and what they link against.

## Requirements

| Requirement | Check |
|---|---|
| Linux, x86-64 | `uname -m` → `x86_64` |
| glibc ≥ 2.34 (Ubuntu 22.04+, Debian 12+, RHEL 9+, Fedora, Arch) | `ldd --version` |
| A Wayland session (X11 works on GNOME, generally not on KDE) | log out and pick one at the login screen if needed |
| A hardware H.264 encoder | just run it; it tells you. There is no software fallback — use the browser broadcaster instead |

## Install the dependencies

All stock distro packages. Screen capture goes through your desktop's own
share dialog (the XDG ScreenCast portal), so install the portal backend for
your desktop if it isn't there already.

Debian / Ubuntu:

```sh
sudo apt install gstreamer1.0-tools gstreamer1.0-pipewire \
  gstreamer1.0-plugins-base gstreamer1.0-plugins-good gstreamer1.0-plugins-bad \
  xdg-desktop-portal
sudo apt install xdg-desktop-portal-gnome   # or -kde, or -wlr (Sway/Hyprland)
```

Fedora:

```sh
sudo dnf install gstreamer1-plugins-base gstreamer1-plugins-good \
  gstreamer1-plugins-bad-free gstreamer1-plugin-pipewire xdg-desktop-portal
sudo dnf install xdg-desktop-portal-gnome   # or -kde, or -wlr
```

Arch:

```sh
sudo pacman -S gstreamer gst-plugins-base gst-plugins-good gst-plugins-bad \
  gst-plugin-pipewire xdg-desktop-portal
sudo pacman -S xdg-desktop-portal-gnome     # or -kde, or -wlr
```

NVIDIA needs nothing extra: the encoder library ships with the driver.

## Run it

```sh
chmod +x gawk-broadcast-gui gawk-broadcast gawk-pw-helper
./gawk-broadcast-gui
```

Press **Start**, pick a screen or window in the share dialog, and share the
6-character code or join link it gives you. It already points at the default
relay; nothing needs configuring. Closing the window ends the broadcast.

To use a different relay, fill in the GUI's fields or run the CLI:

```sh
GAWK_SECRET=… ./gawk-broadcast -url https://relay.example:4433 -app-url https://gawk.example
```

By default the app reports session diagnostics (fps, drops, RTT — never screen
content) to the default relay's collector. Set the GUI's **Telemetry URL** to
`off`, or pass `-telemetry-url off`, to send nothing.

Optional — add a launcher entry and icon for your user (no root):

```sh
./install-desktop.sh              # ./install-desktop.sh --uninstall to remove
```

Run it from the directory you unpacked into and keep the files there; if you
move them, run it again.

## When it doesn't work

- **`error while loading shared libraries: … cannot open shared object file`**
  — a package is missing. `ldd ./gawk-broadcast-gui | grep "not found"` names
  it. The GUI needs the X11 libraries (`libxkbcommon-x11` first of all) even on
  Wayland.
- **"no hardware encoder"** — none of `vulkanh264enc`, `nvh264enc`,
  `vah264enc` works on this GPU. Use the browser broadcaster and tell us your
  GPU and driver.
- **The share dialog offers no screen** — the portal has no screencast backend
  in this session. Switch to Wayland.
- **Per-application audio isn't offered** — `gawk-pw-helper` isn't beside the
  app. Whole-system audio still works.
- Anything else: `./gawk-broadcast -v` prints the GStreamer child's own stderr,
  which is where the real error usually is.

The flags, audio options, rooms and build-from-source steps are in the
[README](https://github.com/Tuhis/gawk/blob/main/gawk-broadcast/README.md).
