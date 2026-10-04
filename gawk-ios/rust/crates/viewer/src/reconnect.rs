//! The viewer's reconnect policy: the SPA's `transport/reconnect.ts`, so a
//! native viewer reacts to a relay drain or a pod death exactly as a web one
//! does. Which closes are terminal comes from the shared wire crate
//! ([`gawk_wire::terminal_for_viewer`]), never restated here.

/// The SPA's `RECONNECT_MAX_ATTEMPTS`: ~100 s of ladder before giving up.
pub const RECONNECT_MAX_ATTEMPTS: u32 = 10;
/// The SPA's `ABRUPT_DROP_RETRY_DELAY_MS`: the replacement pod is already
/// behind the Service.
pub const ABRUPT_DROP_RETRY_DELAY_MS: u64 = 250;

/// The delay before reconnect attempt `attempt` (1-based) after a session
/// ended with `close_code` (`None`: an abrupt drop). 1, 2, 4, 8 s, then
/// 15 s; the first attempt after a drain (4002) is immediate and after an
/// abrupt drop 250 ms. Terminal codes never get here: ask
/// [`gawk_wire::terminal_for_viewer`] first.
pub fn reconnect_delay_ms(attempt: u32, close_code: Option<u32>) -> u64 {
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

    #[test]
    fn a_drain_reconnects_at_once_and_an_abrupt_drop_fast_on_the_first_attempt_only() {
        let drain = Some(gawk_wire::CLOSE_CODE_SERVER_DRAINING);
        assert_eq!(reconnect_delay_ms(1, drain), 0);
        assert_eq!(reconnect_delay_ms(2, drain), 2000);
        assert_eq!(reconnect_delay_ms(1, None), ABRUPT_DROP_RETRY_DELAY_MS);
        assert_eq!(reconnect_delay_ms(2, None), 2000);
    }
}
