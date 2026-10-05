//! VP8 and VP9 decode (docs/67 OD12, D15): every broadcast plays in the
//! app, including the Firefox broadcasters' VP8/VP9, through a libvpx
//! bundled into the core. Decoded frames come out as I420 planes that Swift
//! copies into `CVPixelBuffer`s from an IOSurface pool and enqueues decoded.
//!
//! libvpx comes from `shiguredo_libvpx`'s `source-build`: compiled from the
//! upstream webmproject tag the crate pins, for the host and both iOS
//! targets, never downloaded prebuilt (OD12).
//!
//! **The decoded frame's dimensions are the truth** (CLAUDE.md, docs/01):
//! nothing here reads a size from the codec string or the `DecoderConfig`.
//! A resolution change (D9's rotations) arrives with a new `DecoderConfig`,
//! and the viewer answers it by building a fresh decoder (D13); libvpx would
//! also follow a size change on a keyframe without one, and the frames
//! report whatever it decoded.

use std::fmt;

use shiguredo_libvpx::{DecoderCodec, DecoderConfig};

/// The two codecs this module decodes, read from a `DecoderConfig`'s codec
/// string.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum VpxCodec {
    Vp8,
    Vp9,
}

impl VpxCodec {
    /// Maps a WebCodecs codec string, as broadcasters send it in
    /// `DecoderConfig`: `vp8`, or `vp09.PP.LL.DD…` (`gawk-app`'s
    /// `DEFAULT_CODEC_PREFERENCES`). `None` for anything else, H.264
    /// included, which never reaches libvpx (D15).
    pub fn from_codec_string(codec: &str) -> Option<Self> {
        if codec == "vp8" {
            Some(Self::Vp8)
        } else if codec.starts_with("vp09.") {
            Some(Self::Vp9)
        } else {
            None
        }
    }
}

/// libvpx refused to build a decoder or to decode a frame, or produced a
/// format the viewer doesn't present.
#[derive(Debug)]
pub enum VpxError {
    Libvpx(shiguredo_libvpx::Error),
    /// A high-bit-depth frame (VP9 profile 2/3). No gawk broadcaster
    /// encodes one (every `vp09` preference is 8-bit), and Swift's pool is
    /// 8-bit 4:2:0.
    HighBitDepth,
}

impl fmt::Display for VpxError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Libvpx(e) => write!(f, "vpx: {e}"),
            Self::HighBitDepth => write!(f, "vpx: high-bit-depth frame, want 8-bit I420"),
        }
    }
}

impl std::error::Error for VpxError {}

impl From<shiguredo_libvpx::Error> for VpxError {
    fn from(e: shiguredo_libvpx::Error) -> Self {
        Self::Libvpx(e)
    }
}

/// One plane of a decoded frame: `stride` bytes per row, `rows` rows, the
/// first `width` bytes of each row being picture.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Plane {
    pub data: Vec<u8>,
    pub stride: usize,
    pub width: usize,
    pub rows: usize,
}

/// A decoded 8-bit I420 frame, owned: libvpx reuses its buffers on the next
/// decode. Chroma planes are `ceil(width / 2)` by `ceil(height / 2)`.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct I420Frame {
    pub width: usize,
    pub height: usize,
    pub y: Plane,
    pub u: Plane,
    pub v: Plane,
}

/// A libvpx decoder for one `DecoderConfig`. Feed it every frame in decode
/// order from a keyframe on (the delta-loss rule upstream, D13, guarantees
/// the chain); drop it and build another on a config change.
pub struct VpxDecoder {
    codec: VpxCodec,
    inner: shiguredo_libvpx::Decoder,
}

impl fmt::Debug for VpxDecoder {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("VpxDecoder")
            .field("codec", &self.codec)
            .finish_non_exhaustive()
    }
}

impl VpxDecoder {
    pub fn new(codec: VpxCodec) -> Result<Self, VpxError> {
        let inner = shiguredo_libvpx::Decoder::new(DecoderConfig::new(match codec {
            VpxCodec::Vp8 => DecoderCodec::Vp8,
            VpxCodec::Vp9 => DecoderCodec::Vp9,
        }))?;
        Ok(Self { codec, inner })
    }

    pub fn codec(&self) -> VpxCodec {
        self.codec
    }

    /// Decodes one compressed frame and returns what libvpx emitted for it:
    /// one frame normally, none for a frame it doesn't show.
    pub fn decode(&mut self, frame: &[u8]) -> Result<Vec<I420Frame>, VpxError> {
        self.inner.decode(frame)?;
        // Drain to the end every time, error or not: the binding refuses
        // the next decode while its frame iterator is mid-way.
        let mut out = Vec::new();
        let mut high_bit_depth = false;
        while let Some(f) = self.inner.next_frame()? {
            if f.is_high_depth() {
                high_bit_depth = true;
                continue;
            }
            let (width, height) = (f.width(), f.height());
            let (cw, ch) = (width.div_ceil(2), height.div_ceil(2));
            out.push(I420Frame {
                width,
                height,
                y: plane(f.y_plane(), f.y_stride(), width, height),
                u: plane(f.u_plane(), f.u_stride(), cw, ch),
                v: plane(f.v_plane(), f.v_stride(), cw, ch),
            });
        }
        if high_bit_depth {
            return Err(VpxError::HighBitDepth);
        }
        Ok(out)
    }
}

fn plane(data: &[u8], stride: usize, width: usize, rows: usize) -> Plane {
    Plane {
        data: data.to_vec(),
        stride,
        width,
        rows,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn maps_the_codec_strings_broadcasters_send() {
        assert_eq!(VpxCodec::from_codec_string("vp8"), Some(VpxCodec::Vp8));
        for vp9 in [
            "vp09.00.40.08",
            "vp09.00.31.08",
            "vp09.00.10.08.01.01.01.01.00",
        ] {
            assert_eq!(
                VpxCodec::from_codec_string(vp9),
                Some(VpxCodec::Vp9),
                "{vp9}"
            );
        }
        for other in ["avc1.42E02A", "av01.0.04M.08", "vp9", "vp08", "VP8", ""] {
            assert_eq!(VpxCodec::from_codec_string(other), None, "{other}");
        }
    }

    #[test]
    fn garbage_is_an_error_not_a_panic() {
        for codec in [VpxCodec::Vp8, VpxCodec::Vp9] {
            let mut d = VpxDecoder::new(codec).unwrap();
            assert_eq!(d.codec(), codec);
            assert!(d.decode(&[0xde, 0xad, 0xbe, 0xef]).is_err(), "{codec:?}");
        }
    }
}
