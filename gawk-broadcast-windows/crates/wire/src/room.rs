//! Room control protocol (R42, docs/44 §4.6), mirroring
//! gawk-server/wire/room.go function for function. The layout comment at the
//! top of that file is the spec; this is a restatement, never an import.
//!
//! Four message types travel as length-prefixed records on ONE bidirectional
//! stream per room control session (CONNECT /room/{code}): each record is
//! uint16 length (BE) ‖ Version ‖ Type ‖ payload — the reliable-carrier record
//! shape (R19) applied to a bidirectional stream. Media never rides this
//! stream; a participant's per-broadcast /subscribe sessions are untouched.
//!
//! Parsers are strict in the house style (exact length, reserved bits zero)
//! EXCEPT for the two forward-compatibility seams docs/44 §4.11 reserves on
//! purpose: an unknown RoomEvent kind or RoomCommand kind parses to
//! [`WireError::UnknownRoomKind`] with the header fields filled in so a reader
//! can skip the record (chat 0x40–0x4F and voice 0x50–0x5F are the reserved
//! sub-ranges), and the capability bitmaps are the advertise/request mechanism
//! for those later features.
//!
//! Borrowing: nicknames, labels, codes, messages and tokens borrow the input
//! like every other parser in this crate. Broadcast IDs are the one owned
//! field — Go's parsers return them NORMALIZED (upper-cased, validated against
//! the broadcast ID alphabet), which a borrowed `&str` cannot express — so
//! they are a `String` of exactly [`BROADCAST_ID_LENGTH`] bytes.
//!
//! Layouts, after the common Version ‖ Type prefix:
//!
//! ```text
//! 0x13 RoomHello (client → relay, once, first record on the stream):
//!   byte 2   uint8 protocol      ROOM_PROTOCOL_VERSION
//!   byte 3   uint8 clientKind    ROOM_CLIENT_*
//!   byte 4   uint8 wantCaps      ROOM_CAP_* bitmap; bits 2-7 reserved (0)
//!   byte 5   uint8 nickLen       0..MAX_ROOM_NICKNAME_LEN
//!   bytes 6+ nickname            valid UTF-8
//!
//! 0x14 RoomState (relay → client, full snapshot; replace, never merge):
//!   byte 2   uint8 flags         ROOM_STATE_FLAG_* bitmap; bits 3-7 reserved
//!   byte 3   uint8 caps          ROOM_CAP_* bitmap advertised by the room
//!   4-7      uint32 seq          event sequence this snapshot is current at
//!   8-9      uint16 yourID       the receiving participant's ID
//!   10       uint8 codeLen       1..MAX_ROOM_CODE_LEN, then the display code
//!   ...      uint8 nameLen       0..MAX_ROOM_DISPLAY_NAME_LEN, then display name
//!   ...      uint8 tokenLen      0 or ROOM_CREATOR_TOKEN_SIZE, then the creator
//!                                token — non-empty ONLY in the first
//!                                snapshot after /room/new (docs/44 §4.4)
//!   ...      uint8 attachCount   then that many Attachment records
//!   ...      uint16 partCount    then that many Participant records
//!
//!   Attachment record:
//!     uint8 idLen + broadcast ID, uint8 labelLen + label,
//!     uint8 flags (ROOM_ATTACHMENT_FLAG_*), uint32 viewerCount
//!   Participant record:
//!     uint16 id, uint8 kind (ROOM_CLIENT_*), uint8 flags
//!     (ROOM_PARTICIPANT_FLAG_*), uint8 nickLen + nickname,
//!     uint8 identityLen + identity (reserved, empty in v1 — docs/44 §4.11)
//!
//! 0x15 RoomEvent (relay → client, one delta):
//!   2-5      uint32 seq          monotonic within the home pod's generation
//!   6        uint8 kind          ROOM_EVENT_* kind
//!   7+       payload by kind:
//!     ParticipantJoined / ParticipantUpdated: Participant record
//!     ParticipantLeft:                        uint16 id
//!     AttachmentAdded / AttachmentUpdated:    Attachment record
//!     AttachmentRemoved: uint8 idLen + broadcast ID, uint8 reason
//!     RoomEnding:        uint8 reason (ROOM_END_REASON_*)
//!     CommandRejected:   uint8 command kind, uint8 reason
//!                        (ROOM_REJECT_*), uint8 msgLen + message
//!
//! 0x16 RoomCommand (client → relay):
//!   byte 2   uint8 kind          ROOM_COMMAND_* kind
//!   3+       payload by kind:
//!     Attach:      uint8 idLen + broadcast ID, uint8 tokenLen
//!                  (RESUME_TOKEN_SIZE) + resume token, uint8 labelLen + label
//!     Detach:      uint8 idLen + broadcast ID
//!     SetNickname: uint8 nickLen + nickname
//!     EndRoom, Resync: no payload
//! ```

use crate::error::WireError;
use crate::{
    BROADCAST_ID_ALPHABET, TYPE_ROOM_COMMAND, TYPE_ROOM_EVENT, TYPE_ROOM_HELLO, TYPE_ROOM_STATE,
    VERSION,
};

// --- constants ---------------------------------------------------------------

/// The room control protocol version a RoomHello carries. Independent of the
/// datagram [`VERSION`] byte so the control protocol can evolve (chat, voice)
/// without touching the media wire.
pub const ROOM_PROTOCOL_VERSION: u8 = 1;
/// The uint16 length prefix in front of every record on a room control
/// stream — the carrier record shape.
pub const ROOM_RECORD_HEADER_SIZE: usize = 2;
/// Bounds one record's framed bytes (Version ‖ Type ‖ payload, what the
/// length prefix counts): a reader must never allocate more than this from
/// an untrusted length.
pub const MAX_ROOM_RECORD_SIZE: usize = 16384;
/// Bounds a participant nickname in bytes (UTF-8).
pub const MAX_ROOM_NICKNAME_LEN: usize = 32;
/// Bounds a room's display code: a static slug is 3–32 characters (docs/44
/// §4.1); a dynamic code is [`BROADCAST_ID_LENGTH`].
pub const MAX_ROOM_CODE_LEN: usize = 32;
/// Bounds a static room's optional display name.
pub const MAX_ROOM_DISPLAY_NAME_LEN: usize = 64;
/// Bounds a broadcaster-chosen attachment label.
pub const MAX_ROOM_LABEL_LEN: usize = 32;
/// Bounds the reserved participant identity field (docs/44 §4.11: a key
/// fingerprint later; empty in v1).
pub const MAX_ROOM_IDENTITY_LEN: usize = 64;
/// Bounds a CommandRejected message.
pub const MAX_ROOM_REJECT_MESSAGE_LEN: usize = 128;
/// The byte length of a dynamic room's creator token: the same
/// truncated-HMAC construction as the resume token (docs/44 D8), with its own
/// domain-separation prefix.
pub const ROOM_CREATOR_TOKEN_SIZE: usize = 16;
/// The byte length of a broadcast resume token (R17 W2) as it appears inside
/// a RoomCommand.Attach — the attach proof (docs/44 D9). 128 bits of
/// HMAC-SHA256.
pub const RESUME_TOKEN_SIZE: usize = 16;
/// The length of a broadcast ID (gawk-server/internal/broadcastid.Length):
/// what the room parsers normalize against, together with
/// [`BROADCAST_ID_ALPHABET`].
pub const BROADCAST_ID_LENGTH: usize = 6;

// Client kinds (RoomHello clientKind, Participant.kind).
pub const ROOM_CLIENT_WEB_VIEWER: u8 = 0;
pub const ROOM_CLIENT_WEB_BROADCASTER: u8 = 1;
pub const ROOM_CLIENT_NATIVE: u8 = 2;
const ROOM_CLIENT_KIND_MAX: u8 = ROOM_CLIENT_NATIVE;

// Room capability bits (RoomHello wantCaps, RoomState caps). Both reserved
// for docs/44 §4.11's integrations; a v1 room advertises none.
pub const ROOM_CAP_CHAT: u8 = 0x01;
pub const ROOM_CAP_VOICE: u8 = 0x02;
const ROOM_CAP_MASK: u8 = ROOM_CAP_CHAT | ROOM_CAP_VOICE;

// RoomState flags.
/// Set for a dynamic room (clear: static).
pub const ROOM_STATE_FLAG_DYNAMIC: u8 = 0x01;
/// Set when the receiving participant presented a valid creator token (may
/// detach anyone, may end the room).
pub const ROOM_STATE_FLAG_CREATOR: u8 = 0x02;
/// Set when the receiving participant is allowed to attach (a dynamic room,
/// or a static room whose attach secret was presented or is unset).
pub const ROOM_STATE_FLAG_ATTACH_OK: u8 = 0x04;
const ROOM_STATE_FLAG_MASK: u8 =
    ROOM_STATE_FLAG_DYNAMIC | ROOM_STATE_FLAG_CREATOR | ROOM_STATE_FLAG_ATTACH_OK;

// Attachment flags.
/// Set while the broadcast's publisher session is up; clear means
/// "broadcaster away" (within the broadcast grace).
pub const ROOM_ATTACHMENT_FLAG_LIVE: u8 = 0x01;
const ROOM_ATTACHMENT_FLAG_MASK: u8 = ROOM_ATTACHMENT_FLAG_LIVE;

// Participant flags.
/// Reserved for the voice integration (docs/44 §4.11), carried from day one
/// so the roster's speaking indicator needs no wire change later.
pub const ROOM_PARTICIPANT_FLAG_SPEAKING: u8 = 0x01;
/// Set while the participant has at least one attached broadcast.
pub const ROOM_PARTICIPANT_FLAG_STREAMING: u8 = 0x02;
const ROOM_PARTICIPANT_FLAG_MASK: u8 =
    ROOM_PARTICIPANT_FLAG_SPEAKING | ROOM_PARTICIPANT_FLAG_STREAMING;

// RoomEvent kinds. 0x40–0x4F (chat) and 0x50–0x5F (voice) are reserved; the
// parser returns UnknownRoomKind for any kind it does not know.
pub const ROOM_EVENT_PARTICIPANT_JOINED: u8 = 0x01;
pub const ROOM_EVENT_PARTICIPANT_LEFT: u8 = 0x02;
pub const ROOM_EVENT_PARTICIPANT_UPDATED: u8 = 0x03;
pub const ROOM_EVENT_ATTACHMENT_ADDED: u8 = 0x10;
pub const ROOM_EVENT_ATTACHMENT_REMOVED: u8 = 0x11;
pub const ROOM_EVENT_ATTACHMENT_UPDATED: u8 = 0x12;
pub const ROOM_EVENT_ROOM_ENDING: u8 = 0x20;
pub const ROOM_EVENT_COMMAND_REJECTED: u8 = 0x30;

// RoomCommand kinds. Same reserved sub-ranges as events.
pub const ROOM_COMMAND_ATTACH: u8 = 0x01;
pub const ROOM_COMMAND_DETACH: u8 = 0x02;
pub const ROOM_COMMAND_SET_NICKNAME: u8 = 0x03;
pub const ROOM_COMMAND_END_ROOM: u8 = 0x04;
pub const ROOM_COMMAND_RESYNC: u8 = 0x05;

// RoomEndReason values (RoomEvent.RoomEnding).
pub const ROOM_END_REASON_EMPTY: u8 = 1;
pub const ROOM_END_REASON_CREATOR: u8 = 2;
pub const ROOM_END_REASON_OPERATOR: u8 = 3;

// RoomDetachReason values (RoomEvent.AttachmentRemoved).
pub const ROOM_DETACH_REASON_PUBLISHER: u8 = 0;
pub const ROOM_DETACH_REASON_CREATOR: u8 = 1;
pub const ROOM_DETACH_REASON_EXPIRED: u8 = 2;
pub const ROOM_DETACH_REASON_ROOM_END: u8 = 3;

// RoomRejectReason values (RoomEvent.CommandRejected). Every distinct
// failure a command can hit crosses the wire as its own reason, never a
// catch-all.
/// -max-room-broadcasts (or the CR's override) reached.
pub const ROOM_REJECT_LIMIT: u8 = 1;
/// The attach resume token does not verify.
pub const ROOM_REJECT_BAD_PROOF: u8 = 2;
/// The broadcast is unknown to the fleet, or the attachment to detach does
/// not exist.
pub const ROOM_REJECT_NOT_FOUND: u8 = 3;
/// The participant lacks the grant (attach without the attach secret,
/// detach-other / end without the creator token).
pub const ROOM_REJECT_FORBIDDEN: u8 = 4;
/// The broadcast is attached to another room (D1: at most one room per
/// broadcast).
pub const ROOM_REJECT_ALREADY_ATTACHED: u8 = 5;
/// The relay does not implement the command kind (a reserved chat/voice
/// command on a v1 relay).
pub const ROOM_REJECT_UNSUPPORTED: u8 = 6;
/// The command needs the API server and it is unreachable (docs/44 §6, fail
/// closed).
pub const ROOM_REJECT_UNAVAILABLE: u8 = 7;

// --- structs -----------------------------------------------------------------

/// The parsed contents of a RoomHello record.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub struct RoomHello<'a> {
    /// The room protocol version; [`ROOM_PROTOCOL_VERSION`] today.
    pub protocol: u8,
    /// One of the `ROOM_CLIENT_*` constants.
    pub client_kind: u8,
    /// The requested `ROOM_CAP_*` bitmap (0 in v1 clients).
    pub want_caps: u8,
    /// The requested display name; may be empty (the relay then assigns one).
    pub nickname: &'a str,
}

/// One attached broadcast as carried in RoomState and the attachment events.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct RoomAttachment<'a> {
    /// Normalized (upper-case) broadcast ID, exactly [`BROADCAST_ID_LENGTH`]
    /// bytes once parsed; append normalizes and validates it too.
    pub broadcast_id: String,
    pub label: &'a str,
    pub live: bool,
    pub viewer_count: u32,
}

/// One participant as carried in RoomState and the participant events.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub struct RoomParticipant<'a> {
    pub id: u16,
    /// One of the `ROOM_CLIENT_*` constants.
    pub kind: u8,
    /// `ROOM_PARTICIPANT_FLAG_*` bitmap.
    pub flags: u8,
    pub nickname: &'a str,
    /// Reserved (docs/44 §4.11); empty in v1.
    pub identity: &'a str,
}

/// The parsed contents of a RoomState record.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct RoomState<'a> {
    /// `ROOM_STATE_FLAG_*` bitmap.
    pub flags: u8,
    /// `ROOM_CAP_*` bitmap advertised by the room.
    pub caps: u8,
    pub seq: u32,
    pub your_id: u16,
    pub code: &'a str,
    pub display_name: &'a str,
    /// Empty, or exactly [`ROOM_CREATOR_TOKEN_SIZE`] bytes. Borrows the input
    /// on parse.
    pub creator_token: &'a [u8],
    pub attachments: Vec<RoomAttachment<'a>>,
    pub participants: Vec<RoomParticipant<'a>>,
}

/// The parsed contents of a RoomEvent record. Which fields are meaningful
/// depends on `kind` (see the module layout comment).
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct RoomEvent<'a> {
    pub seq: u32,
    /// One of the `ROOM_EVENT_*` constants.
    pub kind: u8,
    /// Set for ParticipantJoined / ParticipantUpdated; only its `id` for
    /// ParticipantLeft.
    pub participant: RoomParticipant<'a>,
    /// Set for AttachmentAdded / AttachmentUpdated; only its `broadcast_id`
    /// for AttachmentRemoved.
    pub attachment: RoomAttachment<'a>,
    /// The `ROOM_END_REASON_*` (RoomEnding), `ROOM_DETACH_REASON_*`
    /// (AttachmentRemoved) or `ROOM_REJECT_*` (CommandRejected).
    pub reason: u8,
    /// The rejected command's kind (CommandRejected).
    pub command: u8,
    /// The human-readable rejection detail (CommandRejected).
    pub message: &'a str,
}

/// The parsed contents of a RoomCommand record.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct RoomCommand<'a> {
    /// One of the `ROOM_COMMAND_*` constants.
    pub kind: u8,
    /// Set for Attach / Detach (normalized, see [`RoomAttachment`]).
    pub broadcast_id: String,
    /// The attach proof (Attach only), exactly [`RESUME_TOKEN_SIZE`] bytes.
    /// Borrows the input on parse.
    pub resume_token: &'a [u8],
    /// The attachment label (Attach only).
    pub label: &'a str,
    /// The new nickname (SetNickname only).
    pub nickname: &'a str,
}

// --- record framing ----------------------------------------------------------

/// Frames one already-encoded room message (Version ‖ Type ‖ payload, as
/// produced by the `append_room_*` functions) as a length-prefixed record
/// for a room control stream.
pub fn append_room_record(dst: &mut Vec<u8>, msg: &[u8]) -> Result<(), WireError> {
    if msg.len() < 2 || msg.len() > MAX_ROOM_RECORD_SIZE {
        return Err(WireError::BadRoomRecord);
    }
    dst.extend_from_slice(&(msg.len() as u16).to_be_bytes());
    dst.extend_from_slice(msg);
    Ok(())
}

/// Validates a room record's length prefix and returns the framed message
/// length. Exists so a stream reader never allocates from an unvalidated
/// length. A header shorter than [`ROOM_RECORD_HEADER_SIZE`] is
/// `ShortDatagram` ("read more"); a length outside 2..=MAX_ROOM_RECORD_SIZE
/// is `BadRoomRecord` (framing corruption; abandon the stream).
pub fn parse_room_record_length(hdr: &[u8]) -> Result<usize, WireError> {
    if hdr.len() < ROOM_RECORD_HEADER_SIZE {
        return Err(WireError::ShortDatagram {
            len: hdr.len(),
            need: ROOM_RECORD_HEADER_SIZE,
        });
    }
    let n = u16::from_be_bytes([hdr[0], hdr[1]]) as usize;
    if !(2..=MAX_ROOM_RECORD_SIZE).contains(&n) {
        return Err(WireError::BadRoomRecord);
    }
    Ok(n)
}

// --- shared field helpers ----------------------------------------------------

fn check_prefix(msg: &[u8], msg_type: u8, min_len: usize) -> Result<(), WireError> {
    if msg.len() < min_len {
        return Err(WireError::ShortDatagram {
            len: msg.len(),
            need: min_len,
        });
    }
    if msg[0] != VERSION {
        return Err(WireError::BadVersion(msg[0]));
    }
    if msg[1] != msg_type {
        return Err(WireError::BadType {
            got: msg[1],
            want: msg_type,
        });
    }
    Ok(())
}

/// Mirrors broadcastid.Normalize: upper-case, exactly BROADCAST_ID_LENGTH
/// bytes, every byte in the alphabet. Reuses the crate's alphabet; the only
/// place the room parsers allocate.
fn normalize_broadcast_id(raw: &[u8]) -> Option<String> {
    if raw.len() != BROADCAST_ID_LENGTH {
        return None;
    }
    let upper: Vec<u8> = raw.iter().map(u8::to_ascii_uppercase).collect();
    if !upper
        .iter()
        .all(|b| BROADCAST_ID_ALPHABET.as_bytes().contains(b))
    {
        return None;
    }
    // Alphabet bytes are ASCII, so this cannot fail after the check above.
    String::from_utf8(upper).ok()
}

/// A bounds-checked cursor over one record. Every read reports an overrun
/// through the record's sentinel error rather than panicking.
struct Reader<'a> {
    buf: &'a [u8],
    off: usize,
    sentinel: WireError,
}

impl<'a> Reader<'a> {
    fn new(buf: &'a [u8], sentinel: WireError) -> Self {
        Reader {
            buf,
            off: 2,
            sentinel,
        }
    }

    fn fail<T>(&self) -> Result<T, WireError> {
        Err(self.sentinel.clone())
    }

    fn take(&mut self, n: usize) -> Result<&'a [u8], WireError> {
        if self.off + n > self.buf.len() {
            return self.fail();
        }
        let v = &self.buf[self.off..self.off + n];
        self.off += n;
        Ok(v)
    }

    fn u8(&mut self) -> Result<u8, WireError> {
        Ok(self.take(1)?[0])
    }

    fn u16(&mut self) -> Result<u16, WireError> {
        let b = self.take(2)?;
        Ok(u16::from_be_bytes([b[0], b[1]]))
    }

    fn u32(&mut self) -> Result<u32, WireError> {
        let b = self.take(4)?;
        Ok(u32::from_be_bytes([b[0], b[1], b[2], b[3]]))
    }

    /// Reads a uint8-length-prefixed byte field, bounded by `max`.
    fn bytes8(&mut self, max: usize) -> Result<&'a [u8], WireError> {
        let n = self.u8()? as usize;
        if n > max {
            return self.fail();
        }
        self.take(n)
    }

    /// Reads a uint8-length-prefixed UTF-8 string field, bounded by `max`.
    fn str8(&mut self, max: usize) -> Result<&'a str, WireError> {
        let b = self.bytes8(max)?;
        match std::str::from_utf8(b) {
            Ok(s) => Ok(s),
            Err(_) => self.fail(),
        }
    }

    /// Reads a uint8-length-prefixed broadcast ID and normalizes it.
    fn broadcast_id(&mut self) -> Result<String, WireError> {
        let raw = self.bytes8(BROADCAST_ID_LENGTH)?;
        match normalize_broadcast_id(raw) {
            Some(id) => Ok(id),
            None => self.fail(),
        }
    }

    fn attachment(&mut self) -> Result<RoomAttachment<'a>, WireError> {
        let broadcast_id = self.broadcast_id()?;
        let label = self.str8(MAX_ROOM_LABEL_LEN)?;
        let flags = self.u8()?;
        if flags & !ROOM_ATTACHMENT_FLAG_MASK != 0 {
            return self.fail();
        }
        let viewer_count = self.u32()?;
        Ok(RoomAttachment {
            broadcast_id,
            label,
            live: flags & ROOM_ATTACHMENT_FLAG_LIVE != 0,
            viewer_count,
        })
    }

    fn participant(&mut self) -> Result<RoomParticipant<'a>, WireError> {
        let id = self.u16()?;
        let kind = self.u8()?;
        if kind > ROOM_CLIENT_KIND_MAX {
            return self.fail();
        }
        let flags = self.u8()?;
        if flags & !ROOM_PARTICIPANT_FLAG_MASK != 0 {
            return self.fail();
        }
        let nickname = self.str8(MAX_ROOM_NICKNAME_LEN)?;
        let identity = self.str8(MAX_ROOM_IDENTITY_LEN)?;
        Ok(RoomParticipant {
            id,
            kind,
            flags,
            nickname,
            identity,
        })
    }

    /// Asserts the record was consumed exactly (house strictness).
    fn done(&self) -> Result<(), WireError> {
        if self.off != self.buf.len() {
            return self.fail();
        }
        Ok(())
    }
}

fn append_str8(
    dst: &mut Vec<u8>,
    s: &str,
    max: usize,
    sentinel: WireError,
) -> Result<(), WireError> {
    // A &str is UTF-8 by construction; only the length can be wrong here
    // (the Go original also rejects invalid UTF-8 on append).
    if s.len() > max {
        return Err(sentinel);
    }
    dst.push(s.len() as u8);
    dst.extend_from_slice(s.as_bytes());
    Ok(())
}

fn append_broadcast_id(dst: &mut Vec<u8>, id: &str, sentinel: WireError) -> Result<(), WireError> {
    let norm = normalize_broadcast_id(id.as_bytes()).ok_or(sentinel)?;
    dst.push(norm.len() as u8);
    dst.extend_from_slice(norm.as_bytes());
    Ok(())
}

fn append_room_attachment(
    dst: &mut Vec<u8>,
    a: &RoomAttachment<'_>,
    sentinel: WireError,
) -> Result<(), WireError> {
    append_broadcast_id(dst, &a.broadcast_id, sentinel.clone())?;
    append_str8(dst, a.label, MAX_ROOM_LABEL_LEN, sentinel)?;
    let flags = if a.live { ROOM_ATTACHMENT_FLAG_LIVE } else { 0 };
    dst.push(flags);
    dst.extend_from_slice(&a.viewer_count.to_be_bytes());
    Ok(())
}

fn append_room_participant(
    dst: &mut Vec<u8>,
    p: &RoomParticipant<'_>,
    sentinel: WireError,
) -> Result<(), WireError> {
    if p.kind > ROOM_CLIENT_KIND_MAX || p.flags & !ROOM_PARTICIPANT_FLAG_MASK != 0 {
        return Err(sentinel);
    }
    dst.extend_from_slice(&p.id.to_be_bytes());
    dst.extend_from_slice(&[p.kind, p.flags]);
    append_str8(dst, p.nickname, MAX_ROOM_NICKNAME_LEN, sentinel.clone())?;
    append_str8(dst, p.identity, MAX_ROOM_IDENTITY_LEN, sentinel)
}

// --- RoomHello (0x13) --------------------------------------------------------

/// Appends a RoomHello message.
pub fn append_room_hello(dst: &mut Vec<u8>, h: &RoomHello<'_>) -> Result<(), WireError> {
    if h.protocol != ROOM_PROTOCOL_VERSION
        || h.client_kind > ROOM_CLIENT_KIND_MAX
        || h.want_caps & !ROOM_CAP_MASK != 0
    {
        return Err(WireError::BadRoomHello);
    }
    dst.extend_from_slice(&[
        VERSION,
        TYPE_ROOM_HELLO,
        h.protocol,
        h.client_kind,
        h.want_caps,
    ]);
    append_str8(
        dst,
        h.nickname,
        MAX_ROOM_NICKNAME_LEN,
        WireError::BadRoomHello,
    )
}

/// Parses a RoomHello message. Strict: exact length, known protocol version
/// and client kind, reserved capability bits zero.
pub fn parse_room_hello(msg: &[u8]) -> Result<RoomHello<'_>, WireError> {
    check_prefix(msg, TYPE_ROOM_HELLO, 6)?;
    let mut r = Reader::new(msg, WireError::BadRoomHello);
    let protocol = r.u8()?;
    if protocol != ROOM_PROTOCOL_VERSION {
        return r.fail();
    }
    let client_kind = r.u8()?;
    if client_kind > ROOM_CLIENT_KIND_MAX {
        return r.fail();
    }
    let want_caps = r.u8()?;
    if want_caps & !ROOM_CAP_MASK != 0 {
        return r.fail();
    }
    let nickname = r.str8(MAX_ROOM_NICKNAME_LEN)?;
    r.done()?;
    Ok(RoomHello {
        protocol,
        client_kind,
        want_caps,
        nickname,
    })
}

// --- RoomState (0x14) --------------------------------------------------------

/// Appends a RoomState message (relay-originated; mirrored by rule like every
/// other message).
pub fn append_room_state(dst: &mut Vec<u8>, s: &RoomState<'_>) -> Result<(), WireError> {
    if s.flags & !ROOM_STATE_FLAG_MASK != 0
        || s.caps & !ROOM_CAP_MASK != 0
        || s.code.is_empty()
        || (!s.creator_token.is_empty() && s.creator_token.len() != ROOM_CREATOR_TOKEN_SIZE)
        || s.attachments.len() > 255
        || s.participants.len() > 65535
    {
        return Err(WireError::BadRoomState);
    }
    let start = dst.len();
    dst.extend_from_slice(&[VERSION, TYPE_ROOM_STATE, s.flags, s.caps]);
    dst.extend_from_slice(&s.seq.to_be_bytes());
    dst.extend_from_slice(&s.your_id.to_be_bytes());
    append_str8(dst, s.code, MAX_ROOM_CODE_LEN, WireError::BadRoomState)?;
    append_str8(
        dst,
        s.display_name,
        MAX_ROOM_DISPLAY_NAME_LEN,
        WireError::BadRoomState,
    )?;
    dst.push(s.creator_token.len() as u8);
    dst.extend_from_slice(s.creator_token);
    dst.push(s.attachments.len() as u8);
    for a in &s.attachments {
        append_room_attachment(dst, a, WireError::BadRoomState)?;
    }
    dst.extend_from_slice(&(s.participants.len() as u16).to_be_bytes());
    for p in &s.participants {
        append_room_participant(dst, p, WireError::BadRoomState)?;
    }
    if dst.len() - start > MAX_ROOM_RECORD_SIZE {
        return Err(WireError::BadRoomState);
    }
    Ok(())
}

/// Parses a RoomState message. The returned `creator_token` and strings
/// borrow `msg`. Strict: exact length, reserved bits zero.
pub fn parse_room_state(msg: &[u8]) -> Result<RoomState<'_>, WireError> {
    check_prefix(msg, TYPE_ROOM_STATE, 15)?;
    let mut r = Reader::new(msg, WireError::BadRoomState);
    let flags = r.u8()?;
    if flags & !ROOM_STATE_FLAG_MASK != 0 {
        return r.fail();
    }
    let caps = r.u8()?;
    if caps & !ROOM_CAP_MASK != 0 {
        return r.fail();
    }
    let seq = r.u32()?;
    let your_id = r.u16()?;
    let code = r.str8(MAX_ROOM_CODE_LEN)?;
    if code.is_empty() {
        return r.fail();
    }
    let display_name = r.str8(MAX_ROOM_DISPLAY_NAME_LEN)?;
    let creator_token = r.bytes8(ROOM_CREATOR_TOKEN_SIZE)?;
    if !creator_token.is_empty() && creator_token.len() != ROOM_CREATOR_TOKEN_SIZE {
        return r.fail();
    }
    let n = r.u8()? as usize;
    let mut attachments = Vec::with_capacity(n);
    for _ in 0..n {
        attachments.push(r.attachment()?);
    }
    let m = r.u16()? as usize;
    // Each participant record is at least 6 bytes; never pre-size past what
    // the record can actually hold from an untrusted count.
    let mut participants = Vec::with_capacity(m.min(msg.len() / 6));
    for _ in 0..m {
        participants.push(r.participant()?);
    }
    r.done()?;
    Ok(RoomState {
        flags,
        caps,
        seq,
        your_id,
        code,
        display_name,
        creator_token,
        attachments,
        participants,
    })
}

// --- RoomEvent (0x15) --------------------------------------------------------

/// Appends a RoomEvent message (relay-originated; mirrored by rule). An
/// unknown kind is refused with `UnknownRoomKind`: an emitter only produces
/// what it implements.
pub fn append_room_event(dst: &mut Vec<u8>, e: &RoomEvent<'_>) -> Result<(), WireError> {
    let start = dst.len();
    dst.extend_from_slice(&[VERSION, TYPE_ROOM_EVENT]);
    dst.extend_from_slice(&e.seq.to_be_bytes());
    dst.push(e.kind);
    let res = match e.kind {
        ROOM_EVENT_PARTICIPANT_JOINED | ROOM_EVENT_PARTICIPANT_UPDATED => {
            append_room_participant(dst, &e.participant, WireError::BadRoomEvent)
        }
        ROOM_EVENT_PARTICIPANT_LEFT => {
            dst.extend_from_slice(&e.participant.id.to_be_bytes());
            Ok(())
        }
        ROOM_EVENT_ATTACHMENT_ADDED | ROOM_EVENT_ATTACHMENT_UPDATED => {
            append_room_attachment(dst, &e.attachment, WireError::BadRoomEvent)
        }
        ROOM_EVENT_ATTACHMENT_REMOVED => {
            append_broadcast_id(dst, &e.attachment.broadcast_id, WireError::BadRoomEvent).map(
                |()| {
                    dst.push(e.reason);
                },
            )
        }
        ROOM_EVENT_ROOM_ENDING => {
            dst.push(e.reason);
            Ok(())
        }
        ROOM_EVENT_COMMAND_REJECTED => {
            dst.extend_from_slice(&[e.command, e.reason]);
            append_str8(
                dst,
                e.message,
                MAX_ROOM_REJECT_MESSAGE_LEN,
                WireError::BadRoomEvent,
            )
        }
        kind => Err(WireError::UnknownRoomKind { seq: e.seq, kind }),
    };
    if res.is_err() {
        dst.truncate(start);
    }
    res
}

/// Parses a RoomEvent message. An unknown kind (including the reserved
/// chat/voice ranges) returns `UnknownRoomKind` carrying the `seq` and
/// `kind`, so a reader can skip it without losing its place in the sequence.
pub fn parse_room_event(msg: &[u8]) -> Result<RoomEvent<'_>, WireError> {
    check_prefix(msg, TYPE_ROOM_EVENT, 7)?;
    let mut r = Reader::new(msg, WireError::BadRoomEvent);
    let seq = r.u32()?;
    let kind = r.u8()?;
    let mut e = RoomEvent {
        seq,
        kind,
        ..RoomEvent::default()
    };
    match kind {
        ROOM_EVENT_PARTICIPANT_JOINED | ROOM_EVENT_PARTICIPANT_UPDATED => {
            e.participant = r.participant()?;
        }
        ROOM_EVENT_PARTICIPANT_LEFT => {
            e.participant.id = r.u16()?;
        }
        ROOM_EVENT_ATTACHMENT_ADDED | ROOM_EVENT_ATTACHMENT_UPDATED => {
            e.attachment = r.attachment()?;
        }
        ROOM_EVENT_ATTACHMENT_REMOVED => {
            e.attachment.broadcast_id = r.broadcast_id()?;
            e.reason = r.u8()?;
        }
        ROOM_EVENT_ROOM_ENDING => {
            e.reason = r.u8()?;
        }
        ROOM_EVENT_COMMAND_REJECTED => {
            e.command = r.u8()?;
            e.reason = r.u8()?;
            e.message = r.str8(MAX_ROOM_REJECT_MESSAGE_LEN)?;
        }
        kind => return Err(WireError::UnknownRoomKind { seq, kind }),
    }
    r.done()?;
    Ok(e)
}

// --- RoomCommand (0x16) ------------------------------------------------------

/// Appends a RoomCommand message. An unknown kind is refused with
/// `UnknownRoomKind`.
pub fn append_room_command(dst: &mut Vec<u8>, c: &RoomCommand<'_>) -> Result<(), WireError> {
    let start = dst.len();
    dst.extend_from_slice(&[VERSION, TYPE_ROOM_COMMAND, c.kind]);
    let res = match c.kind {
        ROOM_COMMAND_ATTACH => {
            if c.resume_token.len() != RESUME_TOKEN_SIZE {
                Err(WireError::BadRoomCommand)
            } else {
                append_broadcast_id(dst, &c.broadcast_id, WireError::BadRoomCommand).and_then(
                    |()| {
                        dst.push(c.resume_token.len() as u8);
                        dst.extend_from_slice(c.resume_token);
                        append_str8(dst, c.label, MAX_ROOM_LABEL_LEN, WireError::BadRoomCommand)
                    },
                )
            }
        }
        ROOM_COMMAND_DETACH => append_broadcast_id(dst, &c.broadcast_id, WireError::BadRoomCommand),
        ROOM_COMMAND_SET_NICKNAME => append_str8(
            dst,
            c.nickname,
            MAX_ROOM_NICKNAME_LEN,
            WireError::BadRoomCommand,
        ),
        ROOM_COMMAND_END_ROOM | ROOM_COMMAND_RESYNC => Ok(()),
        kind => Err(WireError::UnknownRoomKind { seq: 0, kind }),
    };
    if res.is_err() {
        dst.truncate(start);
    }
    res
}

/// Parses a RoomCommand message. The returned `resume_token` borrows `msg`.
/// An unknown kind returns `UnknownRoomKind` with `kind` filled in (`seq` is
/// 0 — commands carry none), so a relay can answer ROOM_REJECT_UNSUPPORTED.
pub fn parse_room_command(msg: &[u8]) -> Result<RoomCommand<'_>, WireError> {
    check_prefix(msg, TYPE_ROOM_COMMAND, 3)?;
    let mut r = Reader::new(msg, WireError::BadRoomCommand);
    let kind = r.u8()?;
    let mut c = RoomCommand {
        kind,
        ..RoomCommand::default()
    };
    match kind {
        ROOM_COMMAND_ATTACH => {
            c.broadcast_id = r.broadcast_id()?;
            let tok = r.bytes8(RESUME_TOKEN_SIZE)?;
            if tok.len() != RESUME_TOKEN_SIZE {
                return r.fail();
            }
            c.resume_token = tok;
            c.label = r.str8(MAX_ROOM_LABEL_LEN)?;
        }
        ROOM_COMMAND_DETACH => {
            c.broadcast_id = r.broadcast_id()?;
        }
        ROOM_COMMAND_SET_NICKNAME => {
            c.nickname = r.str8(MAX_ROOM_NICKNAME_LEN)?;
        }
        ROOM_COMMAND_END_ROOM | ROOM_COMMAND_RESYNC => {}
        kind => return Err(WireError::UnknownRoomKind { seq: 0, kind }),
    }
    r.done()?;
    Ok(c)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn broadcast_id_normalizes_case_and_rejects_the_excluded_symbols() {
        assert_eq!(normalize_broadcast_id(b"abcdef").as_deref(), Some("ABCDEF"));
        assert_eq!(normalize_broadcast_id(b"K7XQ2M").as_deref(), Some("K7XQ2M"));
        assert!(normalize_broadcast_id(b"0OIL11").is_none());
        assert!(normalize_broadcast_id(b"ABCDE").is_none());
        assert!(normalize_broadcast_id(b"ABCDEFG").is_none());
    }

    #[test]
    fn failed_append_leaves_dst_untouched() {
        let mut dst = vec![0xaa];
        assert_eq!(
            append_room_event(
                &mut dst,
                &RoomEvent {
                    kind: 0x4f,
                    ..RoomEvent::default()
                }
            ),
            Err(WireError::UnknownRoomKind { seq: 0, kind: 0x4f })
        );
        assert_eq!(dst, vec![0xaa]);
        assert_eq!(
            append_room_command(
                &mut dst,
                &RoomCommand {
                    kind: 0x5f,
                    ..RoomCommand::default()
                }
            ),
            Err(WireError::UnknownRoomKind { seq: 0, kind: 0x5f })
        );
        assert_eq!(dst, vec![0xaa]);
    }

    #[test]
    fn room_state_rejects_more_attachments_than_a_byte_counts() {
        let attachments = vec![
            RoomAttachment {
                broadcast_id: "ABCDEF".into(),
                ..RoomAttachment::default()
            };
            256
        ];
        let s = RoomState {
            code: "x",
            attachments,
            ..RoomState::default()
        };
        assert_eq!(
            append_room_state(&mut Vec::new(), &s),
            Err(WireError::BadRoomState)
        );
    }
}
