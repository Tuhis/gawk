//! Server-initiated uni-stream dispatch, mirroring the Go engine's reader
//! (gawk-broadcast/internal/engine/engine.go): streams are read bounded and
//! dispatched **by wire type, never by arrival order** — webtransport stacks
//! accept incoming streams in nondeterministic order, and the resume token
//! beat the announce in about half of real dials (docs/22 finding 9).
//! Unknown types are ignored so a newer relay can't break this client.

use gawk_wire as wire;

/// Read bound for any server uni stream. The largest legitimate message is
/// a 255-byte announce/token (3 + 255 = 258); capabilities are 5 and the
/// telemetry hello 35.
pub const SERVER_STREAM_READ_LIMIT: usize = 258;
/// Deadline for reading one server stream to completion.
pub const SERVER_STREAM_READ_TIMEOUT_MS: u64 = 10_000;

/// One dispatched server message, owned (the stream buffer dies with the
/// read).
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum ServerMessage {
    Announce(String),
    /// Hex-encoded for persistence and the `resume` query param, exactly as
    /// both Linux shells store it.
    ResumeToken(String),
    Capabilities(wire::RelayCapabilities),
    TelemetryHello {
        enabled: bool,
        report_interval_ms: u16,
        token: Vec<u8>,
        /// Hex-encoded obfuscated broadcast key (never the joinable raw ID).
        broadcast_key_hex: String,
    },
    /// A type this build does not know — ignored, never an error.
    Unknown(u8),
}

fn hex(bytes: &[u8]) -> String {
    bytes.iter().map(|b| format!("{b:02x}")).collect()
}

/// Classifies and parses one complete server uni-stream payload. A malformed
/// message of a KNOWN type is an error (strict parsing); the caller logs and
/// ignores that stream.
pub fn dispatch_server_message(msg: &[u8]) -> Result<ServerMessage, wire::WireError> {
    let (_, msg_type) = wire::peek_type(msg)?;
    match msg_type {
        wire::TYPE_BROADCAST_ANNOUNCE => Ok(ServerMessage::Announce(
            wire::parse_broadcast_announce(msg)?.to_owned(),
        )),
        wire::TYPE_RESUME_TOKEN => Ok(ServerMessage::ResumeToken(hex(wire::parse_resume_token(
            msg,
        )?))),
        wire::TYPE_RELAY_CAPABILITIES => Ok(ServerMessage::Capabilities(
            wire::parse_relay_capabilities(msg)?,
        )),
        wire::TYPE_TELEMETRY_HELLO => {
            let h = wire::parse_telemetry_hello(msg)?;
            Ok(ServerMessage::TelemetryHello {
                enabled: h.enabled,
                report_interval_ms: h.report_interval_ms,
                token: h.token.to_vec(),
                broadcast_key_hex: hex(h.broadcast_key),
            })
        }
        other => Ok(ServerMessage::Unknown(other)),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn dispatches_by_type_regardless_of_arrival_order() {
        // Simulate the adverse order docs/22 finding 9 measured: token first,
        // then announce, then capabilities — each classified independently.
        let mut token = Vec::new();
        wire::append_resume_token(&mut token, &[0xde, 0xad]).unwrap();
        let mut announce = Vec::new();
        wire::append_broadcast_announce(&mut announce, "K7XQ2M").unwrap();
        let mut caps = Vec::new();
        wire::append_relay_capabilities(
            &mut caps,
            &wire::RelayCapabilities {
                flags: wire::CAP_PARITY_CHUNKS,
                parity_level: 2,
            },
        )
        .unwrap();

        assert_eq!(
            dispatch_server_message(&token).unwrap(),
            ServerMessage::ResumeToken("dead".into())
        );
        assert_eq!(
            dispatch_server_message(&announce).unwrap(),
            ServerMessage::Announce("K7XQ2M".into())
        );
        assert!(matches!(
            dispatch_server_message(&caps).unwrap(),
            ServerMessage::Capabilities(c) if c.parity_level == 2
        ));
    }

    #[test]
    fn unknown_types_are_ignored_not_errors() {
        assert_eq!(
            dispatch_server_message(&[0x01, 0x7f, 0xaa]).unwrap(),
            ServerMessage::Unknown(0x7f)
        );
    }

    #[test]
    fn malformed_known_types_are_errors() {
        // An announce with a character outside the alphabet.
        let mut bad = Vec::new();
        wire::append_broadcast_announce(&mut bad, "OOOOOO").unwrap();
        assert!(dispatch_server_message(&bad).is_err());
    }
}
