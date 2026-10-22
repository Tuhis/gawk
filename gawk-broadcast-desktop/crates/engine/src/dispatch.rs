//! Server-initiated uni-stream dispatch, mirroring the Go engine's reader
//! (gawk-server/internal/pubsim/engine/engine.go): streams are read bounded and
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
///
/// R74 (docs/76 §9): a home relay's LocalRelayOffer (0x19) can be larger:
/// four candidates of maximal URLs with two pins each, about 1.4 KB. The
/// bound is the larger of the two.
pub const SERVER_STREAM_READ_LIMIT: usize = {
    let telemetry = 5 + wire::MAX_TELEMETRY_ENDPOINT_URL_LEN;
    let offer = MAX_LOCAL_RELAY_OFFER_LEN;
    if offer > telemetry { offer } else { telemetry }
};

/// The largest LocalRelayOffer (0x19) the wire allows.
pub const MAX_LOCAL_RELAY_OFFER_LEN: usize = 3
    + 1
    + wire::MAX_LOCAL_RELAY_HANDLE_LEN
    + 1
    + wire::MAX_RELAY_IDENTITY_NAME_LEN
    + 4
    + 1
    + wire::MAX_LOCAL_RELAY_CANDIDATES
        * (2 + wire::MAX_LOCAL_RELAY_URL_LEN + 1 + wire::MAX_LOCAL_RELAY_HASHES * 32);
// A screening relay's RelayIdentity (R40, docs/71 D13) — header, maximal
// version and name, the notices byte — fits the bound with room to spare.
const _: () = assert!(
    4 + wire::MAX_RELAY_IDENTITY_VERSION_LEN + 1 + wire::MAX_RELAY_IDENTITY_NAME_LEN
        < SERVER_STREAM_READ_LIMIT
);
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
    /// The notices byte of a RelayIdentity (0x11) a relay sent on the
    /// publish session (R40, docs/71 D13): it does so only when it has a
    /// notice to give. Builds before R40 dispatch this as `Unknown(0x11)`
    /// and carry on — the CF1 compatibility claim.
    RelayNotices(u8),
    /// R74 (docs/76 item 45): a PoP's LocalRelayInfo (0x18) — the name, the
    /// link, and on a publish session whether this broadcast is on the
    /// party wall. Sent again when that changes.
    LocalRelayInfo(crate::localrelay::LocalRelay),
    /// R74 (docs/76 item 42): a home relay suggests a party's local relay
    /// to this client (0x19). An offer with no usable candidate is dropped.
    LocalRelayOffer(crate::localrelay::Offer),
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
        wire::TYPE_RELAY_IDENTITY => Ok(ServerMessage::RelayNotices(
            wire::parse_relay_identity(msg)?.notices,
        )),
        wire::TYPE_LOCAL_RELAY_INFO => Ok(ServerMessage::LocalRelayInfo(
            crate::localrelay::LocalRelay::from_wire(&wire::parse_local_relay_info(msg)?),
        )),
        wire::TYPE_LOCAL_RELAY_OFFER => {
            let m = wire::parse_local_relay_offer(msg)?;
            Ok(match crate::localrelay::Offer::from_wire(&m) {
                Some(o) => ServerMessage::LocalRelayOffer(o),
                None => ServerMessage::Unknown(wire::TYPE_LOCAL_RELAY_OFFER),
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

    // R40 (docs/71 D13): a screening relay's identity on the publish session
    // dispatches to its notices; one without the byte reads as no notices.
    #[test]
    fn relay_identity_dispatches_to_its_notices() {
        let id = |notices| {
            let mut msg = Vec::new();
            wire::append_relay_identity(
                &mut msg,
                &wire::RelayIdentity {
                    server_version: "1.42.0",
                    name: "",
                    notices,
                },
            )
            .unwrap();
            msg
        };
        assert_eq!(
            dispatch_server_message(&id(wire::RELAY_NOTICE_SCREENING)).unwrap(),
            ServerMessage::RelayNotices(wire::RELAY_NOTICE_SCREENING)
        );
        assert_eq!(
            dispatch_server_message(&id(0)).unwrap(),
            ServerMessage::RelayNotices(0)
        );
        let mut flagged = id(wire::RELAY_NOTICE_SCREENING);
        flagged[2] = 0x80;
        assert!(dispatch_server_message(&flagged).is_err());
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

    // R74 (docs/76 §6.12): a PoP's 0x18 and a home relay's 0x19 reach the
    // publish session too, owned and sanitized; a malformed one errors.
    #[test]
    fn local_relay_info_and_offer_dispatch_owned() {
        let mut info = Vec::new();
        wire::append_local_relay_info(
            &mut info,
            &wire::LocalRelayInfo {
                linked: true,
                on_wall: true,
                name: "Assembly 2026",
                home_origin: "https://gawk.ioio.fi",
                wall_code: "assembly-2026",
            },
        )
        .unwrap();
        let ServerMessage::LocalRelayInfo(got) = dispatch_server_message(&info).unwrap() else {
            panic!("info");
        };
        assert!(got.linked && got.on_wall);
        assert_eq!(got.name.as_deref(), Some("Assembly 2026"));
        assert_eq!(got.wall_code, "assembly-2026");

        let h = [3u8; 32];
        let mut offer = Vec::new();
        wire::append_local_relay_offer(
            &mut offer,
            &wire::LocalRelayOffer {
                handle: "6f1c2d3e-4a5b-4c6d-8e7f-0123456789ab",
                name: "Assembly 2026",
                validity: 3600,
                candidates: vec![wire::LocalRelayCandidate {
                    url: "https://192.168.1.20:4433",
                    hashes: vec![&h],
                }],
            },
        )
        .unwrap();
        let ServerMessage::LocalRelayOffer(got) = dispatch_server_message(&offer).unwrap() else {
            panic!("offer");
        };
        assert_eq!(got.handle, "6f1c2d3e-4a5b-4c6d-8e7f-0123456789ab");
        assert_eq!(got.candidates[0].hashes, vec![h]);

        let mut bad = offer.clone();
        bad[2] = 1;
        assert!(dispatch_server_message(&bad).is_err());
        let mut bad = info.clone();
        bad[2] = 1;
        assert!(dispatch_server_message(&bad).is_err());
    }

    // A maximal offer (four candidates of 256-byte URLs and two pins each)
    // must not be clipped by the read bound.
    #[test]
    fn read_limit_covers_a_maximal_offer() {
        let h = [1u8; 32];
        let url = format!("https://{}", "a".repeat(wire::MAX_LOCAL_RELAY_URL_LEN - 8));
        let handle = "h".repeat(wire::MAX_LOCAL_RELAY_HANDLE_LEN);
        let name = "n".repeat(wire::MAX_RELAY_IDENTITY_NAME_LEN);
        let cands: Vec<_> = (0..wire::MAX_LOCAL_RELAY_CANDIDATES)
            .map(|_| wire::LocalRelayCandidate {
                url: &url,
                hashes: vec![&h, &h],
            })
            .collect();
        let mut msg = Vec::new();
        wire::append_local_relay_offer(
            &mut msg,
            &wire::LocalRelayOffer {
                handle: &handle,
                name: &name,
                validity: 1,
                candidates: cands,
            },
        )
        .unwrap();
        assert!(msg.len() <= SERVER_STREAM_READ_LIMIT, "{}", msg.len());
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
