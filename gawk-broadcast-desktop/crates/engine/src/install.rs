//! The in-place install (R47, docs/48 D5, D6, D9): where a verified release
//! is staged, how it replaces the running binary, and when it must not.
//!
//! Plain `std::fs`, so every rule here is unit-tested on any host; the
//! shell decides *when* (never while live, docs/48 D4) and relaunches. The
//! verification that comes before any of this is [`crate::update::stage`].
//!
//! Everything happens in the directory the running binary is in, and
//! nowhere else: a staging directory beside it, then one rename per file
//! swapped. A binary in a directory the app may not write, a `.deb`
//! install among them, is never touched.

use crate::defaults::{self, Distribution};
use std::path::{Path, PathBuf};

/// The staging directory, beside the running binary. On the same
/// filesystem by construction, so the final rename is atomic (docs/48 D6).
pub const STAGING_DIR: &str = ".gawk-update";

/// Where the `.deb` puts the Linux binary, and the file dpkg lists it in
/// (docs/63 D2, docs/48 D9).
pub const DEB_BINARY: &str = "/usr/bin/gawk-broadcast-linux";
pub const DEB_LIST: &str = "/var/lib/dpkg/info/gawk-broadcast.list";

/// The binary's name inside the Linux release tarball.
const LINUX_MEMBER: &str = "gawk-broadcast-linux";

/// How a distribution's release asset becomes the running binary.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Layout {
    /// The asset IS the binary (docs/48 D5): rename the running EXE aside,
    /// rename the new one in, delete the old one at the next start.
    WindowsExe,
    /// The asset is the tarball; its one binary replaces the running one by
    /// a single rename (docs/48 D6).
    LinuxTarball,
}

impl Layout {
    /// This distribution's layout, or `None` when it has no in-place
    /// install here (macOS: docs/54 D17).
    pub fn of(dist: &Distribution) -> Option<Layout> {
        if dist.name == defaults::WINDOWS.name {
            Some(Layout::WindowsExe)
        } else if dist.name == defaults::LINUX.name {
            Some(Layout::LinuxTarball)
        } else {
            None
        }
    }
}

/// What this install can do.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Plan {
    /// Stage into `staging`, then swap.
    InPlace { staging: PathBuf },
    /// A `.deb` install: dpkg owns the binary (docs/48 D9).
    Deb,
    /// The directory cannot be written; the R45 notice stays as it is.
    NotWritable(String),
}

/// Decides the install for the binary at `exe`, which is the path read at
/// startup (docs/48 D6). `deb_list` is [`DEB_LIST`] outside the tests.
/// Probing writability creates the staging directory, which the next
/// start's [`cleanup`] removes if nothing is staged in it.
pub fn plan(exe: &Path, layout: Layout, deb_list: &Path) -> Plan {
    if layout == Layout::LinuxTarball && is_deb(exe, deb_list) {
        return Plan::Deb;
    }
    let Some(dir) = exe.parent() else {
        return Plan::NotWritable(format!("{} has no directory", exe.display()));
    };
    let staging = dir.join(STAGING_DIR);
    let probe = staging.join(".probe");
    let writable = std::fs::create_dir_all(&staging)
        .and_then(|()| std::fs::write(&probe, b""))
        .and_then(|()| std::fs::remove_file(&probe));
    match writable {
        Ok(()) => Plan::InPlace { staging },
        Err(e) => Plan::NotWritable(format!("{}: {e}", dir.display())),
    }
}

/// Whether `exe` is the `.deb`'s binary: at [`DEB_BINARY`], and listed by
/// dpkg as installed by package `gawk-broadcast`.
fn is_deb(exe: &Path, deb_list: &Path) -> bool {
    exe == Path::new(DEB_BINARY)
        && std::fs::read_to_string(deb_list).is_ok_and(|l| l.lines().any(|x| x == DEB_BINARY))
}

/// Turns a verified, staged asset into the file that will replace the
/// running binary: the asset itself for the EXE, the one binary unpacked
/// from it for the tarball. Runs off the UI thread, so the click only
/// renames.
pub fn prepare(layout: Layout, staged: &Path, staging: &Path) -> Result<PathBuf, String> {
    match layout {
        Layout::WindowsExe => Ok(staged.to_path_buf()),
        Layout::LinuxTarball => {
            let out = staging.join(LINUX_MEMBER);
            unpack_linux_binary(staged, &out)?;
            // The tarball's other files are not installed (docs/48 D6).
            let _ = std::fs::remove_file(staged);
            Ok(out)
        }
    }
}

#[cfg(target_os = "linux")]
fn unpack_linux_binary(tarball: &Path, out: &Path) -> Result<(), String> {
    use std::os::unix::fs::OpenOptionsExt;
    let file = std::fs::File::open(tarball).map_err(|e| format!("{}: {e}", tarball.display()))?;
    let mut archive = tar::Archive::new(flate2::read::GzDecoder::new(file));
    let entries = archive.entries().map_err(|e| format!("tarball: {e}"))?;
    for entry in entries {
        let mut entry = entry.map_err(|e| format!("tarball: {e}"))?;
        let path = entry.path().map_err(|e| format!("tarball: {e}"))?;
        // `gawk-broadcast-linux` or `./gawk-broadcast-linux`, nothing else:
        // no other name is written, so no path in the archive can reach
        // outside the staging directory.
        let is_binary = path
            .components()
            .filter(|c| !matches!(c, std::path::Component::CurDir))
            .map(|c| c.as_os_str().to_owned())
            .eq([std::ffi::OsString::from(LINUX_MEMBER)]);
        if !is_binary || !entry.header().entry_type().is_file() {
            continue;
        }
        let _ = std::fs::remove_file(out);
        let mut f = std::fs::OpenOptions::new()
            .write(true)
            .create_new(true)
            .mode(0o755)
            .open(out)
            .map_err(|e| format!("{}: {e}", out.display()))?;
        std::io::copy(&mut entry, &mut f).map_err(|e| format!("{}: {e}", out.display()))?;
        f.sync_all()
            .map_err(|e| format!("{}: {e}", out.display()))?;
        return Ok(());
    }
    Err(format!("the tarball has no {LINUX_MEMBER}"))
}

#[cfg(not(target_os = "linux"))]
fn unpack_linux_binary(_tarball: &Path, _out: &Path) -> Result<(), String> {
    Err("the Linux tarball is unpacked only on Linux".into())
}

/// Replaces the running binary at `exe` with `ready` (docs/48 D5, D6).
///
/// Linux: one `rename(2)` over the running binary. The process keeps its
/// inode, so there is no `ETXTBSY`, and the path starts the new build.
///
/// Windows: the running EXE is renamed aside to `<exe>.old` (Windows allows
/// renaming a mapped executable, not deleting it), the new one is renamed
/// in, and a failure there renames the old one back.
pub fn swap(layout: Layout, exe: &Path, ready: &Path) -> Result<(), String> {
    match layout {
        Layout::LinuxTarball => {
            std::fs::rename(ready, exe).map_err(|e| format!("{}: {e}", exe.display()))
        }
        Layout::WindowsExe => {
            let old = old_path(exe);
            let _ = std::fs::remove_file(&old);
            std::fs::rename(exe, &old).map_err(|e| format!("{}: {e}", exe.display()))?;
            if let Err(e) = std::fs::rename(ready, exe) {
                let _ = std::fs::rename(&old, exe);
                return Err(format!("{}: {e}", exe.display()));
            }
            Ok(())
        }
    }
}

/// `<exe>.old`, where the Windows swap parks the running EXE.
pub fn old_path(exe: &Path) -> PathBuf {
    let mut name = exe.file_name().unwrap_or_default().to_owned();
    name.push(".old");
    exe.with_file_name(name)
}

/// At startup, before anything is staged: removes what an earlier run left
/// beside the binary — the staging directory (an interrupted or failed
/// download, or a finished swap's leftovers) and a swapped-out `<exe>.old`.
/// Returns what it removed, for the log.
pub fn cleanup(exe: &Path) -> Vec<PathBuf> {
    let mut removed = Vec::new();
    if let Some(dir) = exe.parent() {
        let staging = dir.join(STAGING_DIR);
        if staging.is_dir() && std::fs::remove_dir_all(&staging).is_ok() {
            removed.push(staging);
        }
    }
    let old = old_path(exe);
    if old.is_file() && std::fs::remove_file(&old).is_ok() {
        removed.push(old);
    }
    removed
}

#[cfg(test)]
mod tests {
    use super::*;

    fn tmp() -> tempfile::TempDir {
        tempfile::tempdir().unwrap()
    }

    #[test]
    fn layouts_follow_the_distribution() {
        assert_eq!(Layout::of(&defaults::WINDOWS), Some(Layout::WindowsExe));
        assert_eq!(Layout::of(&defaults::LINUX), Some(Layout::LinuxTarball));
        assert_eq!(Layout::of(&defaults::MACOS), None, "docs/54 D17, not here");
    }

    #[test]
    fn a_writable_directory_stages_beside_the_binary() {
        let d = tmp();
        let exe = d.path().join("gawk-broadcast-linux");
        std::fs::write(&exe, b"old").unwrap();
        let got = plan(&exe, Layout::LinuxTarball, &d.path().join("no.list"));
        assert_eq!(
            got,
            Plan::InPlace {
                staging: d.path().join(STAGING_DIR)
            }
        );
        assert!(!d.path().join(STAGING_DIR).join(".probe").exists());
    }

    #[cfg(unix)]
    #[test]
    fn a_read_only_directory_is_not_written() {
        use std::os::unix::fs::PermissionsExt;
        let d = tmp();
        let exe = d.path().join("gawk-broadcast-linux");
        std::fs::write(&exe, b"old").unwrap();
        std::fs::set_permissions(d.path(), std::fs::Permissions::from_mode(0o555)).unwrap();
        // Root ignores the mode bits; the rule is still exercised elsewhere.
        let root = std::fs::write(d.path().join("x"), b"").is_ok();
        let got = plan(&exe, Layout::LinuxTarball, &d.path().join("no.list"));
        std::fs::set_permissions(d.path(), std::fs::Permissions::from_mode(0o755)).unwrap();
        if root {
            return;
        }
        assert!(matches!(got, Plan::NotWritable(_)), "{got:?}");
        let names: Vec<_> = std::fs::read_dir(d.path()).unwrap().collect();
        assert_eq!(names.len(), 1, "nothing but the binary: {names:?}");
    }

    #[test]
    fn a_deb_install_is_never_swapped() {
        let d = tmp();
        let list = d.path().join("gawk-broadcast.list");
        std::fs::write(&list, format!("/.\n/usr\n/usr/bin\n{DEB_BINARY}\n")).unwrap();
        assert_eq!(
            plan(Path::new(DEB_BINARY), Layout::LinuxTarball, &list),
            Plan::Deb
        );
        // The same path without dpkg's record is an ordinary, unwritable dir.
        let none = d.path().join("missing.list");
        assert_ne!(
            plan(Path::new(DEB_BINARY), Layout::LinuxTarball, &none),
            Plan::Deb
        );
        // A list that does not name the binary is not ours.
        std::fs::write(&list, "/usr/bin/something-else\n").unwrap();
        assert_ne!(
            plan(Path::new(DEB_BINARY), Layout::LinuxTarball, &list),
            Plan::Deb
        );
    }

    #[test]
    fn the_linux_swap_is_one_rename_and_leaves_share_alone() {
        let d = tmp();
        let exe = d.path().join("gawk-broadcast-linux");
        std::fs::write(&exe, b"old build").unwrap();
        std::fs::create_dir_all(d.path().join("share/applications")).unwrap();
        std::fs::write(d.path().join("share/applications/x.desktop"), b"entry").unwrap();
        let staging = d.path().join(STAGING_DIR);
        std::fs::create_dir_all(&staging).unwrap();
        let ready = staging.join("gawk-broadcast-linux");
        std::fs::write(&ready, b"new build").unwrap();

        swap(Layout::LinuxTarball, &exe, &ready).unwrap();
        assert_eq!(std::fs::read(&exe).unwrap(), b"new build");
        assert!(!ready.exists());
        assert_eq!(
            std::fs::read(d.path().join("share/applications/x.desktop")).unwrap(),
            b"entry"
        );
        assert!(!old_path(&exe).exists(), "Linux keeps no .old");
    }

    #[test]
    fn the_windows_swap_parks_the_old_exe_and_cleanup_removes_it() {
        let d = tmp();
        let exe = d.path().join("gawk-broadcast-windows-x86_64.exe");
        std::fs::write(&exe, b"old exe").unwrap();
        let staging = d.path().join(STAGING_DIR);
        std::fs::create_dir_all(&staging).unwrap();
        let ready = staging.join("gawk-broadcast-windows-x86_64.exe");
        std::fs::write(&ready, b"new exe").unwrap();

        swap(Layout::WindowsExe, &exe, &ready).unwrap();
        assert_eq!(std::fs::read(&exe).unwrap(), b"new exe");
        assert_eq!(std::fs::read(old_path(&exe)).unwrap(), b"old exe");
        assert_eq!(
            old_path(&exe).file_name().unwrap(),
            "gawk-broadcast-windows-x86_64.exe.old"
        );

        let removed = cleanup(&exe);
        assert!(removed.contains(&old_path(&exe)));
        assert!(removed.contains(&staging));
        assert!(!old_path(&exe).exists() && !staging.exists());
        assert_eq!(std::fs::read(&exe).unwrap(), b"new exe", "never the binary");
    }

    #[test]
    fn a_failed_windows_swap_puts_the_old_exe_back() {
        let d = tmp();
        let exe = d.path().join("gawk.exe");
        std::fs::write(&exe, b"old exe").unwrap();
        let missing = d.path().join(STAGING_DIR).join("gawk.exe");
        assert!(swap(Layout::WindowsExe, &exe, &missing).is_err());
        assert_eq!(std::fs::read(&exe).unwrap(), b"old exe");
        assert!(!old_path(&exe).exists());
    }

    #[test]
    fn cleanup_with_nothing_left_over_removes_nothing() {
        let d = tmp();
        let exe = d.path().join("gawk-broadcast-linux");
        std::fs::write(&exe, b"build").unwrap();
        assert!(cleanup(&exe).is_empty());
        assert!(exe.exists());
    }

    #[cfg(target_os = "linux")]
    fn tarball(path: &Path, members: &[(&str, &[u8])]) {
        let f = std::fs::File::create(path).unwrap();
        let gz = flate2::write::GzEncoder::new(f, flate2::Compression::fast());
        let mut b = tar::Builder::new(gz);
        for (name, data) in members {
            let mut h = tar::Header::new_gnu();
            h.set_size(data.len() as u64);
            h.set_mode(0o644);
            h.set_cksum();
            b.append_data(&mut h, name, *data).unwrap();
        }
        b.into_inner().unwrap().finish().unwrap();
    }

    #[cfg(target_os = "linux")]
    #[test]
    fn the_tarball_yields_only_its_binary_executable() {
        use std::os::unix::fs::PermissionsExt;
        let d = tmp();
        let staging = d.path().join(STAGING_DIR);
        std::fs::create_dir_all(&staging).unwrap();
        let tgz = staging.join("gawk-broadcast-linux-x86_64.tar.gz");
        tarball(
            &tgz,
            &[
                ("install-desktop.sh", b"#!/bin/sh"),
                ("share/applications/x.desktop", b"entry"),
                ("gawk-broadcast-linux", b"\x7fELF new"),
            ],
        );
        let ready = prepare(Layout::LinuxTarball, &tgz, &staging).unwrap();
        assert_eq!(ready, staging.join("gawk-broadcast-linux"));
        assert_eq!(std::fs::read(&ready).unwrap(), b"\x7fELF new");
        let mode = std::fs::metadata(&ready).unwrap().permissions().mode();
        assert_eq!(mode & 0o111, 0o111, "executable: {mode:o}");
        assert!(!tgz.exists(), "the tarball is not kept");
        assert!(!staging.join("share").exists() && !staging.join("install-desktop.sh").exists());
    }

    #[cfg(target_os = "linux")]
    #[test]
    fn a_tarball_without_the_binary_is_refused() {
        let d = tmp();
        let tgz = d.path().join("t.tar.gz");
        tarball(&tgz, &[("other/gawk-broadcast-linux", b"nested, not ours")]);
        assert!(prepare(Layout::LinuxTarball, &tgz, d.path()).is_err());
        assert!(!d.path().join("gawk-broadcast-linux").exists());
    }
}
