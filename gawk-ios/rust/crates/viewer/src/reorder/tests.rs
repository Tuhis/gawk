//! Restated from the SPA's `reorder-buffer.test.ts` (its allowance pinned
//! at 0 there, which is D13's rule here) and the freeze-on-gap cases of
//! `reorder-grace.test.ts`. The resilient/DVR profile cases have no
//! counterpart: v1 has no reliable delivery (D13). The jitter-estimator
//! cases move with the estimator to `jitter.rs`.

use super::*;

/// A controlled clock, a live-edge or fixed-offset schedule, and the
/// arrival baseline the pipeline would pass: the running min of
/// `arrival − timestamp`, reset on a restart (over these tests' spans the
/// windowed min and the running min agree).
struct Harness {
    rb: ReorderBuffer,
    t: f64,
    offset_ms: f64,
    decode_lead_ms: f64,
    grace_ms: f64,
    baseline: Option<f64>,
    out: Vec<Reordered>,
    restarts: usize,
}

impl Harness {
    fn new() -> Self {
        Self::paced(0.0)
    }

    fn paced(offset_ms: f64) -> Self {
        Self {
            rb: ReorderBuffer::new(),
            t: 1000.0,
            offset_ms,
            decode_lead_ms: 0.0,
            grace_ms: DELTA_GAP_GRACE_MS,
            baseline: None,
            out: Vec::new(),
            restarts: 0,
        }
    }

    fn pacing(&self) -> Pacing {
        Pacing {
            offset_ms: self.offset_ms,
            decode_lead_ms: self.decode_lead_ms,
            grace_ms: self.grace_ms,
            baseline_ms: self.baseline,
        }
    }

    fn observe(&mut self, ts_us: u64) {
        let delta = self.t - ts_us as f64 / 1000.0;
        self.baseline = Some(self.baseline.map_or(delta, |b| b.min(delta)));
    }

    fn settle(&mut self, mut produced: Vec<Reordered>) {
        for r in &produced {
            if *r == Reordered::Restart {
                self.restarts += 1;
                self.baseline = None;
            }
        }
        self.out.append(&mut produced);
    }

    fn kf_ts(&mut self, frame_id: u32, ts_us: u64, config: Option<VideoConfig>) {
        let mut produced = Vec::new();
        // The restart signal resets the baseline before this frame counts.
        let restart = self
            .rb
            .decode_position
            .is_some_and(|p| p != frame_id && !frame_id_ahead(frame_id, p));
        if restart {
            self.baseline = None;
        }
        self.observe(ts_us);
        let p = self.pacing();
        self.rb.push_keyframe(
            StreamKeyframe {
                frame_id,
                timestamp_us: ts_us,
                config,
                data: vec![frame_id as u8],
            },
            self.t,
            &p,
            &mut produced,
        );
        self.settle(produced);
    }

    fn kf(&mut self, frame_id: u32) {
        self.kf_ts(frame_id, frame_id as u64, None);
    }

    fn delta_ts(&mut self, frame_id: u32, ts_us: u64) {
        if self
            .rb
            .decode_position
            .is_none_or(|p| frame_id_ahead(frame_id, p))
        {
            self.observe(ts_us);
        }
        let mut produced = Vec::new();
        let p = self.pacing();
        self.rb.push_delta(
            frame_id,
            ts_us,
            vec![frame_id as u8],
            self.t,
            &p,
            &mut produced,
        );
        self.settle(produced);
    }

    fn delta(&mut self, frame_id: u32) {
        self.delta_ts(frame_id, frame_id as u64);
    }

    fn tick(&mut self) {
        let mut produced = Vec::new();
        let p = self.pacing();
        self.rb.tick(self.t, &p, &mut produced);
        self.settle(produced);
    }

    fn resync(&mut self) {
        let mut produced = Vec::new();
        let p = self.pacing();
        self.rb.request_resync(self.t, &p, &mut produced);
        self.settle(produced);
    }

    fn released(&self) -> Vec<&ReleasedFrame> {
        self.out
            .iter()
            .filter_map(|r| match r {
                Reordered::Frame(f) => Some(f),
                Reordered::Restart => None,
            })
            .collect()
    }

    fn ids(&self) -> Vec<u32> {
        self.released().iter().map(|f| f.frame_id).collect()
    }

    fn is_key(&self, frame_id: u32) -> bool {
        self.released()
            .iter()
            .find(|f| f.frame_id == frame_id)
            .unwrap()
            .keyframe
    }
}

#[test]
fn holds_deltas_that_arrive_before_their_keyframe_then_releases_key_then_contiguous() {
    let mut h = Harness::new();
    h.delta(1);
    h.delta(2);
    assert!(h.ids().is_empty());
    h.kf(0);
    assert_eq!(h.ids(), [0, 1, 2]);
    assert!(h.is_key(0));
    assert!(!h.is_key(1));
}

#[test]
fn reorders_an_out_of_order_delta_within_a_session() {
    let mut h = Harness::new();
    h.kf(0);
    h.delta(2);
    assert_eq!(h.ids(), [0]);
    h.delta(1);
    assert_eq!(h.ids(), [0, 1, 2]);
}

#[test]
fn releases_a_keyframe_that_is_the_contiguous_next_frame_keeping_buffered_deltas() {
    let mut h = Harness::new();
    h.kf(0);
    h.delta(1);
    h.delta(3);
    assert_eq!(h.ids(), [0, 1]);
    h.kf(2);
    assert_eq!(h.ids(), [0, 1, 2, 3]);
    assert!(h.is_key(2));
}

#[test]
fn carries_an_embedded_config_on_keyframe_releases_only() {
    let mut h = Harness::new();
    let cfg = VideoConfig {
        codec: "avc1.42E01F".into(),
        extradata: vec![1, 2, 3],
    };
    h.kf_ts(0, 0, Some(cfg.clone()));
    h.delta(1);
    assert_eq!(h.released()[0].config, Some(cfg));
    assert_eq!(h.released()[1].config, None);
}

#[test]
fn does_not_release_before_the_first_keyframe() {
    let mut h = Harness::new();
    h.delta(7);
    h.delta(8);
    assert!(h.ids().is_empty());
}

#[test]
fn freezes_on_a_delta_gap_after_the_grace_and_resyncs_at_the_next_keyframe() {
    let mut h = Harness::new();
    h.kf(0);
    h.delta(2); // 1 missing
    assert_eq!(h.ids(), [0]);
    h.t += DELTA_GAP_GRACE_MS - 5.0;
    h.tick();
    assert_eq!(h.ids(), [0]);
    h.t += 10.0;
    h.tick();
    assert_eq!(h.ids(), [0]);
    assert!(h.rb.stats().gap_resyncs >= 1);
    h.kf(3); // drops the orphan delta 2
    assert_eq!(h.ids(), [0, 3]);
    assert!(h.is_key(3));
    h.delta(4);
    assert_eq!(h.ids(), [0, 3, 4]);
}

#[test]
fn survives_a_keyframe_that_arrives_500ms_behind_its_trailing_deltas() {
    let mut h = Harness::new();
    for id in 101..=105 {
        h.delta(id);
    }
    for _ in 0..5 {
        h.t += 100.0;
        h.tick();
    }
    assert!(h.ids().is_empty());
    h.kf(100);
    assert_eq!(h.ids(), [100, 101, 102, 103, 104, 105]);
    assert_eq!(h.rb.stats().keyframe_wait_drops, 0);
}

#[test]
fn drops_undecodable_held_frames_once_they_age_past_the_keyframe_wait() {
    let mut h = Harness::new();
    h.delta(5);
    assert_eq!(h.rb.stats().buffered, 1);
    h.t += KEYFRAME_WAIT_MS + 1.0;
    h.tick();
    assert_eq!(h.rb.stats().buffered, 0);
    assert_eq!(h.rb.stats().keyframe_wait_drops, 1);
}

#[test]
fn immediately_resyncs_to_a_keyframe_buffered_ahead_of_a_delta_gap() {
    let mut h = Harness::new();
    h.kf(0);
    h.delta(1);
    h.kf(5); // 2..4 missing
    assert_eq!(h.ids(), [0, 1, 5]);
    assert_eq!(h.rb.stats().keyframes_released, 2);
}

#[test]
fn request_resync_holds_contiguous_deltas_until_the_next_keyframe() {
    let mut h = Harness::new();
    h.kf(0);
    h.delta(1);
    h.resync();
    h.delta(2);
    h.delta(3);
    assert_eq!(h.ids(), [0, 1]);
    h.kf(5);
    assert_eq!(h.ids(), [0, 1, 5]);
}

#[test]
fn releases_contiguously_across_frame_id_rollover() {
    let mut h = Harness::new();
    h.kf(0xffff_fffe);
    h.delta(0xffff_ffff);
    h.delta(0);
    h.delta(1);
    assert_eq!(h.ids(), [0xffff_fffe, 0xffff_ffff, 0, 1]);
}

#[test]
fn treats_a_post_rollover_keyframe_as_ahead_of_a_pre_rollover_gap() {
    let mut h = Harness::new();
    h.kf(0xffff_fffe);
    h.delta(0xffff_ffff);
    h.kf(30);
    assert_eq!(h.ids(), [0xffff_fffe, 0xffff_ffff, 30]);
}

#[test]
fn drops_a_stale_delta_at_or_below_the_decode_position() {
    let mut h = Harness::new();
    h.kf(5);
    h.delta(3);
    h.delta(5);
    assert_eq!(h.ids(), [5]);
    assert_eq!(h.rb.stats().deltas_dropped, 2);
}

#[test]
fn recovers_across_a_broadcaster_restart() {
    let mut h = Harness::new();
    h.kf(100);
    h.delta(101);
    h.kf(0);
    h.t += DELTA_GAP_GRACE_MS + 1.0;
    h.tick();
    assert_eq!(h.ids(), [100, 101, 0]);
    h.delta(1);
    assert_eq!(h.ids(), [100, 101, 0, 1]);
}

#[test]
fn drops_the_old_sessions_buffered_frames_on_a_restart() {
    let mut h = Harness::new();
    h.kf(100);
    h.delta(101);
    h.delta(103); // 102 lost
    h.t += DELTA_GAP_GRACE_MS + 1.0;
    h.tick();
    h.t += 200.0;
    h.kf(0);
    for i in 1..=10 {
        h.t += 16.0;
        h.delta(i);
    }
    assert_eq!(h.ids(), [100, 101, 0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10]);
    assert_eq!(h.rb.stats().buffered, 0);
}

#[test]
fn ignores_an_exact_duplicate_keyframe_of_the_current_position() {
    let mut h = Harness::new();
    h.kf(0);
    h.kf(0);
    h.delta(1);
    assert_eq!(h.ids(), [0, 1]);
}

#[test]
fn bounds_the_buffer_at_max_buffered_frames() {
    let mut h = Harness::new();
    for i in 1..=(MAX_BUFFERED_FRAMES as u32 + 10) {
        h.delta(i);
    }
    assert!(h.rb.stats().buffered <= MAX_BUFFERED_FRAMES as u64);
    assert!(h.rb.stats().deltas_dropped >= 10);
}

#[test]
fn signals_a_restart_when_a_keyframe_arrives_serially_behind_the_decode_position() {
    let mut h = Harness::new();
    h.kf(100);
    h.delta(101);
    assert_eq!(h.restarts, 0);
    h.kf(0);
    assert_eq!(h.restarts, 1);
    h.delta(1);
    h.kf(2); // ahead again
    assert_eq!(h.restarts, 1);
}

#[test]
fn does_not_signal_a_restart_for_an_exact_duplicate_keyframe() {
    let mut h = Harness::new();
    h.kf(5);
    h.kf(5);
    assert_eq!(h.restarts, 0);
}

#[test]
fn a_keyframe_supersedes_a_same_id_delta_already_buffered() {
    let mut h = Harness::new();
    h.kf(0);
    h.delta(3); // behind a gap
    h.kf(3);
    assert_eq!(h.ids(), [0, 3]);
    assert!(h.is_key(3));
}

// --- smoothed playout (R5 Q3) ----------------------------------------------

#[test]
fn releases_at_the_schedule_not_on_arrival() {
    let mut h = Harness::paced(150.0);
    h.kf_ts(0, 0, None); // arrives at 1000: due 0 + 1000 + 150
    assert!(h.ids().is_empty());
    h.t = 1149.0;
    h.tick();
    assert!(h.ids().is_empty());
    h.t = 1150.0;
    h.tick();
    assert_eq!(h.ids(), [0]);
    h.delta_ts(1, 16_000); // due at 1166
    assert_eq!(h.ids(), [0]);
    h.t = 1166.0;
    h.tick();
    assert_eq!(h.ids(), [0, 1]);
}

#[test]
fn offset_0_releases_immediately() {
    let mut h = Harness::paced(0.0);
    h.kf_ts(0, 0, None);
    h.delta_ts(1, 16_000);
    assert_eq!(h.ids(), [0, 1]);
}

#[test]
fn toggling_mid_session_re_paces_both_ways() {
    let mut h = Harness::paced(0.0);
    h.kf_ts(0, 0, None);
    assert_eq!(h.ids(), [0]);
    h.offset_ms = 150.0;
    h.delta_ts(1, 16_000);
    assert_eq!(h.ids(), [0]);
    h.offset_ms = 0.0;
    h.tick();
    assert_eq!(h.ids(), [0, 1]);
}

#[test]
fn keeps_the_gap_policy_under_pacing() {
    let mut h = Harness::paced(150.0);
    h.kf_ts(0, 0, None);
    h.t = 1150.0;
    h.tick();
    assert_eq!(h.ids(), [0]);
    h.delta_ts(2, 33_000); // 1 never arrives
    h.t += DELTA_GAP_GRACE_MS + 1.0;
    h.tick();
    assert_eq!(h.ids(), [0]);
    assert_eq!(h.rb.stats().gap_resyncs, 1);
    h.kf_ts(5, 100_000, None); // baseline still ~1000: due ≈ 1250
    assert_eq!(h.ids(), [0]);
    h.t = 1250.0;
    h.tick();
    assert_eq!(h.ids(), [0, 5]);
}

#[test]
fn request_resync_still_drops_to_the_keyframe_under_pacing() {
    let mut h = Harness::paced(150.0);
    h.kf_ts(0, 0, None);
    h.t = 1150.0;
    h.tick();
    assert_eq!(h.ids(), [0]);
    h.delta_ts(1, 16_000);
    h.kf_ts(2, 33_000, None);
    h.resync();
    h.t = 1350.0;
    h.tick();
    assert_eq!(h.ids(), [0, 2]);
}

// --- decode lead (R12 T2) --------------------------------------------------

#[test]
fn releases_at_target_minus_lead() {
    let mut h = Harness::paced(150.0);
    h.decode_lead_ms = 35.0;
    h.kf_ts(0, 0, None); // target 1150, release 1115
    h.t = 1114.0;
    h.tick();
    assert!(h.ids().is_empty());
    h.t = 1115.0;
    h.tick();
    assert_eq!(h.ids(), [0]);
}

// --- deep offset (R21) -----------------------------------------------------

#[test]
fn releases_despite_a_newer_keyframe_arriving_every_gop() {
    // A deep offset with live timestamps: the newest keyframe is never due,
    // so "freshest, then check due" would livelock on a black screen.
    let mut h = Harness::paced(3000.0);
    for i in 0..20u32 {
        let ts = (h.t * 1000.0) as u64;
        h.kf_ts(i * 10, ts, None);
        for d in 1..5 {
            h.t += 100.0;
            let ts = (h.t * 1000.0) as u64;
            h.delta_ts(i * 10 + d, ts);
            h.tick();
        }
        h.t += 100.0;
        h.tick();
    }
    let released = h.ids();
    assert!(!released.is_empty());
    // Deltas must survive the wait too, or playback is keyframe-only.
    let deltas = released.iter().filter(|id| *id % 10 != 0).count();
    assert!(deltas > released.len() / 2, "{released:?}");
}

// --- freeze-on-gap under the adaptive grace (reorder-grace.test.ts) --------

#[test]
fn releases_a_straggler_that_arrives_inside_a_widened_grace() {
    let mut h = Harness::new();
    h.grace_ms = 154.0; // the controller warmed to 120 ms of jitter + 34
    h.kf(1);
    h.delta(2);
    h.delta(4); // hole at 3
    h.t += 100.0;
    h.tick();
    assert_eq!(h.rb.stats().gap_resyncs, 0);
    h.t += 20.0;
    h.delta(3);
    assert_eq!(h.ids(), [1, 2, 3, 4]);
    assert_eq!(h.rb.stats().gap_resyncs, 0);
}

#[test]
fn still_freezes_fast_when_the_frame_is_genuinely_lost() {
    let mut h = Harness::new(); // grace at its 60 ms floor
    h.kf(1);
    h.delta(2);
    h.delta(4);
    h.t += DELTA_GAP_GRACE_MS + 1.0;
    h.tick();
    assert_eq!(h.rb.stats().gap_resyncs, 1);
    assert_eq!(h.ids(), [1, 2]);
}

#[test]
fn still_declares_the_gap_once_the_widened_grace_is_spent() {
    let mut h = Harness::new();
    h.grace_ms = 154.0;
    h.kf(1);
    h.delta(2);
    h.delta(4);
    h.t += 120.0 + 34.0 + 1.0;
    h.tick();
    assert_eq!(h.rb.stats().gap_resyncs, 1);
}

#[test]
fn the_keyframe_wait_outlasts_the_playout_offset() {
    assert_eq!(keyframe_wait_ms(0.0), KEYFRAME_WAIT_MS);
    assert_eq!(keyframe_wait_ms(350.0), KEYFRAME_WAIT_MS);
    assert_eq!(keyframe_wait_ms(3000.0), 3500.0);
}

// --- the adaptive grace controller (reorder-grace.test.ts) -----------------

use crate::playout::{HEADROOM_MS, OFFSET_DOWN_DWELL_MS, OFFSET_WARMUP_MS, PlayoutController};

/// Drives the controller the way the stats tick does; returns the clock it
/// stopped at.
fn feed_jitter(c: &mut PlayoutController, jitter_ms: f64, from_ms: f64, for_ms: f64) -> f64 {
    let mut t = from_ms;
    while t <= from_ms + for_ms {
        c.update(Some(jitter_ms), t);
        t += 1000.0;
    }
    t
}

fn warm_to(c: &mut PlayoutController, jitter_ms: f64) -> f64 {
    feed_jitter(c, jitter_ms, 10_000.0, OFFSET_WARMUP_MS + 10_000.0)
}

#[test]
fn grace_is_the_shipped_60ms_before_any_jitter_and_through_the_warmup() {
    let mut c = PlayoutController::with_envelope(GRACE_ENVELOPE);
    assert_eq!(c.offset_ms(), DELTA_GAP_GRACE_MS);
    feed_jitter(&mut c, 200.0, 10_000.0, OFFSET_WARMUP_MS - 1000.0);
    assert_eq!(c.offset_ms(), DELTA_GAP_GRACE_MS);
}

#[test]
fn grace_widens_to_the_measured_jitter_plus_headroom_once_warm() {
    let mut c = PlayoutController::with_envelope(GRACE_ENVELOPE);
    warm_to(&mut c, 120.0);
    assert!((c.offset_ms() - (120.0 + HEADROOM_MS)).abs() < 1e-6);
}

#[test]
fn grace_stays_between_its_floor_and_ceiling() {
    let mut c = PlayoutController::with_envelope(GRACE_ENVELOPE);
    warm_to(&mut c, 3748.0);
    assert_eq!(c.offset_ms(), MAX_DELTA_GAP_GRACE_MS);
    let mut c = PlayoutController::with_envelope(GRACE_ENVELOPE);
    warm_to(&mut c, 0.0);
    assert_eq!(c.offset_ms(), DELTA_GAP_GRACE_MS);
}

#[test]
fn grace_takes_a_large_rise_in_one_step_and_lowers_only_after_the_dwell() {
    let mut c = PlayoutController::with_envelope(GRACE_ENVELOPE);
    let t = warm_to(&mut c, 200.0);
    let raised = c.offset_ms();
    assert!((raised - (200.0 + HEADROOM_MS)).abs() < 1e-6);
    let t = feed_jitter(&mut c, 10.0, t, OFFSET_DOWN_DWELL_MS / 2.0);
    assert_eq!(c.offset_ms(), raised);
    feed_jitter(&mut c, 10.0, t, OFFSET_DOWN_DWELL_MS + 5000.0);
    assert!(c.offset_ms() < raised);
    assert!(c.offset_ms() > DELTA_GAP_GRACE_MS);
}

#[test]
fn grace_seeds_and_floors_at_the_same_value() {
    assert_eq!(GRACE_ENVELOPE.seed_ms, DELTA_GAP_GRACE_MS);
    assert_eq!(GRACE_ENVELOPE.min_ms, DELTA_GAP_GRACE_MS);
    assert_eq!(GRACE_ENVELOPE.max_ms, MAX_DELTA_GAP_GRACE_MS);
}
