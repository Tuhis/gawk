//! The wtransport adapter: the one place the engine touches a real QUIC
//! stack. Everything above this speaks the [`crate::relay::RelaySession`]
//! seam. The vendored wtransport carries the one-variant patch that exposes
//! the HTTP status of a rejected CONNECT (docs/38 D2 gate 2a —
//! vendor/wtransport/GAWK-PATCH.md); close codes arrive through the
//! CLOSE_WEBTRANSPORT_SESSION capsule as `ApplicationClosed` (gate 2b).

use crate::relay::{
    BoxFuture, CancelSignal, KeyframeOutcome, KeyframeWriter, RelaySession, SendDatagramError,
    ServerStream, SessionClose, StartError, StartPhase,
};
use std::time::Duration;
use wtransport::endpoint::endpoint_side::Client;
use wtransport::error::{ConnectingError, ConnectionError};
use wtransport::{ClientConfig, Endpoint, VarInt};

/// QUIC keepalive. Browsers advertise ~30 s idle timeouts and the effective
/// timeout is the min of both endpoints — the keepalive is what holds a
/// session open while nothing is being sent (CLAUDE.md relay invariant;
/// mirrors the Go dialer's 10 s).
pub const KEEP_ALIVE_PERIOD: Duration = Duration::from_secs(10);

/// A live publisher session. Owns the endpoint too: the endpoint drives the
/// connection's I/O and must outlive it.
pub struct WtSession {
    _endpoint: Endpoint<Client>,
    conn: wtransport::Connection,
}

/// Dials the relay's publish endpoint. `insecure` skips server certificate
/// verification — dev certs only, the runtime mirror of the Go `-insecure`
/// flag. On a refused CONNECT the returned [`StartError`] carries the HTTP
/// status (401/403/404/409/429); a dial that never got an HTTP answer
/// carries status 0.
pub async fn dial(url: &str, origin: &str, insecure: bool) -> Result<WtSession, StartError> {
    let connect_err = |status: u16, message: String| StartError {
        phase: StartPhase::Connect,
        status,
        message,
    };

    let builder = ClientConfig::builder().with_bind_default();
    let builder = if insecure {
        builder.with_no_cert_validation()
    } else {
        builder.with_native_certs()
    };
    let config = builder.keep_alive_interval(Some(KEEP_ALIVE_PERIOD)).build();

    let endpoint = Endpoint::client(config)
        .map_err(|e| connect_err(0, format!("could not create a QUIC endpoint: {e}")))?;

    // The Origin header is load-bearing: wtransport sends none by default,
    // and a relay with -allowed-origins matches an absent Origin against
    // nothing (the Linux engine's field bug, docs/38 D19).
    let options = wtransport::endpoint::ConnectOptions::builder(url)
        .add_header("origin", origin)
        .build();

    match endpoint.connect(options).await {
        Ok(conn) => Ok(WtSession {
            _endpoint: endpoint,
            conn,
        }),
        Err(ConnectingError::SessionRejected(status)) => Err(connect_err(
            status,
            format!("the relay refused the publish request (status {status})"),
        )),
        Err(e) => Err(connect_err(0, e.to_string())),
    }
}

impl RelaySession for WtSession {
    fn send_datagram(&self, dgram: &[u8]) -> Result<(), SendDatagramError> {
        use wtransport::error::SendDatagramError as E;
        self.conn.send_datagram(dgram).map_err(|e| match e {
            E::TooLarge => SendDatagramError::TooLarge {
                max_datagram_size: self.conn.max_datagram_size(),
            },
            E::NotConnected | E::UnsupportedByPeer => SendDatagramError::Failed(e.to_string()),
        })
    }

    fn open_keyframe_stream(&self) -> BoxFuture<'_, Result<Box<dyn KeyframeWriter>, String>> {
        Box::pin(async {
            let stream = self
                .conn
                .open_uni()
                .await
                .map_err(|e| e.to_string())?
                .await
                .map_err(|e| e.to_string())?;
            Ok(Box::new(WtKeyframeWriter { stream }) as Box<dyn KeyframeWriter>)
        })
    }

    fn accept_uni(&self) -> BoxFuture<'_, Result<Box<dyn ServerStream>, String>> {
        Box::pin(async {
            let stream = self.conn.accept_uni().await.map_err(|e| e.to_string())?;
            Ok(Box::new(WtServerStream { stream }) as Box<dyn ServerStream>)
        })
    }

    fn receive_datagram(&self) -> BoxFuture<'_, Result<Vec<u8>, String>> {
        Box::pin(async {
            let dgram = self
                .conn
                .receive_datagram()
                .await
                .map_err(|e| e.to_string())?;
            Ok(dgram.to_vec())
        })
    }

    fn closed(&self) -> BoxFuture<'_, SessionClose> {
        Box::pin(async {
            match self.conn.closed().await {
                ConnectionError::ApplicationClosed(close) => {
                    // The capsule's error code is the relay's application
                    // close code (4000/4002/4004…) verbatim.
                    SessionClose::Code(close.code().into_inner() as u32)
                }
                other => SessionClose::Abrupt(other.to_string()),
            }
        })
    }
}

struct WtKeyframeWriter {
    stream: wtransport::SendStream,
}

impl KeyframeWriter for WtKeyframeWriter {
    fn write(
        mut self: Box<Self>,
        msg: Vec<u8>,
        mut cancel: CancelSignal,
    ) -> BoxFuture<'static, KeyframeOutcome> {
        Box::pin(async move {
            enum Sel {
                Write(Result<(), wtransport::error::StreamWriteError>),
                Cancel(u32),
            }
            let outcome = {
                let stream = &mut self.stream;
                let write = async {
                    stream.write_all(&msg).await?;
                    stream.finish().await
                };
                tokio::pin!(write);
                tokio::select! {
                    r = &mut write => Sel::Write(r),
                    c = &mut cancel => Sel::Cancel(c.unwrap_or(crate::sender::KEYFRAME_SUPERSEDED_CODE)),
                }
            };
            match outcome {
                Sel::Write(Ok(())) => KeyframeOutcome::Sent,
                Sel::Write(Err(e)) => KeyframeOutcome::Failed(e.to_string()),
                Sel::Cancel(code) => {
                    let _ = self.stream.reset(VarInt::from_u32(code));
                    KeyframeOutcome::Cancelled
                }
            }
        })
    }

    fn abort(mut self: Box<Self>, code: u32) {
        let _ = self.stream.reset(VarInt::from_u32(code));
    }
}

struct WtServerStream {
    stream: wtransport::RecvStream,
}

impl ServerStream for WtServerStream {
    fn read_to_end(&mut self, limit: usize) -> BoxFuture<'_, Result<Vec<u8>, String>> {
        Box::pin(async move {
            let mut out = Vec::new();
            let mut buf = [0u8; 512];
            loop {
                match self.stream.read(&mut buf).await {
                    Ok(Some(n)) => {
                        if out.len() + n > limit {
                            return Err(format!("server stream exceeds the {limit}-byte bound"));
                        }
                        out.extend_from_slice(&buf[..n]);
                    }
                    Ok(None) => return Ok(out),
                    Err(e) => return Err(e.to_string()),
                }
            }
        })
    }
}
