//! The viewer's reconnect policy: the SPA's `transport/reconnect.ts`, so a
//! native viewer reacts to a relay drain or a pod death exactly as a web one
//! does. Which closes are terminal comes from the shared wire crate
//! ([`gawk_wire::terminal_for_viewer`]), never restated here.

/// The SPA's `RECONNECT_MAX_ATTEMPTS`: ~100 s of ladder before giving up.
pub const RECONNECT_MAX_ATTEMPTS: u32 = 10;
/// The SPA's `ABRUPT_DROP_RETRY_DELAY_MS`: the replacement pod is already
/// behind the Service.
pub const ABRUPT_DROP_RETRY_DELAY_MS: u64 = 250;
/// R74 (docs/76 item 28): a PoP's uplink is full (4012). Not terminal: the
/// viewer tries again every 20 s, joining when a slot frees, and never gives
/// up on it (each refused session connected, so the ladder restarts).
pub const POP_UPLINK_FULL_RETRY_MS: u64 = 20_000;

/// R78 (docs/79 D7): the broadcaster stopped (4014). Not terminal: the
/// viewer waits, dialling the same code every 5 s plus up to 1 s of jitter,
/// forever and outside the reconnect budget, so a restart on the same code
/// resumes by itself. A fixed cadence, not a ladder: the restart is a human
/// action seconds to minutes away, and the jitter spreads a crowd's dials.
pub const BROADCASTER_STOPPED_RETRY_MS: u64 = 5_000;
pub const BROADCASTER_STOPPED_JITTER_MS: u64 = 1_000;

/// The wait before the next dial of a stopped broadcast: 5 s plus a fresh
/// 0–1 s of jitter.
pub fn broadcaster_stopped_delay_ms() -> u64 {
    use std::hash::{BuildHasher, Hasher};
    let mut h = std::collections::hash_map::RandomState::new().build_hasher();
    h.write_u128(
        std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map_or(0, |d| d.as_nanos()),
    );
    BROADCASTER_STOPPED_RETRY_MS + h.finish() % (BROADCASTER_STOPPED_JITTER_MS + 1)
}

/// The delay before reconnect attempt `attempt` (1-based) after a session
/// ended with `close_code` (`None`: an abrupt drop). 1, 2, 4, 8 s, then
/// 15 s; the first attempt after a drain (4002) is immediate and after an
/// abrupt drop 250 ms. Terminal codes never get here: ask
/// [`gawk_wire::terminal_for_viewer`] first.
pub fn reconnect_delay_ms(attempt: u32, close_code: Option<u32>) -> u64 {
    if close_code == Some(gawk_wire::CLOSE_CODE_POP_UPLINK_FULL) {
        return POP_UPLINK_FULL_RETRY_MS;
    }
    if attempt == 1 {
        match close_code {
            Some(gawk_wire::CLOSE_CODE_SERVER_DRAINING) => return 0,
            None => return ABRUPT_DROP_RETRY_DELAY_MS,
            Some(_) => {}
        }
    }
    let exp = attempt.saturating_sub(1).min(4);
    (1000u64 << exp).min(15_000)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn climbs_the_ladder_and_caps_at_15s() {
        let coded = Some(gawk_wire::CLOSE_CODE_SUBSCRIBER_UNRESPONSIVE);
        let ladder: Vec<u64> = (1..=7).map(|a| reconnect_delay_ms(a, coded)).collect();
        assert_eq!(ladder, [1000, 2000, 4000, 8000, 15_000, 15_000, 15_000]);
    }

    // R74 (docs/76 item 28): 4012 is not terminal and retries every 20 s.
    #[test]
    fn a_full_party_uplink_retries_every_20s() {
        let full = Some(gawk_wire::CLOSE_CODE_POP_UPLINK_FULL);
        assert!(!gawk_wire::terminal_for_viewer(
            gawk_wire::CLOSE_CODE_POP_UPLINK_FULL
        ));
        assert_eq!(reconnect_delay_ms(1, full), 20_000);
        assert_eq!(reconnect_delay_ms(3, full), 20_000);
    }

    // R78 (docs/79 D7): 4014 waits 5 s plus up to 1 s, and is not terminal.
    #[test]
    fn a_stopped_broadcast_waits_5s_plus_jitter() {
        assert!(!gawk_wire::terminal_for_viewer(
            gawk_wire::CLOSE_CODE_BROADCASTER_STOPPED
        ));
        for _ in 0..50 {
            let d = broadcaster_stopped_delay_ms();
            assert!((5_000..=6_000).contains(&d), "{d}");
        }
    }

    #[test]
    fn a_drain_reconnects_at_once_and_an_abrupt_drop_fast_on_the_first_attempt_only() {
        let drain = Some(gawk_wire::CLOSE_CODE_SERVER_DRAINING);
        assert_eq!(reconnect_delay_ms(1, drain), 0);
        assert_eq!(reconnect_delay_ms(2, drain), 2000);
        assert_eq!(reconnect_delay_ms(1, None), ABRUPT_DROP_RETRY_DELAY_MS);
        assert_eq!(reconnect_delay_ms(2, None), 2000);
    }
}
