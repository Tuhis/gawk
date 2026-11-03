//! Desktop notifications over `org.freedesktop.Notifications` (docs/58 D9):
//! the urgency hint (critical for start failure, broadcast error, ended
//! unexpectedly and the first viewer; normal otherwise — docs/38 D12's
//! mapping) and `replaces_id`, so a new notice replaces the previous one
//! instead of stacking. Every notification is also a debug-log line. With no
//! session bus they are discarded silently.
//!
//! On zbus, driven from a small tokio runtime of its own (zbus itself runs
//! the connection on zruntime, never its `tokio` feature): the shell's notify hook is synchronous and on the GUI thread,
//! and a slow notification daemon must never stall the window.

use std::collections::HashMap;
use std::sync::OnceLock;
use std::sync::atomic::{AtomicU32, Ordering};
use zbus::Variant;

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
    HashMap<&'static str, Variant<'static>>,
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
    hints.insert("urgency", Variant::U8(if critical { 2 } else { 1 }));
    hints.insert(
        "desktop-entry",
        Variant::from(crate::platform::APP_ID.to_owned()),
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

/// Sends one notification over `conn` and returns the daemon's id for it.
async fn show(
    conn: &zbus::Connection,
    summary: &str,
    body: &str,
    critical: bool,
    replaces: u32,
) -> zbus::Result<u32> {
    let args = notify_args(summary, body, critical, replaces, icon());
    conn.call_method(
        Some("org.freedesktop.Notifications"),
        "/org/freedesktop/Notifications",
        Some("org.freedesktop.Notifications"),
        "Notify",
        &args,
    )
    .await?
    .body()
    .deserialize::<u32>()
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
        match show(
            &conn,
            &summary,
            &body,
            critical,
            LAST_ID.load(Ordering::Acquire),
        )
        .await
        {
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
        assert_eq!(hints["urgency"], Variant::U8(2));
        assert_eq!(timeout, -1);
        let (.., hints, _) = notify_args("Live", "", false, 0, "x");
        assert_eq!(hints["urgency"], Variant::U8(1));
    }

    /// A stand-in notification daemon: answers `Notify` with a fixed id and
    /// passes on what it was sent.
    struct Daemon {
        tx: std::sync::mpsc::Sender<(u32, String, u8)>,
    }

    #[zbus::interface(name = "org.freedesktop.Notifications")]
    impl Daemon {
        #[allow(clippy::too_many_arguments)]
        fn notify(
            &self,
            _app: String,
            replaces: u32,
            _icon: String,
            summary: String,
            _body: String,
            _actions: Vec<String>,
            hints: HashMap<String, zbus::OwnedVariant>,
            _timeout: i32,
        ) -> u32 {
            let urgency = hints
                .get("urgency")
                .and_then(|v| u8::try_from(v).ok())
                .unwrap_or(0);
            let _ = self.tx.send((replaces, summary, urgency));
            42
        }
    }

    // zbus 6 runs every connection on its own runtime (zruntime), never
    // tokio's (docs/gotchas.md): a notification sent from this module's tokio
    // runtime must still reach the daemon and bring its id back.
    #[test]
    fn a_notification_from_the_notify_runtime_reaches_the_daemon() {
        let Some(bus) = crate::instance::tests::bus("notify") else {
            return;
        };
        let (tx, rx) = std::sync::mpsc::channel();
        let _daemon = zbus::block_on(
            zbus::connection::Builder::address(bus.address.as_str())
                .serve_at("/org/freedesktop/Notifications", Daemon { tx })
                .name("org.freedesktop.Notifications")
                .build(),
        )
        .expect("the stand-in daemon");

        let address = bus.address.clone();
        let (done_tx, done_rx) = std::sync::mpsc::channel();
        runtime().spawn(async move {
            let conn = zbus::connection::Builder::address(address.as_str())
                .build()
                .await;
            let shown = match conn {
                Ok(conn) => show(&conn, "Broadcast ended", "unexpectedly", true, 7).await,
                Err(e) => Err(e),
            };
            let _ = done_tx.send(shown);
        });
        let id = done_rx
            .recv_timeout(std::time::Duration::from_secs(5))
            .expect("the call finished")
            .expect("the daemon answered");
        assert_eq!(id, 42);
        assert_eq!(
            rx.recv_timeout(std::time::Duration::from_secs(5)).unwrap(),
            (7, "Broadcast ended".to_owned(), 2)
        );
    }
}
