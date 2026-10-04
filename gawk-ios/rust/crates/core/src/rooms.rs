//! Joining, as Swift sees it (docs/67 D20, D21): `gawk://` links read by
//! the shared parser every native app uses (`engine::link`, docs/68 D1–D2),
//! never re-parsed here, and a room watched for its roster so the room
//! view can subscribe to each attached broadcast.

use gawk_engine::link::{self, Link};
use gawk_engine::room::{self, RoomConfig};
use gawk_engine::session::EngineEvent;
use gawk_engine::transport::WtRoomDialer;
use std::sync::{Arc, Mutex};
use tokio::sync::{mpsc, watch};

/// A `gawk://` link (docs/68 D1). `relay` is a server other than the
/// default fleet; `None` means the default fleet (D20: a non-default one
/// shows docs/40's persistent strip).
#[derive(Debug, Clone, PartialEq, Eq, uniffi::Enum)]
pub enum GawkLink {
    Watch {
        id: String,
        relay: Option<String>,
    },
    Room {
        code: String,
        nick: Option<String>,
        relay: Option<String>,
    },
    Broadcast {
        room: Option<String>,
        nick: Option<String>,
        relay: Option<String>,
    },
}

/// A parsed link and the parameters it dropped (by name only: a dropped
/// value may be a secret, docs/68 D1).
#[derive(Debug, Clone, PartialEq, Eq, uniffi::Record)]
pub struct ParsedLink {
    pub link: GawkLink,
    pub dropped: Vec<String>,
}

#[derive(Debug, Clone, PartialEq, Eq, uniffi::Error, thiserror::Error)]
pub enum LinkError {
    #[error("{message}")]
    Invalid { code: String, message: String },
}

/// Reads a `gawk://` link the way the desktop apps do.
#[uniffi::export]
pub fn parse_gawk_link(raw: String) -> Result<ParsedLink, LinkError> {
    let parsed = link::parse(&raw).map_err(|e| LinkError::Invalid {
        code: e.code().into(),
        message: e.to_string(),
    })?;
    let link = match parsed.link {
        Link::Watch { id, relay } => GawkLink::Watch { id, relay },
        Link::Room { code, nick, relay } => GawkLink::Room { code, nick, relay },
        Link::Broadcast { room, nick, relay } => GawkLink::Broadcast { room, nick, relay },
    };
    Ok(ParsedLink {
        link,
        dropped: parsed.dropped.into_iter().map(|d| d.param).collect(),
    })
}

/// One attached broadcast in a room.
#[derive(Debug, Clone, PartialEq, Eq, uniffi::Record)]
pub struct RoomTile {
    pub broadcast_id: String,
    pub label: String,
    /// Clear while its broadcaster is away (within the grace).
    pub live: bool,
    pub viewer_count: u32,
}

/// The room's picture, replaced on every change.
#[derive(Debug, Clone, PartialEq, Eq, uniffi::Record)]
pub struct RoomView {
    pub code: String,
    pub display_name: String,
    pub participants: u32,
    pub tiles: Vec<RoomTile>,
}

/// Implemented in Swift; called on the room's thread.
#[uniffi::export(with_foreign)]
pub trait RoomListener: Send + Sync {
    fn on_room(&self, room: RoomView);
    /// Reconnecting the control session (attempt counter).
    fn on_reconnecting(&self, attempt: u32);
    /// Over for good: the room ended, the join was refused, or a reconnect
    /// gave up. The tiles' own viewers are untouched.
    fn on_ended(&self, reason: String);
}

/// A room watched for its roster. Dropping it (or [`RoomWatcher::stop`])
/// leaves the room.
#[derive(uniffi::Object)]
pub struct RoomWatcher {
    stop: Mutex<Option<watch::Sender<bool>>>,
}

#[uniffi::export]
impl RoomWatcher {
    #[uniffi::constructor]
    pub fn start(
        relay_url: String,
        code: String,
        nickname: String,
        insecure: bool,
        listener: Arc<dyn RoomListener>,
    ) -> Arc<Self> {
        let (stop_tx, stop_rx) = watch::channel(false);
        let origin = gawk_engine::defaults::origin().to_owned();
        std::thread::Builder::new()
            .name("gawk-room".into())
            .spawn(move || {
                let rt = tokio::runtime::Builder::new_current_thread()
                    .enable_all()
                    .build()
                    .expect("a tokio runtime");
                rt.block_on(async move {
                    let (tx, mut rx) = mpsc::unbounded_channel();
                    let cfg = RoomConfig {
                        relay_url,
                        origin: origin.clone(),
                        insecure,
                        code,
                        nickname,
                        ..RoomConfig::default()
                    };
                    let dialer = Arc::new(WtRoomDialer { origin, insecure });
                    let session = tokio::spawn(room::watch_room(cfg, dialer, tx, stop_rx));
                    while let Some(e) = rx.recv().await {
                        match e {
                            EngineEvent::RoomState(s) => listener.on_room(RoomView {
                                code: s.code,
                                display_name: s.display_name,
                                participants: s.participants,
                                tiles: s
                                    .attachments
                                    .into_iter()
                                    .map(|a| RoomTile {
                                        broadcast_id: a.broadcast_id,
                                        label: a.label,
                                        live: a.live,
                                        viewer_count: a.viewer_count,
                                    })
                                    .collect(),
                            }),
                            EngineEvent::RoomReconnecting { attempt } => {
                                listener.on_reconnecting(attempt)
                            }
                            EngineEvent::RoomEnded { reason } => listener.on_ended(reason),
                            _ => {}
                        }
                    }
                    let _ = session.await;
                });
            })
            .expect("spawn the room thread");
        Arc::new(Self {
            stop: Mutex::new(Some(stop_tx)),
        })
    }

    pub fn stop(&self) {
        if let Some(tx) = self.stop.lock().unwrap().take() {
            let _ = tx.send(true);
        }
    }
}

impl Drop for RoomWatcher {
    fn drop(&mut self) {
        self.stop();
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn links_come_from_the_shared_parser() {
        let p = parse_gawk_link("gawk://watch/K7XQ2M".into()).unwrap();
        assert_eq!(
            p.link,
            GawkLink::Watch {
                id: "K7XQ2M".into(),
                relay: None
            }
        );
        let p = parse_gawk_link("gawk://room/lan-party?nick=Ann&secret=x".into()).unwrap();
        assert!(matches!(p.link, GawkLink::Room { ref code, .. } if code == "lan-party"));
        assert_eq!(p.dropped, ["secret"], "a secret never travels in a link");
        let e = parse_gawk_link("https://gawk.ioio.fi".into()).unwrap_err();
        assert!(matches!(e, LinkError::Invalid { ref code, .. } if code == "not-gawk"));
    }
}
