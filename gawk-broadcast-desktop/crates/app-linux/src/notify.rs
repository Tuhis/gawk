//! Desktop notifications over `org.freedesktop.Notifications` (docs/58 D9):
//! the urgency hint (critical for start failure, broadcast error, ended
//! unexpectedly and the first viewer; normal otherwise — docs/38 D12's
//! mapping) and `replaces_id`, so a new notice replaces the previous one
//! instead of stacking. Every notification is also a debug-log line. With no
//! session bus they are discarded silently.
//!
//! On zbus (already in the graph through ashpd), driven from a small runtime
//! of its own: the shell's notify hook is synchronous and on the GUI thread,
//! and a slow notification daemon must never stall the window.

use std::collections::HashMap;
use std::sync::OnceLock;
use std::sync::atomic::{AtomicU32, Ordering};
use zbus::zvariant::Value;

static LAST_ID: AtomicU32 = AtomicU32::new(0);

/// The icon: the installed app icon (R44 `install-desktop.sh`, docs/53), else
/// a stock one every icon theme carries.
fn icon() -> &'static str {
    static ICON: OnceLock<&'static str> = OnceLock::new();
    ICON.get_or_init(|| {
        let mut dirs: Vec<std::path::PathBuf> = Vec::new();
        if let Some(d) = std::env::var_os("XDG_DATA_HOME") {
            dirs.push(d.into());
        } else if let Some(h) = std::env::var_os("HOME") {
            dirs.push(std::path::PathBuf::from(h).join(".local/share"));
        }
        let data_dirs =
            std::env::var("XDG_DATA_DIRS").unwrap_or_else(|_| "/usr/local/share:/usr/share".into());
        dirs.extend(data_dirs.split(':').map(std::path::PathBuf::from));
        let installed = dirs.iter().any(|d| {
            d.join("icons/hicolor/scalable/apps/fi.ioio.gawk.broadcast.svg")
                .exists()
                || d.join("icons/hicolor/256x256/apps/fi.ioio.gawk.broadcast.png")
                    .exists()
        });
        if installed {
            crate::platform::APP_ID
        } else {
            "video-display"
        }
    })
}

/// The `Notify` call's arguments, in the spec's order: app name, replaces
/// id, icon, summary, body, actions, hints, timeout.
pub type NotifyArgs<'a> = (
    &'static str,
    u32,
    &'a str,
    &'a str,
    &'a str,
    Vec<&'static str>,
    HashMap<&'static str, Value<'static>>,
    i32,
);

/// Builds the call (pure, so the message shape is a unit test).
pub fn notify_args<'a>(
    summary: &'a str,
    body: &'a str,
    critical: bool,
    replaces: u32,
    icon: &'a str,
) -> NotifyArgs<'a> {
    let mut hints = HashMap::new();
    // 1 = normal, 2 = critical (the spec's byte).
    hints.insert("urgency", Value::U8(if critical { 2 } else { 1 }));
    hints.insert(
        "desktop-entry",
        Value::from(crate::platform::APP_ID.to_owned()),
    );
    (
        "gawk broadcast",
        replaces,
        icon,
        summary,
        body,
        Vec::new(),
        hints,
        -1,
    )
}

fn runtime() -> &'static tokio::runtime::Runtime {
    static RT: OnceLock<tokio::runtime::Runtime> = OnceLock::new();
    RT.get_or_init(|| {
        tokio::runtime::Builder::new_multi_thread()
            .worker_threads(1)
            .thread_name("notify")
            .enable_all()
            .build()
            .expect("the notification runtime")
    })
}

/// The shell's notify hook.
pub fn notify(summary: &str, body: &str, critical: bool) {
    if critical {
        log::warn!("notification: {summary}: {body}");
    } else {
        log::info!("notification: {summary}: {body}");
    }
    let (summary, body) = (summary.to_owned(), body.to_owned());
    runtime().spawn(async move {
        let Ok(conn) = zbus::Connection::session().await else {
            return; // no session bus: silently discarded
        };
        let args = notify_args(
            &summary,
            &body,
            critical,
            LAST_ID.load(Ordering::Acquire),
            icon(),
        );
        let reply = conn
            .call_method(
                Some("org.freedesktop.Notifications"),
                "/org/freedesktop/Notifications",
                Some("org.freedesktop.Notifications"),
                "Notify",
                &args,
            )
            .await;
        match reply.and_then(|m| m.body().deserialize::<u32>()) {
            Ok(id) => LAST_ID.store(id, Ordering::Release),
            Err(e) => log::debug!("notification not shown: {e}"),
        }
    });
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn urgency_and_replace_previous_are_in_the_message() {
        let (app, replaces, icon, summary, body, actions, hints, timeout) =
            notify_args("Broadcast ended", "unexpectedly", true, 7, "video-display");
        assert_eq!(app, "gawk broadcast");
        assert_eq!(replaces, 7);
        assert_eq!(
            (icon, summary, body),
            ("video-display", "Broadcast ended", "unexpectedly")
        );
        assert!(actions.is_empty());
        assert_eq!(hints["urgency"], Value::U8(2));
        assert_eq!(timeout, -1);
        let (.., hints, _) = notify_args("Live", "", false, 0, "x");
        assert_eq!(hints["urgency"], Value::U8(1));
    }
}
