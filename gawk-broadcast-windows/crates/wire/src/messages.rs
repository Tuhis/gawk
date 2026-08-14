//! Message encoders and parsers, mirroring gawk-server/wire/wire.go function
//! for function. Append functions extend a `Vec<u8>`; parse functions borrow
//! the input (zero-copy, like the Go originals).

use crate::error::WireError;
use crate::{
    AUDIO_FRAME_HEADER_SIZE, BROADCAST_ID_ALPHABET, CARRIER_PROLOGUE_SIZE,
    CARRIER_RECORD_HEADER_SIZE, CLOCK_MAPPING_SIZE, DELIVERY_ACK_SIZE, FLAG_KEYFRAME,
    FLAG_TELEMETRY_ENABLED, MAX_AUDIO_PAYLOAD, MAX_CHUNK_COUNT, MAX_CHUNK_PAYLOAD,
    MAX_DATAGRAM_SIZE, MAX_KEYFRAME_BYTES, MAX_RELAY_IDENTITY_NAME_LEN,
    MAX_RELAY_IDENTITY_VERSION_LEN, MAX_TELEMETRY_ENDPOINT_URL_LEN, STREAM_FRAME_HEADER_SIZE,
    TELEMETRY_BROADCAST_KEY_SIZE, TELEMETRY_HELLO_SIZE, TELEMETRY_SESSION_TOKEN_SIZE,
    TIME_SYNC_SIZE, TYPE_AUDIO_CONFIG, TYPE_AUDIO_FRAME, TYPE_BROADCAST_ANNOUNCE,
    TYPE_CLOCK_MAPPING, TYPE_DECODER_CONFIG, TYPE_DELIVERY_ACK, TYPE_RELAY_IDENTITY,
    TYPE_RELIABLE_CARRIER, TYPE_RESUME_TOKEN, TYPE_STREAM_FRAME, TYPE_TELEMETRY_ENDPOINT,
    TYPE_TELEMETRY_HELLO, TYPE_TIME_SYNC, TYPE_VIDEO_CHUNK, TYPE_VIEWER_COUNT, VERSION,
    VIDEO_CHUNK_HEADER_SIZE, VIEWER_COUNT_SIZE,
};

fn check_prefix(buf: &[u8], want_type: u8) -> Result<(), WireError> {
    if buf[0] != VERSION {
        return Err(WireError::BadVersion(buf[0]));
    }
    if buf[1] != want_type {
        return Err(WireError::BadType {
            got: buf[1],
            want: want_type,
        });
    }
    Ok(())
}

// --- VideoChunk (0x01) -------------------------------------------------------

/// The parsed header of a VideoChunk datagram.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct VideoChunkHeader {
    /// True if this chunk belongs to a keyframe.
    pub keyframe: bool,
    /// Monotonic per publisher session, starting at 0, wrapping (serial
    /// arithmetic — continuity across resume is load-bearing, docs/38 D5).
    pub frame_id: u32,
    /// 0-based index of this chunk within the frame.
    pub chunk_index: u16,
    /// Total chunks in the frame; always >= 1 and > chunk_index.
    pub chunk_count: u16,
    /// Frame timestamp in microseconds.
    pub timestamp_us: u64,
}

/// Appends a VideoChunk datagram. Mirrors Go `AppendVideoChunk`: the payload
/// budget and index/count relation are checked; MaxChunkCount is a PARSE
/// bound only, exactly as in Go.
pub fn append_video_chunk(
    dst: &mut Vec<u8>,
    h: &VideoChunkHeader,
    payload: &[u8],
) -> Result<(), WireError> {
    if payload.len() > MAX_CHUNK_PAYLOAD {
        return Err(WireError::PayloadTooLarge {
            len: payload.len(),
            max: MAX_CHUNK_PAYLOAD,
        });
    }
    if h.chunk_count == 0 || h.chunk_index >= h.chunk_count {
        return Err(WireError::BadChunkCount {
            index: h.chunk_index.into(),
            count: h.chunk_count.into(),
        });
    }
    let flags = if h.keyframe { FLAG_KEYFRAME } else { 0 };
    dst.extend_from_slice(&[VERSION, TYPE_VIDEO_CHUNK, flags, 0]);
    dst.extend_from_slice(&h.frame_id.to_be_bytes());
    dst.extend_from_slice(&h.chunk_index.to_be_bytes());
    dst.extend_from_slice(&h.chunk_count.to_be_bytes());
    dst.extend_from_slice(&h.timestamp_us.to_be_bytes());
    dst.extend_from_slice(payload);
    Ok(())
}

/// Parses a VideoChunk datagram; the returned payload borrows `dgram`.
pub fn parse_video_chunk(dgram: &[u8]) -> Result<(VideoChunkHeader, &[u8]), WireError> {
    if dgram.len() < VIDEO_CHUNK_HEADER_SIZE {
        return Err(WireError::ShortDatagram {
            len: dgram.len(),
            need: VIDEO_CHUNK_HEADER_SIZE,
        });
    }
    check_prefix(dgram, TYPE_VIDEO_CHUNK)?;
    let h = VideoChunkHeader {
        keyframe: dgram[2] & FLAG_KEYFRAME != 0,
        frame_id: u32::from_be_bytes(dgram[4..8].try_into().unwrap()),
        chunk_index: u16::from_be_bytes(dgram[8..10].try_into().unwrap()),
        chunk_count: u16::from_be_bytes(dgram[10..12].try_into().unwrap()),
        timestamp_us: u64::from_be_bytes(dgram[12..20].try_into().unwrap()),
    };
    if h.chunk_count == 0 || h.chunk_index >= h.chunk_count || h.chunk_count > MAX_CHUNK_COUNT {
        return Err(WireError::BadChunkCount {
            index: h.chunk_index.into(),
            count: h.chunk_count.into(),
        });
    }
    Ok((h, &dgram[VIDEO_CHUNK_HEADER_SIZE..]))
}

// --- DecoderConfig (0x02) ----------------------------------------------------

/// The parsed contents of a DecoderConfig datagram, borrowing the input.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct DecoderConfig<'a> {
    /// WebCodecs codec string, e.g. "avc1.42E02A". 1-255 bytes on the wire.
    pub codec: &'a str,
    /// Codec-specific configuration. EMPTY on this producer's path, always:
    /// we emit Annex-B and the viewer's start-code sniff routes accordingly
    /// (docs/38 D10) — pinned by test, never an avcC record.
    pub extradata: &'a [u8],
}

/// Appends a DecoderConfig datagram.
pub fn append_decoder_config(
    dst: &mut Vec<u8>,
    codec: &str,
    extradata: &[u8],
) -> Result<(), WireError> {
    if codec.is_empty() || codec.len() > 255 {
        return Err(WireError::BadCodec);
    }
    let total = 4 + codec.len() + extradata.len();
    if total > MAX_DATAGRAM_SIZE {
        return Err(WireError::DatagramTooLarge { len: total });
    }
    dst.extend_from_slice(&[VERSION, TYPE_DECODER_CONFIG, 0, codec.len() as u8]);
    dst.extend_from_slice(codec.as_bytes());
    dst.extend_from_slice(extradata);
    Ok(())
}

/// Parses a DecoderConfig datagram; codec and extradata borrow `dgram`.
pub fn parse_decoder_config(dgram: &[u8]) -> Result<DecoderConfig<'_>, WireError> {
    if dgram.len() < 4 {
        return Err(WireError::ShortDatagram {
            len: dgram.len(),
            need: 4,
        });
    }
    check_prefix(dgram, TYPE_DECODER_CONFIG)?;
    let codec_len = dgram[3] as usize;
    if codec_len == 0 {
        return Err(WireError::BadCodec);
    }
    if 4 + codec_len > dgram.len() {
        return Err(WireError::BadCodec);
    }
    let codec = std::str::from_utf8(&dgram[4..4 + codec_len]).map_err(|_| WireError::BadCodec)?;
    Ok(DecoderConfig {
        codec,
        extradata: &dgram[4 + codec_len..],
    })
}

// --- BroadcastAnnounce (0x03) ------------------------------------------------

/// Appends a BroadcastAnnounce message. Like Go, append checks only the
/// length; the alphabet is a parse-side rule.
pub fn append_broadcast_announce(dst: &mut Vec<u8>, id: &str) -> Result<(), WireError> {
    if id.is_empty() || id.len() > 255 {
        return Err(WireError::BadBroadcastId);
    }
    dst.extend_from_slice(&[VERSION, TYPE_BROADCAST_ANNOUNCE, id.len() as u8]);
    dst.extend_from_slice(id.as_bytes());
    Ok(())
}

/// Parses a BroadcastAnnounce message. Strict: the declared ID length must
/// account for the entire message and every character must be in the
/// broadcast ID alphabet.
pub fn parse_broadcast_announce(msg: &[u8]) -> Result<&str, WireError> {
    if msg.len() < 3 {
        return Err(WireError::ShortDatagram {
            len: msg.len(),
            need: 3,
        });
    }
    check_prefix(msg, TYPE_BROADCAST_ANNOUNCE)?;
    let id_len = msg[2] as usize;
    if 3 + id_len != msg.len() {
        return Err(WireError::BadBroadcastId);
    }
    let id = &msg[3..];
    if !id
        .iter()
        .all(|b| BROADCAST_ID_ALPHABET.as_bytes().contains(b))
    {
        return Err(WireError::BadBroadcastId);
    }
    // Alphabet bytes are ASCII, so this cannot fail after the check above.
    std::str::from_utf8(id).map_err(|_| WireError::BadBroadcastId)
}

// --- TimeSync (0x05) ---------------------------------------------------------

/// Appends a TimeSync datagram. A request carries `server_time_us == 0`; the
/// relay echoes the client time and fills its monotonic clock.
pub fn append_time_sync(dst: &mut Vec<u8>, client_time_us: u64, server_time_us: u64) {
    dst.extend_from_slice(&[VERSION, TYPE_TIME_SYNC]);
    dst.extend_from_slice(&client_time_us.to_be_bytes());
    dst.extend_from_slice(&server_time_us.to_be_bytes());
}

/// Parses a TimeSync datagram. Strict: exactly [`TIME_SYNC_SIZE`] bytes.
pub fn parse_time_sync(dgram: &[u8]) -> Result<(u64, u64), WireError> {
    if dgram.len() != TIME_SYNC_SIZE {
        return Err(WireError::BadLength {
            len: dgram.len(),
            want: TIME_SYNC_SIZE,
        });
    }
    check_prefix(dgram, TYPE_TIME_SYNC)?;
    Ok((
        u64::from_be_bytes(dgram[2..10].try_into().unwrap()),
        u64::from_be_bytes(dgram[10..18].try_into().unwrap()),
    ))
}

// --- ClockMapping (0x06) -----------------------------------------------------

/// Appends a ClockMapping datagram: relayClockUs = timestampUs + offsetUs,
/// two's-complement wraparound intended on both sides.
pub fn append_clock_mapping(dst: &mut Vec<u8>, offset_us: i64) {
    dst.extend_from_slice(&[VERSION, TYPE_CLOCK_MAPPING]);
    dst.extend_from_slice(&offset_us.to_be_bytes());
}

/// Parses a ClockMapping datagram. Strict: exactly [`CLOCK_MAPPING_SIZE`] bytes.
pub fn parse_clock_mapping(dgram: &[u8]) -> Result<i64, WireError> {
    if dgram.len() != CLOCK_MAPPING_SIZE {
        return Err(WireError::BadLength {
            len: dgram.len(),
            want: CLOCK_MAPPING_SIZE,
        });
    }
    check_prefix(dgram, TYPE_CLOCK_MAPPING)?;
    Ok(i64::from_be_bytes(dgram[2..10].try_into().unwrap()))
}

// --- ViewerCount (0x0B) ------------------------------------------------------

/// Appends a ViewerCount datagram (relay-originated; this producer only
/// parses it, but the encoder is mirrored by rule like every other message).
pub fn append_viewer_count(dst: &mut Vec<u8>, count: u32) {
    dst.extend_from_slice(&[VERSION, TYPE_VIEWER_COUNT]);
    dst.extend_from_slice(&count.to_be_bytes());
}

/// Parses a ViewerCount datagram. Strict: exactly [`VIEWER_COUNT_SIZE`] bytes.
pub fn parse_viewer_count(dgram: &[u8]) -> Result<u32, WireError> {
    if dgram.len() != VIEWER_COUNT_SIZE {
        return Err(WireError::BadLength {
            len: dgram.len(),
            want: VIEWER_COUNT_SIZE,
        });
    }
    check_prefix(dgram, TYPE_VIEWER_COUNT)?;
    Ok(u32::from_be_bytes(dgram[2..6].try_into().unwrap()))
}

// --- DeliveryAck (0x0C) ------------------------------------------------------

/// What a subscriber is actually being served. Wire-visible values: append,
/// never renumber.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
#[repr(u8)]
pub enum DeliveryMode {
    Datagrams = 0,
    Reliable = 1,
    Dvr = 2,
}

/// Appends a DeliveryAck datagram. `buffer_ms` is 0 whenever mode is not DVR.
pub fn append_delivery_ack(dst: &mut Vec<u8>, mode: DeliveryMode, buffer_ms: u16) {
    dst.extend_from_slice(&[VERSION, TYPE_DELIVERY_ACK, mode as u8]);
    dst.extend_from_slice(&buffer_ms.to_be_bytes());
}

/// Parses a DeliveryAck datagram. An unknown mode is an error rather than a
/// silent fallback — a viewer that cannot name what it was served is the gap
/// this message closes.
pub fn parse_delivery_ack(dgram: &[u8]) -> Result<(DeliveryMode, u16), WireError> {
    if dgram.len() != DELIVERY_ACK_SIZE {
        return Err(WireError::BadLength {
            len: dgram.len(),
            want: DELIVERY_ACK_SIZE,
        });
    }
    check_prefix(dgram, TYPE_DELIVERY_ACK)?;
    let mode = match dgram[2] {
        0 => DeliveryMode::Datagrams,
        1 => DeliveryMode::Reliable,
        2 => DeliveryMode::Dvr,
        other => return Err(WireError::UnknownDeliveryMode(other)),
    };
    Ok((mode, u16::from_be_bytes(dgram[3..5].try_into().unwrap())))
}

// --- TelemetryHello (0x0D) ---------------------------------------------------

/// The parsed contents of a TelemetryHello message, borrowing the input.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct TelemetryHello<'a> {
    /// False means: collect nothing, ignore the other fields — identical to a
    /// relay predating R28 sending no hello at all.
    pub enabled: bool,
    /// The sampling cadence the relay asks clients to use.
    pub report_interval_ms: u16,
    /// The stateless-verifiable session credential. Never logged.
    pub token: &'a [u8],
    /// The obfuscated broadcast key (never the joinable raw ID).
    pub broadcast_key: &'a [u8],
}

/// Appends a TelemetryHello message. Both variable fields are fixed-width on
/// the wire; a wrong length is a bug on the minting side, never a truncation
/// to paper over.
pub fn append_telemetry_hello(dst: &mut Vec<u8>, h: &TelemetryHello<'_>) -> Result<(), WireError> {
    if h.token.len() != TELEMETRY_SESSION_TOKEN_SIZE
        || h.broadcast_key.len() != TELEMETRY_BROADCAST_KEY_SIZE
    {
        return Err(WireError::BadTelemetryHello);
    }
    let flags = if h.enabled { FLAG_TELEMETRY_ENABLED } else { 0 };
    dst.extend_from_slice(&[VERSION, TYPE_TELEMETRY_HELLO, flags]);
    dst.extend_from_slice(&h.report_interval_ms.to_be_bytes());
    dst.extend_from_slice(h.token);
    dst.extend_from_slice(h.broadcast_key);
    Ok(())
}

/// Parses a TelemetryHello message. Strict: exact length, reserved flag bits
/// clear — a set reserved bit means a future field this build would misread.
pub fn parse_telemetry_hello(msg: &[u8]) -> Result<TelemetryHello<'_>, WireError> {
    if msg.len() != TELEMETRY_HELLO_SIZE {
        return Err(WireError::BadLength {
            len: msg.len(),
            want: TELEMETRY_HELLO_SIZE,
        });
    }
    check_prefix(msg, TYPE_TELEMETRY_HELLO)?;
    if msg[2] & !FLAG_TELEMETRY_ENABLED != 0 {
        return Err(WireError::BadTelemetryHello);
    }
    Ok(TelemetryHello {
        enabled: msg[2] & FLAG_TELEMETRY_ENABLED != 0,
        report_interval_ms: u16::from_be_bytes(msg[3..5].try_into().unwrap()),
        token: &msg[5..5 + TELEMETRY_SESSION_TOKEN_SIZE],
        broadcast_key: &msg[5 + TELEMETRY_SESSION_TOKEN_SIZE..],
    })
}

// --- AudioFrame (0x07) -------------------------------------------------------

/// The parsed header of an AudioFrame datagram.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct AudioFrameHeader {
    /// Audio's own u32 sequence space, independent of video frame IDs, same
    /// wrap-aware serial arithmetic.
    pub seq: u32,
    /// On the same clock video capture stamps — A/V skew is a subtraction.
    pub timestamp_us: u64,
}

/// Appends an AudioFrame datagram: exactly one Opus packet, never chunked.
pub fn append_audio_frame(
    dst: &mut Vec<u8>,
    h: &AudioFrameHeader,
    payload: &[u8],
) -> Result<(), WireError> {
    if payload.is_empty() || payload.len() > MAX_AUDIO_PAYLOAD {
        return Err(WireError::BadAudioPayload { len: payload.len() });
    }
    dst.extend_from_slice(&[VERSION, TYPE_AUDIO_FRAME, 0, 0]);
    dst.extend_from_slice(&h.seq.to_be_bytes());
    dst.extend_from_slice(&h.timestamp_us.to_be_bytes());
    dst.extend_from_slice(payload);
    Ok(())
}

/// Parses an AudioFrame datagram; the payload borrows `dgram`. An empty
/// payload is an error — there is no zero-byte Opus packet (DTX is off).
pub fn parse_audio_frame(dgram: &[u8]) -> Result<(AudioFrameHeader, &[u8]), WireError> {
    if dgram.len() < AUDIO_FRAME_HEADER_SIZE {
        return Err(WireError::ShortDatagram {
            len: dgram.len(),
            need: AUDIO_FRAME_HEADER_SIZE,
        });
    }
    check_prefix(dgram, TYPE_AUDIO_FRAME)?;
    if dgram.len() == AUDIO_FRAME_HEADER_SIZE {
        return Err(WireError::BadAudioPayload { len: 0 });
    }
    let h = AudioFrameHeader {
        seq: u32::from_be_bytes(dgram[4..8].try_into().unwrap()),
        timestamp_us: u64::from_be_bytes(dgram[8..16].try_into().unwrap()),
    };
    Ok((h, &dgram[AUDIO_FRAME_HEADER_SIZE..]))
}

// --- AudioConfig (0x08) ------------------------------------------------------

/// The parsed contents of an AudioConfig datagram, borrowing the input.
/// Production values: "opus", 48000, 2, empty description (docs/38 D8).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct AudioConfig<'a> {
    pub codec: &'a str,
    pub sample_rate: u32,
    pub channels: u8,
    pub description: &'a [u8],
}

/// Appends an AudioConfig datagram.
pub fn append_audio_config(dst: &mut Vec<u8>, c: &AudioConfig<'_>) -> Result<(), WireError> {
    if c.codec.is_empty() || c.codec.len() > 255 {
        return Err(WireError::BadCodec);
    }
    if c.sample_rate == 0 || c.channels == 0 {
        return Err(WireError::BadAudioConfig);
    }
    let total = 4 + c.codec.len() + 5 + c.description.len();
    if total > MAX_DATAGRAM_SIZE {
        return Err(WireError::DatagramTooLarge { len: total });
    }
    dst.extend_from_slice(&[VERSION, TYPE_AUDIO_CONFIG, 0, c.codec.len() as u8]);
    dst.extend_from_slice(c.codec.as_bytes());
    dst.extend_from_slice(&c.sample_rate.to_be_bytes());
    dst.push(c.channels);
    dst.extend_from_slice(c.description);
    Ok(())
}

/// Parses an AudioConfig datagram; codec and description borrow `dgram`.
pub fn parse_audio_config(dgram: &[u8]) -> Result<AudioConfig<'_>, WireError> {
    if dgram.len() < 4 {
        return Err(WireError::ShortDatagram {
            len: dgram.len(),
            need: 4,
        });
    }
    check_prefix(dgram, TYPE_AUDIO_CONFIG)?;
    let codec_len = dgram[3] as usize;
    if codec_len == 0 {
        return Err(WireError::BadCodec);
    }
    if 4 + codec_len + 5 > dgram.len() {
        return Err(WireError::BadCodec);
    }
    let codec = std::str::from_utf8(&dgram[4..4 + codec_len]).map_err(|_| WireError::BadCodec)?;
    let c = AudioConfig {
        codec,
        sample_rate: u32::from_be_bytes(
            dgram[4 + codec_len..4 + codec_len + 4].try_into().unwrap(),
        ),
        channels: dgram[4 + codec_len + 4],
        description: &dgram[4 + codec_len + 5..],
    };
    if c.sample_rate == 0 || c.channels == 0 {
        return Err(WireError::BadAudioConfig);
    }
    Ok(c)
}

// --- ResumeToken (0x09) ------------------------------------------------------

/// Appends a ResumeToken message. The token is opaque on the wire (the server
/// mints truncated HMACs, but the format doesn't care).
pub fn append_resume_token(dst: &mut Vec<u8>, token: &[u8]) -> Result<(), WireError> {
    if token.is_empty() || token.len() > 255 {
        return Err(WireError::BadResumeToken);
    }
    dst.extend_from_slice(&[VERSION, TYPE_RESUME_TOKEN, token.len() as u8]);
    dst.extend_from_slice(token);
    Ok(())
}

/// Parses a ResumeToken message. Strict: the declared token length must
/// account for the entire message. The returned slice borrows `msg`.
pub fn parse_resume_token(msg: &[u8]) -> Result<&[u8], WireError> {
    if msg.len() < 3 {
        return Err(WireError::ShortDatagram {
            len: msg.len(),
            need: 3,
        });
    }
    check_prefix(msg, TYPE_RESUME_TOKEN)?;
    let token_len = msg[2] as usize;
    if token_len == 0 || 3 + token_len != msg.len() {
        return Err(WireError::BadResumeToken);
    }
    Ok(&msg[3..])
}

// --- ReliableCarrier (0x0A) --------------------------------------------------

/// Appends the two-byte reliable-carrier stream prologue, written exactly
/// once per carrier stream before the first record.
pub fn append_carrier_prologue(dst: &mut Vec<u8>) {
    dst.extend_from_slice(&[VERSION, TYPE_RELIABLE_CARRIER]);
}

/// Validates the two-byte carrier prologue at the start of `buf`. Does not
/// require `buf` to hold anything beyond it — records follow on the stream.
pub fn parse_carrier_prologue(buf: &[u8]) -> Result<(), WireError> {
    if buf.len() < CARRIER_PROLOGUE_SIZE {
        return Err(WireError::ShortDatagram {
            len: buf.len(),
            need: CARRIER_PROLOGUE_SIZE,
        });
    }
    check_prefix(buf, TYPE_RELIABLE_CARRIER)
}

/// Appends one carrier record (u16 BE length, then the datagram verbatim).
pub fn append_carrier_record(dst: &mut Vec<u8>, dgram: &[u8]) -> Result<(), WireError> {
    if dgram.is_empty() || dgram.len() > MAX_DATAGRAM_SIZE {
        return Err(WireError::BadCarrierRecord {
            declared: dgram.len(),
        });
    }
    dst.extend_from_slice(&(dgram.len() as u16).to_be_bytes());
    dst.extend_from_slice(dgram);
    Ok(())
}

/// Parses one carrier record at the start of `buf`, returning the record's
/// datagram bytes (borrowing `buf`) and the remainder after it. An incomplete
/// record returns [`WireError::ShortDatagram`] (read more from the stream); a
/// zero or oversize declared length returns [`WireError::BadCarrierRecord`]
/// (framing corruption — abandon the stream).
pub fn parse_carrier_record(buf: &[u8]) -> Result<(&[u8], &[u8]), WireError> {
    if buf.len() < CARRIER_RECORD_HEADER_SIZE {
        return Err(WireError::ShortDatagram {
            len: buf.len(),
            need: CARRIER_RECORD_HEADER_SIZE,
        });
    }
    let n = u16::from_be_bytes(buf[..2].try_into().unwrap()) as usize;
    if n == 0 || n > MAX_DATAGRAM_SIZE {
        return Err(WireError::BadCarrierRecord { declared: n });
    }
    if buf.len() < CARRIER_RECORD_HEADER_SIZE + n {
        return Err(WireError::ShortDatagram {
            len: buf.len(),
            need: CARRIER_RECORD_HEADER_SIZE + n,
        });
    }
    Ok((
        &buf[CARRIER_RECORD_HEADER_SIZE..CARRIER_RECORD_HEADER_SIZE + n],
        &buf[CARRIER_RECORD_HEADER_SIZE + n..],
    ))
}

// --- StreamFrame (0x04) ------------------------------------------------------

/// The parsed 24-byte header of a StreamFrame message: the entire payload of
/// one unidirectional stream — this header, then `config_len` bytes of an
/// embedded DecoderConfig datagram (0x01/0x02 prefix included, or nothing
/// when 0), then `payload_len` bytes of the raw encoded keyframe.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct StreamFrameHeader {
    /// Always true today; reserved so future non-keyframe stream frames stay
    /// distinguishable.
    pub keyframe: bool,
    /// Shared numbering space with datagram VideoChunks, so the viewer can
    /// order keyframes (streams) against deltas (datagrams).
    pub frame_id: u32,
    pub timestamp_us: u64,
    /// Byte length of the embedded DecoderConfig datagram (0 if none).
    pub config_len: u32,
    /// Byte length of the encoded keyframe following the config block.
    pub payload_len: u32,
}

/// Appends the 24-byte StreamFrame header.
pub fn append_stream_frame_header(
    dst: &mut Vec<u8>,
    h: &StreamFrameHeader,
) -> Result<(), WireError> {
    if STREAM_FRAME_HEADER_SIZE as u64 + u64::from(h.config_len) + u64::from(h.payload_len)
        > MAX_KEYFRAME_BYTES as u64
    {
        return Err(WireError::KeyframeTooLarge);
    }
    let flags = if h.keyframe { FLAG_KEYFRAME } else { 0 };
    dst.extend_from_slice(&[VERSION, TYPE_STREAM_FRAME, flags, 0]);
    dst.extend_from_slice(&h.frame_id.to_be_bytes());
    dst.extend_from_slice(&h.timestamp_us.to_be_bytes());
    dst.extend_from_slice(&h.config_len.to_be_bytes());
    dst.extend_from_slice(&h.payload_len.to_be_bytes());
    Ok(())
}

/// Parses the 24-byte StreamFrame header at the start of `buf`. Validates
/// version/type and the MaxKeyframeBytes bound, but does not require `buf` to
/// contain the whole message — the caller reads `config_len + payload_len`
/// further bytes from the stream.
pub fn parse_stream_frame_header(buf: &[u8]) -> Result<StreamFrameHeader, WireError> {
    if buf.len() < STREAM_FRAME_HEADER_SIZE {
        return Err(WireError::ShortDatagram {
            len: buf.len(),
            need: STREAM_FRAME_HEADER_SIZE,
        });
    }
    check_prefix(buf, TYPE_STREAM_FRAME)?;
    let h = StreamFrameHeader {
        keyframe: buf[2] & FLAG_KEYFRAME != 0,
        frame_id: u32::from_be_bytes(buf[4..8].try_into().unwrap()),
        timestamp_us: u64::from_be_bytes(buf[8..16].try_into().unwrap()),
        config_len: u32::from_be_bytes(buf[16..20].try_into().unwrap()),
        payload_len: u32::from_be_bytes(buf[20..24].try_into().unwrap()),
    };
    if STREAM_FRAME_HEADER_SIZE as u64 + u64::from(h.config_len) + u64::from(h.payload_len)
        > MAX_KEYFRAME_BYTES as u64
    {
        return Err(WireError::KeyframeTooLarge);
    }
    Ok(h)
}

// --- RelayIdentity (0x11) ----------------------------------------------------

/// The parsed contents of a RelayIdentity message (R37, docs/40 §4.4),
/// borrowing the input. Layout after the common prefix: uint8 flags (all
/// reserved, must be 0), uint8 versionLen + that many printable-ASCII bytes,
/// uint8 nameLen + that many UTF-8 bytes, then reserved extension bytes.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct RelayIdentity<'a> {
    /// The relay's release version string, e.g. "1.42.0".
    pub server_version: &'a str,
    /// The operator-set display name (-server-name), possibly empty.
    /// Attacker-influenced trust UI: renderers sanitize and never show it in
    /// place of the host (docs/40 §4.4 F6).
    pub name: &'a str,
}

/// Appends a RelayIdentity message (relay-originated; this producer only
/// parses it, but the encoder is mirrored by rule like every other message).
pub fn append_relay_identity(dst: &mut Vec<u8>, id: &RelayIdentity<'_>) -> Result<(), WireError> {
    if id.server_version.is_empty() || id.server_version.len() > MAX_RELAY_IDENTITY_VERSION_LEN {
        return Err(WireError::BadRelayIdentity);
    }
    if !id
        .server_version
        .bytes()
        .all(|b| (0x20..=0x7e).contains(&b))
    {
        return Err(WireError::BadRelayIdentity);
    }
    // A &str is UTF-8 by construction; only the length can be wrong here
    // (the Go original also rejects invalid UTF-8 on append).
    if id.name.len() > MAX_RELAY_IDENTITY_NAME_LEN {
        return Err(WireError::BadRelayIdentity);
    }
    dst.extend_from_slice(&[
        VERSION,
        TYPE_RELAY_IDENTITY,
        0,
        id.server_version.len() as u8,
    ]);
    dst.extend_from_slice(id.server_version.as_bytes());
    dst.push(id.name.len() as u8);
    dst.extend_from_slice(id.name.as_bytes());
    Ok(())
}

/// Parses a RelayIdentity message. Deliberate deviation from house parser
/// strictness (docs/40 §4.4, restated so it survives review): trailing bytes
/// beyond the name are IGNORED — they are the message's reserved extension
/// space, the mechanism managed mode will append fields through. The flags
/// byte stays strict (reserved-must-be-zero): flags gate *interpretation* of
/// what a parser already reads, so an unknown flag genuinely is unparseable.
pub fn parse_relay_identity(msg: &[u8]) -> Result<RelayIdentity<'_>, WireError> {
    if msg.len() < 5 {
        return Err(WireError::ShortDatagram {
            len: msg.len(),
            need: 5,
        });
    }
    check_prefix(msg, TYPE_RELAY_IDENTITY)?;
    if msg[2] != 0 {
        return Err(WireError::BadRelayIdentity);
    }
    let version_len = msg[3] as usize;
    if version_len == 0 || version_len > MAX_RELAY_IDENTITY_VERSION_LEN {
        return Err(WireError::BadRelayIdentity);
    }
    if 4 + version_len + 1 > msg.len() {
        return Err(WireError::BadRelayIdentity);
    }
    let version_bytes = &msg[4..4 + version_len];
    if !version_bytes.iter().all(|b| (0x20..=0x7e).contains(b)) {
        return Err(WireError::BadRelayIdentity);
    }
    // Printable ASCII, so this cannot fail after the check above.
    let server_version =
        std::str::from_utf8(version_bytes).map_err(|_| WireError::BadRelayIdentity)?;
    let name_len = msg[4 + version_len] as usize;
    if name_len > MAX_RELAY_IDENTITY_NAME_LEN {
        return Err(WireError::BadRelayIdentity);
    }
    let name_start = 4 + version_len + 1;
    if name_start + name_len > msg.len() {
        return Err(WireError::BadRelayIdentity);
    }
    let name = std::str::from_utf8(&msg[name_start..name_start + name_len])
        .map_err(|_| WireError::BadRelayIdentity)?;
    // Bytes past the name are the reserved extension space — ignored.
    Ok(RelayIdentity {
        server_version,
        name,
    })
}

// --- TelemetryEndpoint (0x12) ------------------------------------------------

/// Appends a TelemetryEndpoint message (R37, docs/40 §4.10; relay-originated,
/// mirrored by rule). Layout after the common prefix: uint8 flags (0), uint16
/// urlLen (big-endian), then urlLen bytes of an absolute https URL.
pub fn append_telemetry_endpoint(dst: &mut Vec<u8>, ingest_url: &str) -> Result<(), WireError> {
    if !valid_telemetry_endpoint_url(ingest_url) {
        return Err(WireError::BadTelemetryEndpoint);
    }
    dst.extend_from_slice(&[VERSION, TYPE_TELEMETRY_ENDPOINT, 0]);
    dst.extend_from_slice(&(ingest_url.len() as u16).to_be_bytes());
    dst.extend_from_slice(ingest_url.as_bytes());
    Ok(())
}

/// Parses a TelemetryEndpoint message. Same parser stance as RelayIdentity:
/// strict flags, tolerated trailing bytes, and the URL must validate — a
/// message that fails here degrades client-side to "no advertised URL",
/// never to a failed session (docs/40 §4.10).
pub fn parse_telemetry_endpoint(msg: &[u8]) -> Result<&str, WireError> {
    if msg.len() < 6 {
        return Err(WireError::ShortDatagram {
            len: msg.len(),
            need: 6,
        });
    }
    check_prefix(msg, TYPE_TELEMETRY_ENDPOINT)?;
    if msg[2] != 0 {
        return Err(WireError::BadTelemetryEndpoint);
    }
    let url_len = u16::from_be_bytes(msg[3..5].try_into().unwrap()) as usize;
    if url_len == 0 || url_len > MAX_TELEMETRY_ENDPOINT_URL_LEN {
        return Err(WireError::BadTelemetryEndpoint);
    }
    if 5 + url_len > msg.len() {
        return Err(WireError::BadTelemetryEndpoint);
    }
    let ingest_url =
        std::str::from_utf8(&msg[5..5 + url_len]).map_err(|_| WireError::BadTelemetryEndpoint)?;
    if !valid_telemetry_endpoint_url(ingest_url) {
        return Err(WireError::BadTelemetryEndpoint);
    }
    // Bytes past the URL are the reserved extension space — ignored.
    Ok(ingest_url)
}

/// The one rule for advertised ingest URLs, applied on append and on parse
/// like the Go original's validateTelemetryEndpointURL. Documented mirror
/// divergence: Go delegates structure to url.Parse; this is a conservative
/// hand check (printable ASCII above space, "https://" case-insensitive,
/// non-empty host) — an exotic host Go's parser would reject can pass here,
/// but the engine re-validates any adopted URL with a full URL parser, and a
/// well-behaved relay never emits one.
fn valid_telemetry_endpoint_url(s: &str) -> bool {
    if s.is_empty() || s.len() > MAX_TELEMETRY_ENDPOINT_URL_LEN {
        return false;
    }
    if !s.bytes().all(|b| (0x21..=0x7e).contains(&b)) {
        return false;
    }
    let Some(rest) = s
        .get(..8)
        .filter(|p| p.eq_ignore_ascii_case("https://"))
        .map(|_| &s[8..])
    else {
        return false;
    };
    let host = rest.split(['/', '?', '#']).next().unwrap_or("");
    !host.is_empty()
}

#[cfg(test)]
mod tests {
    use super::*;

    // Strict-parse rejection coverage. The golden byte pins live in
    // tests/golden.rs; these assert the refusal semantics the relay and the
    // other mirrors rely on.

    fn video_chunk(h: &VideoChunkHeader, payload: &[u8]) -> Vec<u8> {
        let mut v = Vec::new();
        append_video_chunk(&mut v, h, payload).unwrap();
        v
    }

    #[test]
    fn video_chunk_round_trip_and_bounds() {
        let h = VideoChunkHeader {
            keyframe: false,
            frame_id: 42,
            chunk_index: 2,
            chunk_count: 7,
            timestamp_us: 123_456,
        };
        let d = video_chunk(&h, b"payload");
        let (got, payload) = parse_video_chunk(&d).unwrap();
        assert_eq!(got, h);
        assert_eq!(payload, b"payload");

        // Empty payload is legal (a zero-length frame still emits one chunk).
        let d = video_chunk(
            &VideoChunkHeader {
                chunk_index: 0,
                chunk_count: 1,
                ..h
            },
            b"",
        );
        let (_, payload) = parse_video_chunk(&d).unwrap();
        assert!(payload.is_empty());

        assert!(matches!(
            append_video_chunk(&mut Vec::new(), &h, &vec![0u8; MAX_CHUNK_PAYLOAD + 1]),
            Err(WireError::PayloadTooLarge { .. })
        ));
        assert!(matches!(
            append_video_chunk(
                &mut Vec::new(),
                &VideoChunkHeader {
                    chunk_index: 7,
                    chunk_count: 7,
                    ..h
                },
                b""
            ),
            Err(WireError::BadChunkCount { .. })
        ));
    }

    #[test]
    fn video_chunk_parse_rejects_malformed() {
        let good = video_chunk(
            &VideoChunkHeader {
                keyframe: true,
                frame_id: 1,
                chunk_index: 0,
                chunk_count: 1,
                timestamp_us: 1,
            },
            b"x",
        );
        assert!(parse_video_chunk(&good[..VIDEO_CHUNK_HEADER_SIZE - 1]).is_err());

        let mut bad = good.clone();
        bad[0] = 0x02;
        assert!(matches!(
            parse_video_chunk(&bad),
            Err(WireError::BadVersion(0x02))
        ));

        let mut bad = good.clone();
        bad[1] = TYPE_DECODER_CONFIG;
        assert!(matches!(
            parse_video_chunk(&bad),
            Err(WireError::BadType { .. })
        ));

        // chunkCount == 0.
        let mut bad = good.clone();
        bad[10] = 0;
        bad[11] = 0;
        assert!(matches!(
            parse_video_chunk(&bad),
            Err(WireError::BadChunkCount { .. })
        ));

        // chunkCount > MAX_CHUNK_COUNT is a parse bound (memory-inflation guard).
        let mut bad = good.clone();
        bad[10..12].copy_from_slice(&(MAX_CHUNK_COUNT + 1).to_be_bytes());
        assert!(matches!(
            parse_video_chunk(&bad),
            Err(WireError::BadChunkCount { .. })
        ));
    }

    #[test]
    fn decoder_config_rejects_bad_codec() {
        assert_eq!(
            append_decoder_config(&mut Vec::new(), "", b""),
            Err(WireError::BadCodec)
        );
        let long = "x".repeat(256);
        assert_eq!(
            append_decoder_config(&mut Vec::new(), &long, b""),
            Err(WireError::BadCodec)
        );
        assert!(matches!(
            append_decoder_config(&mut Vec::new(), "vp8", &vec![0u8; MAX_DATAGRAM_SIZE]),
            Err(WireError::DatagramTooLarge { .. })
        ));

        // codecLen overrunning the datagram.
        let mut d = Vec::new();
        append_decoder_config(&mut d, "vp8", b"").unwrap();
        let mut bad = d.clone();
        bad[3] = 200;
        assert_eq!(parse_decoder_config(&bad), Err(WireError::BadCodec));
        let mut bad = d;
        bad[3] = 0;
        assert_eq!(parse_decoder_config(&bad), Err(WireError::BadCodec));
    }

    #[test]
    fn announce_validates_alphabet_and_exact_length() {
        let mut msg = Vec::new();
        append_broadcast_announce(&mut msg, "K7XQ2M").unwrap();
        assert_eq!(parse_broadcast_announce(&msg).unwrap(), "K7XQ2M");

        // '0', 'O', '1', 'I', 'L' are excluded from the alphabet.
        for bad_id in ["K7XQ20", "OOOOOO", "ABC1DE", "ILLILL"] {
            let mut m = Vec::new();
            append_broadcast_announce(&mut m, bad_id).unwrap();
            assert_eq!(
                parse_broadcast_announce(&m),
                Err(WireError::BadBroadcastId),
                "{bad_id}"
            );
        }

        // Declared length must account for the entire message.
        let mut trailing = msg.clone();
        trailing.push(b'A');
        assert_eq!(
            parse_broadcast_announce(&trailing),
            Err(WireError::BadBroadcastId)
        );
    }

    #[test]
    fn fixed_size_messages_reject_any_other_length() {
        let mut ts = Vec::new();
        append_time_sync(&mut ts, 1, 2);
        assert!(parse_time_sync(&ts[..TIME_SYNC_SIZE - 1]).is_err());
        let mut long = ts.clone();
        long.push(0);
        assert!(parse_time_sync(&long).is_err());

        let mut cm = Vec::new();
        append_clock_mapping(&mut cm, -5);
        assert_eq!(parse_clock_mapping(&cm).unwrap(), -5);
        assert!(parse_clock_mapping(&cm[..CLOCK_MAPPING_SIZE - 1]).is_err());

        let mut vc = Vec::new();
        append_viewer_count(&mut vc, 9);
        assert_eq!(parse_viewer_count(&vc).unwrap(), 9);
        let mut long = vc.clone();
        long.push(0);
        assert!(parse_viewer_count(&long).is_err());

        let mut ack = Vec::new();
        append_delivery_ack(&mut ack, DeliveryMode::Dvr, 3000);
        assert_eq!(parse_delivery_ack(&ack).unwrap(), (DeliveryMode::Dvr, 3000));
        assert!(parse_delivery_ack(&ack[..DELIVERY_ACK_SIZE - 1]).is_err());
    }

    #[test]
    fn delivery_ack_unknown_mode_is_an_error_not_a_fallback() {
        let mut ack = Vec::new();
        append_delivery_ack(&mut ack, DeliveryMode::Datagrams, 0);
        ack[2] = 3;
        assert_eq!(
            parse_delivery_ack(&ack),
            Err(WireError::UnknownDeliveryMode(3))
        );
    }

    #[test]
    fn telemetry_hello_rejects_reserved_bits_and_wrong_lengths() {
        let token = [0u8; TELEMETRY_SESSION_TOKEN_SIZE];
        let key = [0u8; TELEMETRY_BROADCAST_KEY_SIZE];
        let hello = TelemetryHello {
            enabled: true,
            report_interval_ms: 2000,
            token: &token,
            broadcast_key: &key,
        };
        let mut msg = Vec::new();
        append_telemetry_hello(&mut msg, &hello).unwrap();

        let mut reserved = msg.clone();
        reserved[2] |= 0x80;
        assert_eq!(
            parse_telemetry_hello(&reserved),
            Err(WireError::BadTelemetryHello)
        );

        assert!(parse_telemetry_hello(&msg[..TELEMETRY_HELLO_SIZE - 1]).is_err());

        assert_eq!(
            append_telemetry_hello(
                &mut Vec::new(),
                &TelemetryHello {
                    token: &token[..23],
                    ..hello
                }
            ),
            Err(WireError::BadTelemetryHello)
        );
    }

    #[test]
    fn audio_frame_rejects_empty_and_oversize_payloads() {
        let h = AudioFrameHeader {
            seq: 1,
            timestamp_us: 1,
        };
        assert_eq!(
            append_audio_frame(&mut Vec::new(), &h, b""),
            Err(WireError::BadAudioPayload { len: 0 })
        );
        assert!(append_audio_frame(&mut Vec::new(), &h, &vec![0; MAX_AUDIO_PAYLOAD + 1]).is_err());

        // A header-only datagram parses as "empty payload" and is refused.
        let mut d = Vec::new();
        append_audio_frame(&mut d, &h, b"x").unwrap();
        assert_eq!(
            parse_audio_frame(&d[..AUDIO_FRAME_HEADER_SIZE]),
            Err(WireError::BadAudioPayload { len: 0 })
        );
    }

    #[test]
    fn audio_config_rejects_zero_rate_or_channels() {
        let good = AudioConfig {
            codec: "opus",
            sample_rate: 48000,
            channels: 2,
            description: b"",
        };
        let mut d = Vec::new();
        append_audio_config(&mut d, &good).unwrap();
        assert_eq!(parse_audio_config(&d).unwrap(), good);

        assert_eq!(
            append_audio_config(
                &mut Vec::new(),
                &AudioConfig {
                    sample_rate: 0,
                    ..good
                }
            ),
            Err(WireError::BadAudioConfig)
        );
        assert_eq!(
            append_audio_config(
                &mut Vec::new(),
                &AudioConfig {
                    channels: 0,
                    ..good
                }
            ),
            Err(WireError::BadAudioConfig)
        );

        // Zero rate on the wire is rejected at parse too.
        let mut bad = d.clone();
        bad[4 + 4..4 + 4 + 4].copy_from_slice(&0u32.to_be_bytes());
        assert_eq!(parse_audio_config(&bad), Err(WireError::BadAudioConfig));
    }

    #[test]
    fn resume_token_declared_length_must_cover_message() {
        let mut msg = Vec::new();
        append_resume_token(&mut msg, &[0xAA; 16]).unwrap();
        assert_eq!(parse_resume_token(&msg).unwrap(), &[0xAA; 16]);

        let mut trailing = msg.clone();
        trailing.push(0);
        assert_eq!(
            parse_resume_token(&trailing),
            Err(WireError::BadResumeToken)
        );
        assert!(append_resume_token(&mut Vec::new(), &[]).is_err());
    }

    #[test]
    fn carrier_record_distinguishes_incomplete_from_corrupt() {
        let mut rec = Vec::new();
        append_carrier_record(&mut rec, b"datagram").unwrap();

        // Incomplete: read more from the stream.
        assert!(matches!(
            parse_carrier_record(&rec[..5]),
            Err(WireError::ShortDatagram { .. })
        ));

        // Zero length: framing corruption, abandon the stream.
        assert!(matches!(
            parse_carrier_record(&[0, 0, 1, 2]),
            Err(WireError::BadCarrierRecord { declared: 0 })
        ));
        // Oversize declared length: same.
        let over = ((MAX_DATAGRAM_SIZE + 1) as u16).to_be_bytes();
        assert!(matches!(
            parse_carrier_record(&[over[0], over[1], 0]),
            Err(WireError::BadCarrierRecord { .. })
        ));

        // Two back-to-back records parse in sequence.
        append_carrier_record(&mut rec, b"second").unwrap();
        let (first, rest) = parse_carrier_record(&rec).unwrap();
        assert_eq!(first, b"datagram");
        let (second, rest) = parse_carrier_record(rest).unwrap();
        assert_eq!(second, b"second");
        assert!(rest.is_empty());
    }

    #[test]
    fn stream_frame_header_enforces_keyframe_ceiling() {
        let h = StreamFrameHeader {
            keyframe: true,
            frame_id: 1,
            timestamp_us: 1,
            config_len: 0,
            payload_len: (MAX_KEYFRAME_BYTES - STREAM_FRAME_HEADER_SIZE) as u32,
        };
        // Exactly at the ceiling is legal…
        assert!(append_stream_frame_header(&mut Vec::new(), &h).is_ok());
        // …one byte over is not, on append and on parse.
        let over = StreamFrameHeader {
            payload_len: h.payload_len + 1,
            ..h
        };
        assert_eq!(
            append_stream_frame_header(&mut Vec::new(), &over),
            Err(WireError::KeyframeTooLarge)
        );
        let mut buf = Vec::new();
        append_stream_frame_header(&mut buf, &h).unwrap();
        buf[20..24].copy_from_slice(&(h.payload_len + 1).to_be_bytes());
        assert_eq!(
            parse_stream_frame_header(&buf),
            Err(WireError::KeyframeTooLarge)
        );
    }

    fn relay_identity(version: &str, name: &str) -> Vec<u8> {
        let mut v = Vec::new();
        append_relay_identity(
            &mut v,
            &RelayIdentity {
                server_version: version,
                name,
            },
        )
        .unwrap();
        v
    }

    #[test]
    fn relay_identity_rejects_nonzero_flags_strictly() {
        // Flags gate interpretation, so they stay strict even though
        // trailing bytes are tolerated (docs/40 §4.4).
        let mut flagged = relay_identity("1.42.0", "gawk home");
        flagged[2] = 0x01;
        assert_eq!(
            parse_relay_identity(&flagged),
            Err(WireError::BadRelayIdentity)
        );
    }

    #[test]
    fn relay_identity_rejects_length_overruns() {
        let good = relay_identity("1.42.0", "gawk home");

        // versionLen overrunning the message.
        let mut overrun = good.clone();
        overrun[3] = (good.len() - 4) as u8 + 1;
        assert_eq!(
            parse_relay_identity(&overrun),
            Err(WireError::BadRelayIdentity)
        );
        // versionLen == 0 and versionLen > max.
        let mut zero = good.clone();
        zero[3] = 0;
        assert_eq!(
            parse_relay_identity(&zero),
            Err(WireError::BadRelayIdentity)
        );
        let mut over_max = good.clone();
        over_max[3] = (MAX_RELAY_IDENTITY_VERSION_LEN + 1) as u8;
        assert_eq!(
            parse_relay_identity(&over_max),
            Err(WireError::BadRelayIdentity)
        );
        // nameLen overrunning the message (in range, declared past the end).
        let mut name_overrun = good.clone();
        name_overrun[4 + 6] = 60;
        assert_eq!(
            parse_relay_identity(&name_overrun),
            Err(WireError::BadRelayIdentity)
        );
        // nameLen above the bound.
        let mut name_too_long = good.clone();
        name_too_long[4 + 6] = (MAX_RELAY_IDENTITY_NAME_LEN + 1) as u8;
        assert_eq!(
            parse_relay_identity(&name_too_long),
            Err(WireError::BadRelayIdentity)
        );
        // Shorter than the 5-byte minimum.
        assert!(matches!(
            parse_relay_identity(&good[..4]),
            Err(WireError::ShortDatagram { .. })
        ));
    }

    #[test]
    fn relay_identity_rejects_bad_strings() {
        // Non-printable version byte on the wire.
        let mut bad_version = relay_identity("1.42.0", "");
        bad_version[4] = 0x00;
        assert_eq!(
            parse_relay_identity(&bad_version),
            Err(WireError::BadRelayIdentity)
        );
        // Invalid UTF-8 in the name on the wire (a lone continuation byte).
        let mut bad_name = relay_identity("1.42.0", "ok");
        let name_start = 4 + 6 + 1;
        bad_name[name_start] = 0xff;
        assert_eq!(
            parse_relay_identity(&bad_name),
            Err(WireError::BadRelayIdentity)
        );
        // Append-side refusals: empty/oversize version, non-printable
        // version, oversize name.
        let oversize_version = "v".repeat(MAX_RELAY_IDENTITY_VERSION_LEN + 1);
        for bad in ["", oversize_version.as_str(), "1.\x01"] {
            assert_eq!(
                append_relay_identity(
                    &mut Vec::new(),
                    &RelayIdentity {
                        server_version: bad,
                        name: "",
                    }
                ),
                Err(WireError::BadRelayIdentity),
                "{bad:?}"
            );
        }
        assert_eq!(
            append_relay_identity(
                &mut Vec::new(),
                &RelayIdentity {
                    server_version: "1",
                    name: &"n".repeat(MAX_RELAY_IDENTITY_NAME_LEN + 1),
                }
            ),
            Err(WireError::BadRelayIdentity)
        );
    }

    #[test]
    fn telemetry_endpoint_rejects_nonzero_flags_and_overruns() {
        let mut good = Vec::new();
        append_telemetry_endpoint(&mut good, "https://x.example/ingest").unwrap();

        let mut flagged = good.clone();
        flagged[2] = 0x80;
        assert_eq!(
            parse_telemetry_endpoint(&flagged),
            Err(WireError::BadTelemetryEndpoint)
        );
        // urlLen 256 > remaining bytes.
        let mut overrun = good.clone();
        overrun[3] = 0x01;
        overrun[4] = 0x00;
        assert_eq!(
            parse_telemetry_endpoint(&overrun),
            Err(WireError::BadTelemetryEndpoint)
        );
        // urlLen == 0 and urlLen > max.
        let mut zero = good.clone();
        zero[3] = 0;
        zero[4] = 0;
        assert_eq!(
            parse_telemetry_endpoint(&zero),
            Err(WireError::BadTelemetryEndpoint)
        );
        let mut over_max = good.clone();
        over_max[3..5]
            .copy_from_slice(&((MAX_TELEMETRY_ENDPOINT_URL_LEN + 1) as u16).to_be_bytes());
        assert_eq!(
            parse_telemetry_endpoint(&over_max),
            Err(WireError::BadTelemetryEndpoint)
        );
        assert!(matches!(
            parse_telemetry_endpoint(&good[..5]),
            Err(WireError::ShortDatagram { .. })
        ));
    }

    #[test]
    fn telemetry_endpoint_refuses_non_https_urls_on_both_sides() {
        // Non-https and unparseable URLs are refused on append and parse —
        // a client must never adopt them (docs/40 §5).
        for bad in [
            "http://x.example/ingest",
            "not a url",
            "",
            "https://",
            "HTTPS://",
        ] {
            assert_eq!(
                append_telemetry_endpoint(&mut Vec::new(), bad),
                Err(WireError::BadTelemetryEndpoint),
                "{bad:?}"
            );
        }
        let oversize = format!(
            "https://x.example/{}",
            "p".repeat(MAX_TELEMETRY_ENDPOINT_URL_LEN)
        );
        assert_eq!(
            append_telemetry_endpoint(&mut Vec::new(), &oversize),
            Err(WireError::BadTelemetryEndpoint)
        );
        // Scheme matching is case-insensitive, like Go's url.Parse.
        assert!(append_telemetry_endpoint(&mut Vec::new(), "HTTPS://x.example/i").is_ok());
        // A bad URL on the wire is a parse error too, not just an append one.
        let url = "https://x.example/i";
        let mut on_wire = vec![VERSION, TYPE_TELEMETRY_ENDPOINT, 0];
        on_wire.extend_from_slice(&(url.len() as u16).to_be_bytes());
        on_wire.extend_from_slice(url.as_bytes());
        on_wire[5] = b'x'; // "xttps://…"
        assert_eq!(
            parse_telemetry_endpoint(&on_wire),
            Err(WireError::BadTelemetryEndpoint)
        );
    }
}
