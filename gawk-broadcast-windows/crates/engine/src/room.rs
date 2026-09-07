//! The room control session (R42, docs/44 §4.6 / §4.8 RM6): the native
//! broadcaster's side of the room protocol. One bidirectional stream carries
//! length-prefixed records; this module dials it, says RoomHello, keeps a
//! small owned summary of the room, and — the whole point for a broadcaster
//! — attaches the running broadcast with its resume token as the proof.
//!
//! Attach timing is the subtle part and mirrors the shell's identity latch:
//! the relay's announce and resume-token streams arrive in NO fixed order
//! (docs/22 finding 9), and the room's first snapshot can arrive before
//! either. The attach is sent only once BOTH the room is joined and the
//! publish identity is complete, and it is re-sent whenever the publish
//! session resumes (the relay's attach ownership is per control session, so
//! a reconnected broadcaster must re-attach before it may detach) and after
//! every room reconnect. A mint attaches implicitly, but the attachment it
//! creates has no owner participant on the relay; one idempotent Attach
//! after the first snapshot claims it, so the minter is flagged streaming
//! and can detach as the publisher — the same as the Linux engine.
//!
//! Like the publish session this speaks a seam — [`RoomConn`] +
//! [`RoomDialer`] — so tests script closes, rejections and record order
//! without a relay; the integration suite runs the same code against the
//! real one.

use crate::relay::{BoxFuture, SessionClose, StartError};
use crate::resume::{RESUME_WINDOW, ResumeBackoff};
use crate::session::EngineEvent;
use gawk_wire as wire;
use std::sync::Arc;
use std::time::Duration;
use tokio::sync::{mpsc, watch};

/// Everything a room control session needs. Built by the session from its
/// [`crate::session::SessionConfig`] plus the shell's runtime choice (join
/// this code / mint a new room).
#[derive(Debug, Clone, Default)]
pub struct RoomConfig {
    pub relay_url: String,
    pub origin: String,
    pub insecure: bool,
    /// The code (or static slug) to join. Empty ⇒ mint a dynamic room from
    /// the running broadcast through `/room/new` once its identity is known.
    pub code: String,
    /// A static room's attach key; presented as `?attach=` on join.
    pub attach_secret: String,
    /// The relay's `-room-create-secret`, presented on a mint.
    pub create_secret: String,
    /// A creator token (hex) from an earlier mint of `code`, presented as
    /// `?creator=`; the session fills this in itself after a mint so its
    /// reconnects keep the creator grant.
    pub creator_token_hex: String,
    /// The tile label the attachment carries.
    pub label: String,
    /// The RoomHello nickname (the relay assigns one when empty).
    pub nickname: String,
}

/// The publish identity the attach proof is built from. `generation`
/// changes on every publish resume so the room re-attaches (the relay's
/// attach ownership is per control session and the broadcast's hub state
/// was rebuilt).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Identity {
    pub broadcast_id: String,
    /// The raw 16-byte resume token — inside RoomCommand.Attach it travels
    /// as bytes, not hex.
    pub resume_token: Vec<u8>,
    pub generation: u64,
}

/// One attached broadcast, owned, as the shell sees it.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct RoomAttachmentInfo {
    pub broadcast_id: String,
    pub label: String,
    /// Clear while the broadcaster is away (within the broadcast grace).
    pub live: bool,
    pub viewer_count: u32,
}

/// The small owned summary of the room the engine keeps for its shell:
/// replaced from every RoomState and patched from every RoomEvent.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct RoomSummary {
    /// The display code (a dynamic code or a static slug).
    pub code: String,
    pub display_name: String,
    pub your_id: u16,
    pub dynamic: bool,
    /// This participant holds the creator grant.
    pub creator: bool,
    /// This participant may attach.
    pub attach_ok: bool,
    /// The room's HMAC'd key, hex — the /statusz and telemetry handle, the
    /// only form a log line may carry.
    pub key_hex: String,
    pub attachments: Vec<RoomAttachmentInfo>,
    pub participants: u32,
}

impl RoomSummary {
    fn from_state(s: &wire::RoomState<'_>) -> Self {
        RoomSummary {
            code: s.code.to_owned(),
            display_name: s.display_name.to_owned(),
            your_id: s.your_id,
            dynamic: s.flags & wire::ROOM_STATE_FLAG_DYNAMIC != 0,
            creator: s.flags & wire::ROOM_STATE_FLAG_CREATOR != 0,
            attach_ok: s.flags & wire::ROOM_STATE_FLAG_ATTACH_OK != 0,
            key_hex: hex_encode(s.key),
            attachments: s.attachments.iter().map(attachment_info).collect(),
            participants: s.participants.len() as u32,
        }
    }

    fn upsert(&mut self, a: RoomAttachmentInfo) {
        match self
            .attachments
            .iter_mut()
            .find(|x| x.broadcast_id == a.broadcast_id)
        {
            Some(x) => *x = a,
            None => self.attachments.push(a),
        }
    }

    fn remove(&mut self, broadcast_id: &str) {
        self.attachments.retain(|x| x.broadcast_id != broadcast_id);
    }

    /// Whether `broadcast_id` is attached (live or away).
    pub fn has(&self, broadcast_id: &str) -> bool {
        self.attachments
            .iter()
            .any(|x| x.broadcast_id == broadcast_id)
    }
}

fn attachment_info(a: &wire::RoomAttachment<'_>) -> RoomAttachmentInfo {
    RoomAttachmentInfo {
        broadcast_id: a.broadcast_id.clone(),
        label: a.label.to_owned(),
        live: a.live,
        viewer_count: a.viewer_count,
    }
}

// --- seam ----------------------------------------------------------------

/// One room control stream plus the session it rides on. `read` is exact
/// (a record reader must never see a short read as a boundary), `write`
/// takes a complete framed record, and `closed` yields the session's close
/// cause once it is dead — the same shape the publish seam uses.
pub trait RoomConn: Send + Sync + 'static {
    fn write(&self, record: &[u8]) -> BoxFuture<'_, Result<(), String>>;
    fn read(&self, n: usize) -> BoxFuture<'_, Result<Vec<u8>, String>>;
    fn closed(&self) -> BoxFuture<'_, SessionClose>;
}

/// Dials one room control session. The engine's transport implements it
/// with wtransport; tests script it.
pub trait RoomDialer: Send + Sync + 'static {
    fn dial(&self, url: &str) -> BoxFuture<'_, Result<Arc<dyn RoomConn>, StartError>>;
}

/// The shell's runtime requests to a running room session.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum RoomRequest {
    /// Detach this broadcast (when attached) and leave the room.
    Detach,
}

// --- URLs ----------------------------------------------------------------

/// Builds the join URL: `/room/{code}` with the optional `creator` (hex
/// token) and `attach` (static-room key) query parameters — the relay only
/// reads query params, for the same reason `publish_url` does. https is
/// enforced like every other relay URL.
pub fn room_url(
    relay_url: &str,
    code: &str,
    attach_secret: &str,
    creator_token_hex: &str,
) -> Result<String, String> {
    if code.is_empty() {
        return Err("room code is empty".into());
    }
    let mut url = parse_https(relay_url)?;
    url.set_path(&format!("/room/{code}"));
    {
        let mut q = url.query_pairs_mut();
        if !creator_token_hex.is_empty() {
            q.append_pair("creator", creator_token_hex);
        }
        if !attach_secret.is_empty() {
            q.append_pair("attach", attach_secret);
        }
    }
    if url.query() == Some("") {
        url.set_query(None);
    }
    Ok(url.into())
}

/// Builds the mint URL: `/room/new?broadcast=…&resume=…[&label=…][&create=…]`
/// — a dynamic room minted from a LIVE broadcast whose resume token the
/// caller holds (docs/44 §4.4). Both the ID and the hex token are required.
pub fn room_new_url(
    relay_url: &str,
    broadcast_id: &str,
    resume_token_hex: &str,
    label: &str,
    create_secret: &str,
) -> Result<String, String> {
    if broadcast_id.is_empty() || resume_token_hex.is_empty() {
        return Err("a room can only be minted from a broadcast whose identity is known".into());
    }
    let mut url = parse_https(relay_url)?;
    url.set_path("/room/new");
    {
        let mut q = url.query_pairs_mut();
        q.append_pair("broadcast", broadcast_id);
        q.append_pair("resume", resume_token_hex);
        if !label.is_empty() {
            q.append_pair("label", label);
        }
        if !create_secret.is_empty() {
            q.append_pair("create", create_secret);
        }
    }
    Ok(url.into())
}

fn parse_https(relay_url: &str) -> Result<url::Url, String> {
    let url = url::Url::parse(relay_url).map_err(|e| format!("bad relay URL: {e}"))?;
    if url.scheme() != "https" {
        return Err(format!("relay URL must be https, got {}", url.scheme()));
    }
    Ok(url)
}

/// The user-facing sentence for a refused room dial. The statuses are the
/// relay's (gawk-server/internal/transport/room.go); keep in step.
pub fn room_dial_message(e: &StartError) -> String {
    match e.status {
        403 => {
            "the relay refused the room: wrong attach key, creator token or create secret".into()
        }
        404 => "the relay knows no such room (or the broadcast to mint it from)".into(),
        409 => "this broadcast is already in another room".into(),
        429 => "the room is full, or the relay has no free room slot".into(),
        451 => "the relay operator has banned this address from rooms".into(),
        503 => "the relay's room store is unavailable — try again shortly".into(),
        _ => e.message.clone(),
    }
}

/// Whether a room dial's HTTP status means retrying can only fail again.
/// 429 (full / no slot) and 503 (store down) are transient; 0 is a bare
/// transport failure.
pub fn room_dial_terminal(status: u16) -> bool {
    matches!(status, 403 | 404 | 409 | 451)
}

/// The sentence for a room ending (RoomEvent.RoomEnding reason).
pub fn room_end_message(reason: Option<u8>) -> String {
    match reason {
        Some(wire::ROOM_END_REASON_EMPTY) => "the room ended: it stayed empty".into(),
        Some(wire::ROOM_END_REASON_CREATOR) => "the room's creator ended it".into(),
        Some(wire::ROOM_END_REASON_OPERATOR) => "the relay operator ended the room".into(),
        _ => "the room ended".into(),
    }
}

/// The sentence for an attachment removal (RoomEvent.AttachmentRemoved
/// reason).
pub fn detach_message(reason: u8) -> String {
    match reason {
        wire::ROOM_DETACH_REASON_PUBLISHER => "you detached this broadcast".into(),
        wire::ROOM_DETACH_REASON_CREATOR => "the room's creator detached this broadcast".into(),
        wire::ROOM_DETACH_REASON_EXPIRED => "the broadcast expired".into(),
        wire::ROOM_DETACH_REASON_ROOM_END => "the room ended".into(),
        other => format!("detached (reason {other})"),
    }
}

/// The sentence for a CommandRejected reason.
pub fn reject_message(reason: u8) -> String {
    match reason {
        wire::ROOM_REJECT_LIMIT => "the room has no free broadcast slot".into(),
        wire::ROOM_REJECT_BAD_PROOF => "the resume token did not verify".into(),
        wire::ROOM_REJECT_NOT_FOUND => "the broadcast is unknown to the relay".into(),
        wire::ROOM_REJECT_FORBIDDEN => "not allowed (missing attach key or creator grant)".into(),
        wire::ROOM_REJECT_ALREADY_ATTACHED => "this broadcast is already in another room".into(),
        wire::ROOM_REJECT_UNSUPPORTED => "the relay does not support this command".into(),
        wire::ROOM_REJECT_UNAVAILABLE => "the relay's room store is unavailable".into(),
        other => format!("rejected (reason {other})"),
    }
}

// --- hex + bounds --------------------------------------------------------

pub fn hex_encode(bytes: &[u8]) -> String {
    bytes.iter().map(|b| format!("{b:02x}")).collect()
}

/// Decodes a hex string (either case); `None` for odd length or non-hex.
pub fn hex_decode(s: &str) -> Option<Vec<u8>> {
    if !s.len().is_multiple_of(2) || !s.is_ascii() {
        return None;
    }
    (0..s.len())
        .step_by(2)
        .map(|i| u8::from_str_radix(&s[i..i + 2], 16).ok())
        .collect()
}

/// Truncates to at most `max` bytes on a char boundary — the wire bounds
/// nicknames and labels in BYTES, and a user typing past the bound must get
/// a shorter name, not a rejected hello.
pub fn truncate_utf8(s: &str, max: usize) -> &str {
    if s.len() <= max {
        return s;
    }
    let mut end = max;
    while !s.is_char_boundary(end) {
        end -= 1;
    }
    &s[..end]
}

// --- framing ---------------------------------------------------------------

/// Frames one message as a record; the message is built by the caller
/// through the wire crate's `append_room_*`.
fn frame(msg: &[u8]) -> Result<Vec<u8>, String> {
    let mut rec = Vec::with_capacity(msg.len() + wire::ROOM_RECORD_HEADER_SIZE);
    wire::append_room_record(&mut rec, msg).map_err(|e| e.to_string())?;
    Ok(rec)
}

fn hello_record(nickname: &str) -> Result<Vec<u8>, String> {
    let mut msg = Vec::new();
    wire::append_room_hello(
        &mut msg,
        &wire::RoomHello {
            protocol: wire::ROOM_PROTOCOL_VERSION,
            client_kind: wire::ROOM_CLIENT_NATIVE,
            want_caps: 0,
            nickname: truncate_utf8(nickname, wire::MAX_ROOM_NICKNAME_LEN),
        },
    )
    .map_err(|e| e.to_string())?;
    frame(&msg)
}

fn command_record(c: &wire::RoomCommand<'_>) -> Result<Vec<u8>, String> {
    let mut msg = Vec::new();
    wire::append_room_command(&mut msg, c).map_err(|e| e.to_string())?;
    frame(&msg)
}

/// Reads one framed message (Version ‖ Type ‖ payload) from the stream. The
/// length prefix is validated before anything is allocated.
async fn read_record(conn: &dyn RoomConn) -> Result<Vec<u8>, String> {
    let hdr = conn.read(wire::ROOM_RECORD_HEADER_SIZE).await?;
    let n = wire::parse_room_record_length(&hdr).map_err(|e| e.to_string())?;
    conn.read(n).await
}

// --- the session -----------------------------------------------------------

pub(crate) struct RoomCtx {
    pub cfg: RoomConfig,
    pub dialer: Arc<dyn RoomDialer>,
    pub identity: watch::Receiver<Option<Identity>>,
    pub events: mpsc::UnboundedSender<EngineEvent>,
    pub stop: watch::Receiver<bool>,
    pub requests: mpsc::UnboundedReceiver<RoomRequest>,
}

enum Serve {
    /// The shell stopped the session; nothing to say.
    Stopped,
    /// We detached and left, or the room ended: already reported.
    Done,
    /// The control session died for a reason worth reconnecting.
    Lost,
}

/// Runs one room session to completion: the first dial (a mint waits for
/// the publish identity), then serve / reconnect until the room ends, the
/// shell detaches, or the session is stopped.
pub(crate) async fn run_room(mut ctx: RoomCtx) {
    let minted = ctx.cfg.code.is_empty();
    let url = if minted {
        let Some(id) = wait_identity(&mut ctx).await else {
            return;
        };
        room_new_url(
            &ctx.cfg.relay_url,
            &id.broadcast_id,
            &hex_encode(&id.resume_token),
            truncate_utf8(&ctx.cfg.label, wire::MAX_ROOM_LABEL_LEN),
            &ctx.cfg.create_secret,
        )
    } else {
        room_url(
            &ctx.cfg.relay_url,
            &ctx.cfg.code,
            &ctx.cfg.attach_secret,
            &ctx.cfg.creator_token_hex,
        )
    };
    let url = match url {
        Ok(u) => u,
        Err(e) => {
            let _ = ctx.events.send(EngineEvent::RoomEnded { reason: e });
            return;
        }
    };
    let mut conn = match ctx.dialer.dial(&url).await {
        Ok(c) => c,
        Err(e) => {
            let _ = ctx.events.send(EngineEvent::RoomEnded {
                reason: room_dial_message(&e),
            });
            return;
        }
    };
    loop {
        match serve_room(&mut ctx, conn.clone()).await {
            Serve::Stopped | Serve::Done => return,
            Serve::Lost => {}
        }
        match reconnect(&mut ctx).await {
            Ok(next) => conn = next,
            Err(None) => return,
            Err(Some(reason)) => {
                let _ = ctx.events.send(EngineEvent::RoomEnded { reason });
                return;
            }
        }
    }
}

/// Waits for the publish identity (announce + token). `None` = stopped.
async fn wait_identity(ctx: &mut RoomCtx) -> Option<Identity> {
    loop {
        if let Some(id) = ctx.identity.borrow().clone() {
            return Some(id);
        }
        tokio::select! {
            changed = ctx.identity.changed() => {
                if changed.is_err() {
                    return None;
                }
            }
            changed = ctx.stop.changed() => {
                if changed.is_err() || *ctx.stop.borrow() {
                    return None;
                }
            }
        }
    }
}

/// Per-connection state. Reset on every (re)connect: the relay's view of
/// who attached is per control session.
struct Local {
    summary: RoomSummary,
    joined: bool,
    last_seq: Option<u32>,
    /// The identity generation the attach was sent for on THIS connection.
    attached_gen: Option<u64>,
    /// Whether the room currently lists our broadcast.
    ours_listed: bool,
    leaving: bool,
    ending_reason: Option<u8>,
}

async fn serve_room(ctx: &mut RoomCtx, conn: Arc<dyn RoomConn>) -> Serve {
    let hello = match hello_record(&ctx.cfg.nickname) {
        Ok(h) => h,
        Err(e) => {
            let _ = ctx.events.send(EngineEvent::RoomEnded { reason: e });
            return Serve::Done;
        }
    };
    if conn.write(&hello).await.is_err() {
        return Serve::Lost;
    }

    // A detached reader: `read` is exact and a select! that dropped a
    // half-done read would lose bytes, so records arrive through a channel.
    let (rec_tx, mut rec_rx) = mpsc::unbounded_channel::<Vec<u8>>();
    let reader = {
        let conn = conn.clone();
        tokio::spawn(async move {
            while let Ok(rec) = read_record(conn.as_ref()).await {
                if rec_tx.send(rec).is_err() {
                    return;
                }
            }
        })
    };

    let mut local = Local {
        summary: RoomSummary::default(),
        joined: false,
        last_seq: None,
        attached_gen: None,
        ours_listed: false,
        leaving: false,
        ending_reason: None,
    };
    let leave_timer = tokio::time::sleep(Duration::from_secs(3600));
    tokio::pin!(leave_timer);

    let end = loop {
        tokio::select! {
            rec = rec_rx.recv() => {
                let Some(rec) = rec else {
                    break Serve::Lost;
                };
                match handle_record(ctx, conn.as_ref(), &mut local, &rec).await {
                    Ok(Some(done)) => break done,
                    Ok(None) => {}
                    Err(()) => break Serve::Lost,
                }
            }
            changed = ctx.stop.changed() => {
                if changed.is_err() || *ctx.stop.borrow() {
                    break Serve::Stopped;
                }
            }
            changed = ctx.identity.changed() => {
                if changed.is_err() {
                    break Serve::Stopped;
                }
                if maybe_attach(ctx, conn.as_ref(), &mut local).await.is_err() {
                    break Serve::Lost;
                }
            }
            req = ctx.requests.recv() => {
                match req {
                    Some(RoomRequest::Detach) => {
                        if local.leaving {
                            continue;
                        }
                        local.leaving = true;
                        let ours = ctx.identity.borrow().clone();
                        match ours {
                            Some(id) if local.ours_listed => {
                                let rec = command_record(&wire::RoomCommand {
                                    kind: wire::ROOM_COMMAND_DETACH,
                                    broadcast_id: id.broadcast_id,
                                    ..wire::RoomCommand::default()
                                });
                                let written = match rec {
                                    Ok(rec) => conn.write(&rec).await.is_ok(),
                                    Err(_) => false,
                                };
                                if !written {
                                    break Serve::Lost;
                                }
                                // Wait (bounded) for the relay's
                                // AttachmentRemoved so the detach is on the
                                // wire before we hang up.
                                leave_timer
                                    .as_mut()
                                    .reset(tokio::time::Instant::now() + Duration::from_secs(3));
                            }
                            _ => {
                                let _ = ctx.events.send(EngineEvent::RoomDetached {
                                    reason: "you left the room".into(),
                                });
                                break Serve::Done;
                            }
                        }
                    }
                    None => break Serve::Stopped,
                }
            }
            _ = &mut leave_timer, if local.leaving => {
                let _ = ctx.events.send(EngineEvent::RoomDetached {
                    reason: "you left the room".into(),
                });
                break Serve::Done;
            }
        }
    };
    reader.abort();

    match end {
        Serve::Lost => match conn.closed().await {
            SessionClose::Code(wire::CLOSE_CODE_ROOM_ENDED) => {
                let _ = ctx.events.send(EngineEvent::RoomEnded {
                    reason: room_end_message(local.ending_reason),
                });
                Serve::Done
            }
            // Post-upgrade refusals (400 malformed, 404 ended meanwhile):
            // a redial would only collect the same answer.
            SessionClose::Code(code @ (400 | 404)) => {
                let _ = ctx.events.send(EngineEvent::RoomEnded {
                    reason: format!("the relay closed the room session (code {code})"),
                });
                Serve::Done
            }
            _ => Serve::Lost,
        },
        other => other,
    }
}

/// Sends Attach once the room is joined and the identity is complete, and
/// again whenever the identity generation moves (a publish resume). `Err`
/// = the write failed (the connection is gone).
async fn maybe_attach(ctx: &RoomCtx, conn: &dyn RoomConn, local: &mut Local) -> Result<(), ()> {
    if !local.joined {
        return Ok(());
    }
    let Some(id) = ctx.identity.borrow().clone() else {
        return Ok(());
    };
    if local.attached_gen == Some(id.generation) {
        return Ok(());
    }
    let rec = command_record(&wire::RoomCommand {
        kind: wire::ROOM_COMMAND_ATTACH,
        broadcast_id: id.broadcast_id,
        resume_token: &id.resume_token,
        label: truncate_utf8(&ctx.cfg.label, wire::MAX_ROOM_LABEL_LEN),
        ..wire::RoomCommand::default()
    })
    .map_err(|_| ())?;
    conn.write(&rec).await.map_err(|_| ())?;
    local.attached_gen = Some(id.generation);
    Ok(())
}

/// Dispatches one record. `Ok(Some(_))` ends the connection's serve loop;
/// `Err` = a write failed.
async fn handle_record(
    ctx: &mut RoomCtx,
    conn: &dyn RoomConn,
    local: &mut Local,
    rec: &[u8],
) -> Result<Option<Serve>, ()> {
    let Ok((_, t)) = wire::peek_type(rec) else {
        return Ok(None);
    };
    match t {
        wire::TYPE_ROOM_STATE => {
            let Ok(s) = wire::parse_room_state(rec) else {
                return Ok(None);
            };
            if !s.creator_token.is_empty() {
                // The first snapshot after a mint: the grant that lets this
                // session rejoin as creator, and the code viewers join by.
                ctx.cfg.creator_token_hex = hex_encode(s.creator_token);
                let _ = ctx.events.send(EngineEvent::RoomCreated {
                    code: s.code.to_owned(),
                    creator_token_hex: ctx.cfg.creator_token_hex.clone(),
                });
            }
            ctx.cfg.code = s.code.to_owned();
            local.summary = RoomSummary::from_state(&s);
            local.last_seq = Some(s.seq);
            local.joined = true;
            let id = ctx.identity.borrow().clone();
            local.ours_listed = id
                .as_ref()
                .is_some_and(|i| local.summary.has(&i.broadcast_id));
            // Every snapshot re-proves the attach — a resync or reconnect
            // replaced state, and a mint's first snapshot lists us without
            // an owner participant (the relay attaches on the minter's
            // behalf; the idempotent Attach that follows claims ownership,
            // which is what flags us streaming and lets us detach as the
            // publisher).
            local.attached_gen = None;
            let _ = ctx
                .events
                .send(EngineEvent::RoomState(local.summary.clone()));
            maybe_attach(ctx, conn, local).await?;
            Ok(None)
        }
        wire::TYPE_ROOM_EVENT => {
            let (seq, ev) = match wire::parse_room_event(rec) {
                Ok(e) => (e.seq, Some(e)),
                // A reserved (chat/voice) kind: skipped, but it still
                // occupies a sequence number.
                Err(wire::WireError::UnknownRoomKind { seq, .. }) => (seq, None),
                Err(_) => return Ok(None),
            };
            // Gap detection (docs/44 §4.6): CommandRejected carries the
            // current seq without advancing it, so it never reads as a gap.
            if let Some(last) = local.last_seq
                && seq > last.wrapping_add(1)
            {
                let rec = command_record(&wire::RoomCommand {
                    kind: wire::ROOM_COMMAND_RESYNC,
                    ..wire::RoomCommand::default()
                })
                .map_err(|_| ())?;
                conn.write(&rec).await.map_err(|_| ())?;
            }
            local.last_seq = Some(local.last_seq.map_or(seq, |l| l.max(seq)));
            let Some(ev) = ev else {
                return Ok(None);
            };
            handle_event(ctx, local, &ev)
        }
        _ => Ok(None),
    }
}

fn handle_event(
    ctx: &RoomCtx,
    local: &mut Local,
    ev: &wire::RoomEvent<'_>,
) -> Result<Option<Serve>, ()> {
    let our_id = ctx
        .identity
        .borrow()
        .as_ref()
        .map(|i| i.broadcast_id.clone());
    let is_ours = |id: &str| our_id.as_deref() == Some(id);
    match ev.kind {
        wire::ROOM_EVENT_PARTICIPANT_JOINED => {
            local.summary.participants += 1;
        }
        wire::ROOM_EVENT_PARTICIPANT_LEFT => {
            local.summary.participants = local.summary.participants.saturating_sub(1);
        }
        wire::ROOM_EVENT_PARTICIPANT_UPDATED => return Ok(None),
        wire::ROOM_EVENT_ATTACHMENT_ADDED | wire::ROOM_EVENT_ATTACHMENT_UPDATED => {
            let ours = is_ours(&ev.attachment.broadcast_id);
            local.summary.upsert(attachment_info(&ev.attachment));
            if ours && !local.ours_listed {
                local.ours_listed = true;
                let _ = ctx.events.send(EngineEvent::RoomAttached);
            }
        }
        wire::ROOM_EVENT_ATTACHMENT_REMOVED => {
            let ours = is_ours(&ev.attachment.broadcast_id);
            local.summary.remove(&ev.attachment.broadcast_id);
            if ours {
                local.ours_listed = false;
                local.attached_gen = None;
                let _ = ctx.events.send(EngineEvent::RoomDetached {
                    reason: detach_message(ev.reason),
                });
                if local.leaving {
                    return Ok(Some(Serve::Done));
                }
            }
        }
        wire::ROOM_EVENT_ROOM_ENDING => {
            local.ending_reason = Some(ev.reason);
            return Ok(None);
        }
        wire::ROOM_EVENT_COMMAND_REJECTED => {
            let _ = ctx.events.send(EngineEvent::RoomRejected {
                reason: reject_message(ev.reason),
                message: ev.message.to_owned(),
            });
            if local.leaving && ev.command == wire::ROOM_COMMAND_DETACH {
                return Ok(Some(Serve::Done));
            }
            return Ok(None);
        }
        _ => return Ok(None),
    }
    let _ = ctx
        .events
        .send(EngineEvent::RoomState(local.summary.clone()));
    Ok(None)
}

/// Redials the room with the resume backoff until it works, the relay says
/// it never will, or the window closes. `Err(None)` = stopped.
async fn reconnect(ctx: &mut RoomCtx) -> Result<Arc<dyn RoomConn>, Option<String>> {
    let url = room_url(
        &ctx.cfg.relay_url,
        &ctx.cfg.code,
        &ctx.cfg.attach_secret,
        &ctx.cfg.creator_token_hex,
    )
    .map_err(Some)?;
    let mut backoff = ResumeBackoff::new();
    let deadline = tokio::time::Instant::now() + RESUME_WINDOW;
    let mut attempt: u32 = 0;
    loop {
        attempt += 1;
        if *ctx.stop.borrow() {
            return Err(None);
        }
        let _ = ctx.events.send(EngineEvent::RoomReconnecting { attempt });
        let delay = backoff.next_delay();
        tokio::select! {
            _ = tokio::time::sleep(delay) => {}
            changed = ctx.stop.changed() => {
                if changed.is_err() || *ctx.stop.borrow() {
                    return Err(None);
                }
            }
        }
        match ctx.dialer.dial(&url).await {
            Ok(conn) => return Ok(conn),
            Err(e) => {
                if room_dial_terminal(e.status) || tokio::time::Instant::now() > deadline {
                    return Err(Some(format!(
                        "lost the room and could not rejoin: {}",
                        room_dial_message(&e)
                    )));
                }
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn room_urls_follow_the_relay_query_contract() {
        assert_eq!(
            room_url("https://api.gawk.ioio.fi:4433", "K7XQ2M", "", "").unwrap(),
            "https://api.gawk.ioio.fi:4433/room/K7XQ2M"
        );
        // A static slug with its attach key; a creator token rides too.
        assert_eq!(
            room_url(
                "https://relay.example:4433/",
                "lan-party",
                "k3y",
                "aa".repeat(16).as_str()
            )
            .unwrap(),
            format!(
                "https://relay.example:4433/room/lan-party?creator={}&attach=k3y",
                "aa".repeat(16)
            )
        );
        assert_eq!(
            room_new_url(
                "https://localhost:4433",
                "K7XQ2M",
                "deadbeef",
                "Juho's PC",
                ""
            )
            .unwrap(),
            "https://localhost:4433/room/new?broadcast=K7XQ2M&resume=deadbeef&label=Juho%27s+PC"
        );
        assert_eq!(
            room_new_url("https://localhost:4433", "K7XQ2M", "deadbeef", "", "cr3ate").unwrap(),
            "https://localhost:4433/room/new?broadcast=K7XQ2M&resume=deadbeef&create=cr3ate"
        );
        // https enforced like publish_url; a mint needs both halves of the
        // identity; a join needs a code.
        assert!(room_url("http://localhost:4433", "K7XQ2M", "", "").is_err());
        assert!(room_url("https://localhost:4433", "", "", "").is_err());
        assert!(room_new_url("http://localhost:4433", "K7XQ2M", "deadbeef", "", "").is_err());
        assert!(room_new_url("https://localhost:4433", "K7XQ2M", "", "", "").is_err());
        assert!(room_new_url("https://localhost:4433", "", "deadbeef", "", "").is_err());
    }

    #[test]
    fn hex_round_trips_and_rejects_junk() {
        assert_eq!(hex_encode(&[0xde, 0xad, 0x01]), "dead01");
        assert_eq!(hex_decode("DEAD01").unwrap(), vec![0xde, 0xad, 0x01]);
        assert_eq!(hex_decode("").unwrap(), Vec::<u8>::new());
        assert!(hex_decode("abc").is_none());
        assert!(hex_decode("zz").is_none());
        assert!(hex_decode("ää").is_none());
    }

    #[test]
    fn truncation_respects_char_boundaries() {
        assert_eq!(truncate_utf8("short", 32), "short");
        // 'ä' is two bytes: a cut inside it steps back.
        assert_eq!(truncate_utf8("aääa", 2), "a");
        assert_eq!(truncate_utf8("aääa", 3), "aä");
        assert_eq!(
            truncate_utf8(&"x".repeat(40), wire::MAX_ROOM_NICKNAME_LEN).len(),
            32
        );
    }

    // --- the session, against a scripted control stream -------------------

    use crate::relay::StartPhase;
    use std::collections::VecDeque;
    use std::sync::Mutex;
    use tokio::sync::mpsc::{UnboundedReceiver, UnboundedSender};

    /// One scripted control session: bytes to the client arrive in chunks
    /// (so `read` must assemble records), each client write is one record.
    struct FakeConn {
        inbox: tokio::sync::Mutex<(UnboundedReceiver<Vec<u8>>, Vec<u8>)>,
        outbox: UnboundedSender<Vec<u8>>,
        close: watch::Receiver<Option<SessionClose>>,
    }

    impl RoomConn for FakeConn {
        fn write(&self, record: &[u8]) -> BoxFuture<'_, Result<(), String>> {
            let r = self
                .outbox
                .send(record.to_vec())
                .map_err(|_| "closed".to_string());
            Box::pin(async move { r })
        }
        fn read(&self, n: usize) -> BoxFuture<'_, Result<Vec<u8>, String>> {
            Box::pin(async move {
                let mut g = self.inbox.lock().await;
                while g.1.len() < n {
                    match g.0.recv().await {
                        Some(chunk) => g.1.extend_from_slice(&chunk),
                        None => return Err("closed".into()),
                    }
                }
                Ok(g.1.drain(..n).collect())
            })
        }
        fn closed(&self) -> BoxFuture<'_, SessionClose> {
            let mut rx = self.close.clone();
            Box::pin(async move {
                loop {
                    if let Some(c) = rx.borrow().clone() {
                        return c;
                    }
                    if rx.changed().await.is_err() {
                        return SessionClose::Abrupt("gone".into());
                    }
                }
            })
        }
    }

    /// The relay's end of one scripted session.
    struct RelayEnd {
        to_client: Option<UnboundedSender<Vec<u8>>>,
        from_client: UnboundedReceiver<Vec<u8>>,
        close: watch::Sender<Option<SessionClose>>,
    }

    impl RelayEnd {
        fn send_msg(&self, msg: &[u8]) {
            let mut rec = Vec::new();
            wire::append_room_record(&mut rec, msg).unwrap();
            // Split across two chunks: the reader must assemble.
            let (a, b) = rec.split_at(rec.len() / 2);
            let tx = self.to_client.as_ref().unwrap();
            tx.send(a.to_vec()).unwrap();
            tx.send(b.to_vec()).unwrap();
        }
        fn send_state(&self, s: &wire::RoomState<'_>) {
            let mut msg = Vec::new();
            wire::append_room_state(&mut msg, s).unwrap();
            self.send_msg(&msg);
        }
        fn send_event(&self, e: &wire::RoomEvent<'_>) {
            let mut msg = Vec::new();
            wire::append_room_event(&mut msg, e).unwrap();
            self.send_msg(&msg);
        }
        /// The next record the client wrote, header stripped.
        async fn next(&mut self) -> Vec<u8> {
            let rec = tokio::time::timeout(Duration::from_secs(5), self.from_client.recv())
                .await
                .expect("client record")
                .expect("client stream open");
            let n = wire::parse_room_record_length(&rec[..2]).unwrap();
            assert_eq!(rec.len(), n + 2);
            rec[2..].to_vec()
        }
        async fn next_command(&mut self) -> wire::RoomCommand<'static> {
            let msg = self.next().await;
            let c = wire::parse_room_command(&msg).unwrap();
            wire::RoomCommand {
                kind: c.kind,
                broadcast_id: c.broadcast_id,
                resume_token: Box::leak(c.resume_token.to_vec().into_boxed_slice()),
                label: Box::leak(c.label.to_owned().into_boxed_str()),
                nickname: Box::leak(c.nickname.to_owned().into_boxed_str()),
            }
        }
        /// Kills the session with the given cause.
        fn close(&mut self, cause: SessionClose) {
            let _ = self.close.send(Some(cause));
            self.to_client = None;
        }
    }

    fn scripted() -> (Arc<dyn RoomConn>, RelayEnd) {
        let (to_client, inbox) = mpsc::unbounded_channel();
        let (outbox, from_client) = mpsc::unbounded_channel();
        let (close_tx, close_rx) = watch::channel(None);
        let conn = Arc::new(FakeConn {
            inbox: tokio::sync::Mutex::new((inbox, Vec::new())),
            outbox,
            close: close_rx,
        });
        (
            conn,
            RelayEnd {
                to_client: Some(to_client),
                from_client,
                close: close_tx,
            },
        )
    }

    struct FakeDialer {
        conns: Mutex<VecDeque<Result<Arc<dyn RoomConn>, u16>>>,
        urls: Mutex<Vec<String>>,
    }

    impl RoomDialer for FakeDialer {
        fn dial(&self, url: &str) -> BoxFuture<'_, Result<Arc<dyn RoomConn>, StartError>> {
            self.urls.lock().unwrap().push(url.to_owned());
            let next = self.conns.lock().unwrap().pop_front();
            Box::pin(async move {
                match next {
                    Some(Ok(c)) => Ok(c),
                    Some(Err(status)) => Err(StartError {
                        phase: StartPhase::Connect,
                        status,
                        message: format!("status {status}"),
                    }),
                    None => Err(StartError {
                        phase: StartPhase::Connect,
                        status: 0,
                        message: "no scripted connection".into(),
                    }),
                }
            })
        }
    }

    struct Harness {
        identity: watch::Sender<Option<Identity>>,
        events: UnboundedReceiver<EngineEvent>,
        stop: watch::Sender<bool>,
        requests: UnboundedSender<RoomRequest>,
        dialer: Arc<FakeDialer>,
        task: tokio::task::JoinHandle<()>,
    }

    impl Harness {
        fn start(cfg: RoomConfig, conns: Vec<Result<Arc<dyn RoomConn>, u16>>) -> Self {
            let (identity, identity_rx) = watch::channel(None);
            let (events_tx, events) = mpsc::unbounded_channel();
            let (stop, stop_rx) = watch::channel(false);
            let (requests, requests_rx) = mpsc::unbounded_channel();
            let dialer = Arc::new(FakeDialer {
                conns: Mutex::new(conns.into_iter().collect()),
                urls: Mutex::new(Vec::new()),
            });
            let task = tokio::spawn(run_room(RoomCtx {
                cfg,
                dialer: dialer.clone(),
                identity: identity_rx,
                events: events_tx,
                stop: stop_rx,
                requests: requests_rx,
            }));
            Harness {
                identity,
                events,
                stop,
                requests,
                dialer,
                task,
            }
        }

        async fn event(&mut self) -> EngineEvent {
            tokio::time::timeout(Duration::from_secs(5), self.events.recv())
                .await
                .expect("an event")
                .expect("events open")
        }

        /// Skips RoomState summaries until something else arrives.
        async fn event_not_state(&mut self) -> EngineEvent {
            loop {
                match self.event().await {
                    EngineEvent::RoomState(_) => continue,
                    other => return other,
                }
            }
        }

        fn set_identity(&self, generation: u64) {
            self.identity
                .send(Some(Identity {
                    broadcast_id: "K7XQ2M".into(),
                    resume_token: vec![0xab; wire::RESUME_TOKEN_SIZE],
                    generation,
                }))
                .unwrap();
        }
    }

    fn join_cfg() -> RoomConfig {
        RoomConfig {
            relay_url: "https://relay.example:4433".into(),
            code: "lan-party".into(),
            attach_secret: "k3y".into(),
            label: "Juho's PC".into(),
            nickname: "Juho".into(),
            ..RoomConfig::default()
        }
    }

    fn snapshot<'a>(seq: u32, attachments: Vec<wire::RoomAttachment<'a>>) -> wire::RoomState<'a> {
        wire::RoomState {
            flags: wire::ROOM_STATE_FLAG_ATTACH_OK,
            seq,
            your_id: 7,
            code: "lan-party",
            attachments,
            ..wire::RoomState::default()
        }
    }

    fn ours(live: bool) -> wire::RoomAttachment<'static> {
        wire::RoomAttachment {
            broadcast_id: "K7XQ2M".into(),
            label: "Juho's PC",
            live,
            viewer_count: 0,
        }
    }

    #[tokio::test]
    async fn hello_first_then_attach_only_once_joined_and_identified() {
        let (conn, mut relay) = scripted();
        let mut h = Harness::start(join_cfg(), vec![Ok(conn)]);

        // The first record on the stream is the hello: native, our nickname.
        let hello_msg = relay.next().await;
        let hello = wire::parse_room_hello(&hello_msg).unwrap();
        assert_eq!(hello.client_kind, wire::ROOM_CLIENT_NATIVE);
        assert_eq!(hello.nickname, "Juho");
        assert_eq!(
            h.dialer.urls.lock().unwrap()[0],
            "https://relay.example:4433/room/lan-party?attach=k3y"
        );

        // Joined before the identity is known: a summary, no attach yet.
        relay.send_state(&snapshot(10, vec![]));
        match h.event().await {
            EngineEvent::RoomState(s) => {
                assert_eq!(s.code, "lan-party");
                assert!(s.attach_ok && !s.creator);
            }
            other => panic!("{other:?}"),
        }
        assert!(
            tokio::time::timeout(Duration::from_millis(100), relay.from_client.recv())
                .await
                .is_err(),
            "no attach before the identity"
        );

        // Both halves known: the attach carries id, raw token and label.
        h.set_identity(0);
        let c = relay.next_command().await;
        assert_eq!(c.kind, wire::ROOM_COMMAND_ATTACH);
        assert_eq!(c.broadcast_id, "K7XQ2M");
        assert_eq!(c.resume_token, &[0xab; 16][..]);
        assert_eq!(c.label, "Juho's PC");

        // The relay confirms: RoomAttached, and the summary lists us.
        relay.send_event(&wire::RoomEvent {
            seq: 11,
            kind: wire::ROOM_EVENT_ATTACHMENT_ADDED,
            attachment: ours(true),
            ..wire::RoomEvent::default()
        });
        assert_eq!(h.event().await, EngineEvent::RoomAttached);
        match h.event().await {
            EngineEvent::RoomState(s) => assert!(s.has("K7XQ2M") && s.attachments[0].live),
            other => panic!("{other:?}"),
        }

        // A publish resume moves the generation: re-attach, nothing else.
        h.set_identity(1);
        assert_eq!(relay.next_command().await.kind, wire::ROOM_COMMAND_ATTACH);
        h.set_identity(1);
        assert!(
            tokio::time::timeout(Duration::from_millis(100), relay.from_client.recv())
                .await
                .is_err(),
            "the same generation attaches once"
        );
        h.stop.send(true).unwrap();
        let _ = h.task.await;
    }

    #[tokio::test]
    async fn a_sequence_gap_resyncs_but_a_rejection_does_not() {
        let (conn, mut relay) = scripted();
        let mut h = Harness::start(join_cfg(), vec![Ok(conn)]);
        relay.next().await; // hello
        relay.send_state(&snapshot(10, vec![]));
        h.event().await;

        // CommandRejected carries the CURRENT seq (10): not a gap.
        relay.send_event(&wire::RoomEvent {
            seq: 10,
            kind: wire::ROOM_EVENT_COMMAND_REJECTED,
            command: wire::ROOM_COMMAND_ATTACH,
            reason: wire::ROOM_REJECT_BAD_PROOF,
            message: "resume token does not match",
            ..wire::RoomEvent::default()
        });
        assert_eq!(
            h.event().await,
            EngineEvent::RoomRejected {
                reason: reject_message(wire::ROOM_REJECT_BAD_PROOF),
                message: "resume token does not match".into(),
            }
        );
        // 11 was missed: 12 is a gap → Resync.
        relay.send_event(&wire::RoomEvent {
            seq: 12,
            kind: wire::ROOM_EVENT_PARTICIPANT_JOINED,
            participant: wire::RoomParticipant {
                id: 9,
                nickname: "x",
                ..wire::RoomParticipant::default()
            },
            ..wire::RoomEvent::default()
        });
        assert_eq!(relay.next_command().await.kind, wire::ROOM_COMMAND_RESYNC);
        // A reserved (chat) kind still occupies a seq: 13 is not a gap.
        let mut chat = vec![wire::VERSION, wire::TYPE_ROOM_EVENT];
        chat.extend_from_slice(&13u32.to_be_bytes());
        chat.push(0x40);
        relay.send_msg(&chat);
        relay.send_event(&wire::RoomEvent {
            seq: 14,
            kind: wire::ROOM_EVENT_PARTICIPANT_LEFT,
            participant: wire::RoomParticipant {
                id: 9,
                ..wire::RoomParticipant::default()
            },
            ..wire::RoomEvent::default()
        });
        for want in [1, 0] {
            match h.event().await {
                EngineEvent::RoomState(s) => assert_eq!(s.participants, want),
                other => panic!("{other:?}"),
            }
        }
        assert!(
            tokio::time::timeout(Duration::from_millis(100), relay.from_client.recv())
                .await
                .is_err(),
            "no second resync"
        );
        h.stop.send(true).unwrap();
        let _ = h.task.await;
    }

    #[tokio::test]
    async fn room_ended_4007_is_terminal_for_the_room_only() {
        let (conn, mut relay) = scripted();
        let (spare, _spare_relay) = scripted();
        let mut h = Harness::start(join_cfg(), vec![Ok(conn), Ok(spare)]);
        relay.next().await;
        relay.send_state(&snapshot(1, vec![]));
        h.event().await;
        relay.send_event(&wire::RoomEvent {
            seq: 2,
            kind: wire::ROOM_EVENT_ROOM_ENDING,
            reason: wire::ROOM_END_REASON_CREATOR,
            ..wire::RoomEvent::default()
        });
        relay.close(SessionClose::Code(wire::CLOSE_CODE_ROOM_ENDED));
        assert_eq!(
            h.event_not_state().await,
            EngineEvent::RoomEnded {
                reason: room_end_message(Some(wire::ROOM_END_REASON_CREATOR)),
            }
        );
        let _ = h.task.await;
        assert_eq!(
            h.dialer.urls.lock().unwrap().len(),
            1,
            "no redial after 4007"
        );
    }

    #[tokio::test(start_paused = true)]
    async fn a_mint_waits_for_the_identity_and_reconnects_as_creator() {
        let (conn, mut relay) = scripted();
        let (conn2, mut relay2) = scripted();
        let cfg = RoomConfig {
            code: String::new(),
            create_secret: "cr3ate".into(),
            ..join_cfg()
        };
        let mut h = Harness::start(cfg, vec![Ok(conn), Ok(conn2)]);
        tokio::task::yield_now().await;
        assert!(
            h.dialer.urls.lock().unwrap().is_empty(),
            "no dial without an identity"
        );
        h.set_identity(0);
        let hello = relay.next().await;
        assert!(wire::parse_room_hello(&hello).is_ok());
        assert_eq!(
            h.dialer.urls.lock().unwrap()[0],
            format!(
                "https://relay.example:4433/room/new?broadcast=K7XQ2M&resume={}&label=Juho%27s+PC&create=cr3ate",
                "ab".repeat(16)
            )
        );

        // The first snapshot carries the creator token and lists us already.
        let token = [0x5a; wire::ROOM_CREATOR_TOKEN_SIZE];
        relay.send_state(&wire::RoomState {
            flags: wire::ROOM_STATE_FLAG_DYNAMIC
                | wire::ROOM_STATE_FLAG_CREATOR
                | wire::ROOM_STATE_FLAG_ATTACH_OK,
            seq: 1,
            your_id: 1,
            code: "QX7P2K",
            creator_token: &token,
            attachments: vec![ours(true)],
            ..wire::RoomState::default()
        });
        assert_eq!(
            h.event().await,
            EngineEvent::RoomCreated {
                code: "QX7P2K".into(),
                creator_token_hex: "5a".repeat(16),
            }
        );
        match h.event().await {
            EngineEvent::RoomState(s) => assert!(s.creator && s.dynamic && s.has("K7XQ2M")),
            other => panic!("{other:?}"),
        }
        // A mint attaches implicitly, but the minted attachment has no
        // owner participant: exactly one idempotent Attach claims it.
        let c = relay.next_command().await;
        assert_eq!(c.kind, wire::ROOM_COMMAND_ATTACH);
        assert_eq!(c.broadcast_id, "K7XQ2M");
        assert_eq!(c.label, "Juho's PC");
        assert!(
            tokio::time::timeout(Duration::from_millis(100), relay.from_client.recv())
                .await
                .is_err(),
            "one ownership attach after the mint, not two"
        );

        // An abrupt loss: reconnect by code with the creator token, then
        // hello + a fresh attach (ownership is per control session).
        relay.close(SessionClose::Abrupt("idle".into()));
        assert_eq!(
            h.event_not_state().await,
            EngineEvent::RoomReconnecting { attempt: 1 }
        );
        assert!(wire::parse_room_hello(&relay2.next().await).is_ok());
        assert_eq!(
            h.dialer.urls.lock().unwrap()[1],
            format!(
                "https://relay.example:4433/room/QX7P2K?creator={}&attach=k3y",
                "5a".repeat(16)
            )
        );
        relay2.send_state(&wire::RoomState {
            flags: wire::ROOM_STATE_FLAG_DYNAMIC
                | wire::ROOM_STATE_FLAG_CREATOR
                | wire::ROOM_STATE_FLAG_ATTACH_OK,
            seq: 5,
            your_id: 2,
            code: "QX7P2K",
            attachments: vec![ours(true)],
            ..wire::RoomState::default()
        });
        assert_eq!(relay2.next_command().await.kind, wire::ROOM_COMMAND_ATTACH);
        h.stop.send(true).unwrap();
        let _ = h.task.await;
    }

    #[tokio::test(start_paused = true)]
    async fn a_refused_rejoin_gives_up_with_the_relay_s_reason() {
        let (conn, mut relay) = scripted();
        let mut h = Harness::start(join_cfg(), vec![Ok(conn), Err(429), Err(404)]);
        relay.next().await;
        relay.close(SessionClose::Code(wire::CLOSE_CODE_SERVER_DRAINING));
        assert_eq!(
            h.event().await,
            EngineEvent::RoomReconnecting { attempt: 1 }
        );
        // 429 is transient (retried); 404 is terminal.
        assert_eq!(
            h.event().await,
            EngineEvent::RoomReconnecting { attempt: 2 }
        );
        match h.event().await {
            EngineEvent::RoomEnded { reason } => {
                assert!(reason.contains("no such room"), "{reason}")
            }
            other => panic!("{other:?}"),
        }
        let _ = h.task.await;
    }

    #[tokio::test]
    async fn detach_sends_the_command_and_leaves_on_the_relay_s_confirmation() {
        let (conn, mut relay) = scripted();
        let mut h = Harness::start(join_cfg(), vec![Ok(conn)]);
        relay.next().await;
        h.set_identity(0);
        relay.send_state(&snapshot(1, vec![ours(true)]));
        h.event().await;
        // Listed already, so the snapshot's attach is re-proven once…
        assert_eq!(relay.next_command().await.kind, wire::ROOM_COMMAND_ATTACH);
        // …then the shell detaches.
        h.requests.send(RoomRequest::Detach).unwrap();
        let c = relay.next_command().await;
        assert_eq!(
            (c.kind, c.broadcast_id.as_str()),
            (wire::ROOM_COMMAND_DETACH, "K7XQ2M")
        );
        relay.send_event(&wire::RoomEvent {
            seq: 2,
            kind: wire::ROOM_EVENT_ATTACHMENT_REMOVED,
            attachment: wire::RoomAttachment {
                broadcast_id: "K7XQ2M".into(),
                ..wire::RoomAttachment::default()
            },
            reason: wire::ROOM_DETACH_REASON_PUBLISHER,
            ..wire::RoomEvent::default()
        });
        assert_eq!(
            h.event().await,
            EngineEvent::RoomDetached {
                reason: detach_message(wire::ROOM_DETACH_REASON_PUBLISHER),
            }
        );
        let _ = h.task.await;
        assert_eq!(
            h.dialer.urls.lock().unwrap().len(),
            1,
            "leaving is not a loss"
        );
    }

    #[test]
    fn dial_statuses_split_terminal_from_transient() {
        for s in [403, 404, 409, 451] {
            assert!(room_dial_terminal(s), "{s}");
        }
        for s in [0, 429, 500, 503] {
            assert!(!room_dial_terminal(s), "{s}");
        }
        let e = StartError {
            phase: crate::relay::StartPhase::Connect,
            status: 409,
            message: "x".into(),
        };
        assert!(room_dial_message(&e).contains("another room"));
    }
}
