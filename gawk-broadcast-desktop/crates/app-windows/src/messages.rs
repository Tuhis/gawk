//! Curated error sentences (docs/38 D12.6). The rule ported from the Linux
//! shell's shadowing bug: SENTINELS ARE CHECKED FIRST, before any generic
//! `StartError` rendering — the engine wraps failures, and rendering the
//! wrapper first would bury each curated sentence under a debug chain.
//! "Start a new broadcast instead" is offered ONLY for connect-phase
//! failures (R1's mint-fallback bug stays fixed in all shells).

use gawk_engine::relay::{StartError, StartPhase};

/// The shell's classified start failure.
#[cfg_attr(not(windows), allow(dead_code))] // the refusal is only minted by the Windows pipeline
pub enum StartFailure {
    /// The cascade refused: no hardware encoder survived trial (G3).
    NoHardwareEncoder,
    /// The relay/engine failed to start.
    Relay(StartError),
    /// Capture setup failed outside the encoder (WGC/D3D).
    Capture(String),
}

/// The user-facing sentence for a start failure. `app_url` is the resolved
/// frontend origin (for the refusal's browser pointer).
pub fn message(failure: &StartFailure, app_url: &str) -> String {
    match failure {
        // Sentinel first — never shadowed by generic rendering.
        StartFailure::NoHardwareEncoder => gawk_encode::cascade::refusal_message(app_url),
        StartFailure::Capture(msg) => format!("Could not start screen capture: {msg}"),
        StartFailure::Relay(se) => relay_message(se),
    }
}

/// The Linux `StartError.Message()` texts, verbatim (they are product copy,
/// not incidental strings).
fn relay_message(se: &StartError) -> String {
    match se.status {
        401 => "The relay rejected the publish secret. Check the secret in your settings matches the relay's.".into(),
        404 => "That broadcast code no longer exists on the relay. Start a new broadcast to get a fresh code.".into(),
        409 => "Someone is already publishing to that broadcast code. Start a new broadcast to get a fresh code.".into(),
        429 => "The relay is at capacity (too many broadcasts, or too many connection attempts). Try again in a moment.".into(),
        // R39 (docs/42 D15): the ban gate answers pre-upgrade, so there is no
        // close code to carry the reason — the status IS the message. The
        // browser broadcaster cannot read it at all, which is exactly why
        // spelling it out here is worth doing.
        451 => "This broadcast ID or your address is banned by the relay operator. Contact the operator if you think this is a mistake.".into(),
        _ => match se.phase {
            StartPhase::Connect => format!("Could not reach the relay: {}", se.message),
            StartPhase::Capture => format!("Could not start capture: {}", se.message),
        },
    }
}

/// Whether the error card may offer "Start a new broadcast instead": only
/// when the failure was in the connect phase — a capture-phase failure had a
/// live publisher session on that ID, and silently minting strands viewers.
pub fn can_mint(failure: &StartFailure) -> bool {
    matches!(
        failure,
        StartFailure::Relay(StartError {
            phase: StartPhase::Connect,
            ..
        })
    )
}

/// Notifications show only the first line — multi-line curated messages
/// keep their detail in the error card.
pub fn first_line(s: &str) -> &str {
    s.split('\n').next().unwrap_or(s)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn relay(phase: StartPhase, status: u16) -> StartFailure {
        StartFailure::Relay(StartError {
            phase,
            status,
            message: "dial tcp: refused".into(),
        })
    }

    #[test]
    fn statuses_map_to_the_curated_sentences() {
        for (status, needle) in [
            (401u16, "rejected the publish secret"),
            (404, "no longer exists"),
            (409, "already publishing"),
            (429, "at capacity"),
            (451, "banned by the relay operator"),
        ] {
            let m = message(&relay(StartPhase::Connect, status), "https://gawk.ioio.fi");
            assert!(m.contains(needle), "{status}: {m}");
        }
        assert!(
            message(&relay(StartPhase::Connect, 0), "x").starts_with("Could not reach the relay:")
        );
        assert!(
            message(&relay(StartPhase::Capture, 0), "x").starts_with("Could not start capture:")
        );
    }

    #[test]
    fn refusal_sentinel_preempts_generic_rendering() {
        let m = message(&StartFailure::NoHardwareEncoder, "https://gawk.ioio.fi");
        assert!(m.contains("deliberately has no software encoder"));
        assert!(m.contains("https://gawk.ioio.fi"));
    }

    #[test]
    fn mint_is_offered_only_for_connect_phase_failures() {
        assert!(can_mint(&relay(StartPhase::Connect, 429)));
        assert!(!can_mint(&relay(StartPhase::Capture, 0)));
        assert!(!can_mint(&StartFailure::NoHardwareEncoder));
        assert!(!can_mint(&StartFailure::Capture("wgc".into())));
    }
}
