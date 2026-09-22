//! The Opus encoder under R25's contract, verbatim (docs/38 D8 / docs/28):
//! 48 kHz/stereo FORCED (channel-mapping-family-1 multistream would break
//! WebCodecs against gawk's empty description), 128 kbps CONSTANT — not a
//! setting — 20 ms frames (one packet per datagram, never chunked),
//! `dtx=false` (silence must not look like loss), `inband_fec=false`,
//! restricted-low-delay. Portable: libopus builds everywhere, so the
//! packet-shape tests run on any host.

use gawk_engine::media::{AUDIO_BITRATE_BPS, AUDIO_CHANNELS, AUDIO_FRAME_MS, AUDIO_SAMPLE_RATE};

/// Samples per channel in one 20 ms frame at 48 kHz.
pub const FRAME_SAMPLES: usize = (AUDIO_SAMPLE_RATE as usize / 1000) * AUDIO_FRAME_MS as usize;
/// Interleaved stereo length of one frame.
pub const FRAME_INTERLEAVED_LEN: usize = FRAME_SAMPLES * AUDIO_CHANNELS as usize;
/// Encode output ceiling. One 20 ms packet at 128 kbps is ~320 B; anything
/// approaching the datagram budget is dropped upstream anyway.
const MAX_PACKET: usize = 1400;

pub struct OpusEncoder {
    inner: opus::Encoder,
}

impl OpusEncoder {
    pub fn new() -> Result<Self, String> {
        let mut inner = opus::Encoder::new(
            AUDIO_SAMPLE_RATE,
            opus::Channels::Stereo,
            // RESTRICTED_LOWDELAY: CELT-only, no mode switches, min delay.
            opus::Application::LowDelay,
        )
        .map_err(|e| format!("opus encoder create: {e}"))?;
        inner
            .set_bitrate(opus::Bitrate::Bits(AUDIO_BITRATE_BPS as i32))
            .map_err(|e| format!("opus bitrate: {e}"))?;
        // Constant means constant: CBR, so the wire cost is predictable and
        // silence is not distinguishable from speech by size.
        inner.set_vbr(false).map_err(|e| format!("opus cbr: {e}"))?;
        inner
            .set_dtx(false)
            .map_err(|e| format!("opus dtx off: {e}"))?;
        inner
            .set_inband_fec(false)
            .map_err(|e| format!("opus fec off: {e}"))?;
        Ok(Self { inner })
    }

    /// Encodes exactly one 20 ms interleaved-stereo frame into one packet.
    pub fn encode(&mut self, interleaved: &[f32]) -> Result<Vec<u8>, String> {
        if interleaved.len() != FRAME_INTERLEAVED_LEN {
            return Err(format!(
                "frame must be {FRAME_INTERLEAVED_LEN} interleaved samples, got {}",
                interleaved.len()
            ));
        }
        self.inner
            .encode_vec_float(interleaved, MAX_PACKET)
            .map_err(|e| format!("opus encode: {e}"))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::toc;

    fn sine_frame(phase0: f32) -> Vec<f32> {
        // 440 Hz stereo sine, loud enough that CBR has something to code.
        (0..FRAME_SAMPLES)
            .flat_map(|i| {
                let s = (phase0 + i as f32 * 440.0 * std::f32::consts::TAU / 48_000.0).sin() * 0.6;
                [s, s]
            })
            .collect()
    }

    // WB5 acceptance: "Opus packets: TOC says stereo + 20 ms … (decode
    // produced packets)".
    #[test]
    fn produced_packets_pass_the_toc_gate_and_decode() {
        let mut enc = OpusEncoder::new().unwrap();
        let fmt = crate::advertised_format("test");
        let mut dec = opus::Decoder::new(48_000, opus::Channels::Stereo).unwrap();
        for i in 0..10 {
            let pkt = enc.encode(&sine_frame(i as f32)).unwrap();
            assert!(!pkt.is_empty());
            toc::verify_against_config(&pkt, &fmt).unwrap();
            // Each packet decodes to exactly one 20 ms frame.
            let mut out = vec![0f32; FRAME_INTERLEAVED_LEN];
            let decoded = dec.decode_float(&pkt, &mut out, false).unwrap();
            assert_eq!(decoded, FRAME_SAMPLES);
        }
    }

    #[test]
    fn wrong_frame_length_is_refused() {
        let mut enc = OpusEncoder::new().unwrap();
        assert!(enc.encode(&vec![0f32; 960]).is_err());
    }

    // CBR sanity: packets of the same duration should be near-identical in
    // size (128 kbps × 20 ms ≈ 320 B).
    #[test]
    fn cbr_packets_hold_the_configured_rate() {
        let mut enc = OpusEncoder::new().unwrap();
        let sizes: Vec<usize> = (0..20)
            .map(|i| enc.encode(&sine_frame(i as f32)).unwrap().len())
            .collect();
        for &s in &sizes[3..] {
            assert!((300..=340).contains(&s), "packet size {s}");
        }
    }
}
