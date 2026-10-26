//! Room voice through the Mumble bridge (R79, docs/80 D20), mirroring
//! gawk-server/wire/voice.go function for function. The layout comment at the
//! top of that file is the spec; this is a restatement, never an import.
//!
//! Four data-plane messages travel on a browser's voice session with
//! gawk-mumble-bridge (CONNECT /voice on the bridge's public listener), never
//! on a relay session:
//!
//! - VoiceHello, VoiceState and VoiceControl are RECORDS on the one
//!   bidirectional stream the browser opens (the room record framing:
//!   uint16 length ‖ Version ‖ Type ‖ payload, [`crate::append_room_record`] /
//!   [`crate::parse_room_record_length`]).
//! - VoiceFrame is a DATAGRAM in both directions: one Opus packet each, like
//!   AudioFrame. Voice has no keyframes and no reassembly.
//!
//! The native clients do not implement voice (docs/80 OD11); these are here
//! because every mirror carries every message.
//!
//! Layouts, after the common Version ‖ Type prefix:
//!
//! ```text
//! 0x1A VoiceHello (browser → bridge, the first record on the stream):
//!   byte 2   uint8 protocol      VOICE_PROTOCOL_VERSION
//!   byte 3   uint8 flags         VOICE_HELLO_FLAG_*; bits 3-7 reserved (0)
//!   4-5      uint16 ticketLen    1..MAX_VOICE_TICKET_LEN
//!   6+       ticket              opaque here
//!
//! 0x1B VoiceFrame (datagram, both directions; VOICE_FRAME_HEADER_SIZE bytes
//! of header, then the payload):
//!   byte 2   uint8 flags         VOICE_FRAME_FLAG_*; bits 1-7 reserved (0)
//!   byte 3   uint8 target        Mumble voice target 0..MAX_VOICE_TARGET
//!   4-7      uint32 speaker      the speaker's Mumble session ID; 0 from a
//!                                browser
//!   8-11     uint32 seq          Mumble sequence number, in 10 ms units
//!   12+      payload             one Opus packet, 1..MAX_VOICE_PAYLOAD bytes;
//!                                empty ONLY with the end-of-transmission flag
//!
//! 0x1C VoiceState (bridge → browser record, whenever it changes):
//!   byte 2   uint8 flags         VOICE_STATE_FLAG_*; bits 6-7 reserved (0)
//!   3-6      uint32 session      the browser's own Mumble session ID, 0
//!                                until its Mumble connection is up
//!   7        uint8 nameLen       0..MAX_VOICE_NAME_LEN, then the name (UTF-8)
//!
//! 0x1D VoiceControl (browser → bridge record):
//!   byte 2   uint8 flags         VOICE_CONTROL_FLAG_*; bits 2-7 reserved (0)
//! ```

use crate::error::WireError;
use crate::room::{Reader, append_str8, check_prefix};
use crate::{
    MAX_DATAGRAM_SIZE, TYPE_VOICE_CONTROL, TYPE_VOICE_FRAME, TYPE_VOICE_HELLO, TYPE_VOICE_STATE,
    VERSION,
};

// --- constants ---------------------------------------------------------------

/// The voice protocol version a VoiceHello carries, independent of the
/// datagram [`VERSION`] byte.
pub const VOICE_PROTOCOL_VERSION: u8 = 1;
/// The fixed VoiceFrame header, including the common 2-byte prefix.
pub const VOICE_FRAME_HEADER_SIZE: usize = 12;
/// The largest Opus packet a VoiceFrame carries: the datagram limit.
pub const MAX_VOICE_PAYLOAD: usize = MAX_DATAGRAM_SIZE - VOICE_FRAME_HEADER_SIZE;
/// The largest Mumble voice target (5 bits).
pub const MAX_VOICE_TARGET: u8 = 31;
/// Bounds the opaque ticket in a VoiceHello and a VoiceTicket event.
pub const MAX_VOICE_TICKET_LEN: usize = 512;
/// Bounds a Mumble user name on the voice wire (VoiceState, the roster).
pub const MAX_VOICE_NAME_LEN: usize = 64;
/// Bounds the bridge URL in a VoiceTicket event.
pub const MAX_VOICE_URL_LEN: usize = 512;
/// Bounds one VoiceRosterUpdated event; keeps it under MAX_ROOM_RECORD_SIZE.
pub const MAX_VOICE_ROSTER_ENTRIES: usize = 200;

// VoiceHello flags.
/// Join muted.
pub const VOICE_HELLO_FLAG_SELF_MUTE: u8 = 0x01;
/// Join deafened.
pub const VOICE_HELLO_FLAG_SELF_DEAF: u8 = 0x02;
/// This browser never sends voice (docs/80 D21).
pub const VOICE_HELLO_FLAG_LISTEN_ONLY: u8 = 0x04;
const VOICE_HELLO_FLAG_MASK: u8 =
    VOICE_HELLO_FLAG_SELF_MUTE | VOICE_HELLO_FLAG_SELF_DEAF | VOICE_HELLO_FLAG_LISTEN_ONLY;

// VoiceFrame flags.
/// The last frame of a talk spurt (Mumble's end-of-transmission bit).
pub const VOICE_FRAME_FLAG_END: u8 = 0x01;
const VOICE_FRAME_FLAG_MASK: u8 = VOICE_FRAME_FLAG_END;

// VoiceState flags.
/// Murmur (an admin or an ACL) muted this user.
pub const VOICE_STATE_FLAG_SERVER_MUTE: u8 = 0x01;
/// Murmur deafened this user.
pub const VOICE_STATE_FLAG_SERVER_DEAF: u8 = 0x02;
/// Murmur suppressed this user (no Speak permission in the channel).
pub const VOICE_STATE_FLAG_SUPPRESSED: u8 = 0x04;
/// The user's own self-mute as murmur holds it.
pub const VOICE_STATE_FLAG_SELF_MUTE: u8 = 0x08;
/// The user's own self-deafen as murmur holds it.
pub const VOICE_STATE_FLAG_SELF_DEAF: u8 = 0x10;
/// The bridged Mumble connection is up and in the room's channel.
pub const VOICE_STATE_FLAG_CONNECTED: u8 = 0x20;
const VOICE_STATE_FLAG_MASK: u8 = 0x3f;

// VoiceControl flags.
pub const VOICE_CONTROL_FLAG_SELF_MUTE: u8 = 0x01;
pub const VOICE_CONTROL_FLAG_SELF_DEAF: u8 = 0x02;
const VOICE_CONTROL_FLAG_MASK: u8 = VOICE_CONTROL_FLAG_SELF_MUTE | VOICE_CONTROL_FLAG_SELF_DEAF;

// --- structs -----------------------------------------------------------------

/// The parsed contents of a VoiceHello record.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub struct VoiceHello<'a> {
    pub protocol: u8,
    /// `VOICE_HELLO_FLAG_*` bitmap.
    pub flags: u8,
    /// Borrows the input on parse.
    pub ticket: &'a [u8],
}

/// The parsed header of a VoiceFrame datagram.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub struct VoiceFrameHeader {
    pub end: bool,
    pub target: u8,
    pub speaker: u32,
    pub seq: u32,
}

/// The parsed contents of a VoiceState record.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub struct VoiceState<'a> {
    /// `VOICE_STATE_FLAG_*` bitmap.
    pub flags: u8,
    pub session: u32,
    pub name: &'a str,
}

// --- VoiceHello (0x1A) -------------------------------------------------------

/// Appends a VoiceHello message.
pub fn append_voice_hello(dst: &mut Vec<u8>, h: &VoiceHello<'_>) -> Result<(), WireError> {
    if h.protocol != VOICE_PROTOCOL_VERSION
        || h.flags & !VOICE_HELLO_FLAG_MASK != 0
        || h.ticket.is_empty()
        || h.ticket.len() > MAX_VOICE_TICKET_LEN
    {
        return Err(WireError::BadVoiceHello);
    }
    dst.extend_from_slice(&[VERSION, TYPE_VOICE_HELLO, h.protocol, h.flags]);
    dst.extend_from_slice(&(h.ticket.len() as u16).to_be_bytes());
    dst.extend_from_slice(h.ticket);
    Ok(())
}

/// Parses a VoiceHello message. Strict: exact length, known protocol,
/// reserved bits zero, a non-empty ticket within bounds.
pub fn parse_voice_hello(msg: &[u8]) -> Result<VoiceHello<'_>, WireError> {
    check_prefix(msg, TYPE_VOICE_HELLO, 6)?;
    let (protocol, flags) = (msg[2], msg[3]);
    if protocol != VOICE_PROTOCOL_VERSION || flags & !VOICE_HELLO_FLAG_MASK != 0 {
        return Err(WireError::BadVoiceHello);
    }
    let n = u16::from_be_bytes([msg[4], msg[5]]) as usize;
    if n == 0 || n > MAX_VOICE_TICKET_LEN || 6 + n != msg.len() {
        return Err(WireError::BadVoiceHello);
    }
    Ok(VoiceHello {
        protocol,
        flags,
        ticket: &msg[6..],
    })
}

// --- VoiceFrame (0x1B) -------------------------------------------------------

/// Appends a VoiceFrame datagram. The payload may be empty only when
/// `h.end` is set.
pub fn append_voice_frame(
    dst: &mut Vec<u8>,
    h: &VoiceFrameHeader,
    payload: &[u8],
) -> Result<(), WireError> {
    if payload.len() > MAX_VOICE_PAYLOAD
        || (payload.is_empty() && !h.end)
        || h.target > MAX_VOICE_TARGET
    {
        return Err(WireError::BadVoiceFrame);
    }
    let flags = if h.end { VOICE_FRAME_FLAG_END } else { 0 };
    dst.extend_from_slice(&[VERSION, TYPE_VOICE_FRAME, flags, h.target]);
    dst.extend_from_slice(&h.speaker.to_be_bytes());
    dst.extend_from_slice(&h.seq.to_be_bytes());
    dst.extend_from_slice(payload);
    Ok(())
}

/// Parses a VoiceFrame datagram. The payload borrows `dgram`.
pub fn parse_voice_frame(dgram: &[u8]) -> Result<(VoiceFrameHeader, &[u8]), WireError> {
    check_prefix(dgram, TYPE_VOICE_FRAME, VOICE_FRAME_HEADER_SIZE)?;
    let flags = dgram[2];
    if flags & !VOICE_FRAME_FLAG_MASK != 0 {
        return Err(WireError::BadVoiceFrame);
    }
    let h = VoiceFrameHeader {
        end: flags & VOICE_FRAME_FLAG_END != 0,
        target: dgram[3],
        speaker: u32::from_be_bytes([dgram[4], dgram[5], dgram[6], dgram[7]]),
        seq: u32::from_be_bytes([dgram[8], dgram[9], dgram[10], dgram[11]]),
    };
    if h.target > MAX_VOICE_TARGET {
        return Err(WireError::BadVoiceFrame);
    }
    let payload = &dgram[VOICE_FRAME_HEADER_SIZE..];
    if payload.is_empty() && !h.end {
        return Err(WireError::BadVoiceFrame);
    }
    Ok((h, payload))
}

// --- VoiceState (0x1C) -------------------------------------------------------

/// Appends a VoiceState message.
pub fn append_voice_state(dst: &mut Vec<u8>, s: &VoiceState<'_>) -> Result<(), WireError> {
    if s.flags & !VOICE_STATE_FLAG_MASK != 0 || s.name.len() > MAX_VOICE_NAME_LEN {
        return Err(WireError::BadVoiceState);
    }
    dst.extend_from_slice(&[VERSION, TYPE_VOICE_STATE, s.flags]);
    dst.extend_from_slice(&s.session.to_be_bytes());
    append_str8(dst, s.name, MAX_VOICE_NAME_LEN, WireError::BadVoiceState)
}

/// Parses a VoiceState message. Strict: exact length, reserved bits zero,
/// the name bounded and UTF-8.
pub fn parse_voice_state(msg: &[u8]) -> Result<VoiceState<'_>, WireError> {
    check_prefix(msg, TYPE_VOICE_STATE, 8)?;
    let mut r = Reader::new(msg, WireError::BadVoiceState);
    let flags = r.u8()?;
    if flags & !VOICE_STATE_FLAG_MASK != 0 {
        return r.fail();
    }
    let session = r.u32()?;
    let name = r.str8(MAX_VOICE_NAME_LEN)?;
    r.done()?;
    Ok(VoiceState {
        flags,
        session,
        name,
    })
}

// --- VoiceControl (0x1D) -----------------------------------------------------

/// Appends a VoiceControl message carrying `flags`.
pub fn append_voice_control(dst: &mut Vec<u8>, flags: u8) -> Result<(), WireError> {
    if flags & !VOICE_CONTROL_FLAG_MASK != 0 {
        return Err(WireError::BadVoiceControl);
    }
    dst.extend_from_slice(&[VERSION, TYPE_VOICE_CONTROL, flags]);
    Ok(())
}

/// Parses a VoiceControl message and returns its flags.
pub fn parse_voice_control(msg: &[u8]) -> Result<u8, WireError> {
    check_prefix(msg, TYPE_VOICE_CONTROL, 3)?;
    if msg.len() != 3 {
        return Err(WireError::BadVoiceControl);
    }
    if msg[2] & !VOICE_CONTROL_FLAG_MASK != 0 {
        return Err(WireError::BadVoiceControl);
    }
    Ok(msg[2])
}

/// Cuts a Mumble name to [`MAX_VOICE_NAME_LEN`] bytes on a char boundary.
/// Producers call it before they put a name on the wire. Go's
/// `TruncateVoiceName` also replaces invalid UTF-8 first; a `&str` is valid
/// by construction, so that half has nothing to do here.
pub fn truncate_voice_name(s: &str) -> &str {
    if s.len() <= MAX_VOICE_NAME_LEN {
        return s;
    }
    let mut end = MAX_VOICE_NAME_LEN;
    while !s.is_char_boundary(end) {
        end -= 1;
    }
    &s[..end]
}
