//! The wtransport adapter: the one place the engine touches a real QUIC
//! stack. Everything above this speaks the [`crate::relay::RelaySession`]
//! seam. The vendored wtransport carries the one-variant patch that exposes
//! the HTTP status of a rejected CONNECT (docs/38 D2 gate 2a —
//! vendor/wtransport/GAWK-PATCH.md); close codes arrive through the
//! CLOSE_WEBTRANSPORT_SESSION capsule as `ApplicationClosed` (gate 2b).

use crate::relay::{
    BoxFuture, CancelSignal, CarrierStream, KeyframeOutcome, KeyframeWriter, PathCounters,
    PublishDialer, RelaySession, SendDatagramError, ServerStream, SessionClose, StartError,
    StartPhase,
};
use crate::room::{RoomConn, RoomDialer};
use std::sync::Arc;
use std::sync::atomic::{AtomicU64, Ordering};
use std::time::Duration;
use wtransport::endpoint::endpoint_side::Client;
use wtransport::error::{ConnectingError, ConnectionError};
use wtransport::quinn::congestion::{BbrConfig, ControllerFactory};
use wtransport::{ClientConfig, Endpoint, VarInt};

/// QUIC keepalive. Browsers advertise ~30 s idle timeouts and the effective
/// timeout is the min of both endpoints — the keepalive is what holds a
/// session open while nothing is being sent (CLAUDE.md relay invariant;
/// mirrors the Go dialer's 10 s).
pub const KEEP_ALIVE_PERIOD: Duration = Duration::from_secs(10);

/// How long one dial attempt may go unanswered before the dial abandons it
/// and starts over on a fresh endpoint. One QUIC connection whose first
/// packets were lost recovers slowly or never: its Initials back off
/// exponentially (sent at 0, 1, 3, 7, 15 s), and when it is the relay's
/// answers that are lost the relay gives up on the handshake and forgets
/// the connection while the client keeps probing it — 40-60 s in
/// "connecting" (R65 IO5's CI flake). A fresh endpoint is a new port and
/// connection ID, which the relay answers at once. A healthy dial takes a
/// couple of round trips, so this only ever cuts short a stuck one.
pub const DIAL_ATTEMPT_TIMEOUT: Duration = Duration::from_secs(5);

/// How long a dial keeps starting over before it reports no answer (status
/// 0): quinn's default idle timeout, which is what bounded a single attempt
/// before attempts were cut short.
pub const DIAL_TIMEOUT: Duration = Duration::from_secs(30);

/// The publish connection's QUIC congestion controller (R73 RU4, docs/75
/// D5): `congestion` in `broadcast.json`, reported as `uplinkCongestion`.
/// A support lever, not a Quality-page setting.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Congestion {
    /// quinn's default, and the transport as it was before R73.
    Cubic,
    /// quinn's BBR (`BbrConfig::default()`), in both delivery modes.
    Bbr,
}

/// The engine's default controller: Cubic until RU4's gates and RU7's
/// hardware pass hold (docs/75 D5, §8 step 3) — the one place that flip
/// happens; a blank `congestion` follows it.
pub const DEFAULT_CONGESTION: Congestion = Congestion::Cubic;

impl Default for Congestion {
    fn default() -> Self {
        DEFAULT_CONGESTION
    }
}

impl Congestion {
    /// The `broadcast.json` value and the telemetry/diagnostics word.
    pub fn as_str(self) -> &'static str {
        match self {
            Congestion::Cubic => "cubic",
            Congestion::Bbr => "bbr",
        }
    }

    /// Parses a stored value; anything else (blank included) is `None`,
    /// which the config layer resolves to [`DEFAULT_CONGESTION`].
    pub fn parse(s: &str) -> Option<Congestion> {
        match s.trim() {
            "cubic" => Some(Congestion::Cubic),
            "bbr" => Some(Congestion::Bbr),
            _ => None,
        }
    }

    /// The controller to install, or `None` to leave quinn's transport
    /// exactly as it is (Cubic): `cubic` must restore today's transport,
    /// so it touches nothing rather than re-stating quinn's default.
    fn controller_factory(self) -> Option<Arc<dyn ControllerFactory + Send + Sync>> {
        match self {
            Congestion::Cubic => None,
            Congestion::Bbr => Some(Arc::new(BbrConfig::default())),
        }
    }
}

/// The transport config a publish dial with `congestion` installs over the
/// builder's, or `None` to keep the builder's untouched. Everything but the
/// controller is what the builder sets: quinn's defaults plus the keepalive.
fn publish_transport(congestion: Congestion) -> Option<wtransport::config::QuicTransportConfig> {
    let factory = congestion.controller_factory()?;
    let mut t = wtransport::config::QuicTransportConfig::default();
    t.keep_alive_interval(Some(KEEP_ALIVE_PERIOD));
    t.congestion_controller_factory(factory);
    Some(t)
}

/// A live publisher session. Owns the endpoint too: the endpoint drives the
/// connection's I/O and must outlive it.
pub struct WtSession {
    _endpoint: Endpoint<Client>,
    conn: wtransport::Connection,
    /// Datagrams `send_datagram` accepted on this connection — every one,
    /// best-effort time sync included, because quinn's `frame_tx.datagram`
    /// counts them all (docs/75 D8).
    datagrams_offered: AtomicU64,
}

/// Dials the relay's publish endpoint. `insecure` skips server certificate
/// verification — dev certs only, the runtime mirror of the Go `-insecure`
/// flag. On a refused CONNECT the returned [`StartError`] carries the HTTP
/// status (401/403/404/409/429); a dial that never got an HTTP answer
/// carries status 0.
pub async fn dial(url: &str, origin: &str, insecure: bool) -> Result<WtSession, StartError> {
    dial_publish(url, origin, insecure, Congestion::default()).await
}

/// [`dial`] with the congestion controller chosen (docs/75 D5): what every
/// publish leg — the first and each reclaim — dials through.
pub async fn dial_publish(
    url: &str,
    origin: &str,
    insecure: bool,
    congestion: Congestion,
) -> Result<WtSession, StartError> {
    let (endpoint, conn) = connect(url, origin, insecure, "publish", congestion).await?;
    Ok(WtSession {
        _endpoint: endpoint,
        conn,
        datagrams_offered: AtomicU64::new(0),
    })
}

/// Dials a subscribe route (`/subscribe/{id}`) for the iOS viewer (R65,
/// docs/67 D13). The engine never subscribes itself; the session is the same
/// [`RelaySession`] seam a publisher gets (datagrams both ways, server uni
/// streams, the close code), and refusals surface exactly as for [`dial`].
pub async fn dial_subscribe(
    url: &str,
    origin: &str,
    insecure: bool,
) -> Result<WtSession, StartError> {
    let (endpoint, conn) = connect(url, origin, insecure, "subscribe", Congestion::Cubic).await?;
    Ok(WtSession {
        _endpoint: endpoint,
        conn,
        datagrams_offered: AtomicU64::new(0),
    })
}

fn connect_err(status: u16, message: String) -> StartError {
    StartError {
        phase: StartPhase::Connect,
        status,
        message,
    }
}

/// The shared CONNECT: attempts of at most [`DIAL_ATTEMPT_TIMEOUT`], each
/// on a fresh endpoint, until one is answered or [`DIAL_TIMEOUT`] passes.
/// Only an unanswered attempt is retried: a refused CONNECT (its status)
/// and every other error return from the attempt that met them.
async fn connect(
    url: &str,
    origin: &str,
    insecure: bool,
    what: &str,
    congestion: Congestion,
) -> Result<(Endpoint<Client>, wtransport::Connection), StartError> {
    let deadline = tokio::time::Instant::now() + DIAL_TIMEOUT;
    loop {
        let left = deadline.saturating_duration_since(tokio::time::Instant::now());
        if left.is_zero() {
            return Err(connect_err(
                0,
                format!(
                    "connection timed out: no answer from the relay in {} s",
                    DIAL_TIMEOUT.as_secs()
                ),
            ));
        }
        let attempt = connect_once(url, origin, insecure, what, congestion);
        if let Ok(done) = tokio::time::timeout(left.min(DIAL_ATTEMPT_TIMEOUT), attempt).await {
            return done;
        }
        // Timed out: the attempt's endpoint dropped with it; start over.
    }
}

/// One attempt: its own endpoint (the endpoint drives the connection's I/O
/// and must outlive it), the Origin header, the keepalive.
async fn connect_once(
    url: &str,
    origin: &str,
    insecure: bool,
    what: &str,
    congestion: Congestion,
) -> Result<(Endpoint<Client>, wtransport::Connection), StartError> {
    let builder = ClientConfig::builder().with_bind_default();
    let builder = if insecure {
        builder.with_no_cert_validation()
    } else {
        #[cfg(target_os = "ios")]
        {
            builder.with_custom_tls(ios_tls().map_err(|e| connect_err(0, e))?)
        }
        #[cfg(not(target_os = "ios"))]
        {
            builder.with_native_certs()
        }
    };
    let mut config = builder.keep_alive_interval(Some(KEEP_ALIVE_PERIOD)).build();
    if let Some(transport) = publish_transport(congestion) {
        config
            .quic_config_mut()
            .transport_config(Arc::new(transport));
    }

    let endpoint = Endpoint::client(config)
        .map_err(|e| connect_err(0, format!("could not create a QUIC endpoint: {e}")))?;

    // The Origin header is load-bearing: wtransport sends none by default,
    // and a relay with -allowed-origins matches an absent Origin against
    // nothing (the Linux engine's field bug, docs/38 D19).
    let options = wtransport::endpoint::ConnectOptions::builder(url)
        .add_header("origin", origin)
        .build();

    match endpoint.connect(options).await {
        Ok(conn) => Ok((endpoint, conn)),
        Err(ConnectingError::SessionRejected(status)) => Err(connect_err(
            status,
            format!("the relay refused the {what} request (status {status})"),
        )),
        Err(e) => Err(connect_err(0, e.to_string())),
    }
}

/// wtransport's default client TLS (TLS 1.3, the WebTransport ALPN) with
/// iOS's own trust: Security.framework through `rustls-platform-verifier`.
/// `with_native_certs` can't serve iOS: `rustls-native-certs` has no iOS
/// backend and reads the Unix certificate directories an iPhone doesn't
/// have, so every certificate failed with `UnknownIssuer` (R65, docs/67
/// §12). The desktops keep `with_native_certs`.
#[cfg(target_os = "ios")]
fn ios_tls() -> Result<wtransport::tls::rustls::ClientConfig, String> {
    use std::sync::Arc;
    use wtransport::tls::rustls;

    let provider = Arc::new(rustls::crypto::ring::default_provider());
    let verifier = rustls_platform_verifier::Verifier::new(provider)
        .map_err(|e| format!("could not set up certificate verification: {e}"))?;
    Ok(wtransport::tls::client::build_default_tls_config(
        Arc::new(rustls::RootCertStore::empty()),
        Some(Arc::new(verifier)),
    ))
}

/// A live room control session (R42): the connection plus its ONE
/// bidirectional stream, opened by this client right after the upgrade.
/// The halves sit behind async mutexes so the [`RoomConn`] seam can be
/// `&self` — reads and writes are independent, and the room loop reads on
/// a detached task while it writes commands.
pub struct WtRoomConn {
    _endpoint: Endpoint<Client>,
    conn: wtransport::Connection,
    send: tokio::sync::Mutex<wtransport::SendStream>,
    recv: tokio::sync::Mutex<wtransport::RecvStream>,
}

/// Dials a room route (`/room/new` or `/room/{code}`) and opens the control
/// stream. Statuses surface exactly as for [`dial`].
pub async fn dial_room(url: &str, origin: &str, insecure: bool) -> Result<WtRoomConn, StartError> {
    let (endpoint, conn) = connect(url, origin, insecure, "room", Congestion::Cubic).await?;
    let stream_err = |e: String| StartError {
        phase: StartPhase::Connect,
        status: 0,
        message: format!("could not open the room control stream: {e}"),
    };
    let (send, recv) = conn
        .open_bi()
        .await
        .map_err(|e| stream_err(e.to_string()))?
        .await
        .map_err(|e| stream_err(e.to_string()))?;
    Ok(WtRoomConn {
        _endpoint: endpoint,
        conn,
        send: tokio::sync::Mutex::new(send),
        recv: tokio::sync::Mutex::new(recv),
    })
}

impl RoomConn for WtRoomConn {
    fn write(&self, record: &[u8]) -> BoxFuture<'_, Result<(), String>> {
        let record = record.to_vec();
        Box::pin(async move {
            self.send
                .lock()
                .await
                .write_all(&record)
                .await
                .map_err(|e| e.to_string())
        })
    }

    fn read(&self, n: usize) -> BoxFuture<'_, Result<Vec<u8>, String>> {
        Box::pin(async move {
            let mut buf = vec![0u8; n];
            self.recv
                .lock()
                .await
                .read_exact(&mut buf)
                .await
                .map_err(|e| e.to_string())?;
            Ok(buf)
        })
    }

    fn closed(&self) -> BoxFuture<'_, SessionClose> {
        Box::pin(async { close_cause(&self.conn).await })
    }
}

/// The transport-backed [`RoomDialer`]: the seam's production side.
pub struct WtRoomDialer {
    pub origin: String,
    pub insecure: bool,
}

impl RoomDialer for WtRoomDialer {
    fn dial(&self, url: &str) -> BoxFuture<'_, Result<Arc<dyn RoomConn>, StartError>> {
        let url = url.to_owned();
        Box::pin(async move {
            let conn = dial_room(&url, &self.origin, self.insecure).await?;
            Ok(Arc::new(conn) as Arc<dyn RoomConn>)
        })
    }
}

/// The transport-backed [`PublishDialer`]: what every production reclaim
/// dials through.
pub struct WtPublishDialer {
    pub origin: String,
    pub insecure: bool,
    /// Every reclaim runs the controller the first leg did.
    pub congestion: Congestion,
}

impl PublishDialer for WtPublishDialer {
    fn dial(&self, url: &str) -> BoxFuture<'_, Result<Arc<dyn RelaySession>, StartError>> {
        let url = url.to_owned();
        Box::pin(async move {
            let session = dial_publish(&url, &self.origin, self.insecure, self.congestion).await?;
            Ok(Arc::new(session) as Arc<dyn RelaySession>)
        })
    }
}

async fn close_cause(conn: &wtransport::Connection) -> SessionClose {
    match conn.closed().await {
        ConnectionError::ApplicationClosed(close) => {
            // The capsule's error code is the relay's application close
            // code (4000/4002/4004…) verbatim.
            SessionClose::Code(close.code().into_inner() as u32)
        }
        other => SessionClose::Abrupt(other.to_string()),
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
        })?;
        // quinn's Ok means "queued", not "sent": a full send buffer
        // silently evicts its oldest datagram to make room.
        self.datagrams_offered.fetch_add(1, Ordering::Relaxed);
        Ok(())
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
        Box::pin(async { close_cause(&self.conn).await })
    }

    fn close(&self) {
        self.conn.close(VarInt::from_u32(0), b"");
    }

    fn path_counters(&self) -> Option<PathCounters> {
        let stats = self.conn.quic_connection().stats();
        Some(PathCounters {
            sent: stats.path.sent_packets,
            lost: stats.path.lost_packets,
            datagrams_offered: self.datagrams_offered.load(Ordering::Relaxed),
            datagrams_sent: stats.frame_tx.datagram,
        })
    }

    fn remote_address(&self) -> Option<std::net::SocketAddr> {
        Some(self.conn.remote_address())
    }

    fn open_uplink_carrier(&self) -> BoxFuture<'_, Result<Box<dyn CarrierStream>, String>> {
        Box::pin(async {
            let stream = self
                .conn
                .open_uni()
                .await
                .map_err(|e| e.to_string())?
                .await
                .map_err(|e| e.to_string())?;
            Ok(Box::new(WtCarrier { stream }) as Box<dyn CarrierStream>)
        })
    }
}

/// An uplink carrier on a wtransport uni stream (docs/75 D1).
struct WtCarrier {
    stream: wtransport::SendStream,
}

impl CarrierStream for WtCarrier {
    fn set_priority(&mut self, priority: i32) {
        self.stream.set_priority(priority);
    }

    fn write<'a>(&'a mut self, buf: &'a [u8]) -> BoxFuture<'a, Result<(), String>> {
        Box::pin(async move { self.stream.write_all(buf).await.map_err(|e| e.to_string()) })
    }

    fn finish(&mut self) -> BoxFuture<'_, Result<(), String>> {
        // wtransport's finish() is quinn's finish() then stopped(): it
        // resolves once every byte is acknowledged (docs/75 D3).
        Box::pin(async move { self.stream.finish().await.map_err(|e| e.to_string()) })
    }

    fn reset(&mut self, code: u32) {
        let _ = self.stream.reset(VarInt::from_u32(code));
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

    fn set_priority(&self, priority: i32) {
        self.stream.set_priority(priority);
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

#[cfg(test)]
mod tests {
    use super::*;
    use wtransport::quinn::congestion::{Bbr, Cubic};

    /// What the builder produced before R73, and still produces for Cubic:
    /// quinn's defaults plus the keepalive.
    fn todays_transport() -> wtransport::config::QuicTransportConfig {
        let mut t = wtransport::config::QuicTransportConfig::default();
        t.keep_alive_interval(Some(KEEP_ALIVE_PERIOD));
        t
    }

    // RU4: `cubic` restores today's transport exactly — the dial installs
    // nothing over the builder's config, so quinn's own default (Cubic)
    // runs, as it did before the setting existed.
    #[test]
    fn cubic_leaves_todays_transport_untouched() {
        assert!(publish_transport(Congestion::Cubic).is_none());
        assert!(Congestion::Cubic.controller_factory().is_none());
        // quinn's default factory is Cubic's.
        let built = Arc::new(wtransport::quinn::congestion::CubicConfig::default())
            .build(std::time::Instant::now(), 1200);
        assert!(built.into_any().downcast::<Cubic>().is_ok());
    }

    // RU4: `bbr` changes the controller and nothing else.
    #[test]
    fn bbr_installs_quinns_bbr_and_changes_nothing_else() {
        let factory = Congestion::Bbr.controller_factory().expect("a factory");
        let built = factory.build(std::time::Instant::now(), 1200);
        assert!(
            built.into_any().downcast::<Bbr>().is_ok(),
            "BbrConfig::default()"
        );
        // TransportConfig's Debug lists every field but the controller.
        let bbr = publish_transport(Congestion::Bbr).unwrap();
        assert_eq!(format!("{bbr:?}"), format!("{:?}", todays_transport()));
    }

    #[test]
    fn congestion_words_round_trip_and_blank_is_the_default() {
        for c in [Congestion::Cubic, Congestion::Bbr] {
            assert_eq!(Congestion::parse(c.as_str()), Some(c));
        }
        assert_eq!(Congestion::parse(""), None);
        assert_eq!(Congestion::parse("reno"), None);
        assert_eq!(Congestion::default(), DEFAULT_CONGESTION);
        // docs/75 D5, §8 step 2: today's controller stays the default until
        // RU4's gates and RU7 pass.
        assert_eq!(DEFAULT_CONGESTION, Congestion::Cubic);
    }
}
