//! The `gawk://` handler registration (R66 OD4, docs/68 D9): per user under
//! `HKCU\Software\Classes\gawk`, on every launch of the primary, so a moved
//! EXE repairs its own key. It never takes the scheme from another program
//! and never unregisters.
//!
//! The decision is a pure function of what both hives hold, unit-tested on
//! any host; only the registry I/O is Windows code.

use std::path::Path;

/// The marker value that says the key is ours.
pub const OWNER: &str = "gawk-broadcast-desktop";

/// What one hive holds at `Software\Classes\gawk`.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Existing {
    Absent,
    Present {
        /// The `GawkOwner` value, if any.
        owner: Option<String>,
        /// `shell\open\command`'s default value, if any.
        command: Option<String>,
    },
}

/// What to do with the user's key.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Action {
    /// Nothing anywhere: write ours.
    Write,
    /// Ours, pointing at this EXE already.
    Nothing,
    /// Ours, pointing at an EXE that moved: rewrite the command.
    Rewrite,
    /// Another program's key in HKCU.
    ForeignUser,
    /// Another program registered the scheme machine-wide; an HKCU key
    /// would take it from that program for this user.
    ForeignMachine,
}

/// `shell\open\command`'s value for `exe`: `"<exe>" "%1"` (D7).
pub fn command(exe: &str) -> String {
    format!("\"{exe}\" \"%1\"")
}

/// `DefaultIcon`'s value for `exe`.
pub fn icon(exe: &str) -> String {
    format!("\"{exe}\",0")
}

/// D9's decision. `HKCR` merges the hives with a user key overriding a
/// machine one, which is why both are read.
pub fn decide(hkcu: &Existing, hklm: &Existing, exe: &str) -> Action {
    match hkcu {
        Existing::Present { owner, command: c } if owner.as_deref() == Some(OWNER) => {
            if c.as_deref() == Some(command(exe).as_str()) {
                Action::Nothing
            } else {
                Action::Rewrite
            }
        }
        Existing::Present { .. } => Action::ForeignUser,
        Existing::Absent => match hklm {
            Existing::Present { owner, .. } if owner.as_deref() != Some(OWNER) => {
                Action::ForeignMachine
            }
            _ => Action::Write,
        },
    }
}

/// True for a launch that must not register (D9): from under `%TEMP%`,
/// where Explorer extracts a "run from inside the zip", or from the update
/// staging directory. Compared case-insensitively, as Windows paths are.
pub fn transient(exe: &Path, temp: Option<&Path>) -> bool {
    let lower = |p: &Path| p.to_string_lossy().to_lowercase().replace('/', "\\");
    let exe = lower(exe);
    if let Some(temp) = temp {
        let temp = lower(temp);
        let temp = temp.trim_end_matches('\\');
        if !temp.is_empty() && exe.starts_with(&format!("{temp}\\")) {
            return true;
        }
    }
    exe.split('\\')
        .any(|c| c == gawk_engine::install::STAGING_DIR)
}

/// Registers this EXE as the `gawk://` handler when D9 says to, and logs
/// what it did. Never fails the launch.
#[cfg(windows)]
pub fn ensure() {
    let exe = match std::env::current_exe() {
        Ok(p) => p,
        Err(e) => {
            log::warn!("gawk:// links: no path to this EXE ({e}); not registered");
            return;
        }
    };
    let temp = std::env::var_os("TEMP").map(std::path::PathBuf::from);
    if transient(&exe, temp.as_deref()) {
        log::info!("gawk:// links: running from a temporary path; not registered");
        return;
    }
    let exe = exe.to_string_lossy().into_owned();
    let hkcu = win::read(win::Hive::User);
    let hklm = win::read(win::Hive::Machine);
    match decide(&hkcu, &hklm, &exe) {
        Action::Nothing => log::info!("gawk:// links: registered to this EXE"),
        a @ (Action::Write | Action::Rewrite) => match win::write(&exe) {
            Ok(()) => log::info!(
                "gawk:// links: {}",
                if a == Action::Write {
                    "registered"
                } else {
                    "registration moved to this EXE"
                }
            ),
            Err(e) => log::warn!("gawk:// links: could not register: {e}"),
        },
        Action::ForeignUser => {
            log::info!("gawk:// links: another program owns the scheme for this user; left alone")
        }
        Action::ForeignMachine => {
            log::info!("gawk:// links: another program owns the scheme machine-wide; left alone")
        }
    }
}

#[cfg(windows)]
mod win {
    use super::{Existing, OWNER, command, icon};
    use windows::Win32::Foundation::{ERROR_SUCCESS, WIN32_ERROR};
    use windows::Win32::System::Registry::{
        HKEY, HKEY_CURRENT_USER, HKEY_LOCAL_MACHINE, KEY_READ, KEY_WRITE, REG_OPTION_NON_VOLATILE,
        REG_SZ, RRF_RT_REG_SZ, RegCloseKey, RegCreateKeyExW, RegGetValueW, RegOpenKeyExW,
        RegSetValueExW,
    };
    use windows::core::{HSTRING, PCWSTR, w};

    const KEY: PCWSTR = w!("Software\\Classes\\gawk");

    pub enum Hive {
        User,
        Machine,
    }

    fn check(e: WIN32_ERROR) -> windows::core::Result<()> {
        if e == ERROR_SUCCESS {
            Ok(())
        } else {
            Err(e.into())
        }
    }

    /// A REG_SZ value under `KEY\sub`, `None` when absent or unreadable.
    fn string(root: HKEY, sub: &str, value: PCWSTR) -> Option<String> {
        let path = HSTRING::from(if sub.is_empty() {
            "Software\\Classes\\gawk".to_owned()
        } else {
            format!("Software\\Classes\\gawk\\{sub}")
        });
        let mut buf = [0u16; 2048];
        let mut size = (buf.len() * 2) as u32;
        // SAFETY: `buf` is writable for `size` bytes; the call writes a
        // NUL-terminated string and the byte count it wrote.
        let e = unsafe {
            RegGetValueW(
                root,
                &path,
                value,
                RRF_RT_REG_SZ,
                None,
                Some(buf.as_mut_ptr().cast()),
                Some(&mut size),
            )
        };
        if e != ERROR_SUCCESS {
            return None;
        }
        let len = (size as usize / 2).saturating_sub(1).min(buf.len());
        Some(String::from_utf16_lossy(&buf[..len]))
    }

    pub fn read(hive: Hive) -> Existing {
        let root = match hive {
            Hive::User => HKEY_CURRENT_USER,
            Hive::Machine => HKEY_LOCAL_MACHINE,
        };
        let mut key = HKEY::default();
        // SAFETY: `key` receives the opened handle, closed below.
        let opened = unsafe { RegOpenKeyExW(root, KEY, None, KEY_READ, &mut key) };
        if opened != ERROR_SUCCESS {
            return Existing::Absent;
        }
        // SAFETY: `key` was opened above.
        unsafe {
            let _ = RegCloseKey(key);
        }
        Existing::Present {
            owner: string(root, "", w!("GawkOwner")),
            command: string(root, "shell\\open\\command", PCWSTR::null()),
        }
    }

    /// Sets `HKCU\Software\Classes\gawk\<sub>`'s `value` (null = default).
    fn set(sub: &str, value: PCWSTR, data: &str) -> windows::core::Result<()> {
        let path = HSTRING::from(if sub.is_empty() {
            "Software\\Classes\\gawk".to_owned()
        } else {
            format!("Software\\Classes\\gawk\\{sub}")
        });
        let mut key = HKEY::default();
        // SAFETY: `key` receives the created/opened handle, closed below.
        check(unsafe {
            RegCreateKeyExW(
                HKEY_CURRENT_USER,
                &path,
                None,
                PCWSTR::null(),
                REG_OPTION_NON_VOLATILE,
                KEY_WRITE,
                None,
                &mut key,
                None,
            )
        })?;
        let wide: Vec<u16> = data.encode_utf16().chain(std::iter::once(0)).collect();
        // SAFETY: REG_SZ data is the NUL-terminated UTF-16 string as bytes.
        let bytes =
            unsafe { std::slice::from_raw_parts(wide.as_ptr().cast::<u8>(), wide.len() * 2) };
        // SAFETY: `key` is open for writing.
        let r = check(unsafe { RegSetValueExW(key, value, None, REG_SZ, Some(bytes)) });
        // SAFETY: as above.
        unsafe {
            let _ = RegCloseKey(key);
        }
        r
    }

    /// docs/68 D9's key, exactly. The command goes last, so a partial
    /// write never leaves a key that launches anything.
    pub fn write(exe: &str) -> windows::core::Result<()> {
        set("", PCWSTR::null(), "URL:gawk")?;
        set("", w!("URL Protocol"), "")?;
        set("", w!("GawkOwner"), OWNER)?;
        set("DefaultIcon", PCWSTR::null(), &icon(exe))?;
        set("shell\\open\\command", PCWSTR::null(), &command(exe))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::path::PathBuf;

    const EXE: &str = r"C:\Tools\gawk-broadcast.exe";

    fn ours(cmd: &str) -> Existing {
        Existing::Present {
            owner: Some(OWNER.into()),
            command: Some(cmd.into()),
        }
    }

    fn foreign() -> Existing {
        Existing::Present {
            owner: None,
            command: Some(r#""C:\Other\other.exe" "%1""#.into()),
        }
    }

    #[test]
    fn the_five_cases() {
        use Existing::Absent;
        // Nothing in either hive.
        assert_eq!(decide(&Absent, &Absent, EXE), Action::Write);
        // Ours, same path.
        assert_eq!(decide(&ours(&command(EXE)), &Absent, EXE), Action::Nothing);
        // Ours, moved.
        let old = command(r"D:\Downloads\gawk-broadcast.exe");
        assert_eq!(decide(&ours(&old), &Absent, EXE), Action::Rewrite);
        // Foreign in HKCU, whatever HKLM holds.
        assert_eq!(decide(&foreign(), &Absent, EXE), Action::ForeignUser);
        assert_eq!(decide(&foreign(), &foreign(), EXE), Action::ForeignUser);
        // Foreign in HKLM only.
        assert_eq!(decide(&Absent, &foreign(), EXE), Action::ForeignMachine);
    }

    #[test]
    fn ours_without_a_command_is_rewritten_and_a_bare_key_is_foreign() {
        let half = Existing::Present {
            owner: Some(OWNER.into()),
            command: None,
        };
        assert_eq!(decide(&half, &Existing::Absent, EXE), Action::Rewrite);
        let bare = Existing::Present {
            owner: None,
            command: None,
        };
        assert_eq!(decide(&bare, &Existing::Absent, EXE), Action::ForeignUser);
        // Ours machine-wide (an admin copied it) does not block the user key.
        assert_eq!(
            decide(&Existing::Absent, &ours(&command(EXE)), EXE),
            Action::Write
        );
    }

    #[test]
    fn the_values_quote_the_path() {
        assert_eq!(command(EXE), r#""C:\Tools\gawk-broadcast.exe" "%1""#);
        assert_eq!(icon(EXE), r#""C:\Tools\gawk-broadcast.exe",0"#);
    }

    #[test]
    fn temporary_paths_never_register() {
        let temp = PathBuf::from(r"C:\Users\u\AppData\Local\Temp");
        let zip = PathBuf::from(r"c:\users\U\appdata\local\temp\Temp1_gawk.zip\gawk-broadcast.exe");
        assert!(transient(&zip, Some(&temp)));
        let staged = PathBuf::from(r"C:\Tools\.gawk-update\gawk-broadcast.exe");
        assert!(transient(&staged, Some(&temp)));
        assert!(!transient(&PathBuf::from(EXE), Some(&temp)));
        assert!(!transient(&PathBuf::from(EXE), None));
        // A sibling that merely starts with the same letters is not inside.
        let sibling = PathBuf::from(r"C:\Users\u\AppData\Local\Temporary\gawk.exe");
        assert!(!transient(&sibling, Some(&temp)));
    }
}
