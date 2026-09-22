//! Annex-B H.264 bitstream inspection, ported from
//! gawk-broadcast/internal/engine/sps.go. Pure and portable: this is the
//! half of the encode crate that runs (and tests) on any host.
//!
//! The codec string is parsed from the bitstream, never assumed (docs/19
//! Decision 8, inherited by docs/38 D10): the encoder may pick a different
//! level than we asked for — level is a function of resolution, framerate
//! and bitrate, and hardware encoders round it up — and on the Annex-B path
//! this string is the ONLY thing telling the viewer's decoder what it is
//! about to get. The viewer's extradata-derived correction runs only for
//! AVCC, which this path deliberately never produces.

const NAL_TYPE_NON_IDR: u8 = 1;
const NAL_TYPE_IDR: u8 = 5;
const NAL_TYPE_SPS: u8 = 7;
const NAL_TYPE_PPS: u8 = 8;

/// Derives the WebCodecs codec string ("avc1.42E02A") from the first SPS in
/// an Annex-B access unit.
///
/// The three bytes are profile_idc, constraint_flags+reserved, level_idc,
/// and they sit immediately after the 1-byte NAL header — before any
/// position where an emulation-prevention byte can legally occur (an EPB
/// requires two preceding zero bytes, and the earliest it can appear is the
/// fourth byte of the RBSP). So reading them raw is safe here, and ONLY
/// here; anything deeper into the SPS needs real EPB removal.
pub fn parse_codec_string(au: &[u8]) -> Option<String> {
    annex_b_nals(au).find_map(|nal| {
        if nal.len() < 4 || nal[0] & 0x1f != NAL_TYPE_SPS {
            return None;
        }
        Some(format!("avc1.{:02X}{:02X}{:02X}", nal[1], nal[2], nal[3]))
    })
}

/// Whether an access unit contains an IDR slice — the engine's definition of
/// a keyframe, and what routes it onto a reliable uni stream instead of
/// datagrams.
pub fn has_idr(au: &[u8]) -> bool {
    annex_b_nals(au).any(|nal| !nal.is_empty() && nal[0] & 0x1f == NAL_TYPE_IDR)
}

/// Whether the buffer carries both an SPS and a PPS NAL — the
/// "self-describing IDR" test behind the header-prepend path (docs/38 D9's
/// SPS/PPS-before-every-IDR invariant; vendor MFTs differ, V-6).
pub fn has_sps_pps(buf: &[u8]) -> bool {
    let mut sps = false;
    let mut pps = false;
    for nal in annex_b_nals(buf) {
        match nal.first().map(|b| b & 0x1f) {
            Some(NAL_TYPE_SPS) => sps = true,
            Some(NAL_TYPE_PPS) => pps = true,
            _ => {}
        }
    }
    sps && pps
}

/// Whether an access unit contains any B slice.
///
/// This exists to be ASSERTED, not consulted: no-B-frames is a hard encoder
/// invariant (docs/38 D9), because the entire viewer pipeline assumes decode
/// order == presentation order. A B-frame would not fail loudly; it would
/// produce subtly wrong playback. The trial-encode gate pins b-frames = 0
/// per candidate, and this is what proves the assertion can detect a
/// violation.
pub fn has_b_slices(au: &[u8]) -> bool {
    annex_b_nals(au).any(|nal| {
        if nal.len() < 2 {
            return false;
        }
        matches!(nal[0] & 0x1f, NAL_TYPE_NON_IDR | NAL_TYPE_IDR)
            // slice_type values wrap at 5 (0..4 and 5..9 mean the same
            // types): 0=P 1=B 2=I 3=SP 4=SI.
            && matches!(slice_type(nal), Some(t) if t % 5 == 1)
    })
}

/// Reads slice_type from a slice NAL's header: first_mb_in_slice (ue), then
/// slice_type (ue).
fn slice_type(nal: &[u8]) -> Option<u32> {
    let rbsp = unescape_rbsp(&nal[1..]);
    let mut r = BitReader { buf: &rbsp, pos: 0 };
    r.ue()?; // first_mb_in_slice
    r.ue()
}

/// Removes emulation-prevention bytes: the encoder inserts a 0x03 after any
/// 00 00 that would otherwise look like a start code, and a bit reader must
/// not see it.
fn unescape_rbsp(b: &[u8]) -> Vec<u8> {
    let mut out = Vec::with_capacity(b.len());
    let mut zeros = 0;
    for &c in b {
        if zeros >= 2 && c == 0x03 {
            zeros = 0;
            continue; // the emulation-prevention byte itself
        }
        if c == 0 {
            zeros += 1;
        } else {
            zeros = 0;
        }
        out.push(c);
    }
    out
}

/// Reads unsigned exp-Golomb values, MSB first.
struct BitReader<'a> {
    buf: &'a [u8],
    pos: usize, // bit position
}

impl BitReader<'_> {
    fn bit(&mut self) -> Option<u32> {
        if self.pos >= self.buf.len() * 8 {
            return None;
        }
        let b = (self.buf[self.pos / 8] >> (7 - (self.pos % 8))) & 1;
        self.pos += 1;
        Some(u32::from(b))
    }

    /// One ue(v): count leading zeros, then read that many bits.
    fn ue(&mut self) -> Option<u32> {
        let mut zeros = 0u32;
        loop {
            if self.bit()? == 1 {
                break;
            }
            zeros += 1;
            if zeros > 31 {
                return None; // malformed; refuse rather than loop
            }
        }
        let mut v = 0u32;
        for _ in 0..zeros {
            v = (v << 1) | self.bit()?;
        }
        Some(v + (1 << zeros) - 1)
    }
}

/// Rewrites a length-prefixed (AVCC) access unit — what VideoToolbox
/// emits (docs/54 D8) — as Annex-B with 4-byte start codes. `len_size` is
/// the NAL length field's size from the format description (1, 2 or 4).
///
/// `None` for anything malformed — a length running past the end, a
/// zero-length NAL, a dangling partial length, an unsupported size — never
/// a truncated AU: a half-rewritten keyframe would poison every viewer's
/// decoder until the next one.
pub fn avcc_to_annex_b(avcc: &[u8], len_size: usize) -> Option<Vec<u8>> {
    if !matches!(len_size, 1 | 2 | 4) {
        return None;
    }
    let mut out = Vec::with_capacity(avcc.len() + avcc.len() / 64 + 16);
    let mut i = 0;
    while i < avcc.len() {
        let len_bytes = avcc.get(i..i + len_size)?;
        let n = len_bytes
            .iter()
            .fold(0usize, |acc, &b| (acc << 8) | usize::from(b));
        i += len_size;
        if n == 0 {
            return None;
        }
        let nal = avcc.get(i..i.checked_add(n)?)?;
        out.extend_from_slice(&[0, 0, 0, 1]);
        out.extend_from_slice(nal);
        i += n;
    }
    Some(out)
}

/// Raw NAL units (the SPS and PPS out of a format description) as an
/// Annex-B prefix — the headers prepended to every IDR (docs/54 D8).
pub fn annex_b_from_nals<'a>(nals: impl IntoIterator<Item = &'a [u8]>) -> Vec<u8> {
    let mut out = Vec::new();
    for nal in nals {
        out.extend_from_slice(&[0, 0, 0, 1]);
        out.extend_from_slice(nal);
    }
    out
}

/// Iterates the NAL units in an Annex-B buffer, yielding each NAL's bytes
/// without its start code. Accepts both 3- and 4-byte start codes, which
/// encoders mix freely within one AU. Trailing zero padding is trimmed — it
/// is not part of the NAL.
pub fn annex_b_nals(buf: &[u8]) -> impl Iterator<Item = &[u8]> {
    let mut nals = Vec::new();
    let mut start: Option<usize> = None;
    let mut i = 0;
    while i + 2 < buf.len() {
        if buf[i] != 0 || buf[i + 1] != 0 {
            i += 1;
            continue;
        }
        let sc_len = if buf[i + 2] == 1 {
            3
        } else if i + 3 < buf.len() && buf[i + 2] == 0 && buf[i + 3] == 1 {
            4
        } else {
            i += 1;
            continue;
        };
        if let Some(s) = start {
            nals.push(trim_trailing_zeros(&buf[s..i]));
        }
        i += sc_len;
        start = Some(i);
    }
    if let Some(s) = start
        && s < buf.len()
    {
        nals.push(trim_trailing_zeros(&buf[s..]));
    }
    nals.into_iter()
}

fn trim_trailing_zeros(mut nal: &[u8]) -> &[u8] {
    while let [rest @ .., 0] = nal {
        nal = rest;
    }
    nal
}

#[cfg(test)]
mod tests {
    use super::*;

    // The synthetic vectors from the Go engine's sps_test.go — the two
    // implementations must derive identical strings from identical bytes.

    #[test]
    fn codec_string_from_synthetic_sps() {
        // profile_idc 0x42, constraint flags 0xE0, level_idc 0x2A — the
        // codec string the browser broadcaster negotiates first.
        let au = [0, 0, 0, 1, 0x67, 0x42, 0xE0, 0x2A, 0x99];
        assert_eq!(parse_codec_string(&au).as_deref(), Some("avc1.42E02A"));
    }

    #[test]
    fn codec_string_handles_three_byte_start_codes() {
        let au = [0, 0, 1, 0x67, 0x64, 0x00, 0x28, 0xAC];
        assert_eq!(parse_codec_string(&au).as_deref(), Some("avc1.640028"));
    }

    #[test]
    fn no_sps_means_no_codec_string() {
        // A single P-slice NAL: no SPS anywhere.
        let au = [0, 0, 0, 1, 0x41, 0xC0];
        assert_eq!(parse_codec_string(&au), None);
    }

    #[test]
    fn idr_detection() {
        let idr = [0, 0, 0, 1, 0x65, 0x88, 0x80];
        let non_idr = [0, 0, 0, 1, 0x41, 0xC0];
        assert!(has_idr(&idr));
        assert!(!has_idr(&non_idr));
        // Mixed AU: SPS + PPS + IDR (the shape every keyframe must have on
        // the empty-extradata path).
        let mixed = [
            0, 0, 0, 1, 0x67, 0x42, 0xE0, 0x2A, 0x99, // SPS
            0, 0, 0, 1, 0x68, 0xCE, 0x38, 0x80, // PPS
            0, 0, 1, 0x65, 0x88, 0x80, // IDR, 3-byte start code
        ];
        assert!(has_idr(&mixed));
        assert_eq!(parse_codec_string(&mixed).as_deref(), Some("avc1.42E02A"));
    }

    #[test]
    fn b_slice_detection_via_exp_golomb() {
        // Slice header bits: first_mb_in_slice=0 is ue "1"; slice_type
        // follows. slice_type=1 (B) is ue "010" → bits 1 010 padded →
        // 0xA0. slice_type=0 (P) is ue "1" → bits 1 1 padded → 0xC0.
        let b_slice = [0, 0, 0, 1, 0x41, 0xA0];
        let p_slice = [0, 0, 0, 1, 0x41, 0xC0];
        assert!(has_b_slices(&b_slice));
        assert!(!has_b_slices(&p_slice));
        // slice_type 6 (B, wrapped range 5..9): ue(6) = "00111" → bits
        // 1 00111 padded → 0x9C.
        let b_slice_wrapped = [0, 0, 0, 1, 0x41, 0x9C];
        assert!(has_b_slices(&b_slice_wrapped));
    }

    #[test]
    fn emulation_prevention_bytes_are_removed() {
        assert_eq!(
            unescape_rbsp(&[0x00, 0x00, 0x03, 0x01]),
            vec![0x00, 0x00, 0x01]
        );
        // A 0x03 without two preceding zeros is data, not an EPB.
        assert_eq!(unescape_rbsp(&[0x00, 0x03, 0x00]), vec![0x00, 0x03, 0x00]);
        // Two EPB sequences back to back.
        assert_eq!(
            unescape_rbsp(&[0x00, 0x00, 0x03, 0x00, 0x00, 0x03, 0x02]),
            vec![0x00, 0x00, 0x00, 0x00, 0x02]
        );
    }

    #[test]
    fn trailing_zero_padding_is_trimmed() {
        let au = [0u8, 0, 0, 1, 0x67, 0x42, 0xE0, 0x2A, 0x99, 0x00, 0x00];
        let nals: Vec<_> = annex_b_nals(&au).collect();
        assert_eq!(nals.len(), 1);
        assert_eq!(nals[0], &[0x67, 0x42, 0xE0, 0x2A, 0x99]);
    }

    // docs/54 D8: VideoToolbox emits AVCC (length-prefixed NALs) with the
    // parameter sets only in the format description; the wire wants
    // Annex-B with SPS/PPS in-band. These pin the rewrite, on the same
    // vectors the Windows (Media Foundation) path is tested with.

    /// The inverse, for building fixtures: Annex-B → AVCC with `len`-byte
    /// big-endian lengths.
    fn to_avcc(annex_b: &[u8], len: usize) -> Vec<u8> {
        let mut out = Vec::new();
        for nal in annex_b_nals(annex_b) {
            let n = nal.len() as u32;
            out.extend_from_slice(&n.to_be_bytes()[4 - len..]);
            out.extend_from_slice(nal);
        }
        out
    }

    #[test]
    fn avcc_rewrites_to_annex_b_nal_for_nal() {
        // SPS (the Go vectors' 0x42E02A), PPS, IDR — mixed 3/4-byte start
        // codes, as Media Foundation vendors emit them.
        let annex_b = [
            0, 0, 0, 1, 0x67, 0x42, 0xE0, 0x2A, 0x99, // SPS
            0, 0, 1, 0x68, 0xCE, 0x3C, 0x80, // PPS
            0, 0, 0, 1, 0x65, 0x88, 0x84, 0x00, 0x33, // IDR
        ];
        for len in [1usize, 2, 4] {
            let avcc = to_avcc(&annex_b, len);
            let back = avcc_to_annex_b(&avcc, len).expect("well-formed");
            assert_eq!(
                annex_b_nals(&back).collect::<Vec<_>>(),
                annex_b_nals(&annex_b).collect::<Vec<_>>(),
                "len {len}"
            );
            // Always 4-byte start codes out, and the SPS still parses.
            assert_eq!(&back[..4], &[0, 0, 0, 1]);
            assert_eq!(parse_codec_string(&back).as_deref(), Some("avc1.42E02A"));
            assert!(has_idr(&back) && has_sps_pps(&back));
        }
    }

    #[test]
    fn malformed_avcc_is_refused_not_truncated() {
        // A length running past the end.
        assert_eq!(avcc_to_annex_b(&[0, 0, 0, 9, 0x65, 0x88], 4), None);
        // A zero-length NAL.
        assert_eq!(avcc_to_annex_b(&[0, 0, 0, 0], 4), None);
        // A dangling partial length field.
        assert_eq!(avcc_to_annex_b(&[0, 0, 0, 2, 0x41, 0xC0, 0, 0], 4), None);
        // An unsupported length size.
        assert_eq!(avcc_to_annex_b(&[1, 0x41], 3), None);
        assert_eq!(avcc_to_annex_b(&[], 4), Some(Vec::new()));
    }

    #[test]
    fn parameter_sets_become_an_annex_b_prefix() {
        let sps: &[u8] = &[0x67, 0x64, 0x00, 0x28, 0xAC];
        let pps: &[u8] = &[0x68, 0xEE, 0x3C, 0x80];
        let prefix = annex_b_from_nals([sps, pps]);
        assert_eq!(
            prefix,
            [&[0u8, 0, 0, 1][..], sps, &[0, 0, 0, 1], pps].concat()
        );
        assert!(has_sps_pps(&prefix));
        assert_eq!(parse_codec_string(&prefix).as_deref(), Some("avc1.640028"));
    }
}
