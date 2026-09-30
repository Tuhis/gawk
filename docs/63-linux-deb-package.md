# R61 — A `.deb` for the Linux broadcaster (docs/63)

**Status**: designed and implemented 2026-09-30 (DB1–DB3 in one PR). The
owner's end-to-end pass (§4, manual) is open. Chunks **DB1–DB3**
(`DB` = Debian package). Touches `gawk-broadcast-desktop/tools/linux`, the
`linux` job of `broadcast-desktop.yml` and its release attach, the project
site's Linux card and the docs. No Rust change, no relay change, no wire
change. **Amends docs/58 OD7**, which shipped the Linux app as a tarball only
("No AppImage, Flatpak or `.deb`"), without recorded reasoning.

## 1. Purpose

On Wayland the window can't supply its own icon. winit 0.30 has no
`xdg-toplevel-icon-v1`, so Slint's `Window.icon` reaches Windows and X11 but
not Wayland. GNOME and KDE take the window's `app_id`
(`fi.ioio.gawk.broadcast`, set with `slint::set_xdg_app_id`) and look for a
desktop entry of the same name; that entry's `Icon=` is the icon. The
tarball ships the entry, but nothing installs it until the user runs
`install-desktop.sh`, so in practice the app shows the stock Wayland icon in
the taskbar, the task switcher and its notifications.

A `.deb` installs the entry and the hicolor icons to `/usr/share` as part of
installing the app, and dpkg's triggers refresh the icon cache and the
desktop database. It also turns INSTALL.md's apt line into `Depends`, so the
GStreamer plugins and the portal arrive with the app instead of on a second
read of the instructions.

Owner decision 2026-09-30: `.deb` over Flatpak and AppImage. AppImage does
not solve the problem (it installs nothing either). Flatpak does, but would
need the in-process PipeWire control connection (docs/58 OD3/OD4) and the
hardware encoders proven inside the sandbox first.

## 2. Decisions

| # | Decision | Why |
|---|---|---|
| D1 | **The `.deb` ships beside the tarball; it replaces nothing.** The release carries both `gawk-broadcast-linux-x86_64.tar.gz` and `gawk-broadcast_<version>_amd64.deb`, both covered by `SHA256SUMS`. The `gawk-broadcast-linux` manifest's `primary` stays the tarball; the `.deb` is listed in its `assets` like every other file on the release. | Fedora and Arch users still need the tarball. The in-app update check (docs/47 D5) pins `asset.name` to the tarball, and changing the primary would make every installed app treat the manifest as invalid, which it reads as "no update". |
| D2 | **Package `gawk-broadcast`, asset `gawk-broadcast_<version>_amd64.deb` (e.g. `gawk-broadcast_2.0.0_amd64.deb`), binary `/usr/bin/gawk-broadcast-linux`, version = `version.txt`, architecture `amd64`.** `build-deb.sh` names the file itself from `version.txt`. The site builds the name from the manifest's `version` and looks it up exactly. | The package name can't be `gawk`: Debian and Ubuntu already ship GNU awk under that name. The file name follows Debian's `<package>_<version>_<arch>.deb` (what `dpkg-name` produces), not the fixed per-platform names of docs/47 D5 (owner request 2026-09-30). A downloaded package then says which version it is, and several can sit side by side in `~/Downloads`. That is safe here because nothing keys on a fixed `.deb` name: the update check keys on the tarball (D1), and the site derives the exact name from the validated version rather than matching a pattern. `gh release download -p 'gawk-broadcast_*_amd64.deb'` still works. The binary name matches the tarball's, and `Exec=` in the entry is already the bare name. |
| D3 | **Built by `tools/linux/build-deb.sh` with plain `dpkg-deb`.** No cargo-deb, debhelper or nfpm. The script reuses `assemble-share.sh` for the `share/` tree, so the tarball and the package can't disagree on the layout. | The package is ten files. dpkg-dev is already in the build container (via `build-essential`), so the script adds no tool to install or pin. This follows docs/53 D2's rule against build tools the runner image doesn't assert. |
| D4 | **`Depends` = `dpkg-shlibdeps` on the binary + two fixed lists of what the app loads at runtime, which no linker sees.** shlibdeps maps the binary's `NEEDED` entries to the build container's packages with versioned floors (`libc6 (>= 2.39)`, …). The windowing libraries Slint's winit backend and GL renderer `dlopen()` go in `DLOPEN_DEPENDS`: xkbcommon (+x11), wayland-client/-egl, EGL, GL, X11, X11-xcb, xcb, Xcursor, Xi, Xrender. The runtime list covers GStreamer and the portal: `gstreamer1.0-pipewire`, `-plugins-base`, `-plugins-good`, `-plugins-bad`, `xdg-desktop-portal`. | shlibdeps gets the t64 names (`libglib2.0-0t64`, …) and the version floors right for the build's own release, so the package states the same floor as docs/58 OD14 (Ubuntu 24.04, Debian 13) with no hand-kept table. The dlopen list was found by testing, not by reading `NEEDED`: the first draft missed all of it, and `libxrender1` hid behind the GStreamer plugins' own dependencies until D8's direct-dependency check. The runtime list is INSTALL.md's apt line. The desktop-specific portal backend is left out: `xdg-desktop-portal` recommends one for the running desktop. |
| D5 | **Layout**: `/usr/bin/gawk-broadcast-linux`; `/usr/share/applications/fi.ioio.gawk.broadcast.desktop`; `/usr/share/icons/hicolor/{16…256,scalable}/apps/fi.ioio.gawk.broadcast.*`; `/usr/share/doc/gawk-broadcast/` with `copyright` (DEP-5, Apache-2.0 via `common-licenses`), `changelog.gz` (one entry pointing at the release page), `THIRD-PARTY-NOTICES-linux.md` and `BUILD-INFO.txt`. **No `install-desktop.sh`.** | The installed entry is the one GNOME and KDE resolve from the `app_id`, and the notifier already looks for the icon in `/usr/share` (docs/53 D5), so the whole R44 chain works without an app change. The script would only write a second, per-user copy of the entry. |
| D6 | **No maintainer scripts.** | `hicolor-icon-theme` and `desktop-file-utils` own dpkg file triggers on those directories, so the icon cache and the desktop database are refreshed with no `postinst` of ours. A package with no scripts runs no code as root beyond dpkg's own. |
| D7 | **Unsigned, and no apt repository.** Integrity comes from `SHA256SUMS`, as for the EXE (docs/38 D17). Users update the way they do today: the in-app notice (R45) → the release page → `sudo apt install ./gawk-broadcast_<version>_amd64.deb`, which upgrades in place. | A repository needs a signing key held by CI, hosting and a key-distribution story. That is worth doing only if the `.deb` is used. Nothing here prevents adding one later: the package name and version scheme are what a repository would serve. R47 keeps this path for `.deb` installs: the app never swaps a dpkg-owned binary, and its notice names the newer `.deb` and the apt command (docs/48 D9, 2026-09-30). |
| D8 | **CI builds it in the `linux` job and install-tests it in fresh containers.** `build-deb.sh` runs after the tarball checks. A new `linux-deb` job, a matrix over `ubuntu:24.04` and `debian:trixie`, runs `tools/linux/test-deb.sh`: `lintian --fail-on error`, `apt install ./….deb` from the stock archive, `ldd` finds every linked library, every versioned soname the binary names as a string (its `dlopen()` targets) resolves **and comes from a package the `.deb` depends on directly**, the plugin files for every element the app creates are present, the entry validates with its icon installed, and a purge leaves nothing. `attach-release` attaches the `.deb` only when `linux-deb` passed. It is a soft dependency, like the tarball: never holding the Windows release. | The build container has every `-dev` package installed, so an install there would satisfy any `Depends` the package forgot. Only a fresh container of each supported release proves the stated dependencies are enough. Debian 13 is the other named floor in INSTALL.md. |
| D9 | **A leftover per-user entry wins, so the docs tell tarball users to remove it first.** `$XDG_DATA_HOME` (`~/.local/share`) comes before `/usr/share` in the lookup, so an earlier `install-desktop.sh` entry keeps launching the old tarball binary. INSTALL.md, the README and the site say to run `./install-desktop.sh --uninstall` before installing the package. The package never touches home directories. | A root-run package editing per-user files is wrong in principle, and dpkg doesn't know which users ran the script. The failure is visible (the launcher starts the old version) and the fix is one command the user already has. |
| D10 | **Only the release commit's build carries the plain version; every other build is visibly not a release** (owner request 2026-09-30). `build-deb.sh` appends `DEB_VERSION_SUFFIX` to both the file name and the package's `Version`. The `linux` job sets it to `+pr<N>.g<sha7>` on a pull request, and to `+dev.g<sha7>` on a push or dispatch whose commit did not change `version.txt`. A push or `attach_to` backfill of the commit that did (the release-please merge) builds `gawk-broadcast_<version>_amd64.deb`. `attach-release` refuses a suffixed `.deb` even when the tag gate passes, and attaches the tarball without it. | A PR artifact named `gawk-broadcast_2.0.0_amd64.deb` would be indistinguishable from the release, and `apt` would report it as 2.0.0. The suffix makes the build name itself. In dpkg's ordering `2.0.0 < 2.0.0+pr406.g… < 2.0.1`, so a tester who installed a PR build is upgraded by the next real release rather than stuck above it. The rule is decided from the commit (did it bump `version.txt`?) rather than from the tag, because the tag is created by release-please.yml on the same push and may not exist yet when the build runs. A PR never counts, since the release PR changes `version.txt` too. The attach-time refusal is a second check for the case where the two rules disagree. |

### Rejected

- **Flatpak.** It solves the icon, but the sandbox has to allow the
  in-process PipeWire graph work behind per-app audio (docs/58 OD3/OD4), and
  hardware encode through the runtime's GStreamer and GL extensions is
  untested. Worth a spike, not a default.
- **AppImage.** An AppImage installs no desktop entry, so on Wayland it has
  exactly the tarball's problem unless the user runs a separate integration
  tool. It would also have to bundle GStreamer and its hardware plugins.
- **cargo-deb.** It is convenient for crates, but it is a tool the build
  container would have to install and pin, and it works out `Depends` with
  the same `dpkg-shlibdeps` call D4 already makes.
- **A `postinst` that removes `~/.local/share/applications/fi.ioio.gawk.broadcast.desktop`**
  (D9). Root editing user files.
- **Making the `.deb` the manifest's `primary`.** It breaks every installed
  app's update check (D1).

## 3. Where it plugs in

| Place | Change |
|---|---|
| `gawk-broadcast-desktop/tools/linux/build-deb.sh` | new (D2–D6) |
| `gawk-broadcast-desktop/tools/linux/test-deb.sh` | new: the install test (D8) |
| `.github/workflows/broadcast-desktop.yml` | `linux`: build the `.deb`, upload it with the tarball; new `linux-deb` matrix job; `attach-release`: attach the `.deb` when `linux-deb` passed |
| `site/index.html`, `site/site.js` | a second button on the Linux card, filled from the manifest's `assets` entry for the `.deb` |
| INSTALL.md (tarball), desktop README §Linux | the `.deb` path first, the tarball for other distros, D9's uninstall step |
| docs/58 OD7 | dated amendment pointing here |
| `docs/gotchas.md`, `docs/README.md`, `ROADMAP.md` | gotcha (D9), index row, R61 row + entry |

## 4. Chunks and acceptance criteria

| Chunk | Scope | Verified by |
|---|---|---|
| **DB1** | `build-deb.sh`, `test-deb.sh` (D2–D6, D9) | Locally, in the `ubuntu:24.04` dev container: `build-deb.sh` on a release binary produces a package whose `dpkg-deb -f` shows `Package: gawk-broadcast`, the workspace version, `Architecture: amd64` and a `Depends` that starts with `libc6 (>= 2.39)`; `dpkg-deb -c` lists the binary, the entry, all eight icon files and the doc files, and no `install-desktop.sh`. `test-deb.sh` passes in fresh `ubuntu:24.04` and `debian:trixie` containers, with `no-manual-page` as the only lintian warning. **Negative checks**: the same package repacked without `DLOPEN_DEPENDS` fails, naming each soname and the package it came from, and so does one without the runtime list either. Done 2026-09-30. |
| **DB2** | CI (D8, D10): `linux` builds and uploads the `.deb`, suffixed unless it is the release commit; `linux-deb` matrix; `attach-release` | The `linux-deb` job goes green on both images in the PR. The artifact holds both files, and the `.deb` in it is named `gawk-broadcast_<version>+pr<N>.g<sha7>_amd64.deb`. `attach-release` copies the `.deb` into `release-assets` only when `needs.linux-deb.result == 'success'`, so it's covered by `SHA256SUMS`, and warns rather than failing otherwise. |
| **DB3** | Site card, INSTALL.md, desktop README, docs/58 amendment, gotcha, index, ROADMAP | Review. The site's `.deb` button stays hidden until the manifest lists `gawk-broadcast_<its version>_amd64.deb` whose URL passes the same prefix check as the primary, so a release without one never shows a dead button. |

Success criterion, end to end — **the manual pass, owner-run**, on the first
release that carries the package:

1. On a KDE Plasma (Wayland) machine that has never run `install-desktop.sh`
   (or after `./install-desktop.sh --uninstall`):
   `sudo apt install ./gawk-broadcast_<version>_amd64.deb`.
2. The app is in the launcher with the purple bolt; started from there, the
   taskbar entry and the task switcher show the bolt too, and a notification
   from the app carries it.
3. `sudo apt remove gawk-broadcast` removes the launcher entry; the settings
   in `~/.config/gawk/` stay.

## 5. Security considerations

The package runs no maintainer scripts (D6), so installing it executes
nothing of ours as root. It writes only under `/usr`. It is unsigned (D7):
`SHA256SUMS` on the release is the integrity check, as for the Windows EXE
and the tarball. `apt install ./file.deb` installs a local file and trusts no
new repository or key. The CI install test runs in throwaway containers and
holds no secrets.

## 6. Gotchas surfaced (mirrored in `docs/gotchas.md`)

- **A per-user desktop entry shadows the package's.** `~/.local/share`
  comes before `/usr/share`, so an old `install-desktop.sh` entry keeps
  launching the tarball binary after the `.deb` is installed (D9).
- **Only a fresh container proves `Depends`.** The build container's `-dev`
  packages pull in every runtime library, so installing the package there
  passes whatever `Depends` says (D8).
- **`dpkg-shlibdeps` can't see `dlopen()`.** winit and glutin load
  xkbcommon, Wayland, EGL/GL and the X11 libraries by soname at runtime, so
  none of them is a `NEEDED` entry, and "it resolves after install" isn't
  proof either: the GStreamer plugins pull most of them in transitively. The
  test requires each one to come from a direct dependency (D4, D8).
