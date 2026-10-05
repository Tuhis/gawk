//! The viewer's decode layer (docs/67 D15, D16, OD12): what turns a
//! reassembled frame or an audio packet into something AVFoundation can
//! present. Pure, synchronous and host-tested; the session that feeds it and
//! the Swift that enqueues its output live elsewhere.
//!
//! - [`opus`]: `AudioFrame` packets to interleaved `f32` PCM through libopus
//!   (D16), configured by `AudioConfig` as the SPA's `AudioDecoder` is.
//! - [`vpx`]: VP8 and VP9 to I420 planes through a bundled libvpx (OD12),
//!   which Swift copies into `CVPixelBuffer`s.
//! - [`h264`]: no decode at all. H.264 is enqueued compressed to an
//!   `AVSampleBufferDisplayLayer` (D15), so this is the bitstream plumbing
//!   that needs: parameter sets for a `CMVideoFormatDescription` and access
//!   units in length-prefixed form, whichever shape the broadcaster sent.
//!
//! **A `DecoderConfig` change resets the decoder** (D13; D9's rotations
//! arrive that way): the caller drops the decoder it has and builds one from
//! the new config, and each decoder here is cheap to rebuild.

pub mod h264;
pub mod opus;
pub mod vpx;
