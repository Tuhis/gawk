//! The ScreenCast portal (docs/58 D3), over ashpd: the docs/19 D5 handshake —
//! `CreateSession` → `SelectSources` → `Start` → `OpenPipeWireRemote` — with
//! the options docs/19 settled: monitor or window, one source, the cursor
//! embedded, and **no `persist_mode` and no restore token, ever**. Every
//! broadcast asks what to share (docs/19 D5 as reversed, 2026-07-16).
//!
//! Picked before Start, like the macOS Share card: the grant and its fd are
//! held in memory until the broadcast ends or the user re-picks, then
//! released. Within one broadcast the grant is reused for cascade retries and
//! capture rebuilds, and never costs a second picker.
//!
//! The portal work runs on one long-lived runtime owned by this module:
//! zbus's connection tasks live on the runtime that created them, and the
//! portal session lives exactly as long as that connection — a per-pick
//! runtime would end the screencast the moment it was dropped.

use ashpd::desktop::screencast::{
    CursorMode, OpenPipeWireRemoteOptions, Screencast, SelectSourcesOptions, SourceType,
    StartCastOptions,
};
use ashpd::desktop::{CreateSessionOptions, Session};
use std::os::fd::{AsRawFd, OwnedFd};
use std::sync::OnceLock;

/// What was picked.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum SourceKind {
    Monitor,
    Window,
}

impl SourceKind {
    /// A virtual or unknown source takes the monitor path (D3).
    pub fn from_portal(t: Option<SourceType>) -> Self {
        match t {
            Some(SourceType::Window) => Self::Window,
            _ => Self::Monitor,
        }
    }

    /// The shell's capture mode: "app" for a window, "screen" otherwise.
    pub fn capture_mode(self) -> &'static str {
        match self {
            Self::Window => "app",
            Self::Monitor => "screen",
        }
    }

    /// The Share card's mode line. The portal never says whose window it
    /// was (docs/39 §1), so the card states the mode, never a guessed app.
    pub fn label(self) -> &'static str {
        match self {
            Self::Window => "One window",
            Self::Monitor => "Whole screen",
        }
    }
}

/// A held screencast grant: the PipeWire remote fd and the node to read.
pub struct Grant {
    pub fd: OwnedFd,
    pub node_id: u32,
    pub kind: SourceKind,
    /// The stream's size in compositor coordinates, when the portal reports
    /// one — the fit input (docs/39 D2). The frames remain the truth.
    pub size: Option<(u32, u32)>,
    session: Option<Session<Screencast>>,
}

impl std::fmt::Debug for Grant {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Grant")
            .field("fd", &self.fd.as_raw_fd())
            .field("node_id", &self.node_id)
            .field("kind", &self.kind)
            .field("size", &self.size)
            .finish()
    }
}

impl Grant {
    /// A grant with no portal session behind it: what a shell's tests hold
    /// in place of the desktop's picker, which no test environment has.
    /// Releasing it only closes `fd`.
    #[doc(hidden)]
    pub fn detached(fd: OwnedFd, node_id: u32, kind: SourceKind, size: Option<(u32, u32)>) -> Self {
        Self {
            fd,
            node_id,
            kind,
            size,
            session: None,
        }
    }

    /// Ends the portal session (the compositor's sharing indicator goes out)
    /// and closes the remote. Also done on drop.
    pub fn release(mut self) {
        self.close();
    }

    fn close(&mut self) {
        if let Some(session) = self.session.take() {
            runtime().spawn(async move {
                if let Err(e) = session.close().await {
                    log::debug!("portal session close: {e}");
                }
            });
        }
    }
}

impl Drop for Grant {
    fn drop(&mut self) {
        self.close();
    }
}

/// How a pick ended.
#[derive(Debug)]
pub enum Picked {
    Granted(Grant),
    /// The user dismissed the picker: silent, no error card.
    Cancelled,
    /// No portal, or it failed: a sentence naming the portal.
    Failed(String),
}

fn runtime() -> &'static tokio::runtime::Runtime {
    static RT: OnceLock<tokio::runtime::Runtime> = OnceLock::new();
    RT.get_or_init(|| {
        tokio::runtime::Builder::new_multi_thread()
            .worker_threads(1)
            .thread_name("portal")
            .enable_all()
            .build()
            .expect("the portal runtime")
    })
}

/// The `SelectSources` request, exactly: monitor | window, one source, the
/// cursor embedded — and deliberately nothing else. No persist mode and no
/// restore token (D3); a test asserts the serialized request.
pub fn select_options() -> SelectSourcesOptions {
    SelectSourcesOptions::default()
        .set_sources(SourceType::Monitor | SourceType::Window)
        .set_multiple(false)
        .set_cursor_mode(CursorMode::Embedded)
}

/// Opens the desktop's own picker without blocking the caller; `done` runs
/// on the portal thread with the outcome.
pub fn pick(done: impl FnOnce(Picked) + Send + 'static) {
    runtime().spawn(async move {
        done(pick_async().await);
    });
}

async fn pick_async() -> Picked {
    let cast = match Screencast::new().await {
        Ok(c) => c,
        Err(e) => return Picked::Failed(unavailable(&e)),
    };
    let session = match cast.create_session(CreateSessionOptions::default()).await {
        Ok(s) => s,
        Err(e) => return Picked::Failed(unavailable(&e)),
    };
    // From here the session exists; any early exit closes it.
    let outcome = handshake(&cast, &session).await;
    match outcome {
        Ok((fd, stream)) => Picked::Granted(Grant {
            fd,
            node_id: stream.pipe_wire_node_id(),
            kind: SourceKind::from_portal(stream.source_type()),
            size: stream
                .size()
                .filter(|&(w, h)| w > 0 && h > 0)
                .map(|(w, h)| (w as u32, h as u32)),
            session: Some(session),
        }),
        Err(outcome) => {
            let _ = session.close().await;
            outcome
        }
    }
}

async fn handshake(
    cast: &Screencast,
    session: &Session<Screencast>,
) -> Result<(OwnedFd, ashpd::desktop::screencast::Stream), Picked> {
    cast.select_sources(session, select_options())
        .await
        .and_then(|r| r.response())
        .map_err(outcome_of)?;
    let streams = cast
        .start(session, None, StartCastOptions::default())
        .await
        .and_then(|r| r.response())
        .map_err(outcome_of)?;
    let stream = streams
        .streams()
        .first()
        .cloned()
        .ok_or_else(|| Picked::Failed("The screen-share portal returned no stream.".into()))?;
    let fd = cast
        .open_pipe_wire_remote(session, OpenPipeWireRemoteOptions::default())
        .await
        .map_err(|e| {
            log::warn!("OpenPipeWireRemote failed: {e}");
            Picked::Failed("The screen-share portal gave no PipeWire access.".into())
        })?;
    Ok((fd, stream))
}

/// A dismissed picker is `Cancelled`; everything else is a sentence.
fn outcome_of(e: ashpd::Error) -> Picked {
    match e {
        ashpd::Error::Response(ashpd::desktop::ResponseError::Cancelled) => Picked::Cancelled,
        e => {
            log::warn!("screen-share portal request failed: {e}");
            Picked::Failed("The screen-share portal failed; see the debug log.".into())
        }
    }
}

/// The card's sentence stays short (it is a one-line banner); the D-Bus
/// detail goes to the debug log.
fn unavailable(e: &ashpd::Error) -> String {
    log::warn!("screen-share portal unavailable: {e}");
    "No screen-share portal found. Install xdg-desktop-portal and your desktop's backend \
(-kde, -gnome or -wlr)."
        .into()
}

#[cfg(test)]
mod tests {
    use super::*;
    use ashpd::zvariant::{self, OwnedValue, serialized::Context};
    use std::collections::HashMap;

    /// D3: the request carries types, multiple and cursor mode (plus the
    /// handle token every request has) — and never a persist mode or a
    /// restore token. Asserted on the serialized request, not on our intent.
    #[test]
    fn select_sources_never_persists_the_choice() {
        let ctxt = Context::new_dbus(zvariant::LE, 0);
        let bytes = zvariant::to_bytes(ctxt, &select_options()).unwrap();
        let (dict, _): (HashMap<String, OwnedValue>, _) = bytes.deserialize().unwrap();
        let mut keys: Vec<&str> = dict.keys().map(String::as_str).collect();
        keys.sort();
        assert_eq!(keys, ["cursor_mode", "handle_token", "multiple", "types"]);
        assert_eq!(
            u32::try_from(&dict["types"]).unwrap(),
            0b11,
            "monitor | window"
        );
        assert!(!bool::try_from(&dict["multiple"]).unwrap());
        assert_eq!(u32::try_from(&dict["cursor_mode"]).unwrap(), 2, "embedded");
    }

    #[test]
    fn a_virtual_or_unknown_source_takes_the_monitor_path() {
        assert_eq!(
            SourceKind::from_portal(Some(SourceType::Window)),
            SourceKind::Window
        );
        assert_eq!(
            SourceKind::from_portal(Some(SourceType::Monitor)),
            SourceKind::Monitor
        );
        assert_eq!(
            SourceKind::from_portal(Some(SourceType::Virtual)),
            SourceKind::Monitor
        );
        assert_eq!(SourceKind::from_portal(None), SourceKind::Monitor);
        assert_eq!(SourceKind::Window.capture_mode(), "app");
        assert_eq!(SourceKind::Monitor.label(), "Whole screen");
    }

    #[test]
    fn a_dismissed_picker_is_silent_and_anything_else_is_a_sentence() {
        assert!(matches!(
            outcome_of(ashpd::Error::Response(
                ashpd::desktop::ResponseError::Cancelled
            )),
            Picked::Cancelled
        ));
        match outcome_of(ashpd::Error::NoResponse) {
            Picked::Failed(s) => assert!(s.contains("portal"), "{s}"),
            other => panic!("{other:?}"),
        }
        let s = unavailable(&ashpd::Error::NoResponse);
        assert!(s.contains("xdg-desktop-portal"));
        assert!(s.len() < 120, "a one-line banner: {s}");
    }
}
