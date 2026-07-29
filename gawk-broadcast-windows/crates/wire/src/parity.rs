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
//! subscribers at every level below it. This producer only ever COMPUTES
//! parity; reconstruction is the viewer's job, so `RecoverChunks` is
//! deliberately not mirrored here.

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
}
