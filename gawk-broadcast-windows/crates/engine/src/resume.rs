//! The auto-resume policy, ported from gawk-broadcast/internal/engine/
//! resume.go — transport-only reconnect: capture, the encoder and the
//! frameId space all survive, because the expensive parts of a broadcast
//! have nothing to do with which QUIC connection the bytes left on. The Go
//! engine shipped without this and it cost a live broadcast (docs/19's
//! DE6G6P incident); this port starts with it.

use std::time::Duration;

/// Pause before the first reclaim attempt. Not zero: when the transport
/// cannot surface a close code we cannot tell a planned drain from an abrupt
/// death, and must not hot-loop against a relay that is genuinely gone. Fits
/// inside R17's ≤1 s rollout-blip budget.
pub const RESUME_INITIAL_DELAY: Duration = Duration::from_millis(250);
/// Backoff cap — kept well under any sane grace so the window is spent
/// trying rather than waiting.
pub const RESUME_MAX_DELAY: Duration = Duration::from_secs(5);
/// Bounds the whole effort; matches the relay's DEFAULT -broadcast-grace. A
/// relay whose grace is shorter answers 404 the moment the hub is gone,
/// which ends this sooner and for the right reason.
pub const RESUME_WINDOW: Duration = Duration::from_secs(5 * 60);

/// Whether a close code means this broadcaster must stay down.
///
/// 4004 is the load-bearing one, and it is a relay INVARIANT rather than a
/// preference: "newest publisher wins" only converges because the deposed
/// client does not come back — two auto-resuming engines would depose each
/// other forever.
///
/// 4006 (R39, docs/42 §4.4) is terminal for a different reason: the operator
/// killed this broadcast and the ID is banned for at least the kill cooldown,
/// so every reclaim would collect a 451. Resuming would spend the whole
/// RESUME_WINDOW hammering a gate designed to refuse it.
pub fn terminal_for_publisher(code: u32) -> bool {
    matches!(
        code,
        gawk_wire::CLOSE_CODE_BROADCAST_ENDED
            | gawk_wire::CLOSE_CODE_PUBLISHER_SUPERSEDED
            | gawk_wire::CLOSE_CODE_TERMINATED_BY_OPERATOR
    )
}

/// The user-facing sentence for a terminal close.
pub fn close_code_message(code: u32) -> String {
    match code {
        gawk_wire::CLOSE_CODE_PUBLISHER_SUPERSEDED => {
            "another broadcaster took over this code — this session has been superseded".into()
        }
        gawk_wire::CLOSE_CODE_BROADCAST_ENDED => "the relay ended this broadcast".into(),
        gawk_wire::CLOSE_CODE_TERMINATED_BY_OPERATOR => {
            "this broadcast was terminated by the server operator".into()
        }
        other => format!("the relay closed this session (code {other})"),
    }
}

/// Whether a reclaim's HTTP status means retrying can only ever fail again.
/// Everything else — including a bare transport failure, which arrives as
/// status 0 — is worth another attempt. These are the statuses the relay's
/// handlePublish actually returns; keep in step with
/// gawk-server/internal/transport/server.go.
///
/// This IS the auto-resume retry cap docs/42 §4.4 asks for on repeated
/// 403/451: the cap is one. Both are verdicts about *this* broadcaster rather
/// than transient relay conditions, so counting to a larger number before
/// believing them would only mean more dials against a gate whose whole job is
/// to refuse them. Reading the status at all is the native broadcasters'
/// advantage over the browser (D15) — spending it on a retry budget would
/// waste it.
pub fn resume_terminal(status: u16) -> bool {
    matches!(
        status,
        401 // the publish secret is wrong
        | 403 // the resume token was refused
        | 404 // the grace expired: this broadcast is gone
        | 409 // someone else holds the code
        | 451 // R39: banned by the relay operator (docs/42 D15)
    )
}

/// The backoff ladder: 250 ms doubling to a 5 s cap.
#[derive(Debug)]
pub struct ResumeBackoff {
    next: Duration,
}

impl ResumeBackoff {
    pub fn new() -> Self {
        Self {
            next: RESUME_INITIAL_DELAY,
        }
    }

    /// The delay to sleep before the next attempt; doubles, capped.
    pub fn next_delay(&mut self) -> Duration {
        let d = self.next;
        self.next = (self.next * 2).min(RESUME_MAX_DELAY);
        d
    }
}

impl Default for ResumeBackoff {
    fn default() -> Self {
        Self::new()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn terminal_close_codes_are_exactly_4000_4004_and_4006() {
        assert!(terminal_for_publisher(4000));
        assert!(terminal_for_publisher(4004));
        // R39 (docs/42 §4.4): an operator kill. Resuming would only collect
        // 451s for the whole kill cooldown.
        assert!(terminal_for_publisher(
            gawk_wire::CLOSE_CODE_TERMINATED_BY_OPERATOR
        ));
        // 4002 (drain) is the case auto-resume EXISTS for; 4001/4003 are not
        // publisher-terminal either.
        for code in [4001, 4002, 4003, 4005, 0] {
            assert!(!terminal_for_publisher(code), "{code}");
        }
    }

    #[test]
    fn terminal_closes_render_their_own_sentence() {
        assert!(close_code_message(4004).contains("superseded"));
        assert!(close_code_message(4000).contains("ended this broadcast"));
        assert!(
            close_code_message(gawk_wire::CLOSE_CODE_TERMINATED_BY_OPERATOR)
                .contains("terminated by the server operator")
        );
        // Anything else still reads as a sentence, not a bare number.
        assert!(close_code_message(4242).contains("code 4242"));
    }

    #[test]
    fn terminal_statuses_match_the_relay_handler() {
        // 451 is R39's ban rejection (docs/42 D15). It is terminal on the
        // FIRST sighting: that is the "cap auto-resume retries on repeated
        // 403/451" requirement, at its tightest.
        for status in [401, 403, 404, 409, 451] {
            assert!(resume_terminal(status), "{status}");
        }
        // Status 0 is a bare transport failure — always worth retrying; 429
        // (relay full) and 5xx are transient.
        for status in [0, 400, 429, 500, 503] {
            assert!(!resume_terminal(status), "{status}");
        }
    }

    #[test]
    fn backoff_doubles_from_250ms_to_a_5s_cap() {
        let mut b = ResumeBackoff::new();
        let mut got = Vec::new();
        for _ in 0..7 {
            got.push(b.next_delay().as_millis() as u64);
        }
        assert_eq!(got, vec![250, 500, 1000, 2000, 4000, 5000, 5000]);
    }
}
