//! The encoder cascade and its trial gate (docs/38 D9), portable by
//! design: candidates and trials enter through seams so every policy —
//! "enumeration is not acceptance", last-good-first, advance-on-failure,
//! refusal when nothing survives — is a unit test with scripted trials.
//! The Windows half (`mft`) supplies real MFT enumeration and real trial
//! encodes; R14's core probing lesson (a listed element is not a working
//! element) transfers verbatim.

use crate::h264;

/// One enumerated hardware encoder, pre-trial. `id` is the stable key the
/// last-good cache stores (the MFT's friendly name — vendor-stable, and
/// legible in the config file and diagnostics).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Candidate {
    pub id: String,
}

/// One access unit out of a trial encode.
#[derive(Debug, Clone)]
pub struct TrialAu {
    pub data: Vec<u8>,
    /// Sample time in 100 ns ticks, as the MFT stamped it.
    pub time_100ns: i64,
}

/// Everything a trial run produced, for the invariant checks.
#[derive(Debug, Clone, Default)]
pub struct TrialRun {
    pub inputs_fed: usize,
    pub aus: Vec<TrialAu>,
    /// Input timestamps, in feed order (VFR pass-through check).
    pub input_times_100ns: Vec<i64>,
    /// The index (into inputs) where a mid-trial IDR was forced.
    pub forced_idr_at: Option<usize>,
    /// `MF_MT_MPEG_SEQUENCE_HEADER` from the negotiated output type, if the
    /// MFT exposes one — the prepend cache when in-band headers are absent.
    pub sequence_header: Vec<u8>,
}

/// What surviving the trial gate proves (D9's invariant table).
#[derive(Debug, Clone, PartialEq)]
pub struct Accepted {
    pub id: String,
    /// SPS-derived, never assumed (D10).
    pub codec_string: String,
    /// Cached SPS/PPS to prepend before IDRs when the vendor MFT omits
    /// in-band headers (V-6); empty when in-band headers are present.
    pub prepend_headers: Vec<u8>,
}

/// Runs one real trial encode for a candidate. The Windows implementation
/// feeds ~30 synthetic NV12 frames; tests script arbitrary outcomes.
pub trait TrialRunner {
    fn run(&mut self, candidate: &Candidate) -> Result<TrialRun, String>;
}

/// D9's refusal, verbatim modulo the resolved app URL: the message differs
/// from Linux because the REASON differs — on Windows the browser hardware-
/// encodes fine; what this app adds is capture fidelity.
pub fn refusal_message(app_url: &str) -> String {
    format!(
        "No hardware H.264 encoder was found, so gawk-broadcast can't start — \
it deliberately has no software encoder. The browser broadcaster at {app_url} \
hardware-encodes fine on Windows; what you lose without this app is \
per-application audio and background-window capture."
    )
}

/// Why the cascade refused (G3). Carries the enumerated-but-rejected trail
/// for diagnostics.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct NoHardwareEncoder {
    pub tried: Vec<(String, String)>,
}

/// The cascade: last-good first (re-verified, never trusted), then every
/// remaining candidate in enumeration order; first to survive the trial
/// gate wins; none surviving is the refusal path.
pub fn choose(
    candidates: &[Candidate],
    last_good: Option<&str>,
    trial: &mut dyn TrialRunner,
) -> Result<Accepted, NoHardwareEncoder> {
    let mut order: Vec<&Candidate> = Vec::with_capacity(candidates.len());
    if let Some(lg) = last_good
        && let Some(c) = candidates.iter().find(|c| c.id == lg)
    {
        order.push(c);
    }
    order.extend(
        candidates
            .iter()
            .filter(|c| Some(c.id.as_str()) != last_good),
    );

    let mut tried = Vec::new();
    for c in order {
        log::info!(
            "trial: {}{}",
            c.id,
            if Some(c.id.as_str()) == last_good {
                " (last-good, re-verified first)"
            } else {
                ""
            }
        );
        match trial.run(c).and_then(|run| validate_trial(&run)) {
            Ok(v) => {
                log::info!(
                    "accepted: {} (codec {}, {} prepend-header bytes)",
                    c.id,
                    v.codec_string,
                    v.prepend_headers.len()
                );
                return Ok(Accepted {
                    id: c.id.clone(),
                    codec_string: v.codec_string,
                    prepend_headers: v.prepend_headers,
                });
            }
            Err(why) => {
                log::warn!("rejected: {}: {why}", c.id);
                tried.push((c.id.clone(), why));
            }
        }
    }
    Err(NoHardwareEncoder { tried })
}

/// The trial verdict, before identity is attached.
#[derive(Debug, Clone, PartialEq)]
pub struct Validated {
    pub codec_string: String,
    pub prepend_headers: Vec<u8>,
}

/// Enforces the D9 invariant table on a trial run. Every rejection names
/// its invariant — the `tried` trail is a diagnostic, not a shrug.
pub fn validate_trial(run: &TrialRun) -> Result<Validated, String> {
    if run.aus.is_empty() {
        return Err("trial produced no output".into());
    }

    // ≤ 1 frame encoder-internal latency at drain (AVLowLatencyMode).
    if run.inputs_fed > run.aus.len() + 1 {
        return Err(format!(
            "encoder retains {} frames at drain (low-latency mode not honored)",
            run.inputs_fed - run.aus.len()
        ));
    }

    // No B-frames: decode order == presentation order, twice over — output
    // times strictly monotonic AND no B slice in any AU.
    for pair in run.aus.windows(2) {
        if pair[1].time_100ns <= pair[0].time_100ns {
            return Err(
                "output sample times not strictly monotonic (reordering ⇒ B-frames)".into(),
            );
        }
    }
    for au in &run.aus {
        if h264::has_b_slices(&au.data) {
            return Err(
                "B slice in trial bitstream (AVEncMPVDefaultBPictureCount not honored)".into(),
            );
        }
    }

    // VFR pass-through: output timestamps are the input timestamps, not a
    // rewrite onto a nominal grid.
    for au in &run.aus {
        if !run.input_times_100ns.contains(&au.time_100ns) {
            return Err(format!(
                "output timestamp {} is not one of the input timestamps (VFR rewritten)",
                au.time_100ns
            ));
        }
    }

    // IDR spacing: with GOP = fps/2 the trial's ~30 frames must contain at
    // least two IDRs (the first AU plus at least one cadence IDR).
    let idr_flags: Vec<bool> = run.aus.iter().map(|au| h264::has_idr(&au.data)).collect();
    if !idr_flags[0] {
        return Err("first trial AU is not an IDR".into());
    }
    if idr_flags.iter().filter(|&&k| k).count() < 2 {
        return Err("no cadence IDR within the trial (GOP setting not honored)".into());
    }

    // On-demand IDR (resume re-prime, D5): the forced frame must yield an
    // IDR at-or-shortly-after the forcing point.
    if let Some(at) = run.forced_idr_at {
        let window = idr_flags.iter().skip(at).take(3);
        if !window.into_iter().any(|&k| k) {
            return Err("forced keyframe did not produce an IDR".into());
        }
    }

    // SPS/PPS before every IDR — in-band, or cached headers to prepend
    // (load-bearing because extradata is empty on the Annex-B path, D10).
    let missing_inband = run
        .aus
        .iter()
        .any(|au| h264::has_idr(&au.data) && !h264::has_sps_pps(&au.data));
    let prepend_headers = if missing_inband {
        if run.sequence_header.is_empty() || !h264::has_sps_pps(&run.sequence_header) {
            return Err(
                "IDRs lack in-band SPS/PPS and the MFT exposes no sequence header to prepend"
                    .into(),
            );
        }
        run.sequence_header.clone()
    } else {
        Vec::new()
    };

    // The codec string comes from the bitstream (D10) — from the first SPS
    // wherever it lives (in-band or the cached headers).
    let codec_string = run
        .aus
        .iter()
        .find_map(|au| h264::parse_codec_string(&au.data))
        .or_else(|| h264::parse_codec_string(&run.sequence_header))
        .ok_or("no SPS anywhere in the trial output")?;

    Ok(Validated {
        codec_string,
        prepend_headers,
    })
}

/// The IDR header-prepend path (D10/V-6): cached SPS/PPS ahead of an IDR AU
/// that lacks its own. Deltas and self-describing IDRs pass through
/// untouched.
pub fn ensure_idr_headers(au: Vec<u8>, is_idr: bool, cached: &[u8]) -> Vec<u8> {
    if !is_idr || cached.is_empty() || h264::has_sps_pps(&au) {
        return au;
    }
    let mut out = Vec::with_capacity(cached.len() + au.len());
    out.extend_from_slice(cached);
    out.extend_from_slice(&au);
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    // Annex-B building blocks (start code + NAL header byte + payload).
    fn nal(ty: u8, payload: &[u8]) -> Vec<u8> {
        let mut v = vec![0, 0, 0, 1, ty];
        v.extend_from_slice(payload);
        v
    }
    fn sps() -> Vec<u8> {
        // profile 66 (0x42), constraints 0xE0, level 42 (0x2A).
        nal(0x67, &[0x42, 0xE0, 0x2A, 0x8D])
    }
    fn pps() -> Vec<u8> {
        nal(0x68, &[0xCE, 0x38, 0x80])
    }
    fn idr_slice() -> Vec<u8> {
        // slice_type: ue(first_mb)=0 (bit 1), ue(slice_type)=7 → I. Bits:
        // 1, 0001000 → 0x88.
        nal(0x65, &[0x88, 0x80])
    }
    fn p_slice() -> Vec<u8> {
        // ue(first_mb)=0, ue(slice_type)=0 → P: bits 1,1 → 0xC0.
        nal(0x41, &[0xC0, 0x80])
    }
    fn b_slice() -> Vec<u8> {
        // ue(first_mb)=0, ue(slice_type)=1 → B: bits 1,010 → 0xA8? →
        // 1 then 010 = 0b1010_0000 = 0xA0.
        nal(0x41, &[0xA0, 0x80])
    }

    fn idr_with_headers() -> Vec<u8> {
        let mut v = sps();
        v.extend(pps());
        v.extend(idr_slice());
        v
    }

    fn healthy_run() -> TrialRun {
        // 30 in, 30 out, IDR at 0 and 15, headers in-band, forced IDR at 20
        // honored by making frame 20 an IDR too.
        let mut aus = Vec::new();
        let mut inputs = Vec::new();
        for i in 0..30i64 {
            let t = i * 166_667;
            inputs.push(t);
            let data = if i == 0 || i == 15 || i == 20 {
                idr_with_headers()
            } else {
                p_slice()
            };
            aus.push(TrialAu {
                data,
                time_100ns: t,
            });
        }
        TrialRun {
            inputs_fed: 30,
            aus,
            input_times_100ns: inputs,
            forced_idr_at: Some(20),
            sequence_header: Vec::new(),
        }
    }

    #[test]
    fn a_healthy_trial_is_accepted_with_the_sps_codec_string() {
        let v = validate_trial(&healthy_run()).unwrap();
        assert_eq!(v.codec_string, "avc1.42E02A");
        assert!(v.prepend_headers.is_empty());
    }

    #[test]
    fn each_invariant_violation_is_named() {
        // B slice.
        let mut run = healthy_run();
        run.aus[5].data = b_slice();
        assert!(validate_trial(&run).unwrap_err().contains("B slice"));

        // Reordered output times.
        let mut run = healthy_run();
        run.aus[6].time_100ns = run.aus[4].time_100ns - 1;
        assert!(validate_trial(&run).unwrap_err().contains("monotonic"));

        // Latency: retained frames at drain.
        let mut run = healthy_run();
        run.aus.truncate(27);
        assert!(validate_trial(&run).unwrap_err().contains("retains"));

        // Rewritten timestamps.
        let mut run = healthy_run();
        run.aus[3].time_100ns += 1;
        // keep monotonicity intact for this mutation
        assert!(
            validate_trial(&run)
                .unwrap_err()
                .contains("not one of the input")
        );

        // No cadence IDR.
        let mut run = healthy_run();
        run.aus[15].data = p_slice();
        run.aus[20].data = p_slice();
        run.forced_idr_at = None;
        assert!(validate_trial(&run).unwrap_err().contains("cadence IDR"));

        // Forced IDR ignored.
        let mut run = healthy_run();
        run.aus[20].data = p_slice();
        assert!(
            validate_trial(&run)
                .unwrap_err()
                .contains("forced keyframe")
        );
    }

    #[test]
    fn missing_inband_headers_fall_back_to_the_sequence_header() {
        let mut run = healthy_run();
        for i in [0usize, 15, 20] {
            run.aus[i].data = idr_slice(); // IDR without SPS/PPS
        }
        // Without a sequence header: rejected, not papered over.
        assert!(
            validate_trial(&run)
                .unwrap_err()
                .contains("sequence header")
        );

        // With one: accepted, headers cached for prepending, codec string
        // parsed from the cached SPS.
        let mut hdr = sps();
        hdr.extend(pps());
        run.sequence_header = hdr.clone();
        let v = validate_trial(&run).unwrap();
        assert_eq!(v.prepend_headers, hdr);
        assert_eq!(v.codec_string, "avc1.42E02A");
    }

    #[test]
    fn prepend_path_fixes_bare_idrs_and_leaves_the_rest_alone() {
        let mut hdr = sps();
        hdr.extend(pps());

        let bare = idr_slice();
        let fixed = ensure_idr_headers(bare.clone(), true, &hdr);
        assert!(crate::h264::has_sps_pps(&fixed));
        assert!(fixed.ends_with(&bare));

        // Self-describing IDR: untouched.
        let described = idr_with_headers();
        assert_eq!(ensure_idr_headers(described.clone(), true, &hdr), described);
        // Delta: untouched.
        let delta = p_slice();
        assert_eq!(ensure_idr_headers(delta.clone(), false, &hdr), delta);
    }

    struct Scripted(Vec<(String, Result<TrialRun, String>)>);
    impl TrialRunner for Scripted {
        fn run(&mut self, c: &Candidate) -> Result<TrialRun, String> {
            let i = self
                .0
                .iter()
                .position(|(id, _)| *id == c.id)
                .unwrap_or_else(|| panic!("unscripted candidate {}", c.id));
            self.0[i].1.clone()
        }
    }

    fn cands(ids: &[&str]) -> Vec<Candidate> {
        ids.iter()
            .map(|s| Candidate { id: s.to_string() })
            .collect()
    }

    #[test]
    fn refusal_when_enumeration_is_empty_g3() {
        let mut trial = Scripted(vec![]);
        let err = choose(&[], None, &mut trial).unwrap_err();
        assert!(err.tried.is_empty());
        // And the message is D9's, pointing at the browser.
        let msg = refusal_message("https://gawk.ioio.fi");
        assert!(msg.contains("deliberately has no software encoder"));
        assert!(msg.contains("https://gawk.ioio.fi"));
        assert!(msg.contains("per-application audio"));
    }

    #[test]
    fn last_good_is_reverified_first_then_the_cascade_advances() {
        let healthy = healthy_run();
        let mut trial = Scripted(vec![
            ("NVIDIA H264 Encoder MFT".into(), Err("driver broke".into())),
            ("AMD VCE H.264".into(), Ok(healthy.clone())),
        ]);
        // Last-good NVIDIA fails its re-verification; the cascade advances
        // to AMD instead of refusing.
        let chosen = choose(
            &cands(&["AMD VCE H.264", "NVIDIA H264 Encoder MFT"]),
            Some("NVIDIA H264 Encoder MFT"),
            &mut trial,
        )
        .unwrap();
        assert_eq!(chosen.id, "AMD VCE H.264");
    }

    #[test]
    fn nothing_surviving_refuses_with_the_tried_trail() {
        let mut trial = Scripted(vec![
            ("A".into(), Err("no output".into())),
            ("B".into(), Err("b-frames".into())),
        ]);
        let err = choose(&cands(&["A", "B"]), None, &mut trial).unwrap_err();
        assert_eq!(err.tried.len(), 2);
        assert_eq!(err.tried[0].0, "A");
    }
}
