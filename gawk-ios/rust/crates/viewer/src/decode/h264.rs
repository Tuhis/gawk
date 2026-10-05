//! H.264 passthrough (docs/67 D15): H.264 is never decoded here. Swift
//! enqueues it compressed to an `AVSampleBufferDisplayLayer`, which decodes
//! in hardware, and this module hands Swift the two things that needs: the
//! parameter sets (an `avcC` record, or the SPS and PPS lists) for
//! `CMVideoFormatDescriptionCreateFromH264ParameterSets`, and each access
//! unit as a length-prefixed (AVCC) sample.
//!
//! Broadcasters send one of two shapes, and **the `DecoderConfig` decides
//! which, never the frame** (`gawk-app/src/transport/viewer.ts`):
//!
//! - **AVCC**, from browser broadcasters: the codec is `avc1.*` and the
//!   extradata is an `avcC` record (version byte `0x01`). Samples are already
//!   length-prefixed and pass through untouched. The record is normalized
//!   first ([`normalize_avcc_extradata`]) and the codec string re-derived
//!   from its profile bytes when the two disagree, as the SPA does.
//! - **Annex-B**, from the native broadcasters (docs/38 D9, docs/54 D8):
//!   empty extradata, start codes, and SPS/PPS in band before every IDR.
//!   The parameter sets are lifted out of the stream into the format
//!   description, and the remaining NALs are re-framed with 4-byte lengths.
//!
//! An AVCC length prefix for a 256–511-byte NAL reads `00 00 01 xx`, exactly
//! an Annex-B start code, which is why sniffing the frame is wrong.
//!
//! The Annex-B NAL splitter is the desktop broadcasters' own
//! (`gawk_encode::h264::annex_b_nals`), so the viewer and the encoders agree
//! on what a NAL is. The TS vectors are restated in the tests below, never
//! imported.

use std::fmt;

use gawk_encode::h264::annex_b_nals;

const NAL_SPS: u8 = 7;
const NAL_PPS: u8 = 8;
const NAL_AUD: u8 = 9;

/// The NAL length size of the samples this module writes for Annex-B input,
/// and of the `avcC` it builds for them.
pub const ANNEX_B_OUTPUT_LENGTH_SIZE: u8 = 4;

/// Whether a `DecoderConfig` describes AVCC: the codec is `avc1*` and the
/// extradata is an `avcC` record. Anything else H.264 is Annex-B.
pub fn is_avcc(codec: &str, extradata: &[u8]) -> bool {
    codec.starts_with("avc1") && extradata.first() == Some(&0x01)
}

/// Rewrites a possibly out-of-spec `avcC` record into one decoders accept:
/// the reserved bits forced to 1 (Chrome requires them) and the Firefox
/// double-byte SPS/PPS bug undone (the NAL header byte duplicated, shifting
/// the real `profile_idc`; shipped by Firefox broadcasters). A port of
/// `gawk-app/src/transport/avcc.ts`, quirks included: a record that isn't an
/// `avcC` comes back unchanged, and bytes after the PPS list (the High
/// profile extension) are not carried over.
pub fn normalize_avcc_extradata(extradata: &[u8]) -> Vec<u8> {
    let e = extradata;
    if e.len() < 7 || e[0] != 0x01 {
        return e.to_vec();
    }
    // Version, profile, compatibility, level; then the reserved bits.
    let mut out = e[..4].to_vec();
    out.push(e[4] | 0xfc);
    let num_sps = e[5] & 0x1f;
    out.push(num_sps | 0xe0);

    let mut off = 6;
    let mut sps_was_buggy = false;
    for _ in 0..num_sps {
        let Some(nal) = next_record_nal(e, &mut off) else {
            break;
        };
        // The bug's signature: the SPS header byte doubled, so the real
        // profile_idc (which the avcC header also carries) sits at index 2.
        let fixed =
            if nal.len() > 2 && nal[0] & 0x1f == NAL_SPS && nal[0] == nal[1] && nal[2] == e[1] {
                sps_was_buggy = true;
                &nal[1..]
            } else {
                nal
            };
        push_record_nal(&mut out, fixed);
    }

    if off < e.len() {
        let num_pps = e[off];
        off += 1;
        out.push(num_pps);
        for _ in 0..num_pps {
            let Some(nal) = next_record_nal(e, &mut off) else {
                break;
            };
            // Undone only when the SPS showed the bug.
            let fixed =
                if sps_was_buggy && nal.len() > 2 && nal[0] & 0x1f == NAL_PPS && nal[0] == nal[1] {
                    &nal[1..]
                } else {
                    nal
                };
            push_record_nal(&mut out, fixed);
        }
    }
    out
}

/// Reads one 16-bit-length-prefixed NAL out of an `avcC` record, advancing
/// `off`; `None` (with `off` where avcc.ts leaves it) if it doesn't fit.
fn next_record_nal<'a>(record: &'a [u8], off: &mut usize) -> Option<&'a [u8]> {
    let len_bytes = record.get(*off..*off + 2)?;
    let len = usize::from(u16::from_be_bytes([len_bytes[0], len_bytes[1]]));
    *off += 2;
    let nal = record.get(*off..*off + len)?;
    *off += len;
    Some(nal)
}

fn push_record_nal(out: &mut Vec<u8>, nal: &[u8]) {
    out.extend_from_slice(&(nal.len() as u16).to_be_bytes());
    out.extend_from_slice(nal);
}

/// The RFC 6381 codec string from an `avcC`'s profile, compatibility and
/// level bytes (`avc1.64001F`), as the SPA derives it. `None` if the record
/// is too short to hold them.
pub fn codec_from_avcc(avcc: &[u8]) -> Option<String> {
    let b = avcc.get(1..4)?;
    Some(format!("avc1.{:02X}{:02X}{:02X}", b[0], b[1], b[2]))
}

/// Splits a length-prefixed sample into NAL payloads. A length running past
/// the end is an error, never a truncated NAL.
pub fn split_avcc(data: &[u8], length_size: u8) -> Result<Vec<&[u8]>, H264Error> {
    let n = usize::from(length_size);
    let mut nals = Vec::new();
    let mut off = 0;
    while off < data.len() {
        let len = data
            .get(off..off + n)
            .ok_or(H264Error::MalformedAvcc)?
            .iter()
            .fold(0usize, |acc, &b| (acc << 8) | usize::from(b));
        off += n;
        nals.push(data.get(off..off + len).ok_or(H264Error::MalformedAvcc)?);
        off += len;
    }
    Ok(nals)
}

/// Parses an `avcC` record's length size and SPS/PPS lists.
fn parse_avcc_record(record: &[u8]) -> Result<(u8, ParameterSets), H264Error> {
    let bad = H264Error::MalformedAvccRecord;
    if record.len() < 7 || record[0] != 0x01 {
        return Err(bad);
    }
    let length_size = (record[4] & 0x03) + 1;
    let mut off = 6;
    let mut sps = Vec::new();
    for _ in 0..record[5] & 0x1f {
        sps.push(
            next_record_nal(record, &mut off)
                .ok_or(bad.clone())?
                .to_vec(),
        );
    }
    let num_pps = *record.get(off).ok_or(bad.clone())?;
    off += 1;
    let mut pps = Vec::new();
    for _ in 0..num_pps {
        pps.push(
            next_record_nal(record, &mut off)
                .ok_or(bad.clone())?
                .to_vec(),
        );
    }
    if sps.is_empty() || pps.is_empty() {
        return Err(bad);
    }
    // ISO/IEC 14496-15 allows NAL lengths of 1, 2 or 4 bytes; a 3 can't be
    // handed to CoreMedia.
    if length_size == 3 {
        return Err(bad);
    }
    Ok((length_size, ParameterSets { sps, pps }))
}

/// Pulls the SPS and PPS out of Annex-B NALs; `None` unless both appear.
fn in_band_parameter_sets(buf: &[u8]) -> Option<ParameterSets> {
    let mut ps = ParameterSets {
        sps: Vec::new(),
        pps: Vec::new(),
    };
    for nal in annex_b_nals(buf) {
        match nal.first().map(|b| b & 0x1f) {
            Some(NAL_SPS) => ps.sps.push(nal.to_vec()),
            Some(NAL_PPS) => ps.pps.push(nal.to_vec()),
            _ => {}
        }
    }
    (!ps.sps.is_empty() && !ps.pps.is_empty()).then_some(ps)
}

/// Why a config or an access unit can't become a sample.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum H264Error {
    /// An AVCC length prefix that is cut short or runs past the sample.
    MalformedAvcc,
    /// An `avcC` record whose SPS or PPS list doesn't parse.
    MalformedAvccRecord,
    /// An access unit with no NAL in it.
    EmptyAccessUnit,
    /// An Annex-B access unit before any SPS/PPS has been seen: there is no
    /// format description to enqueue it under. The delta-loss rule (D13)
    /// waits for a keyframe, which carries them.
    NoParameterSets,
}

impl fmt::Display for H264Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(match self {
            Self::MalformedAvcc => "h264: malformed AVCC length prefix",
            Self::MalformedAvccRecord => "h264: malformed avcC record",
            Self::EmptyAccessUnit => "h264: access unit has no NAL units",
            Self::NoParameterSets => "h264: no SPS/PPS seen yet",
        })
    }
}

impl std::error::Error for H264Error {}

/// The bitstream shape a `DecoderConfig` announced.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum BitstreamFormat {
    /// Length-prefixed samples with `length_size`-byte prefixes (1, 2 or 4).
    Avcc {
        length_size: u8,
    },
    AnnexB,
}

/// SPS and PPS NAL units (without start codes or lengths), in order: what
/// `CMVideoFormatDescriptionCreateFromH264ParameterSets` takes, with the
/// NAL length size of the samples.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ParameterSets {
    pub sps: Vec<Vec<u8>>,
    pub pps: Vec<Vec<u8>>,
}

impl ParameterSets {
    /// An `avcC` record over these parameter sets with `length_size`-byte
    /// NAL lengths, for a format description built from the `avcC` atom.
    /// Profile, compatibility and level come from the first SPS.
    pub fn avcc(&self, length_size: u8) -> Vec<u8> {
        let first = self.sps.first().map(Vec::as_slice).unwrap_or_default();
        let header = |i: usize| first.get(i).copied().unwrap_or(0);
        let mut out = vec![
            0x01,
            header(1), // profile_idc
            header(2), // constraint flags
            header(3), // level_idc
            0xfc | (length_size.saturating_sub(1) & 0x03),
            0xe0 | (self.sps.len() as u8 & 0x1f),
        ];
        for sps in &self.sps {
            push_record_nal(&mut out, sps);
        }
        out.push(self.pps.len() as u8);
        for pps in &self.pps {
            push_record_nal(&mut out, pps);
        }
        out
    }
}

/// One access unit, ready to wrap in a `CMSampleBuffer`.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Sample {
    /// Length-prefixed NAL units, with the stream's NAL length size.
    pub data: Vec<u8>,
    /// The format description must be (re)built from
    /// [`H264Stream::parameter_sets`] before this sample is enqueued: always
    /// for the stream's first sample, then whenever the in-band SPS/PPS
    /// change (Annex-B only; an AVCC stream's are fixed by its config).
    pub format_changed: bool,
}

/// The passthrough state for one `DecoderConfig`. Like the decoders, it is
/// rebuilt on a config change (D13).
#[derive(Debug, Clone)]
pub struct H264Stream {
    codec: String,
    format: BitstreamFormat,
    avcc: Option<Vec<u8>>,
    parameter_sets: Option<ParameterSets>,
    /// Whether a sample has gone out, so the first one reports the format.
    announced: bool,
}

impl H264Stream {
    /// Reads a `DecoderConfig`'s codec string and extradata.
    pub fn new(codec: &str, extradata: &[u8]) -> Result<Self, H264Error> {
        if !is_avcc(codec, extradata) {
            return Ok(Self {
                codec: codec.to_owned(),
                format: BitstreamFormat::AnnexB,
                avcc: None,
                parameter_sets: in_band_parameter_sets(extradata),
                announced: false,
            });
        }
        let avcc = normalize_avcc_extradata(extradata);
        let (length_size, parameter_sets) = parse_avcc_record(&avcc)?;
        // Trust the bitstream over the negotiation (docs/19 Decision 8): the
        // encoder may have picked another profile or level.
        let codec = match codec_from_avcc(&avcc) {
            Some(actual) if !actual.eq_ignore_ascii_case(codec) => actual,
            _ => codec.to_owned(),
        };
        Ok(Self {
            codec,
            format: BitstreamFormat::Avcc { length_size },
            avcc: Some(avcc),
            parameter_sets: Some(parameter_sets),
            announced: false,
        })
    }

    /// The codec string to report: the config's, or the one the `avcC`
    /// says when they disagree (AVCC only).
    pub fn codec(&self) -> &str {
        &self.codec
    }

    pub fn format(&self) -> BitstreamFormat {
        self.format
    }

    /// The NAL length size of the samples [`H264Stream::sample`] returns.
    pub fn nal_length_size(&self) -> u8 {
        match self.format {
            BitstreamFormat::Avcc { length_size } => length_size,
            BitstreamFormat::AnnexB => ANNEX_B_OUTPUT_LENGTH_SIZE,
        }
    }

    /// The parameter sets in force: the `avcC`'s, or the last in-band ones.
    pub fn parameter_sets(&self) -> Option<&ParameterSets> {
        self.parameter_sets.as_ref()
    }

    /// The `avcC` record in force: the normalized extradata for AVCC, or one
    /// built from the in-band parameter sets for Annex-B.
    pub fn avcc(&self) -> Option<Vec<u8>> {
        match &self.avcc {
            Some(record) => Some(record.clone()),
            None => self
                .parameter_sets
                .as_ref()
                .map(|ps| ps.avcc(ANNEX_B_OUTPUT_LENGTH_SIZE)),
        }
    }

    /// Converts one access unit (a reassembled frame's payload) into a
    /// length-prefixed sample. AVCC passes through after validation;
    /// Annex-B drops its AUD, SPS and PPS NALs (the SPS and PPS go to the
    /// format description) and re-frames the rest.
    pub fn sample(&mut self, access_unit: &[u8]) -> Result<Sample, H264Error> {
        if let BitstreamFormat::Avcc { length_size } = self.format {
            if split_avcc(access_unit, length_size)?.is_empty() {
                return Err(H264Error::EmptyAccessUnit);
            }
            return Ok(Sample {
                data: access_unit.to_vec(),
                format_changed: !std::mem::replace(&mut self.announced, true),
            });
        }

        let mut data = Vec::with_capacity(access_unit.len());
        let mut in_band = ParameterSets {
            sps: Vec::new(),
            pps: Vec::new(),
        };
        for nal in annex_b_nals(access_unit) {
            match nal.first().map(|b| b & 0x1f) {
                Some(NAL_SPS) => in_band.sps.push(nal.to_vec()),
                Some(NAL_PPS) => in_band.pps.push(nal.to_vec()),
                Some(NAL_AUD) | None => {}
                Some(_) => {
                    data.extend_from_slice(&(nal.len() as u32).to_be_bytes());
                    data.extend_from_slice(nal);
                }
            }
        }
        // Nothing but delimiters and parameter sets is nothing to show.
        if data.is_empty() {
            return Err(H264Error::EmptyAccessUnit);
        }
        let mut format_changed = !self.announced;
        if !in_band.sps.is_empty()
            && !in_band.pps.is_empty()
            && self.parameter_sets.as_ref() != Some(&in_band)
        {
            self.parameter_sets = Some(in_band);
            format_changed = true;
        }
        if self.parameter_sets.is_none() {
            return Err(H264Error::NoParameterSets);
        }
        self.announced = true;
        Ok(Sample {
            data,
            format_changed,
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// viewer.test.ts "keeps the avcC description when a keyframe starts
    /// with a start-code-like prefix": Constrained Baseline 4.2, one SPS
    /// `67 42 e0 2a`, one PPS `68 ce`.
    const AVCC_42E02A: [u8; 17] = [
        0x01, 0x42, 0xe0, 0x2a, 0xff, 0xe1, 0x00, 0x04, 0x67, 0x42, 0xe0, 0x2a, 0x01, 0x00, 0x02,
        0x68, 0xce,
    ];

    /// The same test's keyframe: one 300-byte NAL, so its 4-byte length
    /// prefix is `00 00 01 2c`, byte-identical to an Annex-B start code.
    fn start_code_like_avcc_frame() -> Vec<u8> {
        let mut frame = 300u32.to_be_bytes().to_vec();
        frame.extend(std::iter::repeat_n(0x65, 300));
        frame
    }

    #[test]
    fn the_config_not_the_frame_decides_the_format() {
        assert!(is_avcc("avc1.42E02A", &AVCC_42E02A));
        // Annex-B publishers send empty or start-code extradata.
        assert!(!is_avcc("avc1.42E02A", &[]));
        assert!(!is_avcc("avc1.42E02A", &[0, 0, 0, 1, 0x67, 0x42]));
        // Only an avc1 codec string is AVCC; avc3 and VP9 are not.
        assert!(!is_avcc("avc3.42E02A", &AVCC_42E02A));
        assert!(!is_avcc("vp09.00.40.08", &AVCC_42E02A));

        let mut s = H264Stream::new("avc1.42E02A", &AVCC_42E02A).unwrap();
        assert_eq!(s.format(), BitstreamFormat::Avcc { length_size: 4 });
        assert_eq!(
            s.avcc().as_deref(),
            Some(&AVCC_42E02A[..]),
            "description kept"
        );
        let frame = start_code_like_avcc_frame();
        let sample = s.sample(&frame).unwrap();
        assert_eq!(
            sample.data, frame,
            "passed through, not re-split as Annex-B"
        );
        // The first sample is always a format change: Swift needs a format
        // description before it can enqueue anything. The next is not.
        assert!(sample.format_changed);
        assert!(!s.sample(&frame).unwrap().format_changed);
    }

    #[test]
    fn avcc_parameter_sets_come_from_the_record() {
        let s = H264Stream::new("avc1.42E02A", &AVCC_42E02A).unwrap();
        assert_eq!(
            s.parameter_sets(),
            Some(&ParameterSets {
                sps: vec![vec![0x67, 0x42, 0xe0, 0x2a]],
                pps: vec![vec![0x68, 0xce]],
            })
        );
        assert_eq!(s.nal_length_size(), 4);
    }

    #[test]
    fn the_codec_string_follows_the_avcc_when_they_disagree() {
        // Negotiated Baseline 3.1, but the encoder produced High 4.0.
        let mut high = AVCC_42E02A;
        high[1..4].copy_from_slice(&[0x64, 0x00, 0x28]);
        let s = H264Stream::new("avc1.42E01F", &high).unwrap();
        assert_eq!(s.codec(), "avc1.640028");

        // Agreement is case-insensitive and keeps the config's spelling.
        let s = H264Stream::new("avc1.42e02a", &AVCC_42E02A).unwrap();
        assert_eq!(s.codec(), "avc1.42e02a");

        // Annex-B keeps the config's codec: the desktop broadcasters derive
        // it from the SPS already (gawk_encode::h264::parse_codec_string).
        let s = H264Stream::new("avc1.4D4028", &[]).unwrap();
        assert_eq!(s.codec(), "avc1.4D4028");
        assert_eq!(s.format(), BitstreamFormat::AnnexB);
    }

    #[test]
    fn codec_from_avcc_is_uppercase_hex_of_bytes_1_to_3() {
        // fmp4-muxer.test.ts "derives the codec string from the avcC
        // profile bytes".
        assert_eq!(
            codec_from_avcc(&AVCC_42E02A).as_deref(),
            Some("avc1.42E02A")
        );
        assert_eq!(
            codec_from_avcc(&[0x01, 0x64, 0x00, 0x0a]).as_deref(),
            Some("avc1.64000A")
        );
        assert_eq!(codec_from_avcc(&[0x01, 0x64, 0x00]), None);
    }

    #[test]
    fn normalize_forces_the_reserved_bits() {
        let mut lax = AVCC_42E02A;
        lax[4] = 0x03; // lengthSizeMinusOne = 3, reserved bits 0
        lax[5] = 0x01; // one SPS, reserved bits 0
        assert_eq!(normalize_avcc_extradata(&lax), AVCC_42E02A.to_vec());
        // Already compliant: unchanged.
        assert_eq!(normalize_avcc_extradata(&AVCC_42E02A), AVCC_42E02A.to_vec());
    }

    #[test]
    fn normalize_undoes_the_firefox_double_byte_bug() {
        // SPS 67 67 42 e0 2a (header byte doubled; the real profile_idc
        // 0x42, which avcC byte 1 also says, sits at index 2), and PPS
        // 68 68 ce doubled the same way.
        let buggy = [
            0x01, 0x42, 0xe0, 0x2a, 0xff, 0xe1, 0x00, 0x05, 0x67, 0x67, 0x42, 0xe0, 0x2a, 0x01,
            0x00, 0x03, 0x68, 0x68, 0xce,
        ];
        assert_eq!(normalize_avcc_extradata(&buggy), AVCC_42E02A.to_vec());
    }

    #[test]
    fn normalize_leaves_a_doubled_pps_alone_when_the_sps_was_fine() {
        // The PPS fix is gated on the SPS fix: 68 68 is only the bug's
        // signature when the SPS showed it.
        let pps_only = [
            0x01, 0x42, 0xe0, 0x2a, 0xff, 0xe1, 0x00, 0x04, 0x67, 0x42, 0xe0, 0x2a, 0x01, 0x00,
            0x03, 0x68, 0x68, 0xce,
        ];
        assert_eq!(normalize_avcc_extradata(&pps_only), pps_only.to_vec());
    }

    #[test]
    fn normalize_returns_a_non_avcc_record_unchanged() {
        for not_avcc in [
            &[][..],
            &[0x01, 0x42, 0xe0, 0x2a, 0xff, 0xe1][..],
            &[0, 0, 0, 1, 0x67, 0x42, 0xe0, 0x2a][..],
        ] {
            assert_eq!(normalize_avcc_extradata(not_avcc), not_avcc.to_vec());
        }
    }

    #[test]
    fn a_truncated_avcc_length_is_an_error() {
        // fmp4-muxer.test.ts "throws on a truncated AVCC length".
        assert_eq!(
            split_avcc(&[0, 0, 0, 10, 1, 2], 4),
            Err(H264Error::MalformedAvcc)
        );
        // A dangling partial prefix too.
        assert_eq!(
            split_avcc(&[0, 0, 0, 2, 0xaa, 0xbb, 0, 0], 4),
            Err(H264Error::MalformedAvcc)
        );
        assert_eq!(
            split_avcc(&[0, 2, 0xaa, 0xbb, 0, 1, 0xcc], 2).unwrap(),
            vec![&[0xaa, 0xbb][..], &[0xcc][..]]
        );

        let mut s = H264Stream::new("avc1.42E02A", &AVCC_42E02A).unwrap();
        assert_eq!(
            s.sample(&[0, 0, 0, 10, 1, 2]),
            Err(H264Error::MalformedAvcc)
        );
        assert_eq!(s.sample(&[]), Err(H264Error::EmptyAccessUnit));
    }

    #[test]
    fn a_malformed_avcc_record_is_refused() {
        // Declares a 16-byte SPS in a 17-byte record.
        let mut bad = AVCC_42E02A;
        bad[7] = 0x10;
        assert_eq!(
            H264Stream::new("avc1.42E02A", &bad).unwrap_err(),
            H264Error::MalformedAvccRecord
        );
    }

    #[test]
    fn annex_b_moves_parameter_sets_out_and_length_prefixes_the_rest() {
        // AUD, SPS, PPS, IDR slice — the native broadcasters' keyframe —
        // with a 3-byte start code mixed in, as encoders mix them.
        let idr = [
            0, 0, 0, 1, 0x09, 0xf0, // AUD
            0, 0, 0, 1, 0x67, 0x42, 0xe0, 0x2a, // SPS
            0, 0, 1, 0x68, 0xce, // PPS (3-byte start code)
            0, 0, 0, 1, 0x65, 0x88, 0x84, // IDR
        ];
        let mut s = H264Stream::new("avc1.42E02A", &[]).unwrap();
        assert_eq!(s.parameter_sets(), None);
        assert_eq!(s.avcc(), None);
        assert_eq!(s.nal_length_size(), ANNEX_B_OUTPUT_LENGTH_SIZE);

        let sample = s.sample(&idr).unwrap();
        assert_eq!(sample.data, vec![0, 0, 0, 3, 0x65, 0x88, 0x84]);
        assert!(sample.format_changed);
        // The avcC built from them is the browser broadcaster's record.
        assert_eq!(s.avcc().as_deref(), Some(&AVCC_42E02A[..]));

        // A delta: AUD + slice, same parameter sets.
        let delta = [0, 0, 0, 1, 0x09, 0xf0, 0, 0, 0, 1, 0x41, 0x9a, 0x21];
        let sample = s.sample(&delta).unwrap();
        assert_eq!(sample.data, vec![0, 0, 0, 3, 0x41, 0x9a, 0x21]);
        assert!(!sample.format_changed);

        // The next IDR repeats the same SPS/PPS: no new format description.
        assert!(!s.sample(&idr).unwrap().format_changed);

        // A new SPS (a rotation re-encoded at another level) is a change.
        let mut idr2 = idr;
        idr2[13] = 0x28;
        assert!(s.sample(&idr2).unwrap().format_changed);
        assert_eq!(
            s.parameter_sets().unwrap().sps,
            vec![vec![0x67, 0x42, 0xe0, 0x28]]
        );
    }

    #[test]
    fn annex_b_before_any_parameter_sets_has_nothing_to_enqueue_under() {
        let mut s = H264Stream::new("avc1.42E02A", &[]).unwrap();
        let delta = [0, 0, 0, 1, 0x41, 0x9a, 0x21];
        assert_eq!(s.sample(&delta), Err(H264Error::NoParameterSets));
        assert_eq!(s.sample(&[0, 0, 0]), Err(H264Error::EmptyAccessUnit));
    }

    #[test]
    fn start_code_extradata_seeds_the_parameter_sets() {
        // "Annex-B publishers send empty (or start-code) extradata."
        let extradata = [0, 0, 0, 1, 0x67, 0x42, 0xe0, 0x2a, 0, 0, 0, 1, 0x68, 0xce];
        let mut s = H264Stream::new("avc1.42E02A", &extradata).unwrap();
        assert_eq!(s.format(), BitstreamFormat::AnnexB);
        assert_eq!(s.avcc().as_deref(), Some(&AVCC_42E02A[..]));
        let sample = s.sample(&[0, 0, 0, 1, 0x41, 0x9a]).unwrap();
        assert_eq!(sample.data, vec![0, 0, 0, 2, 0x41, 0x9a]);
        assert!(
            sample.format_changed,
            "the first sample announces the format"
        );
        // The keyframe repeating the seeded sets in band changes nothing.
        let idr = [
            0, 0, 0, 1, 0x67, 0x42, 0xe0, 0x2a, 0, 0, 0, 1, 0x68, 0xce, 0, 0, 0, 1, 0x65, 0x88,
        ];
        assert!(!s.sample(&idr).unwrap().format_changed);
    }

    #[test]
    fn built_avcc_carries_the_first_sps_profile_bytes() {
        // fmp4-muxer.test.ts "synthesizes a well-formed avcC from in-band
        // SPS/PPS": profile and level from the SPS, 4-byte lengths, and the
        // SPS parses back out.
        let sps = vec![0x67, 0x64, 0x00, 0x1f, 0xac, 0xd9];
        let pps = vec![0x68, 0xeb, 0xe3, 0xcb];
        let ps = ParameterSets {
            sps: vec![sps.clone()],
            pps: vec![pps.clone()],
        };
        let avcc = ps.avcc(4);
        assert_eq!(avcc[1], sps[1]);
        assert_eq!(avcc[3], sps[3]);
        assert_eq!(avcc[4] & 0x03, 3, "lengthSizeMinusOne");
        let back = H264Stream::new("avc1.64001F", &avcc).unwrap();
        assert_eq!(back.parameter_sets(), Some(&ps));
        assert_eq!(back.format(), BitstreamFormat::Avcc { length_size: 4 });
    }
}
