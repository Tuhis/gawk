//! Forward parity for the datagram delta path (R29, docs/34), mirroring
//! gawk-server/wire/parity.go.
//!
//! A delta frame split into n data chunks gets up to two parity symbols:
//!
//! ```text
//! P = d0 ^ d1 ^ ... ^ d(n-1)
//! Q = (g^0 * d0) ^ (g^1 * d1) ^ ... ^ (g^(n-1) * d(n-1))    g = 2 in GF(2^8)
//! ```
//!
//! RAID-6 P/Q: MDS for k <= 2, and P alone IS the k=1 code — that prefix
//! property is what lets one computation at the fleet's parity level serve
//! subscribers at every level below it.
//!
//! Recovery ([`recover_chunks`], Go's `RecoverChunks`) is mirrored too. Until
//! R65 every consumer of this crate was a broadcaster that only COMPUTES
//! parity, and reconstruction was left to the viewers (Go and the SPA's TS).
//! The iOS app's native player is the first viewer written in Rust, so it
//! needs the repair half here rather than a fifth copy of the math in the
//! app (docs/67 D14). Its tests restate `parity_test.go`'s recovery cases.

use crate::error::WireError;
use crate::{
    MAX_CHUNK_PAYLOAD, MAX_DATAGRAM_SIZE, TYPE_PARITY_CHUNK, TYPE_RELAY_CAPABILITIES, VERSION,
};

/// Fixed header size of a ParityChunk datagram. Deliberately 13 and not 20:
/// a parity symbol is as long as the longest data chunk (up to 1180 bytes),
/// so a 20-byte header carrying a timestamp would breach MaxDatagramSize.
pub const PARITY_CHUNK_HEADER_SIZE: usize = 13;

/// The largest k the P/Q scheme supports.
pub const MAX_PARITY_SYMBOLS: usize = 2;

/// Bounds n: g^i has period 255, so beyond it two data chunks would share a
/// Q coefficient and the 2-erasure solve divides by zero. An explicit guard,
/// not an assumption.
pub const MAX_PARITY_DATA_CHUNKS: usize = 255;

/// Exact size of a RelayCapabilities message.
pub const RELAY_CAPABILITIES_SIZE: usize = 5;

/// The relay understands ParityChunk datagrams and filters them per
/// subscriber. A producer that does not see this bit sends no parity —
/// byte-identical to pre-R29 (docs/38 D4).
pub const CAP_PARITY_CHUNKS: u16 = 1 << 0;

/// The relay accepts striped delivery (R30). Viewer-side; this producer only
/// needs the flags word to keep parsing when new bits appear ("new bits,
/// never new bytes").
pub const CAP_STRIPED_DELIVERY: u16 = 1 << 1;

// --- GF(2^8), primitive polynomial 0x11D, generator 2 ------------------------

/// Exp/log tables built at compile time; the exp cycle is duplicated so
/// exponent sums up to 508 need no modulo (same layout as the Go tables).
const fn build_gf_tables() -> ([u8; 512], [u8; 256]) {
    let mut exp = [0u8; 512];
    let mut log = [0u8; 256];
    let mut x: u8 = 1;
    let mut i = 0;
    while i < 255 {
        exp[i] = x;
        log[x as usize] = i as u8;
        let hi = x & 0x80 != 0;
        x <<= 1;
        if hi {
            x ^= 0x1d;
        }
        i += 1;
    }
    let mut j = 255;
    while j < 512 {
        exp[j] = exp[j - 255];
        j += 1;
    }
    (exp, log)
}

static GF_TABLES: ([u8; 512], [u8; 256]) = build_gf_tables();

fn gf_mul(a: u8, b: u8) -> u8 {
    if a == 0 || b == 0 {
        return 0;
    }
    let (exp, log) = &GF_TABLES;
    exp[log[a as usize] as usize + log[b as usize] as usize]
}

/// a / b in GF(2^8). Panics on b == 0, as Go's `gfDiv` does: every caller
/// divides by a g^i or a sum of two distinct g^i, never zero.
fn gf_div(a: u8, b: u8) -> u8 {
    assert!(b != 0, "wire: division by zero in GF(2^8)");
    if a == 0 {
        return 0;
    }
    let (exp, log) = &GF_TABLES;
    exp[log[a as usize] as usize + 255 - log[b as usize] as usize]
}

/// g^i for g = 2: the Q coefficient of data chunk i.
fn gf_pow2(i: usize) -> u8 {
    GF_TABLES.0[i % 255]
}

// --- Parity computation -------------------------------------------------------

/// Returns `min(k, chunks.len())` parity symbols over the chunk payloads,
/// each as long as the longest chunk (shorter chunks are treated as
/// zero-padded). `k == 0` or no chunks returns an empty vec. Parity is
/// computed over chunk PAYLOADS, not whole datagrams.
pub fn compute_parity(chunks: &[&[u8]], k: usize) -> Result<Vec<Vec<u8>>, WireError> {
    if k > MAX_PARITY_SYMBOLS {
        return Err(WireError::ParityUnsupported);
    }
    let n = chunks.len();
    if k == 0 || n == 0 {
        return Ok(Vec::new());
    }
    if n > MAX_PARITY_DATA_CHUNKS {
        return Err(WireError::ParityUnsupported);
    }
    // n == 1: P duplicates the chunk, and a second symbol would duplicate it
    // again. min(k, n) keeps that from being wire waste.
    let k = k.min(n);

    let width = chunks.iter().map(|c| c.len()).max().unwrap_or(0);
    if width > MAX_CHUNK_PAYLOAD {
        return Err(WireError::ParityUnsupported);
    }

    let mut out = vec![vec![0u8; width]; k];
    for (i, chunk) in chunks.iter().enumerate() {
        for (b, &v) in chunk.iter().enumerate() {
            out[0][b] ^= v;
        }
        if k > 1 {
            let coeff = gf_pow2(i);
            for (b, &v) in chunk.iter().enumerate() {
                out[1][b] ^= gf_mul(coeff, v);
            }
        }
    }
    Ok(out)
}

// --- Recovery ---------------------------------------------------------------

/// Reconstructs missing data chunks in place, mirroring Go's `RecoverChunks`.
/// A `None` entry in `chunks` or `parity` means "not received"; `parity[0]`
/// is P and `parity[1]` is Q. `frame_bytes` is the total encoded frame
/// length — the only thing that says how long the final (short) chunk is.
///
/// The symbol width is taken from a surviving parity symbol, not inferred:
/// every symbol is exactly as long as the longest data chunk, and one is
/// present whenever recovery is possible at all.
///
/// Returns [`WireError::ParityUnrecoverable`] when there are more data
/// erasures than surviving parity symbols — a routine outcome on a lossy
/// link that the caller counts, not a fault — and
/// [`WireError::ParityUnsupported`] for a shape that cannot be valid (no
/// chunks, n > 255, symbols of different widths, or a `frame_bytes` or
/// chunk length inconsistent with the symbol width). On any error `chunks`
/// is left untouched.
pub fn recover_chunks(
    chunks: &mut [Option<Vec<u8>>],
    parity: &[Option<&[u8]>],
    frame_bytes: usize,
) -> Result<(), WireError> {
    let n = chunks.len();
    if n == 0 || n > MAX_PARITY_DATA_CHUNKS {
        return Err(WireError::ParityUnsupported);
    }

    let missing: Vec<usize> = (0..n).filter(|&i| chunks[i].is_none()).collect();
    if missing.is_empty() {
        return Ok(());
    }

    let have_p = parity.first().copied().flatten();
    let have_q = parity.get(1).copied().flatten();
    let avail = usize::from(have_p.is_some()) + usize::from(have_q.is_some());
    if missing.len() > avail {
        return Err(WireError::ParityUnrecoverable);
    }

    let width = have_p.or(have_q).map_or(0, <[u8]>::len);
    if width == 0 {
        return Err(WireError::ParityUnrecoverable);
    }
    if let (Some(p), Some(q)) = (have_p, have_q)
        && p.len() != q.len()
    {
        return Err(WireError::ParityUnsupported);
    }

    // frame_bytes must be consistent with n full-width chunks minus the final
    // chunk's shortfall, or the header is lying and reconstruction would
    // silently produce garbage.
    let last_len = frame_bytes as i64 - ((n - 1) * width) as i64;
    if last_len <= 0 || last_len > width as i64 {
        return Err(WireError::ParityUnsupported);
    }
    let last_len = last_len as usize;
    for (i, c) in chunks.iter().enumerate() {
        if let Some(c) = c {
            let want = if i == n - 1 { last_len } else { width };
            if c.len() != want {
                return Err(WireError::ParityUnsupported);
            }
        }
    }

    // Zero-padded view of a surviving chunk, so the GF arithmetic sees a
    // rectangle. Missing chunks read as zero, which the solves skip anyway.
    let byte_at = |c: &Option<Vec<u8>>, b: usize| -> u8 {
        c.as_ref().and_then(|c| c.get(b).copied()).unwrap_or(0)
    };

    let mut recovered: Vec<(usize, Vec<u8>)> = Vec::with_capacity(2);
    match missing[..] {
        [x] => {
            if let Some(p) = have_p {
                // d_x = P ^ (XOR of survivors)
                let mut acc = p.to_vec();
                for (i, c) in chunks.iter().enumerate() {
                    if i == x {
                        continue;
                    }
                    for (b, a) in acc.iter_mut().enumerate() {
                        *a ^= byte_at(c, b);
                    }
                }
                recovered.push((x, acc));
            } else {
                // Only Q survived: d_x = (Q ^ sum(g^i * d_i, i != x)) / g^x
                let q = have_q.expect("avail >= 1 without P implies Q");
                let mut acc = q.to_vec();
                for (i, c) in chunks.iter().enumerate() {
                    if i == x {
                        continue;
                    }
                    let coeff = gf_pow2(i);
                    for (b, a) in acc.iter_mut().enumerate() {
                        *a ^= gf_mul(coeff, byte_at(c, b));
                    }
                }
                let inv = gf_pow2(x);
                for a in &mut acc {
                    *a = gf_div(*a, inv);
                }
                recovered.push((x, acc));
            }
        }
        [x, y] => {
            // Both P and Q are present (avail == 2 was checked above).
            let (p, q) = (have_p.unwrap(), have_q.unwrap());
            let mut pm = p.to_vec(); // d_x ^ d_y
            let mut qm = q.to_vec(); // g^x*d_x ^ g^y*d_y
            for (i, c) in chunks.iter().enumerate() {
                if i == x || i == y {
                    continue;
                }
                let coeff = gf_pow2(i);
                for b in 0..width {
                    let v = byte_at(c, b);
                    pm[b] ^= v;
                    qm[b] ^= gf_mul(coeff, v);
                }
            }
            let (gx, gy) = (gf_pow2(x), gf_pow2(y));
            let den = gx ^ gy;
            if den == 0 {
                // Unreachable while n <= MAX_PARITY_DATA_CHUNKS, which is
                // exactly what that bound is for.
                return Err(WireError::ParityUnsupported);
            }
            let dx: Vec<u8> = (0..width)
                .map(|b| gf_div(gf_mul(gy, pm[b]) ^ qm[b], den))
                .collect();
            let dy: Vec<u8> = (0..width).map(|b| pm[b] ^ dx[b]).collect();
            recovered.push((x, dx));
            recovered.push((y, dy));
        }
        // More than two erasures never passes the avail check (avail <= 2).
        _ => return Err(WireError::ParityUnrecoverable),
    }

    for (i, mut out) in recovered {
        if i == n - 1 {
            out.truncate(last_len);
        }
        chunks[i] = Some(out);
    }
    Ok(())
}

// --- ParityChunk wire format ---------------------------------------------------

/// The header of a ParityChunk datagram.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct ParityChunkHeader {
    pub frame_id: u32,
    /// 0 = P, 1 = Q.
    pub parity_index: u8,
    /// n, the frame's DATA chunk count.
    pub chunk_count: u16,
    /// Total encoded frame length — the field that says how long the final
    /// (short) chunk is, since the header carries no timestamp.
    pub frame_bytes: u32,
}

/// Appends a ParityChunk datagram.
pub fn append_parity_chunk(
    dst: &mut Vec<u8>,
    h: &ParityChunkHeader,
    payload: &[u8],
) -> Result<(), WireError> {
    if payload.len() > MAX_CHUNK_PAYLOAD {
        return Err(WireError::PayloadTooLarge {
            len: payload.len(),
            max: MAX_CHUNK_PAYLOAD,
        });
    }
    if h.chunk_count == 0 || h.chunk_count as usize > MAX_PARITY_DATA_CHUNKS {
        return Err(WireError::BadChunkCount {
            index: h.parity_index.into(),
            count: h.chunk_count.into(),
        });
    }
    if h.parity_index as usize >= MAX_PARITY_SYMBOLS {
        return Err(WireError::BadChunkCount {
            index: h.parity_index.into(),
            count: h.chunk_count.into(),
        });
    }
    dst.extend_from_slice(&[VERSION, TYPE_PARITY_CHUNK]);
    dst.extend_from_slice(&h.frame_id.to_be_bytes());
    dst.push(h.parity_index);
    dst.extend_from_slice(&h.chunk_count.to_be_bytes());
    dst.extend_from_slice(&h.frame_bytes.to_be_bytes());
    dst.extend_from_slice(payload);
    Ok(())
}

/// Parses a ParityChunk datagram; the payload borrows `dgram`.
pub fn parse_parity_chunk(dgram: &[u8]) -> Result<(ParityChunkHeader, &[u8]), WireError> {
    if dgram.len() < PARITY_CHUNK_HEADER_SIZE {
        return Err(WireError::ShortDatagram {
            len: dgram.len(),
            need: PARITY_CHUNK_HEADER_SIZE,
        });
    }
    if dgram.len() > MAX_DATAGRAM_SIZE {
        return Err(WireError::PayloadTooLarge {
            len: dgram.len(),
            max: MAX_DATAGRAM_SIZE,
        });
    }
    if dgram[0] != VERSION {
        return Err(WireError::BadVersion(dgram[0]));
    }
    if dgram[1] != TYPE_PARITY_CHUNK {
        return Err(WireError::BadType {
            got: dgram[1],
            want: TYPE_PARITY_CHUNK,
        });
    }
    let h = ParityChunkHeader {
        frame_id: u32::from_be_bytes(dgram[2..6].try_into().unwrap()),
        parity_index: dgram[6],
        chunk_count: u16::from_be_bytes(dgram[7..9].try_into().unwrap()),
        frame_bytes: u32::from_be_bytes(dgram[9..13].try_into().unwrap()),
    };
    if h.chunk_count == 0 || h.chunk_count as usize > MAX_PARITY_DATA_CHUNKS {
        return Err(WireError::BadChunkCount {
            index: h.parity_index.into(),
            count: h.chunk_count.into(),
        });
    }
    if h.parity_index as usize >= MAX_PARITY_SYMBOLS {
        return Err(WireError::BadChunkCount {
            index: h.parity_index.into(),
            count: h.chunk_count.into(),
        });
    }
    Ok((h, &dgram[PARITY_CHUNK_HEADER_SIZE..]))
}

// --- RelayCapabilities wire format ---------------------------------------------

/// What the relay tells a client about optional features, once per session.
/// Capability GROWTH is new bits in the flags word, never new bytes — this
/// parser is strict by size and must survive future flags (pinned by test).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct RelayCapabilities {
    pub flags: u16,
    /// The fleet parity level producers should emit (0..=2).
    pub parity_level: u8,
}

/// Appends a RelayCapabilities message.
pub fn append_relay_capabilities(
    dst: &mut Vec<u8>,
    c: &RelayCapabilities,
) -> Result<(), WireError> {
    if c.parity_level as usize > MAX_PARITY_SYMBOLS {
        return Err(WireError::ParityUnsupported);
    }
    dst.extend_from_slice(&[VERSION, TYPE_RELAY_CAPABILITIES]);
    dst.extend_from_slice(&c.flags.to_be_bytes());
    dst.push(c.parity_level);
    Ok(())
}

/// Parses a RelayCapabilities message. Strict: exactly 5 bytes.
pub fn parse_relay_capabilities(msg: &[u8]) -> Result<RelayCapabilities, WireError> {
    if msg.len() != RELAY_CAPABILITIES_SIZE {
        return Err(WireError::ShortDatagram {
            len: msg.len(),
            need: RELAY_CAPABILITIES_SIZE,
        });
    }
    if msg[0] != VERSION {
        return Err(WireError::BadVersion(msg[0]));
    }
    if msg[1] != TYPE_RELAY_CAPABILITIES {
        return Err(WireError::BadType {
            got: msg[1],
            want: TYPE_RELAY_CAPABILITIES,
        });
    }
    let c = RelayCapabilities {
        flags: u16::from_be_bytes(msg[2..4].try_into().unwrap()),
        parity_level: msg[4],
    };
    if c.parity_level as usize > MAX_PARITY_SYMBOLS {
        return Err(WireError::ParityUnsupported);
    }
    Ok(c)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn gf_tables_match_the_reference_generator() {
        // g^0..g^7 for g=2 over 0x11D.
        let expect = [1u8, 2, 4, 8, 16, 32, 64, 128];
        for (i, want) in expect.iter().enumerate() {
            assert_eq!(gf_pow2(i), *want, "g^{i}");
        }
        // First reduction: g^8 = 0x1d.
        assert_eq!(gf_pow2(8), 0x1d);
        // Period 255: g^255 == g^0.
        assert_eq!(gf_pow2(255), 1);
        // Multiplication sanity: a*1 == a, a*0 == 0.
        for a in 0..=255u8 {
            assert_eq!(gf_mul(a, 1), a);
            assert_eq!(gf_mul(a, 0), 0);
        }
    }

    #[test]
    fn parity_over_erasure_repairs_by_xor() {
        // Self-check of the P math without a decoder: P ^ (all survivors)
        // reproduces the erased chunk, zero-padded to symbol width.
        let chunks: Vec<&[u8]> = vec![b"aaaa", b"bb", b"cccc"];
        let symbols = compute_parity(&chunks, 1).unwrap();
        let p = &symbols[0];
        let mut recovered = p.clone();
        for (b, v) in chunks[0].iter().enumerate() {
            recovered[b] ^= v;
        }
        for (b, v) in chunks[2].iter().enumerate() {
            recovered[b] ^= v;
        }
        assert_eq!(&recovered[..2], b"bb");
        assert_eq!(&recovered[2..], &[0, 0]);
    }

    #[test]
    fn parity_shape_rules_mirror_go() {
        // k = 0 or no chunks: no symbols.
        assert!(compute_parity(&[], 2).unwrap().is_empty());
        assert!(compute_parity(&[b"x".as_slice()], 0).unwrap().is_empty());
        // k > 2 is unsupported.
        assert_eq!(
            compute_parity(&[b"x".as_slice()], 3),
            Err(WireError::ParityUnsupported)
        );
        // n > 255 is unsupported (the MDS bound).
        let chunk = [0u8; 4];
        let many: Vec<&[u8]> = (0..256).map(|_| chunk.as_slice()).collect();
        assert_eq!(compute_parity(&many, 1), Err(WireError::ParityUnsupported));
        // A chunk wider than MaxChunkPayload is unsupported.
        let wide = vec![0u8; MAX_CHUNK_PAYLOAD + 1];
        assert_eq!(
            compute_parity(&[wide.as_slice()], 1),
            Err(WireError::ParityUnsupported)
        );
    }

    #[test]
    fn parity_chunk_parse_rejects_malformed() {
        let mut good = Vec::new();
        append_parity_chunk(
            &mut good,
            &ParityChunkHeader {
                frame_id: 1,
                parity_index: 0,
                chunk_count: 4,
                frame_bytes: 16,
            },
            &[1, 2, 3, 4],
        )
        .unwrap();

        assert!(parse_parity_chunk(&good[..PARITY_CHUNK_HEADER_SIZE - 1]).is_err());

        let mut bad = good.clone();
        bad[0] = 0x02;
        assert!(parse_parity_chunk(&bad).is_err());

        let mut bad = good.clone();
        bad[1] = 0x01;
        assert!(parse_parity_chunk(&bad).is_err());

        // Zero count.
        let mut bad = good.clone();
        bad[7] = 0;
        bad[8] = 0;
        assert!(parse_parity_chunk(&bad).is_err());

        // Count > 255.
        let mut bad = good.clone();
        bad[7] = 0xff;
        bad[8] = 0xff;
        assert!(parse_parity_chunk(&bad).is_err());

        // Parity index out of range.
        let mut bad = good.clone();
        bad[6] = MAX_PARITY_SYMBOLS as u8;
        assert!(parse_parity_chunk(&bad).is_err());

        // Whole datagram over MaxDatagramSize.
        let mut bad = good.clone();
        bad.extend_from_slice(&vec![0u8; MAX_DATAGRAM_SIZE]);
        assert!(parse_parity_chunk(&bad).is_err());

        // Oversize payload refused on append too.
        assert!(matches!(
            append_parity_chunk(
                &mut Vec::new(),
                &ParityChunkHeader {
                    frame_id: 1,
                    parity_index: 0,
                    chunk_count: 2,
                    frame_bytes: 4
                },
                &vec![0u8; MAX_CHUNK_PAYLOAD + 1],
            ),
            Err(WireError::PayloadTooLarge { .. })
        ));
    }

    #[test]
    fn relay_capabilities_rejects_bad_level_and_length() {
        assert_eq!(
            append_relay_capabilities(
                &mut Vec::new(),
                &RelayCapabilities {
                    flags: 0,
                    parity_level: 3
                }
            ),
            Err(WireError::ParityUnsupported)
        );
        let mut good = Vec::new();
        append_relay_capabilities(
            &mut good,
            &RelayCapabilities {
                flags: CAP_PARITY_CHUNKS,
                parity_level: 1,
            },
        )
        .unwrap();
        assert!(parse_relay_capabilities(&good[..RELAY_CAPABILITIES_SIZE - 1]).is_err());
        let mut long = good.clone();
        long.push(0);
        assert!(parse_relay_capabilities(&long).is_err());
    }

    // --- Recovery, restated from gawk-server/wire/parity_test.go ---------------

    #[test]
    fn gf_mul_identities_and_inverse() {
        // TestGFMulIdentitiesAndInverse.
        for a in 0..=255u8 {
            assert_eq!(gf_mul(a, 0), 0, "gf_mul({a}, 0)");
            assert_eq!(gf_mul(a, 1), a, "gf_mul({a}, 1)");
        }
        // Every non-zero element has a multiplicative inverse.
        for a in 1..=255u8 {
            let inv = gf_div(1, a);
            assert_eq!(gf_mul(a, inv), 1, "a={a} * inv={inv}");
        }
    }

    #[test]
    fn gf_pow_is_distinct_below_255() {
        // TestGFPowIsDistinctBelow255: the Q coefficients must be distinct
        // over the supported range or the 2-erasure solve divides by zero.
        let mut seen = [None::<usize>; 256];
        for i in 0..MAX_PARITY_DATA_CHUNKS {
            let c = gf_pow2(i) as usize;
            assert!(seen[c].is_none(), "g^{i} == g^{:?}", seen[c]);
            seen[c] = Some(i);
        }
        assert_eq!(gf_pow2(0), gf_pow2(255), "period 255");
    }

    /// Deterministic xorshift64 — Go's math/rand stream is not reproducible
    /// here, and the exhaustive test asserts a property, not fixed bytes.
    struct Rng(u64);
    impl Rng {
        fn fill(&mut self, buf: &mut [u8]) {
            for b in buf {
                self.0 ^= self.0 << 13;
                self.0 ^= self.0 >> 7;
                self.0 ^= self.0 << 17;
                *b = self.0 as u8;
            }
        }
    }

    /// makeFrame: n chunks of chunk_len bytes with a short final chunk, and
    /// the total frame length they encode.
    fn make_frame(rng: &mut Rng, n: usize, chunk_len: usize) -> (Vec<Vec<u8>>, usize) {
        let chunks: Vec<Vec<u8>> = (0..n)
            .map(|i| {
                let len = if i == n - 1 {
                    chunk_len / 2 + 1
                } else {
                    chunk_len
                };
                let mut c = vec![0u8; len];
                rng.fill(&mut c);
                c
            })
            .collect();
        let frame_bytes = (n - 1) * chunk_len + chunks[n - 1].len();
        (chunks, frame_bytes)
    }

    #[test]
    fn recover_all_erasure_pairs() {
        // TestRecoverAllErasurePairs: for every supported n and EVERY pair of
        // erasure positions among the n+k transmitted chunks (a == b models a
        // single erasure), recovery reproduces the original bytes exactly, or
        // fails when data erasures exceed SURVIVING parity.
        let mut rng = Rng(1);
        for n in [1usize, 2, 3, 9, 17, 64, 254, 255] {
            let (orig, frame_bytes) = make_frame(&mut rng, n, 32);
            let k = if n < 2 { 1 } else { 2 };
            let refs: Vec<&[u8]> = orig.iter().map(Vec::as_slice).collect();
            let parity = compute_parity(&refs, k).unwrap();
            let total = n + parity.len();
            for a in 0..total {
                for b in a..total {
                    let mut chunks: Vec<Option<Vec<u8>>> = orig.iter().cloned().map(Some).collect();
                    let mut par: Vec<Option<&[u8]>> =
                        parity.iter().map(|p| Some(p.as_slice())).collect();
                    for pos in [a, b] {
                        if pos < n {
                            chunks[pos] = None;
                        } else {
                            par[pos - n] = None;
                        }
                    }
                    let lost = chunks.iter().filter(|c| c.is_none()).count();
                    let surviving = par.iter().filter(|p| p.is_some()).count();
                    let res = recover_chunks(&mut chunks, &par, frame_bytes);
                    if lost > surviving {
                        assert!(
                            res.is_err(),
                            "n={n} erasures ({a},{b}): recovered {lost} losses with {surviving} symbols"
                        );
                        continue;
                    }
                    res.unwrap_or_else(|e| panic!("n={n} erasures ({a},{b}): {e}"));
                    for (i, want) in orig.iter().enumerate() {
                        assert_eq!(
                            chunks[i].as_deref(),
                            Some(want.as_slice()),
                            "n={n} erasures ({a},{b}): chunk {i}"
                        );
                    }
                }
            }
        }
    }

    #[test]
    fn recover_preserves_short_final_chunk() {
        // TestRecoverPreservesShortFinalChunk: frame_bytes is the only thing
        // that says how short the last chunk is.
        let orig: [&[u8]; 3] = [&[1, 2, 3, 4], &[5, 6, 7, 8], &[9]];
        let frame_bytes = 9;
        let parity = compute_parity(&orig, 2).unwrap();
        let par: Vec<Option<&[u8]>> = parity.iter().map(|p| Some(p.as_slice())).collect();
        let mut chunks = vec![Some(orig[0].to_vec()), Some(orig[1].to_vec()), None];
        recover_chunks(&mut chunks, &par, frame_bytes).unwrap();
        assert_eq!(
            chunks[2].as_deref(),
            Some([9u8].as_slice()),
            "padding must be trimmed"
        );
    }

    #[test]
    fn recover_fails_cleanly_without_parity() {
        // TestRecoverFailsCleanlyWithoutParity.
        let orig: [&[u8]; 2] = [&[1, 2], &[3, 4]];
        let mut chunks = vec![None, Some(orig[1].to_vec())];
        assert_eq!(
            recover_chunks(&mut chunks, &[], 4),
            Err(WireError::ParityUnrecoverable)
        );
        // A frame with nothing missing needs no parity and must not error.
        let mut full = vec![Some(orig[0].to_vec()), Some(orig[1].to_vec())];
        assert_eq!(recover_chunks(&mut full, &[], 4), Ok(()));
    }

    #[test]
    fn recover_rejects_inconsistent_frame_bytes() {
        // TestRecoverRejectsInconsistentFrameBytes.
        let orig: [&[u8]; 2] = [&[1, 2, 3, 4], &[5, 6]];
        let parity = compute_parity(&orig, 2).unwrap();
        let par: Vec<Option<&[u8]>> = parity.iter().map(|p| Some(p.as_slice())).collect();
        let mut chunks = vec![None, Some(orig[1].to_vec())];
        // frame_bytes far larger than n * chunkLen is structurally impossible.
        assert_eq!(
            recover_chunks(&mut chunks, &par, 9999),
            Err(WireError::ParityUnsupported)
        );
    }
}
