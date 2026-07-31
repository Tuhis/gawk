//! The debug log (docs/38 F-8): `debug.log` next to `broadcast.json`,
//! rotated on every launch. It exists because the shell is
//! `windows_subsystem = "windows"` — there is no console, so anything that
//! only reaches stderr (the encoder cascade's per-candidate rejection
//! trail, most importantly) is invisible on exactly the machine where it is
//! needed. Every start attempt logs enough runtime detail here to diagnose
//! a refusal from the file alone: adapter identity, MFT enumeration, each
//! trial rejection with its step-named error, and session lifecycle.
//!
//! Volume discipline: lifecycle events only — never per-frame, never
//! per-packet. The file must stay trivially attachable to a bug report.

use std::io::Write;
use std::path::{Path, PathBuf};
use std::sync::Mutex;

struct FileLogger {
    file: Mutex<std::fs::File>,
}

impl log::Log for FileLogger {
    fn enabled(&self, metadata: &log::Metadata) -> bool {
        // Our crates only: dependency internals (quinn, slint, …) would
        // swamp the file and leak nothing we can act on.
        metadata.target().starts_with("gawk")
    }

    fn log(&self, record: &log::Record) {
        if !self.enabled(record.metadata()) {
            return;
        }
        let line = format_line(
            &now_rfc3339(),
            record.level(),
            record.target(),
            &record.args().to_string(),
        );
        // Mirror to stderr for dev shells that do have a console.
        eprint!("{line}");
        if let Ok(mut f) = self.file.lock() {
            let _ = f.write_all(line.as_bytes());
            let _ = f.flush();
        }
    }

    fn flush(&self) {}
}

/// One log line: timestamp, level, module, message. Multi-line messages
/// stay one entry (continuation lines are indented, not re-stamped).
fn format_line(timestamp: &str, level: log::Level, target: &str, msg: &str) -> String {
    let msg = msg.replace('\n', "\n    ");
    format!("{timestamp} {level:5} [{target}] {msg}\n")
}

/// Installs the logger writing to `debug.log` in `dir` (the config
/// directory), rotating any previous log to `debug.log.old` so a report
/// always carries the failing launch, not an unbounded history. Returns the
/// log path, or `None` when the directory is unknown or the file cannot be
/// opened — the app runs on regardless; logging is never load-bearing.
pub fn init(dir: Option<&Path>) -> Option<PathBuf> {
    let dir = dir?;
    let path = rotate(dir);
    let file = std::fs::OpenOptions::new()
        .create(true)
        .append(true)
        .open(&path)
        .ok()?;
    log::set_boxed_logger(Box::new(FileLogger {
        file: Mutex::new(file),
    }))
    .ok()?;
    log::set_max_level(log::LevelFilter::Debug);
    Some(path)
}

/// Ensures `dir` exists and moves an existing `debug.log` aside.
fn rotate(dir: &Path) -> PathBuf {
    let _ = std::fs::create_dir_all(dir);
    let path = dir.join("debug.log");
    if path.exists() {
        let _ = std::fs::rename(&path, dir.join("debug.log.old"));
    }
    path
}

/// Appends the "where to look" pointer to a user-facing error text, so the
/// person seeing the error card knows a diagnosable record exists.
pub fn with_pointer(text: &str, log_path: Option<&Path>) -> String {
    match log_path {
        Some(p) => format!("{text}\n\nDetails were written to {}", p.display()),
        None => text.to_string(),
    }
}

/// RFC 3339 UTC now, std-only (log lines and the diagnostics dump).
pub fn now_rfc3339() -> String {
    let secs = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_secs())
        .unwrap_or(0);
    let days = secs / 86_400;
    let (h, m, s) = ((secs % 86_400) / 3600, (secs % 3600) / 60, secs % 60);
    // Civil-from-days (Howard Hinnant's algorithm).
    let z = days as i64 + 719_468;
    let era = z.div_euclid(146_097);
    let doe = z.rem_euclid(146_097);
    let yoe = (doe - doe / 1460 + doe / 36_524 - doe / 146_096) / 365;
    let y = yoe + era * 400;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let d = doy - (153 * mp + 2) / 5 + 1;
    let mo = if mp < 10 { mp + 3 } else { mp - 9 };
    let y = if mo <= 2 { y + 1 } else { y };
    format!("{y:04}-{mo:02}-{d:02}T{h:02}:{m:02}:{s:02}Z")
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn lines_carry_timestamp_level_target_and_message() {
        let line = format_line(
            "2026-07-31T00:00:00Z",
            log::Level::Warn,
            "gawk_encode::cascade",
            "encoder candidate rejected: X: B slice",
        );
        assert_eq!(
            line,
            "2026-07-31T00:00:00Z WARN  [gawk_encode::cascade] encoder candidate rejected: X: B slice\n"
        );
    }

    #[test]
    fn multi_line_messages_stay_one_entry() {
        let line = format_line("t", log::Level::Info, "gawk", "a\nb");
        assert_eq!(line, "t INFO  [gawk] a\n    b\n");
        assert_eq!(line.matches("t INFO").count(), 1);
    }

    #[test]
    fn rotation_moves_the_previous_launch_aside() {
        let dir = std::env::temp_dir().join(format!("gawk-debuglog-test-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        let path = rotate(&dir);
        std::fs::write(&path, "first launch").unwrap();
        let path2 = rotate(&dir);
        assert_eq!(path, path2);
        assert!(!path.exists(), "current log moved aside");
        assert_eq!(
            std::fs::read_to_string(dir.join("debug.log.old")).unwrap(),
            "first launch"
        );
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn pointer_is_appended_only_when_the_log_exists() {
        assert_eq!(with_pointer("boom", None), "boom");
        let with = with_pointer("boom", Some(Path::new("C:\\x\\debug.log")));
        assert!(with.starts_with("boom\n\nDetails were written to "));
        assert!(with.ends_with("debug.log"));
    }

    #[test]
    fn now_rfc3339_is_shaped_right() {
        let s = now_rfc3339();
        assert_eq!(s.len(), 20);
        assert!(s.ends_with('Z'));
        assert!(s.starts_with("20"));
        assert_eq!(&s[4..5], "-");
        assert_eq!(&s[10..11], "T");
    }
}
