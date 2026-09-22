//! The relay seam (docs/38 §5): a narrow trait, not an abstraction for its
//! own sake — the send policy is defined by what happens when sends FAIL,
//! and only a seam lets tests script those failures. Mirrors the Go engine's
//! `RelaySession` interface (gawk-broadcast/internal/engine/relay.go).

use std::future::Future;
use std::pin::Pin;

pub type BoxFuture<'a, T> = Pin<Box<dyn Future<Output = T> + Send + 'a>>;

/// Why a datagram send failed. `TooLarge` is the one the send policy reacts
/// to structurally (shrink the chunk budget once and re-chunk); everything
/// else drops the frame remainder.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum SendDatagramError {
    /// The datagram exceeds the path's max size. The transport's currently
    /// reported maximum rides along so the budget can shrink to fit.
    TooLarge { max_datagram_size: Option<usize> },
    /// The session is dead or the send failed for any other reason.
    Failed(String),
}

/// What happened to a keyframe stream write.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum KeyframeOutcome {
    /// Written and finished cleanly.
    Sent,
    /// Reset because a newer keyframe superseded it (code 1) or teardown
    /// aborted it.
    Cancelled,
    /// The write or finish failed.
    Failed(String),
}

/// Fires to abandon an in-flight keyframe write; the payload is the stream
/// error code (`KEYFRAME_SUPERSEDED_CODE` in practice).
pub type CancelSignal = tokio::sync::oneshot::Receiver<u32>;

/// One reliable uni stream carrying one keyframe. The whole write lifecycle
/// is a single consuming call so the select-on-cancel mechanics live in the
/// adapter (or the test fake), while the policy — one in flight, newest wins
/// — stays in the sender.
pub trait KeyframeWriter: Send {
    /// Writes `msg`, then finishes the stream — unless `cancel` fires first,
    /// in which case the stream is reset with the delivered code.
    fn write(
        self: Box<Self>,
        msg: Vec<u8>,
        cancel: CancelSignal,
    ) -> BoxFuture<'static, KeyframeOutcome>;
    /// Resets the stream without writing (used when the sender is already
    /// closed by teardown when the stream comes back from open).
    fn abort(self: Box<Self>, code: u32);
}

/// A server-initiated uni stream (announce, resume token, capabilities,
/// telemetry hello). The caller bounds the read; the deadline is the
/// caller's (`tokio::time::timeout`).
pub trait ServerStream: Send {
    /// Reads to end of stream, erroring if it exceeds `limit` bytes.
    fn read_to_end(&mut self, limit: usize) -> BoxFuture<'_, Result<Vec<u8>, String>>;
}

/// Why a session ended, as far as the transport can say.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum SessionClose {
    /// The relay closed the session with a WebTransport application close
    /// code (4000/4002/4004…). The resume supervisor's terminal/resumable
    /// split keys off this.
    Code(u32),
    /// Ordinary abrupt death — idle timeout, stateless reset, the connection
    /// simply going away. Precisely the case auto-resume exists for.
    Abrupt(String),
}

/// The narrow session seam. Datagram sends are synchronous (the transport
/// queues); stream operations are async.
pub trait RelaySession: Send + Sync + 'static {
    fn send_datagram(&self, dgram: &[u8]) -> Result<(), SendDatagramError>;
    /// Opens a reliable uni stream for one keyframe.
    fn open_keyframe_stream(&self) -> BoxFuture<'_, Result<Box<dyn KeyframeWriter>, String>>;
    /// Accepts the next server-initiated uni stream.
    fn accept_uni(&self) -> BoxFuture<'_, Result<Box<dyn ServerStream>, String>>;
    /// Receives the next datagram.
    fn receive_datagram(&self) -> BoxFuture<'_, Result<Vec<u8>, String>>;
    /// Resolves when the session dies, with the best available cause.
    fn closed(&self) -> BoxFuture<'_, SessionClose>;
    /// Closes the session cleanly (application close code 0), the way the
    /// Go engine's `Stop` does with `CloseWithError(0)`: the relay learns
    /// the publisher left NOW — viewers see "streamer away", a room shows
    /// the attachment as away — instead of at the QUIC idle timeout, which
    /// is what a shell that still holds the session would otherwise get.
    /// A no-op for a session with nothing to close.
    fn close(&self) {}
}

/// Which phase a start failure happened in. The shells' "Start a new
/// broadcast instead" offer is gated on `Connect` — a capture-phase failure
/// deliberately never offers a mint (R1's orphaned-viewers bug).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum StartPhase {
    Connect,
    Capture,
}

/// A failed start, carrying the HTTP status of a refused CONNECT when there
/// was one (401 wrong secret / 403 token refused / 404 unknown / 409 held /
/// 429 relay full / 451 banned by the operator, R39 docs/42 D15; 0 = the dial
/// never got an HTTP answer).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct StartError {
    pub phase: StartPhase,
    pub status: u16,
    pub message: String,
}

impl std::fmt::Display for StartError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self.phase {
            StartPhase::Connect => write!(
                f,
                "connect failed (status {}): {}",
                self.status, self.message
            ),
            StartPhase::Capture => write!(f, "capture failed: {}", self.message),
        }
    }
}

impl std::error::Error for StartError {}

/// Builds the publish URL the way both Linux shells do
/// (gawk-broadcast/internal/engine/relay.go `PublishURL`): `/publish` to
/// mint, `/publish/{id}` to reclaim; the secret and the hex-encoded resume
/// token ride as query parameters because the relay only reads query params
/// (the browser WebTransport API cannot set headers, and the two
/// broadcasters must authenticate identically). The token is only sent when
/// BOTH an ID and a token are present.
pub fn publish_url(
    relay_url: &str,
    broadcast_id: &str,
    secret: &str,
    resume_token_hex: &str,
) -> Result<String, String> {
    let mut url = url::Url::parse(relay_url).map_err(|e| format!("bad relay URL: {e}"))?;
    if !matches!(url.scheme(), "https") {
        return Err(format!("relay URL must be https, got {}", url.scheme()));
    }
    if broadcast_id.is_empty() {
        url.set_path("/publish");
    } else {
        url.set_path(&format!("/publish/{broadcast_id}"));
    }
    {
        let mut q = url.query_pairs_mut();
        if !secret.is_empty() {
            q.append_pair("secret", secret);
        }
        if !broadcast_id.is_empty() && !resume_token_hex.is_empty() {
            q.append_pair("resume", resume_token_hex);
        }
    }
    if url.query() == Some("") {
        url.set_query(None);
    }
    Ok(url.into())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn mint_reclaim_and_credential_rules() {
        // Mint: no ID, no resume param even if a stale token exists.
        assert_eq!(
            publish_url("https://api.gawk.ioio.fi:4433", "", "s3cret", "deadbeef").unwrap(),
            "https://api.gawk.ioio.fi:4433/publish?secret=s3cret"
        );
        // Reclaim: ID and token both present.
        assert_eq!(
            publish_url(
                "https://api.gawk.ioio.fi:4433",
                "K7XQ2M",
                "s3cret",
                "deadbeef"
            )
            .unwrap(),
            "https://api.gawk.ioio.fi:4433/publish/K7XQ2M?secret=s3cret&resume=deadbeef"
        );
        // No secret configured: parameter omitted entirely.
        assert_eq!(
            publish_url("https://localhost:4433", "", "", "").unwrap(),
            "https://localhost:4433/publish"
        );
        // Token without an ID is never sent (a mint must not carry a stale
        // token), and an ID without a token sends a bare reclaim the R17
        // relay will refuse with 403 — but that refusal is the relay's call.
        assert_eq!(
            publish_url("https://localhost:4433", "K7XQ2M", "", "").unwrap(),
            "https://localhost:4433/publish/K7XQ2M"
        );
        assert!(publish_url("http://localhost:4433", "", "", "").is_err());
        assert!(publish_url("not a url", "", "", "").is_err());
    }
}
