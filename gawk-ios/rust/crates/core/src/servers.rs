//! R37's server picker, as Swift sees it (docs/67 D23, docs/40): the
//! shared `/echo` probe and RelayIdentity read, never re-implemented.

use gawk_engine::probe::{self, ProbeResult};

/// What probing a server found. `name` is the operator's claim from
/// RelayIdentity: shown beside the host, never in place of it (docs/40 F6).
/// `screens`: the relay says it automatically screens streams for explicit
/// content (R40, docs/71 D13) — Settings marks the server "Content
/// screening" (§9.8).
#[derive(Debug, Clone, PartialEq, Eq, uniffi::Enum)]
pub enum ServerProbe {
    Reachable {
        rtt_ms: u32,
        name: Option<String>,
        screens: bool,
    },
    Unreachable,
}

/// Probes `relay_url` (blocking, a few seconds at most: call it off the
/// main thread). `cert_hashes`: the server's pins (R74, docs/76 §6.1 item
/// 5; lowercase hex), empty — the default — for the system store.
#[uniffi::export(default(cert_hashes = []))]
pub fn probe_server(relay_url: String, insecure: bool, cert_hashes: Vec<String>) -> ServerProbe {
    let pins = gawk_engine::transport::cert_hashes_from_hex(&cert_hashes);
    let Ok(rt) = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
    else {
        return ServerProbe::Unreachable;
    };
    let origin = gawk_engine::defaults::origin().to_owned();
    match rt.block_on(probe::probe_pinned(&relay_url, &origin, insecure, &pins)) {
        ProbeResult::Ok {
            rtt_ms,
            name,
            screens,
        } => ServerProbe::Reachable {
            rtt_ms,
            name,
            screens,
        },
        ProbeResult::Failed => ServerProbe::Unreachable,
    }
}

/// Whether `url` is the compiled-in default fleet (the official deployment
/// is the default target, CLAUDE.md); a link to any other shows docs/40's
/// persistent strip (D20).
#[uniffi::export]
pub fn is_default_relay(url: String) -> bool {
    gawk_engine::config::is_default_relay(&url)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_default_fleet_is_recognised() {
        assert!(is_default_relay(gawk_engine::defaults::RELAY_URL.into()));
        assert!(!is_default_relay("https://relay.example:4433".into()));
    }

    #[test]
    fn a_malformed_server_is_unreachable_not_a_panic() {
        assert_eq!(
            probe_server("not a url".into(), false, vec![]),
            ServerProbe::Unreachable
        );
    }

    // R74: a stored value that isn't a hash is left out, never a panic.
    #[test]
    fn a_pinned_probe_of_a_malformed_server_is_unreachable() {
        assert_eq!(
            probe_server("not a url".into(), false, vec!["nope".into()]),
            ServerProbe::Unreachable
        );
    }
}
