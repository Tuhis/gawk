//! The relay probe (R62, docs/64 D1): whether the selected relay answers,
//! and how far away it is. The web picker's probe (gawk-app/src/features/
//! servers/probe.ts), over the engine's own QUIC stack: a WebTransport
//! session on `/echo`, a few echoed datagrams, and the RelayIdentity the
//! relay sends on a uni stream at session start.
//!
//! Failure is ONE state, as on the web: an unreachable relay, a refused
//! session, a bad certificate and a relay that never echoes all mean the
//! same thing to the broadcaster, and the window says it once.

use crate::relay::RelaySession;
use gawk_wire as wire;
use std::sync::Arc;
use std::time::Duration;
use tokio::time::Instant;

/// Echoed datagrams per probe; the result is their median.
pub const PROBE_SAMPLES: usize = 5;
/// Pause between two echo sends.
pub const PROBE_SAMPLE_SPACING: Duration = Duration::from_millis(120);
/// How long the dial may take before the probe counts as failed.
pub const PROBE_CONNECT_TIMEOUT: Duration = Duration::from_secs(4);
/// How long after the session is up the RelayIdentity may still arrive. The
/// session stays open until the identity is read or this passes, whichever
/// comes first: on a fast link the echoes finish before the identity stream,
/// and closing on the echoes alone would call a current relay nameless.
pub const PROBE_IDENTITY_DEADLINE: Duration = Duration::from_millis(1500);
/// How long an echo may take after the last send before it is lost.
pub const PROBE_ECHO_WAIT: Duration = Duration::from_millis(1000);

/// The identity stream is one small message; anything bigger is not it.
const IDENTITY_READ_LIMIT: usize = 512;

/// Marks the probe's datagrams; the byte after it is the sample's index.
const PROBE_MAGIC: &[u8] = b"gawk-probe";

/// What a probe found.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum ProbeResult {
    /// The relay echoed. `rtt_ms` is the median round trip; `name` is the
    /// operator's display name from RelayIdentity, sanitized, when the relay
    /// sent a non-empty one in time. The name is the operator's claim: the
    /// window shows it beside the host, never in place of it (docs/40 F6).
    Ok { rtt_ms: u32, name: Option<String> },
    /// Nothing usable came back.
    Failed,
}

/// The `/echo` URL on a relay: the same origin, no query.
pub fn echo_url(relay_url: &str) -> Result<String, String> {
    let mut url = url::Url::parse(relay_url.trim()).map_err(|e| format!("bad relay URL: {e}"))?;
    if url.scheme() != "https" {
        return Err(format!("relay URL must be https, got {}", url.scheme()));
    }
    url.set_path("/echo");
    url.set_query(None);
    url.set_fragment(None);
    Ok(url.into())
}

/// Probes the relay at `relay_url`: dial `/echo`, then [`probe_session`].
pub async fn probe(relay_url: &str, origin: &str, insecure: bool) -> ProbeResult {
    let Ok(url) = echo_url(relay_url) else {
        return ProbeResult::Failed;
    };
    let dialed = tokio::time::timeout(
        PROBE_CONNECT_TIMEOUT,
        crate::transport::dial(&url, origin, insecure),
    )
    .await;
    match dialed {
        Ok(Ok(session)) => probe_session(Arc::new(session)).await,
        // A refused session and no answer at all read the same to the
        // broadcaster (the module doc); the shell logs the outcome.
        Ok(Err(_)) | Err(_) => ProbeResult::Failed,
    }
}

/// Measures an already-dialed `/echo` session, then closes it. The seam the
/// tests script.
pub async fn probe_session(relay: Arc<dyn RelaySession>) -> ProbeResult {
    let ready = Instant::now();
    let (rtts, name) = tokio::join!(
        measure(relay.clone()),
        read_identity(relay.clone(), ready + PROBE_IDENTITY_DEADLINE)
    );
    relay.close();
    match median(rtts) {
        Some(rtt) => ProbeResult::Ok {
            rtt_ms: u32::try_from(rtt.as_millis()).unwrap_or(u32::MAX),
            name,
        },
        None => ProbeResult::Failed,
    }
}

/// Sends the samples and collects each one's round trip.
async fn measure(relay: Arc<dyn RelaySession>) -> Vec<Duration> {
    let mut sent_at: [Option<Instant>; PROBE_SAMPLES] = [None; PROBE_SAMPLES];
    let mut rtts = Vec::with_capacity(PROBE_SAMPLES);
    let mut next = 0usize;
    let mut tick = tokio::time::interval(PROBE_SAMPLE_SPACING);
    // Far off until the last sample is out.
    let mut give_up = Instant::now() + Duration::from_secs(3600);
    loop {
        tokio::select! {
            _ = tick.tick(), if next < PROBE_SAMPLES => {
                let mut d = PROBE_MAGIC.to_vec();
                d.push(next as u8);
                // A failed send is a lost sample, not a failed probe.
                if relay.send_datagram(&d).is_ok() {
                    sent_at[next] = Some(Instant::now());
                }
                next += 1;
                if next == PROBE_SAMPLES {
                    give_up = Instant::now() + PROBE_ECHO_WAIT;
                }
            }
            got = relay.receive_datagram() => {
                let Ok(d) = got else { break };
                if let Some(i) = sample_index(&d)
                    && let Some(t0) = sent_at[i].take()
                {
                    rtts.push(t0.elapsed());
                    if rtts.len() == PROBE_SAMPLES {
                        break;
                    }
                }
            }
            _ = tokio::time::sleep_until(give_up) => break,
        }
    }
    rtts
}

/// The sample index an echoed datagram carries, if it is one of ours.
fn sample_index(d: &[u8]) -> Option<usize> {
    let rest = d.strip_prefix(PROBE_MAGIC)?;
    match rest {
        [i] if (*i as usize) < PROBE_SAMPLES => Some(*i as usize),
        _ => None,
    }
}

/// Reads the RelayIdentity, until `deadline`. A relay that predates it, or
/// one that sends it late, is still a reachable relay: this only ever costs
/// the name.
async fn read_identity(relay: Arc<dyn RelaySession>, deadline: Instant) -> Option<String> {
    let read = async {
        let mut stream = relay.accept_uni().await.ok()?;
        let msg = stream.read_to_end(IDENTITY_READ_LIMIT).await.ok()?;
        let id = wire::parse_relay_identity(&msg).ok()?;
        sanitize_name(id.name)
    };
    tokio::time::timeout_at(deadline, read).await.ok().flatten()
}

/// The operator's display name as the window may show it: control and
/// bidirectional-override characters dropped (a name must not reorder the
/// host printed beside it), whitespace trimmed, empty as none.
pub fn sanitize_name(name: &str) -> Option<String> {
    let bidi = |c: char| matches!(c, '\u{200e}' | '\u{200f}' | '\u{202a}'..='\u{202e}' | '\u{2066}'..='\u{2069}');
    let clean: String = name
        .chars()
        .filter(|&c| !c.is_control() && !bidi(c))
        .collect();
    let clean = clean.trim();
    (!clean.is_empty()).then(|| clean.to_owned())
}

/// The median of the samples; `None` without any. An even count averages
/// the middle two.
fn median(mut samples: Vec<Duration>) -> Option<Duration> {
    if samples.is_empty() {
        return None;
    }
    samples.sort();
    let mid = samples.len() / 2;
    Some(if samples.len().is_multiple_of(2) {
        (samples[mid - 1] + samples[mid]) / 2
    } else {
        samples[mid]
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_echo_url_keeps_the_origin_and_drops_the_rest() {
        assert_eq!(
            echo_url("https://api.gawk.ioio.fi:4433").unwrap(),
            "https://api.gawk.ioio.fi:4433/echo"
        );
        assert_eq!(
            echo_url(" https://relay.example:4433/publish?secret=x#y ").unwrap(),
            "https://relay.example:4433/echo"
        );
        assert!(echo_url("http://relay.example:4433").is_err());
        assert!(echo_url("not a url").is_err());
    }

    #[test]
    fn the_median_takes_the_middle_sample() {
        let ms = |v: &[u64]| v.iter().map(|&m| Duration::from_millis(m)).collect();
        assert_eq!(median(ms(&[])), None);
        assert_eq!(median(ms(&[40])), Some(Duration::from_millis(40)));
        assert_eq!(
            median(ms(&[10, 50, 20, 40, 30])),
            Some(Duration::from_millis(30))
        );
        assert_eq!(
            median(ms(&[10, 20, 30, 40])),
            Some(Duration::from_millis(25))
        );
    }

    #[test]
    fn a_name_is_shown_clean_or_not_at_all() {
        assert_eq!(
            sanitize_name("  Homelab relay "),
            Some("Homelab relay".into())
        );
        assert_eq!(
            sanitize_name("evil\u{202e}\n name"),
            Some("evil name".into())
        );
        assert_eq!(sanitize_name(" \t "), None);
        assert_eq!(sanitize_name(""), None);
    }

    #[test]
    fn only_our_samples_count() {
        assert_eq!(sample_index(b"gawk-probe\x03"), Some(3));
        assert_eq!(sample_index(b"gawk-probe\x05"), None, "out of range");
        assert_eq!(sample_index(b"gawk-probe"), None);
        assert_eq!(sample_index(b"something else"), None);
    }
}
