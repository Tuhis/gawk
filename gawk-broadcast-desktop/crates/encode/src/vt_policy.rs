//! VideoToolbox encode decisions with the framework taken out (R52 MB3,
//! docs/54 D7): the app-forced GOP, the rate-control numbers, the trial's
//! shape and its cadence check. Pure and portable so each is a host test;
//! `vt` only applies them.

/// The app-forced closed GOP (D7). Low-latency mode is documented as
/// "infinite GOP after the IDR", so `MaxKeyFrameInterval` is set but not
/// relied on: every `fps/2`-th frame *counted at the encoder input* is
/// submitted with `ForceKeyFrame`. Counting frames, not wall-clock: under
/// damage-driven capture a static desktop delivers few frames, so the
/// wall-clock GOP stretches — which is measured and shown (EMA α = 0.3), as
/// on Linux and Windows, rather than padded with synthesized frames.
#[derive(Debug)]
pub struct KeyframeCadence {
    every: u32,
    /// Frames submitted since the last IDR; `None` before the first frame.
    since: Option<u32>,
}

impl KeyframeCadence {
    pub fn new(fps: u32) -> Self {
        Self {
            every: gop_frames(fps),
            since: None,
        }
    }

    /// Whether the frame about to be submitted must be an IDR. `forced` is
    /// an on-demand IDR (the resume re-prime, docs/38 D5); it restarts the
    /// cadence so the next cadence IDR is a full GOP later.
    pub fn next(&mut self, forced: bool) -> bool {
        let idr = match self.since {
            None => true,
            Some(n) => forced || n + 1 >= self.every,
        };
        self.since = Some(if idr { 0 } else { self.since.unwrap_or(0) + 1 });
        idr
    }
}

/// Frames per 500 ms GOP at `fps` (never 0).
pub fn gop_frames(fps: u32) -> u32 {
    (fps / 2).max(1)
}

/// D7's peak-constrained rate control: `AverageBitRate` is 75 % of the
/// user-facing (peak) bitrate, and `DataRateLimits` caps the peak over a
/// one-second window. Returns `(average_bps, peak_bytes, window_seconds)`.
pub fn rate_limits(peak_bps: u32) -> (u32, u32, f64) {
    ((peak_bps / 4) * 3, peak_bps / 8, 1.0)
}

/// The trial (D7): enough frames for a cadence IDR at the GOP boundary and
/// a forced IDR half a GOP after it. Returns `(frames, forced_at)`.
pub fn trial_plan(fps: u32) -> (usize, usize) {
    let gop = gop_frames(fps) as usize;
    (gop + gop / 2 + 2, gop + gop / 2)
}

/// Checks the trial bitstream's IDR positions against the cadence: an IDR
/// at every GOP boundary the trial crossed (frame 0, `gop`, …). The forced
/// IDR and the other D7 invariants are the shared `cascade::validate_trial`'s.
pub fn check_cadence(idr_flags: &[bool], fps: u32) -> Result<(), String> {
    let gop = gop_frames(fps) as usize;
    for at in (0..idr_flags.len()).step_by(gop) {
        if !idr_flags[at] {
            return Err(format!(
                "no IDR at frame {at}: ForceKeyFrame not honored in low-latency mode (V-3)"
            ));
        }
    }
    Ok(())
}

/// A `CMTime` as 100 ns ticks — the unit the trial and the cascade compare
/// timestamps in. `None` for an invalid time or a non-positive timescale.
/// (The capture crate's twin maps host-clock times; this one only ever
/// sees the session-clock times the encoder was fed.)
pub fn cm_time_to_100ns(value: i64, timescale: i32, valid: bool) -> Option<i64> {
    if !valid || timescale <= 0 {
        return None;
    }
    i64::try_from(i128::from(value) * 10_000_000 / i128::from(timescale)).ok()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn cadence_forces_an_idr_every_half_second_of_frames() {
        let mut c = KeyframeCadence::new(60);
        let at: Vec<usize> = (0..91usize).filter(|_| c.next(false)).collect();
        assert_eq!(at, vec![0, 30, 60, 90]);
    }

    #[test]
    fn a_forced_idr_restarts_the_cadence() {
        let mut c = KeyframeCadence::new(60);
        let mut at = Vec::new();
        for i in 0..70usize {
            if c.next(i == 10) {
                at.push(i);
            }
        }
        assert_eq!(at, vec![0, 10, 40]);
    }

    #[test]
    fn gop_is_half_the_rate_and_never_zero() {
        assert_eq!(gop_frames(60), 30);
        assert_eq!(gop_frames(120), 60);
        assert_eq!(gop_frames(5), 2);
        assert_eq!(gop_frames(1), 1);
        assert_eq!(gop_frames(0), 1);
    }

    #[test]
    fn rate_limits_are_seventy_five_percent_average_under_a_one_second_peak() {
        assert_eq!(rate_limits(12_000_000), (9_000_000, 1_500_000, 1.0));
    }

    #[test]
    fn the_trial_crosses_a_gop_boundary_then_forces() {
        assert_eq!(trial_plan(60), (47, 45));
        let (frames, forced) = trial_plan(30);
        assert!(forced > gop_frames(30) as usize && forced < frames);
    }

    #[test]
    fn cadence_check_names_the_missing_idr() {
        let mut flags = vec![false; 47];
        flags[0] = true;
        flags[30] = true;
        assert_eq!(check_cadence(&flags, 60), Ok(()));
        flags[30] = false;
        let err = check_cadence(&flags, 60).unwrap_err();
        assert!(err.contains("frame 30"), "{err}");
        assert!(check_cadence(&[false], 60).is_err());
    }

    #[test]
    fn cm_time_converts() {
        assert_eq!(cm_time_to_100ns(15, 10_000_000, true), Some(15));
        assert_eq!(cm_time_to_100ns(1, 1_000, true), Some(10_000));
        assert_eq!(cm_time_to_100ns(1, 0, true), None);
        assert_eq!(cm_time_to_100ns(1, 1, false), None);
    }
}
