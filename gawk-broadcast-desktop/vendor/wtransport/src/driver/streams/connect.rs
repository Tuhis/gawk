use tracing::debug;
use wtransport_proto::bytes::IoReadError;
use wtransport_proto::error::ErrorCode;
use wtransport_proto::frame::FrameKind;
use wtransport_proto::varint::VarInt;

use super::session::StreamSession;
use std::borrow::Cow;
use wtransport_proto::frame::Frame;
use super::ProtoReadError;
use crate::driver::DriverError;
use crate::error::ApplicationClose;
use std::future::pending;

pub struct ConnectStream {
    stream: Option<StreamSession>,
}

impl ConnectStream {
    pub fn empty() -> Self {
        Self { stream: None }
    }

    pub fn is_empty(&self) -> bool {
        self.stream.is_none()
    }

    pub fn set_stream(&mut self, stream: StreamSession) {
        self.stream = Some(stream);
    }

    // GAWK PATCH (see GAWK-PATCH.md): this reader is rewritten. Capsules are
    // a BYTE STREAM carried in the CONNECT stream's DATA frames (RFC 9297
    // §3.1), so a capsule can and does span DATA-frame boundaries —
    // webtransport-go (the gawk relay) writes the capsule header and value
    // with separate stream writes, producing two DATA frames. Stock
    // wtransport parsed each DATA frame as exactly one complete capsule and
    // silently skipped anything else, which lost every session close code
    // this relay sends (4000/4002/4004 — docs/38 D2 gate 2b). This version
    // accumulates DATA payload bytes and parses capsules from the
    // concatenation; unknown frames on the stream are skipped per RFC 9114
    // §9 instead of being treated as one-frame capsules.
    pub async fn run(&mut self) -> DriverError {
        const CLOSE_WEBTRANSPORT_SESSION: u64 = 0x2843;
        const MAX_REASON_LEN: usize = 1024;
        // Ample for any legal close capsule; a peer streaming an oversized
        // or garbage capsule is violating the protocol.
        const MAX_BUFFERED: usize = 16 * 1024;

        let stream = match self.stream.as_mut() {
            Some(stream) => stream,
            None => pending().await,
        };

        let mut buffer: Vec<u8> = Vec::new();
        loop {
            // Parse every complete capsule available in the buffer.
            while let Some((capsule_type, value_len, header_len)) = peek_capsule(&buffer) {
                if buffer.len() < header_len + value_len {
                    break; // incomplete: read more DATA
                }
                let value = &buffer[header_len..header_len + value_len];
                if capsule_type == CLOSE_WEBTRANSPORT_SESSION {
                    if value.len() < 4 || value.len() > 4 + MAX_REASON_LEN {
                        return DriverError::Proto(ErrorCode::Datagram);
                    }
                    let error_code = u32::from_be_bytes(
                        value[..4].try_into().expect("4B to u32 should succeed"),
                    );
                    let reason = match std::str::from_utf8(&value[4..]) {
                        Ok(reason) => reason.as_bytes().to_vec().into_boxed_slice(),
                        Err(_) => return DriverError::Proto(ErrorCode::Datagram),
                    };
                    // Reset right away to avoid receiving additional data
                    // which would require resetting with ErrorCode::Message.
                    self.stream
                        .take()
                        .unwrap()
                        .reset(ErrorCode::NoError.to_code());
                    return DriverError::ApplicationClosed(ApplicationClose::new(
                        VarInt::from_u32(error_code),
                        reason,
                    ));
                }
                debug!("Skipping unknown capsule of type {capsule_type:#x} ({value_len}B)");
                buffer.drain(..header_len + value_len);
            }
            if buffer.len() > MAX_BUFFERED {
                return DriverError::Proto(ErrorCode::ExcessiveLoad);
            }

            match stream.read_frame().await {
                Ok(frame) => match frame.kind() {
                    FrameKind::Data => buffer.extend_from_slice(frame.payload()),
                    // Unknown/grease frames on the CONNECT stream are
                    // ignored (RFC 9114 §9).
                    other => debug!("Skipping frame {other:?} on the CONNECT stream"),
                },
                Err(ProtoReadError::H3(error_code)) => return DriverError::Proto(error_code),
                Err(ProtoReadError::IO(io_error)) => {
                    return match io_error {
                        // Cleanly terminating a CONNECT stream without a
                        // CLOSE_WEBTRANSPORT_SESSION capsule SHALL be
                        // semantically equivalent to one with an error code
                        // of 0 and an empty error string.
                        IoReadError::ImmediateFin => DriverError::ApplicationClosed(
                            ApplicationClose::new(VarInt::from_u32(0), Box::new([])),
                        ),
                        IoReadError::UnexpectedFin | IoReadError::Reset => {
                            DriverError::Proto(ErrorCode::ClosedCriticalStream)
                        }
                        IoReadError::NotConnected => DriverError::NotConnected,
                    };
                }
            }
        }
    }
}

impl ConnectStream {
    // GAWK PATCH (see GAWK-PATCH.md): the sending half of a session close.
    // Stock wtransport can only close the QUIC connection, whose application
    // code a webtransport-go peer never reads as the SESSION's close code
    // (its CONNECT-stream reader just fails). The gawk relay reads a
    // publisher's deliberate stop (4014) from the session's close error, so
    // the code must travel the way a browser sends it: a
    // CLOSE_WEBTRANSPORT_SESSION capsule in a DATA frame on the CONNECT
    // stream, then FIN (RFC 9297 §3.2, draft-ietf-webtrans-http3 §6). The
    // FIN's wait resolves once the peer's QUIC stack has acknowledged every
    // byte (or stopped the stream, or the connection died). That is not yet
    // enough: quic-go fails a stream read with the connection's error even
    // over buffered bytes, so a CONNECTION_CLOSE that lands before the
    // relay's reader ran still loses the capsule. The read loop after it
    // waits for the peer's own answer — webtransport-go resets the CONNECT
    // stream once it has parsed the close — and only then may the worker
    // close the connection. A peer that never answers is the caller's to
    // bound (`Connection::close_session`).
    pub async fn close_session(&mut self, error_code: u32, reason: Box<[u8]>) -> DriverError {
        const CLOSE_WEBTRANSPORT_SESSION: u64 = 0x2843;
        // The value's own limit (4-byte code + at most 1024 reason bytes).
        let reason_len = reason.len().min(1024);
        let mut capsule = Vec::with_capacity(4 + 4 + 4 + reason_len);
        put_varint(&mut capsule, CLOSE_WEBTRANSPORT_SESSION);
        put_varint(&mut capsule, (4 + reason_len) as u64);
        capsule.extend_from_slice(&error_code.to_be_bytes());
        capsule.extend_from_slice(&reason[..reason_len]);
        if let Some(mut stream) = self.stream.take() {
            if stream
                .write_frame(Frame::new_data(Cow::Owned(capsule)))
                .await
                .is_ok()
            {
                stream.finish().await;
                // Until the peer resets or ends its half, or the connection
                // dies: anything it still sends is of no interest.
                while stream.read_frame().await.is_ok() {}
            }
        }
        DriverError::ApplicationClosed(ApplicationClose::new(
            VarInt::from_u32(error_code),
            reason,
        ))
    }
}

/// Appends one QUIC variable-length integer (RFC 9000 §16), shortest form.
fn put_varint(buf: &mut Vec<u8>, v: u64) {
    if v < 1 << 6 {
        buf.push(v as u8);
    } else if v < 1 << 14 {
        buf.extend_from_slice(&((v as u16) | 0x4000).to_be_bytes());
    } else if v < 1 << 30 {
        buf.extend_from_slice(&((v as u32) | 0x8000_0000).to_be_bytes());
    } else {
        buf.extend_from_slice(&(v | 0xc000_0000_0000_0000).to_be_bytes());
    }
}

/// Peeks one capsule header (QUIC varint type, QUIC varint length) at the
/// start of `buf`. Returns `(type, value_len, header_len)` or `None` when the
/// header itself is still incomplete.
fn peek_capsule(buf: &[u8]) -> Option<(u64, usize, usize)> {
    let (capsule_type, n1) = peek_varint(buf)?;
    let (value_len, n2) = peek_varint(&buf[n1..])?;
    Some((capsule_type, value_len as usize, n1 + n2))
}

/// Decodes one QUIC variable-length integer (RFC 9000 §16).
fn peek_varint(buf: &[u8]) -> Option<(u64, usize)> {
    let first = *buf.first()?;
    let len = 1usize << (first >> 6);
    if buf.len() < len {
        return None;
    }
    let mut value = u64::from(first & 0x3f);
    for b in &buf[1..len] {
        value = (value << 8) | u64::from(*b);
    }
    Some((value, len))
}
