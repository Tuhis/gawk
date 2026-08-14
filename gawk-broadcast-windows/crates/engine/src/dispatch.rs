//! Server-initiated uni-stream dispatch, mirroring the Go engine's reader
//! (gawk-broadcast/internal/engine/engine.go): streams are read bounded and
//! dispatched **by wire type, never by arrival order** — webtransport stacks
//! accept incoming streams in nondeterministic order, and the resume token
//! beat the announce in about half of real dials (docs/22 finding 9).
//! Unknown types are ignored so a newer relay can't break this client.

use gawk_wire as wire;

/// Read bound for any server uni stream, matching the Go engine's
/// `serverMessageReadLimit`. The largest message the relay sends a publisher
/// is a TelemetryEndpoint (R37, 0x12): 5 header bytes plus a maximal URL
/// (announce/token top out at 258, capabilities are 5, the telemetry hello
/// 35). Anything larger is a misbehaving or hostile relay and must not be
/// able to grow our heap; the one thing the cap can clip is reserved
/// extension bytes past a maximal payload — which parsers ignore anyway
/// (docs/40 §4.9).
pub const SERVER_STREAM_READ_LIMIT: usize = 5 + wire::MAX_TELEMETRY_ENDPOINT_URL_LEN;
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
    /// The relay fleet's advertised telemetry ingest URL (0x12, R37 docs/40
    /// §4.10) — already validated as absolute https by the wire parser.
    TelemetryEndpoint(String),
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
        wire::TYPE_TELEMETRY_ENDPOINT => Ok(ServerMessage::TelemetryEndpoint(
            wire::parse_telemetry_endpoint(msg)?.to_owned(),
        )),
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
    fn telemetry_endpoint_dispatches_and_malformed_ones_error() {
        let mut msg = Vec::new();
        wire::append_telemetry_endpoint(&mut msg, "https://gawk.example.com/ingest").unwrap();
        assert_eq!(
            dispatch_server_message(&msg).unwrap(),
            ServerMessage::TelemetryEndpoint("https://gawk.example.com/ingest".into())
        );
        // A malformed KNOWN type is an error the session logs and ignores —
        // the client never adopts a destination that failed validation
        // (docs/40 §4.10: degrades to "no advertised URL").
        let mut flagged = msg.clone();
        flagged[2] = 0x80;
        assert!(dispatch_server_message(&flagged).is_err());
    }

    // The read bound must fit the largest legitimate message or a current
    // relay's 0x12 would be dropped as oversize.
    #[test]
    fn read_limit_covers_a_maximal_telemetry_endpoint() {
        let url = format!(
            "https://x.example/{}",
            "p".repeat(wire::MAX_TELEMETRY_ENDPOINT_URL_LEN - "https://x.example/".len())
        );
        let mut msg = Vec::new();
        wire::append_telemetry_endpoint(&mut msg, &url).unwrap();
        assert!(msg.len() <= SERVER_STREAM_READ_LIMIT);
        assert_eq!(
            dispatch_server_message(&msg).unwrap(),
            ServerMessage::TelemetryEndpoint(url)
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
