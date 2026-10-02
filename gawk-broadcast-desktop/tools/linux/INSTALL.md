# Installing gawk-broadcast (Linux)

The tarball holds `gawk-broadcast-linux`, the app. `BUILD-INFO.txt` records
the commit it was built from, the glibc it needs and what it links against.

On Ubuntu 24.04 or newer, or Debian 13 or newer, the same release also has
`gawk-broadcast_<version>_amd64.deb`, which is simpler:

```sh
sudo apt install ./gawk-broadcast_*_amd64.deb   # the one you downloaded
```

It installs the packages below, the app, and its launcher entry and icon.
If you ran this tarball's `install-desktop.sh` before, run
`./install-desktop.sh --uninstall` first, or your launcher keeps starting the
tarball copy. To update, install the newer `.deb` the same way. The rest of
this file is for the tarball.

## Requirements

| Requirement | Check |
|---|---|
| Linux, x86-64 | `uname -m` → `x86_64` |
| Ubuntu 24.04 or newer, or equivalent: Debian 13, Fedora 40+, Arch (glibc 2.39, GStreamer 1.24, PipeWire 1.0) | `ldd --version`, `gst-inspect-1.0 --version` |
| A desktop with the XDG screen-share portal. Tested on KDE Plasma (Wayland); GNOME and wlroots desktops should work | log out and pick a Wayland session at the login screen if needed |
| A hardware H.264 encoder | run it; it tells you. There is no software fallback, so use the browser broadcaster instead |

## Install the dependencies

All stock distro packages. Screen capture goes through your desktop's own
share dialog, the XDG ScreenCast portal, so install the portal backend for
your desktop if it isn't there already.

Debian / Ubuntu:

```sh
sudo apt install gstreamer1.0-pipewire gstreamer1.0-plugins-base \
  gstreamer1.0-plugins-good gstreamer1.0-plugins-bad xdg-desktop-portal
sudo apt install xdg-desktop-portal-kde     # or -gnome, or -wlr (Sway/Hyprland)
```

Fedora:

```sh
sudo dnf install gstreamer1-plugins-base gstreamer1-plugins-good \
  gstreamer1-plugins-bad-free gstreamer1-plugin-pipewire xdg-desktop-portal
sudo dnf install xdg-desktop-portal-kde     # or -gnome, or -wlr
```

Arch:

```sh
sudo pacman -S gst-plugins-base gst-plugins-good gst-plugins-bad \
  gst-plugin-pipewire xdg-desktop-portal
sudo pacman -S xdg-desktop-portal-kde       # or -gnome, or -wlr
```

NVIDIA needs nothing extra: the encoder library ships with the driver.
libopus is built into the app, so it is not on the list.

## Run it

```sh
./gawk-broadcast-linux
```

Choose what to share in your desktop's own dialog: one window or a whole
screen. For a window, the app then asks whose audio to send. Pick the
application that is playing sound, or **Whole system**, or **No audio**.
Press **Go live** and share the 6-character code or join link. It already
points at the default relay; nothing needs configuring. Closing the window
ends the broadcast.

By default the app reports session diagnostics (fps, drops, RTT, never screen
content) to the default relay's collector. Set **Diagnostics** to `off` in
Settings to send nothing.

Optional: add a launcher entry and icon for your user (no root):

```sh
./install-desktop.sh              # ./install-desktop.sh --uninstall to remove
```

Run it from the directory you unpacked into and keep the files there; if you
move them, run it again. It uses the same app ID as the older Go app's entry,
so it replaces that one.

Settings live in `~/.config/gawk/broadcast.json`, the same file the older Go
app used. Your relay, secrets, room and encoder cache carry over.

## Updating

When a newer release is out, a notice at the top of the main page says so.
The tarball copy then downloads it in the background while you are not
broadcasting, checks its signature against the release key built into the
app, and offers **Install and relaunch**. Clicking it replaces `gawk-broadcast-linux` in
place and restarts the app. The launcher entry keeps working because the
path does not change. This needs the directory you unpacked into to be
writable by you; if it isn't, download the new tarball by hand. The `.deb`
is never replaced by the app: install the newer `.deb` as above.

## When it doesn't work

- **`error while loading shared libraries: … cannot open shared object file`**:
  a package is missing. `ldd ./gawk-broadcast-linux | grep "not found"` names
  it.
- **"No working hardware H.264 encoder was found"**: none of
  `vulkanh264enc`, `nvh264enc` or `vah264enc` works on this GPU. Check that
  `gstreamer1.0-plugins-bad` is installed (`gst-inspect-1.0 vah264enc`), then
  use the browser broadcaster and tell us your GPU and driver.
- **"No screen-share portal found"**: install `xdg-desktop-portal` and your
  desktop's backend, then log out and back in.
- **Screen capture failed: … could not agree on a frame format**: the
  compositor and GStreamer's PipeWire plugin disagree. Update both to the same
  distro release.
- **The whose-audio list is empty**: apps appear only while they play sound.
  Start the game's audio, or pick **Whole system**.
- Anything else: the debug log next to the settings file
  (`~/.config/gawk/debug.log`) has the details, and **Copy diagnostics** in
  the app puts the rest on your clipboard.

The config keys, the encoder pin and the build-from-source steps are in the
[README](https://github.com/Tuhis/gawk/blob/main/gawk-broadcast-desktop/README.md).
