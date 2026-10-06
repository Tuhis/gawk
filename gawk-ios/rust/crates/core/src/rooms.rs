//! Joining, as Swift sees it (docs/67 D20, D21): `gawk://` links read by
//! the shared parser every native app uses (`engine::link`, docs/68 D1–D2),
//! never re-parsed here, and a room watched for its roster so the room
//! view can subscribe to each attached broadcast. The room field's input,
//! the room link and the watch link are the engine's too (docs/60 D8,
//! docs/70 D16).

use gawk_engine::link::{self, Link};
use gawk_engine::room::{self, RoomConfig, RoomRequest, RoomSummary};
use gawk_engine::session::EngineEvent;
use gawk_engine::transport::WtRoomDialer;
use gawk_engine::{RoomGrant as EngineGrant, RoomInput as EngineInput};
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

/// One person in the room (docs/70 D20's People).
#[derive(Debug, Clone, PartialEq, Eq, uniffi::Record)]
pub struct RoomPerson {
    pub id: u16,
    pub nickname: String,
    /// Owns an attached stream; otherwise watching.
    pub streaming: bool,
}

/// The room's picture, replaced on every change.
#[derive(Debug, Clone, PartialEq, Eq, uniffi::Record)]
pub struct RoomView {
    pub code: String,
    pub display_name: String,
    pub participants: u32,
    pub tiles: Vec<RoomTile>,
    /// Everyone in the room, in the relay's order (docs/70 D20).
    pub people: Vec<RoomPerson>,
    /// This participant's `id` in `people` ("Sam (you)").
    pub your_id: u16,
    /// This participant holds the creator grant (docs/70 D18).
    pub creator: bool,
    /// A minted room; a static room is the operator's slug.
    pub dynamic: bool,
    /// This participant may attach a stream.
    pub attach_ok: bool,
}

/// The engine's room summary as Swift sees it, for the viewer's room and
/// the broadcaster's alike.
pub(crate) fn room_view(s: RoomSummary) -> RoomView {
    RoomView {
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
        people: s
            .people
            .into_iter()
            .map(|p| RoomPerson {
                id: p.id,
                nickname: p.nickname,
                streaming: p.streaming,
            })
            .collect(),
        your_id: s.your_id,
        creator: s.creator,
        dynamic: s.dynamic,
        attach_ok: s.attach_ok,
    }
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
    /// Held for the session: the room reads a closed channel as a stop.
    requests: mpsc::UnboundedSender<RoomRequest>,
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
        let (requests, requests_rx) = mpsc::unbounded_channel();
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
                    let session = tokio::spawn(room::watch_room_with_requests(
                        cfg,
                        dialer,
                        tx,
                        stop_rx,
                        requests_rx,
                    ));
                    while let Some(e) = rx.recv().await {
                        match e {
                            EngineEvent::RoomState(s) => listener.on_room(room_view(s)),
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
            requests,
        })
    }

    /// Renames you in the room (docs/70 D20): SetNickname on the wire, cut
    /// to the wire's 32 bytes. The relay pushes the renamed participant
    /// back, so a later `on_room` shows it. A rename before the room is
    /// joined goes out with the join.
    pub fn set_nickname(&self, nickname: String) {
        let _ = self.requests.send(RoomRequest::SetNickname(nickname));
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

/// A room link's grant (docs/60 D8): what its `?rt=` carries.
#[derive(Debug, Clone, PartialEq, Eq, uniffi::Enum)]
pub enum RoomGrant {
    /// A minted room's creator token, hex: rejoin as its creator.
    Creator { token_hex: String },
    /// A static room's attach key.
    Attach { key: String },
}

impl From<EngineGrant> for RoomGrant {
    fn from(g: EngineGrant) -> Self {
        match g {
            EngineGrant::Creator(token_hex) => RoomGrant::Creator { token_hex },
            EngineGrant::Attach(key) => RoomGrant::Attach { key },
        }
    }
}

impl From<RoomGrant> for EngineGrant {
    fn from(g: RoomGrant) -> Self {
        match g {
            RoomGrant::Creator { token_hex } => EngineGrant::Creator(token_hex),
            RoomGrant::Attach { key } => EngineGrant::Attach(key),
        }
    }
}

/// What the room field held (docs/70 D16): the code, and the grant a
/// pasted room link carried.
#[derive(Debug, Clone, PartialEq, Eq, uniffi::Record)]
pub struct RoomInput {
    pub code: String,
    pub grant: Option<RoomGrant>,
}

/// Reads the room field as the desktop does: a code or slug, a room link
/// with its `?rt=` grant, or a `gawk://` room link. `None` when it holds
/// no usable code.
#[uniffi::export]
pub fn parse_room_input(input: String) -> Option<RoomInput> {
    let EngineInput { code, grant } = gawk_engine::parse_room_input(&input)?;
    Some(RoomInput {
        code,
        grant: grant.map(Into::into),
    })
}

/// The room's link on the reference UI, with the grant when there is one
/// (docs/70 D17's Copy room link).
#[uniffi::export]
pub fn room_link(code: String, grant: Option<RoomGrant>) -> String {
    let grant = grant.map(EngineGrant::from);
    gawk_engine::room_link(gawk_engine::defaults::APP_URL, &code, grant.as_ref())
}

/// A broadcast's watch link on the reference UI (docs/70 D5a).
#[uniffi::export]
pub fn watch_link(broadcast_id: String) -> String {
    gawk_engine::join_link(gawk_engine::defaults::APP_URL, &broadcast_id)
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

    /// docs/70 D18, D20: the people, who you are, and what you may do, as
    /// the relay said them.
    #[test]
    fn the_room_view_carries_the_roster_and_your_grants() {
        let s = RoomSummary {
            code: "QX7P2K".into(),
            display_name: "LAN party".into(),
            your_id: 8,
            dynamic: true,
            creator: true,
            attach_ok: true,
            key_hex: "ab".into(),
            attachments: vec![room::RoomAttachmentInfo {
                broadcast_id: "K7XQ2M".into(),
                label: "Mika".into(),
                live: false,
                viewer_count: 3,
            }],
            participants: 2,
            people: vec![
                room::RoomPerson {
                    id: 7,
                    nickname: "Mika".into(),
                    streaming: true,
                    ..Default::default()
                },
                room::RoomPerson {
                    id: 8,
                    nickname: "Sam".into(),
                    ..Default::default()
                },
            ],
        };
        let v = room_view(s);
        assert_eq!(
            (v.code.as_str(), v.display_name.as_str()),
            ("QX7P2K", "LAN party")
        );
        assert_eq!(v.participants, 2);
        assert_eq!(
            v.tiles,
            [RoomTile {
                broadcast_id: "K7XQ2M".into(),
                label: "Mika".into(),
                live: false,
                viewer_count: 3,
            }]
        );
        assert_eq!(
            v.people,
            [
                RoomPerson {
                    id: 7,
                    nickname: "Mika".into(),
                    streaming: true,
                },
                RoomPerson {
                    id: 8,
                    nickname: "Sam".into(),
                    streaming: false,
                },
            ],
            "the relay's order"
        );
        assert_eq!(v.your_id, 8);
        assert!(v.creator && v.dynamic && v.attach_ok);
    }

    /// The room field, the room link and the watch link are the engine's
    /// (docs/60 D8): a pasted link's grant survives the round trip.
    #[test]
    fn room_input_and_links_come_from_the_engine() {
        let hex = "5a".repeat(16);
        let link = room_link(
            "QX7P2K".into(),
            Some(RoomGrant::Creator {
                token_hex: hex.clone(),
            }),
        );
        assert_eq!(
            link,
            format!("https://gawk.ioio.fi/#/room/QX7P2K?rt=c%3A{hex}")
        );
        assert_eq!(
            parse_room_input(link),
            Some(RoomInput {
                code: "QX7P2K".into(),
                grant: Some(RoomGrant::Creator { token_hex: hex }),
            })
        );
        let keyed = room_link(
            "lan-party".into(),
            Some(RoomGrant::Attach { key: "k3y".into() }),
        );
        assert_eq!(
            parse_room_input(keyed).and_then(|i| i.grant),
            Some(RoomGrant::Attach { key: "k3y".into() })
        );
        assert_eq!(
            parse_room_input(" k7xq2m ".into()),
            Some(RoomInput {
                code: "k7xq2m".into(),
                grant: None,
            })
        );
        assert_eq!(
            parse_room_input("gawk://room/lan-party".into()).map(|i| i.code),
            Some("lan-party".into())
        );
        assert_eq!(parse_room_input("  ".into()), None);
        assert_eq!(
            room_link("QX7P2K".into(), None),
            "https://gawk.ioio.fi/#/room/QX7P2K"
        );
        assert_eq!(
            watch_link("K7XQ2M".into()),
            "https://gawk.ioio.fi/#/view/K7XQ2M"
        );
    }
}
