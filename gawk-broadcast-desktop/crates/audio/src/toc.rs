//! Opus TOC-byte inspection — the R25 Decision 10 gate, ported: the first
//! packet's bitstream is verified against the advertised AudioConfig, and
//! disagreement marks audio errored rather than shipping a lying config
//! (the viewer configures its decoder from the config; a mismatch decodes
//! garbage quietly, which is the worst kind of wrong).

use gawk_engine::media::AudioFormat;

/// What the TOC byte (RFC 6716 §3.1) says about a packet.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Toc {
    pub config: u8,
    pub stereo: bool,
    /// Frame-count code c (0 = one frame per packet).
    pub frame_count_code: u8,
    pub frame_duration_us: u32,
}

/// Parses the TOC byte of an Opus packet.
pub fn parse(packet: &[u8]) -> Option<Toc> {
    let toc = *packet.first()?;
    let config = toc >> 3;
    let frame_duration_us = match config {
        // SILK-only: 10/20/40/60 ms per config-within-group.
        0..=11 => [10_000, 20_000, 40_000, 60_000][(config % 4) as usize],
        // Hybrid: 10/20 ms.
        12..=15 => [10_000, 20_000][(config % 2) as usize],
        // CELT-only: 2.5/5/10/20 ms.
        _ => [2_500, 5_000, 10_000, 20_000][(config % 4) as usize],
    };
    Some(Toc {
        config,
        stereo: toc & 0b100 != 0,
        frame_count_code: toc & 0b11,
        frame_duration_us,
    })
}

/// The R25 Decision 10 check: the packet must be stereo, 20 ms, one frame
/// per packet — exactly what the AudioConfig advertises. Returns the
/// loud-log-worthy reason on mismatch.
pub fn verify_against_config(packet: &[u8], format: &AudioFormat) -> Result<(), String> {
    let toc = parse(packet).ok_or("empty first audio packet")?;
    if format.channels == 2 && !toc.stereo {
        return Err(format!(
            "config says stereo but the TOC byte (config {}) says mono",
            toc.config
        ));
    }
    if toc.frame_duration_us != 20_000 {
        return Err(format!(
            "config implies 20 ms frames but the TOC says {} µs",
            toc.frame_duration_us
        ));
    }
    if toc.frame_count_code != 0 {
        return Err(format!(
            "expected one frame per packet, TOC frame-count code is {}",
            toc.frame_count_code
        ));
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::advertised_format;

    #[test]
    fn parses_the_toc_fields() {
        // config 31 (CELT FB 20 ms), stereo, one frame: 11111_1_00.
        let toc = parse(&[0b1111_1100, 0xAA]).unwrap();
        assert_eq!(toc.config, 31);
        assert!(toc.stereo);
        assert_eq!(toc.frame_count_code, 0);
        assert_eq!(toc.frame_duration_us, 20_000);

        // config 17 (CELT NB 5 ms), mono, code 2: 10001_0_10.
        let toc = parse(&[0b1000_1010]).unwrap();
        assert!(!toc.stereo);
        assert_eq!(toc.frame_count_code, 2);
        assert_eq!(toc.frame_duration_us, 5_000);

        // SILK WB 60 ms = config 11: 01011_1_00.
        assert_eq!(parse(&[0b0101_1100]).unwrap().frame_duration_us, 60_000);
    }

    #[test]
    fn verification_pins_stereo_20ms_single_frame() {
        let f = advertised_format("test");
        assert!(verify_against_config(&[0b1111_1100], &f).is_ok());
        // Mono packet against a stereo config.
        assert!(
            verify_against_config(&[0b1111_1000], &f)
                .unwrap_err()
                .contains("mono")
        );
        // 10 ms frames (config 29): 11101_1_00.
        assert!(
            verify_against_config(&[0b1110_1100], &f)
                .unwrap_err()
                .contains("µs")
        );
        // Multi-frame packet.
        assert!(
            verify_against_config(&[0b1111_1101], &f)
                .unwrap_err()
                .contains("frame-count")
        );
        assert!(verify_against_config(&[], &f).is_err());
    }
}
