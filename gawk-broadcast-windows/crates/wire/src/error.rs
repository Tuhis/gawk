use std::fmt;

/// Errors returned by the parse and append functions. Variants mirror the Go
/// package's sentinel errors one for one (plus two the Go side folds into
/// `ErrBadType`: unknown delivery mode and unknown stripe flags).
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum WireError {
    /// The buffer is too small to contain the expected header (or, for a
    /// carrier record, the declared record) — for stream reads this means
    /// "read more", not "corrupt".
    ShortDatagram { len: usize, need: usize },
    /// Byte 0 is not [`crate::VERSION`].
    BadVersion(u8),
    /// Byte 1 is not the expected message type.
    BadType { got: u8, want: u8 },
    /// chunkCount == 0, chunkIndex >= chunkCount, or a count/index bound
    /// (MaxChunkCount, MaxParityDataChunks, parity index, stripe width) blown.
    BadChunkCount { index: u32, count: u32 },
    /// A payload exceeds its budget (MaxChunkPayload for video and parity).
    PayloadTooLarge { len: usize, max: usize },
    /// A codec string that is empty, longer than 255 bytes, overruns the
    /// datagram, or is not UTF-8 (Rust-mirror divergence, documented in lib.rs).
    BadCodec,
    /// An encoded datagram would exceed MaxDatagramSize.
    DatagramTooLarge { len: usize },
    /// A BroadcastAnnounce with an invalid length or a character outside the
    /// broadcast ID alphabet.
    BadBroadcastId,
    /// A StreamFrame whose declared or actual size exceeds MaxKeyframeBytes.
    KeyframeTooLarge,
    /// A fixed-size message whose length is not exactly the expected size.
    BadLength { len: usize, want: usize },
    /// A ResumeToken with an empty, oversized, or length-mismatched token.
    BadResumeToken,
    /// A carrier record whose declared length is zero or exceeds
    /// MaxDatagramSize — framing corruption; abandon the stream.
    BadCarrierRecord { declared: usize },
    /// An AudioFrame payload that is empty or exceeds MaxAudioPayload.
    BadAudioPayload { len: usize },
    /// An AudioConfig with a zero sample rate or zero channels.
    BadAudioConfig,
    /// A TelemetryHello with a wrong-length token/key or reserved flag bits set.
    BadTelemetryHello,
    /// A DeliveryAck naming a mode this build does not know. An unknown mode
    /// is an error, never a silent fallback.
    UnknownDeliveryMode(u8),
    /// A StripeState with unknown flag bits set.
    UnknownStripeFlags(u8),
    /// A frame shape parity cannot cover (k, n, or width out of range).
    ParityUnsupported,
}

impl fmt::Display for WireError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::ShortDatagram { len, need } => {
                write!(
                    f,
                    "wire: datagram too short: {len} bytes, need at least {need}"
                )
            }
            Self::BadVersion(v) => write!(f, "wire: unsupported version 0x{v:02x}"),
            Self::BadType { got, want } => {
                write!(
                    f,
                    "wire: unexpected message type 0x{got:02x}, want 0x{want:02x}"
                )
            }
            Self::BadChunkCount { index, count } => {
                write!(
                    f,
                    "wire: invalid chunk index/count: index {index}, count {count}"
                )
            }
            Self::PayloadTooLarge { len, max } => {
                write!(f, "wire: payload too large: {len} bytes, max {max}")
            }
            Self::BadCodec => write!(f, "wire: invalid codec string"),
            Self::DatagramTooLarge { len } => {
                write!(f, "wire: datagram exceeds MaxDatagramSize: {len} bytes")
            }
            Self::BadBroadcastId => write!(f, "wire: invalid broadcast ID"),
            Self::KeyframeTooLarge => write!(f, "wire: keyframe exceeds MaxKeyframeBytes"),
            Self::BadLength { len, want } => {
                write!(
                    f,
                    "wire: unexpected message length {len}, want exactly {want}"
                )
            }
            Self::BadResumeToken => write!(f, "wire: invalid resume token"),
            Self::BadCarrierRecord { declared } => {
                write!(f, "wire: invalid carrier record: declared {declared} bytes")
            }
            Self::BadAudioPayload { len } => {
                write!(f, "wire: invalid audio payload: {len} bytes")
            }
            Self::BadAudioConfig => write!(f, "wire: invalid audio config"),
            Self::BadTelemetryHello => write!(f, "wire: invalid telemetry hello"),
            Self::UnknownDeliveryMode(m) => write!(f, "wire: unknown delivery mode {m}"),
            Self::UnknownStripeFlags(b) => write!(f, "wire: unknown stripe flags 0x{b:02x}"),
            Self::ParityUnsupported => write!(f, "wire: parity unsupported for this frame"),
        }
    }
}

impl std::error::Error for WireError {}
