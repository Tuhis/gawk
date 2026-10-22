//! R74 PP5 (docs/76 §6.1 items 3 and 5): a dial pinned to a relay's
//! certificate hashes, against an in-test WebTransport server with a
//! self-signed certificate — the shape a `gawk-relay` serves: ECDSA P-256,
//! under two weeks, SANs that do not name the address it is dialed by (M5).
//! `relay_integration.rs` runs a whole pinned session against the real
//! gawk-server and its `gawk-devcert` certificate.

use gawk_engine::transport::{self, CertHash, Congestion};
use std::net::{Ipv4Addr, SocketAddr};
use std::time::Duration;
use wtransport::{Endpoint, Identity, ServerConfig};

/// A relay stand-in: accepts every session and holds it until the client
/// goes. Returns its URL and the hash of the certificate it serves.
async fn pinned_relay(identity: Identity) -> (String, CertHash) {
    let hash = *identity.certificate_chain().as_slice()[0].hash().as_ref();
    let config = ServerConfig::builder()
        .with_bind_address(SocketAddr::from((Ipv4Addr::LOCALHOST, 0)))
        .with_identity(identity)
        .build();
    let server = Endpoint::server(config).expect("server endpoint");
    let port = server.local_addr().expect("local addr").port();
    tokio::spawn(async move {
        loop {
            let incoming = server.accept().await;
            tokio::spawn(async move {
                let Ok(request) = incoming.await else { return };
                let Ok(conn) = request.accept().await else {
                    return;
                };
                conn.closed().await;
            });
        }
    });
    (format!("https://127.0.0.1:{port}/echo"), hash)
}

/// SANs name `localhost` only: the dial is by IP literal, as a guest dials
/// a LAN relay, and a pin does not check the name.
fn relay_identity(days: u32) -> Identity {
    Identity::self_signed_builder()
        .subject_alt_names(["localhost"])
        .from_now_utc()
        .validity_days(days)
        .build()
        .expect("identity")
}

fn other_hash() -> CertHash {
    [0x5a; 32]
}

const ORIGIN: &str = "gawk-broadcast://test";

async fn within<T>(f: impl std::future::Future<Output = T>) -> T {
    tokio::time::timeout(Duration::from_secs(15), f)
        .await
        .expect("the dial answered")
}

#[tokio::test(flavor = "multi_thread")]
async fn a_dial_pinned_to_the_served_hash_connects() {
    let (url, hash) = pinned_relay(relay_identity(13)).await;
    let session = within(transport::dial_pinned(&url, ORIGIN, false, &[hash])).await;
    assert!(session.is_ok(), "{:?}", session.err());
}

#[tokio::test(flavor = "multi_thread")]
async fn either_of_two_pins_is_enough() {
    // The relay publishes the hash it serves and the next (§6.1 item 3):
    // a client pinning both connects across a rotation, in either order.
    let (url, hash) = pinned_relay(relay_identity(13)).await;
    for pins in [[other_hash(), hash], [hash, other_hash()]] {
        let session = within(transport::dial_pinned(&url, ORIGIN, false, &pins)).await;
        assert!(session.is_ok(), "{:?}", session.err());
    }
}

#[tokio::test(flavor = "multi_thread")]
async fn every_dial_kind_takes_the_pins() {
    let (url, hash) = pinned_relay(relay_identity(13)).await;
    let publish = within(transport::dial_publish_pinned(
        &url,
        ORIGIN,
        false,
        &[hash],
        Congestion::default(),
    ))
    .await;
    assert!(publish.is_ok(), "{:?}", publish.err());
    let subscribe = within(transport::dial_subscribe_pinned(
        &url,
        ORIGIN,
        false,
        &[hash],
    ))
    .await;
    assert!(subscribe.is_ok(), "{:?}", subscribe.err());
    // The room dial opens its control stream too; the stand-in never
    // accepts one, so only a refused handshake would fail fast — reaching
    // the stream open proves the pin passed.
    let room = tokio::time::timeout(
        Duration::from_secs(3),
        transport::dial_room_pinned(&url, ORIGIN, false, &[hash]),
    )
    .await;
    if let Ok(Err(e)) = room {
        assert!(
            e.message.contains("control stream"),
            "the room dial failed before its stream: {}",
            e.message
        );
    }
}

#[tokio::test(flavor = "multi_thread")]
async fn a_wrong_pin_is_refused_at_once() {
    let (url, _) = pinned_relay(relay_identity(13)).await;
    let started = std::time::Instant::now();
    let err = within(transport::dial_pinned(&url, ORIGIN, false, &[other_hash()]))
        .await
        .err()
        .expect("a wrong pin is refused");
    // A TLS refusal, not an unanswered dial: status 0, and well inside
    // the first attempt's timeout.
    assert_eq!(err.status, 0);
    assert!(started.elapsed() < transport::DIAL_ATTEMPT_TIMEOUT);
}

#[tokio::test(flavor = "multi_thread")]
async fn without_pins_the_system_store_still_decides() {
    // No pins and not insecure: the self-signed certificate is unknown to
    // the system store, exactly as before R74.
    let (url, _) = pinned_relay(relay_identity(13)).await;
    let err = within(transport::dial_pinned(&url, ORIGIN, false, &[])).await;
    assert!(err.is_err());
    let err = within(transport::dial(&url, ORIGIN, false)).await;
    assert!(err.is_err());
}

#[tokio::test(flavor = "multi_thread")]
async fn a_pinned_certificate_over_two_weeks_is_refused() {
    // The WebTransport rule the browsers apply too: why the relay's
    // certificate is 13 days.
    let (url, hash) = pinned_relay(relay_identity(15)).await;
    let err = within(transport::dial_pinned(&url, ORIGIN, false, &[hash])).await;
    assert!(err.is_err());
}

#[test]
fn hashes_parse_from_lowercase_hex_only() {
    let hex = "a1".repeat(32);
    assert_eq!(transport::cert_hash_from_hex(&hex), Some([0xa1; 32]));
    assert_eq!(transport::cert_hash_from_hex(&hex.to_uppercase()), None);
    assert_eq!(transport::cert_hash_from_hex(&hex[..62]), None);
    assert_eq!(transport::cert_hash_from_hex(&format!("{hex}00")), None);
    assert_eq!(transport::cert_hash_from_hex(&"zz".repeat(32)), None);
}
