//! Linear PCM as a platform delivers it, into what the shared `Framer`
//! takes: interleaved stereo `f32` at 48 kHz (R52 MB4, docs/54 D6).
//!
//! ScreenCaptureKit describes each audio buffer with an
//! `AudioStreamBasicDescription`; the format is read from it on every
//! buffer, never assumed. Float32 or Int16, planar or interleaved, mono or
//! stereo are handled. A rate other than 48 kHz is refused rather than
//! resampled: the stream is configured for 48 kHz, so anything else is a
//! surprise worth an error (audio drops, video runs on — D6), not a code
//! path to guess at.

use gawk_engine::media::{AUDIO_CHANNELS, AUDIO_SAMPLE_RATE};

/// `kAudioFormatLinearPCM` ('lpcm').
pub const FORMAT_LINEAR_PCM: u32 = 0x6C70_636D;

// `AudioFormatFlags` bits (CoreAudioBaseTypes.h).
const FLAG_IS_FLOAT: u32 = 1 << 0;
const FLAG_IS_BIG_ENDIAN: u32 = 1 << 1;
const FLAG_IS_SIGNED_INTEGER: u32 = 1 << 2;
const FLAG_IS_NON_INTERLEAVED: u32 = 1 << 5;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Sample {
    F32,
    I16,
}

/// A PCM layout this shim can convert.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct PcmFormat {
    pub sample: Sample,
    pub channels: u32,
    /// One buffer per channel (planar) rather than one interleaved buffer.
    pub planar: bool,
}

impl PcmFormat {
    /// From an ASBD's fields; an error names what this shim cannot take.
    pub fn from_asbd(
        format_id: u32,
        flags: u32,
        sample_rate: f64,
        channels: u32,
        bits_per_channel: u32,
    ) -> Result<Self, String> {
        if format_id != FORMAT_LINEAR_PCM {
            return Err(format!("audio is not linear PCM (format {format_id:#x})"));
        }
        if (sample_rate - f64::from(AUDIO_SAMPLE_RATE)).abs() > 0.5 {
            return Err(format!(
                "audio arrives at {sample_rate} Hz, not {AUDIO_SAMPLE_RATE} Hz"
            ));
        }
        if flags & FLAG_IS_BIG_ENDIAN != 0 {
            return Err("big-endian PCM".into());
        }
        if !(1..=2).contains(&channels) {
            return Err(format!("{channels}-channel audio"));
        }
        let sample = match (flags & FLAG_IS_FLOAT != 0, bits_per_channel) {
            (true, 32) => Sample::F32,
            (false, 16) if flags & FLAG_IS_SIGNED_INTEGER != 0 => Sample::I16,
            (float, bits) => {
                return Err(format!(
                    "{bits}-bit {} PCM",
                    if float { "float" } else { "integer" }
                ));
            }
        };
        Ok(Self {
            sample,
            channels,
            planar: flags & FLAG_IS_NON_INTERLEAVED != 0,
        })
    }

    fn bytes_per_sample(self) -> usize {
        match self.sample {
            Sample::F32 => 4,
            Sample::I16 => 2,
        }
    }
}

/// `frames` sample frames from `buffers` (one per channel when planar, one
/// in total when interleaved) as interleaved stereo `f32`. Mono is
/// duplicated to both channels. An error for buffers shorter than `frames`
/// claims — never a partial block, which would shift every later sample.
pub fn to_stereo_interleaved(
    fmt: PcmFormat,
    buffers: &[&[u8]],
    frames: usize,
) -> Result<Vec<f32>, String> {
    let want_buffers = if fmt.planar { fmt.channels as usize } else { 1 };
    if buffers.len() < want_buffers {
        return Err(format!(
            "{} audio buffer(s) for a {}-channel {} layout",
            buffers.len(),
            fmt.channels,
            if fmt.planar { "planar" } else { "interleaved" }
        ));
    }
    let bps = fmt.bytes_per_sample();
    let per_buffer = if fmt.planar {
        frames * bps
    } else {
        frames * bps * fmt.channels as usize
    };
    if buffers[..want_buffers].iter().any(|b| b.len() < per_buffer) {
        return Err("audio buffer shorter than its frame count".into());
    }
    let read = |buf: &[u8], idx: usize| -> f32 {
        let at = idx * bps;
        match fmt.sample {
            Sample::F32 => f32::from_le_bytes(buf[at..at + 4].try_into().unwrap()),
            Sample::I16 => {
                f32::from(i16::from_le_bytes(buf[at..at + 2].try_into().unwrap())) / 32768.0
            }
        }
    };
    let out_channels = AUDIO_CHANNELS as usize;
    let mut out = Vec::with_capacity(frames * out_channels);
    for f in 0..frames {
        for c in 0..out_channels {
            let src_c = c.min(fmt.channels as usize - 1); // mono → both
            let v = if fmt.planar {
                read(buffers[src_c], f)
            } else {
                read(buffers[0], f * fmt.channels as usize + src_c)
            };
            out.push(v);
        }
    }
    Ok(out)
}

#[cfg(test)]
mod tests {
    use super::*;

    const F32_PLANAR: u32 = FLAG_IS_FLOAT | FLAG_IS_NON_INTERLEAVED | (1 << 3); // + packed
    const F32_INTERLEAVED: u32 = FLAG_IS_FLOAT | (1 << 3);
    const I16_INTERLEAVED: u32 = FLAG_IS_SIGNED_INTEGER | (1 << 3);

    fn f32s(v: &[f32]) -> Vec<u8> {
        v.iter().flat_map(|x| x.to_le_bytes()).collect()
    }

    #[test]
    fn what_sck_reports_float32_planar_stereo() {
        let fmt = PcmFormat::from_asbd(FORMAT_LINEAR_PCM, F32_PLANAR, 48_000.0, 2, 32).unwrap();
        assert_eq!(
            fmt,
            PcmFormat {
                sample: Sample::F32,
                channels: 2,
                planar: true
            }
        );
        let left = f32s(&[0.1, 0.2, 0.3]);
        let right = f32s(&[-0.1, -0.2, -0.3]);
        let out = to_stereo_interleaved(fmt, &[&left, &right], 3).unwrap();
        assert_eq!(out, vec![0.1, -0.1, 0.2, -0.2, 0.3, -0.3]);
    }

    #[test]
    fn float32_interleaved_passes_through() {
        let fmt =
            PcmFormat::from_asbd(FORMAT_LINEAR_PCM, F32_INTERLEAVED, 48_000.0, 2, 32).unwrap();
        assert!(!fmt.planar);
        let buf = f32s(&[0.5, -0.5, 0.25, -0.25]);
        assert_eq!(
            to_stereo_interleaved(fmt, &[&buf], 2).unwrap(),
            vec![0.5, -0.5, 0.25, -0.25]
        );
    }

    #[test]
    fn int16_interleaved_scales_to_unit_range() {
        let fmt =
            PcmFormat::from_asbd(FORMAT_LINEAR_PCM, I16_INTERLEAVED, 48_000.0, 2, 16).unwrap();
        let buf: Vec<u8> = [i16::MIN, 16384, 0, -16384]
            .iter()
            .flat_map(|x| x.to_le_bytes())
            .collect();
        assert_eq!(
            to_stereo_interleaved(fmt, &[&buf], 2).unwrap(),
            vec![-1.0, 0.5, 0.0, -0.5]
        );
    }

    #[test]
    fn mono_is_duplicated_to_both_channels() {
        let fmt = PcmFormat::from_asbd(FORMAT_LINEAR_PCM, F32_PLANAR, 48_000.0, 1, 32).unwrap();
        let mono = f32s(&[0.7, 0.8]);
        assert_eq!(
            to_stereo_interleaved(fmt, &[&mono], 2).unwrap(),
            vec![0.7, 0.7, 0.8, 0.8]
        );
    }

    #[test]
    fn surprises_are_refused_not_guessed() {
        let e = |r: Result<PcmFormat, String>| r.unwrap_err();
        assert!(
            e(PcmFormat::from_asbd(
                0x61616320, F32_PLANAR, 48_000.0, 2, 32
            ))
            .contains("not linear PCM")
        );
        assert!(
            e(PcmFormat::from_asbd(
                FORMAT_LINEAR_PCM,
                F32_PLANAR,
                44_100.0,
                2,
                32
            ))
            .contains("44100")
        );
        assert!(
            e(PcmFormat::from_asbd(
                FORMAT_LINEAR_PCM,
                F32_PLANAR,
                48_000.0,
                6,
                32
            ))
            .contains("6-channel")
        );
        assert!(
            e(PcmFormat::from_asbd(
                FORMAT_LINEAR_PCM,
                FLAG_IS_FLOAT | FLAG_IS_BIG_ENDIAN,
                48_000.0,
                2,
                32
            ))
            .contains("big-endian")
        );
        assert!(
            e(PcmFormat::from_asbd(
                FORMAT_LINEAR_PCM,
                FLAG_IS_SIGNED_INTEGER,
                48_000.0,
                2,
                24
            ))
            .contains("24-bit")
        );
    }

    #[test]
    fn short_or_missing_buffers_are_an_error_not_a_partial_block() {
        let fmt = PcmFormat::from_asbd(FORMAT_LINEAR_PCM, F32_PLANAR, 48_000.0, 2, 32).unwrap();
        let left = f32s(&[0.1, 0.2]);
        assert!(
            to_stereo_interleaved(fmt, &[&left], 2).is_err(),
            "one plane of two"
        );
        let right = f32s(&[0.1]);
        assert!(
            to_stereo_interleaved(fmt, &[&left, &right], 2).is_err(),
            "short plane"
        );
        assert_eq!(
            to_stereo_interleaved(fmt, &[&left, &left], 0).unwrap(),
            Vec::<f32>::new()
        );
    }
}
