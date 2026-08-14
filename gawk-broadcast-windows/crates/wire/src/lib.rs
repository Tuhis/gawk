//! The gawk wire format — fourth mirror (docs/38 D4).
//!
//! The source of truth is `gawk-server/wire/wire.go` (plus `parity.go` and
//! `stripe.go`); the other mirrors are the TypeScript `wire.ts` and the Go
//! broadcaster's `internal/wirecheck`. This crate is a hand-written
//! reimplementation with the golden vectors deliberately RESTATED in its
//! tests, never shared or imported: an exported fixture the sides shared
//! could be edited once and stay green everywhere, defeating the purpose.
//!
//! Semantics mirror the Go package exactly:
//! - Every message starts with byte 0 = [`VERSION`], byte 1 = message type.
//! - All multi-byte integers are big-endian.
//! - Parsers are strict and never copy: fixed-size messages reject any other
//!   length, reserved bits are rejected rather than masked, and returned
//!   payload slices borrow the input.
//!
//! One documented divergence: codec strings are returned as `&str`, so a
//! non-UTF-8 codec string is a parse error here where Go would carry the raw
//! bytes. Every codec string gawk produces is ASCII.

#![forbid(unsafe_code)]

mod chunking;
mod error;
mod messages;
mod parity;
mod stripe;

pub use chunking::{ChunkBudget, split_frame};
pub use error::WireError;
pub use messages::*;
pub use parity::{
    CAP_PARITY_CHUNKS, CAP_STRIPED_DELIVERY, MAX_PARITY_DATA_CHUNKS, MAX_PARITY_SYMBOLS,
    PARITY_CHUNK_HEADER_SIZE, ParityChunkHeader, RELAY_CAPABILITIES_SIZE, RelayCapabilities,
    append_parity_chunk, append_relay_capabilities, compute_parity, parse_parity_chunk,
    parse_relay_capabilities,
};
pub use stripe::{
    MAX_STRIPE_LEGS, STRIPE_STATE_SIZE, StripeState, append_stripe_state, parse_stripe_state,
    stripe_ordinal,
};

/// The only wire protocol version currently defined; byte 0 of every message.
pub const VERSION: u8 = 0x01;

// Message types, occupying byte 1 of every datagram (or byte 1 of a
// unidirectional-stream payload). The allocation map lives in
// gawk-server/wire/wire.go — new types are allocated THERE first.
pub const TYPE_VIDEO_CHUNK: u8 = 0x01;
pub const TYPE_DECODER_CONFIG: u8 = 0x02;
pub const TYPE_BROADCAST_ANNOUNCE: u8 = 0x03;
pub const TYPE_STREAM_FRAME: u8 = 0x04;
pub const TYPE_TIME_SYNC: u8 = 0x05;
pub const TYPE_CLOCK_MAPPING: u8 = 0x06;
pub const TYPE_AUDIO_FRAME: u8 = 0x07;
pub const TYPE_AUDIO_CONFIG: u8 = 0x08;
pub const TYPE_RESUME_TOKEN: u8 = 0x09;
pub const TYPE_RELIABLE_CARRIER: u8 = 0x0A;
pub const TYPE_VIEWER_COUNT: u8 = 0x0B;
pub const TYPE_DELIVERY_ACK: u8 = 0x0C;
pub const TYPE_TELEMETRY_HELLO: u8 = 0x0D;
pub const TYPE_PARITY_CHUNK: u8 = 0x0E;
pub const TYPE_RELAY_CAPABILITIES: u8 = 0x0F;
pub const TYPE_STRIPE_STATE: u8 = 0x10;
pub const TYPE_RELAY_IDENTITY: u8 = 0x11;
pub const TYPE_TELEMETRY_ENDPOINT: u8 = 0x12;

// Size constants. A change to any of these is a protocol change, not a
// tuning knob — the constants test pins every one.
pub const MAX_DATAGRAM_SIZE: usize = 1200;
pub const VIDEO_CHUNK_HEADER_SIZE: usize = 20;
pub const MAX_CHUNK_PAYLOAD: usize = MAX_DATAGRAM_SIZE - VIDEO_CHUNK_HEADER_SIZE;
pub const MAX_CHUNK_COUNT: u16 = 3000;
pub const STREAM_FRAME_HEADER_SIZE: usize = 24;
pub const MAX_KEYFRAME_BYTES: usize = 8 << 20;
pub const TIME_SYNC_SIZE: usize = 18;
pub const CLOCK_MAPPING_SIZE: usize = 10;
pub const VIEWER_COUNT_SIZE: usize = 6;
pub const CARRIER_PROLOGUE_SIZE: usize = 2;
pub const CARRIER_RECORD_HEADER_SIZE: usize = 2;
pub const AUDIO_FRAME_HEADER_SIZE: usize = 16;
pub const MAX_AUDIO_PAYLOAD: usize = MAX_DATAGRAM_SIZE - AUDIO_FRAME_HEADER_SIZE;
pub const TELEMETRY_HELLO_SIZE: usize = 35;
pub const TELEMETRY_SESSION_TOKEN_SIZE: usize = 24;
pub const TELEMETRY_BROADCAST_KEY_SIZE: usize = 6;
pub const DELIVERY_ACK_SIZE: usize = 5;
/// RelayIdentity (0x11, R37): a release version is a short string ("1.42.0").
pub const MAX_RELAY_IDENTITY_VERSION_LEN: usize = 32;
/// RelayIdentity (0x11, R37): a display name is one label, not a paragraph.
pub const MAX_RELAY_IDENTITY_NAME_LEN: usize = 64;
/// TelemetryEndpoint (0x12, R37): real ingest URLs are well under this.
pub const MAX_TELEMETRY_ENDPOINT_URL_LEN: usize = 512;

// WebTransport application close codes. Doc comments with the full semantics
// live on the Go constants; the one-line summaries here are what the resume
// supervisor's terminal/resumable split is built on (docs/38 D5).
/// Terminal for viewers: the broadcast was garbage-collected.
pub const CLOSE_CODE_BROADCAST_ENDED: u32 = 4000;
/// Non-terminal: the relay evicted an unresponsive subscriber; reconnect.
pub const CLOSE_CODE_SUBSCRIBER_UNRESPONSIVE: u32 = 4001;
/// Non-terminal and explicitly fast: planned drain, reconnect immediately.
pub const CLOSE_CODE_SERVER_DRAINING: u32 = 4002;
/// Internal edge sessions only; never sent to clients like this one.
pub const CLOSE_CODE_ORIGIN_MOVED: u32 = 4003;
/// Terminal for resume: a newer publisher session claimed this broadcast ID
/// with a verified token. The deposed client must NOT auto-resume back.
pub const CLOSE_CODE_PUBLISHER_SUPERSEDED: u32 = 4004;

/// The broadcast ID alphabet (gawk-server/internal/broadcastid): 31 symbols,
/// no `0 O 1 I L`. IDs are 6 chars; announce parsing validates against this.
pub const BROADCAST_ID_ALPHABET: &str = "23456789ABCDEFGHJKMNPQRSTUVWXYZ";

pub(crate) const FLAG_KEYFRAME: u8 = 0x01;
pub(crate) const FLAG_TELEMETRY_ENABLED: u8 = 0x01;

/// Reports the version and message type bytes of a datagram without
/// validating them; parsers perform validation. Errors only if the datagram
/// is shorter than the 2-byte common prefix.
pub fn peek_type(dgram: &[u8]) -> Result<(u8, u8), WireError> {
    if dgram.len() < 2 {
        return Err(WireError::ShortDatagram {
            len: dgram.len(),
            need: 2,
        });
    }
    Ok((dgram[0], dgram[1]))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn peek_type_reads_prefix_without_validating() {
        assert_eq!(peek_type(&[0x01, 0x0e, 0xff]).unwrap(), (0x01, 0x0e));
        // Garbage version/type still peeks — validation is the parsers' job.
        assert_eq!(peek_type(&[0x7f, 0x99]).unwrap(), (0x7f, 0x99));
        assert!(matches!(
            peek_type(&[0x01]),
            Err(WireError::ShortDatagram { len: 1, need: 2 })
        ));
    }
}
